package s3

import (
	"bytes"
	"context"
	"net"
	"os"
	"testing"
	"time"
)

// AC-40: a Get must not return the body the disk cache holds for an object
// that someone else has overwritten since — neither after the next listing in
// the same run, nor after a restart that lists first.  Needs the local MinIO.
func TestGet_DiskCacheFollowsExternalOverwrite(t *testing.T) {
	if c, err := net.DialTimeout("tcp", "localhost:9000", 300*time.Millisecond); err != nil {
		t.Skip("MinIO not reachable on localhost:9000")
	} else {
		c.Close()
	}
	ctx := context.Background()
	plain := newTestClient(t) // no cache: plays "someone else"
	cacheDir := t.TempDir()
	open := func() *Client {
		c, err := New(Config{Endpoint: "localhost:9000", Bucket: bucket,
			AccessKey: "minioadmin", SecretKey: "minioadmin", CacheDir: cacheDir})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	const key = "stale-cache.bin"
	v1 := bytes.Repeat([]byte("1"), 10)
	v2 := bytes.Repeat([]byte("22"), 7) // another size, as in the ab-pcache EIO
	if err := plain.Put(ctx, key, v1); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = plain.Delete(ctx, key) })

	c := open()
	if got, err := c.Get(ctx, key); err != nil || !bytes.Equal(got, v1) {
		t.Fatalf("first Get = %q, %v", got, err)
	}
	c.cache.WaitAsync()
	if ents, _ := os.ReadDir(cacheDir); len(ents) == 0 {
		t.Fatal("the first Get left nothing in the disk cache: the test would not exercise it")
	}
	if got, err := c.Get(ctx, key); err != nil || !bytes.Equal(got, v1) { // from the cache
		t.Fatalf("cached Get = %q, %v", got, err)
	}

	if err := plain.Put(ctx, key, v2); err != nil { // overwritten elsewhere
		t.Fatal(err)
	}
	if _, err := c.List(ctx, ""); err != nil { // the daemon's periodic sync
		t.Fatal(err)
	}
	if got, err := c.Get(ctx, key); err != nil || !bytes.Equal(got, v2) {
		t.Fatalf("Get after a listing = %q, %v; want the new object", got, err)
	}
	c.cache.WaitAsync()

	// Overwritten again while the daemon is down; the restart lists first.
	if err := plain.Put(ctx, key, v1); err != nil {
		t.Fatal(err)
	}
	c2 := open()
	if _, err := c2.List(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if got, err := c2.Get(ctx, key); err != nil || !bytes.Equal(got, v1) {
		t.Fatalf("Get after a restart = %q, %v; want the new object", got, err)
	}
}
