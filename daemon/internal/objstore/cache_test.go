package objstore

import (
	"os"
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
