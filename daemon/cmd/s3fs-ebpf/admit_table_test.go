package main

import (
	"reflect"
	"testing"
)

// fakeAdmitTable replaces the kmod ioctl and the evict-log drain, empties the
// package's admit table, and returns the snapshots pushed to "kmod".  Restores
// everything when the test ends.
func fakeAdmitTable(t *testing.T) *[][]admitEntry {
	t.Helper()
	origArena, origDrain := admitArena, drainEvictionsFn
	admitMu.Lock()
	origAdmits, origProgs := admits, admitProgs
	admits, admitProgs = nil, nil
	admitMu.Unlock()
	var pushed [][]admitEntry
	admitArena = func(_ string, es []admitEntry) error {
		pushed = append(pushed, append([]admitEntry(nil), es...))
		return nil
	}
	drainEvictionsFn = func(string) ([]uint64, uint32, error) { return nil, 0, nil }
	t.Cleanup(func() {
		admitArena, drainEvictionsFn = origArena, origDrain
		admitMu.Lock()
		admits, admitProgs = origAdmits, origProgs
		admitMu.Unlock()
	})
	return &pushed
}

func inosOf(es []admitEntry) []uint64 {
	out := make([]uint64, 0, len(es))
	for _, e := range es {
		out = append(out, e.Ino)
	}
	return out
}

// kmod breaks eviction score ties by index, so the prewarm list must reach it
// sorted by ino (X-04).
func TestSetAdmitsSortsByIno(t *testing.T) {
	pushed := fakeAdmitTable(t)
	if err := setAdmits("/dev/x", []admitEntry{{Ino: 30}, {Ino: 10}, {Ino: 20}}); err != nil {
		t.Fatal(err)
	}
	if len(*pushed) != 1 || !reflect.DeepEqual(inosOf((*pushed)[0]), []uint64{10, 20, 30}) {
		t.Errorf("pushed %v, want one push of [10 20 30]", *pushed)
	}
}

func TestClearAdmitsPushesAnEmptyTable(t *testing.T) {
	pushed := fakeAdmitTable(t)
	_ = setAdmits("/dev/x", []admitEntry{{Ino: 1}})
	if err := clearAdmits("/dev/x"); err != nil {
		t.Fatal(err)
	}
	if last := (*pushed)[len(*pushed)-1]; len(last) != 0 {
		t.Errorf("last push = %v, want an empty table", last)
	}
	if len(admits) != 0 {
		t.Errorf("daemon-side table still has %d entries", len(admits))
	}
}

func TestDropAdmitIno(t *testing.T) {
	pushed := fakeAdmitTable(t)
	_ = setAdmits("/dev/x", []admitEntry{{Ino: 1}, {Ino: 2, Offset: 0}, {Ino: 2, Offset: 4096}, {Ino: 3}})
	n := len(*pushed)
	dropAdmitIno("/dev/x", 2, nil)
	if got := inosOf((*pushed)[len(*pushed)-1]); !reflect.DeepEqual(got, []uint64{1, 3}) {
		t.Errorf("after dropping ino 2 the table is %v, want [1 3] (every range of the ino goes)", got)
	}
	// An ino that is not admitted changes nothing, so nothing is pushed.
	n = len(*pushed)
	dropAdmitIno("/dev/x", 99, nil)
	if len(*pushed) != n {
		t.Error("dropping an unknown ino pushed a snapshot")
	}
}

func TestDropAdmitsDoesNotPush(t *testing.T) {
	pushed := fakeAdmitTable(t)
	_ = setAdmits("/dev/x", []admitEntry{{Ino: 1}, {Ino: 2}})
	n := len(*pushed)
	if got := dropAdmits([]uint64{2, 77}, nil); got != 1 {
		t.Errorf("dropAdmits dropped %d, want 1", got)
	}
	if got := dropAdmits(nil, nil); got != 0 {
		t.Errorf("dropAdmits(nil) = %d, want 0", got)
	}
	// kmod already dropped them via the policy vote; pushing would re-add them.
	if len(*pushed) != n {
		t.Error("dropAdmits pushed to kmod")
	}
}

func TestPruneAdmitsTo(t *testing.T) {
	pushed := fakeAdmitTable(t)
	_ = setAdmits("/dev/x", []admitEntry{{Ino: 1}, {Ino: 2}, {Ino: 3}})
	n := len(*pushed)
	if err := pruneAdmitsTo("/dev/x", map[uint64]struct{}{1: {}, 3: {}}); err != nil {
		t.Fatal(err)
	}
	if got := inosOf((*pushed)[len(*pushed)-1]); !reflect.DeepEqual(got, []uint64{1, 3}) {
		t.Errorf("pruned table = %v, want [1 3]", got)
	}
	// Nothing left to prune: the ioctl is skipped.
	n = len(*pushed)
	if err := pruneAdmitsTo("/dev/x", map[uint64]struct{}{1: {}, 3: {}}); err != nil {
		t.Fatal(err)
	}
	if len(*pushed) != n {
		t.Error("a prune with no delta pushed a snapshot")
	}
}

func TestAddAdmitRangeKeepsOtherChunksAndIsIdempotent(t *testing.T) {
	pushed := fakeAdmitTable(t)
	chunk0 := admitEntry{Ino: 5, Offset: 0, Size: 4096}
	chunk1 := admitEntry{Ino: 5, Offset: 4096, Size: 4096}
	if err := addAdmitRange("/dev/x", chunk0, nil); err != nil {
		t.Fatal(err)
	}
	if err := addAdmitRange("/dev/x", chunk1, nil); err != nil {
		t.Fatal(err)
	}
	if last := (*pushed)[len(*pushed)-1]; len(last) != 2 {
		t.Fatalf("table = %v, want both chunks of ino 5", last)
	}
	n := len(*pushed)
	if err := addAdmitRange("/dev/x", chunk1, nil); err != nil {
		t.Fatal(err)
	}
	if len(*pushed) != n {
		t.Error("re-adding an admitted (ino, offset) pushed again")
	}
}

func TestReplaceAdmitRangeKeepsNonOverlappingChunks(t *testing.T) {
	pushed := fakeAdmitTable(t)
	_ = setAdmits("/dev/x", []admitEntry{
		{Ino: 5, Offset: 0, Size: 4096}, {Ino: 5, Offset: 4096, Size: 4096}, {Ino: 6, Offset: 0, Size: 4096},
	})
	fresh := admitEntry{Ino: 5, Offset: 0, Size: 4096, Uaddr: 123}
	if err := replaceAdmitRange("/dev/x", fresh, nil); err != nil {
		t.Fatal(err)
	}
	last := (*pushed)[len(*pushed)-1]
	if len(last) != 3 {
		t.Fatalf("table = %v, want 3 entries (chunk 1 of ino 5 and ino 6 kept)", last)
	}
	if last[len(last)-1] != fresh {
		t.Errorf("the replacement is not the last entry: %v", last)
	}
}

func TestRefreshAdmitsRepushesTheSnapshot(t *testing.T) {
	pushed := fakeAdmitTable(t)
	_ = setAdmits("/dev/x", []admitEntry{{Ino: 1}})
	n := len(*pushed)
	if _, err := refreshAdmits("/dev/x"); err != nil {
		t.Fatal(err)
	}
	if len(*pushed) != n+1 {
		t.Errorf("refreshAdmits pushed %d snapshots, want 1", len(*pushed)-n)
	}
}
