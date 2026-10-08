// Package cacher defines utilities for saving and restoring caches from Google
// Cloud storage.
package cacher

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"

	"cloud.google.com/go/storage"
	"golang.org/x/crypto/blake2b"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
)

const (
	contentType  = "application/gzip"
	cacheControl = "public,max-age=3600"
)

// Cacher is responsible for saving and restoring caches.
type Cacher struct {
	client *storage.Client

	debug bool
}

// New creates a new cacher capable of saving and restoring the cache.
func New(ctx context.Context) (*Cacher, error) {
	cred, err := google.FindDefaultCredentials(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to find default credentials: %w", err)
	}

	client, err := storage.NewClient(ctx,
		option.WithUserAgent("gcs-cacher/1.0"),
		option.WithCredentials(cred))
	if err != nil {
		return nil, fmt.Errorf("failed to create storage client: %w", err)
	}

	return &Cacher{
		client: client,
	}, nil
}

// Debug enables or disables debugging for the cacher.
func (c *Cacher) Debug(val bool) {
	c.debug = val
}

// SaveRequest is used as input to the Save operation.
type SaveRequest struct {
	// Bucket is the name of the bucket from which to cache.
	Bucket string

	// Key is the cache key.
	Key string

	// Dir is the directory on disk to cache.
	Dir string
}

// Save caches the given directory in storage.
func (c *Cacher) Save(ctx context.Context, i *SaveRequest) (retErr error) {
	if i == nil {
		return errors.New("missing cache options")
	}

	bucket := i.Bucket
	if bucket == "" {
		return errors.New("missing bucket")
	}

	dir := i.Dir
	if dir == "" {
		return errors.New("missing directory")
	}

	key := i.Key
	if key == "" {
		return errors.New("missing key")
	}

	// Check if the object already exists. If it already exists, we do not want to
	// waste time overwriting the cache.
	attrs, err := c.client.Bucket(bucket).Object(key).Attrs(ctx)
	if err != nil && !errors.Is(err, storage.ErrObjectNotExist) {
		return fmt.Errorf("failed to check if cached object exists: %w", err)
	}
	if attrs != nil {
		c.log("cached object already exists, skipping")
		return nil
	}

	// Create the storage writer
	dne := storage.Conditions{DoesNotExist: true}
	gcsw := c.client.Bucket(bucket).Object(key).If(dne).NewWriter(ctx)
	defer func() {
		c.log("closing gcs writer")
		if cerr := gcsw.Close(); cerr != nil {
			if retErr != nil {
				retErr = fmt.Errorf("%w: failed to close gcs writer: %w", retErr, cerr)
				return
			}
			retErr = fmt.Errorf("failed to close gcs writer: %w", cerr)
		}
	}()

	gcsw.ChunkSize = 128_000_000
	gcsw.ContentType = contentType
	gcsw.CacheControl = cacheControl
	gcsw.ProgressFunc = func(soFar int64) {
		fmt.Printf("uploaded %d bytes\n", soFar)
	}

	// Create the gzip writer
	gzw := gzip.NewWriter(gcsw)
	defer func() {
		c.log("closing gzip writer")
		if cerr := gzw.Close(); cerr != nil {
			if retErr != nil {
				retErr = fmt.Errorf("%w: failed to close gzip writer: %w", retErr, cerr)
				return
			}
			retErr = fmt.Errorf("failed to close gzip writer: %w", cerr)
		}
	}()

	// Create the tar writer
	tw := tar.NewWriter(gzw)
	defer func() {
		c.log("closing tar writer")
		if cerr := tw.Close(); cerr != nil {
			if retErr != nil {
				retErr = fmt.Errorf("%w: failed to close tar writer: %w", retErr, cerr)
				return
			}
			retErr = fmt.Errorf("failed to close tar writer: %w", cerr)
		}
	}()

	// Walk all files create tar
	if err := filepath.Walk(dir, func(name string, f os.FileInfo, err error) error {
		c.log("walking file %s", name)

		if err != nil {
			return err
		}

		if !f.Mode().IsRegular() {
			c.log("file %s is not regular", name)
			return nil
		}

		// Create the tar header
		header, err := tar.FileInfoHeader(f, f.Name())
		if err != nil {
			return fmt.Errorf("failed to create tar header for %s: %w", f.Name(), err)
		}
		header.Name = strings.TrimPrefix(strings.ReplaceAll(name, dir, ""), string(filepath.Separator))

		// Write header to tar
		c.log("writing tar header for %s", name)
		if werr := tw.WriteHeader(header); werr != nil {
			return fmt.Errorf("failed to write tar header for %s: %w", f.Name(), werr)
		}

		// Open and write file to tar
		c.log("opening %s", name)
		file, err := os.Open(name) //nolint:gosec // G122: walks the build step's own directory; symlinks are skipped (Lstat), no untrusted writer
		if err != nil {
			return fmt.Errorf("failed to open %s: %w", f.Name(), err)
		}

		c.log("copying %s to tar", name)
		if _, err := io.Copy(tw, file); err != nil {
			if cerr := file.Close(); cerr != nil {
				return fmt.Errorf("failed to close %s: %w: failed to write tar: %w", f.Name(), cerr, err)
			}
			return fmt.Errorf("failed to write tar for %s: %w", f.Name(), err)
		}

		// Close tar
		c.log("closing %s", name)
		if err := file.Close(); err != nil {
			return fmt.Errorf("failed to close: %w", err)
		}

		return nil
	}); err != nil {
		return fmt.Errorf("failed to walk files: %w", err)
	}

	return nil
}

