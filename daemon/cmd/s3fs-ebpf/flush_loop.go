package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"
	"unsafe"

	fpebpf "github.com/bbsakura/s3fs-ebpf/daemon/internal/ebpf"
	"github.com/bbsakura/s3fs-ebpf/daemon/internal/metrics"
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

// markDirtyArg mirrors `struct s3fs_mark_dirty`.
type markDirtyArg struct {
	Ino uint64
}

// statsArg mirrors `struct s3fs_stats`.
const statsMax = 16
type statsArg struct {
	Count  uint32
	Filled uint32
	Values [statsMax]uint64
}

// drainEvictionsArg mirrors `struct s3fs_drain_evictions`.
const evictLogMax = 256
type drainEvictionsArg struct {
	Cap      uint32
	Filled   uint32
	Overflow uint32
	_pad     uint32
	Inos     [evictLogMax]uint64
}

const (
	s3fsIocPopFlush        = (2 << 30) | (uint32(unsafe.Sizeof(flushEvent{})) << 16) | (0xE3 << 8) | 0x05
	s3fsIocReadDirty       = (3 << 30) | (uint32(unsafe.Sizeof(readDirtyArg{})) << 16) | (0xE3 << 8) | 0x06
	s3fsIocMarkDirty       = (1 << 30) | (uint32(unsafe.Sizeof(markDirtyArg{})) << 16) | (0xE3 << 8) | 0x07
	s3fsIocStats           = (3 << 30) | (uint32(unsafe.Sizeof(statsArg{})) << 16) | (0xE3 << 8) | 0x08
	s3fsIocDrainEvictions  = (3 << 30) | (uint32(unsafe.Sizeof(drainEvictionsArg{})) << 16) | (0xE3 << 8) | 0x09
)

// drainEvictions returns the ino list kmod evicted since the last
// drain. Callers use this to prune their local admit snapshot so the
// next admit_replace doesn't re-send evicted inos.
func drainEvictions(devPath string) ([]uint64, uint32, error) {
	f, err := os.OpenFile(devPath, os.O_RDWR, 0)
	if err != nil {
		return nil, 0, fmt.Errorf("open %s: %w", devPath, err)
	}
	defer f.Close()
	arg := drainEvictionsArg{Cap: evictLogMax}
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL,
		f.Fd(), uintptr(s3fsIocDrainEvictions),
		uintptr(unsafe.Pointer(&arg))); errno != 0 {
		return nil, 0, fmt.Errorf("ioctl DRAIN_EVICTIONS: %w", errno)
	}
	out := make([]uint64, arg.Filled)
	copy(out, arg.Inos[:arg.Filled])
	return out, arg.Overflow, nil
}

// readStats issues S3FS_IOC_STATS and returns the value slots. Slot 0
// is s3fs_evictions_total; slot layout is UAPI-defined additive-only.
func readStats(devPath string) ([]uint64, error) {
	f, err := os.OpenFile(devPath, os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", devPath, err)
	}
	defer f.Close()
	arg := statsArg{Count: statsMax}
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL,
		f.Fd(), uintptr(s3fsIocStats),
		uintptr(unsafe.Pointer(&arg))); errno != 0 {
		return nil, fmt.Errorf("ioctl STATS: %w", errno)
	}
	out := make([]uint64, arg.Filled)
	copy(out, arg.Values[:arg.Filled])
	return out, nil
}

// markDirty tells kmod to re-set fd->dirty for `ino` after a PUT
// failure so the next .release/.fsync re-enqueues the flush event.
func markDirty(devFd int, ino uint64) error {
	arg := markDirtyArg{Ino: ino}
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL,
		uintptr(devFd), uintptr(s3fsIocMarkDirty),
		uintptr(unsafe.Pointer(&arg))); errno != 0 {
		return errno
	}
	return nil
}

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
	metrics.Global.FlushEvents.Add(1)
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
			metrics.Global.FlushErrors.Add(1)
			// Restore dirty flag so the next close/fsync will re-try.
			if mderr := markDirty(devFd, ev.Ino); mderr != nil {
				fmt.Fprintf(os.Stderr, "[flush] ino=%d mark dirty: %v\n",
					ev.Ino, mderr)
			}
			return
		}
		buf = buf[:n]
	}
	putCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := s3c.Put(putCtx, s3key, buf); err != nil {
		fmt.Fprintf(os.Stderr, "[flush] ino=%d s3://%s put: %v\n",
			ev.Ino, s3key, err)
		metrics.Global.FlushErrors.Add(1)
		if mderr := markDirty(devFd, ev.Ino); mderr != nil {
			fmt.Fprintf(os.Stderr, "[flush] ino=%d mark dirty: %v\n",
				ev.Ino, mderr)
		}
		return
	}
	registerIno(s3key, ev.Ino)
	metrics.Global.FlushBytes.Add(uint64(len(buf)))
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
		metrics.Global.FlushErrors.Add(1)
		return
	}
	if hi := uint64(off) + uint64(len(buf)); hi > metrics.Global.ArenaUsedBytes.Load() {
		metrics.Global.ArenaUsedBytes.Store(hi)
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
