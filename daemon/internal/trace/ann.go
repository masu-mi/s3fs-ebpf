package trace

import (
	"container/heap"
	"math"
	"math/rand"
	"sort"
)

// Synthetic ANN workloads (docs/design/ann-workload-measurement-plan.md
// §3). Every generator is deterministic for a given parameter set and
// seed. Popularity (which clusters / regions queries favour) is an input
// assumption, never a measurement: always sweep it.

const (
	inoIVFIndex = 1
	inoIVFRaw   = 2
	inoHNSWBase = 10
	inoHNSWUp   = 11
	inoScan     = 20
)

// Workload is a generated trace plus the size of the data it ranges over.
type Workload struct {
	Events     []Event
	TotalBytes uint64
}

// zipfPicker draws ranks in [0,n) with weight 1/(r+1)^alpha (uniform at 0).
type zipfPicker struct {
	cdf []float64
	n   int
}

func newZipfPicker(n int, alpha float64) *zipfPicker {
	z := &zipfPicker{n: n}
	if alpha == 0 {
		return z
	}
	z.cdf = make([]float64, n)
	tot := 0.0
	for i := 0; i < n; i++ {
		tot += 1 / math.Pow(float64(i+1), alpha)
		z.cdf[i] = tot
	}
	for i := range z.cdf {
		z.cdf[i] /= tot
	}
	return z
}

func (z *zipfPicker) pick(r *rand.Rand) int {
	if z.cdf == nil {
		return r.Intn(z.n)
	}
	return sort.SearchFloat64s(z.cdf, r.Float64())
}

// IVFParams describes an IVF-flat index of N d-dimensional float32
// vectors split into Clusters inverted lists.
type IVFParams struct {
	N, Clusters   int64
	D             int
	Nprobe        int
	Alpha         float64 // cluster popularity: 0 uniform, else Zipf(alpha)
	SizeSkew      bool    // log-normal (sigma 0.5) cluster sizes instead of uniform
	Rerank        int     // random raw-vector reads per query
	Queries       int
	Seed          int64
	ScanEvery     int     // mixed: a full scan every ScanEvery queries (0 = none)
	ScanFrac      float64 // mixed: scanned file size as a fraction of the index
	ScanChunkSize uint64  // mixed: scan read size (default 4 MiB)
}

// GenerateIVF emits per query: the centroid table (step 0), nprobe whole
// clusters in parallel (step 1), then Rerank random raw vectors (step 2).
func GenerateIVF(p IVFParams) Workload {
	r := rand.New(rand.NewSource(p.Seed))
	vec := uint64(p.D)*4 + 8 // vector + id inside a list
	centroidBytes := uint64(p.Clusters) * uint64(p.D) * 4

	sizes := make([]int64, p.Clusters)
	if p.SizeSkew {
		w := make([]float64, p.Clusters)
		sum := 0.0
		for i := range w {
			w[i] = math.Exp(0.5 * r.NormFloat64())
			sum += w[i]
		}
		var used int64
		for i := range w {
			sizes[i] = int64(float64(p.N) * w[i] / sum)
			used += sizes[i]
		}
		sizes[0] += p.N - used
	} else {
		for i := range sizes {
			sizes[i] = p.N / p.Clusters
		}
		sizes[0] += p.N - (p.N/p.Clusters)*p.Clusters
	}
	off := make([]uint64, p.Clusters+1)
	off[0] = centroidBytes
	for i, s := range sizes {
		off[i+1] = off[i] + uint64(s)*vec
	}
	indexBytes := off[p.Clusters]
	rawBytes := uint64(p.N) * uint64(p.D) * 4
	total := indexBytes + rawBytes

	perm := r.Perm(int(p.Clusters)) // popularity rank -> cluster, independent of layout
	pick := newZipfPicker(int(p.Clusters), p.Alpha)

	var scanSize uint64
	if p.ScanEvery > 0 {
		scanSize = uint64(float64(indexBytes) * p.ScanFrac)
		total += scanSize
	}
	scanChunk := p.ScanChunkSize
	if scanChunk == 0 {
		scanChunk = 4 << 20
	}
	var ev []Event
	add := func(e Event) { e.Ts = uint64(len(ev)) * 1_000_000; e.Op = OpRead; ev = append(ev, e) }
	scanPos := uint64(0)
	for q := 1; q <= p.Queries; q++ {
		qid := uint64(q)
		add(Event{Ino: inoIVFIndex, Off: 0, Len: centroidBytes, Qid: qid, Step: 0, Tag: "centroid"})
		chosen := map[int]bool{}
		for len(chosen) < p.Nprobe && len(chosen) < int(p.Clusters) {
			c := perm[pick.pick(r)]
			if chosen[c] {
				continue
			}
			chosen[c] = true
			add(Event{Ino: inoIVFIndex, Off: off[c], Len: uint64(sizes[c]) * vec, Qid: qid, Step: 1, Tag: "cluster"})
		}
		for k := 0; k < p.Rerank; k++ {
			id := uint64(r.Int63n(p.N))
			add(Event{Ino: inoIVFRaw, Off: id * uint64(p.D) * 4, Len: uint64(p.D) * 4, Qid: qid, Step: 2, Tag: "rerank"})
		}
		if p.ScanEvery > 0 && q%p.ScanEvery == 0 {
			// One-shot sequential scan of another namespace; continues where the last stopped.
			for done := uint64(0); done < scanSize; {
				o := scanPos % scanSize
				l := min(scanChunk, scanSize-done, scanSize-o)
				add(Event{Ino: inoScan, Off: o, Len: l, Tag: "scan"})
				scanPos += l
				done += l
			}
		}
	}
	return Workload{Events: ev, TotalBytes: total}
}

