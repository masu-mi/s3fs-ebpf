package main

import (
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bbsakura/s3fs-ebpf/daemon/internal/kmodio"
)

// orderedOps are the handlers of the write-back executor: one per kind of
// event (flush, delete, rename).
type orderedOps struct {
	flush  func(*flushEvent)
	delete func(*deleteEvent)
	rename func(*renameEvent)
	stop   func() bool // optional: checked after every event (shutdown budget)
}

// orderedDrain is the daemon's write-back executor: it handles kmod's flush,
// delete and rename requests one at a time, in the order kmod queued them
// (AC-84).
//
// kmod keeps every request in one FIFO (reqq) that the dispatcher reads in
// order and pushes here. Handling the kinds independently (before v2 Phase 4:
// five queues, three tickers) let a later event of one kind overtake an earlier
// event of another, and that corrupts S3 (specs/tla/FileLifecycle.tla):
//
//   - create b, rename b->c: the rename ran first, found no b in S3 and was
//     dropped; the flush then PUT the old name b while the user sees c.
//   - rename a->b, create a: the flush of the new a overwrote the old a before
//     the rename moved it, so the rename moved the new bytes to b and deleted a.
//
// An fsync's WAIT flush is in the same FIFO: when the executor reaches it,
// every event queued before it has been handled, so a rename or delete queued
// before the fsync is done before its PUT, never after. (The handler runs the
// PUT on its own goroutine so a long PUT does not hold up later events.)
//
// `seq` is kmod's event number; handled is the highest one passed to a handler
// (what a sync reports as applied, AC-85).
type orderedDrain struct {
	handled atomic.Uint64 // seq of the last event passed to a handler
	ops     orderedOps

	mu     sync.Mutex
	cond   *sync.Cond
	q      []*reqEvent
	closed bool
}

func newOrderedDrain(ops orderedOps) *orderedDrain {
	d := &orderedDrain{ops: ops}
	d.cond = sync.NewCond(&d.mu)
	return d
}

// ordDrain is the daemon's executor, set by runReqLoop; appliedSeq reads it.
var ordDrain atomic.Pointer[orderedDrain]

// push queues a request (flush / delete / rename), in the order read from kmod.
func (d *orderedDrain) push(ev *reqEvent) {
	d.mu.Lock()
	d.q = append(d.q, ev)
	d.mu.Unlock()
	d.cond.Signal()
}

// close tells run to return once the queue is empty.
func (d *orderedDrain) close() {
	d.mu.Lock()
	d.closed = true
	d.mu.Unlock()
	d.cond.Broadcast()
}

// run handles requests until close (and the queue is empty) or ops.stop says
// stop (what is left stays in the queue).
func (d *orderedDrain) run() {
	for {
		d.mu.Lock()
		for len(d.q) == 0 && !d.closed {
			d.cond.Wait()
		}
		if len(d.q) == 0 {
			d.mu.Unlock()
			return
		}
		ev := d.q[0]
		d.q = d.q[1:]
		d.mu.Unlock()
		d.handle(ev)
		if d.ops.stop != nil && d.ops.stop() {
			return
		}
	}
}

// reqBarrier is a daemon-local request type (not kmod's): when the executor
// reaches it, every request queued before it has been handled, so seq is applied.
const reqBarrier = 0xFFFF

func (d *orderedDrain) handle(ev *reqEvent) {
	switch ev.Type {
	case reqBarrier:
	case kmodio.ReqFlush:
		fe := reqToFlush(ev)
		trackFlush(ev.Ino, ev.Seq)
		d.ops.flush(fe)
	case kmodio.ReqRename:
		d.ops.rename(reqToRename(ev))
	case kmodio.ReqDelete:
		d.ops.delete(reqToDelete(ev))
	default:
		fmt.Fprintf(os.Stderr, "[events] unknown request type %d (seq %d)\n", ev.Type, ev.Seq)
	}
	d.noteHandled(ev.Seq)
}

// pending is the number of requests queued and not yet handled.
func (d *orderedDrain) pending() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.q)
}

// waitHandled blocks until an event with seq >= want has been handled, or the
// timeout passes. It reports whether it was reached.
func (d *orderedDrain) waitHandled(want uint64, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for d.handled.Load() < want {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(2 * time.Millisecond)
	}
	return true
}

func reqToFlush(w *reqEvent) *flushEvent {
	return &flushEvent{Ino: w.Ino, ParentIno: w.ParentIno, Size: w.Size, Seq: w.Seq,
		NameLen: w.NameLen, Flags: w.Flags, Name: w.Name}
}

