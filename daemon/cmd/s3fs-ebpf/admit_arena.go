package main

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"sync"
	"time"
	"unsafe"

	fpebpf "github.com/bbsakura/s3fs-ebpf/daemon/internal/ebpf"
	"github.com/bbsakura/s3fs-ebpf/daemon/internal/metrics"
	"github.com/bbsakura/s3fs-ebpf/daemon/internal/objstore"
	"golang.org/x/sys/unix"
)

// admitEntry mirrors `struct s3fs_admit_entry` in
// include/s3fs_ebpf_uapi.h. `Uaddr` is the daemon-side user_vm
// address of the range's bytes. `Offset` is the file offset where
// the range starts (D-01); Offset=0 & Size=full_file is the
// whole-file admit (backward-compat default).
type admitEntry struct {
	Ino    uint64
	Uaddr  uint64
	Size   uint64
	Offset uint64
}

// AB-04: populatePagesArg mirrors `struct s3fs_populate_pages`.
// The daemon hands S3 data to kmod via this ioctl so kmod can store it
// in fd->buf (kernel memory) and insert a PCACHE-mode admit entry.
// Reads then use s3fs_folio_fill_from_kmalloc (no GUP).
type populatePagesArg struct {
	Ino     uint64
	Offset  uint64
	Size    uint64
	DataPtr uint64 // user pointer to data bytes (unsafe.Pointer value)
}

// s3fsIocPopulatePages = _IOW(0xE3, 0x0E, struct s3fs_populate_pages)
// _IOW = (1<<30) | (size<<16) | (magic<<8) | nr
const s3fsIocPopulatePages = (1 << 30) | (uint32(unsafe.Sizeof(populatePagesArg{})) << 16) | (0xE3 << 8) | 0x0E

// populatePages sends S3 data to kmod via S3FS_IOC_POPULATE_PAGES.
// kmod copies the bytes into the inode's fd->buf and inserts a PCACHE admit.
// Returns an error if the inode is dirty (user-written content takes precedence).
func populatePages(devPath string, ino uint64, offset int64, data []byte) error {
	if len(data) == 0 {
		return nil
	}
	f, err := os.OpenFile(devPath, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("open %s: %w", devPath, err)
	}
	defer f.Close()
	return populatePagesOn(int(f.Fd()), ino, offset, data)
}

func populatePagesOn(devFd int, ino uint64, offset int64, data []byte) error {
	if len(data) == 0 {
		return nil
	}
	arg := populatePagesArg{
		Ino:     ino,
		Offset:  uint64(offset),
		Size:    uint64(len(data)),
		DataPtr: uint64(uintptr(unsafe.Pointer(&data[0]))),
	}
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL,
		uintptr(devFd), uintptr(s3fsIocPopulatePages),
		uintptr(unsafe.Pointer(&arg))); errno != 0 {
		return fmt.Errorf("ioctl POPULATE_PAGES ino=%d: %w", ino, errno)
	}
	return nil
}

// updateArenaHighWater advances the ArenaUsedBytes gauge when `off+size`
// exceeds the current high-water mark. Called after every AdmitContent so
// /metrics reflects the furthest byte written into the arena.
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
			progs.FreeArenaChunk(r.offset, r.size)
		}
	}
	return admitArena(devPath, admits)
}

// planReplaceAdmit is the pure-function core of replaceAdmit: it
// filters any prior entry sharing entry.Ino out of `admits`, appends
// the fresh entry, and reports all evicted entries' arena ranges so
// the caller can return them to the free-list. Split out so the dedup
// invariant can be unit-tested without an eBPF loader or the
// /dev/s3fs_ebpf ioctl.
//
// T-04: returns freed []arenaChunkPair (all matches) rather than a
// single oldChunk. With D-01 range-list, the same ino can have
// multiple entries in admits; the prior single-value return discarded
// all but the last match's chunk, leaking arena bytes.
//
// arenaBase==0 or e.Uaddr < arenaBase suppresses the chunk report —
// used in offline mode and as a defensive guard against garbage
// uaddrs. The returned slice shares backing storage with `admits`
// (in-place filter), matching the previous implementation.
func planReplaceAdmit(admits []admitEntry, entry admitEntry, arenaBase uint64) (
	next []admitEntry, freed []arenaChunkPair,
) {
	filtered := admits[:0]
	for _, e := range admits {
		if e.Ino == entry.Ino {
			if arenaBase != 0 && e.Uaddr >= arenaBase {
				freed = append(freed, arenaChunkPair{
					offset: e.Uaddr - arenaBase,
					size:   e.Size,
				})
			}
			continue
		}
		filtered = append(filtered, e)
	}
	next = append(filtered, entry)
	return next, freed
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
			progs.FreeArenaChunk(r.offset, r.size)
		}
	}
	return admitArena(devPath, admits)
}

