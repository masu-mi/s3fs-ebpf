package main

import (
	"context"
	"fmt"
	"os"
	"sync"

	"github.com/bbsakura/s3fs-ebpf/daemon/internal/objstore"
)

// AC-89: a file larger than the kmod's capacity is served chunk by chunk (a full
// load would be evicted again, fullAdmitExceedsCapacity).  A sequential reader then
// waits one range GET per 4 MiB chunk, serially: reading 100 MiB through a 32 MiB
// capacity took 25 upcalls and about twice the time of an unbounded cache.  The
// handler of a sequential chunk miss therefore also fetches the next few chunks in
// parallel and populates them without a pending (nobody waits for them): when the
// reader arrives, the chunk is there and raises no miss.
//
// Read-ahead is self-clocking: a miss moves the reader's position, and every
// completed read-ahead requests the next chunk, so `window+1` GETs stay in flight
// while the requested range is at most `ahead` chunks past the last miss.  Hits do
// not reach the daemon, so the old rule (refill only on a miss, `window` chunks
// ahead) let the pipeline drain after every window: the reader read the 3 cached
// chunks, then missed on the next one with nothing in flight (timeline of a 100 MiB
// read through 32 MiB: ~5.6 ms idle per 4 chunks).  `ahead` keeps the chunks
// requested but not yet read within the capacity (capacity in chunks - 2: the
// chunk being read, and one for the capacity pass).
//
// Measured, 100 MiB cold read, capacity 32 MiB (median of 5, two rounds): window 3
// refilled on a miss 154 / 158 ms; ahead 6 with 4 in flight 138 / 140 ms; unbounded
// cache 136 / 128 ms.  One range GET for the whole window was slower (255 ms): the
// reader then waits for all of it; a chunk the capacity pass sheds anyway costs one
// more miss, as before.

// readaheadMaxChunks is the upper bound of the window (GETs in flight = window + 1).
const readaheadMaxChunks = 4

// readaheadMaxAhead is the upper bound of `ahead`.
const readaheadMaxAhead = 8

// readaheadState records, per ino, the first offset not yet requested, the last
// miss and how many read-aheads are in flight.
type readaheadState struct {
	mu       sync.Mutex
	next     int64
	last     int64
	inflight int
}

var readaheads sync.Map // ino -> *readaheadState

// raKey identifies a chunk being read ahead.
type raKey struct {
	ino uint64
	off int64
}

// raInflight maps raKey -> chan struct{}, closed (with ok recorded) when the
// read-ahead of that chunk has finished.  A miss on such a chunk waits for it
// instead of fetching the chunk a second time.
var raInflight sync.Map

type raDone struct {
	ch chan struct{}
	ok bool
}

// readaheadWait waits for an in-flight read-ahead of the chunk at off and reports
// whether the chunk was populated by it.  false: none in flight, or it failed.
func readaheadWait(ctx context.Context, ino uint64, off int64) bool {
	v, found := raInflight.Load(raKey{ino, off})
	if !found {
		return false
	}
	d := v.(*raDone)
	select {
	case <-d.ch:
		return d.ok
	case <-ctx.Done():
		return false
	}
}

// readaheadWindow is the number of chunks to read ahead for a given capacity
// (0 = none).
func readaheadWindow(capacity int64) int {
	if capacity <= 0 {
		return 0
	}
	k := int(capacity/partialAdmitChunk/2) - 1
	if k < 0 {
		k = 0
	}
	if k > readaheadMaxChunks {
		k = readaheadMaxChunks
	}
	return k
}

// readaheadAhead is how many chunks past the last miss may be requested (0 = no
// read-ahead).
func readaheadAhead(capacity int64) int {
	if readaheadWindow(capacity) == 0 {
		return 0
	}
	a := int(capacity/partialAdmitChunk) - 2
	if a < 1 {
		a = 1
	}
	if a > readaheadMaxAhead {
		a = readaheadMaxAhead
	}
	return a
}

// readaheadRestart moves the requested range after a miss at missOff: a reader that
// jumped past it, or went back by more than `ahead` chunks, starts a new window.
func readaheadRestart(next, missOff int64, ahead int) int64 {
	if missOff >= next || missOff+int64(ahead+1)*partialAdmitChunk < next {
		return missOff + partialAdmitChunk
	}
	return next
}

// maybeReadahead starts the read-ahead after the miss at missOff of a large file.
// It does nothing unless the file does not fit in the capacity (the other large
// files are loaded whole) and the reader looks sequential.
func maybeReadahead(ctx context.Context, devPath string, s3c objstore.Backend,
	ev readMissEvent, key string, fullSize, missOff int64) {
	if !patternAwareAdmitEnabled || !fullAdmitExceedsCapacity(fullSize) {
		return
	}
	if ev.SeqCount+ev.RandCount == 0 || ev.seqRatio() < seqAdmitRatioThreshold {
		return
	}
	c, _ := capacityBytesFn()
	window, ahead := readaheadWindow(c), readaheadAhead(c)
	if window == 0 || ahead == 0 {
		return
	}
	v, _ := readaheads.LoadOrStore(ev.Ino, &readaheadState{})
	st := v.(*readaheadState)
	st.mu.Lock()
	st.next = readaheadRestart(st.next, missOff, ahead)
	st.last = missOff
	st.mu.Unlock()
	readaheadFill(ctx, devPath, s3c, ev.Ino, key, fullSize, st, ahead, window+1)
}

// readaheadFill starts read-aheads until `conc` are in flight or the requested range
// is `ahead` chunks past the last miss.  Each one refills when it finishes.
func readaheadFill(ctx context.Context, devPath string, s3c objstore.Backend, ino uint64,
	key string, fullSize int64, st *readaheadState, ahead, conc int) {
	for {
		st.mu.Lock()
		off := st.next
		if off >= fullSize || st.inflight >= conc || off > st.last+int64(ahead)*partialAdmitChunk {
			st.mu.Unlock()
			return
		}
		st.next += partialAdmitChunk
		st.inflight++
		st.mu.Unlock()
		n := int64(partialAdmitChunk)
		if off+n > fullSize {
			n = fullSize - off
		}
		k := raKey{ino, off}
		d := &raDone{ch: make(chan struct{})}
		raInflight.Store(k, d)
		go func(off, n int64) {
			defer func() {
				raInflight.Delete(k)
				close(d.ch)
				st.mu.Lock()
				st.inflight--
				st.mu.Unlock()
				readaheadFill(ctx, devPath, s3c, ino, key, fullSize, st, ahead, conc)
			}()
			body, err := s3c.GetRange(ctx, key, off, n)
			if err != nil {
				return // the reader's own miss fetches it
			}
			if err := populatePagesFn(devPath, ino, off, body); err != nil {
				fmt.Fprintf(os.Stderr, "[readahead] ino=%d off=%d populate: %v\n", ino, off, err)
				return
			}
			d.ok = true
		}(off, n)
	}
}
