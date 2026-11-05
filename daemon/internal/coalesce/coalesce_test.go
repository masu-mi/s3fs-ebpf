package coalesce

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/bbsakura/s3fs-ebpf/daemon/internal/kmodio"
)

const window = 30 * time.Millisecond

type recorder struct {
	mu   sync.Mutex
	seen []kmodio.FlushEvent
}

func (r *recorder) handle(_ context.Context, ev *kmodio.FlushEvent) {
	r.mu.Lock()
	r.seen = append(r.seen, *ev)
	r.mu.Unlock()
}

func (r *recorder) get() []kmodio.FlushEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]kmodio.FlushEvent(nil), r.seen...)
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// A burst for one ino collapses to one flush carrying the latest event.
func TestBurstFiresOnceWithTheLatestEvent(t *testing.T) {
	r := &recorder{}
	c := New(window, r.handle)
	for size := uint64(1); size <= 5; size++ {
		c.Submit(context.Background(), &kmodio.FlushEvent{Ino: 7, Size: size})
		time.Sleep(window / 5) // well inside the window: the timer keeps resetting
	}
	if n := len(r.get()); n != 0 {
		t.Fatalf("fired %d times during the burst, want 0", n)
	}
	waitFor(t, func() bool { return len(r.get()) > 0 })
	time.Sleep(2 * window) // and nothing else follows
	got := r.get()
	if len(got) != 1 || got[0].Ino != 7 || got[0].Size != 5 {
		t.Errorf("flushed %+v, want one event for ino 7 with Size 5", got)
	}
}

func TestInosAreDebouncedIndependently(t *testing.T) {
	r := &recorder{}
	c := New(window, r.handle)
	c.Submit(context.Background(), &kmodio.FlushEvent{Ino: 1})
	c.Submit(context.Background(), &kmodio.FlushEvent{Ino: 2})
	waitFor(t, func() bool { return len(r.get()) == 2 })
}

// fsync cancels the stale close-event so it cannot overwrite the fresh bytes
// once its timer fires.
func TestCancelInoDropsThePendingEvent(t *testing.T) {
	r := &recorder{}
	c := New(window, r.handle)
	c.Submit(context.Background(), &kmodio.FlushEvent{Ino: 1})
	c.Submit(context.Background(), &kmodio.FlushEvent{Ino: 2})
	c.CancelIno(1)
	c.CancelIno(99) // unknown ino: no effect
	waitFor(t, func() bool { return len(r.get()) >= 1 })
	time.Sleep(2 * window)
	got := r.get()
	if len(got) != 1 || got[0].Ino != 2 {
		t.Errorf("flushed %+v, want only ino 2", got)
	}
}

// Shutdown flushes what is still pending instead of dropping unwritten data.
func TestShutdownFlushesPendingEvents(t *testing.T) {
	r := &recorder{}
	c := New(time.Hour, r.handle)
	c.Submit(context.Background(), &kmodio.FlushEvent{Ino: 1, Size: 10})
	c.Submit(context.Background(), &kmodio.FlushEvent{Ino: 2, Size: 20})
	c.Shutdown()
	if got := r.get(); len(got) != 2 {
		t.Errorf("Shutdown flushed %d events, want 2", len(got))
	}
}

// FireAll is the barrier a rename or delete takes (AC-84): everything parked in
// the debounce window goes out now, oldest kmod seq first, without waiting for
// the window.
func TestFireAllHandlesParkedEventsInSeqOrder(t *testing.T) {
	r := &recorder{}
	c := New(time.Hour, r.handle) // the window would never expire in this test
	ctx := context.Background()
	c.Submit(ctx, &kmodio.FlushEvent{Ino: 7, Seq: 9})
	c.Submit(ctx, &kmodio.FlushEvent{Ino: 5, Seq: 3})
	c.Submit(ctx, &kmodio.FlushEvent{Ino: 6, Seq: 5})
	c.Submit(ctx, &kmodio.FlushEvent{Ino: 5, Seq: 11}) // same ino: the latest event wins

	c.FireAll(ctx)

	got := r.get()
	if len(got) != 3 || got[0].Ino != 6 || got[1].Ino != 7 || got[2].Ino != 5 {
		t.Fatalf("handled %+v, want inos 6 (seq 5), 7 (seq 9), 5 (seq 11)", got)
	}
	c.FireAll(ctx) // nothing parked any more
	if len(r.get()) != 3 {
		t.Errorf("a second FireAll handled something: %+v", r.get())
	}
}

// FireMatching is the barrier of a rename / delete (AC-84): only the parked
// events it could conflict with go out; the others keep their debounce window.
func TestFireMatchingLeavesOthersParked(t *testing.T) {
	r := &recorder{}
	c := New(time.Hour, r.handle)
	ctx := context.Background()
	c.Submit(ctx, &kmodio.FlushEvent{Ino: 5, Seq: 3})
	c.Submit(ctx, &kmodio.FlushEvent{Ino: 6, Seq: 5})
	c.Submit(ctx, &kmodio.FlushEvent{Ino: 7, Seq: 9})

	c.FireMatching(ctx, func(ev *kmodio.FlushEvent) bool { return ev.Ino != 6 })

	got := r.get()
	if len(got) != 2 || got[0].Ino != 5 || got[1].Ino != 7 {
		t.Fatalf("handled %+v, want inos 5 then 7", got)
	}
	c.FireAll(ctx) // the one left behind is still parked
	if got := r.get(); len(got) != 3 || got[2].Ino != 6 {
		t.Errorf("after FireAll %+v, want ino 6 last", got)
	}
}
