package main

import (
	"context"
	"errors"
	"testing"

	fpebpf "github.com/bbsakura/s3fs-ebpf/daemon/internal/ebpf"
	"github.com/bbsakura/s3fs-ebpf/daemon/internal/objstore"
	"golang.org/x/sys/unix"
)

// getBackend serves Get / GetRange from one in-memory object per key.
type getBackend struct {
	objstore.Backend
	objects map[string][]byte
	err     error
	gets    []string // "Get key" / "GetRange key off+size"
}

func (g *getBackend) Get(_ context.Context, key string) ([]byte, error) {
	g.gets = append(g.gets, "Get "+key)
	if g.err != nil {
		return nil, g.err
	}
	return g.objects[key], nil
}

func (g *getBackend) GetRange(_ context.Context, key string, off, size int64) ([]byte, error) {
	g.gets = append(g.gets, "GetRange "+key)
	if g.err != nil {
		return nil, g.err
	}
	return g.objects[key][off : off+size], nil
}

// upcall captures what the handler sent to the kmod.
type upcall struct {
	populated []struct {
		off int64
		n   int
	}
	signals       []int32 // errCode of every plain signalAdmit
	verifySignals int     // signalAdmitFlags calls
	verifyResult  error   // what signalAdmitFlags returns (nil = woken)
	populateErr   error
}

// fakeUpcall swaps the ioctls and the capacity lookup, and restores them.
func fakeUpcall(t *testing.T) *upcall {
	t.Helper()
	u := &upcall{}
	origPop, origSig, origFlags := populatePagesFn, signalAdmitFn, signalAdmitFlagsFn
	origCap := capacityBytesFn
	populatePagesFn = func(_ string, _ uint64, off int64, data []byte) error {
		u.populated = append(u.populated, struct {
			off int64
			n   int
		}{off, len(data)})
		return u.populateErr
	}
	signalAdmitFn = func(_ string, _ uint64, _ int64, code int32) error {
		u.signals = append(u.signals, code)
		return nil
	}
	signalAdmitFlagsFn = func(string, uint64, int64, int32, uint32) error {
		u.verifySignals++
		return u.verifyResult
	}
	capacityBytesFn = func() (int64, bool) { return 0, true }
	t.Cleanup(func() {
		populatePagesFn, signalAdmitFn, signalAdmitFlagsFn = origPop, origSig, origFlags
		capacityBytesFn = origCap
	})
	return u
}

func withEntry(t *testing.T, e fpebpf.Entry) {
	t.Helper()
	saved := fpebpf.Snapshot()
	t.Cleanup(func() { fpebpf.Replace(saved) })
	fpebpf.Replace([]fpebpf.Entry{e})
}

func TestReadMissUnknownInoSignalsENOENT(t *testing.T) {
	u := fakeUpcall(t)
	withEntry(t, fpebpf.Entry{Ino: 1})
	handleReadMiss(context.Background(), "/dev/x", &getBackend{}, readMissEvent{Ino: 424242})
	if len(u.signals) != 1 || u.signals[0] != -2 {
		t.Errorf("signals %v, want [-2]", u.signals)
	}
}

func TestReadMissNoBackendSignalsENODATA(t *testing.T) {
	u := fakeUpcall(t)
	withEntry(t, fpebpf.Entry{Ino: 801, Name: "f", S3Key: "f", Fallback: 10})
	handleReadMiss(context.Background(), "/dev/x", nil, readMissEvent{Ino: 801})
	if len(u.signals) != 1 || u.signals[0] != -61 {
		t.Errorf("signals %v, want [-61]", u.signals)
	}
}

