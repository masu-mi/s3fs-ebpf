package main

import (
	"context"

	"errors"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	fpebpf "github.com/bbsakura/s3fs-ebpf/daemon/internal/ebpf"
	"github.com/bbsakura/s3fs-ebpf/daemon/internal/kmodio"
	"github.com/bbsakura/s3fs-ebpf/daemon/internal/metrics"
	"github.com/bbsakura/s3fs-ebpf/daemon/internal/objstore"
	"golang.org/x/sys/unix"
)

// seqRatio is the fraction of sequential reads the kmod counted (miss event
// fields seq_count / rand_count), 0 when nothing was counted.
func TestReadMissEventSeqRatio(t *testing.T) {
	for _, tt := range []struct {
		name       string
		seq, rnd   uint32
		wantRatio  float64
		wantPromo  bool // >= seqAdmitRatioThreshold
		anyCounted bool
	}{
		{"nothing counted", 0, 0, 0, false, false},
		{"all sequential", 10, 0, 1, true, true},
		{"all random", 0, 7, 0, false, true},
		{"threshold edge (70%)", 7, 3, 0.7, true, true},
		{"just below threshold", 69, 31, 0.69, false, true},
		{"u32 sums do not wrap", 1<<32 - 1, 1<<32 - 1, 0.5, false, true},
	} {
		ev := readMissEvent{SeqCount: tt.seq, RandCount: tt.rnd}
		got := ev.seqRatio()
		if d := got - tt.wantRatio; d < -1e-9 || d > 1e-9 {
			t.Errorf("%s: seqRatio = %v, want %v", tt.name, got, tt.wantRatio)
		}
		if promo := tt.anyCounted && got >= seqAdmitRatioThreshold; promo != tt.wantPromo {
			t.Errorf("%s: promotes = %v, want %v", tt.name, promo, tt.wantPromo)
		}
	}
}

// A file the daemon wrote (not in the mirror until the next sync) can be read
// back after its inode was evicted: the miss resolves through the entry
// mirror, where flush recorded the key and size (RecordKey, AC-86).
func TestReadMissOfALocallyWrittenFile(t *testing.T) {
	u := fakeUpcall(t)
	withEntry(t, fpebpf.Entry{Ino: 1})
	fpebpf.RecordKey(7101, 0, "written.bin", "written.bin", 7, 1)
	b := &getBackend{objects: map[string][]byte{"written.bin": []byte("payload")}}
	handleReadMiss(context.Background(), "/dev/x", b, readMissEvent{Ino: 7101})

	if len(u.signals) != 0 || u.verifySignals != 1 {
		t.Errorf("plain signals %v, verify signals %d: want the verifying wake only", u.signals, u.verifySignals)
	}
	if len(u.populated) != 1 || u.populated[0].off != 0 || u.populated[0].n != 7 {
		t.Errorf("populated %+v, want one call of 7 bytes at 0", u.populated)
	}
}

// rangeCounter totals the bytes GetRange was asked for.  (The chunk load fetches
// up to 4 contiguous chunks per request, so the byte count, not the number of
// requests, tells a whole-file fetch from a single chunk.)
type rangeCounter struct {
	objstore.Backend
	object []byte
	bytes  atomic.Int64
}

func (r *rangeCounter) GetRange(_ context.Context, _ string, off, size int64) ([]byte, error) {
	r.bytes.Add(size)
	return r.object[off : off+size], nil
}

// The access pattern in the miss event decides chunk vs whole file for a big
// file (R-01): sequential promotes to fetching every chunk, random and
// "nothing counted" fetch only the missed chunk, and a file bigger than
// capacity_bytes is never promoted.
func TestReadMissPatternAwareAdmit(t *testing.T) {
	size := 3*partialAdmitChunk + 1000 // 4 chunks
	run := func(t *testing.T, ev readMissEvent, capacity int64) *rangeCounter {
		t.Helper()
		fakeUpcall(t)
		capacityBytesFn = func() (int64, bool) { return capacity, true }
		orig := patternAwareAdmitEnabled
		patternAwareAdmitEnabled = true
		t.Cleanup(func() { patternAwareAdmitEnabled = orig })
		ev.Ino = 7201
		withEntry(t, fpebpf.Entry{Ino: 7201, Name: "big", S3Key: "big", Fallback: uint64(size)})
		b := &rangeCounter{object: make([]byte, size)}
		handleReadMiss(context.Background(), "/dev/x", b, ev)
		return b
	}
	// fetched waits until `want` bytes were requested (or 2 s), then a little
	// longer so that anything beyond `want` would show up too.
	fetched := func(b *rangeCounter, want int64) int64 {
		deadline := time.Now().Add(2 * time.Second)
		for b.bytes.Load() < want && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		time.Sleep(50 * time.Millisecond)
		return b.bytes.Load()
	}

	if n := fetched(run(t, readMissEvent{SeqCount: 20}, 0), int64(size)); n != int64(size) {
		t.Errorf("sequential pattern fetched %d bytes, want the whole file (%d)", n, size)
	}
	one := int64(partialAdmitChunk)
	if n := fetched(run(t, readMissEvent{SeqCount: 1, RandCount: 9}, 0), one); n != one {
		t.Errorf("random pattern fetched %d bytes, want one chunk (%d)", n, one)
	}
	if n := fetched(run(t, readMissEvent{}, 0), one); n != one {
		t.Errorf("no pattern counted fetched %d bytes, want one chunk (%d)", n, one)
	}
	if n := fetched(run(t, readMissEvent{SeqCount: 20}, one), one); n != one {
		t.Errorf("sequential but file > capacity fetched %d bytes, want one chunk (%d)", n, one)
	}
}

