package prewarmq

import (
	"reflect"
	"testing"
)

func TestSelectWithinCapacity(t *testing.T) {
	items := []Item{{Ino: 1, Size: 40}, {Ino: 2, Size: 50}, {Ino: 3, Size: 10}, {Ino: 4, Size: 5}}
	if got, skipped := SelectWithinCapacity(items, 0, 0); len(got) != 4 || skipped != 0 {
		t.Fatalf("unlimited: got %v skipped %d", got, skipped)
	}
	got, skipped := SelectWithinCapacity(items, 30, 100)
	want := []Item{{Ino: 1, Size: 40}, {Ino: 3, Size: 10}, {Ino: 4, Size: 5}}
	if !reflect.DeepEqual(got, want) || skipped != 1 {
		t.Fatalf("capped: got %v skipped %d, want %v skipped 1", got, skipped, want)
	}
}

func TestQueue(t *testing.T) {
	q := New()
	q.Done(q.Take())
	q.Add([]Item{{Ino: 7, Size: 70}})
	q.Add([]Item{{Ino: 8, Size: 80}})
	q.Add([]Item{{Ino: 7, Size: 70}}) // already queued: dropped
	if !q.IsWarming(7) || q.IsWarming(9) {
		t.Fatal("warming set wrong")
	}
	got := q.Take()
	defer q.Done(got)
	want := []Item{{Ino: 7, Size: 70}, {Ino: 8, Size: 80}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if rest := q.Take(); len(rest) != 0 {
		t.Fatalf("queue not drained: %v", rest)
	}
	select {
	case <-q.Kick():
	default:
		t.Fatal("queue did not kick the worker")
	}
}

func TestSelectWithinCapacityPrefetch(t *testing.T) {
	items := []Item{
		{Ino: 1, Size: 90},                 // startup: fills the budget
		{Ino: 2, Size: 20, Prefetch: true}, // Prefetch: not summed, ≤ cap/4
		{Ino: 3, Size: 30, Prefetch: true}, // Prefetch: > cap/4, skipped
		{Ino: 4, Size: 20},                 // startup: over budget, skipped
	}
	got, skipped := SelectWithinCapacity(items, 0, 100)
	want := []Item{items[0], items[1]}
	if !reflect.DeepEqual(got, want) || skipped != 2 {
		t.Fatalf("got %v skipped %d, want %v skipped 2", got, skipped, want)
	}
}

func TestDoneClearsWarming(t *testing.T) {
	q := New()
	q.Done(q.Take())
	q.Add([]Item{{Ino: 11, Size: 1}, {Ino: 12, Size: 1}})
	items := q.Take()
	if !q.IsWarming(11) {
		t.Fatal("taken but unread item must stay warming")
	}
	q.Done(items)
	if q.IsWarming(11) || q.IsWarming(12) {
		t.Fatal("doneWarming left items")
	}
	<-q.Kick()
}
