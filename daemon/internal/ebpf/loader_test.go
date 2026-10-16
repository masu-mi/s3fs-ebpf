package ebpf

import (
	"reflect"
	"sort"
	"testing"
)

// TestCoalesceArenaFree_MergesAdjacentChunks pins the H-03 fix:
// consecutive evictions of neighboring arena ranges should collapse
// into a single free-list entry so a subsequent large admit can reuse
// the combined run instead of only fitting into individual scraps.
func TestCoalesceArenaFree_MergesAdjacentChunks(t *testing.T) {
	free := []arenaChunk{
		{offset: 0, size: 100},
		{offset: 100, size: 100}, // adjacent to previous
		{offset: 200, size: 50},  // adjacent to previous
		{offset: 500, size: 40},  // isolated
	}
	got, nextOff := coalesceArenaFree(free, 10_000)

	want := []arenaChunk{
		{offset: 0, size: 250},
		{offset: 500, size: 40},
	}
	if !equalChunks(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
	if nextOff != 10_000 {
		t.Errorf("nextOff = %d, want 10_000 (nothing abuts bump)", nextOff)
	}
}

// TestCoalesceArenaFree_ReturnsTailToBump validates the second part
// of H-03: when the trailing free run reaches the bump allocator,
// the tail must fold back into cacheNextOff (freeing the free-list
// entry) so long-running daemons don't accumulate a permanent
// pseudo-arena entry sitting right below the bump line.
func TestCoalesceArenaFree_ReturnsTailToBump(t *testing.T) {
	free := []arenaChunk{
		{offset: 900, size: 100}, // abuts bump allocator at 1000
		{offset: 100, size: 200},
	}
	got, nextOff := coalesceArenaFree(free, 1000)

	want := []arenaChunk{
		{offset: 100, size: 200},
	}
	if !equalChunks(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
	if nextOff != 900 {
		t.Errorf("nextOff = %d, want 900 (tail folded into bump)", nextOff)
	}
}

// TestCoalesceArenaFree_AllTailMerged is the extreme case: every
// free chunk abuts and unwinds the bump allocator entirely.
func TestCoalesceArenaFree_AllTailMerged(t *testing.T) {
	free := []arenaChunk{
		{offset: 0, size: 100},
		{offset: 100, size: 100},
		{offset: 200, size: 300},
	}
	got, nextOff := coalesceArenaFree(free, 500)

	if len(got) != 0 {
		t.Errorf("got %+v, want empty free-list (all folded)", got)
	}
	if nextOff != 0 {
		t.Errorf("nextOff = %d, want 0", nextOff)
	}
}

// TestCoalesceArenaFree_Empty is a defensive check — an empty
// free-list must survive the call without panic.
func TestCoalesceArenaFree_Empty(t *testing.T) {
	got, nextOff := coalesceArenaFree(nil, 500)
	if len(got) != 0 {
		t.Errorf("got %+v, want empty", got)
	}
	if nextOff != 500 {
		t.Errorf("nextOff = %d, want 500", nextOff)
	}
}

// TestCoalesceArenaFree_UnsortedInput exercises the sort step: even
// when chunks arrive in reverse order (D-02 append with no order
// guarantee), they merge correctly.
func TestCoalesceArenaFree_UnsortedInput(t *testing.T) {
	free := []arenaChunk{
		{offset: 400, size: 100},
		{offset: 0, size: 200},
		{offset: 200, size: 100},
		{offset: 300, size: 100},
	}
	got, nextOff := coalesceArenaFree(free, 10_000)

	want := []arenaChunk{
		{offset: 0, size: 500},
	}
	if !equalChunks(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
	if nextOff != 10_000 {
		t.Errorf("nextOff = %d, want 10_000", nextOff)
	}
}

// TestInsertArenaFreeBySize_MaintainsSizeOrder pins V-05: every
// insertArenaFreeBySize call must leave the free-list sorted ascending
// by size so AdmitContent's sort.Search (V-04) finds the best-fit
// chunk in O(log n) without a full scan.
func TestInsertArenaFreeBySize_MaintainsSizeOrder(t *testing.T) {
	var free []arenaChunk
	inserts := []arenaChunk{
		{offset: 500, size: 75},
		{offset: 100, size: 200},
		{offset: 0, size: 10},
		{offset: 300, size: 200}, // duplicate size — stable insert
		{offset: 900, size: 50},
	}
	for _, c := range inserts {
		free = insertArenaFreeBySize(free, c)
		for i := 1; i < len(free); i++ {
			if free[i].size < free[i-1].size {
				t.Errorf("not size-sorted after inserting %+v: %v", c, free)
				break
			}
		}
	}
}

// TestCoalesceArenaFree_OutputIsSizeSorted pins V-04: coalesceArenaFree
// must always return a size-sorted slice so AdmitContent's sort.Search
// invariant holds after every eviction-triggered coalesce.
func TestCoalesceArenaFree_OutputIsSizeSorted(t *testing.T) {
	cases := []struct {
		name    string
		free    []arenaChunk
		nextOff uint64
	}{
		{
			name:    "two distinct sizes after merge",
			free:    []arenaChunk{{offset: 0, size: 100}, {offset: 100, size: 100}, {offset: 200, size: 50}, {offset: 500, size: 40}},
			nextOff: 10_000,
		},
		{
			name:    "tail folded into bump",
			free:    []arenaChunk{{offset: 900, size: 100}, {offset: 100, size: 200}},
			nextOff: 1000,
		},
		{
			name:    "all chunks merge into one",
			free:    []arenaChunk{{offset: 400, size: 100}, {offset: 0, size: 200}, {offset: 200, size: 100}, {offset: 300, size: 100}},
			nextOff: 10_000,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := coalesceArenaFree(tc.free, tc.nextOff)
			for i := 1; i < len(got); i++ {
				if got[i].size < got[i-1].size {
					t.Errorf("not size-sorted at [%d]: %+v", i, got)
					break
				}
			}
		})
	}
}

func equalChunks(a, b []arenaChunk) bool {
	if len(a) != len(b) {
		return false
	}
	ac := append([]arenaChunk(nil), a...)
	bc := append([]arenaChunk(nil), b...)
	sort.Slice(ac, func(i, j int) bool { return ac[i].offset < ac[j].offset })
	sort.Slice(bc, func(i, j int) bool { return bc[i].offset < bc[j].offset })
	return reflect.DeepEqual(ac, bc)
}

// TestMergeReadPatterns pins the AC-47 per-CPU merge: only slots owned by the
// inode count, counters are summed without u32 wrap, LastEndPos is the
// furthest end any CPU read, and the merged SeqRatio is the ratio over all
// CPUs.
func TestMergeReadPatterns(t *testing.T) {
	const ino = 42
	got, ok := mergeReadPatterns(ino, []s3fsfpS3fsInoSlot{
		{Ino: ino, LastEndPos: 4096, SeqCount: 3, RandCount: 1, TotalBytes: 16384},
		{},                      // CPU that never read anything
		{Ino: 7, SeqCount: 100}, // CPU whose slot another inode took over
		{Ino: ino, LastEndPos: 1 << 20, SeqCount: 1<<32 - 1, RandCount: 0, TotalBytes: 1 << 20},
	})
	if !ok {
		t.Fatal("ok = false, want true")
	}
	want := ReadPattern{
		LastEndPos: 1 << 20,
		SeqCount:   1<<32 + 2,
		RandCount:  1,
		TotalBytes: 16384 + 1<<20,
	}
	if got != want {
		t.Fatalf("merge = %+v, want %+v", got, want)
	}

	// A single sequential reader that migrated once: one CPU saw only
	// the first read as sequential, the other counted its first read
	// (stale last_end_pos) as random. The merged ratio stays above the
	// 0.70 promote threshold.
	rp, _ := mergeReadPatterns(ino, []s3fsfpS3fsInoSlot{
		{Ino: ino, LastEndPos: 8 << 20, SeqCount: 16},
		{Ino: ino, LastEndPos: 10 << 20, SeqCount: 7, RandCount: 1},
	})
	if r := rp.SeqRatio(); r < 0.70 {
		t.Fatalf("SeqRatio after one migration = %.2f, want >= 0.70", r)
	}
	if rp.LastEndPos != 10<<20 {
		t.Fatalf("LastEndPos = %d, want %d", rp.LastEndPos, 10<<20)
	}

	// Slots owned by other inodes (or empty) mean "not seen".
	if _, ok := mergeReadPatterns(ino, []s3fsfpS3fsInoSlot{{Ino: 7, SeqCount: 5}, {}}); ok {
		t.Fatal("ok = true for slots owned by other inodes")
	}
	if r := (ReadPattern{}).SeqRatio(); r != 0 {
		t.Fatalf("empty SeqRatio = %v, want 0", r)
	}
}

// TestSlotIndex pins the hash shared with the BPF side
// (s3fs_slot_index in include/s3fs_shared_types.h): the result must stay in
// range and spread consecutive inos (they come from a counter).
func TestSlotIndex(t *testing.T) {
	seen := map[uint32]int{}
	for ino := uint64(1); ino <= 256; ino++ {
		idx := slotIndex(ino)
		if idx >= 1<<slotBits {
			t.Fatalf("slotIndex(%d) = %d out of range", ino, idx)
		}
		seen[idx]++
	}
	if len(seen) < 200 { // 256 consecutive inos into 1024 slots
		t.Fatalf("only %d distinct slots for 256 consecutive inos", len(seen))
	}
}
