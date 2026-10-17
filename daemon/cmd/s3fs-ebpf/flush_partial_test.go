package main

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/bbsakura/s3fs-ebpf/daemon/internal/objstore"
)

type fakeCopier struct {
	obj     []byte // the object in "S3"
	changed bool   // pinned operations fail with ErrSourceChanged
	got     []objstore.Part
	eager   int // upload parts that arrived with their bytes already in memory
	result  []byte
}

func (f *fakeCopier) GetRangeMatch(_ context.Context, _ string, off, n int64, _ string) ([]byte, error) {
	if f.changed {
		return nil, objstore.ErrSourceChanged
	}
	return f.obj[off : off+n], nil
}

func (f *fakeCopier) PutWithCopy(_ context.Context, _, _ string, parts []objstore.Part) (string, error) {
	if f.changed {
		return "", objstore.ErrSourceChanged
	}
	f.got = parts
	for _, p := range parts {
		if p.Data != nil {
			f.eager++ // AC-65: the bytes must come from Fill, at upload time
		}
	}
	var out []byte
	for _, p := range parts {
		if p.Kind == objstore.PartCopy {
			out = append(out, f.obj[p.Off:p.Off+p.Len]...)
		} else {
			d, err := p.Bytes()
			if err != nil {
				return "", err
			}
			out = append(out, d...)
		}
	}
	f.result = out
	return "new", nil
}

func TestFlushPartial(t *testing.T) {
	const chunk = 4 << 20
	const size = 10*chunk + 123
	old := make([]byte, size)
	for i := range old {
		old[i] = byte(i * 7)
	}
	// kmod's view: chunk 4 holds a modified copy; all else remote.
	want := append([]byte(nil), old...)
	copy(want[4*chunk+100:], bytes.Repeat([]byte{0xAA}, 50))
	kinds := make([]objstore.ChunkKind, 11)
	for i := range kinds {
		kinds[i] = objstore.ChunkRemote
	}
	kinds[4] = objstore.ChunkData
	io := func(f *fakeCopier) partialIO {
		return partialIO{
			readDirty: func(off int64, buf []byte) error { copy(buf, want[off:]); return nil },
			getMatch: func(ctx context.Context, off, n int64) ([]byte, error) {
				return f.GetRangeMatch(ctx, "k", off, n, "e")
			},
			getNow: func(_ context.Context, off, n int64) ([]byte, error) { return f.obj[off : off+n], nil },
		}
	}

	t.Run("copy", func(t *testing.T) {
		f := &fakeCopier{obj: old}
		sent, etag, err := flushPartial(context.Background(), f, nil, io(f), "k", "e", kinds, chunk, size)
		if err != nil || !bytes.Equal(f.result, want) {
			t.Fatalf("err=%v equal=%v", err, bytes.Equal(f.result, want))
		}
		if sent >= size/2 {
			t.Fatalf("sent %d of %d: remote chunks were not copied", sent, size)
		}
		if f.eager != 0 {
			t.Fatalf("%d upload parts were filled before the executor asked (AC-65: fill lazily)", f.eager)
		}
		if etag != "new" {
			t.Fatalf("new ETag %q, want the one PutWithCopy returned", etag)
		}
	})
	t.Run("source changed", func(t *testing.T) {
		f := &fakeCopier{obj: old, changed: true}
		var put []byte
		sent, _, err := flushPartial(context.Background(), f,
			func(_ context.Context, d []byte) error { put = d; return nil }, io(f), "k", "e", kinds, chunk, size)
		if err != nil || sent != size || !bytes.Equal(put, want) {
			t.Fatalf("err=%v sent=%d equal=%v", err, sent, bytes.Equal(put, want))
		}
	})
}

// Every unchanged run is shorter than the 5 MiB part minimum, so PlanParts has
// nothing to copy.  The flush must still go through, as a plain put of the
// whole content (it used to return ErrNoCopy and fail on every close).
func TestFlushPartial_NoCopyRunFallsBackToPut(t *testing.T) {
	const chunk = 4 << 20
	for name, kinds := range map[string][]objstore.ChunkKind{
		"R D R D":   {objstore.ChunkRemote, objstore.ChunkData, objstore.ChunkRemote, objstore.ChunkData},
		"D R D R D": {objstore.ChunkData, objstore.ChunkRemote, objstore.ChunkData, objstore.ChunkRemote, objstore.ChunkData},
	} {
		t.Run(name, func(t *testing.T) {
			size := int64(len(kinds)) * chunk
			old := make([]byte, size)
			want := make([]byte, size)
			for i := range old {
				old[i] = byte(i * 7)
				want[i] = old[i]
			}
			for ci, k := range kinds {
				if k != objstore.ChunkRemote {
					for i := int64(ci) * chunk; i < int64(ci+1)*chunk; i++ {
						want[i] = 0xAA
					}
				}
			}
			f := &fakeCopier{obj: old}
			var put []byte
			io := partialIO{
				readDirty: func(off int64, buf []byte) error { copy(buf, want[off:]); return nil },
				getMatch: func(ctx context.Context, off, n int64) ([]byte, error) {
					return f.GetRangeMatch(ctx, "k", off, n, "e")
				},
				getNow: func(_ context.Context, off, n int64) ([]byte, error) { return f.obj[off : off+n], nil },
			}
			sent, etag, err := flushPartial(context.Background(), f,
				func(_ context.Context, d []byte) error { put = d; return nil }, io, "k", "e", kinds, chunk, size)
			if err != nil || sent != size || etag != "" || !bytes.Equal(put, want) {
				t.Fatalf("err=%v sent=%d etag=%q equal=%v", err, sent, etag, bytes.Equal(put, want))
			}
		})
	}
}

