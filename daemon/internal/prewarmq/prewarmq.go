// Package prewarmq is the queue of files the pcache prewarm worker reads
// through the mount, and the rule that keeps that work within the cache
// capacity.
package prewarmq

import "sync"

// Item is one file to read.
type Item struct {
	Ino      uint64
	Size     uint64
	Prefetch bool // AC-68: queued by a read miss, not by the prewarm policy
}

// Queue holds the items waiting for the worker.  Safe for concurrent use.
type Queue struct {
	mu    sync.Mutex
	items []Item
	// warming holds the inos queued or being read by the worker.  A miss on
	// one of them is the worker's own read, so it must not trigger
	// prefetch_next (that would chain through the whole directory).
	warming map[uint64]struct{}
	kick    chan struct{}
}

// New returns an empty queue.
func New() *Queue {
	return &Queue{
		warming: make(map[uint64]struct{}),
		kick:    make(chan struct{}, 1),
	}
}

// Add queues the items whose ino is not already queued or being read, wakes
// the worker, and returns how many it queued.
func (q *Queue) Add(items []Item) int {
	n := 0
	q.mu.Lock()
	for _, it := range items {
		if _, busy := q.warming[it.Ino]; busy {
			continue
		}
		q.warming[it.Ino] = struct{}{}
		q.items = append(q.items, it)
		n++
	}
	q.mu.Unlock()
	if n > 0 {
		select {
		case q.kick <- struct{}{}:
		default:
		}
	}
	return n
}

// Kick is signalled whenever Add queued something.
func (q *Queue) Kick() <-chan struct{} { return q.kick }

// IsWarming reports whether ino is queued or being read by the worker.
func (q *Queue) IsWarming(ino uint64) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	_, ok := q.warming[ino]
	return ok
}

// Done marks the items as read.  Taken items stay "warming" until then.
func (q *Queue) Done(items []Item) {
	q.mu.Lock()
	for _, it := range items {
		delete(q.warming, it.Ino)
	}
	q.mu.Unlock()
}

// Take removes and returns everything queued.
func (q *Queue) Take() []Item {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := q.items
	q.items = nil
	return out
}

// SelectWithinCapacity keeps startup prewarm items, in order, while the
// running total (starting at used) stays within capBytes: prewarming past the
// capacity would only evict what was just prewarmed.  Prefetch items (AC-68)
// replace older content as the reader moves on, so they are not summed; each
// is kept unless it alone exceeds a quarter of the capacity.  capBytes <= 0
// means no limit.  It returns the kept items and how many were skipped.
func SelectWithinCapacity(items []Item, used uint64, capBytes int64) ([]Item, int) {
	if capBytes <= 0 {
		return items, 0
	}
	var out []Item
	for _, it := range items {
		if it.Prefetch {
			if it.Size > uint64(capBytes)/4 {
				continue
			}
		} else {
			if used+it.Size > uint64(capBytes) {
				continue
			}
			used += it.Size
		}
		out = append(out, it)
	}
	return out, len(items) - len(out)
}
