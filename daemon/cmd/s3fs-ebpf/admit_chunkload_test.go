package main

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

type fakeKmod struct {
	mu        sync.Mutex
	populated map[int64][]byte
	signals   []string
	evictOnce map[int64]bool // first verified signal for the chunk answers EAGAIN
}

func (k *fakeKmod) ops(src []byte, delay time.Duration, failChunk int64) chunkLoadOps {
	return chunkLoadOps{
		getRange: func(ctx context.Context, off, n int64) ([]byte, error) {
			time.Sleep(delay)
			if off == failChunk {
				return nil, fmt.Errorf("boom")
			}
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			return append([]byte(nil), src[off:off+n]...), nil
		},
		populate: func(off int64, d []byte) error {
			k.mu.Lock()
			defer k.mu.Unlock()
			k.populated[off] = d
			return nil
		},
		signal: func(off int64, code int32, flags uint32) error {
			k.mu.Lock()
			defer k.mu.Unlock()
			k.signals = append(k.signals, fmt.Sprintf("%d:%d:%d", off/partialAdmitChunk, code, flags))
			if flags != 0 && k.evictOnce[off] {
				k.evictOnce[off] = false
				delete(k.populated, off)
				return unix.EAGAIN
			}
			return nil
		},
		enforce: func() {},
	}
}

func newFake() *fakeKmod {
	return &fakeKmod{populated: map[int64][]byte{}, evictOnce: map[int64]bool{}}
}

func testBody(size int) []byte {
	b := make([]byte, size)
	for i := range b {
		b[i] = byte(i * 7)
	}
	return b
}

// The whole object ends up populated, the miss chunk is signalled first, and
// a tail chunk shorter than 4 MiB is handled.
func TestChunkLoad_PopulatesAllStartingAtMiss(t *testing.T) {
	size := 5*partialAdmitChunk + 123
	src := testBody(size)
	k := newFake()
	l := newChunkLoad(int64(size))
	if err := l.run(context.Background(), k.ops(src, time.Millisecond, -1), 3); err != nil {
		t.Fatal(err)
	}
	var got []byte
	for i := 0; i < l.n; i++ {
		d, ok := k.populated[int64(i)*partialAdmitChunk]
		if !ok {
			t.Fatalf("chunk %d not populated", i)
		}
		got = append(got, d...)
	}
	if !bytes.Equal(got, src) {
		t.Fatal("populated bytes differ from source")
	}
	// The workers start with the miss chunk; it is among the first signals.
	want := fmt.Sprintf("3:0:%d", s3fsSignalVerifyPopulated)
	found := false
	for _, s := range k.signals[:chunkLoadWorkers] {
		found = found || s == want
	}
	if !found {
		t.Fatalf("signals = %v, want the miss chunk 3 among the first %d", k.signals, chunkLoadWorkers)
	}
}

// A chunk shed between populate and signal is populated again.
func TestChunkLoad_RepopulatesAfterEAGAIN(t *testing.T) {
	size := 3 * partialAdmitChunk
	k := newFake()
	k.evictOnce[partialAdmitChunk] = true
	l := newChunkLoad(int64(size))
	if err := l.run(context.Background(), k.ops(testBody(size), 0, -1), 0); err != nil {
		t.Fatal(err)
	}
	if _, ok := k.populated[partialAdmitChunk]; !ok {
		t.Fatal("chunk 1 was not populated again after EAGAIN")
	}
}

// A failing range GET fails the load and releases followers with -EIO.
func TestChunkLoad_FailureReleasesFollowers(t *testing.T) {
	size := 6 * chunkLoadGroup * chunkLoadWorkers / 4 * partialAdmitChunk
	k := newFake()
	ops := k.ops(testBody(size), 5*time.Millisecond, 0)
	l := newChunkLoad(int64(size))
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); l.follow(context.Background(), ops, l.n-1) }()
	if err := l.run(context.Background(), ops, 0); err == nil {
		t.Fatal("run succeeded despite a failing GET")
	}
	wg.Wait()
	k.mu.Lock()
	defer k.mu.Unlock()
	found := false
	for _, s := range k.signals {
		if s == fmt.Sprintf("%d:-5:0", l.n-1) {
			found = true
		}
	}
	if !found {
		t.Fatalf("last-chunk follower not answered with -EIO; signals=%v", k.signals)
	}
}

// A follower is woken by the loader's populate of its chunk, with no extra GET.
func TestChunkLoad_FollowerWaitsForItsChunk(t *testing.T) {
	size := 4 * partialAdmitChunk
	k := newFake()
	var gets int
	var gmu sync.Mutex
	ops := k.ops(testBody(size), 2*time.Millisecond, -1)
	inner := ops.getRange
	ops.getRange = func(ctx context.Context, off, n int64) ([]byte, error) {
		gmu.Lock()
		gets++
		gmu.Unlock()
		return inner(ctx, off, n)
	}
	l := newChunkLoad(int64(size))
	done := make(chan struct{})
	go func() { l.follow(context.Background(), ops, 2); close(done) }()
	if err := l.run(context.Background(), ops, 0); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("follower not released")
	}
	if want := (l.n + chunkLoadGroup - 1) / chunkLoadGroup; gets != want {
		t.Fatalf("GETs = %d, want %d (one per group of %d chunks)", gets, want, chunkLoadGroup)
	}
}
