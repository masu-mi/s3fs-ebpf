package main

import (
	"context"
	"errors"
	"testing"

	"github.com/bbsakura/s3fs-ebpf/daemon/internal/metrics"
	"github.com/bbsakura/s3fs-ebpf/daemon/internal/objstore"
)

// fakeFlushKmod records what handleFlush asked of the kernel module.
type fakeFlushKmod struct {
	dirty      []byte
	readErr    error
	partial    func() (bool, int64, int64, error)
	signalled  []int32
	markedDirt []uint64
	readCalls  int
}

func (f *fakeFlushKmod) SignalFlush(_ uint64, errCode int32) error {
	f.signalled = append(f.signalled, errCode)
	return nil
}
func (f *fakeFlushKmod) MarkDirty(ino uint64) error {
	f.markedDirt = append(f.markedDirt, ino)
	return nil
}
func (f *fakeFlushKmod) ReadDirty(_ uint64, buf []byte) (int, error) {
	f.readCalls++
	if f.readErr != nil {
		return 0, f.readErr
	}
	return copy(buf, f.dirty), nil
}
func (f *fakeFlushKmod) TryPartial(context.Context, objstore.Backend, string, uint64) (bool, int64, int64, error) {
	if f.partial != nil {
		return f.partial()
	}
	return false, 0, 0, nil
}

// putRecorder is a Backend that only records Put.
type putRecorder struct {
	objstore.Backend
	key  string
	data []byte
	err  error
}

func (p *putRecorder) Put(_ context.Context, key string, data []byte) error {
	p.key, p.data = key, append([]byte(nil), data...)
	return p.err
}

func flushEv(ino uint64, name string, size uint64) *flushEvent {
	ev := &flushEvent{Ino: ino, Size: size, NameLen: uint32(len(name))}
	copy(ev.Name[:], name)
	return ev
}

func TestHandleFlushPutsTheDirtyBytes(t *testing.T) {
	k := &fakeFlushKmod{dirty: []byte("hello")}
	b := &putRecorder{}
	before := metrics.Global.FlushBytes.Load()
	handleFlushWith(context.Background(), k, "/dev/x", nil, b, flushEv(9001, "flush-ok.txt", 5))

	if b.key != "flush-ok.txt" || string(b.data) != "hello" {
		t.Errorf("Put(%q, %q), want flush-ok.txt / hello", b.key, b.data)
	}
	if len(k.signalled) != 1 || k.signalled[0] != 0 {
		t.Errorf("signalled %v, want one success (0)", k.signalled)
	}
	if len(k.markedDirt) != 0 {
		t.Errorf("a successful flush marked the inode dirty: %v", k.markedDirt)
	}
	if got := metrics.Global.FlushBytes.Load() - before; got != 5 {
		t.Errorf("FlushBytes grew by %d, want 5", got)
	}
}

// READ_DIRTY runs for an empty file too: it is what tells the kmod this PUT
// covers the flush (W1/W3).
func TestHandleFlushEmptyFileStillReadsDirty(t *testing.T) {
	k := &fakeFlushKmod{}
	b := &putRecorder{}
	handleFlushWith(context.Background(), k, "/dev/x", nil, b, flushEv(9002, "empty.txt", 0))
	if k.readCalls != 1 || b.key != "empty.txt" || len(b.data) != 0 {
		t.Errorf("readCalls=%d put=%q/%d bytes, want 1 read and an empty put", k.readCalls, b.key, len(b.data))
	}
}

func TestHandleFlushNoBackendDrops(t *testing.T) {
	k := &fakeFlushKmod{dirty: []byte("x")}
	handleFlushWith(context.Background(), k, "/dev/x", nil, nil, flushEv(9003, "n.txt", 1))
	if len(k.signalled) != 1 || k.signalled[0] != -61 {
		t.Errorf("signalled %v, want -ENODATA (-61)", k.signalled)
	}
	if k.readCalls != 0 {
		t.Error("read the dirty bytes without a backend to put them to")
	}
}

func TestHandleFlushReadDirtyErrorRestoresDirtyFlag(t *testing.T) {
	k := &fakeFlushKmod{readErr: errors.New("copy failed")}
	b := &putRecorder{}
	errs := metrics.Global.FlushErrors.Load()
	handleFlushWith(context.Background(), k, "/dev/x", nil, b, flushEv(9004, "r.txt", 3))
	if len(k.markedDirt) != 1 || k.markedDirt[0] != 9004 {
		t.Errorf("marked dirty %v, want [9004] so the next close retries", k.markedDirt)
	}
	if len(k.signalled) != 1 || k.signalled[0] != -5 {
		t.Errorf("signalled %v, want -EIO (-5)", k.signalled)
	}
	if b.key != "" {
		t.Error("put after a failed read")
	}
	if metrics.Global.FlushErrors.Load() != errs+1 {
		t.Error("FlushErrors not incremented")
	}
}

func TestHandleFlushPutErrorRestoresDirtyFlag(t *testing.T) {
	k := &fakeFlushKmod{dirty: []byte("abc")}
	b := &putRecorder{err: errors.New("s3 down")}
	handleFlushWith(context.Background(), k, "/dev/x", nil, b, flushEv(9005, "p.txt", 3))
	if len(k.markedDirt) != 1 || k.markedDirt[0] != 9005 {
		t.Errorf("marked dirty %v, want [9005]", k.markedDirt)
	}
	if len(k.signalled) != 1 || k.signalled[0] != -5 {
		t.Errorf("signalled %v, want -EIO (-5)", k.signalled)
	}
}

func TestHandleFlushPartialPath(t *testing.T) {
	k := &fakeFlushKmod{partial: func() (bool, int64, int64, error) { return true, 4096, 1 << 20, nil }}
	b := &putRecorder{}
	bytes := metrics.Global.FlushBytes.Load()
	handleFlushWith(context.Background(), k, "/dev/x", nil, b, flushEv(9006, "big.bin", 1<<20))
	if b.key != "" || k.readCalls != 0 {
		t.Errorf("full put ran after a handled partial flush (put=%q reads=%d)", b.key, k.readCalls)
	}
	if got := metrics.Global.FlushBytes.Load() - bytes; got != 4096 {
		t.Errorf("FlushBytes grew by %d, want the 4096 bytes actually sent", got)
	}
	if len(k.signalled) != 1 || k.signalled[0] != 0 {
		t.Errorf("signalled %v, want one success", k.signalled)
	}

	// A failed partial flush restores the dirty flag and reports EIO.
	k = &fakeFlushKmod{partial: func() (bool, int64, int64, error) { return true, 0, 0, errors.New("copy part failed") }}
	handleFlushWith(context.Background(), k, "/dev/x", nil, b, flushEv(9007, "big2.bin", 1<<20))
	if len(k.markedDirt) != 1 || len(k.signalled) != 1 || k.signalled[0] != -5 {
		t.Errorf("partial failure: marked=%v signalled=%v, want one markDirty and -5", k.markedDirt, k.signalled)
	}
}
