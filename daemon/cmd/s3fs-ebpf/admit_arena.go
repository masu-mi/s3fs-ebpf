package main

import (
	"fmt"
	"os"
	"sort"
	"sync"

	"github.com/bbsakura/s3fs-ebpf/daemon/internal/admit"
	fpebpf "github.com/bbsakura/s3fs-ebpf/daemon/internal/ebpf"
	"github.com/bbsakura/s3fs-ebpf/daemon/internal/metrics"
)

// updateArenaHighWater advances the ArenaUsedBytes gauge when `off+size`
// exceeds the current high-water mark. Called after every AdmitContent so
// /metrics reflects the furthest byte written into the arena.
// The planners are pure and live in internal/admit; these keep the local names.
type arenaChunkPair = admit.ChunkPair

var (
	planReplaceAdmit      = admit.PlanReplace
	planReplaceAdmitRange = admit.PlanReplaceRange
	planPruneAdmits       = admit.PlanPrune
	rangesOverlap         = admit.RangesOverlap
)

func updateArenaHighWater(off uint64, size int) {
	if hi := off + uint64(size); hi > metrics.Global.ArenaUsedBytes.Load() {
		metrics.Global.ArenaUsedBytes.Store(hi)
	}
}

// admitTable is the daemon-side source of truth for what kmod
// currently has admitted. The kmod ioctl semantics are "replace the
// whole table"; every mutation therefore has to send the full
// snapshot. Guarded by admitMu.
//
// H-04 invariant: admitMu is held from the moment the slice is
// mutated through the ioctl call. Two concurrent mutators used to
// race by taking snapshots under the lock, releasing, then issuing
// ioctls in an arbitrary order — kmod would end up with whichever
// snapshot won the syscall race, silently losing the loser's
// mutation. Holding the lock across the ioctl serializes the
// push order and guarantees kmod's admit table always reflects the
// latest committed daemon-side state.
var (
	admitMu sync.Mutex
	admits  []admitEntry
)

// drainEvictionsFn is drainEvictions behind a var so tests can feed a
// fake kmod evict log.
var drainEvictionsFn = drainEvictions

// admitProgs is the loader that owns the arena, so reconcile can return
// evicted ranges to the free-list from any push path. Set once by
// setAdmitProgs after fpebpf.Load; nil in offline mode / tests.
// Guarded by admitMu.
var admitProgs *fpebpf.Programs

func setAdmitProgs(p *fpebpf.Programs) {
	admitMu.Lock()
	admitProgs = p
	admitMu.Unlock()
}

// sharedDevFile holds the single /dev/s3fs_ebpf fd opened by openSharedDev.
// All admit/signal ioctls reuse this fd instead of opening per-call (V-01).
// Nil until openSharedDev is called; fallback to per-call open when nil
// so tests without a real device still compile and run.
var sharedDevFile *os.File

// openSharedDev opens devPath once and stores it for all ioctl helpers.
// The caller (runFastpath) is responsible for closing it on exit.
func openSharedDev(devPath string) (*os.File, error) {
	f, err := os.OpenFile(devPath, os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", devPath, err)
	}
	sharedDevFile = f
	return f, nil
}

// devFileOrOpen returns sharedDevFile when available, otherwise opens
// devPath for a single call (offline mode / test fallback). The returned
// close func must be called; it is a no-op when the shared fd was used.
func devFileOrOpen(devPath string) (f *os.File, close func(), err error) {
	if sharedDevFile != nil {
		return sharedDevFile, func() {}, nil
	}
	f, err = os.OpenFile(devPath, os.O_RDWR, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("open %s: %w", devPath, err)
	}
	return f, func() { f.Close() }, nil
}

