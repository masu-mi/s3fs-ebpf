// Backend selection flags and environment helpers shared by subcommands.

package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/bbsakura/s3fs-ebpf/daemon/internal/gcs"
	"github.com/bbsakura/s3fs-ebpf/daemon/internal/objstore"
	"github.com/bbsakura/s3fs-ebpf/daemon/internal/s3"
)

// backendFlags registers the shared flags for backend selection
// (S3-compat via internal/s3 or native GCS) and returns a callback
// that constructs the concrete [objstore.Backend]. Backend-specific
// flags are ignored under the other --backend so envvars can stay
// set for both without conflict.
func backendFlags(fs *flag.FlagSet) func() (objstore.Backend, error) {
	backend := fs.String("backend", envOr("S3FS_BACKEND", objstore.KindS3),
		"object-store backend: s3 (MinIO / AWS / any S3-compatible endpoint) or gcs (Google Cloud Storage)")
	endpoint := fs.String("endpoint", envOr("S3FS_ENDPOINT", "localhost:9000"),
		"[s3] S3-compatible endpoint (host:port, no scheme — use --ssl for https)")
	bucket := fs.String("bucket", envOr("S3FS_BUCKET", "s3fs-test"), "Bucket name (both backends)")
	access := fs.String("access-key", envOr("S3FS_ACCESS_KEY", "minioadmin"), "[s3] Access key")
	secret := fs.String("secret-key", envOr("S3FS_SECRET_KEY", "minioadmin"), "[s3] Secret key")
	ssl := fs.Bool("ssl", false, "[s3] Use HTTPS")
	cacheDir := fs.String("cache-dir", envOr("S3FS_CACHE_DIR", ""),
		"local staging directory (enables use_cache-equivalent short-circuit; both backends)")
	diskCacheCapacity := fs.Int64("disk-cache-capacity", envInt64("S3FS_DISK_CACHE_CAPACITY", 0),
		"disk cache size cap in bytes (0 = unlimited, S-07)")
	// GOOGLE_APPLICATION_CREDENTIALS is honored by the GCS SDK's ADC
	// chain — don't pre-read it here or an unset flag with a set env
	// would bypass metadata-server / gcloud login fallbacks.
	gcsCreds := fs.String("gcs-credentials", "",
		"[gcs] path to a service-account JSON key file. Empty falls back to Application Default Credentials (GOOGLE_APPLICATION_CREDENTIALS, gcloud login, GCE metadata).")
	keyPrefix := fs.String("key-prefix", envOr("S3FS_KEY_PREFIX", ""),
		"map the mount root to this bucket key prefix (e.g. 'sub/dir/'). Empty exposes the whole bucket.")
	return func() (objstore.Backend, error) {
		var (
			b   objstore.Backend
			err error
		)
		switch *backend {
		case objstore.KindS3:
			b, err = s3.New(s3.Config{
				Endpoint:      *endpoint,
				Bucket:        *bucket,
				AccessKey:     *access,
				SecretKey:     *secret,
				UseSSL:        *ssl,
				CacheDir:      *cacheDir,
				MaxCacheBytes: *diskCacheCapacity,
			})
		case objstore.KindGCS:
			b, err = gcs.New(context.Background(), gcs.Config{
				Bucket:          *bucket,
				CredentialsFile: *gcsCreds,
				CacheDir:        *cacheDir,
				MaxCacheBytes:   *diskCacheCapacity,
			})
		default:
			return nil, fmt.Errorf("unknown --backend %q (want %s or %s)",
				*backend, objstore.KindS3, objstore.KindGCS)
		}
		if err != nil {
			return nil, err
		}
		return objstore.NewPrefixed(b, *keyPrefix), nil
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt64(key string, fallback int64) int64 {
	if v := os.Getenv(key); v != "" {
		var n int64
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil {
			return n
		}
	}
	return fallback
}
