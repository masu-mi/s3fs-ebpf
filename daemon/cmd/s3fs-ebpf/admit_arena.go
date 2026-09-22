package main

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"unsafe"

	fpebpf "github.com/bbsakura/s3fs-ebpf/daemon/internal/ebpf"
	"github.com/bbsakura/s3fs-ebpf/daemon/internal/s3"
	"golang.org/x/sys/unix"
)

// admitEntry mirrors `struct s3fs_admit_entry` in
// include/s3fs_ebpf_uapi.h. `Uaddr` is the daemon-side user_vm
// address of the object bytes.
type admitEntry struct {
	Ino   uint64
	Uaddr uint64
	Size  uint64
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
	return admitArena(devPath, snap)
}

// clearAdmits empties the daemon-side snapshot and tells kmod to
// drop the admit table. Called on daemon shutdown.
func clearAdmits(devPath string) error {
	admitMu.Lock()
	admits = admits[:0]
	admitMu.Unlock()
	return admitArena(devPath, nil)
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
	entry := fpebpf.LookupByIno(ev.Ino)
	if entry == nil {
		fmt.Fprintf(os.Stderr, "[upcall] ino=%d unknown — signaling ENOENT\n", ev.Ino)
		_ = signalAdmit(devPath, ev.Ino, -2 /* -ENOENT */)
		return
	}
	if s3c == nil {
		fmt.Fprintf(os.Stderr, "[upcall] ino=%d %s: no S3 backend — signaling ENODATA\n",
			ev.Ino, entry.Name)
		_ = signalAdmit(devPath, ev.Ino, -61 /* -ENODATA */)
		return
	}

	body, err := s3c.Get(ctx, entry.S3Key)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[upcall] ino=%d s3://%s get: %v\n",
			ev.Ino, entry.S3Key, err)
		_ = signalAdmit(devPath, ev.Ino, -5 /* -EIO */)
		return
	}
	off, err := progs.AdmitContent(body)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[upcall] ino=%d admit content: %v\n", ev.Ino, err)
		_ = signalAdmit(devPath, ev.Ino, -12 /* -ENOMEM */)
		return
	}
	uaddr := progs.Arena().BaseAddr() + uint64(off)

	if err := addAdmit(devPath, admitEntry{
		Ino: ev.Ino, Uaddr: uaddr, Size: uint64(len(body)),
	}); err != nil {
		fmt.Fprintf(os.Stderr, "[upcall] ino=%d admit arena: %v\n", ev.Ino, err)
		_ = signalAdmit(devPath, ev.Ino, -5 /* -EIO */)
		return
	}
	fmt.Printf("[upcall] ino=%d %s admitted %d bytes → uaddr=0x%x\n",
		ev.Ino, entry.Name, len(body), uaddr)
	_ = signalAdmit(devPath, ev.Ino, 0)
}