// HNSWParams describes a single-level-base HNSW graph stored as one
// node record per vertex.
type HNSWParams struct {
	N       int64
	D, M    int
	Ef      int
	Layout  string  // "bfs" keeps graph neighbours adjacent; "insertion" scatters them
	Alpha   float64 // query-region popularity: 0 uniform, else Zipf(alpha)
	Queries int
	Seed    int64
}

const (
	hnswWindow   = 4096 // logical-id span of a node's local neighbours
	hnswLongPerc = 20   // % of neighbours that are long links
)

func splitmix(x uint64) uint64 {
	x += 0x9e3779b97f4a7c15
	x = (x ^ x>>30) * 0xbf58476d1ce4e5b9
	x = (x ^ x>>27) * 0x94d049bb133111eb
	return x ^ x>>31
}

// hnswNeighbors returns the (deterministic) logical neighbours of v:
// mostly within hnswWindow of v, hnswLongPerc% anywhere.
func hnswNeighbors(v, n int64, m0 int, buf []int64) []int64 {
	buf = buf[:0]
	for j := 0; j < m0; j++ {
		h := splitmix(uint64(v)*64 + uint64(j))
		h2 := splitmix(h)
		if h%100 < hnswLongPerc {
			buf = append(buf, int64(h2%uint64(n)))
		} else {
			d := int64(h2%(2*hnswWindow+1)) - hnswWindow
			buf = append(buf, ((v+d)%n+n)%n)
		}
	}
	return buf
}

type cand struct {
	id   int64
	dist int64
}
type candHeap []cand

func (h candHeap) Len() int           { return len(h) }
func (h candHeap) Less(i, j int) bool { return h[i].dist < h[j].dist }
func (h candHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *candHeap) Push(x any)        { *h = append(*h, x.(cand)) }
func (h *candHeap) Pop() any          { o := *h; x := o[len(o)-1]; *h = o[:len(o)-1]; return x }

func nextPrime(n int64) int64 {
	for ; ; n++ {
		ok := n > 1
		for d := int64(2); d*d <= n; d++ {
			if n%d == 0 {
				ok = false
				break
			}
		}
		if ok {
			return n
		}
	}
}

func circDist(a, b, n int64) int64 {
	d := a - b
	if d < 0 {
		d = -d
	}
	return min(d, n-d)
}

// GenerateHNSW emits per query: one read per upper layer (serial), the
// entry node, then Ef best-first expansions; each expansion reads all
// 2M neighbour records in parallel (one step per hop), so the step chain
// is the dependency chain the plan asks to count.
func GenerateHNSW(p HNSWParams) Workload {
	r := rand.New(rand.NewSource(p.Seed))
	m0 := 2 * p.M
	node := uint64(p.D)*4 + uint64(m0)*4 + 8
	pos := func(id int64) uint64 { return uint64(id) }
	if p.Layout == "insertion" {
		k := nextPrime(int64(float64(p.N) * 0.6180339887))
		pos = func(id int64) uint64 { return uint64(id * k % p.N) }
	}
	// Upper layers: layer l holds N/M^l nodes, laid out one after another.
	var layerBase []uint64
	var layerDiv []int64
	cnt, div, base := p.N/int64(p.M), int64(p.M), uint64(0)
	for cnt >= 1 {
		layerBase = append(layerBase, base)
		layerDiv = append(layerDiv, div)
		base += uint64(cnt)
		cnt /= int64(p.M)
		div *= int64(p.M)
	}
	total := uint64(p.N)*node + base*node

	regions := int(p.N / hnswWindow)
	if regions < 1 {
		regions = 1
	}
	perm := r.Perm(regions)
	pick := newZipfPicker(regions, p.Alpha)

	var ev []Event
	add := func(e Event) { e.Ts = uint64(len(ev)) * 1_000_000; e.Op = OpRead; ev = append(ev, e) }
	buf := make([]int64, 0, m0)
	for q := 1; q <= p.Queries; q++ {
		qid := uint64(q)
		t := int64(perm[pick.pick(r)])*hnswWindow + r.Int63n(hnswWindow)
		t %= p.N
		step := uint32(0)
		for l := len(layerBase) - 1; l >= 0; l-- { // top layer first
			idx := layerBase[l] + uint64(t/layerDiv[l])
			add(Event{Ino: inoHNSWUp, Off: idx * node, Len: node, Qid: qid, Step: step, Tag: "upper"})
			step++
		}
		entry := ((t+r.Int63n(8*hnswWindow)-4*hnswWindow)%p.N + p.N) % p.N
		add(Event{Ino: inoHNSWBase, Off: pos(entry) * node, Len: node, Qid: qid, Step: step, Tag: "node"})
		step++
		visited := map[int64]bool{entry: true}
		h := &candHeap{{entry, circDist(entry, t, p.N)}}
		for hop := 0; hop < p.Ef && h.Len() > 0; hop++ {
			cur := heap.Pop(h).(cand)
			buf = hnswNeighbors(cur.id, p.N, m0, buf)
			for _, nb := range buf {
				if visited[nb] {
					continue
				}
				visited[nb] = true
				add(Event{Ino: inoHNSWBase, Off: pos(nb) * node, Len: node, Qid: qid, Step: step, Tag: "node"})
				heap.Push(h, cand{nb, circDist(nb, t, p.N)})
			}
			step++
		}
	}
	return Workload{Events: ev, TotalBytes: total}
}
