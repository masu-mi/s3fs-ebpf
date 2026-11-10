package main

import (
	"fmt"
	"os"
	"unsafe"

	"github.com/bbsakura/s3fs-ebpf/daemon/internal/kmodio"
	"golang.org/x/sys/unix"
)

// s3fsIocPopulatePages = _IOW(0xE3, 0x0E, struct s3fs_populate_pages)
// _IOW = (1<<30) | (size<<16) | (magic<<8) | nr

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

// evalPass issues S3FS_IOC_EVAL: the kmod runs its capacity / policy pass now
// (replay hits, on_insert / on_size, votes, evict down to capacity_bytes).
// The returned count is always 0 (the kmod keeps no admit snapshot for the
// daemon to count any more); it stays so /policy/eval and the capacity hook
// keep their shape.
func evalPass(devPath string) (int, error) {
	f, closeFn, err := devFileOrOpen(devPath)
	if err != nil {
		return 0, err
	}
	defer closeFn()
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL,
		f.Fd(), uintptr(s3fsIocEval), 0); errno != 0 {
		return 0, fmt.Errorf("ioctl EVAL: %w", errno)
	}
	return 0, nil
}

// -- C: read-miss upcall reader + handler ---------------------------------

// s3fsDoneVerifyPopulated mirrors S3FS_DONE_VERIFY_POPULATED: kmod only wakes
// the waiter if the chunk is cached, else returns EAGAIN (AC-45).
const s3fsDoneVerifyPopulated = 0x1

// signalAdmit answers a MISS: wake the waiters of the chunk. seq is the seq of the
// request being answered (0: whichever plain-read record is live for the chunk, a
// chunk load signalling a chunk whose request it did not see).
func signalAdmit(devPath string, ino uint64, chunkOffset int64, seq uint64, errCode int32) error {
	return signalAdmitFlags(devPath, ino, chunkOffset, seq, errCode, 0)
}

// signalAdmitFlags is signalAdmit with S3FS_DONE_* flags.
func signalAdmitFlags(devPath string, ino uint64, chunkOffset int64, seq uint64, errCode int32, flags uint32) error {
	f, closeFn, err := devFileOrOpen(devPath)
	if err != nil {
		return err
	}
	defer closeFn()
	arg := doneArg{Type: kmodio.ReqMiss, Flags: flags, Ino: ino, ChunkOffset: uint64(chunkOffset), Seq: seq, Err: errCode}
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL,
		f.Fd(), uintptr(s3fsIocDone),
		uintptr(unsafe.Pointer(&arg))); errno != 0 {
		return fmt.Errorf("ioctl DONE: %w", errno)
	}
	return nil
}

// The upcall handlers reach the kmod through these variables so tests can
// replace the ioctls; production uses the real ones.
var (
	populatePagesFn    = populatePages
	signalAdmitFn      = signalAdmit
	signalAdmitFlagsFn = signalAdmitFlags
)
