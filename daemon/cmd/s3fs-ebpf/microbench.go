package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"
)

// runMicrobench measures pure syscall latency (stat/read) in a tight
// loop against a target path. Reports min / median / p95 / p99 / max
// so we can compare Fast Path VFS overhead against s3fs-fuse without
// hyperfine's fork/exec dominating the numbers.
func runMicrobench(args []string) int {
	fs := flag.NewFlagSet("microbench", flag.ExitOnError)
	op := fs.String("op", "stat", "syscall to measure: stat|read")
	path := fs.String("path", "", "target path; a %d is replaced by the running operation number (warmup included, from 0), so each operation touches a different file")
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
		if err := doOp(*op, benchPath(*path, i)); err != nil {
			fmt.Fprintf(os.Stderr, "microbench: warmup #%d: %v\n", i, err)
			return 1
		}
	}

	samples := make([]time.Duration, *iters)
	start := time.Now()
	for i := 0; i < *iters; i++ {
		t0 := time.Now()
		if err := doOp(*op, benchPath(*path, *warmup+i)); err != nil {
			// Bench abort (e.g. mount removed mid-run): print partial
			// stats + non-zero exit instead of a Go panic that dumps a
			// stack trace and confuses bench harness output.
			fmt.Fprintf(os.Stderr, "microbench: iter %d/%d aborted: %v\n",
				i, *iters, err)
			samples = samples[:i]
			break
		}
		samples[i] = time.Since(t0)
	}
	total := time.Since(start)
	if len(samples) == 0 {
		fmt.Fprintln(os.Stderr, "microbench: no samples collected")
		return 1
	}

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

// benchPath expands a %d in the path pattern to n.
func benchPath(pattern string, n int) string {
	if !strings.Contains(pattern, "%d") {
		return pattern
	}
	return fmt.Sprintf(pattern, n)
}

func doOp(op, path string) error {
	switch op {
	case "stat":
		_, err := os.Stat(path)
		return err
	case "read":
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		_, err = io.Copy(io.Discard, f)
		f.Close()
		return err
	default:
		return fmt.Errorf("unknown op: %s", op)
	}
}
