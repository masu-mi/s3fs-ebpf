package ebpf

import (
	"sort"
	"testing"
)

// withEntries swaps the live table for the test and restores it afterwards.
func withEntries(t *testing.T, es []Entry) {
	t.Helper()
	entriesMu.Lock()
	saved := entries
	entriesMu.Unlock()
	Replace(es)
	t.Cleanup(func() {
		entriesMu.Lock()
		entries = saved
		entriesMu.Unlock()
	})
}

func TestBootstrapReturnsACopy(t *testing.T) {
	a := Bootstrap()
	if len(a) == 0 {
		t.Fatal("empty bootstrap table")
	}
	a[0].Name = "mutated"
	if Bootstrap()[0].Name == "mutated" {
		t.Error("Bootstrap handed out its backing array")
	}
}

func TestReplaceAndSnapshotCopy(t *testing.T) {
	in := []Entry{{Ino: 7, Name: "a"}, {Ino: 8, Name: "b"}}
	withEntries(t, in)
	in[0].Name = "changed by caller"
	snap := Snapshot()
	if len(snap) != 2 || snap[0].Name != "a" {
		t.Fatalf("Replace kept the caller's slice: %+v", snap)
	}
	snap[1].Name = "changed by reader"
	if Snapshot()[1].Name != "b" {
		t.Error("Snapshot handed out the live slice")
	}
}

func TestLookupByIno(t *testing.T) {
	withEntries(t, []Entry{{Ino: 7, Name: "a"}, {Ino: 8, Name: "b"}})
	e := LookupByIno(8)
	if e == nil || e.Name != "b" {
		t.Fatalf("LookupByIno(8) = %+v", e)
	}
	e.Name = "changed"
	if LookupByIno(8).Name != "b" {
		t.Error("LookupByIno returned a pointer into the live table")
	}
	if LookupByIno(999) != nil {
		t.Error("LookupByIno(unknown) != nil")
	}
}

func TestSetSizeByIno(t *testing.T) {
	withEntries(t, []Entry{{Ino: 7, Fallback: 1}, {Ino: 8, Fallback: 2}})
	SetSizeByIno(8, 4096)
	SetSizeByIno(999, 1) // unknown ino: no effect, no panic
	if got := LookupByIno(8).Fallback; got != 4096 {
		t.Errorf("size of ino 8 = %d, want 4096", got)
	}
	if got := LookupByIno(7).Fallback; got != 1 {
		t.Errorf("size of ino 7 = %d, want 1 (untouched)", got)
	}
}

func TestWarmCacheBookkeeping(t *testing.T) {
	p := &Programs{cacheKnown: map[uint64]uint64{10: 0, 11: stubSize}}
	if !p.IsWarmed(10) || p.IsWarmed(12) {
		t.Error("IsWarmed does not follow cacheKnown")
	}
	got := p.KnownInos()
	sort.Slice(got, func(i, j int) bool { return got[i] < got[j] })
	if len(got) != 2 || got[0] != 10 || got[1] != 11 {
		t.Errorf("KnownInos = %v, want [10 11]", got)
	}
	// An ino that is already warm returns its offset without touching the
	// arena or the BPF map (both nil here).
	if off, err := p.WarmCache(InodeStub{Ino: 11}); err != nil || off != stubSize {
		t.Errorf("WarmCache(known) = %d, %v; want %d, nil", off, err, stubSize)
	}
}
