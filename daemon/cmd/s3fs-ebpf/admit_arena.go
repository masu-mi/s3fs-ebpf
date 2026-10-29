package main

import (
	"fmt"
	"os"
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

// programsArena adapts the eBPF loader to admit.Arena.
type programsArena struct{ p *fpebpf.Programs }

func (a programsArena) BaseAddr() uint64              { return a.p.Arena().BaseAddr() }
func (a programsArena) FreeArenaChunk(off, sz uint64) { a.p.FreeArenaChunk(off, sz) }

// arenaOf returns progs as an admit.Arena, or a nil interface for a nil
// loader (offline mode / tests).
func arenaOf(progs *fpebpf.Programs) admit.Arena {
	if progs == nil {
		return nil
	}
	return programsArena{progs}
}

// table is the process-wide admit table (see internal/admit).  The push and
// the evict-log drain go through admitArena / drainEvictionsFn so tests can
// swap them.
var table = admit.NewTable(
	func(dev string, es []admitEntry) error { return admitArena(dev, es) },
	func(dev string) ([]uint64, uint32, error) { return drainEvictionsFn(dev) },
)

// drainEvictionsFn is drainEvictions behind a var so tests can feed a fake
// kmod evict log.
var drainEvictionsFn = drainEvictions

// The admit-table operations keep their old names; the logic is in
// internal/admit.
func setAdmitProgs(p *fpebpf.Programs)                 { table.SetArena(arenaOf(p)) }
func setAdmits(dev string, initial []admitEntry) error { return table.Set(dev, initial) }
func replaceAdmit(dev string, e admitEntry, progs *fpebpf.Programs) error {
	return table.Replace(dev, e, arenaOf(progs))
}
func replaceAdmitRange(dev string, e admitEntry, progs *fpebpf.Programs) error {
	return table.ReplaceRange(dev, e, arenaOf(progs))
}
func addAdmitRange(dev string, e admitEntry, _ *fpebpf.Programs) error { return table.AddRange(dev, e) }
func refreshAdmits(dev string) (int, error)                            { return table.Refresh(dev) }
func clearAdmits(dev string) error                                     { return table.Clear(dev) }
func dropAdmits(evicted []uint64, progs *fpebpf.Programs) int {
	return table.Drop(evicted, arenaOf(progs))
}
func dropAdmitIno(dev string, ino uint64, progs *fpebpf.Programs) {
	table.DropIno(dev, ino, arenaOf(progs))
}
func pruneAdmitsTo(dev string, keep map[uint64]struct{}) error { return table.Prune(dev, keep) }

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
