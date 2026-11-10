package main

import (
	"context"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	fpebpf "github.com/bbsakura/s3fs-ebpf/daemon/internal/ebpf"
	"github.com/bbsakura/s3fs-ebpf/daemon/internal/metrics"
	"github.com/bbsakura/s3fs-ebpf/daemon/internal/objstore"
)

// Object staging (backlog AC-88, docs/design/object-staging.md, specs/tla/ObjStaging.tla).
//
// A cold read of many small files (cat of a directory) is a chain: miss on file i ->
// upcall -> S3 GET -> populate -> reader opens file i+1.  The GET is 90% of the
// handler (728 of 820 us per file, §18.57), and the reader's next open depends on
// the previous read, so nothing overlaps.  When the misses walk a directory in name
// order, the handler of file i therefore also starts the GETs of the next `depth`
// files into daemon memory; the miss of file i+1 then populates from memory.
//
// A staged copy is only a copy of what S3 held when it was fetched.  The model
// shows two rules keep it from being served after a local write: every PUT /
// delete / rename of a key invalidates it BEFORE and AFTER the S3 call
// (stagingInvalidate), and a fetch that finished across an invalidation is
// dropped (flight.stale).  An object changed by someone else is covered only by
// the TTL.

// stagingCfg bounds the memory and the work.
type stagingCfg struct {
	maxBytes int64         // staged bytes held (and in flight) at most
	maxObj   int64         // objects larger than this are not staged
	depth    int           // files ahead of the current miss
	workers  int           // GETs in flight at most
	ttl      time.Duration // a staged copy older than this is not served
}

var defaultStagingCfg = stagingCfg{maxBytes: 64 << 20, maxObj: 1 << 20, depth: 16, workers: 8, ttl: 5 * time.Second}

// stagingSeqRun is how many consecutive successors (in name order) of the
// previous miss start the read-ahead.
const stagingSeqRun = 2

// stagingMaxEntries: a table larger than this is not indexed (the index is
// rebuilt when the table changes, and its memory grows with it); staging is off.
const stagingMaxEntries = 500000

// stagingTuneWindow fetches are judged together; below stagingMinUse of them
// used, staging pauses for stagingPause (a directory read in another order).
const (
	stagingTuneWindow = 256
	stagingMinUse     = 0.25
	stagingPause      = 10 * time.Second
)

type stagedObj struct {
	data []byte
	at   time.Time
}

type stagingFlight struct {
	done  chan struct{}
	stale bool // invalidated while in flight (guarded by stager.mu)
}

type sibling struct {
	ino  uint64
	key  string
	size uint64
}

type dirState struct {
	last      int
	run       int
	fetchedTo int
}

type stager struct {
	cfg stagingCfg
	get func(ctx context.Context, key string) ([]byte, error)
	now func() time.Time
	sem chan struct{}

	mu      sync.Mutex
	staged  map[string]*stagedObj
	order   []string // insertion order, for eviction
	flights map[string]*stagingFlight
	bytes   int64

	// the predictor (guarded by pmu)
	pmu      sync.Mutex
	gen      func() uint64
	list     func() []fpebpf.Entry
	byParent map[uint64][]sibling // regular files of a directory, in name order
	pos      map[uint64]int       // ino -> position in its directory's list
	parentOf map[uint64]uint64
	idxGen   uint64
	idxAt    time.Time
	dirs     map[uint64]*dirState
	pausedTo time.Time

	issued atomic.Uint64
	used   atomic.Uint64
	lastN  uint64 // issued at the last judgement
	lastU  uint64
}

var staging *stager // nil = off

func newStager(cfg stagingCfg, get func(ctx context.Context, key string) ([]byte, error)) *stager {
	return &stager{
		cfg:     cfg,
		get:     get,
		now:     time.Now,
		sem:     make(chan struct{}, cfg.workers),
		staged:  map[string]*stagedObj{},
		flights: map[string]*stagingFlight{},
		dirs:    map[uint64]*dirState{},
		gen:     fpebpf.Generation,
		list:    fpebpf.Snapshot,
	}
}

// invalidate drops the staged copy of key and marks a fetch in flight as stale.
// Call it before and after every S3 write of the key (ObjStaging.tla: InvalStart,
// InvalDone).
func (s *stager) invalidate(key string) {
	s.mu.Lock()
	if so := s.staged[key]; so != nil {
		s.bytes -= int64(len(so.data))
		delete(s.staged, key)
		metrics.Global.StagingWasted.Add(1)
	}
	if f := s.flights[key]; f != nil {
		f.stale = true
	}
	s.mu.Unlock()
}

func stagingInvalidate(key string) {
	if staging != nil && key != "" {
		staging.invalidate(key)
	}
}

// take returns the staged copy of key if it is there (or arrives from a fetch
// already in flight) and still matches the entry: size equal, not expired.  The
// copy is consumed: a later miss of the same file (evicted from kmod) GETs again.
func (s *stager) take(ctx context.Context, key string, size uint64) ([]byte, bool) {
	for pass := 0; pass < 2; pass++ {
		s.mu.Lock()
		if so := s.staged[key]; so != nil {
			delete(s.staged, key)
			s.bytes -= int64(len(so.data))
			ok := uint64(len(so.data)) == size && s.now().Sub(so.at) <= s.cfg.ttl
			s.mu.Unlock()
			if !ok {
				metrics.Global.StagingWasted.Add(1)
				return nil, false
			}
			s.used.Add(1)
			metrics.Global.StagingHits.Add(1)
			return so.data, true
		}
		f := s.flights[key]
		s.mu.Unlock()
		if f == nil || pass == 1 {
			return nil, false
		}
		select {
		case <-f.done:
		case <-ctx.Done():
			return nil, false
		}
	}
	return nil, false
}

