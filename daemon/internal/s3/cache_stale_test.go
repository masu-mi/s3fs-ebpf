package s3

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
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

// AC-75: a chunk fetched by GetRange lands in the disk cache and is served
// from it (even once the object is gone from the bucket), and a chunk of an
// overwritten object is not served after the next listing.  Needs the local
// MinIO.
func TestGetRange_DiskCache(t *testing.T) {
	plain := newTestClient(t) // no cache: plays "someone else"
	ctx := context.Background()
	c, err := New(Config{Endpoint: "localhost:9000", Bucket: bucket,
		AccessKey: "minioadmin", SecretKey: "minioadmin", CacheDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	const key = "range-cache.bin"
	v1 := bytes.Repeat([]byte("1"), 64)
	v2 := bytes.Repeat([]byte("2"), 64)
	if err := plain.Put(ctx, key, v1); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = plain.Delete(ctx, key) })
	if _, err := c.List(ctx, ""); err != nil { // learn the ETag
		t.Fatal(err)
	}
	if got, err := c.GetRange(ctx, key, 16, 32); err != nil || !bytes.Equal(got, v1[16:48]) {
		t.Fatalf("GetRange = %q, %v", got, err)
	}
	c.cache.WaitAsync()
	if err := plain.Delete(ctx, key); err != nil { // only the cache can answer now
		t.Fatal(err)
	}
	if got, err := c.GetRange(ctx, key, 16, 32); err != nil || !bytes.Equal(got, v1[16:48]) {
		t.Fatalf("cached GetRange = %q, %v", got, err)
	}
	if err := plain.Put(ctx, key, v2); err != nil {
		t.Fatal(err)
	}
	if _, err := c.List(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if got, err := c.GetRange(ctx, key, 16, 32); err != nil || !bytes.Equal(got, v2[16:48]) {
		t.Fatalf("GetRange after overwrite = %q, %v; want the new object", got, err)
	}
}

// countGets counts the GET requests that reach the server.
type countGets struct {
	n  atomic.Int64
	rt http.RoundTripper
}

func (c *countGets) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Method == http.MethodGet && r.URL.Query().Get("list-type") == "" {
		c.n.Add(1)
	}
	return c.rt.RoundTrip(r)
}

// AC-75 regression: a 64 MiB object read chunk by chunk (as serveChunkLoad
// does) a second time costs no S3 GET at all.  Needs the local MinIO.
func TestGetRange_LargeObjectSecondReadNoGET(t *testing.T) {
	plain := newTestClient(t)
	ctx := context.Background()
	c, err := New(Config{Endpoint: "localhost:9000", Bucket: bucket,
		AccessKey: "minioadmin", SecretKey: "minioadmin", CacheDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	cnt := &countGets{rt: http.DefaultTransport}
	c.mc, err = minio.New("localhost:9000", &minio.Options{
		Creds: credentials.NewStaticV4("minioadmin", "minioadmin", ""), Transport: cnt})
	if err != nil {
		t.Fatal(err)
	}
	const key, chunk, total = "range-cache-64m.bin", 4 * mib, 64 * mib
	data := randBytes(total)
	if err := plain.Put(ctx, key, data); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = plain.Delete(ctx, key) })
	if _, err := c.List(ctx, ""); err != nil {
		t.Fatal(err)
	}
	pass := func() {
		for off := int64(0); off < total; off += chunk {
			got, err := c.GetRange(ctx, key, off, chunk)
			if err != nil || !bytes.Equal(got, data[off:off+chunk]) {
				t.Fatalf("GetRange(%d): %v", off, err)
			}
		}
		c.cache.WaitAsync()
	}
	pass()
	if cnt.n.Load() == 0 {
		t.Fatal("the first pass made no GET: the counter does not work")
	}
	cnt.n.Store(0)
	pass()
	if n := cnt.n.Load(); n != 0 {
		t.Fatalf("second pass made %d S3 GETs, want 0", n)
	}
}
