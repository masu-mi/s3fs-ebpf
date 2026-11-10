package main

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	fpebpf "github.com/bbsakura/s3fs-ebpf/daemon/internal/ebpf"
)

// fakeS3 serves "v<n>:<key>" bodies; gate (if set) blocks each GET until released.
type fakeS3 struct {
	mu    sync.Mutex
	ver   map[string]int
	gets  map[string]int
	gate  chan struct{}
	start chan string // GET started (when non-nil)
}

func newFakeS3() *fakeS3 { return &fakeS3{ver: map[string]int{}, gets: map[string]int{}} }

func (f *fakeS3) get(ctx context.Context, key string) ([]byte, error) {
	f.mu.Lock()
	f.gets[key]++
	v := f.ver[key] // what S3 holds when it serves the GET
	g, st := f.gate, f.start
	f.mu.Unlock()
	if st != nil {
		st <- key
	}
	if g != nil {
		<-g
	}
	return []byte(fmt.Sprintf("v%d:%s", v, key)), nil
}

func (f *fakeS3) put(key string) { f.mu.Lock(); f.ver[key]++; f.mu.Unlock() }

func body(v int, key string) string { return fmt.Sprintf("v%d:%s", v, key) }

func newTestStager(f *fakeS3) *stager {
	cfg := stagingCfg{maxBytes: 1 << 20, maxObj: 1 << 20, depth: 3, workers: 4, ttl: time.Hour}
	return newStager(cfg, f.get)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for i := 0; i < 500; i++ {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}

func TestStagingTakeHitAndConsume(t *testing.T) {
	f := newFakeS3()
	s := newTestStager(f)
	key := "d/a"
	size := uint64(len(body(0, key)))
	s.fetch(context.Background(), sibling{ino: 1, key: key, size: size})
	b, ok := s.take(context.Background(), key, size) // waits for the flight
	if !ok || string(b) != body(0, key) {
		t.Fatalf("take = %q %v", b, ok)
	}
	if _, ok := s.take(context.Background(), key, size); ok {
		t.Fatal("a staged copy must be consumed by the first take")
	}
}

func TestStagingSizeMismatchIsAMiss(t *testing.T) {
	f := newFakeS3()
	s := newTestStager(f)
	s.fetch(context.Background(), sibling{ino: 1, key: "k", size: uint64(len(body(0, "k")))})
	if _, ok := s.take(context.Background(), "k", 999); ok {
		t.Fatal("a copy of another size than the entry's must not be served")
	}
}

func TestStagingInvalidateDropsStaged(t *testing.T) {
	f := newFakeS3()
	s := newTestStager(f)
	size := uint64(len(body(0, "k")))
	s.fetch(context.Background(), sibling{ino: 1, key: "k", size: size})
	waitFor(t, "staged", func() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.staged["k"] != nil })
	s.invalidate("k")
	if _, ok := s.take(context.Background(), "k", size); ok {
		t.Fatal("served a copy after invalidate")
	}
}

// ObjStaging.tla, variant done-nogen: a fetch that was served before a write and
// finishes after the PUT's invalidation must not be stored.
func TestStagingFetchAcrossInvalidationIsDropped(t *testing.T) {
	f := newFakeS3()
	f.gate = make(chan struct{})
	f.start = make(chan string, 1)
	s := newTestStager(f)
	size := uint64(len(body(0, "k")))
	s.fetch(context.Background(), sibling{ino: 1, key: "k", size: size})
	<-f.start  // S3 has served version 0 ...
	f.put("k") // ... the file is rewritten (PUT done) ...
	s.invalidate("k")
	close(f.gate) // ... and only now the old GET returns
	waitFor(t, "flight done", func() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.flights["k"] == nil })
	if b, ok := s.take(context.Background(), "k", size); ok {
		t.Fatalf("served %q fetched across an invalidation", b)
	}
}

func TestStagingByteLimitAndTTL(t *testing.T) {
	f := newFakeS3()
	s := newTestStager(f)
	s.cfg.maxBytes = 30
	now := time.Unix(1000, 0)
	s.now = func() time.Time { return now }
	for i := 0; i < 5; i++ {
		k := fmt.Sprintf("k%d", i)
		s.fetch(context.Background(), sibling{ino: uint64(i), key: k, size: uint64(len(body(0, k)))})
		waitFor(t, "fetch "+k, func() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.flights[k] == nil })
	}
	s.mu.Lock()
	over := s.bytes > s.cfg.maxBytes
	s.mu.Unlock()
	if over {
		t.Fatalf("staged %d bytes, limit %d", s.bytes, s.cfg.maxBytes)
	}
	now = now.Add(2 * time.Hour)
	s.cfg.ttl = time.Second
	if _, ok := s.take(context.Background(), "k4", uint64(len(body(0, "k4")))); ok {
		t.Fatal("served an expired copy")
	}
}

func testEntries(n int) []fpebpf.Entry {
	var out []fpebpf.Entry
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("f%03d", i)
		out = append(out, fpebpf.Entry{Ino: uint64(100 + i), ParentIno: 1, Name: name, S3Key: "d/" + name,
			Mode: 0o100644, Fallback: uint64(len(body(0, "d/"+name)))})
	}
	return out
}

func newPredictorStager(f *fakeS3, n int) *stager {
	s := newTestStager(f)
	es := testEntries(n)
	s.gen = func() uint64 { return 1 }
	s.list = func() []fpebpf.Entry { return es }
	return s
}

func TestStagingSequentialMissesFetchAhead(t *testing.T) {
	f := newFakeS3()
	s := newPredictorStager(f, 10)
	ctx := context.Background()
	s.observe(ctx, 100) // f000
	s.observe(ctx, 101) // f001: one successor, not yet
	f.mu.Lock()
	n := len(f.gets)
	f.mu.Unlock()
	if n != 0 {
		t.Fatalf("fetched %d before %d successors", n, stagingSeqRun)
	}
	s.observe(ctx, 102) // f002: second successor -> f003..f005
	waitFor(t, "3 fetches", func() bool { f.mu.Lock(); defer f.mu.Unlock(); return len(f.gets) == 3 })
	for _, k := range []string{"d/f003", "d/f004", "d/f005"} {
		if _, ok := s.take(ctx, k, uint64(len(body(0, k)))); !ok {
			t.Errorf("%s not staged", k)
		}
	}
	s.observe(ctx, 103) // served from stage; the window slides by one
	waitFor(t, "f006", func() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.gets["d/f006"] == 1 })
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.gets["d/f004"] != 1 {
		t.Errorf("f004 fetched %d times", f.gets["d/f004"])
	}
}

func TestStagingRandomMissesDoNotFetch(t *testing.T) {
	f := newFakeS3()
	s := newPredictorStager(f, 20)
	for _, ino := range []uint64{100, 105, 102, 110, 101, 118} {
		s.observe(context.Background(), ino)
	}
	time.Sleep(20 * time.Millisecond)
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.gets) != 0 {
		t.Fatalf("fetched %v for a non-sequential pattern", f.gets)
	}
}

func TestStagingPausesWhenNothingIsUsed(t *testing.T) {
	f := newFakeS3()
	s := newPredictorStager(f, 2000)
	ctx := context.Background()
	// a reader that walks the order but consumes nothing from the stage
	for i := 0; i < 600; i++ {
		s.observe(ctx, uint64(100+i))
		time.Sleep(50 * time.Microsecond)
	}
	s.pmu.Lock()
	paused := s.now().Before(s.pausedTo)
	s.pmu.Unlock()
	if !paused {
		t.Fatal("staging kept fetching although no staged copy was used")
	}
}
