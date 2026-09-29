package main

import (
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
)

// TestPlanReplaceAdmit_ReplacesExistingIno pins the fix for the
// flush-refresh duplicate bug: when the ino is already in the admit
// snapshot, the fresh entry must REPLACE the old one (not append
// alongside it) and the old arena chunk must be reported so the
// caller can return it to the D-02 free-list. Without this, a
// PUT-heavy workload leaks arena and inflates kmod's admit hash.
func TestPlanReplaceAdmit_ReplacesExistingIno(t *testing.T) {
	const arenaBase uint64 = 0x100000
	admits := []admitEntry{
		{Ino: 42, Uaddr: arenaBase + 0, Size: 100},
	}
	fresh := admitEntry{Ino: 42, Uaddr: arenaBase + 200, Size: 50}

	next, oldChunk, hasOldChunk := planReplaceAdmit(admits, fresh, arenaBase)

	if got, want := len(next), 1; got != want {
		t.Fatalf("len(next) = %d, want %d (dedup should keep one entry per ino)", got, want)
	}
	if !reflect.DeepEqual(next[0], fresh) {
		t.Errorf("next[0] = %+v, want %+v (fresh entry must win)", next[0], fresh)
	}
	if !hasOldChunk {
		t.Fatal("hasOldChunk = false, want true (old arena chunk must be reported for free-list)")
	}
	wantChunk := arenaChunkPair{offset: 0, size: 100}
	if oldChunk != wantChunk {
		t.Errorf("oldChunk = %+v, want %+v", oldChunk, wantChunk)
	}
}

// TestPlanReplaceAdmit_AppendsWhenAbsent covers the first-admission
// path: no prior entry for the ino, so the entry just appends and
// no arena chunk is reported.
func TestPlanReplaceAdmit_AppendsWhenAbsent(t *testing.T) {
	const arenaBase uint64 = 0x100000
	admits := []admitEntry{
		{Ino: 7, Uaddr: arenaBase + 0, Size: 40},
	}
	fresh := admitEntry{Ino: 42, Uaddr: arenaBase + 100, Size: 50}

	next, _, hasOldChunk := planReplaceAdmit(admits, fresh, arenaBase)

	if got, want := len(next), 2; got != want {
		t.Fatalf("len(next) = %d, want %d", got, want)
	}
	if next[0].Ino != 7 || next[1] != fresh {
		t.Errorf("next = %+v, want prior entry preserved + fresh appended", next)
	}
	if hasOldChunk {
		t.Error("hasOldChunk = true, want false (no prior entry to free)")
	}
}

// TestPlanReplaceAdmit_LeavesOtherInosUntouched guards against a
// too-eager filter that drops unrelated entries.
func TestPlanReplaceAdmit_LeavesOtherInosUntouched(t *testing.T) {
	const arenaBase uint64 = 0x100000
	admits := []admitEntry{
		{Ino: 7, Uaddr: arenaBase + 0, Size: 40},
		{Ino: 42, Uaddr: arenaBase + 100, Size: 60},
		{Ino: 99, Uaddr: arenaBase + 200, Size: 10},
	}
	fresh := admitEntry{Ino: 42, Uaddr: arenaBase + 500, Size: 30}

	next, oldChunk, hasOldChunk := planReplaceAdmit(admits, fresh, arenaBase)

	if got, want := len(next), 3; got != want {
		t.Fatalf("len(next) = %d, want %d (7 + 99 kept, 42 replaced)", got, want)
	}
	seenInos := map[uint64]admitEntry{}
	for _, e := range next {
		seenInos[e.Ino] = e
	}
	if e, ok := seenInos[7]; !ok || e.Uaddr != arenaBase+0 {
		t.Errorf("ino=7 missing or altered: %+v (ok=%v)", e, ok)
	}
	if e, ok := seenInos[99]; !ok || e.Uaddr != arenaBase+200 {
		t.Errorf("ino=99 missing or altered: %+v (ok=%v)", e, ok)
	}
	if e, ok := seenInos[42]; !ok || e != fresh {
		t.Errorf("ino=42 = %+v, want %+v", e, fresh)
	}
	if !hasOldChunk || oldChunk != (arenaChunkPair{offset: 100, size: 60}) {
		t.Errorf("oldChunk = %+v hasOldChunk=%v, want offset=100 size=60",
			oldChunk, hasOldChunk)
	}
}

