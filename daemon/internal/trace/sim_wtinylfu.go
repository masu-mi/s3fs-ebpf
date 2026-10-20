package trace

import "container/list"

// AC-66 (docs/design/wtinylfu-pick-plan.md): reference W-TinyLFU and
// S3-FIFO simulators, plus a W-TinyLFU that follows the BPF version
// (bpf/policy_wtinylfu_pick_ops.bpf.c) step for step, so the cost of the
// BPF approximations (CLOCK instead of LRU inside each segment) is visible
// apart from the live bench's round effects.

// --- frequency sketch -----------------------------------------------

const (
	sketchRows = 4
	sketchMax  = 15 // 4-bit counters
)

var sketchSeeds = [sketchRows]uint32{0x9e3779b1, 0x85ebca77, 0xc2b2ae3d, 0x27d4eb2f}

// sketch is a 4-row Count-Min sketch with saturating 4-bit counters.
// After `sample` increments every counter is halved (TinyLFU's reset).
// Same hash and geometry as the BPF policy.
type sketch struct {
	width  uint32
	cnt    []uint8
	adds   int
	sample int
}

func newSketch(width uint32, sample int) *sketch {
	return &sketch{width: width, cnt: make([]uint8, sketchRows*width), sample: sample}
}

func (s *sketch) slot(ino uint64, row int) uint32 {
	h := ino * 0x9e3779b97f4a7c15
	h ^= uint64(sketchSeeds[row])
	h ^= h >> 33
	return uint32(row)*s.width + uint32(h&uint64(s.width-1))
}

func (s *sketch) increment(ino uint64) {
	for r := 0; r < sketchRows; r++ {
		if i := s.slot(ino, r); s.cnt[i] < sketchMax {
			s.cnt[i]++
		}
	}
	s.adds++
	if s.sample > 0 && s.adds >= s.sample {
		s.reset()
	}
}

func (s *sketch) reset() {
	for i := range s.cnt {
		s.cnt[i] >>= 1
	}
	s.adds = 0
}

func (s *sketch) estimate(ino uint64) uint8 {
	est := uint8(sketchMax)
	for r := 0; r < sketchRows; r++ {
		if c := s.cnt[s.slot(ino, r)]; c < est {
			est = c
		}
	}
	return est
}

// --- W-TinyLFU, the reference (Caffeine's shape) --------------------

// Segments of the reference W-TinyLFU.
const (
	segWindow = iota
	segProbation
	segProtected
)

type wtlfuEntry struct {
	ino uint64
	seg int
}

type wtinylfuSim struct {
	cap, windowCap, protectedCap int
	items                        map[uint64]*list.Element
	seg                          [3]*list.List // front = MRU
	sk                           *sketch
}

// NewWTinyLFU returns W-TinyLFU with exact LRU segments: a 1 % window
// (at least one item), main split 20 % probation / 80 % protected, a
// sketch reset after 10 × cap accesses, and admission by frequency
// (the window's victim must beat probation's LRU to stay).
func NewWTinyLFU(cap int) Policy {
	w := cap / 100
	if w < 1 {
		w = 1
	}
	p := &wtinylfuSim{
		cap: cap, windowCap: w, protectedCap: (cap - w) * 80 / 100,
		items: make(map[uint64]*list.Element),
		sk:    newSketch(sketchWidth(cap), 10*cap),
	}
	for i := range p.seg {
		p.seg[i] = list.New()
	}
	return p
}

func sketchWidth(cap int) uint32 {
	w := uint32(1024)
	for int(w) < 4*cap {
		w <<= 1
	}
	return w
}

func (p *wtinylfuSim) Name() string { return "wtinylfu" }

func (p *wtinylfuSim) Access(ino uint64) bool {
	p.sk.increment(ino)
	if el, ok := p.items[ino]; ok {
		e := el.Value.(*wtlfuEntry)
		switch e.seg {
		case segProbation:
			p.seg[segProbation].Remove(el)
			e.seg = segProtected
			p.items[ino] = p.seg[segProtected].PushFront(e)
			if p.seg[segProtected].Len() > p.protectedCap {
				d := p.seg[segProtected].Back()
				de := p.seg[segProtected].Remove(d).(*wtlfuEntry)
				de.seg = segProbation
				p.items[de.ino] = p.seg[segProbation].PushFront(de)
			}
		default:
			p.seg[e.seg].MoveToFront(el)
		}
		return true
	}
	p.items[ino] = p.seg[segWindow].PushFront(&wtlfuEntry{ino: ino, seg: segWindow})
	if p.seg[segWindow].Len() <= p.windowCap {
		return false
	}
	// The window's LRU becomes the candidate for main.
	c := p.seg[segWindow].Remove(p.seg[segWindow].Back()).(*wtlfuEntry)
	c.seg = segProbation
	if len(p.items) <= p.cap {
		p.items[c.ino] = p.seg[segProbation].PushFront(c)
		return false
	}
	victim := p.seg[segProbation].Back()
	if victim == nil {
		victim = p.seg[segProtected].Back()
	}
	if victim == nil {
		delete(p.items, c.ino)
		return false
	}
	ve := victim.Value.(*wtlfuEntry)
	if p.sk.estimate(c.ino) > p.sk.estimate(ve.ino) {
		p.seg[ve.seg].Remove(victim)
		delete(p.items, ve.ino)
		p.items[c.ino] = p.seg[segProbation].PushFront(c)
	} else {
		delete(p.items, c.ino)
	}
	return false
}

