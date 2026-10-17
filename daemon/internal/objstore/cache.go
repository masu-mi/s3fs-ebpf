package objstore

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// DiskCache is the local staging store both backends use to satisfy
// whole-object Get without a round-trip. Freshness check is
// timestamp-of-file only (no ETag / If-None-Match); good enough to
// match s3fs-fuse's -o use_cache behavior. Cache poisoning is out of
// scope — same trust boundary as arena admits.
//
// Zero value is a valid no-op cache: all operations short-circuit
// when dir is empty.
//
// usedBytes is a heap-allocated atomic counter that tracks the current
// total on-disk size. It lets evictIfNeeded skip os.ReadDir entirely
// when under the cap (V2-01 fast path). Stored as *int64 rather than
// sync/atomic.Int64 so that the DiskCache value can be assigned once
// from NewDiskCache without triggering go vet noCopy warnings.
//
// AA-05: watermark-based eviction. evictIfNeeded fires the slow path
// (ReadDir + mtime sort + remove) when usedBytes + newBytes exceeds
// highWMFrac × maxBytes, and evicts down to lowWMFrac × maxBytes.
// This batches evictions: instead of making exactly one write's worth
// of room, we clear a 20 %-point band (90→70 % of maxBytes) per cycle,
// matching the pattern used by OS page-cache and SSD staging tiers.
// Both fractions are set to sensible defaults in NewDiskCache.
type DiskCache struct {
	dir        string
	tag        string
	bucket     string
	maxBytes   int64
	highWMFrac float64 // trigger threshold (0.0–1.0); default 0.90
	lowWMFrac  float64 // eviction target (0.0–1.0);  default 0.70
	usedBytes  *int64  // nil when eviction disabled; atomic via sync/atomic ops
	async      *asyncState
}

// maxAsyncWrites bounds the number of write-behind goroutines (and so the
// bodies they pin in memory). A WriteAsync past the bound is dropped: the
// cache is best-effort and the next Get simply refetches.
const maxAsyncWrites = 4

// asyncState is shared by every copy of a DiskCache (the struct is
// returned and stored by value). mu orders a write-behind commit against
// Invalidate: a commit that lost the race is discarded, so an async write
// can never resurrect a body that Put / Delete / Rename just invalidated.
type asyncState struct {
	mu      sync.Mutex
	pending map[string]*pendingWrite
	sem     chan struct{}
	wg      sync.WaitGroup
	seq     atomic.Uint64 // unique tmp suffix
}

type pendingWrite struct{ cancelled bool }

// NewDiskCache returns a cache rooted at `dir`. `tag` prefixes stderr
// log lines so operators can tell which backend logged a
// write/rename failure. `bucket` namespaces cached keys so two
// backends can share `dir` without colliding on identical key names.
// `maxBytes` caps the total on-disk size; 0 means unlimited.
// Empty `dir` yields a valid no-op cache.
func NewDiskCache(dir, tag, bucket string, maxBytes int64) (DiskCache, error) {
	c := DiskCache{
		dir: dir, tag: tag, bucket: bucket, maxBytes: maxBytes,
		highWMFrac: 0.90, lowWMFrac: 0.70,
	}
	if dir == "" {
		return c, nil
	}
	c.async = &asyncState{
		pending: make(map[string]*pendingWrite),
		sem:     make(chan struct{}, maxAsyncWrites),
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return c, fmt.Errorf("%s cache mkdir %s: %w", tag, dir, err)
	}
	if maxBytes > 0 {
		// Seed the counter from the current directory so a daemon restart
		// with an existing cache doesn't undercount and skip eviction.
		used := c.scanDir()
		c.usedBytes = &used
	}
	return c, nil
}

func (c *DiskCache) enabled() bool { return c != nil && c.dir != "" }

func (c *DiskCache) path(key string) string {
	h := sha256.Sum256([]byte(c.bucket + "/" + key))
	return filepath.Join(c.dir, hex.EncodeToString(h[:]))
}

// scanDir walks the cache directory and returns the total size of all
// non-temporary files. Called once at startup to seed usedBytes, and
// again in evictIfNeeded's slow path to re-sync after external changes.
func (c *DiskCache) scanDir() int64 {
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		return 0
	}
	var total int64
	for _, e := range entries {
		if e.IsDir() || strings.HasSuffix(e.Name(), ".tmp") {
			continue
		}
		if info, err := e.Info(); err == nil {
			total += info.Size()
		}
	}
	return total
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
	c.write(key, buf, nil)
}

// WriteAsync is Write moved off the caller's path (AC-52: the disk write
// used to sit inside the cold read's latency). buf must not be modified
// afterwards. The write is dropped when maxAsyncWrites are already in
// flight, and discarded if Invalidate runs before it commits.
func (c *DiskCache) WriteAsync(key string, buf []byte) {
	if !c.enabled() {
		return
	}
	st := c.async
	select {
	case st.sem <- struct{}{}:
	default:
		return
	}
	pw := &pendingWrite{}
	st.mu.Lock()
	if old := st.pending[key]; old != nil {
		old.cancelled = true
	}
	st.pending[key] = pw
	st.mu.Unlock()
	st.wg.Add(1)
	go func() {
		defer st.wg.Done()
		defer func() { <-st.sem }()
		c.write(key, buf, pw)
		st.mu.Lock()
		if st.pending[key] == pw {
			delete(st.pending, key)
		}
		st.mu.Unlock()
	}()
}

