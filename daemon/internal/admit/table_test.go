package admit

import (
	"reflect"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/bbsakura/s3fs-ebpf/daemon/internal/kmodio"
)

type entry = kmodio.AdmitEntry

// fakeKmod records the snapshots pushed to the "kmod" and serves a scripted
// evict log.
type fakeKmod struct {
	mu     sync.Mutex
	pushed [][]entry
	evict  [][]uint64
}

func (f *fakeKmod) push(_ string, es []entry) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pushed = append(f.pushed, append([]entry(nil), es...))
	return nil
}

func (f *fakeKmod) drain(string) ([]uint64, uint32, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.evict) == 0 {
		return nil, 0, nil
	}
	out := f.evict[0]
	f.evict = f.evict[1:]
	return out, 0, nil
}

func (f *fakeKmod) last() []entry { return f.pushed[len(f.pushed)-1] }

func newFake() (*Table, *fakeKmod) {
	f := &fakeKmod{}
	return NewTable(f.push, f.drain), f
}

func inosOf(es []entry) []uint64 {
	out := make([]uint64, 0, len(es))
	for _, e := range es {
		out = append(out, e.Ino)
	}
	return out
}

// fakeArena records the chunks given back to the free-list.
type fakeArena struct {
	base  uint64
	freed []ChunkPair
}

func (a *fakeArena) BaseAddr() uint64 { return a.base }
func (a *fakeArena) FreeArenaChunk(off, size uint64) {
	a.freed = append(a.freed, ChunkPair{Offset: off, Size: size})
}

// H-04: two concurrent mutators used to snapshot under the lock, release it
// and race their ioctls, so the kmod kept whichever snapshot won and lost the
// other's mutation.  The push must run with mu held; with N mutators the final
// table has all N inos.  Run under -race to catch a release-early pattern.
func TestReplaceConcurrentPushIsSerialized(t *testing.T) {
	const N = 64
	var tbl *Table
	var callsUnderLock int64
	tbl = NewTable(func(string, []entry) error {
		if tbl.mu.TryLock() {
			tbl.mu.Unlock()
			t.Error("push invoked without the table lock held")
		} else {
			atomic.AddInt64(&callsUnderLock, 1)
		}
		return nil
	}, func(string) ([]uint64, uint32, error) { return nil, 0, nil })

	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(ino uint64) {
			defer wg.Done()
			_ = tbl.Replace("/dev/null", entry{Ino: ino, Uaddr: 0x1000 * ino, Size: 44}, nil)
		}(uint64(i + 1))
	}
	wg.Wait()

	if got := atomic.LoadInt64(&callsUnderLock); got != N {
		t.Errorf("pushes under the lock = %d, want %d", got, N)
	}
	if got := len(tbl.Snapshot()); got != N {
		t.Fatalf("final table has %d entries, want %d (concurrent Replace lost mutations)", got, N)
	}
}

// T-02: Drop returns the number of entries removed, not the number of freed
// arena chunks; they differ without an arena (offline mode).
func TestDropReturnsDroppedCount(t *testing.T) {
	tbl, _ := newFake()
	_ = tbl.Set("/dev/x", []entry{{Ino: 10, Uaddr: 0x1000, Size: 44}, {Ino: 11, Uaddr: 0x2000, Size: 44}, {Ino: 12, Uaddr: 0x3000, Size: 44}})
	if n := tbl.Drop([]uint64{10, 12}, nil); n != 2 {
		t.Errorf("Drop returned %d, want 2", n)
	}
	if got := len(tbl.Snapshot()); got != 1 {
		t.Errorf("%d entries left, want 1", got)
	}
}

