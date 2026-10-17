package main

import (
	"bytes"
	"context"
	"testing"

	"github.com/bbsakura/s3fs-ebpf/daemon/internal/objstore"
)

type fakeCopier struct {
	obj     []byte // the object in "S3"
	changed bool   // pinned operations fail with ErrSourceChanged
	got     []objstore.Part
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
	var out []byte
	for _, p := range parts {
		if p.Kind == objstore.PartCopy {
			out = append(out, f.obj[p.Off:p.Off+p.Len]...)
		} else {
			out = append(out, p.Data...)
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
		sent, err := flushPartial(context.Background(), f, nil, io(f), "k", "e", kinds, chunk, size)
		if err != nil || !bytes.Equal(f.result, want) {
			t.Fatalf("err=%v equal=%v", err, bytes.Equal(f.result, want))
		}
		if sent >= size/2 {
			t.Fatalf("sent %d of %d: remote chunks were not copied", sent, size)
		}
	})
	t.Run("source changed", func(t *testing.T) {
		f := &fakeCopier{obj: old, changed: true}
		var put []byte
		sent, err := flushPartial(context.Background(), f,
			func(_ context.Context, d []byte) error { put = d; return nil }, io(f), "k", "e", kinds, chunk, size)
		if err != nil || sent != size || !bytes.Equal(put, want) {
			t.Fatalf("err=%v sent=%d equal=%v", err, sent, bytes.Equal(put, want))
		}
	})
}
