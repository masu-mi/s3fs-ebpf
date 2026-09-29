package main

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
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

// admitTable is the daemon-side source of truth for what kmod
// currently has admitted. The kmod ioctl semantics are "replace the
// whole table"; every mutation therefore has to send the full
// snapshot. Guarded by admitMu.
var (
	admitMu sync.Mutex
	admits  []admitEntry
)

// setAdmits stores the initial (prewarm) admit list and pushes it to
// kmod. Any prior state is dropped.
func setAdmits(devPath string, initial []admitEntry) error {
	admitMu.Lock()
	admits = append(admits[:0], initial...)
	snap := append([]admitEntry(nil), admits...)
	admitMu.Unlock()
	metrics.Global.AdmitsCurrent.Store(uint64(len(snap)))
	return admitArena(devPath, snap)
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
	var oldChunk arenaChunkPair
	var oldChunkValid bool
	var arenaBase uint64
	if progs != nil {
		arenaBase = progs.Arena().BaseAddr()
	}
	filtered := admits[:0]
	for _, e := range admits {
		if e.Ino == entry.Ino {
			if progs != nil && arenaBase != 0 && e.Uaddr >= arenaBase {
				oldChunk = arenaChunkPair{
					offset: uint32(e.Uaddr - arenaBase),
					size:   uint32(e.Size),
				}
				oldChunkValid = true
			}
			continue
		}
		filtered = append(filtered, e)
	}
	admits = append(filtered, entry)
	snap := append([]admitEntry(nil), admits...)
	admitMu.Unlock()
	metrics.Global.AdmitsCurrent.Store(uint64(len(snap)))
	if oldChunkValid {
		progs.FreeArenaChunk(oldChunk.offset, oldChunk.size)
	}
	return admitArena(devPath, snap)
}

// refreshAdmits re-pushes the current snapshot to kmod without any
// mutation. Used by POST /policy/eval to force a policy round when
// steady state (no admissions, no evictions) would otherwise leave
// the vtable idle. Bench harnesses rely on this to observe more than
// the single prewarm-time eviction round.
func refreshAdmits(devPath string) (int, error) {
	admitMu.Lock()
	snap := append([]admitEntry(nil), admits...)
	admitMu.Unlock()
	if err := admitArena(devPath, snap); err != nil {
		return 0, err
	}
	return len(snap), nil
}

// clearAdmits empties the daemon-side snapshot and tells kmod to
// drop the admit table. Called on daemon shutdown.
func clearAdmits(devPath string) error {
	admitMu.Lock()
	admits = admits[:0]
	admitMu.Unlock()
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
	skip := make(map[uint64]struct{}, len(evicted))
	for _, ino := range evicted {
		skip[ino] = struct{}{}
	}
	admitMu.Lock()
	filtered := admits[:0]
	var freedRanges []arenaChunkPair
	arenaBase := uint64(0)
	if progs != nil {
		arenaBase = progs.Arena().BaseAddr()
	}
	for _, e := range admits {
		if _, ok := skip[e.Ino]; ok {
			if progs != nil && arenaBase != 0 && e.Uaddr >= arenaBase {
				freedRanges = append(freedRanges, arenaChunkPair{
					offset: uint32(e.Uaddr - arenaBase),
					size:   uint32(e.Size),
				})
			}
			continue
		}
		filtered = append(filtered, e)
	}
	admits = filtered
	admitMu.Unlock()
	metrics.Global.AdmitsCurrent.Store(uint64(len(filtered)))
	if progs != nil {
		for _, r := range freedRanges {
			progs.FreeArenaChunk(r.offset, r.size)
		}
	}
	return len(freedRanges)
}

// arenaChunkPair is dropAdmits's per-eviction bookkeeping — a
// tiny value type so the "compute-then-free" split doesn't need to
// hold admitMu while calling into progs.FreeArenaChunk.
type arenaChunkPair struct {
	offset uint32
	size   uint32
}

// pruneAdmitsTo drops any admit whose ino is not in `keepInos`. Called
// after a bucket sync when a key disappeared from S3: kmod's dyn entry
// is already gone (shrink_dcache flushed the dentry), but the admit
// hash keeps the orphan ino. Without this, long-running daemons leak
// arena buffer + admit entries. See docs/week4b-implementation-notes.md
// §4-2.
func pruneAdmitsTo(devPath string, keepInos map[uint64]struct{}) error {
	admitMu.Lock()
	pruned := admits[:0]
	dropped := 0
	for _, e := range admits {
		if _, ok := keepInos[e.Ino]; ok {
			pruned = append(pruned, e)
		} else {
			dropped++
		}
	}
	admits = pruned
	snap := append([]admitEntry(nil), admits...)
	admitMu.Unlock()
	metrics.Global.AdmitsCurrent.Store(uint64(len(snap)))
	if dropped == 0 {
		return nil // no delta, skip the ioctl
	}
	return admitArena(devPath, snap)
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

// admitArena issues S3FS_IOC_ADMIT_ARENA against /dev/s3fs_ebpf.
// Passing an empty `entries` slice clears the kmod-side admit table.
// The ioctl must be issued from a process whose mm holds the uaddr
// mappings (kmod calls get_task_mm(current) internally).
func admitArena(devPath string, entries []admitEntry) error {
	f, err := os.OpenFile(devPath, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("open %s: %w", devPath, err)
	}
	defer f.Close()
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
	_pad   uint32
}

// signalAdmitArg mirrors `struct s3fs_signal_admit`.
type signalAdmitArg struct {
	Ino  uint64
	Err  int32
	_pad uint32
}

const s3fsIocSignalAdmit = (1 << 30) | (uint32(unsafe.Sizeof(signalAdmitArg{})) << 16) | (0xE3 << 8) | 0x03

func signalAdmit(devPath string, ino uint64, errCode int32) error {
	f, err := os.OpenFile(devPath, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("open %s: %w", devPath, err)
	}
	defer f.Close()
	arg := signalAdmitArg{Ino: ino, Err: errCode}
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL,
		f.Fd(), uintptr(s3fsIocSignalAdmit),
		uintptr(unsafe.Pointer(&arg))); errno != 0 {
		return fmt.Errorf("ioctl SIGNAL_ADMIT: %w", errno)
	}
	return nil
}

