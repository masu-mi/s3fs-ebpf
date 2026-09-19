// Package s3 wraps the object-store slow path. The skeleton keeps this
// pure stdlib so the Week 0 build has zero external deps; Week 1 swaps
// in github.com/minio/minio-go/v7.
package s3

import "errors"

// Config is the connection settings for the object store.
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
}

// ErrNotImplemented is returned until the real client lands.
var ErrNotImplemented = errors.New("s3 client not implemented in Week 0 skeleton")

// New constructs a Client. Skeleton just remembers the config.
func New(cfg Config) (*Client, error) {
	return &Client{cfg: cfg}, nil
}

// Get returns the object bytes for key. Skeleton stub.
func (c *Client) Get(key string) ([]byte, error) {
	return nil, ErrNotImplemented
}
