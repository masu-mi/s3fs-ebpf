package main

import (
	"errors"
	"io"
	"os"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"
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

	next, freed := planReplaceAdmit(admits, fresh, arenaBase)

	if got, want := len(next), 1; got != want {
		t.Fatalf("len(next) = %d, want %d (dedup should keep one entry per ino)", got, want)
	}
	if !reflect.DeepEqual(next[0], fresh) {
		t.Errorf("next[0] = %+v, want %+v (fresh entry must win)", next[0], fresh)
	}
	if len(freed) != 1 {
		t.Fatalf("len(freed) = %d, want 1 (old arena chunk must be reported for free-list)", len(freed))
	}
	wantChunk := arenaChunkPair{offset: 0, size: 100}
	if freed[0] != wantChunk {
		t.Errorf("freed[0] = %+v, want %+v", freed[0], wantChunk)
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

	next, freed := planReplaceAdmit(admits, fresh, arenaBase)

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
	admits := []admitEntry{
		{Ino: 7, Uaddr: arenaBase + 0, Size: 40},
		{Ino: 42, Uaddr: arenaBase + 100, Size: 60},
		{Ino: 99, Uaddr: arenaBase + 200, Size: 10},
	}
	fresh := admitEntry{Ino: 42, Uaddr: arenaBase + 500, Size: 30}

	next, freed := planReplaceAdmit(admits, fresh, arenaBase)

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
	if len(freed) != 1 || freed[0] != (arenaChunkPair{offset: 100, size: 60}) {
		t.Errorf("freed = %+v, want [{offset:100 size:60}]", freed)
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

	next, freed := planReplaceAdmit(admits, fresh, 0)

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
	admits := []admitEntry{
		{Ino: 42, Uaddr: arenaBase + 0, Size: 100, Offset: 0},
		{Ino: 42, Uaddr: arenaBase + 200, Size: 60, Offset: 4096},
	}
	fresh := admitEntry{Ino: 42, Uaddr: arenaBase + 500, Size: 30, Offset: 8192}

	next, freed := planReplaceAdmit(admits, fresh, arenaBase)

	if got, want := len(next), 1; got != want {
		t.Fatalf("len(next) = %d, want %d (all prior entries for ino=42 collapse)", got, want)
	}
	if next[0] != fresh {
		t.Errorf("next[0] = %+v, want %+v", next[0], fresh)
	}
	if got, want := len(freed), 2; got != want {
		t.Fatalf("len(freed) = %d, want %d (one chunk per prior entry)", got, want)
	}
	chunks := map[uint32]uint32{}
	for _, r := range freed {
		chunks[r.offset] = r.size
	}
	if sz, ok := chunks[0]; !ok || sz != 100 {
		t.Errorf("missing chunk offset=0 size=100; freed=%v", freed)
	}
	if sz, ok := chunks[200]; !ok || sz != 60 {
		t.Errorf("missing chunk offset=200 size=60; freed=%v", freed)
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

// TestOSFileCloseWakesBlockedRead pins the OS/runtime behavior
// runReadMissLoop's shutdown relies on (H-05): when a *os.File is
// Close()d while another goroutine is blocked in Read, Read must
// return promptly with io.EOF or os.ErrClosed rather than hang.
//
// This is generic Go / kernel behavior and should always hold, but
// runReadMissLoop is our only reader that depends on it, so a
// regression here would be silent in prod (the daemon would linger
// past SIGINT). Belt-and-suspenders: the production path also has a
// 5s shutdown-warn log (see readMissShutdownWarn) that would surface
// a hang, but we'd rather fail a test than wait on a bench.
func TestOSFileCloseWakesBlockedRead(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	defer w.Close()

	readErr := make(chan error, 1)
	go func() {
		var buf [8]byte
		_, err := r.Read(buf[:])
		readErr <- err
	}()

	// Give Read a chance to actually block before we close.
	time.Sleep(20 * time.Millisecond)
	if err := r.Close(); err != nil {
		t.Fatalf("r.Close: %v", err)
	}

	select {
	case err := <-readErr:
		if !errors.Is(err, io.EOF) && !errors.Is(err, os.ErrClosed) {
			t.Fatalf("Read after Close returned %v, want io.EOF or os.ErrClosed", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Read did not wake within 500ms of Close — runReadMissLoop shutdown would hang")
	}
}


// TestPlanPruneAdmits_FreesArenaChunksForDropped pins the T-01 fix:
// pruneAdmitsTo must return arena ranges for entries that are not in
// keepInos so the caller can give them back to the D-02 free-list.
// Prior code only counted dropped entries and leaked the arena bytes.
func TestPlanPruneAdmits_FreesArenaChunksForDropped(t *testing.T) {
	const arenaBase uint64 = 0x200000
	snapshot := []admitEntry{
		{Ino: 1, Uaddr: arenaBase + 0, Size: 100},
		{Ino: 2, Uaddr: arenaBase + 200, Size: 50},
		{Ino: 3, Uaddr: arenaBase + 400, Size: 80},
	}
	keep := map[uint64]struct{}{2: {}} // only ino 2 survives

	pruned, freed := planPruneAdmits(snapshot, keep, arenaBase)

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
	chunks := map[uint32]uint32{}
	for _, r := range freed {
		chunks[r.offset] = r.size
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
	snapshot := []admitEntry{
		{Ino: 1, Uaddr: 0x100000, Size: 64},
		{Ino: 2, Uaddr: 0x200000, Size: 32},
	}
	pruned, freed := planPruneAdmits(snapshot, map[uint64]struct{}{1: {}}, 0)

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
	snapshot := []admitEntry{
		{Ino: 7, Uaddr: arenaBase + 0, Size: 44},
		{Ino: 8, Uaddr: arenaBase + 100, Size: 44},
	}
	keep := map[uint64]struct{}{7: {}, 8: {}}

	pruned, freed := planPruneAdmits(snapshot, keep, arenaBase)

	if got, want := len(pruned), 2; got != want {
		t.Fatalf("len(pruned) = %d, want %d", got, want)
	}
	if len(freed) != 0 {
		t.Errorf("freed = %v, want empty (no entries dropped)", freed)
	}
}

// TestDropAdmitsLocked_ReturnsDroppedCount pins the T-02 fix: the
// return value must be the number of admit entries actually removed,
// not the number of freed arena chunks. The two differ when progs==nil
// (offline mode), where no arena chunks are freed but entries are still
// dropped — the prior code returned 0 in that case, causing the
// "[evict-drain] dropped 0 admits" log to mislead operators.
func TestDropAdmitsLocked_ReturnsDroppedCount(t *testing.T) {
	// Offline mode: progs==nil, so no arena chunks are freed.
	// The returned count must still reflect the number of entries removed.
	admitMu.Lock()
	admits = []admitEntry{
		{Ino: 10, Uaddr: 0x1000, Size: 44},
		{Ino: 11, Uaddr: 0x2000, Size: 44},
		{Ino: 12, Uaddr: 0x3000, Size: 44},
	}
	n := dropAdmitsLocked([]uint64{10, 12}, nil /* progs=nil, offline */)
	gotAdmits := len(admits)
	admitMu.Unlock()

	if n != 2 {
		t.Errorf("dropAdmitsLocked returned %d, want 2 (two inos removed)", n)
	}
	if gotAdmits != 1 {
		t.Errorf("len(admits) = %d after drop, want 1", gotAdmits)
	}
}

// S-04: every push drains kmod's evict log first, so an ino kmod just
// evicted is not re-sent (resurrected) by the next push, while a fresh
// re-admit of that same ino still goes through.
func TestReplaceAdmit_ReconcilesEvictionsBeforePush(t *testing.T) {
	admitMu.Lock()
	admits = []admitEntry{
		{Ino: 1, Uaddr: 0x1000, Size: 44},
		{Ino: 2, Uaddr: 0x2000, Size: 44},
		{Ino: 3, Uaddr: 0x3000, Size: 44},
	}
	admitMu.Unlock()

	var pushed [][]admitEntry
	origArena, origDrain := admitArena, drainEvictionsFn
	admitArena = func(_ string, entries []admitEntry) error {
		pushed = append(pushed, append([]admitEntry(nil), entries...))
		return nil
	}
	log := [][]uint64{{2}, {3}} // kmod evicted 2, then (next round) 3
	drainEvictionsFn = func(string) ([]uint64, uint32, error) {
		if len(log) == 0 {
			return nil, 0, nil
		}
		out := log[0]
		log = log[1:]
		return out, 0, nil
	}
	defer func() { admitArena, drainEvictionsFn = origArena, origDrain }()

	// Miss on ino 4: the push must not carry evicted ino 2.
	if err := replaceAdmit("/dev/null", admitEntry{Ino: 4, Uaddr: 0x4000, Size: 44}, nil); err != nil {
		t.Fatal(err)
	}
	// Re-read of evicted ino 3 (evicted between the two pushes): the
	// stale copy goes, the fresh admit stays.
	if err := replaceAdmit("/dev/null", admitEntry{Ino: 3, Uaddr: 0x5000, Size: 44}, nil); err != nil {
		t.Fatal(err)
	}

	inos := func(es []admitEntry) map[uint64]uint64 {
		m := map[uint64]uint64{}
		for _, e := range es {
			m[e.Ino] = e.Uaddr
		}
		return m
	}
	p0, p1 := inos(pushed[0]), inos(pushed[1])
	if _, ok := p0[2]; ok || len(p0) != 3 {
		t.Fatalf("push 0 = %v, want {1,3,4} without evicted 2", p0)
	}
	if len(p1) != 3 || p1[3] != 0x5000 {
		t.Fatalf("push 1 = %v, want {1,4,3@0x5000}", p1)
	}
}
