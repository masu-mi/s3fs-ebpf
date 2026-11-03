package main

import (
	"sync"
	"testing"
	"time"
)

// A parked event popped before a WAIT event for the same ino is covered by the
// WAIT's PUT and must not run: it would PUT its stale size (0 for the empty
// placeholder .create queues) over the finished object (issue #35, B step).
func TestFlushOrder_ParkedBeforeWaitIsDropped(t *testing.T) {
	o := newFlushOrder()
	o.noteParked(7)
	o.noteWait(7)
	ran := false
	if o.runParked(7, func() { ran = true }) || ran {
		t.Fatal("parked event popped before a WAIT event ran")
	}
}

func TestFlushOrder_ParkedAfterWaitRuns(t *testing.T) {
	o := newFlushOrder()
	o.noteWait(7)
	o.noteParked(7)
	ran := false
	if !o.runParked(7, func() { ran = true }) || !ran {
		t.Fatal("parked event popped after a WAIT event did not run")
	}
}

func TestFlushOrder_OtherInoUnaffected(t *testing.T) {
	o := newFlushOrder()
	o.noteParked(8)
	o.noteWait(7)
	ran := false
	o.runParked(8, func() { ran = true })
	if !ran {
		t.Fatal("a WAIT event for another ino dropped this ino's parked event")
	}
}

// The race itself: the parked event's timer has already fired and its handler
// is waiting while the WAIT event's PUT is in progress. It must wait for that
// PUT, find itself superseded and not PUT anything afterwards.
func TestFlushOrder_FiredParkedStandsAsideForRunningWait(t *testing.T) {
	o := newFlushOrder()
	o.noteParked(7)
	o.noteWait(7)

	inWait := make(chan struct{})
	releaseWait := make(chan struct{})
	var mu sync.Mutex
	var puts []string
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		o.runWait(7, func() {
			close(inWait)
			<-releaseWait // the 100 MiB PUT in flight
			mu.Lock()
			puts = append(puts, "wait:100MiB")
			mu.Unlock()
		})
	}()
	<-inWait

	wg.Add(1)
	go func() {
		defer wg.Done()
		o.runParked(7, func() {
			mu.Lock()
			puts = append(puts, "parked:0B")
			mu.Unlock()
		})
	}()
	time.Sleep(50 * time.Millisecond) // let it block on the ino lock
	close(releaseWait)
	wg.Wait()

	if len(puts) != 1 || puts[0] != "wait:100MiB" {
		t.Fatalf("puts = %v, want only the WAIT event's", puts)
	}
}

// handleFlush calls for one ino never overlap.
func TestFlushOrder_SerialisesPerIno(t *testing.T) {
	o := newFlushOrder()
	var mu sync.Mutex
	active, maxActive := 0, 0
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			fn := func() {
				mu.Lock()
				active++
				if active > maxActive {
					maxActive = active
				}
				mu.Unlock()
				time.Sleep(2 * time.Millisecond)
				mu.Lock()
				active--
				mu.Unlock()
			}
			if i%2 == 0 {
				o.runWait(5, fn)
			} else {
				o.noteParked(5)
				o.runParked(5, fn)
			}
		}(i)
	}
	wg.Wait()
	if maxActive != 1 {
		t.Fatalf("%d flushes of one ino ran at once", maxActive)
	}
	if len(o.locks) != 0 {
		t.Fatalf("%d ino locks leaked", len(o.locks))
	}
}
