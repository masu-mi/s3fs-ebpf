package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync/atomic"
	"time"
	"unsafe"

	fpebpf "github.com/bbsakura/s3fs-ebpf/daemon/internal/ebpf"
	"github.com/bbsakura/s3fs-ebpf/daemon/internal/kmodio"
	"github.com/bbsakura/s3fs-ebpf/daemon/internal/metrics"
	"github.com/bbsakura/s3fs-ebpf/daemon/internal/objstore"
	"golang.org/x/sys/unix"
)

// flushFlagWait matches S3FS_FLUSH_FLAG_WAIT in the UAPI header.
const flushFlagWait uint32 = 0x1

// doneFlush tells kmod the PUT of `ino` finished with `errCode` (S3FS_IOC_DONE,
// type FLUSH). Err != 0 also makes kmod set the file dirty again so the next
// close / fsync retries (this was a separate MARK_DIRTY ioctl). -ENOENT (no
// waiting fsync, or the inode is gone) is a normal outcome for the .release path:
// we call this after every PUT.
func doneFlush(devFd int, ino uint64, errCode int32) error {
	arg := doneArg{Type: kmodio.ReqFlush, Ino: ino, Err: errCode}
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL,
		uintptr(devFd), uintptr(s3fsIocDone),
		uintptr(unsafe.Pointer(&arg))); errno != 0 {
		if errno == unix.ENOENT {
			return nil // no fsync() caller waiting — fire-and-forget path
		}
		return errno
	}
	return nil
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

// dirtyTooSmallError is the kmod refusing READ_DIRTY with -ENOSPC (AC-71): buf is
// smaller than the inode's content, so nothing was copied and the flush was not
// claimed. Need is the size to ask for.
type dirtyTooSmallError struct{ Need uint64 }

func (e *dirtyTooSmallError) Error() string {
	return fmt.Sprintf("read dirty: buffer smaller than the content (%d bytes needed)", e.Need)
}

// dirtyReadRetries bounds how often a flush re-reads with a larger buffer
// because the file grew between the size it was queued with and the read.
const dirtyReadRetries = 3

// readDirtyAll reads the whole content of `ino`. It starts from the size the event
// carried (the size when it was queued, possibly stale) and asks again with the
// size the kmod reports until the buffer holds everything.
func readDirtyAll(k flushKmod, ino, size uint64) ([]byte, error) {
	for try := 0; ; try++ {
		buf := make([]byte, size)
		n, err := k.ReadDirty(ino, buf)
		var small *dirtyTooSmallError
		if errors.As(err, &small) && try < dirtyReadRetries {
			size = small.Need
			continue
		}
		if err != nil {
			return nil, err
		}
		return buf[:n], nil
	}
}

// readDirty copies the content of `ino` into buf and returns the actual byte
// count. A buf smaller than the content is a *dirtyTooSmallError.
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
		if errno == unix.ENOSPC {
			return 0, &dirtyTooSmallError{Need: arg.OutBytes}
		}
		return 0, errno
	}
	return int(arg.OutBytes), nil
}

// shutdownDrainBudget bounds how long the daemon keeps handling queued
// write-back requests after ctx.Done. 10s handles the "last write PUT still in
// flight" case without deadlocking the whole daemon on an unreachable S3.
var shutdownDrainBudget = 10 * time.Second

