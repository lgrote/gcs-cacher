# syntax=docker/dockerfile:1
# Go image build — the house template (avacom-infra docs/go-service-standard.md G16).
# gcs-cacher is a Cloud Build builder image (restore/save cache step), not a k8s service: target
# linux/amd64 (Cloud Build workers are x86), binary /main (main package at the repo root).
#
# Cloud Build runs every build on a fresh VM: BuildKit cache mounts start empty, only layers come
# back (registry cache, mode=max). So the expensive work lives in layers keyed on what it depends on:
#   tools    – this file only
#   modules  – go.mod/go.sum
#   deps     – the set of imported packages: every dependency (and the standard library) compiled
#              for the builder's arch (tests, lint) and for TARGETARCH (the binary)
# A commit then compiles only this module's packages. Measured on datar (Cloud Build, E2_HIGHCPU_8,
# warm, code change): build-push 181–211 s → 131–140 s. Never mount a cache over /go/pkg/mod or
# /root/.cache/go-build below `base`: the mount would hide the layer it replaces.
#
# Build stages run on the builder's arch (x86 in Cloud Build); Go cross-compiles to TARGETARCH
# (here amd64, cloudbuild.yaml --platform) — no QEMU. The runtime stage is pulled per target.

FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS base
WORKDIR /src
# GOTOOLCHAIN=local: the image's Go builds and scans — govulncheck reports the standard library of
# the Go that ships.
ENV CGO_ENABLED=0 GOTOOLCHAIN=local
# CI tools, pinned: lint findings shift between releases, so a bump is a deliberate commit. Built by
# the image's Go (an older-Go golangci-lint fails to load newer code). govulncheck fetches the
# vulnerability database when it runs, so a pinned binary still sees new advisories. The cache
# mounts keep the tools' build cache out of the layer: only the two binaries land in it.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.14.0 && \
    go install golang.org/x/vuln/cmd/govulncheck@v1.8.0
COPY go.mod go.sum ./
RUN go mod download && find /go/pkg/mod/cache/download -name '*.zip' -delete

# The imported packages outside this module: the cache key of `deps`. Re-listed on every build
# (seconds); `deps` is rebuilt only when the list changes.
FROM base AS deplist
ARG TARGETARCH
COPY . .
RUN f='{{if not (and .Module .Module.Main)}}{{.ImportPath}}{{end}}' && \
    go list -deps -test -f "$f" ./... | grep -v ' \[' | sort -u > /deps-host && \
    GOARCH=$TARGETARCH go list -deps -f "$f" ./... | sort -u > /deps-target

# Compiled with the flags the later steps use (a different flag is a cache miss): the tests as they
# are, the binary with -trimpath.
FROM base AS deps
ARG TARGETARCH
COPY --from=deplist /deps-host /deps-target /
RUN go build $(cat /deps-host) && GOARCH=$TARGETARCH go build -trimpath $(cat /deps-target)

FROM deps AS src
COPY . .

# The gates, one after the other so lint and govulncheck reuse what the tests compiled. They run
# concurrently with `build`; never split them into parallel stages (each compiles everything again
# and they fight over the CPUs — datar 2026-10-05: 11 min instead of 4). go vet runs in golangci-lint
# (govet, enable-all), hence -vet=off. Integration tests need Docker and are skipped by -short.
FROM src AS checks
RUN go test -short -vet=off ./...
RUN --mount=type=cache,target=/root/.cache/golangci-lint golangci-lint run ./...
RUN govulncheck ./... && touch /checks-ok

FROM src AS build
ARG TARGETARCH
RUN GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/main .

# static: CA certificates and tzdata, no libc, no shell, UID 65532.
FROM gcr.io/distroless/static-debian12:nonroot
# The gate's marker (empty file): the image cannot be built when a check fails.
COPY --from=checks /checks-ok /.gates/
COPY --from=build /out/main /main
# nosemgrep: missing-user-entrypoint -- distroless:nonroot runs as UID 65532
ENTRYPOINT ["/main"]