func (p *wtinylfuSim) Invalidate(ino uint64) {
	if el, ok := p.items[ino]; ok {
		p.seg[el.Value.(*wtlfuEntry).seg].Remove(el)
		delete(p.items, ino)
	}
}

func (p *wtinylfuSim) Size() int { return len(p.items) }

// --- W-TinyLFU as the BPF policy does it -----------------------------

type clockEntry struct {
	ino uint64
	seg int
	hit bool
}

type wtinylfuClockSim struct {
	cap   int
	items map[uint64]*list.Element
	seg   [3]*list.List // front = newest, back = oldest
	cand  *list.Element // latest window overflow, while it stays in probation
	sk    *sketch
	// The BPF side resets the sketch lazily (on_hit only counts), at the
	// start of on_insert / pick_victim.
	sample int
	// Every item weighs this much (the kmod's on_size); budgets are in
	// weight units as in the BPF version.
	weight uint64
}

// NewWTinyLFUClock follows bpf/policy_wtinylfu_pick_ops.bpf.c: segments
// are FIFOs with a hit bit (CLOCK), a hit only sets the bit, and all
// movement happens on insert (window balance) or eviction (pick).  Budgets
// come from the resident count, as the BPF side does not know the
// capacity.  Items weigh 4 KiB each, as the kmod reports bytes through
// on_size (so a 1 % window of a small cache holds no item: Caffeine's
// weighted behaviour).  The kmod's rounds, the pending shed and the BPF
// side's limbo and rejected queues are not modelled.
func NewWTinyLFUClock(cap int) Policy { return newWTinyLFUClock(cap, 4096) }

func newWTinyLFUClock(cap int, weight uint64) *wtinylfuClockSim {
	p := &wtinylfuClockSim{
		cap: cap, items: make(map[uint64]*list.Element),
		sk: newSketch(1024, 0), weight: weight,
	}
	for i := range p.seg {
		p.seg[i] = list.New()
	}
	return p
}

func (p *wtinylfuClockSim) Name() string { return "wtinylfu-clock" }

func (p *wtinylfuClockSim) Access(ino uint64) bool {
	p.sk.increment(ino) // on_hit: any read of a known ino
	if el, ok := p.items[ino]; ok {
		el.Value.(*clockEntry).hit = true
		return true
	}
	for len(p.items) >= p.cap {
		p.pick()
	}
	p.insert(ino)
	return false
}

// budgets returns how many items the window and protected segments may
// hold, from the BPF side's byte budgets.
func (p *wtinylfuClockSim) budgets() (window, protected int) {
	total := uint64(len(p.items)) * p.weight
	wb := total / 100
	if wb == 0 {
		wb = 1
	}
	var pb uint64
	if total > wb {
		pb = (total - wb) * 80 / 100
	}
	return int(wb / p.weight), int(pb / p.weight)
}

// move takes el out of its segment and appends it to seg as the newest.
func (p *wtinylfuClockSim) move(el *list.Element, seg int) *list.Element {
	e := el.Value.(*clockEntry)
	p.seg[e.seg].Remove(el)
	if p.cand == el {
		p.cand = nil
	}
	e.seg = seg
	nel := p.seg[seg].PushFront(e)
	p.items[e.ino] = nel
	return nel
}

func (p *wtinylfuClockSim) evict(el *list.Element) {
	e := el.Value.(*clockEntry)
	p.seg[e.seg].Remove(el)
	if p.cand == el {
		p.cand = nil
	}
	delete(p.items, e.ino)
}

func (p *wtinylfuClockSim) insert(ino uint64) {
	p.maybeReset()
	p.items[ino] = p.seg[segWindow].PushFront(&clockEntry{ino: ino, seg: segWindow})
	defer func() {
		p.sample = 10 * len(p.items)
		if p.sample < 16 {
			p.sample = 16
		}
	}()
	for i := 0; i < 2*len(p.items)+2; i++ {
		window, _ := p.budgets()
		b := p.seg[segWindow].Back()
		if p.seg[segWindow].Len() <= window || b == nil {
			return
		}
		e := b.Value.(*clockEntry)
		if e.hit {
			e.hit = false
			p.move(b, segWindow)
			continue
		}
		p.cand = p.move(b, segProbation)
		return
	}
}

