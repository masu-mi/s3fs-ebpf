package s3

import (
	"testing"

	"github.com/bbsakura/s3fs-ebpf/daemon/internal/objstore"
)

// TestInvalidateAfterRename_ClearsBothKeys pins the fix for the
// stale-cache-on-rename bug: prior to the fix, only oldKey was
// invalidated, so a Get(newKey) that happened after an overwrite
// rename returned the pre-rename body from the local disk cache.
func TestInvalidateAfterRename_ClearsBothKeys(t *testing.T) {
	dir := t.TempDir()
	cache, err := objstore.NewDiskCache(dir, objstore.KindS3, "bucket")
	if err != nil {
		t.Fatalf("NewDiskCache: %v", err)
	}
	cache.Write("old", []byte("OLD"))
	cache.Write("new", []byte("PRIOR-NEW"))

	c := &Client{cache: cache}
	c.invalidateAfterRename("old", "new")

	if _, ok := cache.Read("old"); ok {
		t.Error("oldKey still cached after rename")
	}
	if _, ok := cache.Read("new"); ok {
		t.Error("newKey still cached after rename — regression: overwrite rename would return stale body")
	}
}