// runReadMissLoop reads /dev/s3fs_ebpf and services each event by
// fetching the object from S3, admitting it, and signaling the
// blocked kmod .read. Terminates when the device read returns EOF or
// ctx is done.
func runReadMissLoop(ctx context.Context, devPath string,
	progs *fpebpf.Programs, s3c objstore.Backend) {
	f, err := os.OpenFile(devPath, os.O_RDWR, 0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[upcall] open %s: %v\n", devPath, err)
		return
	}
	defer f.Close()

	// Cancel-triggered close so blocked .read wakes with EOF.
	go func() {
		<-ctx.Done()
		f.Close()
	}()

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
		}
		fmt.Printf("[upcall] miss ino=%d offset=%d count=%d\n",
			ev.Ino, ev.Offset, ev.Count)
		go handleReadMiss(ctx, devPath, progs, s3c, ev)
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
)

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
	entry := fpebpf.LookupByIno(ev.Ino)
	if entry == nil {
		fmt.Fprintf(os.Stderr, "[upcall] ino=%d unknown — signaling ENOENT\n", ev.Ino)
		metrics.Global.ReadMissErrors.Add(1)
		_ = signalAdmit(devPath, ev.Ino, -2 /* -ENOENT */)
		return
	}
	if s3c == nil {
		fmt.Fprintf(os.Stderr, "[upcall] ino=%d %s: no S3 backend — signaling ENODATA\n",
			ev.Ino, entry.Name)
		metrics.Global.ReadMissErrors.Add(1)
		_ = signalAdmit(devPath, ev.Ino, -61 /* -ENODATA */)
		return
	}

	// D-01 partial admit path. Small files (< threshold): fetch
	// whole object, admit at offset=0. Large files: fetch just the
	// chunk covering ev.Offset via S3 range GET, admit with the
	// range's file offset so subsequent .read_folio reads at
	// nearby offsets hit the same chunk.
	var body []byte
	var chunkOffset int64 = 0
	fullSize := int64(entry.Fallback)
	var err error
	if fullSize > 0 && fullSize > partialAdmitThreshold {
		// Align to chunk boundary containing the miss.
		chunkOffset = (ev.Offset / partialAdmitChunk) * partialAdmitChunk
		chunkSize := int64(partialAdmitChunk)
		if chunkOffset+chunkSize > fullSize {
			chunkSize = fullSize - chunkOffset
		}
		body, err = s3c.GetRange(ctx, entry.S3Key, chunkOffset, chunkSize)
	} else {
		body, err = s3c.Get(ctx, entry.S3Key)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "[upcall] ino=%d s3://%s get: %v\n",
			ev.Ino, entry.S3Key, err)
		metrics.Global.ReadMissErrors.Add(1)
		_ = signalAdmit(devPath, ev.Ino, -5 /* -EIO */)
		return
	}
	off, err := progs.AdmitContent(body)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[upcall] ino=%d admit content: %v\n", ev.Ino, err)
		metrics.Global.ReadMissErrors.Add(1)
		_ = signalAdmit(devPath, ev.Ino, -12 /* -ENOMEM */)
		return
	}
	uaddr := progs.Arena().BaseAddr() + uint64(off)
	// Track high-water arena usage. AdmitContent returns the offset
	// where it wrote; assume monotonic (§4.5 note on arena gc未実装).
	if hi := uint64(off) + uint64(len(body)); hi > metrics.Global.ArenaUsedBytes.Load() {
		metrics.Global.ArenaUsedBytes.Store(hi)
	}

	if err := replaceAdmit(devPath, admitEntry{
		Ino:    ev.Ino,
		Uaddr:  uaddr,
		Size:   uint64(len(body)),
		Offset: uint64(chunkOffset),
	}, progs); err != nil {
		fmt.Fprintf(os.Stderr, "[upcall] ino=%d admit arena: %v\n", ev.Ino, err)
		metrics.Global.ReadMissErrors.Add(1)
		_ = signalAdmit(devPath, ev.Ino, -5 /* -EIO */)
		return
	}
	if chunkOffset > 0 {
		fmt.Printf("[upcall] ino=%d %s admitted range [%d,%d) → uaddr=0x%x (partial)\n",
			ev.Ino, entry.Name, chunkOffset, chunkOffset+int64(len(body)), uaddr)
	} else {
		fmt.Printf("[upcall] ino=%d %s admitted %d bytes → uaddr=0x%x\n",
			ev.Ino, entry.Name, len(body), uaddr)
	}
	_ = signalAdmit(devPath, ev.Ino, 0)
}
