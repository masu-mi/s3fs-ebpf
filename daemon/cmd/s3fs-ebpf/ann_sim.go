package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/bbsakura/s3fs-ebpf/daemon/internal/trace"
)

// runAnnSim is P1/P2 of docs/design/ann-workload-measurement-plan.md: it
// generates a synthetic ANN trace (IVF / HNSW / mixed) per seed and replays
// it, chunk-granular, through every policy at several capacities given as a
// fraction of the data size. Output is one markdown table.
func runAnnSim(args []string) int {
	fs := flag.NewFlagSet("ann-sim", flag.ExitOnError)
	kind := fs.String("workload", "ivf", "ivf | hnsw | mixed")
	n := fs.Int64("n", 10_000_000, "number of vectors")
	d := fs.Int("d", 128, "dimension")
	clusters := fs.Int64("clusters", 0, "IVF clusters (0 = 4*sqrt(n))")
	nprobe := fs.Int("nprobe", 8, "IVF nprobe")
	alpha := fs.Float64("alpha", 1.1, "popularity: 0 uniform, else Zipf(alpha)")
	skew := fs.Bool("size-skew", false, "IVF log-normal cluster sizes")
	rerank := fs.Int("rerank", 10, "IVF raw-vector reads per query")
	m := fs.Int("m", 16, "HNSW M")
	ef := fs.Int("ef", 50, "HNSW efSearch")
	layout := fs.String("layout", "bfs", "HNSW layout: bfs | insertion")
	queries := fs.Int("queries", 20000, "queries per trace")
	scanEvery := fs.Int("scan-every", 500, "mixed: queries between full scans")
	scanFrac := fs.Float64("scan-frac", 0.5, "mixed: scanned size / index size")
	chunk := fs.Uint64("chunk", 4<<20, "cache chunk size in bytes")
	caps := fs.String("caps", "1,5,10,25", "capacities, percent of data size")
	policies := fs.String("policies", "lru,sieve,s3fifo,wtinylfu,wtinylfu-clock,opt,noevict", "policies")
	seeds := fs.Int("seeds", 5, "seeds per point")
	warmup := fs.Float64("warmup", 0.2, "fraction of events not counted")
	readahead := fs.Int("readahead", 0, "chunks fetched past cluster/scan/centroid reads")
	emit := fs.String("emit", "", "write the seed-1 trace to this JSONL path and exit")
	fs.Parse(args)

	gen := func(seed int64) trace.Workload {
		switch *kind {
		case "hnsw":
			return trace.GenerateHNSW(trace.HNSWParams{N: *n, D: *d, M: *m, Ef: *ef, Layout: *layout, Alpha: *alpha, Queries: *queries, Seed: seed})
		case "ivf", "mixed":
			c := *clusters
			if c == 0 {
				c = int64(4 * sqrt(float64(*n)))
			}
			p := trace.IVFParams{N: *n, Clusters: c, D: *d, Nprobe: *nprobe, Alpha: *alpha, SizeSkew: *skew, Rerank: *rerank, Queries: *queries, Seed: seed}
			if *kind == "mixed" {
				p.ScanEvery, p.ScanFrac = *scanEvery, *scanFrac
			}
			return trace.GenerateIVF(p)
		}
		fmt.Fprintf(os.Stderr, "ann-sim: unknown workload %q\n", *kind)
		os.Exit(2)
		return trace.Workload{}
	}
	if *emit != "" {
		f, err := os.Create(*emit)
		if err != nil {
			fmt.Fprintln(os.Stderr, "ann-sim:", err)
			return 1
		}
		defer f.Close()
		if err := trace.Write(f, gen(1).Events); err != nil {
			fmt.Fprintln(os.Stderr, "ann-sim:", err)
			return 1
		}
		return 0
	}

	var capPct []float64
	for _, s := range strings.Split(*caps, ",") {
		var v float64
		fmt.Sscanf(strings.TrimSpace(s), "%f", &v)
		capPct = append(capPct, v)
	}
	names := strings.Split(*policies, ",")
	o := trace.ChunkOpts{ChunkSize: *chunk, Warmup: *warmup, Readahead: *readahead}

	type key struct {
		cap  int
		name string
	}
	results := map[key][]trace.ChunkResult{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 6)
	totalChunks := 0
	for s := 1; s <= *seeds; s++ {
		wg.Add(1)
		go func(seed int64) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			w := gen(seed)
			total := int((w.TotalBytes + *chunk - 1) / *chunk)
			stream := trace.AccessStream(w.Events, o)
			mu.Lock()
			totalChunks = total
			mu.Unlock()
			for ci, pct := range capPct {
				cp := max(1, int(float64(total)*pct/100))
				for _, name := range names {
					name = strings.TrimSpace(name)
					p, err := makePolicy(name, cp, stream)
					if err != nil {
						fmt.Fprintln(os.Stderr, "ann-sim:", err)
						os.Exit(1)
					}
					r := trace.ChunkReplay(p, w.Events, o)
					mu.Lock()
					results[key{ci, name}] = append(results[key{ci, name}], r)
					mu.Unlock()
				}
			}
		}(int64(s))
	}
	wg.Wait()

	fmt.Printf("# ann-sim workload=%s n=%d d=%d alpha=%g queries=%d chunk=%d seeds=%d warmup=%g readahead=%d data=%d chunks\n\n",
		*kind, *n, *d, *alpha, *queries, *chunk, *seeds, *warmup, *readahead, totalChunks)
	tagSet := map[string]bool{}
	for _, rs := range results {
		for _, r := range rs {
			for t := range r.ByTag {
				tagSet[t] = true
			}
		}
	}
	var tags []string
	for _, t := range []string{"centroid", "upper", "cluster", "node", "rerank", "scan"} {
		if tagSet[t] {
			tags = append(tags, t)
		}
	}
	hdr := "| cap | policy | req hit | byte hit | GET/q | MB/q | serial/q |"
	sep := "|---|---|---|---|---|---|---|"
	for _, t := range tags {
		hdr += " " + t + " |"
		sep += "---|"
	}
	if *readahead > 0 {
		hdr += " pf acc |"
		sep += "---|"
	}
	fmt.Println(hdr)
	fmt.Println(sep)
	for ci, pct := range capPct {
		for _, name := range names {
			name = strings.TrimSpace(name)
			rs := results[key{ci, name}]
			f := func(g func(trace.ChunkResult) float64) string { return meanRange(rs, g) }
			line := fmt.Sprintf("| %g%% | %s | %s | %s | %s | %s | %s |", pct, name,
				f(func(r trace.ChunkResult) float64 { return 100 * r.ReqHitRate() }),
				f(func(r trace.ChunkResult) float64 { return 100 * r.ByteHitRate() }),
				f(trace.ChunkResult.GetsPerQuery),
				f(func(r trace.ChunkResult) float64 { return r.BytesPerQuery() / (1 << 20) }),
				f(trace.ChunkResult.SerialPerQuery))
			for _, t := range tags {
				t := t
				line += " " + f(func(r trace.ChunkResult) float64 { return 100 * r.TagRate(t) }) + " |"
			}
			if *readahead > 0 {
				line += " " + f(func(r trace.ChunkResult) float64 { return 100 * r.PfAccuracy() }) + " |"
			}
			fmt.Println(line)
		}
	}
	return 0
}

func sqrt(x float64) float64 {
	z := x / 2
	for i := 0; i < 60; i++ {
		z = (z + x/z) / 2
	}
	return z
}

// meanRange renders mean [min–max] over seeds.
func meanRange(rs []trace.ChunkResult, g func(trace.ChunkResult) float64) string {
	if len(rs) == 0 {
		return "-"
	}
	lo, hi, sum := g(rs[0]), g(rs[0]), 0.0
	for _, r := range rs {
		v := g(r)
		sum += v
		lo, hi = min(lo, v), max(hi, v)
	}
	return fmt.Sprintf("%.1f [%.1f–%.1f]", sum/float64(len(rs)), lo, hi)
}
