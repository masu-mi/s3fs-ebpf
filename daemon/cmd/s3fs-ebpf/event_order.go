package main

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"

	"golang.org/x/sys/unix"
)

// orderedOps is what drainOrdered needs: a pop and a handler per kind of kmod
// write-back event (flush, delete, rename).
type orderedOps struct {
	popFlush  func(*flushEvent) error
	popDelete func(*deleteEvent) error
	popRename func(*renameEvent) error
	flush     func(*flushEvent)
	delete    func(*deleteEvent)
	rename    func(*renameEvent)
	stop      func() bool // optional: checked after every event (shutdown budget)
}

// orderedDrain handles kmod's flush, delete and rename events in the order kmod
// queued them (AC-84).
//
// kmod keeps three queues, but gives every event one strictly increasing seq,
// assigned under the lock that queues it. Handling the queues independently
// (three tickers, as before) lets a later event of one kind overtake an earlier
// event of another, and that corrupts S3 (specs/tla/FileLifecycle.tla):
//
//   - create b, rename b->c: the rename ran first, found no b in S3 and was
//     dropped; the flush then PUT the old name b while the user sees c.
//   - rename a->b, create a: the flush of the new a overwrote the old a before
//     the rename moved it, so the rename moved the new bytes to b and deleted a.
//
// Merging by seq is safe because of the lock: once an event with seq S has been
// popped, every event with a smaller seq is already queued. So after the last
// pop, an empty queue holds nothing older than any held event. The drain keeps
// one popped head per queue (they stay held between runs), polls the empty
// slots until a whole pass finds nothing new, then handles the smallest seq.
//
// run(limit) handles only events with seq < limit. The ticker runs it with no
// limit. An fsync (WAIT) flush is handled at once by its own goroutine; it calls
// run(its seq) first, so a rename or delete queued before it is done before its
// PUT, never after.
type orderedDrain struct {
	mu  sync.Mutex
	ops orderedOps
	hf  *flushEvent
	hd  *deleteEvent
	hr  *renameEvent
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

func (d *orderedDrain) runWith(ops orderedOps, limit uint64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	failed := false
	pop := func(err error) { // a real error stops the drain; EAGAIN is just "empty"
		if !errors.Is(err, unix.EAGAIN) {
			fmt.Fprintf(os.Stderr, "[events] pop: %v\n", err)
			failed = true
		}
	}
	fill := func() bool {
		got := false
		if d.hf == nil {
			var ev flushEvent
			if err := ops.popFlush(&ev); err == nil {
				d.hf, got = &ev, true
			} else {
				pop(err)
			}
		}
		if d.hd == nil {
			var ev deleteEvent
			if err := ops.popDelete(&ev); err == nil {
				d.hd, got = &ev, true
			} else {
				pop(err)
			}
		}
		if d.hr == nil {
			var ev renameEvent
			if err := ops.popRename(&ev); err == nil {
				d.hr, got = &ev, true
			} else {
				pop(err)
			}
		}
		return got && !failed
	}
	for {
		for fill() {
		}
		if failed {
			return
		}
		sf, sd, sr := noSeqLimit, noSeqLimit, noSeqLimit
		if d.hf != nil {
			sf = d.hf.Seq
		}
		if d.hd != nil {
			sd = d.hd.Seq
		}
		if d.hr != nil {
			sr = d.hr.Seq
		}
		if min3(sf, sd, sr) >= limit {
			return // nothing older than limit is left (held events wait for a later run)
		}
		switch {
		case sf <= sd && sf <= sr:
			ev := d.hf
			d.hf = nil
			ops.flush(ev)
		case sr <= sd:
			ev := d.hr
			d.hr = nil
			ops.rename(ev)
		default:
			ev := d.hd
			d.hd = nil
			ops.delete(ev)
		}
		if ops.stop != nil && ops.stop() {
			return
		}
	}
}

func min3(a, b, c uint64) uint64 {
	if b < a {
		a = b
	}
	if c < a {
		a = c
	}
	return a
}
