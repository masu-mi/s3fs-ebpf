package admit

import (
	"reflect"
	"testing"

	"github.com/bbsakura/s3fs-ebpf/daemon/internal/kmodio"
)

// TestPlanReplaceAdmit_ReplacesExistingIno pins the fix for the
// flush-refresh duplicate bug: when the ino is already in the admit
// snapshot, the fresh entry must REPLACE the old one (not append
// alongside it) and the old arena chunk must be reported so the
// caller can return it to the D-02 free-list. Without this, a
// PUT-heavy workload leaks arena and inflates kmod's admit hash.
func TestPlanReplaceAdmit_ReplacesExistingIno(t *testing.T) {
	const arenaBase uint64 = 0x100000
	admits := []kmodio.AdmitEntry{
		{Ino: 42, Uaddr: arenaBase + 0, Size: 100},
	}
	fresh := kmodio.AdmitEntry{Ino: 42, Uaddr: arenaBase + 200, Size: 50}

	next, freed := PlanReplace(admits, fresh, arenaBase)

	if got, want := len(next), 1; got != want {
		t.Fatalf("len(next) = %d, want %d (dedup should keep one entry per ino)", got, want)
	}
	if !reflect.DeepEqual(next[0], fresh) {
		t.Errorf("next[0] = %+v, want %+v (fresh entry must win)", next[0], fresh)
	}
	if len(freed) != 1 {
		t.Fatalf("len(freed) = %d, want 1 (old arena chunk must be reported for free-list)", len(freed))
	}
	wantChunk := ChunkPair{Offset: 0, Size: 100}
	if freed[0] != wantChunk {
		t.Errorf("freed[0] = %+v, want %+v", freed[0], wantChunk)
	}
}

// TestPlanReplaceAdmit_AppendsWhenAbsent covers the first-admission
// path: no prior entry for the ino, so the entry just appends and
// no arena chunk is reported.
func TestPlanReplaceAdmit_AppendsWhenAbsent(t *testing.T) {
	const arenaBase uint64 = 0x100000
	admits := []kmodio.AdmitEntry{
		{Ino: 7, Uaddr: arenaBase + 0, Size: 40},
	}
	fresh := kmodio.AdmitEntry{Ino: 42, Uaddr: arenaBase + 100, Size: 50}

	next, freed := PlanReplace(admits, fresh, arenaBase)

	if got, want := len(next), 2; got != want {
		t.Fatalf("len(next) = %d, want %d", got, want)
	}
	if next[0].Ino != 7 || next[1] != fresh {
		t.Errorf("next = %+v, want prior entry preserved + fresh appended", next)
	}
	if len(freed) != 0 {
		t.Errorf("freed = %v, want empty (no prior entry to free)", freed)
	}
}

// TestPlanReplaceAdmit_LeavesOtherInosUntouched guards against a
// too-eager filter that drops unrelated entries.
func TestPlanReplaceAdmit_LeavesOtherInosUntouched(t *testing.T) {
	const arenaBase uint64 = 0x100000
	admits := []kmodio.AdmitEntry{
		{Ino: 7, Uaddr: arenaBase + 0, Size: 40},
		{Ino: 42, Uaddr: arenaBase + 100, Size: 60},
		{Ino: 99, Uaddr: arenaBase + 200, Size: 10},
	}
	fresh := kmodio.AdmitEntry{Ino: 42, Uaddr: arenaBase + 500, Size: 30}

	next, freed := PlanReplace(admits, fresh, arenaBase)

	if got, want := len(next), 3; got != want {
		t.Fatalf("len(next) = %d, want %d (7 + 99 kept, 42 replaced)", got, want)
	}
	seenInos := map[uint64]kmodio.AdmitEntry{}
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
	if len(freed) != 1 || freed[0] != (ChunkPair{Offset: 100, Size: 60}) {
		t.Errorf("freed = %+v, want [{Offset:100 Size:60}]", freed)
	}
}

// TestPlanReplaceAdmit_ArenaBaseZeroSkipsFree covers offline mode
// (progs==nil upstream). arenaBase=0 means no arena wired; the
// filter still dedups, but no chunk is reported since there's
// nowhere to return it.
func TestPlanReplaceAdmit_ArenaBaseZeroSkipsFree(t *testing.T) {
	admits := []kmodio.AdmitEntry{
		{Ino: 42, Uaddr: 0x100000, Size: 100},
	}
	fresh := kmodio.AdmitEntry{Ino: 42, Uaddr: 0x100200, Size: 50}

	next, freed := PlanReplace(admits, fresh, 0)

	if got, want := len(next), 1; got != want {
		t.Fatalf("len(next) = %d, want %d", got, want)
	}
	if len(freed) != 0 {
		t.Errorf("freed = %v, want empty when arenaBase is 0 (offline mode)", freed)
	}
}