// RestoreRequest is used as input to the Restore operation.
type RestoreRequest struct {
	// Bucket is the name of the bucket from which to cache.
	Bucket string

	// Keys is the ordered list of keys to restore.
	Keys []string

	// Dir is the directory on disk to cache.
	Dir string
}

// Restore restores the key from the cache into the dir on disk.
func (c *Cacher) Restore(ctx context.Context, i *RestoreRequest) (retErr error) {
	if i == nil {
		return errors.New("missing cache options")
	}

	bucket := i.Bucket
	if bucket == "" {
		return errors.New("missing bucket")
	}

	dir := i.Dir
	if dir == "" {
		return errors.New("missing directory")
	}

	keys := i.Keys
	if len(keys) < 1 {
		return errors.New("expected at least one cache key")
	}

	// Get the bucket handle
	bucketHandle := c.client.Bucket(bucket)

	// Try to find an earlier cached item by looking for the "newest" item with
	// one of the provided key fallbacks as a prefix.
	var match *storage.ObjectAttrs
	for _, key := range keys {
		c.log("searching for objects with prefix %s", key)

		it := bucketHandle.Objects(ctx, &storage.Query{
			Prefix: key,
		})

		for {
			attrs, err := it.Next()
			if errors.Is(err, iterator.Done) {
				break
			}
			if err != nil {
				return fmt.Errorf("failed to list %s in bucket %s: %w", key, bucket, err)
			}

			c.log("found object %s", key)

			if match == nil || attrs.Updated.After(match.Updated) {
				c.log("setting %s as best candidate", key)
				match = attrs
				continue
			}
		}
	}

	// Ensure we found one
	if match == nil {
		return fmt.Errorf("failed to find cached objects among keys %q", keys)
	}

	// Ensure the output directory exists
	c.log("making target directory %s", dir)
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // G301: restored caches are read by later Cloud Build steps running as other users
		return fmt.Errorf("failed to make target directory: %w", err)
	}

	// Create the gcs reader
	gcsr, err := bucketHandle.Object(match.Name).NewReader(ctx)
	if err != nil {
		return fmt.Errorf("failed to create object reader: %w", err)
	}
	defer func() {
		c.log("closing gcs reader")
		if cerr := gcsr.Close(); cerr != nil {
			if retErr != nil {
				retErr = fmt.Errorf("%w: failed to close gcs reader: %w", retErr, cerr)
				return
			}
			retErr = fmt.Errorf("failed to close gcs reader: %w", cerr)
		}
	}()

	// Create the gzip reader
	gzr, err := gzip.NewReader(gcsr)
	if err != nil {
		return fmt.Errorf("failed to create gzip reader: %w", err)
	}
	defer func() {
		c.log("closing gzip reader")
		if cerr := gzr.Close(); cerr != nil {
			if retErr != nil {
				retErr = fmt.Errorf("%w: failed to close gzip reader: %w", retErr, cerr)
				return
			}
			retErr = fmt.Errorf("failed to close gzip reader: %w", cerr)
		}
	}()

	// Create the tar reader
	tr := tar.NewReader(gzr)

	// Unzip and untar each file into the target directory
	if err := func() error {
		for {
			header, err := tr.Next()
			if err != nil {
				if err == io.EOF {
					// No more files
					return nil
				}

				return fmt.Errorf("failed to read header: %w", err)
			}

			// Not entirely sure how this happens? I think it was because I uploaded a
			// bad tarball. Nonetheless, we shall check.
			if header == nil {
				c.log("header is nil")
				continue
			}

			// An entry outside dir (absolute, or with ..) is never written: the archive is
			// rejected rather than extracted partially.
			if !filepath.IsLocal(header.Name) {
				return fmt.Errorf("refusing tar entry outside the target directory: %q", header.Name)
			}
			target := filepath.Join(dir, header.Name) //nolint:gosec // G305: filepath.IsLocal above rejects entries outside dir
			c.log("working on %s", target)

			switch header.Typeflag {
			case tar.TypeDir:
				c.log("creating directory %s", target)

				if err := os.MkdirAll(target, 0o755); err != nil { //nolint:gosec // G301: see the target directory above
					return fmt.Errorf("failed to make directory %s: %w", target, err)
				}
			case tar.TypeReg:
				c.log("creating file %s", target)

				// Create the parent directory in case it does not exist...
				parent := filepath.Dir(target)
				if err := os.MkdirAll(parent, 0o755); err != nil { //nolint:gosec // G301: see the target directory above
					return fmt.Errorf("failed to make parent directory %s: %w", parent, err)
				}

				c.log("opening %s", target)
				f, err := os.OpenFile(target, os.O_CREATE|os.O_RDWR, os.FileMode(header.Mode))
				if err != nil {
					return fmt.Errorf("failed to open %s: %w", target, err)
				}

				c.log("copying %s to disk", target)
				if _, err := io.Copy(f, tr); err != nil { //nolint:gosec // G110: archives come from the project's own cache bucket; caches are unbounded by design
					if cerr := f.Close(); cerr != nil {
						return fmt.Errorf("failed to close %s: %w: failed to untar: %w", target, cerr, err)
					}
					return fmt.Errorf("failed to untar %s: %w", target, err)
				}

				// Close f here instead of deferring
				c.log("closing %s", target)
				if err := f.Close(); err != nil {
					return fmt.Errorf("failed to close %s: %w", target, err)
				}
			default:
				return fmt.Errorf("unknown header type %v for %s", header.Typeflag, target)
			}
		}
	}(); err != nil {
		return fmt.Errorf("failed to download file: %w", err)
	}

	return nil
}