// flushQueueOps are the executor's handlers (see orderedDrain): flush events go
// through the debounce coalescer (fsync's WAIT ones straight to the PUT); a
// rename or delete first waits for every flush older than it that could conflict
// to reach S3, because it acts on the keys those PUT.
//
// While *shutdown is set (ctx is done, the daemon is draining what kmod still
// had queued) events are handled in-line: handleFlush issues the PUT (and
// signal_flush for WAIT events) itself rather than parking into the coalescer's
// debounce timer and then exiting before it fires. Count of handled events goes
// to *drained.
func flushQueueOps(ctx context.Context, devFd int, devPath string,
	s3c objstore.Backend, coal *coalescer, shutdown *atomic.Bool,
	deadline *atomic.Int64, drained *atomic.Int64) orderedOps {
	// Before a rename or delete, the parked flushes it could conflict with must
	// reach S3: those of the same inode and those under either name. The rest keep
	// their debounce window (an rm -rf would otherwise fire every pending PUT).
	barrier := func(match func(*flushEvent) bool) {
		if coal != nil {
			if shutdown.Load() {
				coal.FireAll(context.Background())
				return
			}
			coal.FireMatching(ctx, match)
		}
	}
	hctx := func() context.Context {
		if shutdown.Load() {
			return context.Background() // a canceled parent must not short-circuit the drain
		}
		return ctx
	}
	return orderedOps{
		flush: func(ev *flushEvent) {
			defer drained.Add(1)
			if shutdown.Load() {
				handleFlush(hctx(), devFd, devPath, s3c, ev)
				return
			}
			// fsync-triggered (WAIT flag): fire straight through so the
			// blocked caller returns as soon as PUT completes. All other
			// events go through the debounce coalescer.
			if ev.Flags&flushFlagWait != 0 || coal == nil {
				// Drop any earlier debounced event for this ino so a
				// stale close-event's PUT can't fire after ours and
				// overwrite the fresh bytes. (Every earlier event has been
				// handled: the executor is in kmod's order.)
				if coal != nil {
					flushOrd.noteWait(ev.Ino)
					coal.CancelIno(ev.Ino)
				}
				// The PUT runs on its own goroutine: a big one must not hold up
				// the events queued after it.
				go flushOrd.runWait(ev.Ino, func() {
					handleFlush(ctx, devFd, devPath, s3c, ev)
				})
				return
			}
			flushOrd.noteParked(ev.Ino)
			coal.Submit(ctx, ev)
		},
		delete: func(ev *deleteEvent) {
			defer drained.Add(1)
			barrier(func(f *flushEvent) bool { return flushTouchesDelete(f, ev) })
			handleDelete(hctx(), s3c, ev)
		},
		rename: func(ev *renameEvent) {
			defer drained.Add(1)
			barrier(func(f *flushEvent) bool { return flushTouchesRename(f, ev) })
			handleRename(hctx(), s3c, ev)
		},
		stop: func() bool {
			dl := deadline.Load()
			return shutdown.Load() && dl != 0 && time.Now().UnixNano() > dl
		},
	}
}

// flushKmod is what handleFlush asks of the kernel module for one event, so
// the flush path can be exercised without a device.
type flushKmod interface {
	// Done tells kmod the PUT finished: it wakes the fsync() waiter with the
	// outcome (-ENOENT, no waiter, is not an error) and, for errCode != 0,
	// restores the dirty flag so the next close/fsync retries.
	Done(ino uint64, errCode int32) error
	// ReadDirty copies the inode's dirty bytes into buf.
	ReadDirty(ino uint64, buf []byte) (int, error)
	// TryPartial flushes a file that has remote chunks with a multipart copy
	// (AC-51); handled=false means "use the full put".
	TryPartial(ctx context.Context, s3c objstore.Backend, key string, ino uint64) (handled bool, sent, size int64, err error)
}

// devKmod is flushKmod over the /dev/s3fs_ebpf fd.
type devKmod struct{ fd int }

func (d devKmod) Done(ino uint64, errCode int32) error { return doneFlush(d.fd, ino, errCode) }
func (d devKmod) ReadDirty(ino uint64, buf []byte) (int, error) {
	return readDirty(d.fd, ino, buf)
}
func (d devKmod) TryPartial(ctx context.Context, s3c objstore.Backend, key string, ino uint64) (bool, int64, int64, error) {
	return tryFlushPartial(ctx, d.fd, s3c, key, ino)
}

func handleFlush(ctx context.Context, devFd int, devPath string,
	s3c objstore.Backend, ev *flushEvent) {
	handleFlushWith(ctx, devKmod{devFd}, devPath, s3c, ev)
}

