package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"

	"github.com/bbsakura/s3fs-ebpf/daemon/internal/metrics"
	"github.com/bbsakura/s3fs-ebpf/daemon/internal/objstore"
	"golang.org/x/sys/unix"
)

// AC-52 step 3: a full admit of a large file in pcache mode used to be one
// s3c.Get followed by POPULATE, and the reader slept until the last byte had
// arrived (300 MiB: GET 435-591 ms, then POPULATE 100-284 ms, serially).  The
// chunk load below fetches the 4 MiB chunks with a few parallel range GETs,
// populates and signals them one by one in file order starting at the miss,
// so the reader is woken as soon as its own chunk is in.
//
// The reader then reaches the next chunk before it is populated and raises
// another miss.  That handler must not start a second full-file load: a
// chunkLoad is registered per ino, and a handler that finds one waits for
// its chunk instead (follow).

// chunkLoadWorkers is the number of range GETs in flight per load.  Local
// MinIO levels off at 4 (docs/journal/benchmark-results.md §18.34).
const chunkLoadWorkers = 4

// chunkLoadGroup is how many contiguous chunks one range GET fetches (16 MiB);
// they are populated and signalled one by one.  Fewer, larger requests cut
// the per-request cost on the object store (§18.37).
const chunkLoadGroup = 4

type chunkLoad struct {
	size int64
	n    int
	done []chan struct{} // closed once chunk i is populated or the load failed

	mu  sync.Mutex
	ok  []bool // chunk i was populated
	err error
}

// chunkLoadOps are the effects of a load, so tests can replace them.
type chunkLoadOps struct {
	getRange func(ctx context.Context, off, n int64) ([]byte, error)
	populate func(off int64, data []byte) error
	signal   func(chunkOff int64, errCode int32, flags uint32) error
	enforce  func()
}

// chunkLoads maps ino -> *chunkLoad for loads in flight.
var chunkLoads sync.Map

func newChunkLoad(size int64) *chunkLoad {
	n := int((size + partialAdmitChunk - 1) / partialAdmitChunk)
	l := &chunkLoad{size: size, n: n, done: make([]chan struct{}, n),
		ok: make([]bool, n)}
	for i := range l.done {
		l.done[i] = make(chan struct{})
	}
	return l
}

func (l *chunkLoad) chunkLen(i int) int64 {
	off := int64(i) * partialAdmitChunk
	return min(int64(partialAdmitChunk), l.size-off)
}

func (l *chunkLoad) setErr(err error) {
	l.mu.Lock()
	if l.err == nil {
		l.err = err
	}
	l.mu.Unlock()
}

// populateAndSignal populates chunk i and wakes its waiter, re-populating
// (bounded) when kmod reports that the chunk was evicted in between — the
// same contract as the single-chunk path (signal VERIFY_POPULATED, AC-45 C2).
// A chunk nobody waits for answers ENOENT, which is fine.
func (l *chunkLoad) populateAndSignal(ops chunkLoadOps, i int, data []byte, enforce bool) error {
	off := int64(i) * partialAdmitChunk
	for attempt := 0; ; attempt++ {
		if err := ops.populate(off, data); err != nil {
			return err
		}
		if enforce && attempt == 0 {
			ops.enforce()
		}
		err := ops.signal(off, 0, s3fsSignalVerifyPopulated)
		if !errors.Is(err, unix.EAGAIN) {
			return nil
		}
		if attempt >= signalVerifyRetries {
			_ = ops.signal(off, 0, 0) // let the reader's own retry / fallback take over
			return nil
		}
		metrics.Global.ReadMissRepopulates.Add(1)
	}
}