// planReplaceAdmitRange is the pure core of replaceAdmitRange: entries of
// entry.Ino whose file range [Offset, Offset+Size) overlaps the new one are
// removed (and reported in freed, like planReplaceAdmit), everything else is
// kept, and entry is appended.  A whole-object entry overlaps every range of
// its ino, so it behaves exactly like planReplaceAdmit.
func planReplaceAdmitRange(admits []admitEntry, entry admitEntry, arenaBase uint64) (
	next []admitEntry, freed []arenaChunkPair,
) {
	filtered := admits[:0]
	for _, e := range admits {
		if e.Ino == entry.Ino && rangesOverlap(e.Offset, e.Size, entry.Offset, entry.Size) {
			if arenaBase != 0 && e.Uaddr >= arenaBase {
				freed = append(freed, arenaChunkPair{
					offset: e.Uaddr - arenaBase,
					size:   e.Size,
				})
			}
			continue
		}
		filtered = append(filtered, e)
	}
	next = append(filtered, entry)
	return next, freed
}

// rangesOverlap reports whether [aOff, aOff+aSize) and [bOff, bOff+bSize)
// share a byte.  A zero-length range overlaps nothing.
func rangesOverlap(aOff, aSize, bOff, bSize uint64) bool {
	return aSize > 0 && bSize > 0 && aOff < bOff+bSize && bOff < aOff+aSize
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
					offset: e.Uaddr - arenaBase,
					size:   e.Size,
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
			progs.FreeArenaChunk(r.offset, r.size)
		}
	}
	return dropped
}

// arenaChunkPair is dropAdmits's per-eviction bookkeeping — a
// tiny value type so the "compute-then-free" split doesn't need to
// hold admitMu while calling into progs.FreeArenaChunk.
// T-05: uint64 so offsets > 4 GiB are handled correctly if the arena
// is ever extended; current arena is 128 MiB so this is defensive.
type arenaChunkPair struct {
	offset uint64
	size   uint64
}

// planPruneAdmits is the pure-function core of pruneAdmitsTo: given the
// current snapshot, it returns the filtered slice (keeping only inos in
// keepInos) and the arena ranges of the dropped entries so the caller can
// return them to the D-02 free-list. arenaBase==0 suppresses chunk
// reporting (offline mode). Split out so the arena-free invariant can be
// unit-tested without an eBPF loader.
func planPruneAdmits(
	snapshot []admitEntry,
	keepInos map[uint64]struct{},
	arenaBase uint64,
) (pruned []admitEntry, freed []arenaChunkPair) {
	pruned = snapshot[:0]
	for _, e := range snapshot {
		if _, ok := keepInos[e.Ino]; ok {
			pruned = append(pruned, e)
			continue
		}
		if arenaBase != 0 && e.Uaddr >= arenaBase {
			freed = append(freed, arenaChunkPair{
				offset: e.Uaddr - arenaBase,
				size:   e.Size,
			})
		}
	}
	return pruned, freed
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
			admitProgs.FreeArenaChunk(r.offset, r.size)
		}
	}
	if dropped == 0 {
		return nil // no delta, skip the ioctl
	}
	return admitArena(devPath, admits)
}

// admitArenaArg mirrors `struct s3fs_admit_arena`.
type admitArenaArg struct {
	Count   uint32
	_pad    uint32
	Entries uint64
}

// s3fsIocAdmitArena encodes _IOW(0xE3, 0x02, struct s3fs_admit_arena).
// _IOW direction = 1; size = sizeof(admitArenaArg) = 16 bytes.
const s3fsIocAdmitArena = (1 << 30) | (uint32(unsafe.Sizeof(admitArenaArg{})) << 16) | (0xE3 << 8) | 0x02

