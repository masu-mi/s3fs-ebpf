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
		{offset: 900, size: 100},  // abuts bump allocator at 1000
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
