package main

import (
	"reflect"
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