// TestPlanReplaceAdmit_MultiRangeFreesAllChunks is the T-04 regression
// test: when the same ino has multiple entries (D-01 range-list), all
// prior chunks must be returned — not just the last one the loop saw.
// Prior code used a single `oldChunk` value and overwrote it on each
// iteration, leaking all but the last match.
func TestPlanReplaceAdmit_MultiRangeFreesAllChunks(t *testing.T) {
	const arenaBase uint64 = 0x100000
	admits := []kmodio.AdmitEntry{
		{Ino: 42, Uaddr: arenaBase + 0, Size: 100, Offset: 0},
		{Ino: 42, Uaddr: arenaBase + 200, Size: 60, Offset: 4096},
	}
	fresh := kmodio.AdmitEntry{Ino: 42, Uaddr: arenaBase + 500, Size: 30, Offset: 8192}

	next, freed := PlanReplace(admits, fresh, arenaBase)

	if got, want := len(next), 1; got != want {
		t.Fatalf("len(next) = %d, want %d (all prior entries for ino=42 collapse)", got, want)
	}
	if next[0] != fresh {
		t.Errorf("next[0] = %+v, want %+v", next[0], fresh)
	}
	if got, want := len(freed), 2; got != want {
		t.Fatalf("len(freed) = %d, want %d (one chunk per prior entry)", got, want)
	}
	chunks := map[uint64]uint64{}
	for _, r := range freed {
		chunks[r.Offset] = r.Size
	}
	if sz, ok := chunks[0]; !ok || sz != 100 {
		t.Errorf("missing chunk offset=0 size=100; freed=%v", freed)
	}
	if sz, ok := chunks[200]; !ok || sz != 60 {
		t.Errorf("missing chunk offset=200 size=60; freed=%v", freed)
	}
}

// TestPlanReplaceAdmitRange_KeepsOtherChunks is the AC-49 regression test: a
// chunk admit of an ino must leave that ino's other chunks admitted, and
// must replace (and free) only the entry it overlaps.
func TestPlanReplaceAdmitRange_KeepsOtherChunks(t *testing.T) {
	const arenaBase uint64 = 0x100000
	const chunk = 4 << 20
	admits := []kmodio.AdmitEntry{
		{Ino: 42, Uaddr: arenaBase + 0, Size: chunk, Offset: 0},
		{Ino: 42, Uaddr: arenaBase + chunk, Size: chunk, Offset: chunk},
		{Ino: 7, Uaddr: arenaBase + 3*chunk, Size: 100, Offset: 0}, // other ino
	}

	// Admit chunk 2 of ino 42: nothing overlaps, nothing is freed.
	fresh := kmodio.AdmitEntry{Ino: 42, Uaddr: arenaBase + 4*chunk, Size: 2 << 20, Offset: 2 * chunk}
	next, freed := PlanReplaceRange(append([]kmodio.AdmitEntry(nil), admits...), fresh, arenaBase)
	if len(next) != 4 {
		t.Fatalf("len(next) = %d, want 4 (all kept + the new range): %+v", len(next), next)
	}
	if len(freed) != 0 {
		t.Fatalf("freed = %v, want none", freed)
	}

	// Re-admit chunk 1 (e.g. after an eviction round trip): only the old
	// chunk 1 entry is replaced and its arena bytes freed.
	again := kmodio.AdmitEntry{Ino: 42, Uaddr: arenaBase + 9*chunk, Size: chunk, Offset: chunk}
	next, freed = PlanReplaceRange(append([]kmodio.AdmitEntry(nil), admits...), again, arenaBase)
	if len(next) != 3 {
		t.Fatalf("len(next) = %d, want 3: %+v", len(next), next)
	}
	if len(freed) != 1 || freed[0].Offset != chunk || freed[0].Size != chunk {
		t.Fatalf("freed = %v, want exactly the old chunk-1 bytes", freed)
	}
	if next[len(next)-1] != again {
		t.Fatalf("new entry not appended last: %+v", next)
	}

	// A whole-object admit overlaps every range of its ino, like
	// PlanReplace, and still leaves other inos alone.
	whole := kmodio.AdmitEntry{Ino: 42, Uaddr: arenaBase + 20*chunk, Size: 3 * chunk, Offset: 0}
	next, freed = PlanReplaceRange(append([]kmodio.AdmitEntry(nil), admits...), whole, arenaBase)
	if len(next) != 2 || len(freed) != 2 {
		t.Fatalf("whole-object admit: next=%+v freed=%v, want 2 entries and 2 frees", next, freed)
	}
}

