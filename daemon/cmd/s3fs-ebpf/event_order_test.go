package main

import (
	"errors"
	"fmt"
	"testing"

	"golang.org/x/sys/unix"
)

// queues is a fake kmod: three FIFO queues whose events carry kmod's seq.
type queues struct {
	flush  []flushEvent
	del    []deleteEvent
	rename []renameEvent
	// onPop runs after every successful pop (e.g. to enqueue another event the
	// way a concurrent syscall would).
	onPop func()
}

func (q *queues) ops(log *[]string) orderedOps {
	return orderedOps{
		popFlush: func(ev *flushEvent) error {
			if len(q.flush) == 0 {
				return unix.EAGAIN
			}
			*ev, q.flush = q.flush[0], q.flush[1:]
			if q.onPop != nil {
				q.onPop()
			}
			return nil
		},
		popDelete: func(ev *deleteEvent) error {
			if len(q.del) == 0 {
				return unix.EAGAIN
			}
			*ev, q.del = q.del[0], q.del[1:]
			if q.onPop != nil {
				q.onPop()
			}
			return nil
		},
		popRename: func(ev *renameEvent) error {
			if len(q.rename) == 0 {
				return unix.EAGAIN
			}
			*ev, q.rename = q.rename[0], q.rename[1:]
			if q.onPop != nil {
				q.onPop()
			}
			return nil
		},
		flush:  func(ev *flushEvent) { *log = append(*log, fmt.Sprintf("flush#%d", ev.Seq)) },
		delete: func(ev *deleteEvent) { *log = append(*log, fmt.Sprintf("delete#%d", ev.Seq)) },
		rename: func(ev *renameEvent) { *log = append(*log, fmt.Sprintf("rename#%d", ev.Seq)) },
	}
}

func TestDrainOrderedFollowsSeq(t *testing.T) {
	// Model counterexample 2: create b (flush #1), rename b->c (#2): flush first.
	// Counterexample 3: rename a->b (#3), create a (flush #4): rename first.
	q := &queues{
		flush:  []flushEvent{{Seq: 1}, {Seq: 4}, {Seq: 6}},
		rename: []renameEvent{{Seq: 2}, {Seq: 3}},
		del:    []deleteEvent{{Seq: 5}},
	}
	var log []string
	drainOrdered(q.ops(&log))
	want := []string{"flush#1", "rename#2", "rename#3", "flush#4", "delete#5", "flush#6"}
	if fmt.Sprint(log) != fmt.Sprint(want) {
		t.Errorf("order %v, want %v", log, want)
	}
}

// An event queued while the drain is running (after the empty queue was polled)
// with a smaller seq than a held one must still go first: that is what polling
// the empty slots again after every pop guarantees.
func TestDrainOrderedLateOlderEvent(t *testing.T) {
	q := &queues{flush: []flushEvent{{Seq: 2}}}
	done := false
	q.onPop = func() {
		// the flush #2 has just been popped; a rename with seq #1 was assigned
		// before #2 but only becomes visible now (kmod assigns seq under the
		// lock that queues, so this cannot happen there; the polls after a pop
		// make the drain correct even if the queues were read in any order)
		if !done {
			done = true
			q.rename = append(q.rename, renameEvent{Seq: 1})
		}
	}
	var log []string
	drainOrdered(q.ops(&log))
	if fmt.Sprint(log) != fmt.Sprint([]string{"rename#1", "flush#2"}) {
		t.Errorf("order %v, want [rename#1 flush#2]", log)
	}
}

func TestDrainOrderedPopErrorStops(t *testing.T) {
	var log []string
	ops := (&queues{flush: []flushEvent{{Seq: 1}}}).ops(&log)
	ops.popRename = func(*renameEvent) error { return errors.New("ioctl broke") }
	drainOrdered(ops)
	if len(log) != 0 {
		t.Errorf("handled %v after a pop error, want nothing (the event stays for the next tick)", log)
	}
}

func TestDrainOrderedStop(t *testing.T) {
	q := &queues{flush: []flushEvent{{Seq: 1}, {Seq: 2}, {Seq: 3}}}
	var log []string
	ops := q.ops(&log)
	ops.stop = func() bool { return len(log) >= 2 }
	drainOrdered(ops)
	if fmt.Sprint(log) != fmt.Sprint([]string{"flush#1", "flush#2"}) {
		t.Errorf("order %v, want [flush#1 flush#2]", log)
	}
}

// An fsync's flush (seq 5) is handled at once by its own goroutine. Before it
// PUTs, run(5) must have handled the rename queued before it (seq 3) — and
// must not touch the events queued after it, which stay held for the next run.
func TestOrderedDrainRunLimitHoldsNewerEvents(t *testing.T) {
	q := &queues{
		rename: []renameEvent{{Seq: 3}, {Seq: 8}},
		flush:  []flushEvent{{Seq: 2}, {Seq: 6}},
	}
	var log []string
	d := newOrderedDrain(q.ops(&log))

	d.run(5)
	if fmt.Sprint(log) != fmt.Sprint([]string{"flush#2", "rename#3"}) {
		t.Fatalf("run(5) handled %v, want [flush#2 rename#3]", log)
	}
	d.run(noSeqLimit) // the held #6 and #8, in order, nothing lost
	want := []string{"flush#2", "rename#3", "flush#6", "rename#8"}
	if fmt.Sprint(log) != fmt.Sprint(want) {
		t.Errorf("after the full run %v, want %v", log, want)
	}
}
