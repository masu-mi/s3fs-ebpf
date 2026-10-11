package trace

import "math"

// Policy is the interface a simulator implementation satisfies. The
// contract is intentionally narrow — one method per event kind —
// so BPF-side policies can be re-expressed in Go without dragging
// in state that only makes sense in kernel context (folio, mm,
// arena, etc.).
type Policy interface {
	Name() string

	// Access records one read event and returns whether it was
	// a cache hit (true) or a miss that admitted a new item (false).
	// The simulator is responsible for the admit-then-evict cycle
	// when the cache fills up.
	Access(ino uint64) bool

	// Invalidate removes ino from the cache (write semantics). A
	// subsequent Access for the same ino will be a miss. No-ops if
	// ino is not currently admitted.
	Invalidate(ino uint64)

	// Size reports the current admit hash size (for debug / stats).
	Size() int
}

// Result is the numeric summary of one replay.
type Result struct {
	Policy   string  `json:"policy"`
	Accesses int     `json:"accesses"`
	Hits     int     `json:"hits"`
	Misses   int     `json:"misses"`
	Rate     float64 `json:"rate"` // hits / accesses
}

// Replay runs `events` against `p` and returns hits/misses tally.
// Read events drive Access; write events call Invalidate so the next
// read of the same ino counts as a miss. Only reads count toward
// Accesses (and therefore Rate).
func Replay(p Policy, events []Event) Result {
	r := Result{Policy: p.Name()}
	for _, e := range events {
		switch e.Op {
		case OpRead:
			r.Accesses++
			if p.Access(e.Ino) {
				r.Hits++
			} else {
				r.Misses++
			}
		case OpWrite:
			p.Invalidate(e.Ino)
		}
	}
	if r.Accesses > 0 {
		r.Rate = float64(r.Hits) / float64(r.Accesses)
	}
	return r
}

// --- policies -----------------------------------------------------
//
// All simulator policies share the "capacity = softcap" contract:
// once the admit set reaches softcap+N, the policy evicts one item
// per new admission to stay near cap. This mirrors the kmod-side
// invariant where softcap=8 is the enforcement point.

// NewLRU returns a strict LRU with a doubly-linked list ordering.
// Different from policy_lru_ops.bpf.c's approximate age-based
// design — this simulator is the reference LRU everyone recognizes.
func NewLRU(cap int) Policy {
	return &lruSim{cap: cap, order: newLRUList()}
}

type lruSim struct {
	cap   int
	items map[uint64]*lruNode
	order *lruList
}

type lruNode struct {
	ino        uint64
	prev, next *lruNode
}

type lruList struct {
	head, tail *lruNode // head = most recently used, tail = LRU
}

func newLRUList() *lruList { return &lruList{} }

func (l *lruList) moveToHead(n *lruNode) {
	if l.head == n {
		return
	}
	// Detach.
	if n.prev != nil {
		n.prev.next = n.next
	}
	if n.next != nil {
		n.next.prev = n.prev
	}
	if l.tail == n {
		l.tail = n.prev
	}
	// Push to head.
	n.prev = nil
	n.next = l.head
	if l.head != nil {
		l.head.prev = n
	}
	l.head = n
	if l.tail == nil {
		l.tail = n
	}
}

func (l *lruList) pushHead(n *lruNode) {
	n.prev = nil
	n.next = l.head
	if l.head != nil {
		l.head.prev = n
	}
	l.head = n
	if l.tail == nil {
		l.tail = n
	}
}

func (l *lruList) popTail() *lruNode {
	if l.tail == nil {
		return nil
	}
	n := l.tail
	l.tail = n.prev
	if l.tail != nil {
		l.tail.next = nil
	} else {
		l.head = nil
	}
	return n
}

func (l *lruSim) Name() string { return "lru" }

func (l *lruSim) Access(ino uint64) bool {
	if l.items == nil {
		l.items = make(map[uint64]*lruNode)
	}
	if n, ok := l.items[ino]; ok {
		l.order.moveToHead(n)
		return true // hit
	}
	// Miss. Admit and evict if over cap.
	n := &lruNode{ino: ino}
	l.items[ino] = n
	l.order.pushHead(n)
	if len(l.items) > l.cap {
		evicted := l.order.popTail()
		if evicted != nil {
			delete(l.items, evicted.ino)
		}
	}
	return false
}

func (l *lruSim) Invalidate(ino uint64) {
	if l.items == nil {
		return
	}
	n, ok := l.items[ino]
	if !ok {
		return
	}
	delete(l.items, ino)
	if n.prev != nil {
		n.prev.next = n.next
	} else {
		l.order.head = n.next
	}
	if n.next != nil {
		n.next.prev = n.prev
	} else {
		l.order.tail = n.prev
	}
}

func (l *lruSim) Size() int { return len(l.items) }

// NewSIEVE returns a SIEVE implementation matching
// bpf/policy_sieve_ops.bpf.c's semantics: per-ino visited bit,
// second-chance clock, evict when visited=0 on hand pass. The
// hand pointer walks the admit list tail→head; simulator uses a
// linked list to match the paper's fig 2 shape.
func NewSIEVE(cap int) Policy {
	return &sieveSim{cap: cap, order: newLRUList()}
}

type sieveSim struct {
	cap   int
	items map[uint64]*sieveNode
	order *lruList // reused as generic doubly-linked list
	// Hand pointer for the SIEVE clock — walks from tail toward
	// head, wraps around. Nil when empty.
	hand *lruNode
}