func reqToDelete(w *reqEvent) *deleteEvent {
	return &deleteEvent{Ino: w.Ino, ParentIno: w.ParentIno, Seq: w.Seq,
		NameLen: w.NameLen, Name: w.Name}
}

func reqToRename(w *reqEvent) *renameEvent {
	return &renameEvent{Ino: w.Ino, OldParentIno: w.ParentIno, NewParentIno: w.NewParentIno,
		Seq: w.Seq, OldNameLen: w.NameLen, NewNameLen: w.NewNameLen,
		OldName: w.Name, NewName: w.NewName}
}

func (d *orderedDrain) noteHandled(seq uint64) {
	for {
		old := d.handled.Load()
		if seq <= old || d.handled.CompareAndSwap(old, seq) {
			return
		}
	}
}

// flushInFlight: ino -> the seqs of flush events that were handed on (parked in
// the coalescer, or being PUT) and whose PUT has not finished. A parked flush is
// not applied to S3 yet, although the drain has "handled" it.
var flushInFlight struct {
	mu sync.Mutex
	m  map[uint64][]uint64
}

func trackFlush(ino, seq uint64) {
	flushInFlight.mu.Lock()
	defer flushInFlight.mu.Unlock()
	if flushInFlight.m == nil {
		flushInFlight.m = map[uint64][]uint64{}
	}
	flushInFlight.m[ino] = append(flushInFlight.m[ino], seq)
}

// untrackFlush is called when handleFlush for ino returns, having PUT the event
// with seq `upto`. Every event of the ino up to that seq is covered by that PUT
// (the coalescer keeps only the latest event; an fsync's PUT covers the older
// ones). A newer event, parked while this PUT was running, stays tracked.
func untrackFlush(ino, upto uint64) {
	flushInFlight.mu.Lock()
	defer flushInFlight.mu.Unlock()
	rest := flushInFlight.m[ino][:0]
	for _, s := range flushInFlight.m[ino] {
		if s > upto {
			rest = append(rest, s)
		}
	}
	if len(rest) == 0 {
		delete(flushInFlight.m, ino)
		return
	}
	flushInFlight.m[ino] = rest
}

// appliedSeq returns W: every flush / rename / delete event with seq <= W has
// been applied to S3 (AC-85). kmod's sync uses it to decide when a tombstone or
// a local entry may go (docs/design/kmod-entry-table-state-machine.md).
//
// This is the conservative form that needs no device: the executor handles
// events in seq order, so all events <= the last handled one have been handed on, and
// any not yet read have a larger seq. Renames and deletes are synchronous; a
// flush only counts once its PUT has finished. It starts at 0 in a new daemon
// (kmod's counter keeps counting); appliedSeqFor is what a sync uses.
func appliedSeq() uint64 {
	d := ordDrain.Load()
	if d == nil {
		return 0
	}
	return capByUnfinishedFlushes(d.handled.Load())
}

func capByUnfinishedFlushes(w uint64) uint64 {
	flushInFlight.mu.Lock()
	defer flushInFlight.mu.Unlock()
	for _, seqs := range flushInFlight.m {
		for _, s := range seqs {
			if s > 0 && s-1 < w {
				w = s - 1
			}
		}
	}
	return w
}

// kmodEventSeq reads the highest event seq kmod has handed out (S3FS_IOC_STATS
// slot 15).
func kmodEventSeq(devPath string) (uint64, error) {
	st, err := readStats(devPath)
	if err != nil {
		return 0, err
	}
	if len(st) <= 15 {
		return 0, fmt.Errorf("kmod reports %d stats slots, event seq is slot 15", len(st))
	}
	return st[15], nil
}

// appliedSeqFor is the W a sync reports: read kmod's current event seq C, wait
// until the executor has handled every request up to C (the dispatcher reads them
// in order, so they are on their way), and report C, held back by flushes whose
// PUT is still running. Unlike appliedSeq it does not depend on how long this
// daemon has been up: after a restart the local entries and tombstones kmod kept
// from before still get promoted / dropped. Falls back to appliedSeq when the
// device cannot say, or the executor does not catch up in time (a slow S3: the
// conservative value only delays a promotion).
func appliedSeqFor(devPath string) uint64 {
	d := ordDrain.Load()
	if d == nil {
		return 0
	}
	c, err := kmodEventSeq(devPath)
	if err != nil {
		return appliedSeq()
	}
	if !d.waitHandled(c, appliedWaitBudget) {
		return appliedSeq()
	}
	return capByUnfinishedFlushes(c)
}

// appliedWaitBudget bounds how long a sync waits for the executor to reach kmod's
// current event seq.
var appliedWaitBudget = 5 * time.Second
