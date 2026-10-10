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

	// Cap at 25 bytes — fits 2 entries (20 B) but not 3 (30 B).
	cache, err := NewDiskCache(dir, KindS3, "bucket", 25)
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

	// Writing c (10 B) would push total to 30 B > 25 B cap.
	// The oldest file (a) must be evicted first.
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
