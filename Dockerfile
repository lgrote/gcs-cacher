FROM golang:1.24-bookworm AS build

COPY go.mod go.sum ./
RUN go mod download

COPY /cacher ./cacher
COPY *.go ./

RUN go build -v -ldflags="-s -w" -o /main ./

FROM gcr.io/distroless/base-nossl-debian12:nonroot
COPY --from=build /main /main
CMD ["/main"]
ENTRYPOINT ["/main"]