// TestPlanReplaceAdmit_ArenaBaseZeroSkipsFree covers offline mode
// (progs==nil upstream). arenaBase=0 means no arena wired; the
// filter still dedups, but no chunk is reported since there's
// nowhere to return it.
func TestPlanReplaceAdmit_ArenaBaseZeroSkipsFree(t *testing.T) {
	admits := []admitEntry{
		{Ino: 42, Uaddr: 0x100000, Size: 100},
	}
	fresh := admitEntry{Ino: 42, Uaddr: 0x100200, Size: 50}

	next, _, hasOldChunk := planReplaceAdmit(admits, fresh, 0)

	if got, want := len(next), 1; got != want {
		t.Fatalf("len(next) = %d, want %d", got, want)
	}
	if hasOldChunk {
		t.Error("hasOldChunk = true, want false when arenaBase is 0 (offline mode)")
	}
}

// TestPlanReplaceAdmit_DedupCollapsesPriorDuplicates codifies the
// recovery path: if a prior version of the code (pre-fix) left
// multiple entries for the same ino, one replaceAdmit call should
// collapse them all rather than leaving stragglers. Only the first
// duplicate's arena chunk is reported today — subsequent stragglers
// leak, which is an accepted limitation because the fix prevents
// them from being created in the first place. This test locks the
// current behavior so future refactors don't silently change it.
func TestPlanReplaceAdmit_DedupCollapsesPriorDuplicates(t *testing.T) {
	const arenaBase uint64 = 0x100000
	admits := []admitEntry{
		{Ino: 42, Uaddr: arenaBase + 0, Size: 100},
		{Ino: 42, Uaddr: arenaBase + 200, Size: 60},
	}
	fresh := admitEntry{Ino: 42, Uaddr: arenaBase + 500, Size: 30}

	next, _, hasOldChunk := planReplaceAdmit(admits, fresh, arenaBase)

	if got, want := len(next), 1; got != want {
		t.Fatalf("len(next) = %d, want %d (all prior entries for ino=42 collapse)", got, want)
	}
	if next[0] != fresh {
		t.Errorf("next[0] = %+v, want %+v", next[0], fresh)
	}
	if !hasOldChunk {
		t.Error("hasOldChunk = false, want true (at least the first duplicate is reported)")
	}
}

// TestReplaceAdmit_ConcurrentPushIsSerialized pins the H-04 fix: the
// prior implementation released admitMu before issuing admitArena, so
// two concurrent mutators could snapshot cleanly and then race their
// ioctls in an arbitrary order — kmod ended up reflecting whichever
// syscall won, silently dropping the loser's mutation.
//
// The fix holds admitMu across the push. We prove serialization by
// swapping admitArena for a capturing fake and asserting (a) every
// push observes a monotonic-strict prefix of ino admissions
// (no push snapshot is smaller than a prior push's snapshot) and
// (b) the final snapshot contains all N distinct inos.
//
// Run under `-race` to also catch any reintroduction of the release-
// early pattern.
func TestReplaceAdmit_ConcurrentPushIsSerialized(t *testing.T) {
	const N = 64

	// Reset global state.
	admitMu.Lock()
	admits = nil
	admitMu.Unlock()

	// Capture every push's ino set + size. admitArena runs with
	// admitMu held per the H-04 invariant, so appending here is
	// safe without extra locking — but assert it explicitly so a
	// regression breaks the test loudly.
	var (
		pushMu     sync.Mutex
		pushedSizes []int
		callsUnderLock int64
	)
	origArena := admitArena
	admitArena = func(devPath string, entries []admitEntry) error {
		if !admitMu.TryLock() {
			atomic.AddInt64(&callsUnderLock, 1)
		} else {
			// admitMu was NOT held by the caller — regression.
			admitMu.Unlock()
			t.Errorf("admitArena invoked without admitMu held")
		}
		pushMu.Lock()
		pushedSizes = append(pushedSizes, len(entries))
		pushMu.Unlock()
		return nil
	}
	defer func() { admitArena = origArena }()

	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(ino uint64) {
			defer wg.Done()
			_ = replaceAdmit("/dev/null", admitEntry{Ino: ino, Uaddr: 0x1000 * ino, Size: 44}, nil)
		}(uint64(i + 1))
	}
	wg.Wait()

	if got := atomic.LoadInt64(&callsUnderLock); got != N {
		t.Errorf("admitArena calls under admitMu = %d, want %d", got, N)
	}

	admitMu.Lock()
	gotLen := len(admits)
	admitMu.Unlock()
	if gotLen != N {
		t.Fatalf("final len(admits) = %d, want %d (concurrent replaceAdmit lost mutations)", gotLen, N)
	}

	// Sizes should be monotonically non-decreasing: each successful
	// push under the lock strictly follows the prior push's mutation,
	// so the ino count can only grow. If some other goroutine
	// snuck in a shorter push (the H-04 bug), this fails.
	for i := 1; i < len(pushedSizes); i++ {
		if pushedSizes[i] < pushedSizes[i-1] {
			t.Fatalf("push[%d] size %d < push[%d] size %d — snapshot regressed",
				i, pushedSizes[i], i-1, pushedSizes[i-1])
		}
	}
}
