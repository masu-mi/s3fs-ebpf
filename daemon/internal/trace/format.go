// Package trace defines the workload trace format used by
// `s3fs-ebpf trace-replay`. A trace is a sequence of read/write
// events; a replayer drives a policy simulator with the sequence
// and reports per-policy hit rate — no kmod, no mount, no S3.
//
// Format: JSON Lines. Each line is one Event. Blank lines and lines
// starting with `#` are ignored so operators can annotate traces
// by hand.
//
//	{"ts":0,"ino":100,"op":"read"}
//	{"ts":1200,"ino":101,"op":"read"}
//	{"ts":2100,"ino":100,"op":"read"}
//
// Fields:
//
//	ts    monotonic nanoseconds from trace start (0 for the first
//	      event). Simulator ignores it today but reserved so future
//	      TTL / time-decay policies have a wall-clock signal.
//	ino   inode number the workload touched. Simulator uses this as
//	      the cache key.
//	op    "read" (miss on empty cache), "write" (invalidates the
//	      entry). Extend with "delete" later if needed.
//
// Optional range fields (docs/design/ann-workload-measurement-plan.md
// P0). Absent (len == 0) means the legacy whole-file event, and the
// event serialises exactly as before:
//
//	off, len  byte range read inside ino. ChunkReplay maps it to
//	          (ino, off/chunk) cache keys.
//	qid       query id (>0) for per-query metrics; 0 = not a query.
//	step      dependency step inside a query. Reads of one (qid, step)
//	          are issued in parallel; steps are serial.
//	tag       free-form read class ("centroid", "cluster", ...) for
//	          per-class hit rates.

// The recorder is intentionally not implemented yet — kmod-side
// ring buffer + drain ioctl is P-04 followup. For now the replay
// harness drives synthetic Zipf traces so we can compare policies
// against the same numeric shape as bench-hitrate.sh.
package trace

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/rand"
	"strings"
)

// Op is a workload operation. Small, fixed-set enum expressed as a
// string so JSON traces are human-readable.
type Op string

const (
	OpRead  Op = "read"
	OpWrite Op = "write"
)

// Event is one line of a trace file.
type Event struct {
	Ts  uint64 `json:"ts"`
	Ino uint64 `json:"ino"`
	Op  Op     `json:"op"`

	Off  uint64 `json:"off,omitempty"`
	Len  uint64 `json:"len,omitempty"`
	Qid  uint64 `json:"qid,omitempty"`
	Step uint32 `json:"step,omitempty"`
	Tag  string `json:"tag,omitempty"`
}

// Read parses trace events from r. Returns the events in order.
// Errors on malformed JSON — a partial trace is better than silently
// dropping lines.
func Read(r io.Reader) ([]Event, error) {
	var out []Event
	scan := bufio.NewScanner(r)
	// Traces can be big — bump the max line size from the 64 KiB
	// default. 1 MiB per line is enough for annotations even with
	// verbose comments.
	scan.Buffer(make([]byte, 0, 64*1024), 1<<20)
	line := 0
	for scan.Scan() {
		line++
		s := strings.TrimSpace(scan.Text())
		if s == "" || strings.HasPrefix(s, "#") {
			continue
		}
		var e Event
		if err := json.Unmarshal([]byte(s), &e); err != nil {
			return nil, fmt.Errorf("trace: line %d: %w", line, err)
		}
		if e.Op == "" {
			e.Op = OpRead // default
		}
		out = append(out, e)
	}
	if err := scan.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// Write emits events to w in JSON Lines format. Ts differences are
// preserved but the base is the caller's problem (usually 0).
func Write(w io.Writer, events []Event) error {
	bw := bufio.NewWriter(w)
	defer bw.Flush()
	enc := json.NewEncoder(bw)
	for _, e := range events {
		if err := enc.Encode(e); err != nil {
			return err
		}
	}
	return nil
}

// GenerateZipf produces `n` read events over `files` distinct inos,
// sampled from Zipf(alpha). Fixed random seed for reproducibility —
// same params → same trace.
//
// The Zipf distribution here uses the classic weight w_i ∝ 1/(i+1)^α
// (ranks starting at 1), the same shape as bench-hitrate.sh. The
// sequence is NOT the bench's: math/rand and Python's random produce
// different streams for seed=42. For a reference on the exact bench
// trace, replay the $OUT/trace.jsonl that bench-hitrate.sh writes.
func GenerateZipf(files int, n int, alpha float64, seed int64) []Event {
	if files < 1 {
		return nil
	}
	// Precompute CDF over inos [inoBase, inoBase+files).
	weights := make([]float64, files)
	total := 0.0
	for i := 0; i < files; i++ {
		weights[i] = 1.0 / math.Pow(float64(i+1), alpha)
		total += weights[i]
	}
	cdf := make([]float64, files)
	acc := 0.0
	for i, w := range weights {
		acc += w / total
		cdf[i] = acc
	}

	rng := rand.New(rand.NewSource(seed))
	// Match bench-hitrate.sh: inos start at 100 + files_offset.
	// Simulator only cares about ino uniqueness so we pick 100..
	const inoBase uint64 = 100
	out := make([]Event, n)
	for i := 0; i < n; i++ {
		r := rng.Float64()
		// Binary-search the CDF.
		lo, hi := 0, files-1
		for lo < hi {
			mid := (lo + hi) / 2
			if cdf[mid] < r {
				lo = mid + 1
			} else {
				hi = mid
			}
		}
		out[i] = Event{
			Ts:  uint64(i * 1_000_000), // 1 ms spacing, synthetic
			Ino: inoBase + uint64(lo),
			Op:  OpRead,
		}
	}
	return out
}
