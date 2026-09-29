package objstore

import "testing"

// TestDiskCache_InvalidateRename pins the fix for the
// stale-cache-on-rename bug: prior to the fix, only oldKey was
// invalidated, so a Get(newKey) after an overwrite rename returned
// the pre-rename body from the local disk cache.
func TestDiskCache_InvalidateRename(t *testing.T) {
	dir := t.TempDir()
	cache, err := NewDiskCache(dir, KindS3, "bucket")
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