// admitArena is a package var (rather than a plain func) so the
// concurrent-push stress test can swap in a snapshot-capturing fake
// without wiring a real /dev/s3fs_ebpf. Production callers get the
// ioctl-backed implementation below.
var admitArena = admitArenaIoctl

// admitArenaIoctl issues S3FS_IOC_ADMIT_ARENA against /dev/s3fs_ebpf.
// Passing an empty `entries` slice clears the kmod-side admit table.
// The ioctl must be issued from a process whose mm holds the uaddr
// mappings (kmod calls get_task_mm(current) internally).
func admitArenaIoctl(devPath string, entries []admitEntry) error {
	f, closeFn, err := devFileOrOpen(devPath)
	if err != nil {
		return err
	}
	defer closeFn()
	return admitArenaOn(f, entries)
}

// admitArenaOn is admitArena on a caller-provided fd. Reuse when
// several ioctls should share one open (mm anchoring, etc.).
func admitArenaOn(f *os.File, entries []admitEntry) error {
	arg := admitArenaArg{
		Count: uint32(len(entries)),
	}
	if len(entries) > 0 {
		arg.Entries = uint64(uintptr(unsafe.Pointer(&entries[0])))
	}
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL,
		f.Fd(), uintptr(s3fsIocAdmitArena),
		uintptr(unsafe.Pointer(&arg))); errno != 0 {
		return fmt.Errorf("ioctl ADMIT_ARENA: %w", errno)
	}
	return nil
}

// -- C: read-miss upcall reader + handler ---------------------------------

// readMissEvent mirrors `struct s3fs_read_miss_event` in
// include/s3fs_ebpf_uapi.h. Layout is fixed by kmod's .read handler.
type readMissEvent struct {
	Ino    uint64
	Offset int64
	Count  uint32
	Flags  uint32
}

// missFlagForWrite mirrors S3FS_MISS_FLAG_FOR_WRITE (AC-50): kmod is about
// to modify the file and needs the chunk at Offset loaded into its fd.
const missFlagForWrite = 0x1

// signalAdmitArg mirrors `struct s3fs_signal_admit` (U13: added ChunkOffset).
type signalAdmitArg struct {
	Ino         uint64
	ChunkOffset uint64
	Err         int32
	Flags       uint32
}

// s3fsSignalVerifyPopulated mirrors S3FS_SIGNAL_VERIFY_POPULATED: kmod only
// wakes the waiter if the chunk is cached, else returns EAGAIN (AC-45).
const s3fsSignalVerifyPopulated = 0x1

const s3fsIocSignalAdmit = (1 << 30) | (uint32(unsafe.Sizeof(signalAdmitArg{})) << 16) | (0xE3 << 8) | 0x03

func signalAdmit(devPath string, ino uint64, chunkOffset int64, errCode int32) error {
	return signalAdmitFlags(devPath, ino, chunkOffset, errCode, 0)
}

// signalAdmitFlags is signalAdmit with S3FS_SIGNAL_* flags.
func signalAdmitFlags(devPath string, ino uint64, chunkOffset int64, errCode int32, flags uint32) error {
	f, closeFn, err := devFileOrOpen(devPath)
	if err != nil {
		return err
	}
	defer closeFn()
	arg := signalAdmitArg{Ino: ino, ChunkOffset: uint64(chunkOffset), Err: errCode, Flags: flags}
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL,
		f.Fd(), uintptr(s3fsIocSignalAdmit),
		uintptr(unsafe.Pointer(&arg))); errno != 0 {
		return fmt.Errorf("ioctl SIGNAL_ADMIT: %w", errno)
	}
	return nil
}