func (p *wtinylfuClockSim) maybeReset() {
	if p.sample > 0 && p.sk.adds >= p.sample {
		p.sk.reset()
	}
}

func (p *wtinylfuClockSim) pick() {
	p.maybeReset()
	for i := 0; i < 4*len(p.items)+8; i++ {
		if _, protected := p.budgets(); p.seg[segProtected].Len() > protected {
			b := p.seg[segProtected].Back()
			e := b.Value.(*clockEntry)
			if e.hit {
				e.hit = false
				p.move(b, segProtected)
			} else {
				p.move(b, segProbation)
			}
			continue
		}
		if v := p.seg[segProbation].Back(); v != nil {
			ve := v.Value.(*clockEntry)
			if ve.hit {
				ve.hit = false
				p.move(v, segProtected)
				continue
			}
			if c := p.cand; c != nil && c != v {
				p.cand = nil
				if p.sk.estimate(c.Value.(*clockEntry).ino) > p.sk.estimate(ve.ino) {
					p.evict(v)
				} else {
					p.evict(c)
				}
				return
			}
			p.evict(v)
			return
		}
		seg := segProtected
		if p.seg[seg].Len() == 0 {
			seg = segWindow
		}
		b := p.seg[seg].Back()
		if b == nil {
			return
		}
		if e := b.Value.(*clockEntry); e.hit {
			e.hit = false
			p.move(b, seg)
			continue
		}
		p.evict(b)
		return
	}
}

func (p *wtinylfuClockSim) Invalidate(ino uint64) {
	if el, ok := p.items[ino]; ok {
		p.evict(el)
	}
}

func (p *wtinylfuClockSim) Size() int { return len(p.items) }

// --- S3-FIFO, the reference -----------------------------------------

type s3fifoSim struct {
	cap       int
	small     *list.List // front = newest
	main      *list.List
	ghost     *list.List
	where     map[uint64]*list.Element
	inGhost   map[uint64]*list.Element
	freq      map[uint64]int
	smallSize int
}

// NewS3FIFO returns S3-FIFO (Yang et al., SOSP 2023): a 10 % small FIFO,
// a main FIFO with 2-bit frequency reinsertion, and a ghost FIFO of the
// main queue's size.  The reference for bpf/policy_s3fifo_pick_ops.bpf.c.
func NewS3FIFO(cap int) Policy {
	s := cap / 10
	if s < 1 {
		s = 1
	}
	return &s3fifoSim{
		cap: cap, smallSize: s,
		small: list.New(), main: list.New(), ghost: list.New(),
		where: make(map[uint64]*list.Element), inGhost: make(map[uint64]*list.Element),
		freq: make(map[uint64]int),
	}
}

func (s *s3fifoSim) Name() string { return "s3fifo" }

func (s *s3fifoSim) Access(ino uint64) bool {
	if _, ok := s.where[ino]; ok {
		if s.freq[ino] < 3 {
			s.freq[ino]++
		}
		return true
	}
	for len(s.where) >= s.cap {
		s.evict()
	}
	s.freq[ino] = 0
	if g, ok := s.inGhost[ino]; ok {
		s.ghost.Remove(g)
		delete(s.inGhost, ino)
		s.where[ino] = s.main.PushFront(ino)
	} else {
		s.where[ino] = s.small.PushFront(ino)
	}
	return false
}

func (s *s3fifoSim) evict() {
	for {
		if s.small.Len() >= s.smallSize || s.main.Len() == 0 {
			b := s.small.Back()
			if b == nil {
				return
			}
			ino := s.small.Remove(b).(uint64)
			if s.freq[ino] > 1 {
				s.freq[ino] = 0
				s.where[ino] = s.main.PushFront(ino)
				continue
			}
			delete(s.where, ino)
			delete(s.freq, ino)
			s.inGhost[ino] = s.ghost.PushFront(ino)
			for s.ghost.Len() > s.cap-s.smallSize && s.ghost.Len() > 0 {
				delete(s.inGhost, s.ghost.Remove(s.ghost.Back()).(uint64))
			}
			return
		}
		b := s.main.Back()
		ino := s.main.Remove(b).(uint64)
		if s.freq[ino] > 0 {
			s.freq[ino]--
			s.where[ino] = s.main.PushFront(ino)
			continue
		}
		delete(s.where, ino)
		delete(s.freq, ino)
		return
	}
}

func (s *s3fifoSim) Invalidate(ino uint64) {
	el, ok := s.where[ino]
	if !ok {
		return
	}
	// list.Remove only unlinks from the list that owns el.
	s.small.Remove(el)
	s.main.Remove(el)
	delete(s.where, ino)
	delete(s.freq, ino)
}

func (s *s3fifoSim) Size() int { return len(s.where) }
