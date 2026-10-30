package trace

import "sort"

// ChunkOpts configures ChunkReplay.
type ChunkOpts struct {
	ChunkSize uint64  // cache granule in bytes (kmod pcache: 4 MiB)
	Warmup    float64 // fraction of leading events whose results are not counted
	Readahead int     // chunks fetched past the end of cluster/scan/centroid reads
}

// TagStat is a per-tag request tally.
type TagStat struct {
	Requests, Hits uint64
}

// ChunkResult summarises one chunk-granular replay. Counters cover only
// events after the warmup prefix; the cache state still advances through it.
type ChunkResult struct {
	Policy string

	Requests, ReqHits uint64 // a request hits when every chunk it touches hits
	Bytes, HitBytes   uint64 // bytes read / bytes served from cache
	Touches, Misses   uint64 // chunk touches after per-step dedup / demand GETs
	MissBytes         uint64 // Misses * ChunkSize: what S3 has to deliver
	PfGets, PfUseful  uint64 // readahead GETs / those later used by a demand read
	PfBytes           uint64

	Queries    uint64 // distinct qids
	SerialGets uint64 // sum over queries of steps that needed at least one GET
	ByTag      map[string]*TagStat
}

func (r ChunkResult) ReqHitRate() float64  { return ratio(r.ReqHits, r.Requests) }
func (r ChunkResult) ByteHitRate() float64 { return ratio(r.HitBytes, r.Bytes) }
func (r ChunkResult) GetsPerQuery() float64 {
	return ratio(r.Misses+r.PfGets, r.Queries)
}
func (r ChunkResult) BytesPerQuery() float64 {
	return ratio(r.MissBytes+r.PfBytes, r.Queries)
}
func (r ChunkResult) SerialPerQuery() float64 { return ratio(r.SerialGets, r.Queries) }
func (r ChunkResult) PfAccuracy() float64     { return ratio(r.PfUseful, r.PfGets) }
func (r ChunkResult) TagRate(tag string) float64 {
	if t := r.ByTag[tag]; t != nil {
		return ratio(t.Hits, t.Requests)
	}
	return 0
}

func ratio(a, b uint64) float64 {
	if b == 0 {
		return 0
	}
	return float64(a) / float64(b)
}

// ChunkKey packs (ino, chunk index) into the uint64 the Policy sees.
func ChunkKey(ino, chunk uint64) uint64 { return ino<<32 | chunk&0xffffffff }

type chunkAccess struct {
	key      uint64
	prefetch bool
	counted  bool
	grp      int // index of the (qid, step) group
	req      int // index of the owning request
	bytes    uint64
}

type chunkPlan struct {
	acc      []chunkAccess
	reqs     []chunkReq
	grpQid   []uint64
	grpCount []bool
}

type chunkReq struct {
	tag     string
	counted bool
}

// planChunks expands events into the policy-independent access stream:
// per (qid, step) group the unique chunks in first-touch order, then the
// readahead chunks. Readahead always issues an Access (it cannot probe
// residency), so the stream does not depend on the policy and OPT can see it.
func planChunks(events []Event, o ChunkOpts) *chunkPlan {
	pl := &chunkPlan{}
	warm := int(float64(len(events)) * o.Warmup)
	cs := o.ChunkSize
	i := 0
	for i < len(events) {
		j := i + 1
		if events[i].Qid != 0 {
			for j < len(events) && events[j].Qid == events[i].Qid && events[j].Step == events[i].Step {
				j++
			}
		}
		grp := len(pl.grpQid)
		pl.grpQid = append(pl.grpQid, events[i].Qid)
		pl.grpCount = append(pl.grpCount, i >= warm)
		seen := map[uint64]int{} // key -> index into pl.acc
		var ra []chunkAccess
		for k := i; k < j; k++ {
			e := events[k]
			if e.Op != OpRead || e.Len == 0 {
				continue
			}
			ri := len(pl.reqs)
			pl.reqs = append(pl.reqs, chunkReq{tag: e.Tag, counted: k >= warm})
			first, last := e.Off/cs, (e.Off+e.Len-1)/cs
			for c := first; c <= last; c++ {
				lo, hi := max(e.Off, c*cs), min(e.Off+e.Len, (c+1)*cs)
				key := ChunkKey(e.Ino, c)
				if idx, ok := seen[key]; ok {
					pl.acc[idx].bytes += hi - lo
					continue
				}
				seen[key] = len(pl.acc)
				pl.acc = append(pl.acc, chunkAccess{key: key, counted: k >= warm, grp: grp, req: ri, bytes: hi - lo})
			}
			if o.Readahead > 0 && (e.Tag == "cluster" || e.Tag == "scan" || e.Tag == "centroid") {
				for c := last + 1; c <= last+uint64(o.Readahead); c++ {
					ra = append(ra, chunkAccess{key: ChunkKey(e.Ino, c), prefetch: true, counted: k >= warm, grp: grp, req: -1})
				}
			}
		}
		pl.acc = append(pl.acc, ra...)
		i = j
	}
	return pl
}

