package main

import "sync"

// flushOrder orders the daemon's flush handling per ino (issue #35).
//
// Flush events reach handleFlush by three roads: the fsync (WAIT) loop, which
// handles an event at once, and the ticker loop, which parks the other events
// in the coalescer for its debounce window. A placeholder that .create queued
// (size 0) is typically parked there when the fsync of the file's data
// arrives; kmod's F1 only drops items still in ITS queue. Left alone, the
// parked event fires in the middle of, or after, the fsync's PUT:
//
//   - it PUTs ev.Size == 0 bytes over the finished object, which loses the
//     data, and
//   - its READ_DIRTY makes kmod treat the fsync's flush as covered, so the
//     fsync is woken before its own PUT has landed.
//
// A non-WAIT event popped before a WAIT event for the same ino is covered by
// that WAIT's PUT (the PUT reads the buffer when it runs), so it is dropped.
// handleFlush calls for one ino never overlap.
type flushOrder struct {
	mu      sync.Mutex
	seq     uint64
	waitSeq map[uint64]uint64 // ino -> seq of the latest WAIT event popped
	subSeq  map[uint64]uint64 // ino -> seq of the latest non-WAIT event parked
	locks   map[uint64]*inoLock
}

type inoLock struct {
	mu   sync.Mutex
	refs int
}

func newFlushOrder() *flushOrder {
	return &flushOrder{
		waitSeq: map[uint64]uint64{},
		subSeq:  map[uint64]uint64{},
		locks:   map[uint64]*inoLock{},
	}
}

// noteWait records that a WAIT event for ino has just been popped.
func (o *flushOrder) noteWait(ino uint64) {
	o.mu.Lock()
	o.seq++
	o.waitSeq[ino] = o.seq
	o.mu.Unlock()
}

// noteParked records that a non-WAIT event for ino has just been popped (and
// parked in the coalescer).
func (o *flushOrder) noteParked(ino uint64) {
	o.mu.Lock()
	o.seq++
	o.subSeq[ino] = o.seq
	o.mu.Unlock()
}

func (o *flushOrder) acquire(ino uint64) *inoLock {
	o.mu.Lock()
	l := o.locks[ino]
	if l == nil {
		l = &inoLock{}
		o.locks[ino] = l
	}
	l.refs++
	o.mu.Unlock()
	l.mu.Lock()
	return l
}

func (o *flushOrder) release(ino uint64, l *inoLock) {
	l.mu.Unlock()
	o.mu.Lock()
	if l.refs--; l.refs == 0 {
		delete(o.locks, ino)
	}
	o.mu.Unlock()
}

// runWait runs fn (a WAIT event's handleFlush) once no other flush of ino is
// running.
func (o *flushOrder) runWait(ino uint64, fn func()) {
	l := o.acquire(ino)
	defer o.release(ino, l)
	fn()
}

// runParked runs fn (a parked event's handleFlush) once no other flush of ino
// is running, unless a WAIT event for ino was popped after the parked event
// was: then the WAIT's PUT covers it and fn does not run. It reports whether
// fn ran.
func (o *flushOrder) runParked(ino uint64, fn func()) bool {
	l := o.acquire(ino)
	defer o.release(ino, l)
	o.mu.Lock()
	sub, w := o.subSeq[ino], o.waitSeq[ino]
	if w > sub {
		delete(o.subSeq, ino)
		o.mu.Unlock()
		return false
	}
	o.mu.Unlock()
	fn()
	return true
}

// flushOrd is the daemon's one flushOrder, shared by the flush loops.
var flushOrd = newFlushOrder()

// flushCoal is the coalescer runFlushLoop parks events in, so the fsync loop
// can cancel what is parked for an ino. nil until runFlushLoop starts.
var (
	flushCoalMu sync.Mutex
	flushCoal   *coalescer
)

func setFlushCoalescer(c *coalescer) { flushCoalMu.Lock(); flushCoal = c; flushCoalMu.Unlock() }

func cancelParked(ino uint64) {
	flushCoalMu.Lock()
	c := flushCoal
	flushCoalMu.Unlock()
	if c != nil {
		c.CancelIno(ino)
	}
}
