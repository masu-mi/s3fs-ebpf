package objstore

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// DiskCache is the local staging store both backends use to satisfy
// whole-object Get without a round-trip.  Cache poisoning is out of scope —
// same trust boundary as arena admits.
//
// AC-40: each file starts with a header (format tag, the ETag and size of the
// object the body came from).  The backends report what they see of the
// object — List, Stat, and the Get that filled the entry — through Observe;
// Read refuses (and removes) a body whose ETag or size differs from the last
// observation, or whose length is not the recorded size.  So an object
// overwritten by someone else is noticed at the next listing (the daemon
// lists at startup, before any Get, and then every --sync-every), and a cache
// left on disk by an earlier run cannot serve the old body.  A key never
// observed is trusted: that is the window before the first listing.
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
	remote     *sync.Map // key -> remoteState, see Observe
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
	c.remote = &sync.Map{}
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

// remoteState is what the backend last reported for a key.
type remoteState struct {
	etag string
	size int64
}

// Observe records the ETag and size the backend reports for key.  Read uses
// the latest one to tell whether a cached body is still the object.
func (c *DiskCache) Observe(key, etag string, size int64) {
	if !c.enabled() || etag == "" {
		return
	}
	c.remote.Store(key, remoteState{etag: etag, size: size})
}

// Read returns the cached body for `key`. ok=false on any error
// (missing file, permission denied, a file without a valid header, a body
// that does not match the recorded size, or one the backend has since
// reported as a different object) — the caller falls through to the origin
// fetch.
func (c *DiskCache) Read(key string) ([]byte, bool) {
	if !c.enabled() {
		return nil, false
	}
	p := c.path(key)
	raw, err := os.ReadFile(p)
	if err != nil {
		return nil, false
	}
	etag, size, body, ok := parseCacheFile(raw)
	if ok && int64(len(body)) == size {
		if v, seen := c.remote.Load(key); !seen {
			return body, true
		} else if rs := v.(remoteState); rs.etag == etag && rs.size == size {
			return body, true
		}
	}
	// Stale or unreadable: drop it so the refetch replaces it.
	c.Invalidate(key)
	return nil, false
}

const cacheMagic = "s3fscache1\n"

// cacheHeader is what precedes the body in a cache file.
func cacheHeader(etag string, size int64) []byte {
	return []byte(cacheMagic + etag + "\n" + strconv.FormatInt(size, 10) + "\n")
}

func parseCacheFile(raw []byte) (etag string, size int64, body []byte, ok bool) {
	if !bytes.HasPrefix(raw, []byte(cacheMagic)) {
		return "", 0, nil, false
	}
	rest := raw[len(cacheMagic):]
	i := bytes.IndexByte(rest, '\n')
	if i < 0 {
		return "", 0, nil, false
	}
	etag = string(rest[:i])
	rest = rest[i+1:]
	j := bytes.IndexByte(rest, '\n')
	if j < 0 {
		return "", 0, nil, false
	}
	n, err := strconv.ParseInt(string(rest[:j]), 10, 64)
	if err != nil || n < 0 {
		return "", 0, nil, false
	}
	return etag, n, rest[j+1:], true
}

// Write persists `buf` under `key`. Best-effort: any failure is
// logged and swallowed so the origin fetch's result still reaches
// the caller.
func (c *DiskCache) Write(key string, buf []byte, etag string) {
	if !c.enabled() {
		return
	}
	c.write(key, buf, etag, nil)
}

// WriteAsync is Write moved off the caller's path (AC-52: the disk write
// used to sit inside the cold read's latency). buf must not be modified
// afterwards. The write is dropped when maxAsyncWrites are already in
// flight, and discarded if Invalidate runs before it commits.
func (c *DiskCache) WriteAsync(key string, buf []byte, etag string) {
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
		c.write(key, buf, etag, pw)
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
func (c *DiskCache) write(key string, buf []byte, etag string, pw *pendingWrite) {
	hdr := cacheHeader(etag, int64(len(buf)))
	c.evictIfNeeded(int64(len(hdr) + len(buf)))

	// Capture the old file size before overwriting (0 for new keys).
	// Needed to keep usedBytes accurate when the same key is re-written.
	var oldSize int64
	if c.usedBytes != nil {
		if info, err := os.Stat(c.path(key)); err == nil {
			oldSize = info.Size()
		}
	}

	tmp := fmt.Sprintf("%s.%d.tmp", c.path(key), c.async.seq.Add(1))
	if err := writeFileParts(tmp, hdr, buf); err != nil {
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
		atomic.AddInt64(c.usedBytes, int64(len(hdr)+len(buf))-oldSize)
	}
}

// writeFileParts writes head then body to path without joining them.
func writeFileParts(path string, head, body []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err = f.Write(head); err == nil {
		_, err = f.Write(body)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
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
