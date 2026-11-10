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
// The window is at most half the capacity (in chunks) minus the chunk being read
// (32 MiB: 3 chunks; measured 2: 221 ms, 3: 200 ms, 4 and 6: no better, 100 MiB cold
// read, no read-ahead 266 ms, unbounded cache 165 ms).  One range GET for the whole
// window was slower (255 ms): the reader then waits for all of it; a chunk the capacity pass sheds anyway
// costs one more miss, as before.

// readaheadMaxChunks is the upper bound of the window.
const readaheadMaxChunks = 4

// readaheadState records, per ino, the first offset not yet requested.
type readaheadState struct {
	mu   sync.Mutex
	next int64
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

// readaheadRange returns the chunk offsets [from, to) to request after a miss on
// the chunk at missOff, given what was requested before (next).  A reader that
// went back (next is further ahead than one window) restarts the window.
func readaheadRange(missOff, next, fullSize int64, window int) (from, to int64) {
	from = missOff + partialAdmitChunk
	if next > from && next <= missOff+int64(window+1)*partialAdmitChunk {
		from = next
	}
	to = missOff + int64(window+1)*partialAdmitChunk
	if to > fullSize {
		to = fullSize
	}
	if from >= to {
		return 0, 0
	}
	return from, to
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
	window := readaheadWindow(c)
	if window == 0 {
		return
	}
	v, _ := readaheads.LoadOrStore(ev.Ino, &readaheadState{})
	st := v.(*readaheadState)
	st.mu.Lock()
	from, to := readaheadRange(missOff, st.next, fullSize, window)
	if to != 0 {
		st.next = to
	}
	st.mu.Unlock()
	for off := from; off < to; off += partialAdmitChunk {
		n := int64(partialAdmitChunk)
		if off+n > fullSize {
			n = fullSize - off
		}
		k := raKey{ev.Ino, off}
		d := &raDone{ch: make(chan struct{})}
		raInflight.Store(k, d)
		go func(off, n int64) {
			defer func() {
				raInflight.Delete(k)
				close(d.ch)
			}()
			body, err := s3c.GetRange(ctx, key, off, n)
			if err != nil {
				return // the reader's own miss fetches it
			}
			if err := populatePagesFn(devPath, ev.Ino, off, body); err != nil {
				fmt.Fprintf(os.Stderr, "[readahead] ino=%d off=%d populate: %v\n", ev.Ino, off, err)
				return
			}
			d.ok = true
		}(off, n)
	}
}