// handleLoadForWrite serves a miss that kmod flagged S3FS_MISS_FLAG_FOR_WRITE
// (AC-50): load the 4 MiB chunk containing ev.Offset into the ino's fd with
// POPULATE_PAGES — whatever the admit mode, since the bytes are for the
// write buffer, not the arena — and signal the waiter once kmod confirms
// the chunk is cached.
func handleLoadForWrite(ctx context.Context, devPath string, s3c objstore.Backend, ev readMissEvent) {
	missChunkOffset := (ev.Offset / partialAdmitChunk) * partialAdmitChunk
	fail := func(code int32, format string, a ...any) {
		fmt.Fprintf(os.Stderr, "[upcall] ino=%d load-for-write: "+format+"\n",
			append([]any{ev.Ino}, a...)...)
		metrics.Global.ReadMissErrors.Add(1)
		_ = signalAdmit(devPath, ev.Ino, missChunkOffset, code)
	}
	entry := fpebpf.LookupByIno(ev.Ino)
	if entry == nil {
		fail(-2 /* -ENOENT */, "unknown ino")
		return
	}
	if s3c == nil {
		fail(-61 /* -ENODATA */, "no S3 backend")
		return
	}
	size := int64(partialAdmitChunk)
	if rest := int64(entry.Fallback) - missChunkOffset; rest < size {
		size = rest
	}
	// kmod sends the length the chunk must hold up to its i_size. The
	// entry's size is older when the file was appended and flushed since the
	// last sync; a chunk cut at the old size never becomes populated and the
	// write fails with EIO.
	if want := int64(ev.Count); want > size && want <= partialAdmitChunk {
		size = want
	}
	if size <= 0 {
		fail(-5 /* -EIO */, "chunk at %d is past the object (%d B)", missChunkOffset, entry.Fallback)
		return
	}
	var body []byte
	var err error
	if mc, ok := s3c.(objstore.MultipartCopier); ok {
		// AC-51: the chunks loaded in one modify cycle must come from the
		// same object the flush copies the rest from.
		etag, perr := pinETag(ctx, s3c, ev.Ino, entry.S3Key)
		if perr != nil {
			fail(-5, "s3://%s stat: %v", entry.S3Key, perr)
			return
		}
		body, err = mc.GetRangeMatch(ctx, entry.S3Key, missChunkOffset, size, etag)
	} else {
		body, err = s3c.GetRange(ctx, entry.S3Key, missChunkOffset, size)
	}
	if err != nil {
		fail(-5, "s3://%s get: %v", entry.S3Key, err)
		return
	}
	for attempt := 0; ; attempt++ {
		if err := populatePages(devPath, ev.Ino, missChunkOffset, body); err != nil {
			fail(-5, "populate pages: %v", err)
			return
		}
		fmt.Printf("[upcall] ino=%d %s load-for-write [%d,%d)\n",
			ev.Ino, entry.Name, missChunkOffset, missChunkOffset+int64(len(body)))
		err := signalAdmitFlags(devPath, ev.Ino, missChunkOffset, 0, s3fsSignalVerifyPopulated)
		if !errors.Is(err, unix.EAGAIN) {
			return // woken, or nobody is waiting (ENOENT)
		}
		if attempt >= signalVerifyRetries {
			_ = signalAdmit(devPath, ev.Ino, missChunkOffset, 0)
			return
		}
		metrics.Global.ReadMissRepopulates.Add(1)
	}
}

// signalVerifyRetries bounds how often handleReadMiss re-populates when kmod
// reports the chunk was evicted before the success signal (AC-45).
const signalVerifyRetries = 3

// readMissWorkers is the maximum number of concurrent handleReadMiss
// goroutines. Bounded to prevent unbounded goroutine growth under
// bursty kmod .read events (V-06). 16 workers overlap S3 round-trips
// without overwhelming the S3 client or the arena allocator.
const readMissWorkers = 16

