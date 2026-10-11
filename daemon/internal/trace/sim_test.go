package trace

import "testing"

func reads(inos ...uint64) []Event {
	out := make([]Event, len(inos))
	for i, ino := range inos {
		out[i] = Event{Ts: uint64(i), Ino: ino, Op: OpRead}
	}
	return out
}

func TestOPTTextbook(t *testing.T) {
	// Classic MIN example: 7 0 1 2 0 3 0 4 2 3 0 3 2 with 3 frames
	// has 7 misses (6 hits).
	ev := reads(7, 0, 1, 2, 0, 3, 0, 4, 2, 3, 0, 3, 2)
	r := Replay(NewOPT(3, ev), ev)
	if r.Hits != 6 || r.Misses != 7 {
		t.Fatalf("opt: hits=%d misses=%d, want 6/7", r.Hits, r.Misses)
	}
}

func TestLRUTextbook(t *testing.T) {
	// Same reference string under LRU with 3 frames: 9 misses.
	ev := reads(7, 0, 1, 2, 0, 3, 0, 4, 2, 3, 0, 3, 2)
	r := Replay(NewLRU(3), ev)
	if r.Misses != 9 {
		t.Fatalf("lru: misses=%d, want 9", r.Misses)
	}
}

// OPT bounds every online policy at the same capacity; noevict bounds
// everything.
func TestBoundsOrdering(t *testing.T) {
	for _, alpha := range []float64{0.5, 0.7, 0.9, 1.2} {
		ev := GenerateZipf(30, 2000, alpha, 42)
		for _, cap := range []int{1, 2, 8, 16} {
			opt := Replay(NewOPT(cap, ev), ev).Hits
			lru := Replay(NewLRU(cap), ev).Hits
			sieve := Replay(NewSIEVE(cap), ev).Hits
			all := Replay(NewNoEvict(), ev).Hits
			if lru > opt || sieve > opt || opt > all {
				t.Fatalf("alpha=%v cap=%d: lru=%d sieve=%d opt=%d noevict=%d",
					alpha, cap, lru, sieve, opt, all)
			}
			sz := NewSIEVE(cap)
			Replay(sz, ev)
			if sz.Size() > cap {
				t.Fatalf("sieve cap=%d grew to %d", cap, sz.Size())
			}
		}
	}
}

// cap=1: every eviction removes the only node (head == tail), the
// case where the hand could be left on an unlinked node.
func TestSIEVECapOne(t *testing.T) {
	ev := reads(1, 2, 1, 3, 3, 2, 1)
	p := NewSIEVE(1)
	r := Replay(p, ev)
	if r.Hits != 1 || p.Size() != 1 {
		t.Fatalf("sieve cap=1: hits=%d size=%d, want 1/1", r.Hits, p.Size())
	}
}

// TestWriteInvalidatesEntry verifies that a write to an admitted ino
// removes it from the cache so the next read is a miss, for every policy.
func TestWriteInvalidatesEntry(t *testing.T) {
	ev := []Event{
		{Ino: 1, Op: OpRead},  // miss — ino 1 admitted
		{Ino: 1, Op: OpWrite}, // invalidate ino 1
		{Ino: 1, Op: OpRead},  // miss — ino 1 must be re-admitted
	}
	policies := []Policy{
		NewLRU(8),
		NewSIEVE(8),
		NewNoEvict(),
		NewOPT(8, ev),
	}
	for _, p := range policies {
		r := Replay(p, ev)
		if r.Hits != 0 || r.Misses != 2 {
			t.Fatalf("%s: hits=%d misses=%d, want 0/2", p.Name(), r.Hits, r.Misses)
		}
	}
}

func TestOPTIgnoresWrites(t *testing.T) {
	ev := []Event{
		{Ino: 1, Op: OpRead}, {Ino: 9, Op: OpWrite},
		{Ino: 2, Op: OpRead}, {Ino: 1, Op: OpRead},
	}
	// The write must not shift OPT's read cursor: with cap=2 the
	// second read of 1 is a hit.
	r := Replay(NewOPT(2, ev), ev)
	if r.Hits != 1 || r.Misses != 2 {
		t.Fatalf("opt: hits=%d misses=%d, want 1/2", r.Hits, r.Misses)
	}
}
