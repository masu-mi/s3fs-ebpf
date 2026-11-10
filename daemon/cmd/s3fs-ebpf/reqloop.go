package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/bbsakura/s3fs-ebpf/daemon/internal/kmodio"
	"github.com/bbsakura/s3fs-ebpf/daemon/internal/objstore"
	"golang.org/x/sys/unix"
)

// runReqLoop is the daemon's one reader of kmod's request queue (reqq): it
// read(2)s /dev/s3fs_ebpf, one struct s3fs_req_event per read, in the order kmod
// queued them, and dispatches by type (v2 Phase 4):
//
//   - MISS (a reader is waiting): a worker goroutine fetches the chunk from S3
//     and populates it. V-06: bounded by a semaphore of readMissWorkers slots; a
//     full semaphore back-pressures the read, kmod's queue keeps the rest.
//   - FLUSH / DELETE / RENAME: pushed to the write-back executor (orderedDrain),
//     which handles them one at a time in this same order (AC-84). They are
//     never held up by S3 work: the push does not block.
//
// Before v2 Phase 4 these came through five queues, a blocking ioctl for fsync
// and 100 ms tickers for the rest.
//
// On ctx.Done the read loop stops, the requests kmod still holds are read and
// handled in-line (bounded by shutdownDrainBudget) so a graceful stop durably
// PUTs the last write, and the debounce layer is flushed.
//
// s3c == nil disables S3 work but still drains the queue so kmod does not grow
// unbounded when someone runs without --s3.
func runReqLoop(ctx context.Context, devPath string, s3c objstore.Backend) {
	// rd is only read from: Go's poller wakes a blocked Read when it is closed
	// (calling Fd() would make it blocking). ctl carries the ioctls.
	rd, err := os.OpenFile(devPath, os.O_RDWR, 0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[upcall] open %s: %v\n", devPath, err)
		return
	}
	defer rd.Close()
	ctl, err := os.OpenFile(devPath, os.O_RDWR, 0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[upcall] open %s: %v\n", devPath, err)
		return
	}
	defer ctl.Close()
	ctlFd := int(ctl.Fd())

	coal := newCoalescer(ctlFd, devPath, s3c, 200*time.Millisecond)
	setFlushCoalescer(coal)
	var shutdown atomic.Bool
	var deadline, drained atomic.Int64
	exec := newOrderedDrain(flushQueueOps(ctx, ctlFd, devPath, s3c, coal, &shutdown, &deadline, &drained))
	ordDrain.Store(exec)
	execDone := make(chan struct{})
	go func() { defer close(execDone); exec.run() }()
	defer func() { ordDrain.Store(nil); setFlushCoalescer(nil); coal.Shutdown() }()

	// Cancel-triggered close so the blocked kmod .read wakes with EOF. The 5s
	// deadline is an H-05 observability net: on any modern kernel the poller
	// wakes the Read, but a stuck kmod could leave it hanging forever. Emit an
	// audible warning so bench harnesses and operators can tell whether shutdown
	// actually completed.
	loopExit := make(chan struct{})
	go func() {
		<-ctx.Done()
		rd.Close()
		select {
		case <-loopExit:
		case <-time.After(readMissShutdownWarn):
			fmt.Fprintf(os.Stderr,
				"[upcall] SHUTDOWN WARN: read loop still blocked %s after close — check kmod .release path\n",
				readMissShutdownWarn)
		}
	}()

	sem := make(chan struct{}, readMissWorkers)
	var ev reqEvent
	buf := unsafe.Slice((*byte)(unsafe.Pointer(&ev)), unsafe.Sizeof(ev))
	for {
		n, err := rd.Read(buf)
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, os.ErrClosed) {
				fmt.Fprintf(os.Stderr, "[upcall] read: %v\n", err)
			}
			break
		}
		if n != len(buf) {
			fmt.Fprintf(os.Stderr, "[upcall] short read: %d bytes\n", n)
			continue
		}
		dispatchReq(ctx, devPath, s3c, exec, sem, &ev)
	}
	close(loopExit)

	// Shutdown: what kmod still has queued (flushes enqueued by .release /
	// .fsync before the daemon was signaled) is read now and handled in-line.
	shutdown.Store(true)
	deadline.Store(time.Now().Add(shutdownDrainBudget).UnixNano())
	drainReqOnShutdown(devPath, exec)
	exec.close()
	<-execDone
	if left := exec.pending(); left > 0 {
		fmt.Fprintf(os.Stderr,
			"[flush] shutdown drain hit %s budget with %d event(s) remaining — some writes may not be durable\n",
			shutdownDrainBudget, left)
	} else if n := drained.Load(); n > 0 {
		fmt.Fprintf(os.Stderr, "[flush] shutdown drained %d event(s) from kmod queues\n", n)
	}
}

// dispatchReq hands one request to its handler. It is called in kmod's order and
// does not block, except for a MISS when the workers are all busy.
func dispatchReq(ctx context.Context, devPath string, s3c objstore.Backend,
	exec *orderedDrain, sem chan struct{}, r *reqEvent) {
	switch r.Type {
	case kmodio.ReqMiss:
		ev := readMissFromReq(r)
		fmt.Printf("[upcall] miss ino=%d offset=%d count=%d\n", ev.Ino, ev.Offset, ev.Count)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem }()
			handleReadMiss(ctx, devPath, s3c, ev)
		}()
	case kmodio.ReqFlush, kmodio.ReqDelete, kmodio.ReqRename:
		cp := *r
		exec.push(&cp)
	default:
		fmt.Fprintf(os.Stderr, "[upcall] unknown request type %d\n", r.Type)
	}
}

// drainReqOnShutdown reads whatever requests kmod still holds (non-blocking) and
// queues the write-back ones for the executor. Misses are dropped: their readers
// are being torn down with the mount.
func drainReqOnShutdown(devPath string, exec *orderedDrain) {
	fd, err := unix.Open(devPath, unix.O_RDWR|unix.O_NONBLOCK, 0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[flush] shutdown drain: open %s: %v\n", devPath, err)
		return
	}
	defer unix.Close(fd)
	var ev reqEvent
	buf := unsafe.Slice((*byte)(unsafe.Pointer(&ev)), unsafe.Sizeof(ev))
	for {
		n, err := unix.Read(fd, buf)
		if err != nil {
			if !errors.Is(err, unix.EAGAIN) {
				fmt.Fprintf(os.Stderr, "[flush] shutdown drain: read: %v\n", err)
			}
			return
		}
		if n != len(buf) {
			return
		}
		switch ev.Type {
		case kmodio.ReqFlush, kmodio.ReqDelete, kmodio.ReqRename:
			cp := ev
			exec.push(&cp)
		}
	}
}