// reconcileEvictionsLocked pulls kmod's evict log and drops those inos
// from the daemon-side snapshot before the caller pushes it (S-04).
// kmod only evicts inside an admit_replace that the daemon itself
// issued under admitMu, so draining right before every push closes
// the window in which drainEvictionsLoop (2 s tick) had not run yet
// and the next push re-sent — and so resurrected — evicted inos.
// Errors are ignored: the periodic loop is still the fallback.
// Caller holds admitMu.
func reconcileEvictionsLocked(devPath string) {
	evicted, _, err := drainEvictionsFn(devPath)
	if err != nil || len(evicted) == 0 {
		return
	}
	dropAdmitsLocked(evicted, admitProgs)
}

// setAdmits stores the initial (prewarm) admit list and pushes it to
// kmod. Any prior state is dropped.
//
// X-04: sort by Ino ascending before storing so kmod receives entries
// in assignment order (lower ino ≈ earlier assigned ≈ oldest). kmod's
// s3fs_victim_cmp breaks score ties by idx (smallest = oldest); without
// this sort the prewarm list arrives in Go map iteration order (random)
// making eviction non-deterministic across runs.
func setAdmits(devPath string, initial []admitEntry) error {
	admitMu.Lock()
	defer admitMu.Unlock()
	sort.Slice(initial, func(i, j int) bool { return initial[i].Ino < initial[j].Ino })
	admits = append(admits[:0], initial...)
	metrics.Global.AdmitsCurrent.Store(uint64(len(admits)))
	return admitArena(devPath, admits)
}

// replaceAdmit inserts `entry` and pushes the whole snapshot to
// kmod, dropping any prior admit for the same ino and returning that
// prior admit's arena bytes to the D-02 free-list. Serves both the
// first-admission path (no prior entry) and the refresh path (flush
// / re-admit after eviction) with the same code path.
//
// Without dedup, a flush after prewarm would push [old, new] for the
// same ino: kmod's hash accepts both, the older one becomes dead
// weight in the bucket, and its arena bytes leak because no
// eviction feedback removes them. `progs` may be nil in offline mode
// (no arena wired) — the free-list update is then skipped.
func replaceAdmit(devPath string, entry admitEntry, progs *fpebpf.Programs) error {
	admitMu.Lock()
	defer admitMu.Unlock()
	// Before planReplaceAdmit: if entry.Ino itself was evicted, its old
	// range is freed here and the fresh entry is then simply appended.
	reconcileEvictionsLocked(devPath)
	var arenaBase uint64
	if progs != nil {
		arenaBase = progs.Arena().BaseAddr()
	}
	next, freed := planReplaceAdmit(admits, entry, arenaBase)
	admits = next
	metrics.Global.AdmitsCurrent.Store(uint64(len(admits)))
	if progs != nil {
		for _, r := range freed {
			progs.FreeArenaChunk(r.Offset, r.Size)
		}
	}
	return admitArena(devPath, admits)
}

// replaceAdmitRange is replaceAdmit for a range of an ino (AC-49): it drops
// only the ino's entries that OVERLAP entry and frees their arena bytes, and
// keeps its other ranges.  replaceAdmit drops every entry of the ino, which
// is right when the new entry is the whole object (prewarm, flush) but made
// a chunk admit (D-01) evict the ino's other chunks: reading chunk 0 threw
// away chunk 1, and the next read fetched chunk 1 from S3 again.
func replaceAdmitRange(devPath string, entry admitEntry, progs *fpebpf.Programs) error {
	admitMu.Lock()
	defer admitMu.Unlock()
	reconcileEvictionsLocked(devPath)
	var arenaBase uint64
	if progs != nil {
		arenaBase = progs.Arena().BaseAddr()
	}
	next, freed := planReplaceAdmitRange(admits, entry, arenaBase)
	admits = next
	metrics.Global.AdmitsCurrent.Store(uint64(len(admits)))
	if progs != nil {
		for _, r := range freed {
			progs.FreeArenaChunk(r.Offset, r.Size)
		}
	}
	return admitArena(devPath, admits)
}

