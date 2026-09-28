package metrics

import (
	"fmt"
	"io"
	"math"
	"sync/atomic"
	"time"
)

// Histogram is a lock-free Prometheus-style histogram. Buckets are
// cumulative (Prometheus convention). Observations that fall into
// bucket i also count for buckets i+1..N-1.
//
// Storage: one atomic.Uint64 per bucket boundary + total count + sum
// (in nanoseconds, tracked as uint64 for atomic ops).
//
// This is a minimal hand-rolled implementation — no labels, no reset,
// no OpenMetrics extensions. Sufficient for the read-miss latency
// panel; upgrade to client_golang when we need label cardinality.
type Histogram struct {
	name    string
	help    string
	bounds  []float64 // seconds, ascending, must exclude +Inf
	buckets []atomic.Uint64
	count   atomic.Uint64
	sumNs   atomic.Uint64
}

// NewHistogram creates a histogram with the given bucket upper
// bounds. The final +Inf bucket is implicit (equals total count).
// Bounds must be strictly increasing seconds.
func NewHistogram(name, help string, bounds []float64) *Histogram {
	h := &Histogram{
		name:    name,
		help:    help,
		bounds:  append([]float64(nil), bounds...),
		buckets: make([]atomic.Uint64, len(bounds)),
	}
	return h
}

// Observe records a duration. Cheap enough to call on every event —
// binary-search-free linear scan since bounds are small (<20).
func (h *Histogram) Observe(d time.Duration) {
	sec := d.Seconds()
	for i, b := range h.bounds {
		if sec <= b {
			h.buckets[i].Add(1)
			// Fall through: Prometheus buckets are cumulative,
			// so any observation in bucket i also increments
			// i+1..N-1 (le is "less than or equal to").
			for j := i + 1; j < len(h.bounds); j++ {
				h.buckets[j].Add(1)
			}
			break
		}
	}
	// +Inf bucket is implicit — represented by total count below.
	h.count.Add(1)
	// Sum in nanoseconds. Approximate for very large observations
	// (~2^63 ns ≈ 292 years), so overflow isn't a concern.
	h.sumNs.Add(uint64(d.Nanoseconds()))
}

// writeTo emits the histogram in Prometheus text exposition format:
//
//	# HELP name help
//	# TYPE name histogram
//	name_bucket{le="B0"} N0
//	name_bucket{le="B1"} N1
//	...
//	name_bucket{le="+Inf"} count
//	name_sum sum_seconds
//	name_count count
func (h *Histogram) writeTo(w io.Writer) {
	fmt.Fprintf(w, "# HELP %s %s\n", h.name, h.help)
	fmt.Fprintf(w, "# TYPE %s histogram\n", h.name)
	for i, b := range h.bounds {
		fmt.Fprintf(w, "%s_bucket{le=\"%s\"} %d\n",
			h.name, formatBound(b), h.buckets[i].Load())
	}
	total := h.count.Load()
	fmt.Fprintf(w, "%s_bucket{le=\"+Inf\"} %d\n", h.name, total)
	fmt.Fprintf(w, "%s_sum %s\n", h.name,
		formatBound(float64(h.sumNs.Load())/1e9))
	fmt.Fprintf(w, "%s_count %d\n", h.name, total)
}

// formatBound prints a float in the compact form Prometheus expects
// ("0.005", "0.01", "1", "+Inf"). Avoids trailing zeros that break
// the text exposition parser in some tools.
func formatBound(v float64) string {
	if math.IsInf(v, 1) {
		return "+Inf"
	}
	// %g is the closest match — up to 6 significant digits, drops
	// trailing zeros. Prometheus accepts this.
	return fmt.Sprintf("%g", v)
}

// Default bucket set for the read-miss latency histogram. Covers
// MinIO-local (1-5 ms) through S3-cold-object (up to seconds).
var DefaultLatencyBounds = []float64{
	0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10,
}

// ReadMissLatency records daemon-side upcall service time — from
// receiving a miss event to sending signalAdmit back. Includes S3
// Get, arena write, and admit ioctl. Registered on the /metrics
// endpoint alongside the counters/gauges.
var ReadMissLatency = NewHistogram(
	"s3fs_ebpf_read_miss_latency_seconds",
	"Wall-clock time from receiving a read-miss upcall to signalAdmit.",
	DefaultLatencyBounds,
)
