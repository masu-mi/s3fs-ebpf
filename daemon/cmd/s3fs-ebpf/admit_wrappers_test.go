package main

import "testing"

// The admit-table functions in this package are thin wrappers over
// internal/admit.Table; this checks they reach it, and that the push still
// goes through the swappable admitArena / drainEvictionsFn.
func TestAdmitWrappersReachTheTable(t *testing.T) {
	origArena, origDrain := admitArena, drainEvictionsFn
	t.Cleanup(func() {
		admitArena, drainEvictionsFn = origArena, origDrain
		_ = clearAdmits("/dev/x")
	})
	var pushes [][]admitEntry
	admitArena = func(_ string, es []admitEntry) error {
		pushes = append(pushes, append([]admitEntry(nil), es...))
		return nil
	}
	drainEvictionsFn = func(string) ([]uint64, uint32, error) { return []uint64{9}, 0, nil }

	if err := setAdmits("/dev/x", []admitEntry{{Ino: 9}, {Ino: 1}}); err != nil {
		t.Fatal(err)
	}
	// Ino 9 is reported evicted by the fake drain, so the next push omits it.
	if err := replaceAdmit("/dev/x", admitEntry{Ino: 2}, nil); err != nil {
		t.Fatal(err)
	}
	last := pushes[len(pushes)-1]
	if len(last) != 2 || last[0].Ino != 1 || last[1].Ino != 2 {
		t.Errorf("last push = %v, want inos [1 2]", last)
	}
	if n, err := refreshAdmits("/dev/x"); err != nil || n != 2 {
		t.Errorf("refreshAdmits = %d, %v; want 2, nil", n, err)
	}
	if got := len(table.Snapshot()); got != 2 {
		t.Errorf("table has %d entries, want 2", got)
	}
	if arenaOf(nil) != nil {
		t.Error("arenaOf(nil) is not a nil interface")
	}
}