// refreshAdmits re-pushes the current snapshot to kmod without any
// mutation. Used by POST /policy/eval to force a policy round when
// steady state (no admissions, no evictions) would otherwise leave
// the vtable idle. Bench harnesses rely on this to observe more than
// the single prewarm-time eviction round.
func refreshAdmits(devPath string) (int, error) {
	admitMu.Lock()
	defer admitMu.Unlock()
	reconcileEvictionsLocked(devPath)
	if err := admitArena(devPath, admits); err != nil {
		return 0, err
	}
	return len(admits), nil
}

// AC-45 C2: capacity enforcement after a pcache populate.
//
// kmod only evicts by capacity inside admit_replace, and a pcache miss
// (populate ioctls + signal_admit) never calls it, so fd->buf kept growing
// until a policy eval or sync happened to come along.  After the last
// populate of a miss, and before signal_admit, handleReadMiss asks kmod to
// run its admit_replace pass (refreshAdmits).  The chunk just populated is
// still pending, so the pass cannot take it from the reader that waits for
// it; older files are evicted by policy score.  Concurrent misses are
// coalesced: one pass runs, and misses that arrive meanwhile share one
// follow-up pass.

// capacityEnforceFn is the pass itself; a var so tests can count calls.
var capacityEnforceFn = refreshAdmits

// capacityBytesFn reads the kmod's capacity_bytes parameter (0 = capacity
// mode off, legacy softcap voting).  A var so tests need no sysfs.
var capacityBytesFn = func() (int64, bool) { return getKmodParamInt64("capacity_bytes") }

var (
	capEnforceMu      sync.Mutex
	capEnforceRunning bool
	capEnforceAgain   bool
)

// enforceCapacityAfterPopulate runs (or joins) a capacity pass when kmod is
// in capacity mode.  Errors are logged only: the miss itself succeeded and
// the periodic loops remain the fallback.
func enforceCapacityAfterPopulate(devPath string) {
	if c, ok := capacityBytesFn(); !ok || c <= 0 {
		return
	}
	capEnforceMu.Lock()
	if capEnforceRunning {
		capEnforceAgain = true
		capEnforceMu.Unlock()
		return
	}
	capEnforceRunning = true
	capEnforceMu.Unlock()

	for {
		if _, err := capacityEnforceFn(devPath); err != nil {
			fmt.Fprintf(os.Stderr, "[capacity] post-populate pass: %v\n", err)
		}
		capEnforceMu.Lock()
		if !capEnforceAgain {
			capEnforceRunning = false
			capEnforceMu.Unlock()
			return
		}
		capEnforceAgain = false
		capEnforceMu.Unlock()
	}
}

// clearAdmits empties the daemon-side snapshot and tells kmod to
// drop the admit table. Called on daemon shutdown.
func clearAdmits(devPath string) error {
	admitMu.Lock()
	defer admitMu.Unlock()
	admits = admits[:0]
	metrics.Global.AdmitsCurrent.Store(0)
	return admitArena(devPath, nil)
}

// dropAdmits removes the listed inos from the daemon-side admit
// snapshot without pushing to kmod (kmod already dropped them via
// policy vote — pushing would re-add them). Called by the eviction
// feedback goroutine after S3FS_IOC_DRAIN_EVICTIONS.
//
// D-02: also returns each dropped entry's arena range to the
// free-list so a subsequent AdmitContent can reuse the bytes. This
// keeps the arena from growing monotonically under sustained
// churn. `progs` may be nil when the loader isn't wired (offline
// mode); free-list update is skipped in that case.
func dropAdmits(evicted []uint64, progs *fpebpf.Programs) int {
	if len(evicted) == 0 {
		return 0
	}
	admitMu.Lock()
	defer admitMu.Unlock()
	return dropAdmitsLocked(evicted, progs)
}

// dropAdmitIno removes every admit of ino and pushes the table to kmod.
func dropAdmitIno(devPath string, ino uint64, progs *fpebpf.Programs) {
	admitMu.Lock()
	defer admitMu.Unlock()
	if dropAdmitsLocked([]uint64{ino}, progs) > 0 {
		_ = admitArena(devPath, admits)
	}
}

