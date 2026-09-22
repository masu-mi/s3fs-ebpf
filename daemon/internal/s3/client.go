// Package s3 is the daemon-side object-store client. Week 2 (d) wires
// it to a real minio-go v7 backend so the Fast Path miss handler can
// eventually round-trip S3 metadata into the arena. Compatible with
// MinIO, AWS S3, or any S3-compatible endpoint.
package s3

import (
	"context"
	"fmt"
	"io"
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
func (c *Client) Get(ctx context.Context, key string) ([]byte, error) {
	obj, err := c.mc.GetObject(ctx, c.cfg.Bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("s3 get %q: %w", key, err)
	}
	defer obj.Close()
	buf, err := io.ReadAll(obj)
	if err != nil {
		return nil, fmt.Errorf("s3 get %q read: %w", key, err)
	}
	return buf, nil
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
