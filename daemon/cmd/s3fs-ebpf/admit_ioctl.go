package main

import (
	"fmt"
	"os"
	"unsafe"

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