// A small file is fetched whole and populated at offset 0; the waiter is woken
// by the verifying signal, with no plain error signal.
func TestReadMissPcacheSmallFile(t *testing.T) {
	u := fakeUpcall(t)
	withEntry(t, fpebpf.Entry{Ino: 802, Name: "small", S3Key: "small", Fallback: 5})
	b := &getBackend{objects: map[string][]byte{"small": []byte("hello")}}
	handleReadMiss(context.Background(), "/dev/x", b, readMissEvent{Ino: 802})

	if len(b.gets) != 1 || b.gets[0] != "Get small" {
		t.Errorf("fetched %v, want one whole-object Get", b.gets)
	}
	if len(u.populated) != 1 || u.populated[0].off != 0 || u.populated[0].n != 5 {
		t.Errorf("populated %+v, want one call of 5 bytes at 0", u.populated)
	}
	if u.verifySignals != 1 || len(u.signals) != 0 {
		t.Errorf("verifying signals=%d plain signals=%v, want 1 and none", u.verifySignals, u.signals)
	}
}

// A large file is fetched chunk by chunk: the 4 MiB-aligned chunk holding the
// miss offset, populated at that offset.
func TestReadMissPcacheLargeFileFetchesOneChunk(t *testing.T) {
	u := fakeUpcall(t)
	size := 3*partialAdmitChunk + 1000
	withEntry(t, fpebpf.Entry{Ino: 803, Name: "big", S3Key: "big", Fallback: uint64(size)})
	b := &getBackend{objects: map[string][]byte{"big": make([]byte, size)}}
	// A miss in the middle of chunk 2.
	handleReadMiss(context.Background(), "/dev/x", b,
		readMissEvent{Ino: 803, Offset: 2*partialAdmitChunk + 17})

	if len(b.gets) != 1 || b.gets[0] != "GetRange big" {
		t.Fatalf("fetched %v, want one range GET", b.gets)
	}
	if len(u.populated) != 1 || u.populated[0].off != 2*partialAdmitChunk || u.populated[0].n != partialAdmitChunk {
		t.Errorf("populated %+v, want one full chunk at %d", u.populated, 2*partialAdmitChunk)
	}
	// The last chunk is cut at the end of the object.
	u.populated = nil
	handleReadMiss(context.Background(), "/dev/x", b,
		readMissEvent{Ino: 803, Offset: 3*partialAdmitChunk + 5})
	if len(u.populated) != 1 || u.populated[0].n != 1000 {
		t.Errorf("tail chunk populated %+v, want 1000 bytes", u.populated)
	}
}

// A body bigger than one populate call is split into 4 MiB pieces (the kmod
// limits each ioctl).
func TestReadMissPcacheSplitsLargeBody(t *testing.T) {
	u := fakeUpcall(t)
	// Small by the entry's size, but the object is 2.5 chunks: Get returns it all.
	withEntry(t, fpebpf.Entry{Ino: 804, Name: "grown", S3Key: "grown", Fallback: 10})
	n := 2*partialAdmitChunk + partialAdmitChunk/2
	b := &getBackend{objects: map[string][]byte{"grown": make([]byte, n)}}
	handleReadMiss(context.Background(), "/dev/x", b, readMissEvent{Ino: 804})
	if len(u.populated) != 3 || u.populated[2].off != 2*partialAdmitChunk || u.populated[2].n != partialAdmitChunk/2 {
		t.Errorf("populated %+v, want 3 pieces ending with a half chunk at %d", u.populated, 2*partialAdmitChunk)
	}
}

func TestReadMissGetErrorSignalsEIO(t *testing.T) {
	u := fakeUpcall(t)
	withEntry(t, fpebpf.Entry{Ino: 805, Name: "e", S3Key: "e", Fallback: 5})
	handleReadMiss(context.Background(), "/dev/x", &getBackend{err: errors.New("s3 down")}, readMissEvent{Ino: 805})
	if len(u.signals) != 1 || u.signals[0] != -5 || len(u.populated) != 0 {
		t.Errorf("signals=%v populated=%v, want [-5] and nothing populated", u.signals, u.populated)
	}
}