// runReadMissLoop reads /dev/s3fs_ebpf and services each event by
// fetching the object from S3, admitting it, and signaling the
// blocked kmod .read. Terminates when the device read returns EOF or
// ctx is done.
//
// V-06: goroutine spawning is bounded by a semaphore channel with
// readMissWorkers slots. A full semaphore back-pressures new events;
// kmod's per-ino wait queue serializes them so no event is lost.
func runReadMissLoop(ctx context.Context, devPath string,
	progs *fpebpf.Programs, s3c objstore.Backend) {
	f, err := os.OpenFile(devPath, os.O_RDWR, 0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[upcall] open %s: %v\n", devPath, err)
		return
	}
	defer f.Close()

	// Cancel-triggered close so the blocked kmod-side .read wakes
	// with EOF. The 5s deadline is an H-05 observability net: on any
	// modern kernel .release drops the wait queue, but a stuck kmod
	// (buggy hook, stale mm ref) could leave Read hanging forever.
	// Emit an audible warning so bench harnesses and operators can
	// tell whether shutdown actually completed.
	loopExit := make(chan struct{})
	go func() {
		<-ctx.Done()
		f.Close()
		select {
		case <-loopExit:
		case <-time.After(readMissShutdownWarn):
			fmt.Fprintf(os.Stderr,
				"[upcall] SHUTDOWN WARN: read loop still blocked %s after close — check kmod .release path\n",
				readMissShutdownWarn)
		}
	}()
	defer close(loopExit)

	sem := make(chan struct{}, readMissWorkers)

	var buf [24]byte // sizeof(readMissEvent)
	for {
		n, err := f.Read(buf[:])
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, os.ErrClosed) {
				fmt.Fprintf(os.Stderr, "[upcall] read: %v\n", err)
			}
			return
		}
		if n < 24 {
			fmt.Fprintf(os.Stderr, "[upcall] short read: %d bytes\n", n)
			continue
		}
		ev := readMissEvent{
			Ino:    binary.LittleEndian.Uint64(buf[0:8]),
			Offset: int64(binary.LittleEndian.Uint64(buf[8:16])),
			Count:  binary.LittleEndian.Uint32(buf[16:20]),
			Flags:  binary.LittleEndian.Uint32(buf[20:24]),
		}
		fmt.Printf("[upcall] miss ino=%d offset=%d count=%d\n",
			ev.Ino, ev.Offset, ev.Count)
		sem <- struct{}{}
		go func(ev readMissEvent) {
			defer func() { <-sem }()
			handleReadMiss(ctx, devPath, progs, s3c, ev)
		}(ev)
	}
}

// PartialAdmitThreshold — files above this size are admitted as a
// single chunk covering just the miss offset (D-01). Below, we
// fetch the whole object (fewer requests, keeps small-file behavior
// identical to pre-D-01). 4 MiB matches S3-tier defaults for small
// PoC deployments; production would set larger.
const (
	partialAdmitThreshold = 4 * 1024 * 1024
	partialAdmitChunk     = 4 * 1024 * 1024

	// seqAdmitRatioThreshold — minimum SeqRatio (from s3fs_read_pattern_map)
	// above which a large-file miss is promoted to a full-file GET.
	// R-01: fentry/generic_file_read_iter populates the map before kmod's
	// .read_folio blocks, so by the time the goroutine runs the ratio often
	// already reflects the sequential access character.
	seqAdmitRatioThreshold = 0.70
)

// fullAdmitExceedsCapacity reports whether promoting a miss to a full-file
// GET would populate more than kmod's capacity_bytes holds (AC-45).  In
// pcache mode the whole body lands in fd chunks before any eviction can
// run, so a file larger than the capacity would only be populated to be
// evicted again; such files keep the chunk-by-chunk admit.
func fullAdmitExceedsCapacity(fullSize int64) bool {
	if !arenaMetadataOnlyEnabled {
		return false
	}
	c, ok := capacityBytesFn()
	return ok && c > 0 && fullSize > c
}

// patternAwareAdmitEnabled gates the R-01 sequential-pattern logic in
// handleReadMiss. Set once by main before any goroutine reads it.
var patternAwareAdmitEnabled bool

// readMissShutdownWarn bounds how long the shutdown watchdog waits
// for the blocking Read to wake after Close before logging. Tuned
// pessimistic — real kmod releases finish in μs.
var readMissShutdownWarn = 5 * time.Second