// AccessStream returns the chunk keys ChunkReplay will feed the policy, as
// read events, for building NewOPT.
func AccessStream(events []Event, o ChunkOpts) []Event {
	pl := planChunks(events, o)
	out := make([]Event, len(pl.acc))
	for i, a := range pl.acc {
		out[i] = Event{Ino: a.key, Op: OpRead}
	}
	return out
}

// ChunkReplay drives p with the chunk-granular access stream of events.
// Because the planned stream is policy independent, build OPT with
// NewOPT(cap, AccessStream(events, o)).
func ChunkReplay(p Policy, events []Event, o ChunkOpts) ChunkResult {
	pl := planChunks(events, o)
	res := ChunkResult{Policy: p.Name(), ByTag: map[string]*TagStat{}}
	reqHit := make([]bool, len(pl.reqs))
	for i := range reqHit {
		reqHit[i] = true
	}
	pf := map[uint64]bool{}
	grpMiss := make([]bool, len(pl.grpQid))
	for _, a := range pl.acc {
		hit := p.Access(a.key)
		if a.prefetch {
			if !hit {
				pf[a.key] = true
				if a.counted {
					res.PfGets++
					res.PfBytes += o.ChunkSize
				}
			}
			continue
		}
		if hit && pf[a.key] {
			delete(pf, a.key)
			if a.counted {
				res.PfUseful++
			}
		} else if !hit {
			delete(pf, a.key)
		}
		if !hit {
			reqHit[a.req] = false
			grpMiss[a.grp] = true
		}
		if a.counted {
			res.Touches++
			res.Bytes += a.bytes
			if hit {
				res.HitBytes += a.bytes
			} else {
				res.Misses++
				res.MissBytes += o.ChunkSize
			}
		}
	}
	qs := map[uint64]struct{}{}
	for g, q := range pl.grpQid {
		if q == 0 || !pl.grpCount[g] {
			continue
		}
		qs[q] = struct{}{}
		if grpMiss[g] {
			res.SerialGets++
		}
	}
	res.Queries = uint64(len(qs))
	for i, r := range pl.reqs {
		if !r.counted {
			continue
		}
		res.Requests++
		ts := res.ByTag[r.tag]
		if ts == nil {
			ts = &TagStat{}
			res.ByTag[r.tag] = ts
		}
		ts.Requests++
		if reqHit[i] {
			res.ReqHits++
			ts.Hits++
		}
	}
	return res
}

// Footprint returns the number of distinct chunks events touch.
func Footprint(events []Event, chunkSize uint64) int {
	seen := map[uint64]struct{}{}
	for _, a := range planChunks(events, ChunkOpts{ChunkSize: chunkSize}).acc {
		seen[a.key] = struct{}{}
	}
	return len(seen)
}

// SortedTags lists the tags of r in a stable order.
func (r ChunkResult) SortedTags() []string {
	var t []string
	for k := range r.ByTag {
		t = append(t, k)
	}
	sort.Strings(t)
	return t
}
