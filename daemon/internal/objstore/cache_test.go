package objstore

import (
	"os"
	"sync/atomic"
	"testing"
	"time"
)

// TestDiskCache_InvalidateRename pins the fix for the
// stale-cache-on-rename bug: prior to the fix, only oldKey was
// invalidated, so a Get(newKey) after an overwrite rename returned
// the pre-rename body from the local disk cache.
func TestDiskCache_InvalidateRename(t *testing.T) {
	dir := t.TempDir()
	cache, err := NewDiskCache(dir, KindS3, "bucket", 0)
	if err != nil {
		t.Fatalf("NewDiskCache: %v", err)
	}
	cache.Write("old", []byte("OLD"), "e")
	cache.Write("new", []byte("PRIOR-NEW"), "e")

	cache.InvalidateRename("old", "new")

	if _, ok := cache.Read("old"); ok {
		t.Error("oldKey still cached after rename")
	}
	if _, ok := cache.Read("new"); ok {
		t.Error("newKey still cached after rename — regression: overwrite rename would return stale body")
	}
}

// TestDiskCache_Eviction pins S-07: when maxBytes is set, the oldest
// cache entries are evicted to make room before each Write.
// fileLen is the on-disk size of an n-byte body written with etag "e": the
// header (AC-40) plus the body.
func fileLen(n int) int64 { return int64(len(cacheHeader("e", int64(n))) + n) }

func TestDiskCache_Eviction(t *testing.T) {
	dir := t.TempDir()
	payload := []byte("0123456789") // 10 bytes each

	// Cap at 30 bytes (hwm=27, lwm=21 with AA-05 watermarks).
	// a+b=20 B ≤ hwm: no eviction. Writing c(10 B): 20+10=30 > hwm →
	// slow path; need=20+10-21=9 → evict a(10 B) only, result [b,c].
	cache, err := NewDiskCache(dir, KindS3, "bucket", 3*fileLen(10)) // 30 B of bodies, scaled by the header
	if err != nil {
		t.Fatalf("NewDiskCache: %v", err)
	}

	cache.Write("a", payload, "e")
	time.Sleep(2 * time.Millisecond) // ensure a's mtime < b's on any fs
	cache.Write("b", payload, "e")

	if _, ok := cache.Read("a"); !ok {
		t.Fatal("key a should be present after two writes within limit")
	}
	if _, ok := cache.Read("b"); !ok {
		t.Fatal("key b should be present after two writes within limit")
	}

	// Writing c (10 B): 20+10=30 > hwm(27), evict a (oldest, need=9).
	// Result: [b,c] at 20 B (= lwm, 1 entry evicted as expected).
	cache.Write("c", payload, "e")

	if _, ok := cache.Read("a"); ok {
		t.Error("key a should have been evicted to make room for c")
	}
	if _, ok := cache.Read("b"); !ok {
		t.Error("key b should still be present after eviction of a")
	}
	if _, ok := cache.Read("c"); !ok {
		t.Error("key c should be present after being written")
	}
}

// TestDiskCache_UsedBytesTracking pins V2-01: the in-memory usedBytes
// counter must stay in sync with the on-disk total so evictIfNeeded can
// skip os.ReadDir on every Write when under the cap.
func TestDiskCache_UsedBytesTracking(t *testing.T) {
	dir := t.TempDir()
	cache, err := NewDiskCache(dir, KindS3, "bucket", 1024)
	if err != nil {
		t.Fatalf("NewDiskCache: %v", err)
	}
	if cache.usedBytes == nil {
		t.Fatal("usedBytes must be non-nil when maxBytes > 0")
	}
	if got := atomic.LoadInt64(cache.usedBytes); got != 0 {
		t.Errorf("initial usedBytes = %d, want 0", got)
	}

	payload := []byte("hello") // 5 bytes
	cache.Write("key1", payload, "e")
	if got := atomic.LoadInt64(cache.usedBytes); got != fileLen(5) {
		t.Errorf("after Write(5 B), usedBytes = %d, want %d", got, fileLen(5))
	}
	cache.Write("key2", payload, "e")
	if got := atomic.LoadInt64(cache.usedBytes); got != 2*fileLen(5) {
		t.Errorf("after 2nd Write(5 B), usedBytes = %d, want %d", got, 2*fileLen(5))
	}

	cache.Invalidate("key1")
	if got := atomic.LoadInt64(cache.usedBytes); got != fileLen(5) {
		t.Errorf("after Invalidate(key1), usedBytes = %d, want %d", got, fileLen(5))
	}

	// Overwriting an existing key must replace the old size, not add.
	cache.Write("key2", []byte("hi"), "e") // 2 bytes, overwrites 5 bytes
	if got := atomic.LoadInt64(cache.usedBytes); got != fileLen(5)+fileLen(2)-fileLen(5) {
		t.Errorf("after overwrite Write(2 B over 5 B), usedBytes = %d, want %d", got, fileLen(2))
	}
}