// S-04: every push drains the kmod's evict log first, so an ino the kmod just
// evicted is not re-sent (resurrected) by the next push, while a fresh
// re-admit of that same ino still goes through.
func TestReplaceReconcilesEvictionsBeforePush(t *testing.T) {
	tbl, f := newFake()
	_ = tbl.Set("/dev/x", []entry{{Ino: 1, Uaddr: 0x1000, Size: 44}, {Ino: 2, Uaddr: 0x2000, Size: 44}, {Ino: 3, Uaddr: 0x3000, Size: 44}})
	f.pushed = nil
	f.evict = [][]uint64{{2}, {3}} // the kmod evicted 2, then (next round) 3

	if err := tbl.Replace("/dev/x", entry{Ino: 4, Uaddr: 0x4000, Size: 44}, nil); err != nil {
		t.Fatal(err)
	}
	if err := tbl.Replace("/dev/x", entry{Ino: 3, Uaddr: 0x5000, Size: 44}, nil); err != nil {
		t.Fatal(err)
	}
	byIno := func(es []entry) map[uint64]uint64 {
		m := map[uint64]uint64{}
		for _, e := range es {
			m[e.Ino] = e.Uaddr
		}
		return m
	}
	p0, p1 := byIno(f.pushed[0]), byIno(f.pushed[1])
	if _, ok := p0[2]; ok || len(p0) != 3 {
		t.Fatalf("push 0 = %v, want {1,3,4} without the evicted 2", p0)
	}
	if len(p1) != 3 || p1[3] != 0x5000 {
		t.Fatalf("push 1 = %v, want {1,4,3@0x5000}", p1)
	}
}

// kmod breaks eviction score ties by index, so the prewarm list must reach it
// sorted by ino (X-04).
func TestSetSortsByIno(t *testing.T) {
	tbl, f := newFake()
	if err := tbl.Set("/dev/x", []entry{{Ino: 30}, {Ino: 10}, {Ino: 20}}); err != nil {
		t.Fatal(err)
	}
	if len(f.pushed) != 1 || !reflect.DeepEqual(inosOf(f.last()), []uint64{10, 20, 30}) {
		t.Errorf("pushed %v, want one push of [10 20 30]", f.pushed)
	}
}

func TestClearPushesAnEmptyTable(t *testing.T) {
	tbl, f := newFake()
	_ = tbl.Set("/dev/x", []entry{{Ino: 1}})
	if err := tbl.Clear("/dev/x"); err != nil {
		t.Fatal(err)
	}
	if len(f.last()) != 0 || len(tbl.Snapshot()) != 0 {
		t.Errorf("after Clear: pushed %v, table %v", f.last(), tbl.Snapshot())
	}
}

func TestDropIno(t *testing.T) {
	tbl, f := newFake()
	_ = tbl.Set("/dev/x", []entry{{Ino: 1}, {Ino: 2, Offset: 0}, {Ino: 2, Offset: 4096}, {Ino: 3}})
	tbl.DropIno("/dev/x", 2, nil)
	if got := inosOf(f.last()); !reflect.DeepEqual(got, []uint64{1, 3}) {
		t.Errorf("after DropIno(2) the table is %v, want [1 3] (every range of the ino goes)", got)
	}
	n := len(f.pushed)
	tbl.DropIno("/dev/x", 99, nil)
	if len(f.pushed) != n {
		t.Error("dropping an unknown ino pushed a snapshot")
	}
}

func TestDropDoesNotPush(t *testing.T) {
	tbl, f := newFake()
	_ = tbl.Set("/dev/x", []entry{{Ino: 1}, {Ino: 2}})
	n := len(f.pushed)
	if got := tbl.Drop(nil, nil); got != 0 {
		t.Errorf("Drop(nil) = %d", got)
	}
	tbl.Drop([]uint64{2, 77}, nil)
	// The kmod already dropped them via the policy vote; pushing would re-add them.
	if len(f.pushed) != n {
		t.Error("Drop pushed to the kmod")
	}
}

func TestPrune(t *testing.T) {
	tbl, f := newFake()
	_ = tbl.Set("/dev/x", []entry{{Ino: 1}, {Ino: 2}, {Ino: 3}})
	keep := map[uint64]struct{}{1: {}, 3: {}}
	if err := tbl.Prune("/dev/x", keep); err != nil {
		t.Fatal(err)
	}
	if got := inosOf(f.last()); !reflect.DeepEqual(got, []uint64{1, 3}) {
		t.Errorf("pruned table = %v, want [1 3]", got)
	}
	n := len(f.pushed)
	if err := tbl.Prune("/dev/x", keep); err != nil {
		t.Fatal(err)
	}
	if len(f.pushed) != n {
		t.Error("a prune with no delta pushed a snapshot")
	}
}

