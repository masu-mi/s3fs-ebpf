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

// flushFlagWait matches S3FS_FLUSH_FLAG_WAIT in the UAPI header.
const flushFlagWait uint32 = 0x1

// signalFlush notifies kmod that a flush PUT completed with `err`.
// -ENOENT (no waiter) is a normal outcome for the .release path; we
// call this after every PUT so both fire-and-forget and blocking
// fsync go through the same completion signal. See E-01 in
// docs/journal/backlog.md and struct s3fs_flush_pending in kmod/vfs_ops.c.
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

// drainEvictionsOn issues S3FS_IOC_DRAIN_EVICTIONS on a caller-provided
// fd. Use this in loops where the fd is opened once and reused (T-03).
func drainEvictionsOn(devFd int) ([]uint64, uint32, error) {
	arg := drainEvictionsArg{Cap: evictLogMax}
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL,
		uintptr(devFd), uintptr(s3fsIocDrainEvictions),
		uintptr(unsafe.Pointer(&arg))); errno != 0 {
		return nil, 0, fmt.Errorf("ioctl DRAIN_EVICTIONS: %w", errno)
	}
	out := make([]uint64, arg.Filled)
	copy(out, arg.Inos[:arg.Filled])
	return out, arg.Overflow, nil
}

// drainEvictions returns the ino list kmod evicted since the last
// drain. Callers use this to prune their local admit snapshot so the
// next admit_replace doesn't re-send evicted inos. For callers that
// already hold an open fd use drainEvictionsOn instead.
func drainEvictions(devPath string) ([]uint64, uint32, error) {
	f, err := os.OpenFile(devPath, os.O_RDWR, 0)
	if err != nil {
		return nil, 0, fmt.Errorf("open %s: %w", devPath, err)
	}
	defer f.Close()
	return drainEvictionsOn(int(f.Fd()))
}

// readStatsOn issues S3FS_IOC_STATS on a caller-provided fd.
// Slot 0 is s3fs_evictions_total; slot layout is UAPI-defined additive-only.
// Use this in loops where the fd is opened once and reused (T-03).
func readStatsOn(devFd int) ([]uint64, error) {
	arg := statsArg{Count: statsMax}
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL,
		uintptr(devFd), uintptr(s3fsIocStats),
		uintptr(unsafe.Pointer(&arg))); errno != 0 {
		return nil, fmt.Errorf("ioctl STATS: %w", errno)
	}
	out := make([]uint64, arg.Filled)
	copy(out, arg.Values[:arg.Filled])
	return out, nil
}

// readStats issues S3FS_IOC_STATS and returns the value slots. For
// callers that already hold an open fd use readStatsOn instead.
func readStats(devPath string) ([]uint64, error) {
	f, err := os.OpenFile(devPath, os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", devPath, err)
	}
	defer f.Close()
	return readStatsOn(int(f.Fd()))
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

// popFlushWait blocks up to 200 ms for a WAIT-flagged flush event.
// Returns nil on success, unix.EAGAIN on timeout, unix.EINTR on signal,
// unix.ENODEV on kmod shutdown. The dev fd must be R/W.
func popFlushWait(devFd int, out *flushEvent) error {
	_, _, errno := unix.Syscall(unix.SYS_IOCTL,
		uintptr(devFd), uintptr(s3fsIocPopFlushWait),
		uintptr(unsafe.Pointer(out)))
	if errno != 0 {
		return errno
	}
	return nil
}

// runFlushWaitLoop handles WAIT-flagged (fsync path) flush events by
// blocking on S3FS_IOC_POP_FLUSH_WAIT instead of the 100 ms ticker.
// This eliminates the up-to-100 ms queuing latency for fsync callers
// (S-08). Exits cleanly on ctx cancellation or kmod shutdown.
func runFlushWaitLoop(ctx context.Context, devPath string,
	progs *fpebpf.Programs, s3c objstore.Backend) {
	f, err := os.OpenFile(devPath, os.O_RDWR, 0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[flush-wait] open %s: %v\n", devPath, err)
		return
	}
	defer f.Close()

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		var ev flushEvent
		if err := popFlushWait(int(f.Fd()), &ev); err != nil {
			if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EINTR) {
				continue
			}
			if errors.Is(err, unix.ENODEV) {
				return
			}
			fmt.Fprintf(os.Stderr, "[flush-wait] pop_wait: %v\n", err)
			return
		}
		handleFlush(ctx, int(f.Fd()), devPath, progs, s3c, &ev)
	}
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
			// H-01 drain-on-shutdown: any flush events already
			// enqueued by kmod .release / .fsync (before the daemon
			// was signaled) are still there. Drain them so a graceful
			// SIGINT durably PUTs the last write instead of dropping
			// it on the floor. Bounded so a stuck S3 doesn't hang
			// shutdown forever.
			drainFlushOnShutdown(int(f.Fd()), devPath, progs, s3c,
				shutdownDrainBudget)
			return
		case <-t.C:
			drainFlushQueue(ctx, int(f.Fd()), devPath, progs, s3c, coal)
		}
	}
}