// evalPass issues S3FS_IOC_EVAL on the device; a file that is not the device
// answers ENOTTY, which must come back wrapped, and a missing device is an
// open error.
func TestEvalPassIoctl(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "dev")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	if _, err := evalPass(f.Name()); !errors.Is(err, unix.ENOTTY) {
		t.Errorf("evalPass on a regular file = %v, want ENOTTY", err)
	}
	if _, err := evalPass(f.Name() + ".missing"); err == nil {
		t.Error("evalPass on a missing device returned nil")
	}
}

// After a plain PUT the entry mirror carries the size just written, so a later
// miss (shrunk / appended file, inode evicted, no sync yet) does not fetch past
// EOF.
func TestHandleFlushUpdatesTheMirrorSize(t *testing.T) {
	withEntry(t, fpebpf.Entry{Ino: 7301, Name: "shrunk.bin", S3Key: "shrunk.bin", Fallback: 6 << 20})
	k := &fakeFlushKmod{dirty: make([]byte, 5<<20)}
	b := &putRecorder{}
	before := metrics.Global.FlushBytes.Load()
	handleFlushWith(context.Background(), k, "/dev/x", b, flushEv(7301, "shrunk.bin", 5<<20))

	if got := fpebpf.LookupByIno(7301); got == nil || got.Fallback != 5<<20 {
		t.Errorf("mirror size after the PUT = %+v, want %d", got, 5<<20)
	}
	if d := metrics.Global.FlushBytes.Load() - before; d != 5<<20 {
		t.Errorf("FlushBytes grew by %d, want %d", d, 5<<20)
	}
}

// A MISS request carries the miss as the fields the handler reads, including an
// offset past 4 GiB and a negative one (a signed field in the struct).
func TestReadMissFromReq(t *testing.T) {
	r := reqEvent{Type: kmodio.ReqMiss, Ino: 0x1122334455667788, Offset: 1<<32 + 4096, Count: 16384,
		Flags: missFlagForWrite | missFlagOpenCheck, SeqCount: 7, RandCount: 3}
	want := readMissEvent{Ino: 0x1122334455667788, Offset: 1<<32 + 4096, Count: 16384,
		Flags: missFlagForWrite | missFlagOpenCheck, SeqCount: 7, RandCount: 3}
	if got := readMissFromReq(&r); got != want {
		t.Errorf("readMissFromReq = %+v, want %+v", got, want)
	}
	r.Offset = -1
	if got := readMissFromReq(&r); got.Offset != -1 {
		t.Errorf("offset = %d, want -1", got.Offset)
	}
}

// offsetRecorder records the offsets GetRange was asked for.
type offsetRecorder struct {
	objstore.Backend
	object []byte
	mu     sync.Mutex
	offs   map[int64]int
	gate   map[int64]chan struct{} // a GetRange at this offset waits for the close
}

func (r *offsetRecorder) GetRange(_ context.Context, _ string, off, size int64) ([]byte, error) {
	r.mu.Lock()
	r.offs[off]++
	g := r.gate[off]
	r.mu.Unlock()
	if g != nil {
		<-g
	}
	return r.object[off : off+size], nil
}

func (r *offsetRecorder) has(off int64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.offs[off] > 0
}

// AC-89: with a capacity the file does not fit in, every miss of a sequential
// reader keeps the read-ahead window full -- the first one starts it, and a
// later one extends it by one chunk per chunk read (capacity 32 MiB: 3 ahead).
// The window used to start only after the missed chunk's own GET and not at all
// for a chunk that was being read ahead, so every fourth chunk was fetched with
// an empty pipeline.
func TestReadMissKeepsTheReadaheadWindowFull(t *testing.T) {
	const ino = 7301
	chunk := int64(partialAdmitChunk)
	size := 12 * chunk // more than the capacity, so no whole-file admit
	fakeUpcall(t)
	capacityBytesFn = func() (int64, bool) { return 32 << 20, true }
	orig := patternAwareAdmitEnabled
	patternAwareAdmitEnabled = true
	t.Cleanup(func() { patternAwareAdmitEnabled = orig })
	withEntry(t, fpebpf.Entry{Ino: ino, Name: "big", S3Key: "big", Fallback: uint64(size)})
	t.Cleanup(func() { readaheads.Delete(uint64(ino)) })
	gate := make(chan struct{})
	b := &offsetRecorder{object: make([]byte, size), offs: map[int64]int{}, gate: map[int64]chan struct{}{chunk: gate}}

	waitFor := func(off int64) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for !b.has(off) && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		if !b.has(off) {
			t.Fatalf("chunk at %d MiB was never requested", off>>20)
		}
	}
	handleReadMiss(context.Background(), "/dev/x", b, readMissEvent{Ino: ino, Offset: 0, SeqCount: 20})
	for _, c := range []int64{1, 2, 3} {
		waitFor(c * chunk)
	}
	time.Sleep(50 * time.Millisecond)
	if b.has(4 * chunk) {
		t.Fatal("a miss on chunk 0 read ahead past the window")
	}
	// The reader reaches chunk 1, whose read-ahead is still in flight (held
	// here): its miss only waits for it, and the window must move to chunk 4
	// meanwhile.
	missDone := make(chan struct{})
	go func() {
		handleReadMiss(context.Background(), "/dev/x", b, readMissEvent{Ino: ino, Offset: chunk, SeqCount: 20})
		close(missDone)
	}()
	waitFor(4 * chunk)
	close(gate)
	<-missDone
	// Let the read-ahead goroutines finish before the fakes are restored.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		busy := false
		raInflight.Range(func(any, any) bool { busy = true; return false })
		if !busy {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("read-ahead did not finish")
}
