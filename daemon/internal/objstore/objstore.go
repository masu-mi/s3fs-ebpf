// Package objstore is the daemon-side object-store abstraction. The
// Fast Path miss handler, prewarm, flush, delete, and rename loops
// all speak this interface so a single build supports both S3-flavor
// backends (MinIO / AWS / any S3-compatible endpoint via internal/s3)
// and Google Cloud Storage (via internal/gcs).
package objstore

import (
	"context"
	"time"
)

// ObjectMeta is the subset of backend metadata the daemon caches.
// Fields mirror what both S3 (HEAD Object) and GCS (Object.Attrs)
// surface; anything backend-specific stays inside the implementing
// package.
type ObjectMeta struct {
	Key          string
	Size         int64
	ETag         string
	LastModified time.Time
	ContentType  string
}

// Backend is the minimal object-store surface the daemon needs. All
// methods take a context so callers can bound the round-trip; the
// implementations use it for HTTP/gRPC deadline propagation.
type Backend interface {
	// Bucket returns the bound bucket name, used only for log output
	// and the metrics `bucket=` label.
	Bucket() string

	// Stat is a HEAD-equivalent — returns ObjectMeta without pulling
	// the body. Prewarm uses this to size the arena buffer before
	// the Get.
	Stat(ctx context.Context, key string) (ObjectMeta, error)

	// Get pulls the whole object into memory. Suitable for
	// small/medium objects (< partialAdmitThreshold); larger objects
	// use GetRange.
	Get(ctx context.Context, key string) ([]byte, error)

	// GetRange fetches `size` bytes starting at `offset` from `key`.
	// D-01 partial admit uses this to load a single chunk of a large
	// object into the arena instead of the whole body.
	GetRange(ctx context.Context, key string, offset, size int64) ([]byte, error)

	// Put uploads `data` under `key` as a single-shot write. Called
	// by the flush coalescer / fsync path.
	Put(ctx context.Context, key string, data []byte) error

	// Delete removes `key`. Called by the delete-drain loop after
	// kmod's .unlink enqueues an event.
	Delete(ctx context.Context, key string) error

	// Rename copies `oldKey` to `newKey` then removes `oldKey`.
	// Neither S3 nor GCS have atomic rename; a partial failure
	// leaves both keys and the caller logs. Called by the
	// rename-drain loop.
	Rename(ctx context.Context, oldKey, newKey string) error

	// List enumerates objects under `prefix`. Empty prefix means the
	// whole bucket. Both implementations return a recursive listing
	// (no CommonPrefixes collapse).
	List(ctx context.Context, prefix string) ([]ObjectMeta, error)
}
