package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"time"
)

// runMicrobench measures pure syscall latency (stat/read) in a tight
// loop against a target path. Reports min / median / p95 / p99 / max
// so we can compare Fast Path VFS overhead against s3fs-fuse without
// hyperfine's fork/exec dominating the numbers.
func runMicrobench(args []string) int {
	fs := flag.NewFlagSet("microbench", flag.ExitOnError)
	op := fs.String("op", "stat", "syscall to measure: stat|read")
	path := fs.String("path", "", "target path")
	iters := fs.Int("iters", 100000, "iterations per run")
	warmup := fs.Int("warmup", 1000, "warmup iterations discarded")
	label := fs.String("label", "", "label to print with results")
	fs.Parse(args)

	if *path == "" {
		fmt.Fprintln(os.Stderr, "microbench: --path required")
		return 2
	}

	// Warmup.
	for i := 0; i < *warmup; i++ {
		mustOp(*op, *path)
	}

	samples := make([]time.Duration, *iters)
	start := time.Now()
	for i := 0; i < *iters; i++ {
		t0 := time.Now()
		mustOp(*op, *path)
		samples[i] = time.Since(t0)
	}
	total := time.Since(start)

	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	pct := func(p float64) time.Duration {
		idx := int(float64(len(samples)-1) * p)
		return samples[idx]
	}
	name := *label
	if name == "" {
		name = fmt.Sprintf("%s %s", *op, *path)
	}
	fmt.Printf("[microbench] %s\n", name)
	fmt.Printf("  iters=%d warmup=%d total=%v ops/s=%.0f\n",
		*iters, *warmup, total, float64(*iters)/total.Seconds())
	fmt.Printf("  min=%v  p50=%v  p95=%v  p99=%v  max=%v\n",
		samples[0], pct(0.50), pct(0.95), pct(0.99), samples[len(samples)-1])
	return 0
}

func mustOp(op, path string) {
	switch op {
	case "stat":
		_, err := os.Stat(path)
		if err != nil {
			panic(err)
		}
	case "read":
		f, err := os.Open(path)
		if err != nil {
			panic(err)
		}
		_, err = io.Copy(io.Discard, f)
		f.Close()
		if err != nil {
			panic(err)
		}
	default:
		panic("unknown op: " + op)
	}
}