func handleReadMiss(ctx context.Context, devPath string,
	progs *fpebpf.Programs, s3c objstore.Backend, ev readMissEvent) {
	metrics.Global.ReadMissUpcalls.Add(1)
	// Time the full miss-service path (S3 Get + arena write +
	// admit_replace) and observe on every return path — including
	// the ENOENT / EIO / ENODATA branches so the histogram reflects
	// real tail behavior, not just the happy path. Bench uses this
	// to compare policies by upcall latency, not just hit rate.
	miss_started := time.Now()
	defer func() {
		metrics.ReadMissLatency.Observe(time.Since(miss_started))
	}()
	if ev.Flags&missFlagForWrite != 0 {
		handleLoadForWrite(ctx, devPath, s3c, ev)
		return
	}
	// U13: compute the aligned chunk boundary that kmod used when creating
	// the pending entry for this miss (matches pending_get_or_create key).
	// ALL signalAdmit calls below must use missChunkOffset so they find
	// the correct per-chunk pending entry.
	missChunkOffset := (ev.Offset / partialAdmitChunk) * partialAdmitChunk

	entry := fpebpf.LookupByIno(ev.Ino)
	if entry == nil {
		fmt.Fprintf(os.Stderr, "[upcall] ino=%d unknown — signaling ENOENT\n", ev.Ino)
		metrics.Global.ReadMissErrors.Add(1)
		_ = signalAdmit(devPath, ev.Ino, missChunkOffset, -2 /* -ENOENT */)
		return
	}
	if s3c == nil {
		fmt.Fprintf(os.Stderr, "[upcall] ino=%d %s: no S3 backend — signaling ENODATA\n",
			ev.Ino, entry.Name)
		metrics.Global.ReadMissErrors.Add(1)
		_ = signalAdmit(devPath, ev.Ino, missChunkOffset, -61 /* -ENODATA */)
		return
	}

	// D-01 partial admit path. Small files (< threshold): fetch
	// whole object, admit at offset=0. Large files: fetch just the
	// chunk covering ev.Offset via S3 range GET, admit with the
	// range's file offset so subsequent .read_folio reads at
	// nearby offsets hit the same chunk.
	//
	// R-01 pattern-aware override: when --pattern-aware-admit is set
	// and s3fs_read_pattern_map shows a strong sequential signal, promote
	// large-file chunk admits to a full-file GET. The fentry hook fires
	// before kmod's .read_folio blocks, so by the time this goroutine runs
	// seq_count is often already ≥ 1, making SeqRatio() = 1.0 on the first
	// miss and saving the remaining chunk round-trips.
	var body []byte
	var chunkOffset int64 = 0
	fullSize := int64(entry.Fallback)
	var err error
	isLarge := fullSize > 0 && fullSize > partialAdmitThreshold
	useFullAdmit := !isLarge // small files always use full admit
	if !useFullAdmit && patternAwareAdmitEnabled && progs != nil &&
		!fullAdmitExceedsCapacity(fullSize) {
		if pat, ok := progs.ReadAccessPattern(ev.Ino); ok && pat.SeqRatio() >= seqAdmitRatioThreshold {
			useFullAdmit = true
			fmt.Printf("[upcall] ino=%d %s seq_ratio=%.2f PROMOTE_SIGNAL=pattern-aware full admit (%d bytes)\n",
				ev.Ino, entry.Name, pat.SeqRatio(), fullSize)
		}
	}
	if useFullAdmit && isLarge && arenaMetadataOnlyEnabled {
		// AC-52 step 3: parallel range GETs, populated and signalled per chunk.
		serveChunkLoad(ctx, devPath, s3c, ev, entry.S3Key, entry.Name, fullSize, missChunkOffset)
		return
	}
	if useFullAdmit {
		body, err = s3c.Get(ctx, entry.S3Key)
	} else {
		// Align to chunk boundary containing the miss.
		chunkOffset = missChunkOffset
		chunkSize := int64(partialAdmitChunk)
		if chunkOffset+chunkSize > fullSize {
			chunkSize = fullSize - chunkOffset
		}
		body, err = s3c.GetRange(ctx, entry.S3Key, chunkOffset, chunkSize)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "[upcall] ino=%d s3://%s get: %v\n",
			ev.Ino, entry.S3Key, err)
		metrics.Global.ReadMissErrors.Add(1)
		_ = signalAdmit(devPath, ev.Ino, missChunkOffset, -5 /* -EIO */)
		return
	}
	// AB-04: when --arena-metadata-only, bypass BPF arena and send data
	// directly to kmod's page cache via POPULATE_PAGES ioctl.
	// The kmod limits each call to S3FS_POPULATE_MAX_SIZE (64 MiB), so split
	// body into partialAdmitChunk-sized pieces; full-admit bodies (seq promote)
	// can be arbitrarily large.
	if arenaMetadataOnlyEnabled {
		for attempt := 0; ; attempt++ {
			off := chunkOffset
			rem := body
			for len(rem) > 0 {
				n := len(rem)
				if n > partialAdmitChunk {
					n = partialAdmitChunk
				}
				if err := populatePages(devPath, ev.Ino, off, rem[:n]); err != nil {
					fmt.Fprintf(os.Stderr, "[upcall] ino=%d populate pages: %v\n", ev.Ino, err)
					metrics.Global.ReadMissErrors.Add(1)
					_ = signalAdmit(devPath, ev.Ino, missChunkOffset, -5 /* -EIO */)
					return
				}
				off += int64(n)
				rem = rem[n:]
			}
			fmt.Printf("[upcall] ino=%d %s pcache [%d,%d)\n",
				ev.Ino, entry.Name, chunkOffset, chunkOffset+int64(len(body)))
			// AC-45 C2: enforce capacity while this miss is still pending.
			enforceCapacityAfterPopulate(devPath)
			// AC-45: kmod wakes the waiter only if the chunk is still
			// cached.  Concurrent handlers for one ino and the capacity
			// pass can evict what we just populated; populate again
			// (bounded) instead of waking the reader with nothing to read.
			err := signalAdmitFlags(devPath, ev.Ino, missChunkOffset, 0,
				s3fsSignalVerifyPopulated)
			if !errors.Is(err, unix.EAGAIN) {
				return // woken, or nobody is waiting (ENOENT)
			}
			if attempt >= signalVerifyRetries {
				// Give up verifying: wake the waiter so its own retry /
				// fallback path takes over rather than hanging.
				_ = signalAdmit(devPath, ev.Ino, missChunkOffset, 0)
				return
			}
			metrics.Global.ReadMissRepopulates.Add(1)
		}
	}

	// Arena path (default).
	off, err := progs.AdmitContent(body)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[upcall] ino=%d admit content: %v\n", ev.Ino, err)
		metrics.Global.ReadMissErrors.Add(1)
		_ = signalAdmit(devPath, ev.Ino, missChunkOffset, -12 /* -ENOMEM */)
		return
	}
	uaddr := progs.Arena().BaseAddr() + off
	updateArenaHighWater(off, len(body))

	// AC-49: replace only the ranges this admit overlaps — a chunk admit
	// must not evict the ino's other chunks.
	if err := replaceAdmitRange(devPath, admitEntry{
		Ino:    ev.Ino,
		Uaddr:  uaddr,
		Size:   uint64(len(body)),
		Offset: uint64(chunkOffset),
	}, progs); err != nil {
		fmt.Fprintf(os.Stderr, "[upcall] ino=%d admit arena: %v\n", ev.Ino, err)
		metrics.Global.ReadMissErrors.Add(1)
		_ = signalAdmit(devPath, ev.Ino, missChunkOffset, -5 /* -EIO */)
		return
	}
	if chunkOffset > 0 {
		fmt.Printf("[upcall] ino=%d %s admitted range [%d,%d) (partial)\n",
			ev.Ino, entry.Name, chunkOffset, chunkOffset+int64(len(body)))
	} else {
		fmt.Printf("[upcall] ino=%d %s admitted %d bytes\n",
			ev.Ino, entry.Name, len(body))
	}
	_ = signalAdmit(devPath, ev.Ino, missChunkOffset, 0)
}