// dropAdmitsLocked is dropAdmits with admitMu already held.
// Returns the number of admit entries actually removed (not the count of
// freed arena chunks, which may be smaller when progs==nil or Uaddr is
// outside the arena). T-02: prior code returned len(freedRanges) causing
// the "[evict-drain] dropped N admits" log to under-report when running
// without a wired arena.
func dropAdmitsLocked(evicted []uint64, progs *fpebpf.Programs) int {
	skip := make(map[uint64]struct{}, len(evicted))
	for _, ino := range evicted {
		skip[ino] = struct{}{}
	}
	filtered := admits[:0]
	var freedRanges []arenaChunkPair
	arenaBase := uint64(0)
	if progs != nil {
		arenaBase = progs.Arena().BaseAddr()
	}
	dropped := 0
	for _, e := range admits {
		if _, ok := skip[e.Ino]; ok {
			dropped++
			if progs != nil && arenaBase != 0 && e.Uaddr >= arenaBase {
				freedRanges = append(freedRanges, arenaChunkPair{
					Offset: e.Uaddr - arenaBase,
					Size:   e.Size,
				})
			}
			continue
		}
		filtered = append(filtered, e)
	}
	admits = filtered
	metrics.Global.AdmitsCurrent.Store(uint64(len(filtered)))
	if progs != nil {
		for _, r := range freedRanges {
			progs.FreeArenaChunk(r.Offset, r.Size)
		}
	}
	return dropped
}

// pruneAdmitsTo drops any admit whose ino is not in `keepInos`. Called
// after a bucket sync when a key disappeared from S3: kmod's dyn entry
// is already gone (shrink_dcache flushed the dentry), but the admit
// hash keeps the orphan ino. Without this, long-running daemons leak
// arena buffer + admit entries. See docs/journal/week4b-implementation-notes.md
// §4-2.
//
// T-01: also returns each dropped entry's arena range to the free-list
// (via admitProgs.FreeArenaChunk) so a prune round reclaims arena bytes
// the same way eviction feedback does. Prior code only incremented a
// counter, leaking arena memory on S3 object deletion.
func pruneAdmitsTo(devPath string, keepInos map[uint64]struct{}) error {
	admitMu.Lock()
	defer admitMu.Unlock()
	reconcileEvictionsLocked(devPath)
	arenaBase := uint64(0)
	if admitProgs != nil {
		arenaBase = admitProgs.Arena().BaseAddr()
	}
	pruned, freed := planPruneAdmits(admits, keepInos, arenaBase)
	dropped := len(admits) - len(pruned)
	admits = pruned
	metrics.Global.AdmitsCurrent.Store(uint64(len(admits)))
	if admitProgs != nil {
		for _, r := range freed {
			admitProgs.FreeArenaChunk(r.Offset, r.Size)
		}
	}
	if dropped == 0 {
		return nil // no delta, skip the ioctl
	}
	return admitArena(devPath, admits)
}

// addAdmitRange appends entry to the admit table and pushes the full
// snapshot to kmod WITHOUT evicting existing entries for the same ino.
// Used by the sequential prefetch path to stage the next chunk alongside
// the current one (replaceAdmit would evict the current chunk first).
// Idempotent: returns nil without ioctl if the exact (ino, offset) pair
// is already admitted.
func addAdmitRange(devPath string, entry admitEntry, progs *fpebpf.Programs) error {
	admitMu.Lock()
	defer admitMu.Unlock()
	reconcileEvictionsLocked(devPath)
	for _, e := range admits {
		if e.Ino == entry.Ino && e.Offset == entry.Offset {
			return nil // already present, skip ioctl
		}
	}
	admits = append(admits, entry)
	metrics.Global.AdmitsCurrent.Store(uint64(len(admits)))
	return admitArena(devPath, admits)
}