// shutdownDrainBudget bounds how long runFlushLoop keeps draining
// after ctx.Done. 10s handles the "last write PUT still in flight"
// case without deadlocking the whole daemon on an unreachable S3.
var shutdownDrainBudget = 10 * time.Second

// drainFlushOnShutdown reads any events still on the kmod flush
// queue and services them synchronously, bounded by `budget`. Uses a
// background ctx so a canceled parent doesn't short-circuit the PUTs
// we are actively trying to complete. coal.Shutdown() (deferred by
// the caller) drains the debounce layer after this returns.
func drainFlushOnShutdown(devFd int, devPath string, progs *fpebpf.Programs,
	s3c objstore.Backend, budget time.Duration) {
	deadline := time.Now().Add(budget)
	ctx := context.Background()
	drained := 0
	for {
		var ev flushEvent
		if err := popFlush(devFd, &ev); err != nil {
			if errors.Is(err, unix.EAGAIN) {
				break // queue empty
			}
			fmt.Fprintf(os.Stderr, "[flush] shutdown pop: %v\n", err)
			return
		}
		// Drive events synchronously on the shutdown path so we
		// don't just re-enqueue into the coalescer's debounce timer
		// and then exit before it fires. handleFlush issues the PUT
		// (and signal_flush for WAIT events) in-line.
		handleFlush(ctx, devFd, devPath, progs, s3c, &ev)
		drained++
		if time.Now().After(deadline) {
			fmt.Fprintf(os.Stderr,
				"[flush] shutdown drain hit %s budget with events remaining — some writes may not be durable\n",
				budget)
			return
		}
	}
	if drained > 0 {
		fmt.Fprintf(os.Stderr, "[flush] shutdown drained %d event(s) from kmod queue\n", drained)
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
	// handleReadMiss timing added in 32eea25.
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

	s3key := resolveEventS3Key(ev.Ino, ev.ParentIno, name)

	if s3c == nil {
		fmt.Printf("[flush] drop ino=%d name=%s (no S3 backend)\n",
			ev.Ino, name)
		putErr = -61 /* -ENODATA — matches read-miss "no backend" */
		return
	}

	// AC-51: unchanged chunks of a large file are copied server-side.
	{
		// AC-65: fill and upload share this deadline (they overlap now); a
		// large object gets 1 s more per 8 MiB.
		pctx, pcancel := context.WithTimeout(ctx, 120*time.Second+time.Duration(ev.Size>>23)*time.Second)
		handled, sent, size, perr := tryFlushPartial(pctx, devFd, s3c, s3key, ev.Ino)
		pcancel()
		if handled {
			if perr != nil {
				fmt.Fprintf(os.Stderr, "[flush] ino=%d s3://%s partial put: %v\n", ev.Ino, s3key, perr)
				metrics.Global.FlushErrors.Add(1)
				if mderr := markDirty(devFd, ev.Ino); mderr != nil {
					fmt.Fprintf(os.Stderr, "[flush] ino=%d mark dirty: %v\n", ev.Ino, mderr)
				}
				putErr = -5 /* -EIO */
				return
			}
			registerIno(s3key, ev.Ino)
			metrics.Global.FlushBytes.Add(uint64(sent))
			fmt.Printf("[flush] ino=%d s3://%s put %d bytes (%d sent, rest copied)\n", ev.Ino, s3key, size, sent)
			// An arena admit of the old content would keep serving it.
			dropAdmitIno(devPath, ev.Ino, progs)
			fpebpf.SetSizeByIno(ev.Ino, uint64(size))
			return
		}
	}

	// READ_DIRTY runs for an empty file too: it is what tells kmod this PUT
	// covers the flush (W1/W3).  Skipping it for size 0 left the fd
	// flushing and pinned, and an fsync waiting for its own PUT timed out.
	buf := make([]byte, ev.Size)
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
	objETags.Delete(ev.Ino) // the modify cycle is over
	metrics.Global.FlushBytes.Add(uint64(len(buf)))
	fmt.Printf("[flush] ino=%d s3://%s put %d bytes\n",
		ev.Ino, s3key, len(buf))

	// Refresh admit so subsequent .read_folio serves the freshly
	// written bytes instead of the stale prewarm content.
	// replaceAdmit drops the prior admit for this ino (returning
	// its arena bytes to the D-02 free-list) before pushing, so a
	// PUT-heavy workload doesn't leak arena or bloat kmod's admit
	// hash with dead duplicates.
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
	updateArenaHighWater(off, len(buf))
	if err := replaceAdmit(devPath, admitEntry{
		Ino:   ev.Ino,
		Uaddr: progs.Arena().BaseAddr() + off,
		Size:  uint64(len(buf)),
	}, progs); err != nil {
		fmt.Fprintf(os.Stderr, "[flush] ino=%d admit arena: %v\n",
			ev.Ino, err)
	}
}