// -- AA-04: sequential read-ahead prefetch --------------------------------

// prefetchSeqEnabled gates runSeqPrefetchLoop. Set once by main before any
// goroutine reads it.
var prefetchSeqEnabled bool

// arenaMetadataOnlyEnabled switches the miss handler to use
// S3FS_IOC_POPULATE_PAGES instead of AdmitContent + admitArena.
// Set once by main from the --arena-metadata-only flag.
var arenaMetadataOnlyEnabled bool

const (
	// prefetchInterval is how often the prefetch goroutine scans admitted inos.
	prefetchInterval = 200 * time.Millisecond
	// prefetchTriggerWindow is the number of bytes before the end of an
	// admitted chunk at which we start fetching the next chunk.
	// partialAdmitChunk / 2 = 2 MiB lookahead.
	prefetchTriggerWindow = partialAdmitChunk / 2
)

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

// seqPrefetchNext returns the offset of the next chunk to pre-admit for an
// ino, given the ino's current admit ranges and the last read position from
// the pattern map. Returns -1 when no prefetch is needed (file fully
// covered, trigger threshold not reached, or next chunk already admitted).
func seqPrefetchNext(inoAdmits []admitEntry, lastEnd uint64, fullSize uint64) int64 {
	if fullSize == 0 || lastEnd == 0 {
		return -1
	}
	for _, e := range inoAdmits {
		end := e.Offset + e.Size
		if lastEnd < e.Offset || lastEnd > end {
			continue
		}
		// lastEnd is within [e.Offset, end].
		// Only trigger when within prefetchTriggerWindow of the chunk boundary.
		if end < prefetchTriggerWindow || lastEnd < end-prefetchTriggerWindow {
			continue
		}
		nextStart := int64(end)
		if nextStart >= int64(fullSize) {
			return -1 // already at or past EOF
		}
		// Check if the next chunk is already admitted.
		for _, e2 := range inoAdmits {
			if int64(e2.Offset) == nextStart {
				return -1
			}
		}
		return nextStart
	}
	return -1
}

