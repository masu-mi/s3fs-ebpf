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
	"github.com/bbsakura/s3fs-ebpf/daemon/internal/s3"
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

// addAdmit appends `entry` and pushes the whole snapshot to kmod.
// Callers should ensure the ino is not already admitted (kmod's
// hash keeps the first hit, so a duplicate is silently ignored on
// the kmod side but wastes hash bucket capacity).
func addAdmit(devPath string, entry admitEntry) error {
	admitMu.Lock()
	admits = append(admits, entry)
	snap := append([]admitEntry(nil), admits...)
	admitMu.Unlock()
	metrics.Global.AdmitsCurrent.Store(uint64(len(snap)))
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
func dropAdmits(evicted []uint64) int {
	if len(evicted) == 0 {
		return 0
	}
	skip := make(map[uint64]struct{}, len(evicted))
	for _, ino := range evicted {
		skip[ino] = struct{}{}
	}
	admitMu.Lock()
	filtered := admits[:0]
	dropped := 0
	for _, e := range admits {
		if _, ok := skip[e.Ino]; ok {
			dropped++
			continue
		}
		filtered = append(filtered, e)
	}
	admits = filtered
	admitMu.Unlock()
	metrics.Global.AdmitsCurrent.Store(uint64(len(filtered)))
	return dropped
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
	progs *fpebpf.Programs, s3c *s3.Client) {
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

func handleReadMiss(ctx context.Context, devPath string,
	progs *fpebpf.Programs, s3c *s3.Client, ev readMissEvent) {
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

	body, err := s3c.Get(ctx, entry.S3Key)
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

	if err := addAdmit(devPath, admitEntry{
		Ino: ev.Ino, Uaddr: uaddr, Size: uint64(len(body)),
	}); err != nil {
		fmt.Fprintf(os.Stderr, "[upcall] ino=%d admit arena: %v\n", ev.Ino, err)
		metrics.Global.ReadMissErrors.Add(1)
		_ = signalAdmit(devPath, ev.Ino, -5 /* -EIO */)
		return
	}
	fmt.Printf("[upcall] ino=%d %s admitted %d bytes → uaddr=0x%x\n",
		ev.Ino, entry.Name, len(body), uaddr)
	_ = signalAdmit(devPath, ev.Ino, 0)
}
