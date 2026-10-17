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
	cache.Write("old", []byte("OLD"))
	cache.Write("new", []byte("PRIOR-NEW"))

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
func TestDiskCache_Eviction(t *testing.T) {
	dir := t.TempDir()
	payload := []byte("0123456789") // 10 bytes each

	// Cap at 30 bytes (hwm=27, lwm=21 with AA-05 watermarks).
	// a+b=20 B ≤ hwm: no eviction. Writing c(10 B): 20+10=30 > hwm →
	// slow path; need=20+10-21=9 → evict a(10 B) only, result [b,c].
	cache, err := NewDiskCache(dir, KindS3, "bucket", 30)
	if err != nil {
		t.Fatalf("NewDiskCache: %v", err)
	}

	cache.Write("a", payload)
	time.Sleep(2 * time.Millisecond) // ensure a's mtime < b's on any fs
	cache.Write("b", payload)

	if _, ok := cache.Read("a"); !ok {
		t.Fatal("key a should be present after two writes within limit")
	}
	if _, ok := cache.Read("b"); !ok {
		t.Fatal("key b should be present after two writes within limit")
	}

	// Writing c (10 B): 20+10=30 > hwm(27), evict a (oldest, need=9).
	// Result: [b,c] at 20 B (= lwm, 1 entry evicted as expected).
	cache.Write("c", payload)

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
	cache.Write("key1", payload)
	if got := atomic.LoadInt64(cache.usedBytes); got != 5 {
		t.Errorf("after Write(5 B), usedBytes = %d, want 5", got)
	}
	cache.Write("key2", payload)
	if got := atomic.LoadInt64(cache.usedBytes); got != 10 {
		t.Errorf("after 2nd Write(5 B), usedBytes = %d, want 10", got)
	}

	cache.Invalidate("key1")
	if got := atomic.LoadInt64(cache.usedBytes); got != 5 {
		t.Errorf("after Invalidate(key1), usedBytes = %d, want 5", got)
	}

	// Overwriting an existing key must replace the old size, not add.
	cache.Write("key2", []byte("hi")) // 2 bytes, overwrites 5 bytes
	if got := atomic.LoadInt64(cache.usedBytes); got != 2 {
		t.Errorf("after overwrite Write(2 B over 5 B), usedBytes = %d, want 2", got)
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
	c1.Write("a", make([]byte, 10))
	c1.Write("b", make([]byte, 10))

	// Second instance simulates a daemon restart with the same dir.
	c2, err := NewDiskCache(dir, KindS3, "bucket", 100)
	if err != nil {
		t.Fatalf("NewDiskCache (restart): %v", err)
	}
	if c2.usedBytes == nil {
		t.Fatal("usedBytes must be non-nil when maxBytes > 0")
	}
	if got := atomic.LoadInt64(c2.usedBytes); got != 20 {
		t.Errorf("after restart, usedBytes = %d, want 20 (seeded from disk)", got)
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
	cache, err := NewDiskCache(dir, KindS3, "bucket", 100)
	if err != nil {
		t.Fatalf("NewDiskCache: %v", err)
	}

	keys := []string{"a", "b", "c", "d", "e", "f", "g", "h", "i"}
	for _, k := range keys {
		time.Sleep(2 * time.Millisecond) // distinct mtimes for stable sort
		cache.Write(k, payload)
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
	cache.Write("j", payload)

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
		cache.Write(string(rune('a'+i)), payload)
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
	cache.WriteAsync("k", []byte("BODY"))
	cache.WaitAsync()
	if got, ok := cache.Read("k"); !ok || string(got) != "BODY" {
		t.Fatalf("Read after WriteAsync = %q, %v", got, ok)
	}

	for i := 0; i < 200; i++ {
		cache.WriteAsync("race", []byte("STALE"))
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