// TestDiskCache_UsedBytesSeededFromDisk pins V2-01: when a cache
// directory already contains files (daemon restart), NewDiskCache must
// seed usedBytes from the actual disk contents so the fast path
// doesn't skip a needed eviction on the first Write.
func TestDiskCache_UsedBytesSeededFromDisk(t *testing.T) {
	dir := t.TempDir()
	// First instance: write 20 bytes across two keys.
	c1, err := NewDiskCache(dir, KindS3, "bucket", 100)
	if err != nil {
		t.Fatalf("NewDiskCache (first): %v", err)
	}
	c1.Write("a", make([]byte, 10), "e")
	c1.Write("b", make([]byte, 10), "e")

	// Second instance simulates a daemon restart with the same dir.
	c2, err := NewDiskCache(dir, KindS3, "bucket", 100)
	if err != nil {
		t.Fatalf("NewDiskCache (restart): %v", err)
	}
	if c2.usedBytes == nil {
		t.Fatal("usedBytes must be non-nil when maxBytes > 0")
	}
	if got := atomic.LoadInt64(c2.usedBytes); got != 2*fileLen(10) {
		t.Errorf("after restart, usedBytes = %d, want %d (seeded from disk)", got, 2*fileLen(10))
	}
}

// TestDiskCache_WatermarkBatchEviction pins AA-05: when the high watermark
// is exceeded, eviction removes enough entries to reach the low watermark,
// not just enough to fit the new write. This amortises ReadDir cost and
// matches OS page-cache / SSD-staging eviction patterns.
func TestDiskCache_WatermarkBatchEviction(t *testing.T) {
	dir := t.TempDir()
	payload := []byte("0123456789") // 10 bytes each

	// maxBytes=100 → hwm=90, lwm=70.
	// Write a..h (8×10=80 B). Still under hwm.
	// Write i (10 B): 80+10=90 ≤ hwm(90) → no eviction. After: 90 B.
	// Write j (10 B): 90+10=100 > hwm(90) → slow path.
	//   total=90, need=90+10-70=30 → evict a,b,c (3×10 B). After: 70 B.
	cache, err := NewDiskCache(dir, KindS3, "bucket", 10*fileLen(10))
	if err != nil {
		t.Fatalf("NewDiskCache: %v", err)
	}

	keys := []string{"a", "b", "c", "d", "e", "f", "g", "h", "i"}
	for _, k := range keys {
		time.Sleep(2 * time.Millisecond) // distinct mtimes for stable sort
		cache.Write(k, payload, "e")
	}
	// All 9 entries (90 B) must still be present — at the high watermark.
	for _, k := range keys {
		if _, ok := cache.Read(k); !ok {
			t.Errorf("key %q should be present before batch-eviction write", k)
		}
	}

	// Write j (10 B) crosses hwm → batch eviction to lwm(70 B).
	// Oldest 3 entries (a, b, c) are evicted.
	time.Sleep(2 * time.Millisecond)
	cache.Write("j", payload, "e")

	for _, k := range []string{"a", "b", "c"} {
		if _, ok := cache.Read(k); ok {
			t.Errorf("key %q should have been batch-evicted to reach low watermark", k)
		}
	}
	for _, k := range []string{"d", "e", "f", "g", "h", "i", "j"} {
		if _, ok := cache.Read(k); !ok {
			t.Errorf("key %q should still be present after batch-eviction", k)
		}
	}
}

// TestDiskCache_EvictionDisabled confirms that maxBytes=0 never evicts.
func TestDiskCache_EvictionDisabled(t *testing.T) {
	dir := t.TempDir()
	payload := make([]byte, 1024*1024) // 1 MiB each

	cache, err := NewDiskCache(dir, KindS3, "bucket", 0)
	if err != nil {
		t.Fatalf("NewDiskCache: %v", err)
	}
	for i := 0; i < 5; i++ {
		cache.Write(string(rune('a'+i)), payload, "e")
	}
	for i := 0; i < 5; i++ {
		if _, ok := cache.Read(string(rune('a' + i))); !ok {
			t.Errorf("key %c evicted unexpectedly with maxBytes=0", 'a'+i)
		}
	}
	_ = os.RemoveAll(dir)
}

