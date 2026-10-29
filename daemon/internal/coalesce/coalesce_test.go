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
