package objstore

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
)

// DiskCache is the local staging store both backends use to satisfy
// whole-object Get without a round-trip. Freshness check is
// timestamp-of-file only (no ETag / If-None-Match); good enough to
// match s3fs-fuse's -o use_cache behavior. Cache poisoning is out of
// scope — same trust boundary as arena admits.
//
// Zero value is a valid no-op cache: all operations short-circuit
// when dir is empty.
type DiskCache struct {
	dir    string
	tag    string
	bucket string
}

// NewDiskCache returns a cache rooted at `dir`. `tag` prefixes stderr
// log lines so operators can tell which backend logged a
// write/rename failure. `bucket` namespaces cached keys so two
// backends can share `dir` without colliding on identical key names.
// Empty `dir` yields a valid no-op cache.
func NewDiskCache(dir, tag, bucket string) (DiskCache, error) {
	c := DiskCache{dir: dir, tag: tag, bucket: bucket}
	if dir == "" {
		return c, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return c, fmt.Errorf("%s cache mkdir %s: %w", tag, dir, err)
	}
	return c, nil
}

func (c *DiskCache) enabled() bool { return c != nil && c.dir != "" }

func (c *DiskCache) path(key string) string {
	h := sha256.Sum256([]byte(c.bucket + "/" + key))
	return filepath.Join(c.dir, hex.EncodeToString(h[:]))
}

// Read returns the cached body for `key`. ok=false on any error
// (missing file, permission denied, etc) — the caller falls through
// to the origin fetch.
func (c *DiskCache) Read(key string) ([]byte, bool) {
	if !c.enabled() {
		return nil, false
	}
	buf, err := os.ReadFile(c.path(key))
	if err != nil {
		return nil, false
	}
	return buf, true
}

// Write persists `buf` under `key`. Best-effort: any failure is
// logged and swallowed so the origin fetch's result still reaches
// the caller.
func (c *DiskCache) Write(key string, buf []byte) {
	if !c.enabled() {
		return
	}
	tmp := c.path(key) + ".tmp"
	if err := os.WriteFile(tmp, buf, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "%s cache write %s: %v\n", c.tag, tmp, err)
		return
	}
	if err := os.Rename(tmp, c.path(key)); err != nil {
		fmt.Fprintf(os.Stderr, "%s cache rename %s: %v\n", c.tag, tmp, err)
	}
}

// Invalidate drops the cached body for `key`. Called from Put /
// Delete / Rename so the next Get re-fetches authoritative state.
func (c *DiskCache) Invalidate(key string) {
	if !c.enabled() {
		return
	}
	_ = os.Remove(c.path(key))
}

// InvalidateRename drops both keys after a successful backend
// CopyObject+Delete. oldKey is gone from the backend; newKey's
// content just changed (rename can overwrite an existing object),
// so its cached body — if any — is stale.
func (c *DiskCache) InvalidateRename(oldKey, newKey string) {
	c.Invalidate(oldKey)
	c.Invalidate(newKey)
}
