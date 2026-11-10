package main

import (
	"errors"
	"fmt"
	"testing"

	"github.com/bbsakura/s3fs-ebpf/daemon/internal/kmodio"
	"golang.org/x/sys/unix"
)

// resetFlushTracking forgets the flushes drainOrdered tracked: the fake handlers
// here never run handleFlush, which is what untracks them in the daemon.
func resetFlushTracking(t *testing.T) {
	t.Helper()
	flushInFlight.mu.Lock()
	flushInFlight.m = nil
	flushInFlight.mu.Unlock()
	t.Cleanup(func() {
		flushInFlight.mu.Lock()
		flushInFlight.m = nil
		flushInFlight.mu.Unlock()
	})
}

// fifo is a fake kmod: the write-back FIFO, whose events carry kmod's seq.
type fifo struct {
	q []wbEvent
}

func wb(typ uint32, seq uint64) wbEvent { return wbEvent{Type: typ, Seq: seq} }

func (f *fifo) ops(log *[]string) orderedOps {
	return orderedOps{
		popWB: func(ev *wbEvent) error {
			if len(f.q) == 0 {
				return unix.EAGAIN
			}
			*ev, f.q = f.q[0], f.q[1:]
			return nil
		},
		flush:  func(ev *flushEvent) { *log = append(*log, fmt.Sprintf("flush#%d", ev.Seq)) },
		delete: func(ev *deleteEvent) { *log = append(*log, fmt.Sprintf("delete#%d", ev.Seq)) },
		rename: func(ev *renameEvent) { *log = append(*log, fmt.Sprintf("rename#%d", ev.Seq)) },
	}
}

// The drain handles events in the order kmod queued them, whatever their kind.
func TestDrainOrderedFollowsFIFO(t *testing.T) {
	resetFlushTracking(t)
	// Model counterexample 2: create b (flush #1), rename b->c (#2): flush first.
	// Counterexample 3: rename a->b (#3), create a (flush #4): rename first.
	f := &fifo{q: []wbEvent{wb(kmodio.WBFlush, 1), wb(kmodio.WBRename, 2), wb(kmodio.WBRename, 3),
		wb(kmodio.WBFlush, 4), wb(kmodio.WBDelete, 5), wb(kmodio.WBFlush, 6)}}
	var log []string
	drainOrdered(f.ops(&log))
	want := []string{"flush#1", "rename#2", "rename#3", "flush#4", "delete#5", "flush#6"}
	if fmt.Sprint(log) != fmt.Sprint(want) {
		t.Errorf("order %v, want %v", log, want)
	}
}

// The fields of a WB event reach the handler of its kind.
func TestDrainOrderedConvertsFields(t *testing.T) {
	resetFlushTracking(t)
	fe := wbEvent{Type: kmodio.WBFlush, Ino: 7, ParentIno: 3, Size: 99, Seq: 1, Flags: 1, NameLen: 1}
	fe.Name[0] = 'f'
	re := wbEvent{Type: kmodio.WBRename, Ino: 8, ParentIno: 3, NewParentIno: 4, Seq: 2, NameLen: 1, NewNameLen: 2}
	re.Name[0] = 'a'
	copy(re.NewName[:], "bb")
	de := wbEvent{Type: kmodio.WBDelete, Ino: 9, ParentIno: 5, Seq: 3, NameLen: 1}
	de.Name[0] = 'd'
	f := &fifo{q: []wbEvent{fe, re, de}}
	var gf *flushEvent
	var gr *renameEvent
	var gd *deleteEvent
	ops := f.ops(new([]string))
	ops.flush = func(e *flushEvent) { gf = e }
	ops.rename = func(e *renameEvent) { gr = e }
	ops.delete = func(e *deleteEvent) { gd = e }
	drainOrdered(ops)
	if gf == nil || gf.Ino != 7 || gf.ParentIno != 3 || gf.Size != 99 || gf.Flags != 1 || gf.NameLen != 1 || gf.Name[0] != 'f' {
		t.Errorf("flush = %+v", gf)
	}
	if gr == nil || gr.Ino != 8 || gr.OldParentIno != 3 || gr.NewParentIno != 4 || gr.OldNameLen != 1 ||
		gr.NewNameLen != 2 || gr.OldName[0] != 'a' || string(gr.NewName[:2]) != "bb" {
		t.Errorf("rename = %+v", gr)
	}
	if gd == nil || gd.Ino != 9 || gd.ParentIno != 5 || gd.NameLen != 1 || gd.Name[0] != 'd' {
		t.Errorf("delete = %+v", gd)
	}
}

func TestDrainOrderedPopErrorStops(t *testing.T) {
	resetFlushTracking(t)
	var log []string
	ops := (&fifo{q: []wbEvent{wb(kmodio.WBFlush, 1)}}).ops(&log)
	ops.popWB = func(*wbEvent) error { return errors.New("ioctl broke") }
	d := newOrderedDrain(ops)
	d.run(noSeqLimit)
	if len(log) != 0 {
		t.Errorf("handled %v after a pop error, want nothing (the event stays for the next tick)", log)
	}
	if !d.failed.Load() {
		t.Error("failed not set after a pop error")
	}
}

