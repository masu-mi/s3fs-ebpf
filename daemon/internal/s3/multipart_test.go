package s3

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/bbsakura/s3fs-ebpf/daemon/internal/objstore"
	"github.com/minio/minio-go/v7"
)

const (
	mib    = 1 << 20
	bucket = "s3fs-multipart-test"
)

// newTestClient connects to the local MinIO (make compose-up) or skips.
func newTestClient(t *testing.T) *Client {
	t.Helper()
	if c, err := net.DialTimeout("tcp", "localhost:9000", 300*time.Millisecond); err != nil {
		t.Skip("MinIO not reachable on localhost:9000")
	} else {
		c.Close()
	}
	c, err := New(Config{Endpoint: "localhost:9000", Bucket: bucket,
		AccessKey: "minioadmin", SecretKey: "minioadmin"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if ok, _ := c.mc.BucketExists(ctx, bucket); !ok {
		if err := c.mc.MakeBucket(ctx, bucket, minio.MakeBucketOptions{}); err != nil {
			t.Fatalf("make bucket: %v", err)
		}
	}
	return c
}

func randBytes(n int) []byte {
	b := make([]byte, n)
	rand.Read(b)
	return b
}

// A 40 MiB object with 4 KiB changed in the middle: the plan copies the
// untouched ranges server-side and uploads only the padded chunk.
func TestPutWithCopy_MidWrite(t *testing.T) {
	c := newTestClient(t)
	ctx := context.Background()
	const key = "mid.bin"
	old := randBytes(40 * mib)
	if err := c.Put(ctx, key, old); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Delete(ctx, key) })
	meta, err := c.Stat(ctx, key)
	if err != nil {
		t.Fatal(err)
	}

	want := append([]byte(nil), old...)
	copy(want[20*mib:], randBytes(4096))

	kinds := make([]objstore.ChunkKind, 10)
	for i := range kinds {
		kinds[i] = objstore.ChunkRemote
	}
	kinds[5] = objstore.ChunkData // chunk 5 = [20 MiB, 24 MiB)
	specs, err := objstore.PlanParts(kinds, 4*mib, 40*mib)
	if err != nil {
		t.Fatal(err)
	}
	parts := make([]objstore.Part, len(specs))
	uploaded := 0
	for i, s := range specs {
		parts[i] = objstore.Part{PartSpec: s}
		if s.Kind == objstore.PartUpload {
			parts[i].Data = want[s.Off : s.Off+s.Len]
			uploaded += int(s.Len)
		}
	}
	if uploaded > 8*mib {
		t.Fatalf("uploads %d bytes, want at most 8 MiB (parts %v)", uploaded, specs)
	}
	etag, err := c.PutWithCopy(ctx, key, meta.ETag, parts)
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.Get(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("object content differs after PutWithCopy")
	}
	if now, _ := c.Stat(ctx, key); now.ETag != etag {
		t.Errorf("returned ETag %q, object has %q", etag, now.ETag)
	}
}

// The source changed after the ETag was taken: the copy fails with
// ErrSourceChanged and the object keeps the other writer's content.
func TestPutWithCopy_SourceChanged(t *testing.T) {
	c := newTestClient(t)
	ctx := context.Background()
	const key = "changed.bin"
	if err := c.Put(ctx, key, randBytes(24*mib)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Delete(ctx, key) })
	meta, _ := c.Stat(ctx, key)

	other := randBytes(24 * mib)
	if err := c.Put(ctx, key, other); err != nil {
		t.Fatal(err)
	}
	parts := []objstore.Part{
		{PartSpec: objstore.PartSpec{Kind: objstore.PartCopy, Off: 0, Len: 12 * mib}},
		{PartSpec: objstore.PartSpec{Kind: objstore.PartUpload, Off: 12 * mib, Len: 12 * mib}, Data: randBytes(12 * mib)},
	}
	if _, err := c.PutWithCopy(ctx, key, meta.ETag, parts); !errors.Is(err, objstore.ErrSourceChanged) {
		t.Fatalf("err = %v, want ErrSourceChanged", err)
	}
	if got, _ := c.Get(ctx, key); !bytes.Equal(got, other) {
		t.Fatal("a failed PutWithCopy changed the object")
	}
	if _, err := c.GetRangeMatch(ctx, key, 0, 4096, meta.ETag); !errors.Is(err, objstore.ErrSourceChanged) {
		t.Fatalf("GetRangeMatch with a stale ETag: err = %v, want ErrSourceChanged", err)
	}
}
