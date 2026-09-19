// Package metrics exposes a Prometheus scrape endpoint on :9600.
// Week 0 uses net/http and a hand-rolled text exposition body so the
// build stays dep-free; Week 1 will migrate to prometheus/client_golang
// once real counters exist.
package metrics

import (
	"fmt"
	"net/http"
	"sync/atomic"
)

// Counters holds the atomic values the fast path bumps.
type Counters struct {
	FastPathHits   atomic.Uint64
	SlowPathMisses atomic.Uint64
}

// Global is a package-level sink so any component can bump counters
// without threading a handle. Replaced with proper registry in Week 1.
var Global Counters

// Serve starts the metrics HTTP endpoint. Blocks until the server exits.
func Serve(addr string) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", handle)
	return http.ListenAndServe(addr, mux)
}

func handle(w http.ResponseWriter, _ *http.Request) {
	fmt.Fprintf(w, "# HELP s3fs_ebpf_fast_path_hits_total Fast-path (KFunc) cache hits.\n")
	fmt.Fprintf(w, "# TYPE s3fs_ebpf_fast_path_hits_total counter\n")
	fmt.Fprintf(w, "s3fs_ebpf_fast_path_hits_total %d\n", Global.FastPathHits.Load())
	fmt.Fprintf(w, "# HELP s3fs_ebpf_slow_path_misses_total Slow-path (S3 API) misses.\n")
	fmt.Fprintf(w, "# TYPE s3fs_ebpf_slow_path_misses_total counter\n")
	fmt.Fprintf(w, "s3fs_ebpf_slow_path_misses_total %d\n", Global.SlowPathMisses.Load())
}
