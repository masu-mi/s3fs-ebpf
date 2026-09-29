// Package gcs is the Google Cloud Storage backend behind
// [objstore.Backend]. Feature parity with internal/s3: Stat, Get,
// GetRange, Put, Delete, Rename (Copy+Delete), List. Same local
// staging-cache semantics.
//
// Auth: Application Default Credentials by default (works with
// `gcloud auth application-default login`, GCE metadata server,
// Workload Identity, or GOOGLE_APPLICATION_CREDENTIALS). Config.CredentialsFile
// overrides ADC for pinning a specific service-account key file.
package gcs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"cloud.google.com/go/storage"
	"github.com/bbsakura/s3fs-ebpf/daemon/internal/objstore"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
)

// Config holds connection settings for the GCS backend. Unlike the
// S3 config there is no endpoint/SSL — GCS is always
// storage.googleapis.com over HTTPS. CredentialsFile is optional;
// empty means use Application Default Credentials.
type Config struct {
	Bucket          string
	CredentialsFile string
	// CacheDir enables the same local staging short-circuit as the
	// S3 backend. Empty disables caching.
	CacheDir string
}

// Client is the daemon-side GCS handle.
type Client struct {
	cfg    Config
	gc     *storage.Client
	bucket *storage.BucketHandle
}

// New constructs a Client bound to `cfg`. Does not validate
// connectivity — the first Stat/Get touches the network.
func New(ctx context.Context, cfg Config) (*Client, error) {
	if cfg.Bucket == "" {
		return nil, fmt.Errorf("gcs: bucket must not be empty")
	}
	var opts []option.ClientOption
	if cfg.CredentialsFile != "" {
		opts = append(opts, option.WithCredentialsFile(cfg.CredentialsFile))
	}
	gc, err := storage.NewClient(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("gcs: init client: %w", err)
	}
	return &Client{cfg: cfg, gc: gc, bucket: gc.Bucket(cfg.Bucket)}, nil
}

// Bucket returns the bound bucket name.
func (c *Client) Bucket() string { return c.cfg.Bucket }

// Close releases the underlying storage client. Callers can leave
// this unwired at daemon shutdown — the process exit tears down the
// HTTP2 conns — but tests should call it.
func (c *Client) Close() error { return c.gc.Close() }

// Stat returns metadata for a single object.
func (c *Client) Stat(ctx context.Context, key string) (objstore.ObjectMeta, error) {
	attrs, err := c.bucket.Object(key).Attrs(ctx)
	if err != nil {
		return objstore.ObjectMeta{}, fmt.Errorf("gcs stat %q: %w", key, err)
	}
	return attrsToMeta(attrs), nil
}

// Get reads the entire object body into memory. Mirrors s3.Client.Get
// including the CacheDir short-circuit; see that file for the
// invariants.
func (c *Client) Get(ctx context.Context, key string) ([]byte, error) {
	if c.cfg.CacheDir != "" {
		if buf, ok := c.readCache(key); ok {
			return buf, nil
		}
	}
	r, err := c.bucket.Object(key).NewReader(ctx)
	if err != nil {
		return nil, fmt.Errorf("gcs get %q: %w", key, err)
	}
	defer r.Close()
	buf, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("gcs get %q read: %w", key, err)
	}
	if c.cfg.CacheDir != "" {
		c.writeCache(key, buf)
	}
	return buf, nil
}

// GetRange fetches `size` bytes starting at `offset`. Skips the
// local disk cache (same policy as s3.Client.GetRange — cache is
// keyed on whole object).
func (c *Client) GetRange(ctx context.Context, key string, offset, size int64) ([]byte, error) {
	r, err := c.bucket.Object(key).NewRangeReader(ctx, offset, size)
	if err != nil {
		return nil, fmt.Errorf("gcs range get %q [%d,%d): %w",
			key, offset, offset+size, err)
	}
	defer r.Close()
	buf, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("gcs range get %q read: %w", key, err)
	}
	return buf, nil
}