func TestAddRangeKeepsOtherChunksAndIsIdempotent(t *testing.T) {
	tbl, f := newFake()
	c0, c1 := entry{Ino: 5, Offset: 0, Size: 4096}, entry{Ino: 5, Offset: 4096, Size: 4096}
	_ = tbl.AddRange("/dev/x", c0)
	_ = tbl.AddRange("/dev/x", c1)
	if len(f.last()) != 2 {
		t.Fatalf("table = %v, want both chunks of ino 5", f.last())
	}
	n := len(f.pushed)
	_ = tbl.AddRange("/dev/x", c1)
	if len(f.pushed) != n {
		t.Error("re-adding an admitted (ino, offset) pushed again")
	}
}

func TestReplaceRangeKeepsNonOverlappingChunks(t *testing.T) {
	tbl, f := newFake()
	_ = tbl.Set("/dev/x", []entry{{Ino: 5, Offset: 0, Size: 4096}, {Ino: 5, Offset: 4096, Size: 4096}, {Ino: 6, Offset: 0, Size: 4096}})
	fresh := entry{Ino: 5, Offset: 0, Size: 4096, Uaddr: 123}
	if err := tbl.ReplaceRange("/dev/x", fresh, nil); err != nil {
		t.Fatal(err)
	}
	last := f.last()
	if len(last) != 3 || last[len(last)-1] != fresh {
		t.Errorf("table = %v, want 3 entries ending with the replacement", last)
	}
}

func TestRefresh(t *testing.T) {
	tbl, f := newFake()
	_ = tbl.Set("/dev/x", []entry{{Ino: 1}, {Ino: 2}})
	n := len(f.pushed)
	got, err := tbl.Refresh("/dev/x")
	if err != nil || got != 2 || len(f.pushed) != n+1 {
		t.Errorf("Refresh = %d, %v with %d new pushes; want 2, nil, 1", got, err, len(f.pushed)-n)
	}
}

// Entries replaced or dropped go back to the arena free-list as arena
// offsets (Uaddr - base).
func TestArenaChunksReturnedToFreeList(t *testing.T) {
	tbl, _ := newFake()
	a := &fakeArena{base: 0x10000}
	tbl.SetArena(a)
	_ = tbl.Set("/dev/x", []entry{{Ino: 1, Uaddr: 0x10000 + 100, Size: 50}, {Ino: 2, Uaddr: 0x10000 + 200, Size: 60}, {Ino: 3, Uaddr: 5, Size: 7}})

	// Replace frees the old copy of ino 1.
	_ = tbl.Replace("/dev/x", entry{Ino: 1, Uaddr: 0x10000 + 400, Size: 50}, a)
	if want := []ChunkPair{{Offset: 100, Size: 50}}; !reflect.DeepEqual(a.freed, want) {
		t.Errorf("after Replace freed %v, want %v", a.freed, want)
	}
	// Drop frees ino 2; ino 3's Uaddr is below the arena base so nothing is freed for it.
	a.freed = nil
	if n := tbl.Drop([]uint64{2, 3}, a); n != 2 {
		t.Fatalf("Drop removed %d, want 2", n)
	}
	if want := []ChunkPair{{Offset: 200, Size: 60}}; !reflect.DeepEqual(a.freed, want) {
		t.Errorf("after Drop freed %v, want %v", a.freed, want)
	}
	// Prune frees through the arena set by SetArena.
	a.freed = nil
	if err := tbl.Prune("/dev/x", map[uint64]struct{}{}); err != nil {
		t.Fatal(err)
	}
	if want := []ChunkPair{{Offset: 400, Size: 50}}; !reflect.DeepEqual(a.freed, want) {
		t.Errorf("after Prune freed %v, want %v", a.freed, want)
	}
}
