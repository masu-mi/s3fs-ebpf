package metrics

import (
	"fmt"
	"io"
	"runtime"
	"runtime/pprof"
)

// writeRuntime emits Go-runtime health as Prometheus text: goroutine
// count, heap footprint, GC pauses. Naming matches client_golang's
// go_* series so any stock Grafana dashboard that consumes
// go_goroutines / go_memstats_* works against this endpoint without
// changes.
//
// J-02: called from handle() after the FS-specific counters so a
// spike in AdmitsCurrent / FlushErrors correlates with heap growth
// or GC pause length on the same scrape row.
//
// Cost: runtime.ReadMemStats briefly halts the collector goroutines
// (μs on healthy heaps, low-ms under pressure). At 5s scrape interval
// this is negligible; if a future load makes it a hotspot the fix is
// to sample on a ticker and cache — the interface here stays the same.
func writeRuntime(w io.Writer) {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)

	gauge(w, "go_goroutines",
		"Currently active goroutines (runtime.NumGoroutine).",
		float64(runtime.NumGoroutine()))
	gauge(w, "go_threads",
		"OS threads created by the Go runtime for this process.",
		float64(numOSThreads()))
	gauge(w, "go_memstats_alloc_bytes",
		"Bytes of allocated heap objects still in use.",
		float64(m.Alloc))
	gauge(w, "go_memstats_sys_bytes",
		"Bytes obtained from the OS across all heap/stack/other subsystems.",
		float64(m.Sys))
	gauge(w, "go_memstats_heap_objects",
		"Count of allocated heap objects.",
		float64(m.HeapObjects))
	gauge(w, "go_memstats_next_gc_bytes",
		"Heap size that will trigger the next GC (m.NextGC).",
		float64(m.NextGC))

	counter(w, "go_gc_runs_total",
		"Total completed GC cycles since process start (m.NumGC).",
		float64(m.NumGC))
	counter(w, "go_gc_pause_seconds_total",
		"Cumulative stop-the-world GC pause across all cycles.",
		float64(m.PauseTotalNs)/1e9)

	// Most recent pause: useful for alerting on individual jank.
	// m.PauseNs is a ring buffer of size 256; the newest entry is at
	// (NumGC-1) mod 256 (Go's own convention).
	if m.NumGC > 0 {
		gauge(w, "go_gc_pause_last_seconds",
			"Duration of the most recent GC stop-the-world pause.",
			float64(m.PauseNs[(m.NumGC+255)%256])/1e9)
	}
}

// numOSThreads returns the number of OS threads the Go runtime has
// created. There is no direct runtime API; pprof's "threadcreate"
// profile count is maintained by the runtime regardless of whether
// pprof is actively sampling, so it's a cheap approximation.
func numOSThreads() int {
	p := pprof.Lookup("threadcreate")
	if p == nil {
		return 0
	}
	return p.Count()
}

// gauge / counter emit one Prometheus text-format metric line.
// Extracted so runtime.go and any future site can share the exact
// formatting (HELP + TYPE + name value newline) without copy-paste.
func gauge(w io.Writer, name, help string, v float64) {
	writeSample(w, name, help, "gauge", v)
}

func counter(w io.Writer, name, help string, v float64) {
	writeSample(w, name, help, "counter", v)
}

func writeSample(w io.Writer, name, help, kind string, v float64) {
	fmt.Fprintf(w, "# HELP %s %s\n", name, help)
	fmt.Fprintf(w, "# TYPE %s %s\n", name, kind)
	fmt.Fprintf(w, "%s %s\n", name, formatFloat(v))
}
