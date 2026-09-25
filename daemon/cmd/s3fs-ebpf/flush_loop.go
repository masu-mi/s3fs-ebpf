package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"
	"unsafe"

	fpebpf "github.com/bbsakura/s3fs-ebpf/daemon/internal/ebpf"
	"github.com/bbsakura/s3fs-ebpf/daemon/internal/s3"
	"golang.org/x/sys/unix"
)

// flushEvent mirrors `struct s3fs_flush_event`.
type flushEvent struct {
	Ino     uint64
	Size    uint64
	NameLen uint32
	_pad    uint32
	Name    [syncNameMax + 1]byte
}

// readDirtyArg mirrors `struct s3fs_read_dirty`.
type readDirtyArg struct {
	Ino      uint64
	Uaddr    uint64
	Size     uint64
	OutBytes uint64
}

const (
	s3fsIocPopFlush  = (2 << 30) | (uint32(unsafe.Sizeof(flushEvent{})) << 16) | (0xE3 << 8) | 0x05
	s3fsIocReadDirty = (3 << 30) | (uint32(unsafe.Sizeof(readDirtyArg{})) << 16) | (0xE3 << 8) | 0x06
)

// popFlush pulls one event or returns unix.EAGAIN when the queue is
// empty. The dev fd must be R/W.
func popFlush(devFd int, out *flushEvent) error {
	_, _, errno := unix.Syscall(unix.SYS_IOCTL,
		uintptr(devFd), uintptr(s3fsIocPopFlush),
		uintptr(unsafe.Pointer(out)))
	if errno != 0 {
		return errno
	}
	return nil
}

// readDirty copies up to len(buf) bytes for `ino` into buf and
// returns the actual byte count.
func readDirty(devFd int, ino uint64, buf []byte) (int, error) {
	arg := readDirtyArg{
		Ino:  ino,
		Size: uint64(len(buf)),
	}
	if len(buf) > 0 {
		arg.Uaddr = uint64(uintptr(unsafe.Pointer(&buf[0])))
	}
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL,
		uintptr(devFd), uintptr(s3fsIocReadDirty),
		uintptr(unsafe.Pointer(&arg))); errno != 0 {
		return 0, errno
	}
	return int(arg.OutBytes), nil
}

// runFlushLoop polls /dev/s3fs_ebpf on `interval` and services each
// queued flush event by reading the current dirty bytes, PUTing them
// to S3, and refreshing the arena admit so subsequent reads see the
// fresh content instead of the stale prewarm bytes. Exits on ctx.
//
// s3c==nil disables PUTs but still drains the queue so kmod doesn't
// grow unbounded when someone runs without --s3.
func runFlushLoop(ctx context.Context, devPath string, progs *fpebpf.Programs,
	s3c *s3.Client, interval time.Duration) {
	if interval <= 0 {
		interval = 100 * time.Millisecond
	}
	f, err := os.OpenFile(devPath, os.O_RDWR, 0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[flush] open %s: %v\n", devPath, err)
		return
	}
	defer f.Close()

	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			drainFlushQueue(ctx, int(f.Fd()), devPath, progs, s3c)
		}
	}
}

func drainFlushQueue(ctx context.Context, devFd int, devPath string,
	progs *fpebpf.Programs, s3c *s3.Client) {
	for {
		var ev flushEvent
		if err := popFlush(devFd, &ev); err != nil {
			if errors.Is(err, unix.EAGAIN) {
				return
			}
			fmt.Fprintf(os.Stderr, "[flush] pop: %v\n", err)
			return
		}
		handleFlush(ctx, devFd, devPath, progs, s3c, &ev)
	}
}

func handleFlush(ctx context.Context, devFd int, devPath string,
	progs *fpebpf.Programs, s3c *s3.Client, ev *flushEvent) {
	name := string(ev.Name[:ev.NameLen])

	// Resolve S3 key: prefer the sync-mirrored entry, fall back to
	// the dentry name (the flat-bucket assumption at PoC scope, so
	// touch creates key=name).
	s3key := name
	if e := fpebpf.LookupByIno(ev.Ino); e != nil {
		s3key = e.S3Key
	}

	if s3c == nil {
		fmt.Printf("[flush] drop ino=%d name=%s (no S3 backend)\n",
			ev.Ino, name)
		return
	}

	buf := make([]byte, ev.Size)
	if ev.Size > 0 {
		n, err := readDirty(devFd, ev.Ino, buf)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[flush] ino=%d read dirty: %v\n",
				ev.Ino, err)
			return
		}
		buf = buf[:n]
	}
	putCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := s3c.Put(putCtx, s3key, buf); err != nil {
		fmt.Fprintf(os.Stderr, "[flush] ino=%d s3://%s put: %v\n",
			ev.Ino, s3key, err)
		return
	}
	registerIno(s3key, ev.Ino)
	fmt.Printf("[flush] ino=%d s3://%s put %d bytes\n",
		ev.Ino, s3key, len(buf))

	// Refresh admit so subsequent .read_folio serves the freshly
	// written bytes instead of the stale prewarm content. addAdmit
	// triggers invalidate_mapping_pages inside kmod, which drops
	// any old folios that were populated from the prior admit.
	if progs == nil {
		return
	}
	off, err := progs.AdmitContent(buf)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[flush] ino=%d admit content: %v\n",
			ev.Ino, err)
		return
	}
	if err := addAdmit(devPath, admitEntry{
		Ino:   ev.Ino,
		Uaddr: progs.Arena().BaseAddr() + uint64(off),
		Size:  uint64(len(buf)),
	}); err != nil {
		fmt.Fprintf(os.Stderr, "[flush] ino=%d admit arena: %v\n",
			ev.Ino, err)
	}
}