type sieveNode struct {
	// Re-use lruNode as the list node (embedded pointer-shape),
	// but store the SIEVE bit on the sim's side to keep the list
	// type generic.
	node    *lruNode
	visited bool
}

func (s *sieveSim) Name() string { return "sieve" }

func (s *sieveSim) Access(ino uint64) bool {
	if s.items == nil {
		s.items = make(map[uint64]*sieveNode)
	}
	if e, ok := s.items[ino]; ok {
		e.visited = true // hit: set visited bit
		return true
	}
	// Miss. If over cap, evict via the SIEVE hand.
	for len(s.items) >= s.cap {
		s.evictOne()
	}
	// Admit at head (newest is head, hand walks from tail).
	n := &lruNode{ino: ino}
	s.order.pushHead(n)
	s.items[ino] = &sieveNode{node: n, visited: false}
	return false
}

func (s *sieveSim) evictOne() {
	// Start hand at tail if unset or if hand fell off.
	if s.hand == nil {
		s.hand = s.order.tail
	}
	for s.hand != nil {
		e := s.items[s.hand.ino]
		if e == nil {
			// Shouldn't happen — defend.
			s.hand = s.hand.prev
			continue
		}
		if e.visited {
			e.visited = false
			// Advance hand toward head (list is tail→head).
			next := s.hand.prev
			if next == nil {
				next = s.order.tail // wrap
			}
			s.hand = next
			continue
		}
		// visited=0 → evict.
		delete(s.items, s.hand.ino)
		// Unlink node from list.
		victim := s.hand
		// Move hand before unlinking so we don't lose position.
		s.hand = victim.prev
		if s.hand == nil {
			s.hand = s.order.tail // will refresh on next call
		}
		if s.hand == victim {
			// victim was the only node (head == tail): don't
			// leave the hand on an unlinked node.
			s.hand = nil
		}
		s.unlink(victim)
		return
	}
}

func (s *sieveSim) unlink(n *lruNode) {
	if n.prev != nil {
		n.prev.next = n.next
	} else {
		s.order.head = n.next
	}
	if n.next != nil {
		n.next.prev = n.prev
	} else {
		s.order.tail = n.prev
	}
}

func (s *sieveSim) Invalidate(ino uint64) {
	if s.items == nil {
		return
	}
	e, ok := s.items[ino]
	if !ok {
		return
	}
	// Advance hand past the evicted node to avoid a dangling pointer.
	if s.hand == e.node {
		s.hand = e.node.prev
		if s.hand == nil {
			s.hand = s.order.tail
		}
		if s.hand == e.node {
			s.hand = nil
		}
	}
	delete(s.items, ino)
	s.unlink(e.node)
}

func (s *sieveSim) Size() int { return len(s.items) }

// NewOPT returns Belady's offline-optimal policy (MIN) for `events`:
// on a miss with a full cache it evicts the item whose next read is
// farthest in the future. No online policy can beat it at the same
// capacity, so it is the upper bound a live hit-rate number at
// softcap=cap has to stay under. Access must be driven with exactly
// the read events of `events`, in order (trace.Replay does this).
func NewOPT(cap int, events []Event) Policy {
	var reads []uint64
	for _, e := range events {
		if e.Op == OpRead {
			reads = append(reads, e.Ino)
		}
	}
	// next[i] = index of the next read of reads[i] after i.
	next := make([]int, len(reads))
	last := make(map[uint64]int)
	for i := len(reads) - 1; i >= 0; i-- {
		if j, ok := last[reads[i]]; ok {
			next[i] = j
		} else {
			next[i] = math.MaxInt
		}
		last[reads[i]] = i
	}
	return &optSim{cap: cap, reads: reads, next: next, items: make(map[uint64]int)}
}

type optSim struct {
	cap   int
	reads []uint64
	next  []int
	pos   int
	items map[uint64]int // ino → index of its next read
}

func (o *optSim) Name() string { return "opt" }

func (o *optSim) Access(ino uint64) bool {
	nu := math.MaxInt
	if o.pos < len(o.reads) && o.reads[o.pos] == ino {
		nu = o.next[o.pos]
	}
	o.pos++
	if _, ok := o.items[ino]; ok {
		o.items[ino] = nu
		return true
	}
	if len(o.items) >= o.cap {
		var victim uint64
		far := -1
		for k, v := range o.items {
			if v > far || (v == far && k < victim) {
				victim, far = k, v
			}
		}
		delete(o.items, victim)
	}
	o.items[ino] = nu
	return false
}

func (o *optSim) Invalidate(ino uint64) { delete(o.items, ino) }

func (o *optSim) Size() int { return len(o.items) }

// NewNoEvict returns an unbounded cache: every re-read hits. It is the
// ceiling for any capacity (only compulsory first-read misses remain)
// and corresponds to the live bench's `observe` policy, which never
// votes to evict.
func NewNoEvict() Policy { return &noEvictSim{items: make(map[uint64]struct{})} }

type noEvictSim struct{ items map[uint64]struct{} }

func (n *noEvictSim) Name() string { return "noevict" }

func (n *noEvictSim) Access(ino uint64) bool {
	if _, ok := n.items[ino]; ok {
		return true
	}
	n.items[ino] = struct{}{}
	return false
}

func (n *noEvictSim) Invalidate(ino uint64) { delete(n.items, ino) }

func (n *noEvictSim) Size() int { return len(n.items) }
