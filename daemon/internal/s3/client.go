// Package s3 is the daemon-side object-store client. Week 2 (d) wires
// it to a real minio-go v7 backend so the Fast Path miss handler can
// eventually round-trip S3 metadata into the arena. Compatible with
// MinIO, AWS S3, or any S3-compatible endpoint.
package s3

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// Config holds connection settings for the object store. Endpoint is a
// host:port with no scheme; UseSSL toggles https vs http.
type Config struct {
	Endpoint  string
	Bucket    string
	AccessKey string
	SecretKey string
	UseSSL    bool
	// CacheDir enables local disk staging for Get(). If set, Get looks
	// for <CacheDir>/<sha256(bucket/key)> first and returns its content
	// without touching S3; on miss it fetches from S3 and populates the
	// cache. Empty string disables caching. Analogous to s3fs-fuse's
	// -o use_cache=<dir> option (bench §0.5 caveat).
	CacheDir string
}

// Client is the daemon-side object-store handle.
type Client struct {
	cfg Config
	mc  *minio.Client
}

// ObjectMeta is the subset of S3 metadata the arena caches.
type ObjectMeta struct {
	Key          string
	Size         int64
	ETag         string
	LastModified time.Time
	ContentType  string
}

// New constructs a Client bound to the given config. Does not validate
// connectivity — callers touch the network on the first call.
func New(cfg Config) (*Client, error) {
	if cfg.Endpoint == "" {
		return nil, fmt.Errorf("s3: endpoint must not be empty")
	}
	if cfg.Bucket == "" {
		return nil, fmt.Errorf("s3: bucket must not be empty")
	}
	mc, err := minio.New(cfg.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure: cfg.UseSSL,
	})
	if err != nil {
		return nil, fmt.Errorf("s3: init minio client: %w", err)
	}
	return &Client{cfg: cfg, mc: mc}, nil
}

// Bucket returns the bound bucket name.
func (c *Client) Bucket() string { return c.cfg.Bucket }

// Stat returns metadata for a single object. Returns a wrapped error
// (containing the underlying minio.ErrorResponse) when the object does
// not exist or the endpoint refuses the request.
func (c *Client) Stat(ctx context.Context, key string) (ObjectMeta, error) {
	info, err := c.mc.StatObject(ctx, c.cfg.Bucket, key, minio.StatObjectOptions{})
	if err != nil {
		return ObjectMeta{}, fmt.Errorf("s3 stat %q: %w", key, err)
	}
	return toMeta(info), nil
}

// Get reads the entire object body into memory. Intended for the
// Week 4 (a) arena admission path where the daemon writes the bytes
// into arena pages that kmod's .read then serves. Not suitable for
// large objects — callers should switch to a streaming variant once
// tiered storage lands.
//
// If Config.CacheDir is set, Get short-circuits through the local
// staging file when present. Freshness check is timestamp-of-file
// only (no ETag/If-None-Match); good enough to prove the fair-bench
// scenario against s3fs-fuse's -o use_cache. Cache poisoning is not
// a concern for the PoC — same trust boundary as arena admits.
// Put uploads `data` as a single-shot PUT under `key`. Week 4 (c)
// wires kmod's .release/.fsync flush events into this path so writes
// through the mount become durable in the backing bucket. Any local
// disk cache stays out of the loop — a PUT invalidates rather than
// updates it, so a subsequent Get re-fetches the fresh body.
func (c *Client) Put(ctx context.Context, key string, data []byte) error {
	r := bytes.NewReader(data)
	_, err := c.mc.PutObject(ctx, c.cfg.Bucket, key, r,
		int64(len(data)), minio.PutObjectOptions{
			ContentType: "application/octet-stream",
		})
	if err != nil {
		return fmt.Errorf("s3 put %q: %w", key, err)
	}
	if c.cfg.CacheDir != "" {
		_ = os.Remove(c.cachePath(key))
	}
	return nil
}

// Delete removes `key` from the backing bucket. Local cache entry
// (if configured) is dropped too so a subsequent Get re-fetches
// authoritative state (which will surface 404 or the newer key).
// Called by the delete-drain loop after kmod's .unlink enqueues an
// event (E-02).
func (c *Client) Delete(ctx context.Context, key string) error {
	err := c.mc.RemoveObject(ctx, c.cfg.Bucket, key, minio.RemoveObjectOptions{})
	if err != nil {
		return fmt.Errorf("s3 delete %q: %w", key, err)
	}
	if c.cfg.CacheDir != "" {
		_ = os.Remove(c.cachePath(key))
	}
	return nil
}

func (c *Client) Get(ctx context.Context, key string) ([]byte, error) {
	if c.cfg.CacheDir != "" {
		if buf, ok := c.readCache(key); ok {
			return buf, nil
		}
	}

	obj, err := c.mc.GetObject(ctx, c.cfg.Bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("s3 get %q: %w", key, err)
	}
	defer obj.Close()
	buf, err := io.ReadAll(obj)
	if err != nil {
		return nil, fmt.Errorf("s3 get %q read: %w", key, err)
	}

	if c.cfg.CacheDir != "" {
		c.writeCache(key, buf) // best-effort; log-only on failure
	}
	return buf, nil
}

// cachePath maps <bucket, key> to a flat filename under CacheDir. Uses
// sha256 to avoid nested directory creation and to sidestep encoding
// issues with keys containing slashes.
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
		fmt.Fprintf(os.Stderr, "s3 cache mkdir %s: %v\n", c.cfg.CacheDir, err)
		return
	}
	tmp := c.cachePath(key) + ".tmp"
	if err := os.WriteFile(tmp, buf, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "s3 cache write %s: %v\n", tmp, err)
		return
	}
	if err := os.Rename(tmp, c.cachePath(key)); err != nil {
		fmt.Fprintf(os.Stderr, "s3 cache rename %s: %v\n", tmp, err)
	}
}

// List enumerates objects under `prefix`. Empty prefix means the whole
// bucket. Recursive listing (across CommonPrefixes) is enabled to keep
// the demo output honest about deep hierarchies.
func (c *Client) List(ctx context.Context, prefix string) ([]ObjectMeta, error) {
	ch := c.mc.ListObjects(ctx, c.cfg.Bucket, minio.ListObjectsOptions{
		Prefix:    prefix,
		Recursive: true,
	})
	var out []ObjectMeta
	for obj := range ch {
		if obj.Err != nil {
			return nil, fmt.Errorf("s3 list %q: %w", prefix, obj.Err)
		}
		out = append(out, toMeta(obj))
	}
	return out, nil
}

func toMeta(o minio.ObjectInfo) ObjectMeta {
	return ObjectMeta{
		Key:          o.Key,
		Size:         o.Size,
		ETag:         o.ETag,
		LastModified: o.LastModified,
		ContentType:  o.ContentType,
	}
}