// run fetches every chunk (starting at missIdx, then wrapping) and populates
// them.  The caller is the handler of the first miss.  Each worker fetches a
// chunk, populates it and wakes its waiter before taking the next one, so the
// GETs and the populate ioctls (page allocation and copy in kmod) overlap and
// both run chunkLoadWorkers wide; chunks complete roughly in file order from
// the miss.
func (l *chunkLoad) run(ctx context.Context, ops chunkLoadOps, missIdx int) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	order := make([]int, 0, l.n)
	for k := 0; k < l.n; k++ {
		order = append(order, (missIdx+k)%l.n)
	}
	// groups of up to chunkLoadGroup chunks that are contiguous in the file
	// (a wrap past the end of the file ends a group), as {position, count}.
	var groups [][2]int
	for k := 0; k < l.n; {
		cnt := 1
		for cnt < chunkLoadGroup && k+cnt < l.n && order[k+cnt] == order[k]+cnt {
			cnt++
		}
		groups = append(groups, [2]int{k, cnt})
		k += cnt
	}
	var next atomic.Int64
	var wg sync.WaitGroup
	for w := 0; w < chunkLoadWorkers && w < len(groups); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				gi := int(next.Add(1)) - 1
				if gi >= len(groups) || ctx.Err() != nil {
					return
				}
				k, cnt := groups[gi][0], groups[gi][1]
				first := order[k]
				goff := int64(first) * partialAdmitChunk
				var glen int64
				for j := 0; j < cnt; j++ {
					glen += l.chunkLen(first + j)
				}
				b, err := ops.getRange(ctx, goff, glen)
				if err == nil && int64(len(b)) != glen {
					err = io.ErrUnexpectedEOF
				}
				if err != nil {
					l.setErr(fmt.Errorf("chunks %d-%d: %w", first, first+cnt-1, err))
					cancel()
					return
				}
				for j := 0; j < cnt; j++ {
					i := first + j
					cb := b[int64(j)*partialAdmitChunk : int64(j)*partialAdmitChunk+l.chunkLen(i)]
					// enforce the capacity once the miss chunk is in and once the
					// last chunk is in, before their signals (still pending-protected).
					perr := l.populateAndSignal(ops, i, cb, k+j == 0 || k+j == l.n-1)
					if perr != nil {
						l.setErr(perr)
						cancel()
						return
					}
					l.mu.Lock()
					l.ok[i] = true
					l.mu.Unlock()
					close(l.done[i])
				}
			}
		}()
	}
	wg.Wait()
	// Release every follower: chunks that were not populated stay !ok.
	l.mu.Lock()
	for i := range l.done {
		if !l.ok[i] {
			close(l.done[i])
		}
	}
	err := l.err
	l.mu.Unlock()
	return err
}

// follow serves a miss on chunk ci of a load that is already running: wait
// for the chunk, then wake the reader (re-populating from the retained body
// if the chunk was shed meanwhile).
func (l *chunkLoad) follow(ctx context.Context, ops chunkLoadOps, ci int) {
	select {
	case <-l.done[ci]:
	case <-ctx.Done():
		return
	}
	l.mu.Lock()
	ok := l.ok[ci]
	l.mu.Unlock()
	off := int64(ci) * partialAdmitChunk
	if !ok {
		_ = ops.signal(off, -5 /* -EIO */, 0)
		return
	}
	for attempt := 0; ; attempt++ {
		err := ops.signal(off, 0, s3fsSignalVerifyPopulated)
		if !errors.Is(err, unix.EAGAIN) {
			return
		}
		if attempt >= signalVerifyRetries {
			_ = ops.signal(off, 0, 0)
			return
		}
		metrics.Global.ReadMissRepopulates.Add(1)
		// the body is not kept (a 300 MiB load would hold it all): refetch
		data, gerr := ops.getRange(ctx, off, l.chunkLen(ci))
		if gerr != nil {
			_ = ops.signal(off, 0, 0)
			return
		}
		if err := ops.populate(off, data); err != nil {
			_ = ops.signal(off, 0, 0)
			return
		}
	}
}

// serveChunkLoad is handleReadMiss for a full admit of a large file in pcache
// mode: start the chunk load for ev.Ino, or follow the one already running.
func serveChunkLoad(ctx context.Context, devPath string, s3c objstore.Backend,
	ev readMissEvent, key, name string, fullSize, missChunkOffset int64) {
	ops := chunkLoadOps{
		getRange: func(ctx context.Context, off, n int64) ([]byte, error) {
			return s3c.GetRange(ctx, key, off, n)
		},
		populate: func(off int64, data []byte) error {
			return populatePagesFn(devPath, ev.Ino, off, data)
		},
		signal: func(off int64, errCode int32, flags uint32) error {
			return signalAdmitFlagsFn(devPath, ev.Ino, off, errCode, flags)
		},
		enforce: func() { enforceCapacityAfterPopulate(devPath) },
	}
	mine := newChunkLoad(fullSize)
	ci := int(missChunkOffset / partialAdmitChunk)
	if v, loaded := chunkLoads.LoadOrStore(ev.Ino, mine); loaded {
		if l := v.(*chunkLoad); ci < l.n {
			l.follow(ctx, ops, ci)
			return
		}
		// The size changed under a running load: load on our own.
		chunkLoads.Store(ev.Ino, mine)
	}
	defer chunkLoads.CompareAndDelete(ev.Ino, mine)
	if ci >= mine.n {
		ci = 0
	}
	if err := mine.run(ctx, ops, ci); err != nil {
		fmt.Fprintf(os.Stderr, "[upcall] ino=%d s3://%s chunk load: %v\n", ev.Ino, key, err)
		metrics.Global.ReadMissErrors.Add(1)
		_ = ops.signal(missChunkOffset, -5 /* -EIO */, 0)
		return
	}
	fmt.Printf("[upcall] ino=%d %s pcache chunk load [0,%d)\n", ev.Ino, name, fullSize)
}