// etagBackend is an object whose ETag the test can change; only Stat and the
// pinned read are implemented.
type etagBackend struct {
	objstore.Backend
	etag    string
	body    []byte            // for Stat's size and GetRange
	onMatch func(etag string) // runs before each pinned read
	reads   int
}

func (b *etagBackend) Stat(context.Context, string) (objstore.ObjectMeta, error) {
	return objstore.ObjectMeta{ETag: b.etag, Size: int64(len(b.body))}, nil
}

func (b *etagBackend) GetRangeMatch(_ context.Context, _ string, off, n int64, etag string) ([]byte, error) {
	b.reads++
	if b.onMatch != nil {
		b.onMatch(etag)
	}
	if etag != b.etag {
		return nil, objstore.ErrSourceChanged
	}
	return make([]byte, n), nil
}

func (b *etagBackend) GetRange(_ context.Context, _ string, off, n int64) ([]byte, error) {
	if off+n > int64(len(b.body)) {
		return nil, errors.New("range past the end")
	}
	return b.body[off : off+n], nil
}

// PutWithCopy plays the executor: the copy source must still be the pinned
// object, and the upload parts' bytes are produced here (Part.Fill).
func (b *etagBackend) PutWithCopy(_ context.Context, _, ifMatch string, parts []objstore.Part) (string, error) {
	if ifMatch != b.etag {
		return "", objstore.ErrSourceChanged
	}
	for _, p := range parts {
		if p.Kind == objstore.PartUpload {
			if _, err := p.Bytes(); err != nil {
				return "", err
			}
		}
	}
	return "", nil
}

func TestGetPinned(t *testing.T) {
	ctx := context.Background()
	t.Run("stale pin, nothing depends on it: re-stat and retry", func(t *testing.T) {
		const ino = 9001
		objETags.Store(uint64(ino), "old")
		b := &etagBackend{etag: "cur"}
		if _, err := getPinned(ctx, b, b, ino, "k", 0, 8, false); err != nil {
			t.Fatalf("err=%v", err)
		}
		if v, _ := objETags.Load(uint64(ino)); v != "cur" {
			t.Fatalf("pin = %v, want cur", v)
		}
		objETags.Delete(uint64(ino))
	})
	t.Run("stale pin, the fd depends on it: conflict", func(t *testing.T) {
		const ino = 9002
		objETags.Store(uint64(ino), "old")
		b := &etagBackend{etag: "cur"}
		if _, err := getPinned(ctx, b, b, ino, "k", 0, 8, true); !errors.Is(err, objstore.ErrSourceChanged) {
			t.Fatalf("err=%v, want ErrSourceChanged", err)
		}
		objETags.Delete(uint64(ino))
	})
	t.Run("a flush re-pinned meanwhile: retry with the new pin", func(t *testing.T) {
		const ino = 9003
		objETags.Store(uint64(ino), "E1")
		b := &etagBackend{etag: "E1"}
		b.onMatch = func(etag string) {
			if b.reads == 1 { // the flush completes just before this read
				b.etag = "E2"
				objETags.Store(uint64(ino), "E2")
			}
		}
		if _, err := getPinned(ctx, b, b, ino, "k", 0, 8, true); err != nil {
			t.Fatalf("err=%v", err)
		}
		objETags.Delete(uint64(ino))
	})
}

// Someone truncated the object after we pinned it: a remote chunk reaches
// past the new end.  The fallback put writes what is left and zeros for the
// rest, instead of failing on every retry.
func TestFlushPartial_ObjectTruncatedUnderTheCopy(t *testing.T) {
	const chunk = 4 << 20
	const size = 4 * chunk
	cur := make([]byte, chunk+1000) // the object now: 1 chunk + 1000 bytes
	for i := range cur {
		cur[i] = byte(i*3 + 1)
	}
	kinds := []objstore.ChunkKind{objstore.ChunkRemote, objstore.ChunkRemote, objstore.ChunkData, objstore.ChunkRemote}
	data := bytes.Repeat([]byte{0xBB}, chunk)
	b := &etagBackend{etag: "E3", body: cur}
	io := partialIO{
		readDirty: func(off int64, buf []byte) error { copy(buf, data); return nil },
		getMatch: func(ctx context.Context, off, n int64) ([]byte, error) {
			return b.GetRangeMatch(ctx, "k", off, n, "E1") // pinned to the old object
		},
		getNow: func(ctx context.Context, off, n int64) ([]byte, error) { return getNowClamped(ctx, b, "k", off, n) },
	}
	var put []byte
	sent, etag, err := flushPartial(context.Background(), b,
		func(_ context.Context, d []byte) error { put = d; return nil }, io, "k", "E1", kinds, chunk, size)
	if err != nil || sent != size || etag != "" || int64(len(put)) != size {
		t.Fatalf("err=%v sent=%d etag=%q len=%d", err, sent, etag, len(put))
	}
	want := make([]byte, size)
	copy(want, cur)
	copy(want[2*chunk:], data)
	if !bytes.Equal(put, want) {
		t.Fatal("content: surviving bytes, then zeros past the object's end, then kmod's chunk")
	}
}