// fetch starts the GET of key unless it is staged or in flight.  Caller has
// decided it is worth it.
func (s *stager) fetch(ctx context.Context, sb sibling) {
	s.mu.Lock()
	if s.staged[sb.key] != nil || s.flights[sb.key] != nil || s.bytes+int64(sb.size) > s.cfg.maxBytes {
		s.mu.Unlock()
		return
	}
	f := &stagingFlight{done: make(chan struct{})}
	s.flights[sb.key] = f
	s.bytes += int64(sb.size) // reserved while in flight
	s.mu.Unlock()
	s.issued.Add(1)
	metrics.Global.StagingFetches.Add(1)
	go func() {
		s.sem <- struct{}{}
		data, err := s.get(ctx, sb.key)
		<-s.sem
		s.mu.Lock()
		s.bytes -= int64(sb.size)
		delete(s.flights, sb.key)
		if !f.stale && err == nil && uint64(len(data)) == sb.size {
			s.bytes += int64(len(data))
			s.staged[sb.key] = &stagedObj{data: data, at: s.now()}
			s.order = append(s.order, sb.key)
			s.trimLocked()
		} else {
			metrics.Global.StagingWasted.Add(1)
		}
		s.mu.Unlock()
		close(f.done)
	}()
}

// trimLocked drops the oldest staged copies while over the byte limit and the
// expired ones at the front.
func (s *stager) trimLocked() {
	now := s.now()
	for len(s.order) > 0 {
		k := s.order[0]
		so := s.staged[k]
		if so != nil && s.bytes <= s.cfg.maxBytes && now.Sub(so.at) <= s.cfg.ttl {
			break
		}
		s.order = s.order[1:]
		if so != nil {
			delete(s.staged, k)
			s.bytes -= int64(len(so.data))
			metrics.Global.StagingWasted.Add(1)
		}
	}
	if len(s.order) == 0 {
		s.order = nil
	}
}

// refreshIndex (pmu held) rebuilds the directory index when the table changed,
// at most once a second.
func (s *stager) refreshIndex() {
	gen := s.gen()
	if s.byParent != nil && (gen == s.idxGen || s.now().Sub(s.idxAt) < time.Second) {
		return
	}
	snap := s.list()
	if len(snap) > stagingMaxEntries {
		snap = nil
	}
	byParent := map[uint64][]sibling{}
	names := map[uint64][]string{}
	for _, e := range snap {
		if e.S3Key == "" || e.Mode&0o170000 != 0o100000 {
			continue
		}
		byParent[e.ParentIno] = append(byParent[e.ParentIno], sibling{ino: e.Ino, key: e.S3Key, size: e.Fallback})
		names[e.ParentIno] = append(names[e.ParentIno], e.Name)
	}
	pos := make(map[uint64]int, len(snap))
	parentOf := make(map[uint64]uint64, len(snap))
	for p, l := range byParent {
		nm := names[p]
		idx := make([]int, len(l))
		for i := range idx {
			idx[i] = i
		}
		sort.Slice(idx, func(a, b int) bool { return nm[idx[a]] < nm[idx[b]] })
		sorted := make([]sibling, len(l))
		for i, j := range idx {
			sorted[i] = l[j]
			pos[l[j].ino] = i
			parentOf[l[j].ino] = p
		}
		byParent[p] = sorted
	}
	s.byParent, s.pos, s.parentOf, s.idxGen, s.idxAt = byParent, pos, parentOf, gen, s.now()
	s.dirs = map[uint64]*dirState{}
}

// observe is called for every miss of a small file.  When the misses walk a
// directory in name order it starts the GETs of the next files.
func (s *stager) observe(ctx context.Context, ino uint64) {
	s.pmu.Lock()
	if s.now().Before(s.pausedTo) {
		s.pmu.Unlock()
		return
	}
	s.refreshIndex()
	p, ok := s.parentOf[ino]
	if !ok {
		s.pmu.Unlock()
		return
	}
	list := s.byParent[p]
	i := s.pos[ino]
	ds := s.dirs[p]
	if ds == nil {
		ds = &dirState{last: i, fetchedTo: i}
		s.dirs[p] = ds
	} else if i == ds.last+1 {
		ds.run++
		ds.last = i
	} else if i != ds.last {
		ds.run, ds.last, ds.fetchedTo = 0, i, i
	}
	var todo []sibling
	if ds.run >= stagingSeqRun {
		j := ds.fetchedTo + 1
		if j < i+1 {
			j = i + 1
		}
		for ; j <= i+s.cfg.depth && j < len(list); j++ {
			if sb := list[j]; sb.size > 0 && int64(sb.size) <= s.cfg.maxObj {
				todo = append(todo, sb)
			}
			ds.fetchedTo = j
		}
	}
	// judge the last window
	if n := s.issued.Load(); n-s.lastN >= stagingTuneWindow {
		u := s.used.Load()
		if float64(u-s.lastU) < stagingMinUse*float64(n-s.lastN) {
			s.pausedTo = s.now().Add(stagingPause)
			for _, d := range s.dirs {
				d.run = 0
			}
		}
		s.lastN, s.lastU = n, u
	}
	s.pmu.Unlock()
	for _, sb := range todo {
		s.fetch(ctx, sb)
	}
}

// stagingFor wires the stager to a backend.
func newBackendStager(cfg stagingCfg, s3c objstore.Backend) *stager {
	return newStager(cfg, s3c.Get)
}
