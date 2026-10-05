package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/bbsakura/s3fs-ebpf/daemon/internal/trace"
)

// runTraceReplay is the P-04 offline trace harness. Feeds a workload
// (either a JSON Lines file — bench-hitrate.sh writes its exact trace
// to $OUT/trace.jsonl — or a synthetic Zipf trace) through Go-side policy simulators and
// reports per-policy hit rates in milliseconds — no kmod, no mount.
//
// Not a replacement for the live bench: BPF struct_ops semantics
// (softcap-driven admit_replace cascade, drain-feedback loop,
// on_hit-per-folio cadence) don't perfectly match the userspace
// simulator. Use trace-replay for algorithm-level intuition, then
// scripts/bench-hitrate.sh for the real numbers.
func runTraceReplay(args []string) int {
	fs := flag.NewFlagSet("trace-replay", flag.ExitOnError)
	tracePath := fs.String("trace", "",
		"Path to JSON Lines trace (see docs/design/trace-harness.md format). "+
			"Overrides --zipf when set.")
	zipf := fs.String("zipf", "",
		"Generate a synthetic Zipf trace: alpha,files,accesses. "+
			"Default '0.7,30,200' uses scripts/bench-hitrate.sh's default "+
			"parameters, but Go's RNG differs from the script's Python "+
			"generator, so the sequence is NOT the bench's — pass the "+
			"bench's $OUT/trace.jsonl via --trace for a like-for-like reference. "+
			"Ignored when --trace is set.")
	policies := fs.String("policies", "lru,sieve,opt,noevict",
		"Comma-separated policy names to run in order. Available: "+
			strings.Join(availablePolicies(), ","))
	softcap := fs.Int("softcap", 8,
		"Cache capacity in admits — matches kmod's S3FS_EVICT_SOFTCAP.")
	seed := fs.Int64("seed", 42,
		"Zipf RNG seed. Same seed = same trace = identical bench across policies.")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr,
			"usage: s3fs-ebpf dev trace-replay [flags]\n\n"+
				"Offline replay of a workload trace against Go-side policy simulators.\n"+
				"Reports per-policy hit rate without loading kmod/BPF.\n\n"+
				"Flags:\n")
		fs.PrintDefaults()
		fmt.Fprintf(os.Stderr, "\nExamples:\n"+
			"  # Reference bounds for the trace a bench-hitrate.sh run used:\n"+
			"  s3fs-ebpf dev trace-replay --trace=/tmp/bench-hitrate/trace.jsonl\n\n"+
			"  # Replay a recorded JSONL trace against all policies:\n"+
			"  s3fs-ebpf dev trace-replay --trace=/tmp/prod.jsonl \\\n"+
			"      --policies=lru,sieve --softcap=32\n")
	}
	fs.Parse(args)

	events, err := loadEvents(*tracePath, *zipf, *seed)
	if err != nil {
		fmt.Fprintf(os.Stderr, "trace-replay: %v\n", err)
		return 1
	}
	if len(events) == 0 {
		fmt.Fprintf(os.Stderr, "trace-replay: empty trace\n")
		return 1
	}

	names := strings.Split(*policies, ",")
	fmt.Printf("# trace-replay: %d events, softcap=%d\n", len(events), *softcap)
	fmt.Printf("# %-10s %10s %10s %10s %8s\n",
		"policy", "accesses", "hits", "misses", "rate")
	for _, name := range names {
		name = strings.TrimSpace(name)
		p, err := makePolicy(name, *softcap, events)
		if err != nil {
			fmt.Fprintf(os.Stderr, "trace-replay: %v\n", err)
			return 1
		}
		r := trace.Replay(p, events)
		fmt.Printf("  %-10s %10d %10d %10d %7.2f%%\n",
			r.Policy, r.Accesses, r.Hits, r.Misses, 100*r.Rate)
	}
	return 0
}

func loadEvents(tracePath, zipfSpec string, seed int64) ([]trace.Event, error) {
	if tracePath != "" {
		f, err := os.Open(tracePath)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		return trace.Read(f)
	}
	// Default uses bench-hitrate.sh's parameters (α=0.7, 30 files,
	// 200 accesses); the sequence itself differs (Go vs Python RNG).
	spec := zipfSpec
	if spec == "" {
		spec = "0.7,30,200"
	}
	var alpha float64
	var files, n int
	if _, err := fmt.Sscanf(spec, "%f,%d,%d", &alpha, &files, &n); err != nil {
		return nil, fmt.Errorf("--zipf: expected 'alpha,files,accesses', got %q", spec)
	}
	return trace.GenerateZipf(files, n, alpha, seed), nil
}

func availablePolicies() []string {
	return []string{"lru", "sieve", "opt", "noevict"}
}

// makePolicy builds a simulator. "opt" (Belady) needs the whole trace
// up front; "noevict" ignores the capacity. Both are reference bounds,
// not candidate policies: a live hit rate at softcap=cap above "opt"
// means the bench is not measuring a capacity-cap cache.
func makePolicy(name string, cap int, events []trace.Event) (trace.Policy, error) {
	switch name {
	case "lru":
		return trace.NewLRU(cap), nil
	case "sieve":
		return trace.NewSIEVE(cap), nil
	case "opt":
		return trace.NewOPT(cap, events), nil
	case "noevict":
		return trace.NewNoEvict(), nil
	default:
		return nil, fmt.Errorf("unknown policy %q — available: %s",
			name, strings.Join(availablePolicies(), ","))
	}
}