// Put uploads `data` as a single-shot write. The GCS resumable
// upload threshold is applied automatically by the SDK's Writer.
func (c *Client) Put(ctx context.Context, key string, data []byte) error {
	w := c.bucket.Object(key).NewWriter(ctx)
	w.ContentType = "application/octet-stream"
	if _, err := w.Write(data); err != nil {
		// Close to release the upload; ignore its error — the Write
		// error is what we surface.
		_ = w.Close()
		return fmt.Errorf("gcs put %q: %w", key, err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("gcs put %q close: %w", key, err)
	}
	if c.cfg.CacheDir != "" {
		_ = os.Remove(c.cachePath(key))
	}
	return nil
}

// Delete removes `key`. ErrObjectNotExist is swallowed so a
// second delete against an already-gone object is idempotent —
// same net behavior as MinIO's DELETE on a missing key.
func (c *Client) Delete(ctx context.Context, key string) error {
	if err := c.bucket.Object(key).Delete(ctx); err != nil {
		if errors.Is(err, storage.ErrObjectNotExist) {
			return nil
		}
		return fmt.Errorf("gcs delete %q: %w", key, err)
	}
	if c.cfg.CacheDir != "" {
		_ = os.Remove(c.cachePath(key))
	}
	return nil
}

// Rename copies then deletes the source. GCS has no atomic rename;
// a partial failure (copy ok, delete fails) leaves both keys, matching
// the s3.Client.Rename contract.
func (c *Client) Rename(ctx context.Context, oldKey, newKey string) error {
	src := c.bucket.Object(oldKey)
	dst := c.bucket.Object(newKey)
	if _, err := dst.CopierFrom(src).Run(ctx); err != nil {
		return fmt.Errorf("gcs copy %q → %q: %w", oldKey, newKey, err)
	}
	if err := src.Delete(ctx); err != nil {
		return fmt.Errorf("gcs delete src %q after copy: %w", oldKey, err)
	}
	if c.cfg.CacheDir != "" {
		_ = os.Remove(c.cachePath(oldKey))
	}
	return nil
}

// List enumerates objects under `prefix`. Empty prefix means the
// whole bucket. Recursive by default — Delimiter is left unset so
// GCS returns every object rather than collapsing on '/'.
func (c *Client) List(ctx context.Context, prefix string) ([]objstore.ObjectMeta, error) {
	it := c.bucket.Objects(ctx, &storage.Query{Prefix: prefix})
	var out []objstore.ObjectMeta
	for {
		attrs, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("gcs list %q: %w", prefix, err)
		}
		out = append(out, attrsToMeta(attrs))
	}
	return out, nil
}

// Compile-time proof gcs.Client implements the shared backend
// interface.
var _ objstore.Backend = (*Client)(nil)

func attrsToMeta(a *storage.ObjectAttrs) objstore.ObjectMeta {
	// GCS returns an MD5 hash and a CRC32C; ETag is not part of
	// ObjectAttrs. Fall back to the object generation number so the
	// field is never empty, which matters for the `s3-stat` output.
	etag := a.Etag
	if etag == "" {
		etag = fmt.Sprintf("gen:%d", a.Generation)
	}
	return objstore.ObjectMeta{
		Key:          a.Name,
		Size:         a.Size,
		ETag:         etag,
		LastModified: a.Updated,
		ContentType:  a.ContentType,
	}
}

// -- local staging cache -------------------------------------------------
//
// Same shape as s3.Client's cache. Kept private to this package
// (rather than lifted to objstore) so each backend can evolve its
// freshness policy independently — GCS could grow ETag/generation
// checks without touching S3.

func (c *Client) cachePath(key string) string {
	h := sha256.Sum256([]byte(c.cfg.Bucket + "/" + key))
	return filepath.Join(c.cfg.CacheDir, hex.EncodeToString(h[:]))
}

func (c *Client) readCache(key string) ([]byte, bool) {
	buf, err := os.ReadFile(c.cachePath(key))
	if err != nil {
		return nil, false
	}
	return buf, true
}

func (c *Client) writeCache(key string, buf []byte) {
	if err := os.MkdirAll(c.cfg.CacheDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "gcs cache mkdir %s: %v\n", c.cfg.CacheDir, err)
		return
	}
	tmp := c.cachePath(key) + ".tmp"
	if err := os.WriteFile(tmp, buf, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "gcs cache write %s: %v\n", tmp, err)
		return
	}
	if err := os.Rename(tmp, c.cachePath(key)); err != nil {
		fmt.Fprintf(os.Stderr, "gcs cache rename %s: %v\n", tmp, err)
	}
}