// HashGlob hashes the files matched by the given glob.
func (c *Cacher) HashGlob(pattern string) (string, error) {
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return "", fmt.Errorf("failed to glob: %w", err)
	}
	return c.HashFiles(matches)
}

// HashFiles hashes the list of file and returns the hex-encoded SHA256.
func (c *Cacher) HashFiles(files []string) (string, error) {
	h, err := blake2b.New(16, nil)
	if err != nil {
		return "", fmt.Errorf("failed to create hash: %w", err)
	}

	hashOne := func(name string, h hash.Hash) (retErr error) {
		c.log("opening %s", name)
		f, err := os.Open(name)
		if err != nil {
			return fmt.Errorf("failed to open file: %w", err)
		}
		defer func() {
			c.log("closing %s", name)
			if cerr := f.Close(); cerr != nil {
				if retErr != nil {
					retErr = fmt.Errorf("%w: failed to close file: %w", retErr, cerr)
					return
				}
				retErr = fmt.Errorf("failed to close file: %w", cerr)
			}
		}()

		c.log("stating %s", name)
		stat, err := f.Stat()
		if err != nil {
			return fmt.Errorf("failed to stat file: %w", err)
		}

		if stat.IsDir() {
			c.log("skipping %s (is a directory)", name)
			return nil
		}

		c.log("hashing %s", name)
		if _, err := io.Copy(h, f); err != nil {
			return fmt.Errorf("failed to hash: %w", err)
		}

		return nil
	}

	for _, name := range files {
		if err := hashOne(name, h); err != nil {
			return "", fmt.Errorf("failed to hash %s: %w", name, err)
		}
	}

	dig := h.Sum(nil)
	return hex.EncodeToString(dig), nil
}

func (c *Cacher) log(msg string, vars ...any) {
	if c.debug {
		log.Printf(msg, vars...)
	}
}
