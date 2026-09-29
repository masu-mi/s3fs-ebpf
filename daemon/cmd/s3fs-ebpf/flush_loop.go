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
	"github.com/bbsakura/s3fs-ebpf/daemon/internal/objstore"
	"golang.org/x/sys/unix"
)

// flushEvent mirrors `struct s3fs_flush_event`.
// Flags carry S3FS_FLUSH_FLAG_* — currently only FLUSH_WAIT (fsync
// path) which tells the coalescer to skip debounce.
type flushEvent struct {
	Ino     uint64
	Size    uint64
	NameLen uint32
	Flags   uint32
	Name    [syncNameMax + 1]byte
}

// flushFlagWait matches S3FS_FLUSH_FLAG_WAIT in the UAPI header.
const flushFlagWait uint32 = 0x1

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
	s3fsIocSignalFlush     = (1 << 30) | (uint32(unsafe.Sizeof(signalAdmitArg{})) << 16) | (0xE3 << 8) | 0x0A
)

// signalFlush notifies kmod that a flush PUT completed with `err`.
// -ENOENT (no waiter) is a normal outcome for the .release path; we
// call this after every PUT so both fire-and-forget and blocking
// fsync go through the same completion signal. See E-01 in
// docs/backlog.md and struct s3fs_flush_pending in kmod/vfs_ops.c.
func signalFlush(devFd int, ino uint64, errCode int32) error {
	arg := signalAdmitArg{Ino: ino, Err: errCode}
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL,
		uintptr(devFd), uintptr(s3fsIocSignalFlush),
		uintptr(unsafe.Pointer(&arg))); errno != 0 {
		if errno == unix.ENOENT {
			return nil // no fsync() caller waiting — fire-and-forget path
		}
		return errno
	}
	return nil
}

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
//
// E-03: events without FLUSH_FLAG_WAIT are debounced through the
// coalescer so a rapid write+close storm collapses to one PUT per
// ino per debounce window. fsync-triggered events (FLUSH_WAIT set)
// bypass coalescing — the caller is blocked on signal_flush and
// latency matters.
func runFlushLoop(ctx context.Context, devPath string, progs *fpebpf.Programs,
	s3c objstore.Backend, interval time.Duration) {
	if interval <= 0 {
		interval = 100 * time.Millisecond
	}
	f, err := os.OpenFile(devPath, os.O_RDWR, 0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[flush] open %s: %v\n", devPath, err)
		return
	}
	defer f.Close()

	coal := newCoalescer(int(f.Fd()), devPath, progs, s3c,
		200*time.Millisecond)
	defer coal.Shutdown()

	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			drainFlushQueue(ctx, int(f.Fd()), devPath, progs, s3c, coal)
		}
	}
}

func drainFlushQueue(ctx context.Context, devFd int, devPath string,
	progs *fpebpf.Programs, s3c objstore.Backend, coal *coalescer) {
	for {
		var ev flushEvent
		if err := popFlush(devFd, &ev); err != nil {
			if errors.Is(err, unix.EAGAIN) {
				return
			}
			fmt.Fprintf(os.Stderr, "[flush] pop: %v\n", err)
			return
		}
		// fsync-triggered (WAIT flag): fire straight through so the
		// blocked caller returns as soon as PUT completes. All other
		// events go through the debounce coalescer.
		if ev.Flags&flushFlagWait != 0 || coal == nil {
			// Drop any earlier debounced event for this ino so a
			// stale close-event's PUT can't fire after ours and
			// overwrite the fresh bytes.
			if coal != nil {
				coal.CancelIno(ev.Ino)
			}
			handleFlush(ctx, devFd, devPath, progs, s3c, &ev)
			continue
		}
		coal.submit(ctx, &ev)
	}
}

func handleFlush(ctx context.Context, devFd int, devPath string,
	progs *fpebpf.Programs, s3c objstore.Backend, ev *flushEvent) {
	metrics.Global.FlushEvents.Add(1)
	// Time the full flush service path (readDirty + S3 Put +
	// arena admit refresh). Defer covers every return branch so
	// the histogram reflects Put-failure retries and no-backend
	// drops, not just the success path — matches the symmetric
	// handleReadMiss timing added in 1cd4d37.
	flush_started := time.Now()
	// Track the PUT outcome so the deferred signalFlush knows what
	// to hand back to the fsync() waiter. Zero = success; negated
	// errno-shaped values flow through to fsync()'s return. Set to
	// non-zero on any error branch before returning. -ENOENT (no
	// waiter) is filtered inside signalFlush.
	putErr := int32(0)
	defer func() {
		metrics.FlushLatency.Observe(time.Since(flush_started))
		if err := signalFlush(devFd, ev.Ino, putErr); err != nil {
			fmt.Fprintf(os.Stderr,
				"[flush] ino=%d signal_flush: %v\n", ev.Ino, err)
		}
	}()
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
		putErr = -61 /* -ENODATA — matches read-miss "no backend" */
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
			putErr = -5 /* -EIO — kmod-side buffer copy failed */
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
		putErr = -5 /* -EIO — S3 PUT failed */
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