// WaitAsync blocks until every WriteAsync issued so far has finished.
func (c *DiskCache) WaitAsync() {
	if c.enabled() {
		c.async.wg.Wait()
	}
}

// write stores buf under key. A non-nil pw makes the final commit
// conditional on pw not having been cancelled by Invalidate.
func (c *DiskCache) write(key string, buf []byte, pw *pendingWrite) {
	c.evictIfNeeded(int64(len(buf)))

	// Capture the old file size before overwriting (0 for new keys).
	// Needed to keep usedBytes accurate when the same key is re-written.
	var oldSize int64
	if c.usedBytes != nil {
		if info, err := os.Stat(c.path(key)); err == nil {
			oldSize = info.Size()
		}
	}

	tmp := fmt.Sprintf("%s.%d.tmp", c.path(key), c.async.seq.Add(1))
	if err := os.WriteFile(tmp, buf, 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "%s cache write %s: %v\n", c.tag, tmp, err)
		_ = os.Remove(tmp)
		return
	}
	if pw != nil {
		c.async.mu.Lock()
		defer c.async.mu.Unlock()
		if pw.cancelled {
			_ = os.Remove(tmp)
			return
		}
	}
	if err := os.Rename(tmp, c.path(key)); err != nil {
		fmt.Fprintf(os.Stderr, "%s cache rename %s: %v\n", c.tag, tmp, err)
		_ = os.Remove(tmp)
		return
	}
	if c.usedBytes != nil {
		atomic.AddInt64(c.usedBytes, int64(len(buf))-oldSize)
	}
}

// Invalidate drops the cached body for `key`. Called from Put /
// Delete / Rename so the next Get re-fetches authoritative state.
func (c *DiskCache) Invalidate(key string) {
	if !c.enabled() {
		return
	}
	p := c.path(key)
	c.async.mu.Lock()
	defer c.async.mu.Unlock()
	if pw := c.async.pending[key]; pw != nil {
		pw.cancelled = true
	}
	if c.usedBytes != nil {
		if info, err := os.Stat(p); err == nil {
			atomic.AddInt64(c.usedBytes, -info.Size())
		}
	}
	_ = os.Remove(p)
}

// InvalidateRename drops both keys after a successful backend
// CopyObject+Delete. oldKey is gone from the backend; newKey's
// content just changed (rename can overwrite an existing object),
// so its cached body — if any — is stale.
func (c *DiskCache) InvalidateRename(oldKey, newKey string) {
	c.Invalidate(oldKey)
	c.Invalidate(newKey)
}

// evictIfNeeded removes the oldest cache files (by mtime) until the
// post-write total would fall at or below the low watermark.
//
// Fast path (V2-01): when the in-memory usedBytes counter is below the
// high watermark, the function returns without any I/O.
//
// Slow path (AA-05): fires when usedBytes + newBytes exceeds
// highWMFrac × maxBytes. It re-syncs the counter from disk, then evicts
// oldest-first until usedBytes + newBytes ≤ lowWMFrac × maxBytes.
// Evicting to 70 % rather than to maxBytes means fewer eviction cycles
// under sustained write pressure (amortises ReadDir cost across writes).
func (c *DiskCache) evictIfNeeded(newBytes int64) {
	if c.maxBytes <= 0 || c.usedBytes == nil {
		return
	}
	hwm := int64(float64(c.maxBytes) * c.highWMFrac)
	// Fast path: counter shows we are below the high watermark.
	if atomic.LoadInt64(c.usedBytes)+newBytes <= hwm {
		return
	}
	// Slow path: scan directory for authoritative sizes and mtime order.
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		return
	}
	type fi struct {
		path  string
		size  int64
		mtime time.Time
	}
	var files []fi
	var total int64
	for _, e := range entries {
		if e.IsDir() || strings.HasSuffix(e.Name(), ".tmp") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, fi{
			path:  filepath.Join(c.dir, e.Name()),
			size:  info.Size(),
			mtime: info.ModTime(),
		})
		total += info.Size()
	}
	// Re-sync the counter to correct any drift accumulated since startup.
	atomic.StoreInt64(c.usedBytes, total)

	lwm := int64(float64(c.maxBytes) * c.lowWMFrac)
	need := total + newBytes - lwm
	if need <= 0 {
		return
	}
	sort.Slice(files, func(i, j int) bool {
		return files[i].mtime.Before(files[j].mtime)
	})
	for _, f := range files {
		if need <= 0 {
			break
		}
		if err := os.Remove(f.path); err == nil {
			need -= f.size
			atomic.AddInt64(c.usedBytes, -f.size)
		}
	}
}
