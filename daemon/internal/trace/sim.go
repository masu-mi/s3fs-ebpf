package trace

// Policy is the interface a simulator implementation satisfies. The
// contract is intentionally narrow — one method per event kind —
// so BPF-side policies can be re-expressed in Go without dragging
// in state that only makes sense in kernel context (folio, mm,
// arena, etc.).
type Policy interface {
	Name() string

	// Access records one workload event and returns whether it was
	// a cache hit (true) or a miss that admitted a new item (false).
	// The simulator is responsible for the admit-then-evict cycle
	// when the cache fills up.
	Access(ino uint64) bool

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
// One line = one policy behavior over the trace; caller loops for
// multi-policy comparison.
func Replay(p Policy, events []Event) Result {
	r := Result{Policy: p.Name(), Accesses: len(events)}
	for _, e := range events {
		if e.Op != OpRead {
			continue // write / delete: simulator scope for now is read
		}
		if p.Access(e.Ino) {
			r.Hits++
		} else {
			r.Misses++
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

func (s *sieveSim) Size() int { return len(s.items) }
