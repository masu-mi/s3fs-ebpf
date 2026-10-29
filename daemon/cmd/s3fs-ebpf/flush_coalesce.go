package main

import (
	"context"
	"sync"
	"time"

	fpebpf "github.com/bbsakura/s3fs-ebpf/daemon/internal/ebpf"
	"github.com/bbsakura/s3fs-ebpf/daemon/internal/metrics"
	"github.com/bbsakura/s3fs-ebpf/daemon/internal/objstore"
)

// coalescer batches close/release-triggered flush events per ino so
// a burst of open+write+close cycles turns into one S3 PUT instead
// of N. Each event resets a per-ino debounce timer; once `window`
// passes without a new event on that ino, the latest snapshot is
// PUT.
//
// fsync-triggered events (FLUSH_WAIT flag) bypass this and go
// straight to handleFlush — see drainFlushQueue in flush_loop.go.
//
// Ordering guarantee: the latest event's byte range wins. Users who
// need per-close durability should use fsync().
type coalescer struct {
	devFd   int
	devPath string
	progs   *fpebpf.Programs
	s3c     objstore.Backend
	window  time.Duration

	mu      sync.Mutex
	pending map[uint64]*pendingCoalesce

	// shutdown drains all pending timers so ctx-cancel flushes
	// everything at exit instead of leaking dirty state.
	done chan struct{}
	wg   sync.WaitGroup
}

type pendingCoalesce struct {
	ev    flushEvent
	timer *time.Timer
}

func newCoalescer(devFd int, devPath string,
	progs *fpebpf.Programs, s3c objstore.Backend, window time.Duration) *coalescer {
	return &coalescer{
		devFd:   devFd,
		devPath: devPath,
		progs:   progs,
		s3c:     s3c,
		window:  window,
		pending: make(map[uint64]*pendingCoalesce),
		done:    make(chan struct{}),
	}
}

// submit either creates a new debounce entry for `ev.Ino` or
// replaces the pending event with `ev` and resets the timer. The
// latest event wins because it reflects the freshest kmod-side
// buffer state — earlier flushes are dominated (fd->size only
// grows within a session; a shrink means user truncated + closed +
// wrote again, which is genuinely a new state to persist).
func (c *coalescer) submit(ctx context.Context, ev *flushEvent) {
	metrics.Global.FlushCoalesced.Add(1)
	c.mu.Lock()
	if p, ok := c.pending[ev.Ino]; ok {
		p.ev = *ev
		// Reset the timer to give the burst another debounce
		// window. Multiple closes in <window will keep pushing
		// this out until quiescence.
		p.timer.Reset(c.window)
		c.mu.Unlock()
		return
	}
	p := &pendingCoalesce{ev: *ev}
	c.pending[ev.Ino] = p
	p.timer = time.AfterFunc(c.window, func() {
		c.fire(ctx, ev.Ino)
	})
	c.mu.Unlock()
}

// fire is the debounce timer's callback — dequeue the ino's
// pending event and hand it to handleFlush. Called on the AfterFunc
// goroutine, so we track wg for orderly shutdown.
func (c *coalescer) fire(ctx context.Context, ino uint64) {
	c.wg.Add(1)
	defer c.wg.Done()

	c.mu.Lock()
	p, ok := c.pending[ino]
	if !ok {
		c.mu.Unlock()
		return
	}
	delete(c.pending, ino)
	ev := p.ev
	c.mu.Unlock()

	metrics.Global.FlushCoalescedFired.Add(1)
	handleFlush(ctx, c.devFd, c.devPath, c.progs, c.s3c, &ev)
}

// CancelIno drops any pending debounced event for `ino` without
// firing it. Called before an fsync-triggered (WAIT) PUT so a
// stale earlier close-event doesn't overwrite fresh fsync bytes
// once the debounce timer eventually fires.
func (c *coalescer) CancelIno(ino uint64) {
	c.mu.Lock()
	p, ok := c.pending[ino]
	if !ok {
		c.mu.Unlock()
		return
	}
	p.timer.Stop()
	delete(c.pending, ino)
	c.mu.Unlock()
}

// Shutdown drains any deferred timers by firing them immediately.
// Called from runFlushLoop's defer so ctx-cancel flushes rather
// than dropping unwritten data.
func (c *coalescer) Shutdown() {
	c.mu.Lock()
	victims := make([]*pendingCoalesce, 0, len(c.pending))
	for _, p := range c.pending {
		p.timer.Stop()
		victims = append(victims, p)
	}
	c.pending = map[uint64]*pendingCoalesce{}
	c.mu.Unlock()
	// Fire outside the lock (handleFlush may take time).
	ctx := context.Background()
	for _, p := range victims {
		handleFlush(ctx, c.devFd, c.devPath, c.progs, c.s3c, &p.ev)
	}
	// Let any in-flight timer callbacks finish.
	c.wg.Wait()
	close(c.done)
}