// runSeqPrefetchLoop periodically inspects admitted inos with a strong
// sequential signal and pre-admits the next chunk before the miss fires.
// Exits when ctx is cancelled. No-op when prefetchSeqEnabled is false.
func runSeqPrefetchLoop(ctx context.Context, devPath string,
	progs *fpebpf.Programs, s3c objstore.Backend) {

	if !prefetchSeqEnabled || progs == nil || s3c == nil {
		return
	}

	ticker := time.NewTicker(prefetchInterval)
	defer ticker.Stop()

	// inflight tracks in-flight prefetches so we don't double-fetch.
	// Key: (ino<<20 | chunkIndex) — chunkIndex = chunkOffset/partialAdmitChunk.
	var inflight sync.Map

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		// Snapshot admits under the lock to avoid holding it during S3 calls.
		admitMu.Lock()
		snapshot := append([]admitEntry(nil), admits...)
		admitMu.Unlock()

		// Group entries by ino.
		byIno := make(map[uint64][]admitEntry, len(snapshot))
		for _, e := range snapshot {
			byIno[e.Ino] = append(byIno[e.Ino], e)
		}

		for ino, inoAdmits := range byIno {
			pat, ok := progs.ReadAccessPattern(ino)
			if !ok || pat.SeqRatio() < seqAdmitRatioThreshold {
				continue
			}
			entry := fpebpf.LookupByIno(ino)
			if entry == nil || entry.Fallback == 0 {
				continue
			}
			nextOff := seqPrefetchNext(inoAdmits, pat.LastEndPos, entry.Fallback)
			if nextOff < 0 {
				continue
			}
			chunkIdx := uint64(nextOff) / partialAdmitChunk
			inflightKey := ino<<20 | chunkIdx
			if _, loaded := inflight.LoadOrStore(inflightKey, struct{}{}); loaded {
				continue
			}
			go func(ino uint64, chunkOffset int64, key uint64) {
				defer inflight.Delete(key)
				e := fpebpf.LookupByIno(ino)
				if e == nil {
					return
				}
				fullSize := int64(e.Fallback)
				chunkSize := int64(partialAdmitChunk)
				if chunkOffset+chunkSize > fullSize {
					chunkSize = fullSize - chunkOffset
				}
				body, err := s3c.GetRange(ctx, e.S3Key, chunkOffset, chunkSize)
				if err != nil {
					fmt.Printf("[prefetch] ino=%d %s offset=%d: %v\n",
						ino, e.Name, chunkOffset, err)
					return
				}
				off, err := progs.AdmitContent(body)
				if err != nil {
					fmt.Printf("[prefetch] ino=%d %s offset=%d admit: %v\n",
						ino, e.Name, chunkOffset, err)
					return
				}
				uaddr := progs.Arena().BaseAddr() + off
				updateArenaHighWater(off, len(body))
				if err := addAdmitRange(devPath, admitEntry{
					Ino:    ino,
					Uaddr:  uaddr,
					Size:   uint64(len(body)),
					Offset: uint64(chunkOffset),
				}, progs); err != nil {
					fmt.Printf("[prefetch] ino=%d %s offset=%d push: %v\n",
						ino, e.Name, chunkOffset, err)
					return
				}
				fmt.Printf("[prefetch] ino=%d %s range [%d,%d) pre-admitted\n",
					ino, e.Name, chunkOffset, chunkOffset+chunkSize)
			}(ino, nextOff, inflightKey)
		}
	}
}