func handleFlushWith(ctx context.Context, k flushKmod, devPath string,
	s3c objstore.Backend, ev *flushEvent) {
	metrics.Global.FlushEvents.Add(1)
	defer untrackFlush(ev.Ino, ev.Seq) // AC-85: this PUT (and every older event of the ino it covers) is done
	// Time the full flush service path (readDirty + S3 Put). Defer covers every return branch so
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
		if err := k.Done(ev.Ino, putErr); err != nil {
			fmt.Fprintf(os.Stderr,
				"[flush] ino=%d done: %v\n", ev.Ino, err)
		}
	}()
	name := string(ev.Name[:ev.NameLen])

	s3key := resolveEventS3Key(ev.Ino, ev.ParentIno, name)
	// Object staging: no staged copy of this key may outlive the PUT (before and
	// after it, ObjStaging.tla); the deferred call runs before the waiter is woken.
	stagingInvalidate(s3key)
	defer stagingInvalidate(s3key)
	// AC-86: the mirror learns a created file (or its new name) only at the next
	// sync; read misses and open_check look the key up there.
	fpebpf.RecordKey(ev.Ino, ev.ParentIno, name, s3key, ev.Size, ev.Seq)

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
		handled, sent, size, perr := k.TryPartial(pctx, s3c, s3key, ev.Ino)
		pcancel()
		if handled {
			if perr != nil {
				fmt.Fprintf(os.Stderr, "[flush] ino=%d s3://%s partial put: %v\n", ev.Ino, s3key, perr)
				metrics.Global.FlushErrors.Add(1)
				putErr = -5 /* -EIO */
				return
			}
			registerIno(s3key, ev.Ino)
			metrics.Global.FlushBytes.Add(uint64(sent))
			fmt.Printf("[flush] ino=%d s3://%s put %d bytes (%d sent, rest copied)\n", ev.Ino, s3key, size, sent)
			fpebpf.SetSizeByIno(ev.Ino, uint64(size))
			return
		}
	}

	// READ_DIRTY runs for an empty file too: it is what tells kmod this PUT
	// covers the flush (W1/W3).  Skipping it for size 0 left the fd
	// flushing and pinned, and an fsync waiting for its own PUT timed out.
	buf, err := readDirtyAll(k, ev.Ino, ev.Size)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[flush] ino=%d read dirty: %v\n",
			ev.Ino, err)
		metrics.Global.FlushErrors.Add(1)
		// DONE(err) restores the dirty flag so the next close/fsync will re-try.
		putErr = -5 /* -EIO — kmod-side buffer copy failed */
		return
	}
	putCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := s3c.Put(putCtx, s3key, buf); err != nil {
		fmt.Fprintf(os.Stderr, "[flush] ino=%d s3://%s put: %v\n",
			ev.Ino, s3key, err)
		metrics.Global.FlushErrors.Add(1)
		putErr = -5 /* -EIO — S3 PUT failed */
		return
	}
	registerIno(s3key, ev.Ino)
	objETags.Delete(ev.Ino) // the modify cycle is over
	metrics.Global.FlushBytes.Add(uint64(len(buf)))
	fmt.Printf("[flush] ino=%d s3://%s put %d bytes\n",
		ev.Ino, s3key, len(buf))
	// The entry mirror keeps the pre-write size until the next sync; a read
	// that misses after the file was evicted (shrunk, appended, ...) must use
	// the size just written (the partial path above does the same).
	fpebpf.SetSizeByIno(ev.Ino, uint64(len(buf)))

	// No re-admit here: after a successful signal_flush the kmod keeps the
	// chunks it just PUT (clean, populated) and admits them itself, so a read
	// right after the write is served from the fd (AC-82).
}

// flushTouchesDelete / flushTouchesRename: could the flush event f and the
// delete / rename event conflict on an S3 key? Same inode, or the flush is for
// one of the names the event acts on (a re-created name is a different inode:
// rename a->b, create a).
func flushTouchesDelete(f *flushEvent, d *deleteEvent) bool {
	return f.Ino == d.Ino || (f.ParentIno == d.ParentIno &&
		string(f.Name[:f.NameLen]) == string(d.Name[:d.NameLen]))
}

func flushTouchesRename(f *flushEvent, r *renameEvent) bool {
	if f.Ino == r.Ino {
		return true
	}
	name := string(f.Name[:f.NameLen])
	return (f.ParentIno == r.OldParentIno && name == string(r.OldName[:r.OldNameLen])) ||
		(f.ParentIno == r.NewParentIno && name == string(r.NewName[:r.NewNameLen]))
}
