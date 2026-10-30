package trace

import (
	"bytes"
	"reflect"
	"testing"
)

const mib = 1 << 20

// A legacy event must serialise exactly as before the range fields existed.
func TestLegacyEventUnchanged(t *testing.T) {
	var b bytes.Buffer
	if err := Write(&b, []Event{{Ts: 5, Ino: 100, Op: OpRead}}); err != nil {
		t.Fatal(err)
	}
	if got, want := b.String(), "{\"ts\":5,\"ino\":100,\"op\":\"read\"}\n"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestRangeEventRoundTrip(t *testing.T) {
	in := []Event{{Ts: 1, Ino: 2, Op: OpRead, Off: 8 * mib, Len: 4 * mib, Qid: 3, Step: 1, Tag: "cluster"}}
	var b bytes.Buffer
	Write(&b, in)
	out, err := Read(&b)
	if err != nil || !reflect.DeepEqual(in, out) {
		t.Fatalf("round trip: %v %v", out, err)
	}
}

func TestChunkReplayHandComputed(t *testing.T) {
	o := ChunkOpts{ChunkSize: 4 * mib}
	ev := []Event{
		// q1 reads chunks 0 and 1 (spans a boundary), then q2 re-reads chunk 0.
		{Ino: 1, Op: OpRead, Off: 3 * mib, Len: 2 * mib, Qid: 1, Tag: "a"},
		{Ino: 1, Op: OpRead, Off: 0, Len: mib, Qid: 2, Tag: "a"},
	}
	r := ChunkReplay(NewNoEvict(), ev, o)
	if r.Requests != 2 || r.ReqHits != 1 || r.Touches != 3 || r.Misses != 2 {
		t.Fatalf("%+v", r)
	}
	if r.MissBytes != 8*mib || r.Bytes != 3*mib || r.HitBytes != mib {
		t.Fatalf("bytes %+v", r)
	}
	if r.Queries != 2 || r.GetsPerQuery() != 1 || r.SerialPerQuery() != 0.5 {
		t.Fatalf("per query %+v", r)
	}
}

func TestChunkReplayDedupAndReadahead(t *testing.T) {
	o := ChunkOpts{ChunkSize: 4 * mib, Readahead: 1}
	ev := []Event{
		// Two reads in the same chunk and step collapse into one touch.
		{Ino: 1, Op: OpRead, Off: 0, Len: mib, Qid: 1, Tag: "cluster"},
		{Ino: 1, Op: OpRead, Off: mib, Len: mib, Qid: 1, Tag: "cluster"},
		// Chunk 1 was prefetched by the first group, so this hits and counts as useful.
		{Ino: 1, Op: OpRead, Off: 4 * mib, Len: mib, Qid: 2, Tag: "cluster"},
	}
	r := ChunkReplay(NewNoEvict(), ev, o)
	if r.Touches != 2 || r.Misses != 1 || r.PfGets != 2 || r.PfUseful != 1 {
		t.Fatalf("%+v", r)
	}
}

func TestOPTBoundsOnChunkStream(t *testing.T) {
	w := GenerateIVF(IVFParams{N: 200000, Clusters: 400, D: 64, Nprobe: 4, Alpha: 0.9, Rerank: 4, Queries: 400, Seed: 1})
	o := ChunkOpts{ChunkSize: mib}
	cap := Footprint(w.Events, o.ChunkSize) / 10
	opt := ChunkReplay(NewOPT(cap, AccessStream(w.Events, o)), w.Events, o)
	lru := ChunkReplay(NewLRU(cap), w.Events, o)
	all := ChunkReplay(NewNoEvict(), w.Events, o)
	if !(lru.ByteHitRate() <= opt.ByteHitRate()+1e-9 && opt.ByteHitRate() <= all.ByteHitRate()+1e-9) {
		t.Fatalf("lru %.3f opt %.3f noevict %.3f", lru.ByteHitRate(), opt.ByteHitRate(), all.ByteHitRate())
	}
}

func TestGeneratorsDeterministic(t *testing.T) {
	ip := IVFParams{N: 100000, Clusters: 300, D: 32, Nprobe: 3, Alpha: 1.1, SizeSkew: true, Rerank: 2, Queries: 50, Seed: 9, ScanEvery: 10, ScanFrac: 0.5}
	if !reflect.DeepEqual(GenerateIVF(ip), GenerateIVF(ip)) {
		t.Fatal("ivf not deterministic")
	}
	hp := HNSWParams{N: 100000, D: 32, M: 16, Ef: 20, Layout: "insertion", Alpha: 0.7, Queries: 20, Seed: 9}
	if !reflect.DeepEqual(GenerateHNSW(hp), GenerateHNSW(hp)) {
		t.Fatal("hnsw not deterministic")
	}
}

// Per-query reads of the uniform IVF layout match a hand calculation.
func TestIVFQueryShape(t *testing.T) {
	p := IVFParams{N: 1_000_000, Clusters: 1000, D: 128, Nprobe: 8, Rerank: 10, Queries: 5, Seed: 1}
	w := GenerateIVF(p)
	if got, want := len(w.Events), 5*(1+8+10); got != want {
		t.Fatalf("events %d want %d", got, want)
	}
	var bytesQ0 uint64
	for _, e := range w.Events {
		if e.Qid == 1 {
			bytesQ0 += e.Len
		}
	}
	// centroids 1000*128*4 + 8 clusters of 1000 vectors * (128*4+8) + 10 raw * 128*4
	want := uint64(1000*128*4 + 8*1000*(128*4+8) + 10*128*4)
	if bytesQ0 != want {
		t.Fatalf("bytes/query %d want %d", bytesQ0, want)
	}
	if w.TotalBytes != uint64(1000*128*4+1_000_000*(128*4+8)+1_000_000*128*4) {
		t.Fatalf("total %d", w.TotalBytes)
	}
}

// BFS layout must touch fewer distinct chunks per query than insertion order.
func TestHNSWLayoutLocality(t *testing.T) {
	mk := func(l string) float64 {
		w := GenerateHNSW(HNSWParams{N: 1_000_000, D: 128, M: 16, Ef: 50, Layout: l, Queries: 30, Seed: 3})
		r := ChunkReplay(NewNoEvict(), w.Events, ChunkOpts{ChunkSize: 4 * mib})
		return float64(r.Touches) / float64(r.Queries)
	}
	if b, i := mk("bfs"), mk("insertion"); !(b < i) {
		t.Fatalf("bfs %.1f insertion %.1f touches/query", b, i)
	}
}
