// Package gcs is the Google Cloud Storage backend behind
// [objstore.Backend].
//
// Auth: Application Default Credentials by default (works with
// `gcloud auth application-default login`, GCE metadata server,
// Workload Identity, or GOOGLE_APPLICATION_CREDENTIALS).
// Config.CredentialsFile pins a specific service-account key file
// and bypasses the ADC chain.
package gcs

import (
	"context"
	"errors"
	"fmt"
	"io"

	"cloud.google.com/go/storage"
	"github.com/bbsakura/s3fs-ebpf/daemon/internal/objstore"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
)

// Config holds connection settings. Unlike S3 there is no
// endpoint/SSL — GCS is always storage.googleapis.com over HTTPS.
// CredentialsFile is optional; empty means use ADC.
type Config struct {
	Bucket          string
	CredentialsFile string
	// CacheDir enables the same local staging short-circuit as the
	// S3 backend. Empty disables caching.
	CacheDir string
	// MaxCacheBytes caps the disk cache size (S-07). 0 = unlimited.
	MaxCacheBytes int64
}

type Client struct {
	cfg    Config
	gc     *storage.Client
	bucket *storage.BucketHandle
	cache  objstore.DiskCache
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
	cache, err := objstore.NewDiskCache(cfg.CacheDir, objstore.KindGCS, cfg.Bucket, cfg.MaxCacheBytes)
	if err != nil {
		return nil, err
	}
	return &Client{cfg: cfg, gc: gc, bucket: gc.Bucket(cfg.Bucket), cache: cache}, nil
}

func (c *Client) Bucket() string { return c.cfg.Bucket }

// Close releases the underlying storage client. The daemon can leave
// this unwired at shutdown — process exit tears down the HTTP/2
// conns — but tests should call it.
func (c *Client) Close() error { return c.gc.Close() }

func (c *Client) Stat(ctx context.Context, key string) (objstore.ObjectMeta, error) {
	attrs, err := c.bucket.Object(key).Attrs(ctx)
	if err != nil {
		return objstore.ObjectMeta{}, fmt.Errorf("gcs stat %q: %w", key, err)
	}
	return attrsToMeta(attrs), nil
}

func (c *Client) Get(ctx context.Context, key string) ([]byte, error) {
	if buf, ok := c.cache.Read(key); ok {
		return buf, nil
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
	c.cache.WriteAsync(key, buf)
	return buf, nil
}

// GetRange skips the local disk cache — cache is keyed on whole
// object.
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

func (c *Client) Put(ctx context.Context, key string, data []byte) error {
	w := c.bucket.Object(key).NewWriter(ctx)
	w.ContentType = "application/octet-stream"
	if _, err := w.Write(data); err != nil {
		_ = w.Close()
		return fmt.Errorf("gcs put %q: %w", key, err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("gcs put %q close: %w", key, err)
	}
	c.cache.Invalidate(key)
	return nil
}

// Delete swallows ErrObjectNotExist so a second delete against an
// already-gone object is idempotent — matches MinIO's behavior on a
// missing key.
func (c *Client) Delete(ctx context.Context, key string) error {
	if err := c.bucket.Object(key).Delete(ctx); err != nil {
		if errors.Is(err, storage.ErrObjectNotExist) {
			return nil
		}
		return fmt.Errorf("gcs delete %q: %w", key, err)
	}
	c.cache.Invalidate(key)
	return nil
}

// Rename copies then deletes the source. GCS has no atomic rename;
// partial failure (copy ok, delete fails) leaves both keys.
func (c *Client) Rename(ctx context.Context, oldKey, newKey string) error {
	src := c.bucket.Object(oldKey)
	dst := c.bucket.Object(newKey)
	if _, err := dst.CopierFrom(src).Run(ctx); err != nil {
		return fmt.Errorf("gcs copy %q → %q: %w", oldKey, newKey, err)
	}
	if err := src.Delete(ctx); err != nil {
		return fmt.Errorf("gcs delete src %q after copy: %w", oldKey, err)
	}
	c.cache.InvalidateRename(oldKey, newKey)
	return nil
}

// List is recursive by default — Delimiter left unset so GCS returns
// every object rather than collapsing on '/'.
func (c *Client) List(ctx context.Context, prefix string) ([]objstore.ObjectMeta, error) {
	it := c.bucket.Objects(ctx, &storage.Query{Prefix: prefix})
	out := make([]objstore.ObjectMeta, 0, 256)
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

var _ objstore.Backend = (*Client)(nil)

func attrsToMeta(a *storage.ObjectAttrs) objstore.ObjectMeta {
	// GCS returns an MD5 hash and a CRC32C; ETag is not part of
	// ObjectAttrs. Fall back to the object generation number so the
	// field is never empty, which matters for the s3-stat output.
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
