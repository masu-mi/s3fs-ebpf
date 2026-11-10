package main

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"unsafe"

	"github.com/bbsakura/s3fs-ebpf/daemon/internal/kmodio"
	"golang.org/x/sys/unix"
)

// orderedOps is what drainOrdered needs: a pop of kmod's write-back FIFO and a
// handler per kind of event (flush, delete, rename).
type orderedOps struct {
	popWB  func(*wbEvent) error
	flush  func(*flushEvent)
	delete func(*deleteEvent)
	rename func(*renameEvent)
	stop   func() bool // optional: checked after every event (shutdown budget)
}

// orderedDrain handles kmod's write-back events (flush, delete, rename) in the
// order kmod queued them (AC-84).
//
// kmod keeps them in one FIFO (S3FS_IOC_POP_WB), so the pop order is the order
// the operations happened. Handling them independently (three queues and three
// tickers, as before v2 Phase 4) let a later event of one kind overtake an
// earlier event of another, and that corrupts S3 (specs/tla/FileLifecycle.tla):
//
//   - create b, rename b->c: the rename ran first, found no b in S3 and was
//     dropped; the flush then PUT the old name b while the user sees c.
//   - rename a->b, create a: the flush of the new a overwrote the old a before
//     the rename moved it, so the rename moved the new bytes to b and deleted a.
//
// Every event also has a kmod-wide seq, assigned under the lock that queues it
// (the WAIT-flush queue shares the counter): once an event with seq S exists,
// every WB event with a smaller seq is already in the FIFO. The drain keeps one
// popped head (it stays held between runs when its seq is past the limit).
//
// run(limit) handles only events with seq < limit. The ticker runs it with no
// limit. An fsync (WAIT) flush is handled at once by its own goroutine; it calls
// run(its seq) first, so a rename or delete queued before it is done before its
// PUT, never after.
type orderedDrain struct {
	failed  atomic.Bool   // the last step stopped on a pop error
	handled atomic.Uint64 // seq of the last event passed to the handlers
	mu      sync.Mutex
	ops     orderedOps
	head    *wbEvent
}

const noSeqLimit = ^uint64(0)

func newOrderedDrain(ops orderedOps) *orderedDrain { return &orderedDrain{ops: ops} }

// ordDrain is the daemon's drain, set by runFlushLoop; runFlushWaitLoop calls it.
var ordDrain atomic.Pointer[orderedDrain]

// orderedBefore makes every event queued before seq (an fsync's) be handled now.
func orderedBefore(seq uint64) {
	if d := ordDrain.Load(); d != nil && seq > 0 {
		d.run(seq)
	}
}

// drainOrdered is a one-shot drain with its own state (tests, shutdown).
func drainOrdered(ops orderedOps) { newOrderedDrain(ops).run(noSeqLimit) }

func (d *orderedDrain) run(limit uint64) { d.runWith(d.ops, limit) }

// runWith handles events with seq < limit, one at a time. The lock is taken per
// event, not for the whole run: an fsync that calls run(its seq) has to wait for
// the event being handled (a rename or delete does S3 work), not for everything
// the ticker's run has queued up.
func (d *orderedDrain) runWith(ops orderedOps, limit uint64) {
	for d.step(ops, limit) {
	}
}

// step handles the oldest event (if its seq < limit) and reports whether there
// may be more.
func (d *orderedDrain) step(ops orderedOps, limit uint64) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.head == nil {
		var ev wbEvent
		if err := ops.popWB(&ev); err != nil {
			// a real error stops the drain; EAGAIN is just "empty"
			failed := !errors.Is(err, unix.EAGAIN)
			if failed {
				fmt.Fprintf(os.Stderr, "[events] pop: %v\n", err)
			}
			d.failed.Store(failed)
			return false
		}
		d.head = &ev
	}
	d.failed.Store(false)
	if d.head.Seq >= limit {
		return false // nothing older than limit is left (the head waits for a later run)
	}
	ev := d.head
	d.head = nil
	switch ev.Type {
	case kmodio.WBFlush:
		fe := wbToFlush(ev)
		trackFlush(ev.Ino, ev.Seq)
		ops.flush(fe)
	case kmodio.WBRename:
		ops.rename(wbToRename(ev))
	case kmodio.WBDelete:
		ops.delete(wbToDelete(ev))
	default:
		fmt.Fprintf(os.Stderr, "[events] unknown write-back event type %d (seq %d)\n", ev.Type, ev.Seq)
	}
	d.noteHandled(ev.Seq)
	return ops.stop == nil || !ops.stop()
}

func wbToFlush(w *wbEvent) *flushEvent {
	return &flushEvent{Ino: w.Ino, ParentIno: w.ParentIno, Size: w.Size, Seq: w.Seq,
		NameLen: w.NameLen, Flags: w.Flags, Name: w.Name}
}

func wbToDelete(w *wbEvent) *deleteEvent {
	return &deleteEvent{Ino: w.Ino, ParentIno: w.ParentIno, Seq: w.Seq,
		NameLen: w.NameLen, Name: w.Name}
}

func wbToRename(w *wbEvent) *renameEvent {
	return &renameEvent{Ino: w.Ino, OldParentIno: w.ParentIno, NewParentIno: w.NewParentIno,
		Seq: w.Seq, OldNameLen: w.NameLen, NewNameLen: w.NewNameLen,
		OldName: w.Name, NewName: w.NewName}
}

// popWB pulls the oldest write-back event or returns unix.EAGAIN when the queue
// is empty. The dev fd must be R/W.
func popWB(devFd int, out *wbEvent) error {
	_, _, errno := unix.Syscall(unix.SYS_IOCTL,
		uintptr(devFd), uintptr(s3fsIocPopWB),
		uintptr(unsafe.Pointer(out)))
	if errno != 0 {
		return errno
	}
	return nil
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
// This is the conservative form that needs no device: the drain handles events
// in seq order, so all events <= the last handled one have been handed on, and
// any not yet popped have a larger seq. Renames and deletes are synchronous; a
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

// appliedSeqFor is the W a sync reports: read kmod's current event seq C, handle
// every queued event up to C (they are all visible: seq is assigned under the
// lock that queues), and report C, held back by flushes whose PUT is still
// running. Unlike appliedSeq it does not depend on how long this daemon has been
// up: after a restart the local entries and tombstones kmod kept from before
// still get promoted / dropped. Falls back to appliedSeq when the device cannot
// say, or a pop failed and events up to C may be left.
func appliedSeqFor(devPath string) uint64 {
	d := ordDrain.Load()
	if d == nil {
		return 0
	}
	c, err := kmodEventSeq(devPath)
	if err != nil {
		return appliedSeq()
	}
	d.run(c + 1)
	if d.failed.Load() {
		return appliedSeq()
	}
	return capByUnfinishedFlushes(c)
}