func TestDrainOrderedStop(t *testing.T) {
	resetFlushTracking(t)
	f := &fifo{q: []wbEvent{wb(kmodio.WBFlush, 1), wb(kmodio.WBFlush, 2), wb(kmodio.WBFlush, 3)}}
	var log []string
	ops := f.ops(&log)
	ops.stop = func() bool { return len(log) >= 2 }
	drainOrdered(ops)
	if fmt.Sprint(log) != fmt.Sprint([]string{"flush#1", "flush#2"}) {
		t.Errorf("order %v, want [flush#1 flush#2]", log)
	}
}

// An fsync's flush (seq 5) is handled at once by its own goroutine. Before it
// PUTs, run(5) must have handled the events queued before it (seq 2, 3) — and
// must not touch the events queued after it, which stay held for the next run.
func TestOrderedDrainRunLimitHoldsNewerEvents(t *testing.T) {
	resetFlushTracking(t)
	f := &fifo{q: []wbEvent{wb(kmodio.WBFlush, 2), wb(kmodio.WBRename, 3), wb(kmodio.WBFlush, 6), wb(kmodio.WBRename, 8)}}
	var log []string
	d := newOrderedDrain(f.ops(&log))

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

// appliedSeq is what a sync tells kmod (AC-85): the last event handed on, but
// never past a flush whose PUT has not finished (a parked flush is not in S3).
func TestAppliedSeqStopsBeforeUnfinishedFlush(t *testing.T) {
	resetFlushTracking(t)
	d := newOrderedDrain(orderedOps{})
	ordDrain.Store(d)
	t.Cleanup(func() { ordDrain.Store(nil) })

	if got := appliedSeq(); got != 0 {
		t.Fatalf("nothing handled: appliedSeq = %d, want 0", got)
	}
	d.noteHandled(20)
	if got := appliedSeq(); got != 20 {
		t.Fatalf("appliedSeq = %d, want 20", got)
	}
	trackFlush(7, 12) // flush #12 parked in the coalescer
	trackFlush(8, 15)
	if got := appliedSeq(); got != 11 {
		t.Errorf("with flush #12 unfinished: appliedSeq = %d, want 11", got)
	}
	untrackFlush(7, 12) // its PUT finished
	if got := appliedSeq(); got != 14 {
		t.Errorf("with flush #15 unfinished: appliedSeq = %d, want 14", got)
	}
	untrackFlush(8, 15)
	d.noteHandled(9) // never goes backwards
	if got := appliedSeq(); got != 20 {
		t.Errorf("appliedSeq = %d, want 20", got)
	}
}

// A PUT for event #5 that finishes while a newer event #9 of the same ino is
// parked must not make #9 look applied.
func TestAppliedSeqKeepsNewerParkedFlush(t *testing.T) {
	resetFlushTracking(t)
	d := newOrderedDrain(orderedOps{})
	ordDrain.Store(d)
	t.Cleanup(func() { ordDrain.Store(nil) })
	d.noteHandled(30)

	trackFlush(7, 5) // #5 fires: PUT in flight
	trackFlush(7, 9) // #9 is parked meanwhile
	untrackFlush(7, 5)
	if got := appliedSeq(); got != 8 {
		t.Errorf("appliedSeq = %d after #5's PUT with #9 parked, want 8", got)
	}
	untrackFlush(7, 9)
	if got := appliedSeq(); got != 30 {
		t.Errorf("appliedSeq = %d after #9's PUT, want 30", got)
	}
}

func TestFlushBarrierPredicates(t *testing.T) {
	mk := func(ino, parent uint64, name string) *flushEvent {
		ev := &flushEvent{Ino: ino, ParentIno: parent, NameLen: uint32(len(name))}
		copy(ev.Name[:], name)
		return ev
	}
	ren := &renameEvent{Ino: 1, OldParentIno: 0, NewParentIno: 0, OldNameLen: 1, NewNameLen: 1}
	ren.OldName[0], ren.NewName[0] = 'a', 'b'
	del := &deleteEvent{Ino: 1, ParentIno: 0, NameLen: 1}
	del.Name[0] = 'a'

	for _, tt := range []struct {
		name string
		f    *flushEvent
		ren  bool
		del  bool
	}{
		{"same inode, other name", mk(1, 0, "zzz"), true, true},
		{"re-created old name (other inode)", mk(2, 0, "a"), true, true},
		{"someone created the new name", mk(3, 0, "b"), true, false},
		{"unrelated file", mk(4, 0, "c"), false, false},
		{"same name in another directory", mk(5, 9, "a"), false, false},
	} {
		if got := flushTouchesRename(tt.f, ren); got != tt.ren {
			t.Errorf("%s: touches rename a->b = %v, want %v", tt.name, got, tt.ren)
		}
		if got := flushTouchesDelete(tt.f, del); got != tt.del {
			t.Errorf("%s: touches delete a = %v, want %v", tt.name, got, tt.del)
		}
	}
}