func TestRangesOverlap(t *testing.T) {
	for _, c := range []struct {
		aOff, aSize, bOff, bSize uint64
		want                     bool
	}{
		{0, 10, 10, 10, false}, // adjacent
		{0, 10, 9, 10, true},
		{5, 10, 0, 100, true}, // contained
		{0, 0, 0, 10, false},  // empty
		{20, 5, 0, 10, false},
	} {
		if got := RangesOverlap(c.aOff, c.aSize, c.bOff, c.bSize); got != c.want {
			t.Errorf("RangesOverlap(%d,%d,%d,%d) = %v, want %v",
				c.aOff, c.aSize, c.bOff, c.bSize, got, c.want)
		}
	}
}

// TestPlanPruneAdmits_FreesArenaChunksForDropped pins the T-01 fix:
// pruneAdmitsTo must return arena ranges for entries that are not in
// keepInos so the caller can give them back to the D-02 free-list.
// Prior code only counted dropped entries and leaked the arena bytes.
func TestPlanPruneAdmits_FreesArenaChunksForDropped(t *testing.T) {
	const arenaBase uint64 = 0x200000
	snapshot := []kmodio.AdmitEntry{
		{Ino: 1, Uaddr: arenaBase + 0, Size: 100},
		{Ino: 2, Uaddr: arenaBase + 200, Size: 50},
		{Ino: 3, Uaddr: arenaBase + 400, Size: 80},
	}
	keep := map[uint64]struct{}{2: {}} // only ino 2 survives

	pruned, freed := PlanPrune(snapshot, keep, arenaBase)

	if got, want := len(pruned), 1; got != want {
		t.Fatalf("len(pruned) = %d, want %d", got, want)
	}
	if pruned[0].Ino != 2 {
		t.Errorf("pruned[0].Ino = %d, want 2", pruned[0].Ino)
	}
	if got, want := len(freed), 2; got != want {
		t.Fatalf("len(freed) = %d, want %d (one chunk per dropped ino)", got, want)
	}
	// Check both dropped inos' chunks are present (order not guaranteed).
	chunks := map[uint64]uint64{}
	for _, r := range freed {
		chunks[r.Offset] = r.Size
	}
	if sz, ok := chunks[0]; !ok || sz != 100 {
		t.Errorf("missing chunk offset=0 size=100; freed=%v", freed)
	}
	if sz, ok := chunks[400]; !ok || sz != 80 {
		t.Errorf("missing chunk offset=400 size=80; freed=%v", freed)
	}
}

// TestPlanPruneAdmits_ArenaBaseZeroSkipsFree covers offline mode:
// when arenaBase=0, no chunks are reported even though entries are dropped.
func TestPlanPruneAdmits_ArenaBaseZeroSkipsFree(t *testing.T) {
	snapshot := []kmodio.AdmitEntry{
		{Ino: 1, Uaddr: 0x100000, Size: 64},
		{Ino: 2, Uaddr: 0x200000, Size: 32},
	}
	pruned, freed := PlanPrune(snapshot, map[uint64]struct{}{1: {}}, 0)

	if got, want := len(pruned), 1; got != want {
		t.Fatalf("len(pruned) = %d, want %d", got, want)
	}
	if len(freed) != 0 {
		t.Errorf("freed = %v, want empty (arenaBase=0 must suppress chunk reports)", freed)
	}
}

// TestPlanPruneAdmits_KeepAllProducesNoDelta pins the no-change path:
// when all inos are in keepInos no entries are dropped and no chunks
// are reported. pruneAdmitsTo relies on len(freed)==0 to skip the ioctl.
func TestPlanPruneAdmits_KeepAllProducesNoDelta(t *testing.T) {
	const arenaBase uint64 = 0x100000
	snapshot := []kmodio.AdmitEntry{
		{Ino: 7, Uaddr: arenaBase + 0, Size: 44},
		{Ino: 8, Uaddr: arenaBase + 100, Size: 44},
	}
	keep := map[uint64]struct{}{7: {}, 8: {}}

	pruned, freed := PlanPrune(snapshot, keep, arenaBase)

	if got, want := len(pruned), 2; got != want {
		t.Fatalf("len(pruned) = %d, want %d", got, want)
	}
	if len(freed) != 0 {
		t.Errorf("freed = %v, want empty (no entries dropped)", freed)
	}
}