func TestReadMissPopulateErrorSignalsEIO(t *testing.T) {
	u := fakeUpcall(t)
	u.populateErr = errors.New("ioctl failed")
	withEntry(t, fpebpf.Entry{Ino: 806, Name: "p", S3Key: "p", Fallback: 5})
	b := &getBackend{objects: map[string][]byte{"p": []byte("12345")}}
	handleReadMiss(context.Background(), "/dev/x", b, readMissEvent{Ino: 806})
	if len(u.signals) != 1 || u.signals[0] != -5 || u.verifySignals != 0 {
		t.Errorf("signals=%v verify=%d, want [-5] and no verify", u.signals, u.verifySignals)
	}
}

// AC-45: when the kmod says the chunk was evicted before the wake-up (EAGAIN),
// the handler populates again, at most signalVerifyRetries times, and then
// wakes the reader anyway rather than hanging it.
func TestReadMissRepopulatesThenGivesUp(t *testing.T) {
	u := fakeUpcall(t)
	u.verifyResult = unix.EAGAIN
	withEntry(t, fpebpf.Entry{Ino: 807, Name: "r", S3Key: "r", Fallback: 5})
	b := &getBackend{objects: map[string][]byte{"r": []byte("12345")}}
	handleReadMiss(context.Background(), "/dev/x", b, readMissEvent{Ino: 807})
	if want := signalVerifyRetries + 1; len(u.populated) != want || u.verifySignals != want {
		t.Errorf("populated %d times, verified %d times, want %d each", len(u.populated), u.verifySignals, want)
	}
	if len(u.signals) != 1 || u.signals[0] != 0 {
		t.Errorf("plain signals %v, want a final success wake-up [0]", u.signals)
	}
}

func TestLoadForWrite(t *testing.T) {
	u := fakeUpcall(t)
	withEntry(t, fpebpf.Entry{Ino: 808, Name: "w", S3Key: "w", Fallback: 100})
	b := &getBackend{objects: map[string][]byte{"w": make([]byte, 100)}}

	handleReadMiss(context.Background(), "/dev/x", b, readMissEvent{Ino: 808, Flags: missFlagForWrite})
	if len(b.gets) != 1 || b.gets[0] != "GetRange w" || len(u.populated) != 1 || u.populated[0].n != 100 {
		t.Errorf("gets=%v populated=%+v, want one range GET of the 100-byte object", b.gets, u.populated)
	}

	// kmod's i_size may be larger than the (older) entry size: the chunk is
	// cut at what kmod asked for, so it can become populated.
	u.populated, b.objects["w"] = nil, make([]byte, 300)
	handleReadMiss(context.Background(), "/dev/x", b, readMissEvent{Ino: 808, Count: 300, Flags: missFlagForWrite})
	if len(u.populated) != 1 || u.populated[0].n != 300 {
		t.Errorf("populated %+v, want 300 bytes (the length kmod asked for)", u.populated)
	}

	// A chunk entirely past the object is an I/O error, not an empty populate.
	u.populated, u.signals = nil, nil
	handleReadMiss(context.Background(), "/dev/x", b,
		readMissEvent{Ino: 808, Offset: 2 * partialAdmitChunk, Flags: missFlagForWrite})
	if len(u.signals) != 1 || u.signals[0] != -5 || len(u.populated) != 0 {
		t.Errorf("signals=%v populated=%v, want [-5] and no populate", u.signals, u.populated)
	}

	// Unknown ino / no backend.
	u.signals = nil
	handleReadMiss(context.Background(), "/dev/x", b, readMissEvent{Ino: 999999, Flags: missFlagForWrite})
	handleReadMiss(context.Background(), "/dev/x", nil, readMissEvent{Ino: 808, Flags: missFlagForWrite})
	if len(u.signals) != 2 || u.signals[0] != -2 || u.signals[1] != -61 {
		t.Errorf("signals %v, want [-2 -61]", u.signals)
	}
}

// statBackend answers Stat only: present keys succeed, absent ones wrap
// objstore.ErrNotFound, and failErr (if set) is returned for every key.
type statBackend struct {
	objstore.Backend
	present map[string]bool
	failErr error
	stats   []string
}

