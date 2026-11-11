package main

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/bbsakura/s3fs-ebpf/daemon/internal/kmodio"
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

func req(typ uint32, seq uint64) *reqEvent { return &reqEvent{Type: typ, Seq: seq} }

// execLog builds an executor whose handlers append "kind#seq" to *log.
func execLog(log *[]string) *orderedDrain {
	return newOrderedDrain(orderedOps{
		flush:  func(ev *flushEvent) { *log = append(*log, fmt.Sprintf("flush#%d", ev.Seq)) },
		delete: func(ev *deleteEvent) { *log = append(*log, fmt.Sprintf("delete#%d", ev.Seq)) },
		rename: func(ev *renameEvent) { *log = append(*log, fmt.Sprintf("rename#%d", ev.Seq)) },
	})
}

// runAll pushes the events, closes the executor and runs it to the end.
func runAll(d *orderedDrain, evs ...*reqEvent) {
	for _, e := range evs {
		d.push(e)
	}
	d.close()
	d.run()
}

// The executor handles events in the order kmod queued them, whatever their kind.
func TestExecutorFollowsKmodOrder(t *testing.T) {
	resetFlushTracking(t)
	// Model counterexample 2: create b (flush #1), rename b->c (#2): flush first.
	// Counterexample 3: rename a->b (#3), create a (flush #4): rename first.
	var log []string
	runAll(execLog(&log), req(kmodio.ReqFlush, 1), req(kmodio.ReqRename, 2), req(kmodio.ReqRename, 3),
		req(kmodio.ReqFlush, 4), req(kmodio.ReqDelete, 5), req(kmodio.ReqFlush, 6))
	want := []string{"flush#1", "rename#2", "rename#3", "flush#4", "delete#5", "flush#6"}
	if fmt.Sprint(log) != fmt.Sprint(want) {
		t.Errorf("order %v, want %v", log, want)
	}
}

// A WAIT flush (fsync) is in the same queue: it is handled after every event
// queued before it and before every event queued after it.
func TestExecutorWaitFlushKeepsItsPlace(t *testing.T) {
	resetFlushTracking(t)
	var log []string
	wait := req(kmodio.ReqFlush, 3)
	wait.Flags = 1
	runAll(execLog(&log), req(kmodio.ReqRename, 2), wait, req(kmodio.ReqDelete, 4))
	if fmt.Sprint(log) != fmt.Sprint([]string{"rename#2", "flush#3", "delete#4"}) {
		t.Errorf("order %v", log)
	}
}

// The fields of a request reach the handler of its kind.
func TestExecutorConvertsFields(t *testing.T) {
	resetFlushTracking(t)
	fe := reqEvent{Type: kmodio.ReqFlush, Ino: 7, ParentIno: 3, Size: 99, Seq: 1, Flags: 1, NameLen: 1}
	fe.Name[0] = 'f'
	re := reqEvent{Type: kmodio.ReqRename, Ino: 8, ParentIno: 3, NewParentIno: 4, Seq: 2, NameLen: 1, NewNameLen: 2}
	re.Name[0] = 'a'
	copy(re.NewName[:], "bb")
	de := reqEvent{Type: kmodio.ReqDelete, Ino: 9, ParentIno: 5, Seq: 3, NameLen: 1}
	de.Name[0] = 'd'
	var gf *flushEvent
	var gr *renameEvent
	var gd *deleteEvent
	d := newOrderedDrain(orderedOps{
		flush:  func(e *flushEvent) { gf = e },
		rename: func(e *renameEvent) { gr = e },
		delete: func(e *deleteEvent) { gd = e },
	})
	runAll(d, &fe, &re, &de)
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

func TestExecutorStop(t *testing.T) {
	resetFlushTracking(t)
	var log []string
	d := execLog(&log)
	d.ops.stop = func() bool { return len(log) >= 2 }
	runAll(d, req(kmodio.ReqFlush, 1), req(kmodio.ReqFlush, 2), req(kmodio.ReqFlush, 3))
	if fmt.Sprint(log) != fmt.Sprint([]string{"flush#1", "flush#2"}) {
		t.Errorf("order %v, want [flush#1 flush#2]", log)
	}
	if d.pending() != 1 {
		t.Errorf("pending = %d, want 1 (left for the budget message)", d.pending())
	}
}

// run blocks while the queue is empty, wakes on push, and returns after close.
func TestExecutorWaitsForEvents(t *testing.T) {
	resetFlushTracking(t)
	var mu sync.Mutex
	var log []string
	d := newOrderedDrain(orderedOps{flush: func(ev *flushEvent) {
		mu.Lock()
		log = append(log, fmt.Sprintf("flush#%d", ev.Seq))
		mu.Unlock()
	}})
	done := make(chan struct{})
	go func() { d.run(); close(done) }()
	time.Sleep(20 * time.Millisecond)
	d.push(req(kmodio.ReqFlush, 1))
	if !d.waitHandled(1, time.Second) {
		t.Fatal("event 1 not handled")
	}
	d.push(req(kmodio.ReqFlush, 2))
	d.close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("run did not return after close")
	}
	mu.Lock()
	defer mu.Unlock()
	if fmt.Sprint(log) != "[flush#1 flush#2]" {
		t.Errorf("log %v", log)
	}
}

// waitHandled is what a sync uses before it reports kmod's event seq as applied.
func TestWaitHandledTimesOut(t *testing.T) {
	d := newOrderedDrain(orderedOps{})
	if d.waitHandled(5, 20*time.Millisecond) {
		t.Error("waitHandled(5) true with nothing handled")
	}
	d.noteHandled(5)
	if !d.waitHandled(5, 20*time.Millisecond) {
		t.Error("waitHandled(5) false after handled=5")
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

// A daemon that starts after kmod's seq has already advanced (a restart) must not
// leave `handled` at 0: the barrier brings it to kmod's seq once what was queued
// before it is done, and not earlier.
func TestBarrierAdoptsKmodSeqAfterEarlierEvents(t *testing.T) {
	resetFlushTracking(t)
	var log []string
	d := execLog(&log)
	runAll(d, req(kmodio.ReqFlush, 21), req(reqBarrier, 23))
	if got := d.handled.Load(); got != 23 {
		t.Fatalf("handled = %d, want 23", got)
	}
	if len(log) != 1 || log[0] != "flush#21" {
		t.Fatalf("barrier must not reach a handler; log = %v", log)
	}
	if !d.waitHandled(23, time.Millisecond) {
		t.Fatal("waitHandled(23) should return at once after the barrier")
	}
}