// TestDiskCache_WriteAsync checks the write-behind path (AC-52): the body
// lands after WaitAsync, and an Invalidate that races with it wins, so a
// Put/Delete is never followed by a stale cached body.
func TestDiskCache_WriteAsync(t *testing.T) {
	dir := t.TempDir()
	cache, err := NewDiskCache(dir, KindS3, "bucket", 1<<20)
	if err != nil {
		t.Fatalf("NewDiskCache: %v", err)
	}
	cache.WriteAsync("k", []byte("BODY"), "e")
	cache.WaitAsync()
	if got, ok := cache.Read("k"); !ok || string(got) != "BODY" {
		t.Fatalf("Read after WriteAsync = %q, %v", got, ok)
	}

	for i := 0; i < 200; i++ {
		cache.WriteAsync("race", []byte("STALE"), "e")
		cache.Invalidate("race")
		cache.WaitAsync()
		if _, ok := cache.Read("race"); ok {
			t.Fatalf("iteration %d: Invalidate lost to an async write", i)
		}
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 1 {
		t.Errorf("leftover files in cache dir: %d, want 1", len(ents))
	}
}

// AC-40: a body cached from one object must not be served once the backend
// reports another (an overwrite by someone else).
func TestDiskCache_StaleBodyIsNotServed(t *testing.T) {
	newCache := func(dir string) DiskCache {
		c, err := NewDiskCache(dir, KindS3, "bucket", 0)
		if err != nil {
			t.Fatalf("NewDiskCache: %v", err)
		}
		return c
	}
	body := []byte("0123456789")

	t.Run("a key never observed is trusted", func(t *testing.T) {
		c := newCache(t.TempDir())
		c.Write("k", body, "E1")
		if got, ok := c.Read("k"); !ok || string(got) != string(body) {
			t.Fatalf("Read = %q, %v", got, ok)
		}
	})
	t.Run("same ETag and size: hit", func(t *testing.T) {
		c := newCache(t.TempDir())
		c.Write("k", body, "E1")
		c.Observe("k", "E1", int64(len(body)))
		if _, ok := c.Read("k"); !ok {
			t.Fatal("miss on an unchanged object")
		}
	})
	t.Run("a different ETag is a miss and the file goes", func(t *testing.T) {
		c := newCache(t.TempDir())
		c.Write("k", body, "E1")
		c.Observe("k", "E2", int64(len(body)))
		if _, ok := c.Read("k"); ok {
			t.Fatal("served a body of the old object")
		}
		if _, err := os.Stat(c.path("k")); !os.IsNotExist(err) {
			t.Fatalf("stale file kept: %v", err)
		}
	})
	t.Run("a different size is a miss", func(t *testing.T) {
		c := newCache(t.TempDir())
		c.Write("k", body, "E1")
		c.Observe("k", "E1", int64(len(body))+5)
		if _, ok := c.Read("k"); ok {
			t.Fatal("served a body of the wrong size")
		}
	})
	t.Run("a body left by an earlier run", func(t *testing.T) {
		dir := t.TempDir()
		old := newCache(dir)
		old.Write("k", body, "E1")
		// The restarted daemon lists the bucket first: the object is E2 now.
		c := newCache(dir)
		c.Observe("k", "E2", 4)
		if _, ok := c.Read("k"); ok {
			t.Fatal("a restart served the old body")
		}
	})
	t.Run("a file without a header (older format) is a miss", func(t *testing.T) {
		c := newCache(t.TempDir())
		if err := os.WriteFile(c.path("k"), body, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, ok := c.Read("k"); ok {
			t.Fatal("served a headerless file")
		}
	})
	t.Run("a truncated body is a miss", func(t *testing.T) {
		c := newCache(t.TempDir())
		c.Write("k", body, "E1")
		raw, _ := os.ReadFile(c.path("k"))
		if err := os.WriteFile(c.path("k"), raw[:len(raw)-3], 0o600); err != nil {
			t.Fatal(err)
		}
		if _, ok := c.Read("k"); ok {
			t.Fatal("served a truncated body")
		}
	})
}