func (s *statBackend) Stat(_ context.Context, key string) (objstore.ObjectMeta, error) {
	s.stats = append(s.stats, key)
	if s.failErr != nil {
		return objstore.ObjectMeta{}, s.failErr
	}
	if !s.present[key] {
		return objstore.ObjectMeta{}, errors.Join(objstore.ErrNotFound, errors.New("404"))
	}
	return objstore.ObjectMeta{Key: key}, nil
}

func TestOpenCheck(t *testing.T) {
	u := fakeUpcall(t)
	withEntry(t, fpebpf.Entry{Ino: 909, Name: "o", S3Key: "o", Fallback: 10})
	b := &statBackend{present: map[string]bool{"o": true}}
	ev := readMissEvent{Ino: 909, Flags: missFlagOpenCheck}

	handleReadMiss(context.Background(), "/dev/x", b, ev)
	if len(b.stats) != 1 || len(u.signals) != 1 || u.signals[0] != 0 {
		t.Errorf("present: stats=%v signals=%v, want one HEAD and [0]", b.stats, u.signals)
	}

	u.signals = nil
	delete(b.present, "o") // deleted from S3 after the last sync
	handleReadMiss(context.Background(), "/dev/x", b, ev)
	if len(u.signals) != 1 || u.signals[0] != -2 {
		t.Errorf("deleted: signals=%v, want [-2]", u.signals)
	}

	u.signals = nil
	b.failErr = errors.New("connection refused")
	handleReadMiss(context.Background(), "/dev/x", b, ev)
	if len(u.signals) != 1 || u.signals[0] != -5 {
		t.Errorf("S3 failure: signals=%v, want [-5] (not ENOENT)", u.signals)
	}

	// Unknown ino / no backend; neither counts as a read-miss upcall.
	u.signals = nil
	handleReadMiss(context.Background(), "/dev/x", b, readMissEvent{Ino: 999999, Flags: missFlagOpenCheck})
	handleReadMiss(context.Background(), "/dev/x", nil, ev)
	if len(u.signals) != 2 || u.signals[0] != -2 || u.signals[1] != -61 {
		t.Errorf("signals %v, want [-2 -61]", u.signals)
	}
}

// A file this daemon created or renamed is not in the entry mirror until the
// next sync, so flush and rename record its key there (RecordKey, AC-86): the
// open check HEADs the key the daemon wrote, and a read miss would GET it.
func TestOpenCheckUsesRecordedKeys(t *testing.T) {
	u := fakeUpcall(t)
	b := &statBackend{present: map[string]bool{"dir/new.txt": true, "after": true}}

	fpebpf.RecordKey(910, 3, "new.txt", "dir/new.txt", 5, 2) // created and flushed, not in the mirror
	t.Cleanup(func() { fpebpf.Replace(nil) })
	handleReadMiss(context.Background(), "/dev/x", b, readMissEvent{Ino: 910, Flags: missFlagOpenCheck})
	if len(b.stats) != 1 || b.stats[0] != "dir/new.txt" || len(u.signals) != 1 || u.signals[0] != 0 {
		t.Errorf("created: stats=%v signals=%v, want HEAD dir/new.txt and [0]", b.stats, u.signals)
	}

	withEntry(t, fpebpf.Entry{Ino: 911, Name: "before", S3Key: "before", Fallback: 1})
	fpebpf.RecordKey(911, 0, "after", "after", 0, 3) // renamed since the last sync
	b.stats, u.signals = nil, nil
	handleReadMiss(context.Background(), "/dev/x", b, readMissEvent{Ino: 911, Flags: missFlagOpenCheck})
	if len(b.stats) != 1 || b.stats[0] != "after" || u.signals[0] != 0 {
		t.Errorf("renamed: stats=%v signals=%v, want HEAD of the new key and [0]", b.stats, u.signals)
	}
	if e := fpebpf.LookupByIno(911); e == nil || e.Fallback != 1 || e.Mode != 0 {
		t.Errorf("RecordKey on a known ino must keep its size and mode, got %+v", e)
	}
}
