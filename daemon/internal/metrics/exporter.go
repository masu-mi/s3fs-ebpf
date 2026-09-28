// Package metrics exposes a Prometheus scrape endpoint on :9600.
// Hand-rolled text exposition (no client_golang dependency) — the
// metric set is small enough that a switch to client_golang is not
// worth pulling in the transitive dep tree at PoC scope. Migrate
// when we need histograms or per-label cardinality.
package metrics

import (
	"fmt"
	"net/http"
	"sync/atomic"
)

// Counters holds the atomic values components bump.
//
// Two shapes coexist:
//   - *counter* values are monotonically increasing (`_total` suffix).
//     Prometheus derives rate() from them.
//   - *gauge* values reflect a current-state snapshot (queue depth,
//     bytes in use, table sizes). Written with Store(), no Add().
type Counters struct {
	// BPF-side fast path counters. Populated from the per-CPU map
	// snapshot each sample tick — Store the absolute value rather
	// than incrementing, since the map is authoritative.
	FastPathHits   atomic.Uint64 // counter
	SlowPathMisses atomic.Uint64 // counter
	UnknownFS      atomic.Uint64 // counter

	// Week 4 (a) C: on-demand read-miss upcall handled by the daemon.
	// Bumped from admit_arena.go:handleReadMiss.
	ReadMissUpcalls  atomic.Uint64 // counter
	ReadMissErrors   atomic.Uint64 // counter (S3 Get failed, admit failed, etc.)

	// Week 4 (b): dynamic bucket sync loop.
	SyncRounds         atomic.Uint64 // counter, bumped per successful sync
	SyncErrors         atomic.Uint64 // counter
	SyncEntriesCurrent atomic.Uint64 // gauge, len(entries) after last sync

	// Week 4 (c): write-back flush.
	FlushEvents atomic.Uint64 // counter, one per POP_FLUSH drained
	FlushBytes  atomic.Uint64 // counter, bytes PUT to S3
	FlushErrors atomic.Uint64 // counter, PUT failures

	// admit table (arena content admission) — daemon-owned snapshot.
	AdmitsCurrent atomic.Uint64 // gauge, len(admits) after last replace

	// arena backing store.
	ArenaCapacityBytes atomic.Uint64 // gauge, arena.Size() (constant per run)
	ArenaUsedBytes     atomic.Uint64 // gauge, high-water AdmitContent offset

	// Layer 2/3 policy program (BPF fentry on s3fs_policy_on_admit).
	// Sourced from the loaded .bpf.o's policy_stats map on every
	// sample tick. Zero when no --policy-bpf is active.
	PolicyHookInvocations atomic.Uint64 // counter
	PolicyLastAdmitCount  atomic.Uint64 // gauge, last admit_replace snapshot count

	// Cumulative admit-table evictions driven by the policy program
	// (fmod_ret / struct_ops vote → kmod hash_del). Sourced from
	// S3FS_IOC_STATS[0].
	PolicyEvictionsTotal atomic.Uint64 // counter
}

// Global is a package-level sink so any component can bump counters
// without threading a handle. Bumped from goroutines; atomics only.
var Global Counters

// ReloadHandler is the daemon-supplied POST /policy/reload handler
// (registered by fastpath at startup). Left nil when no policy engine
// is active; the endpoint returns 503.
var ReloadHandler http.HandlerFunc

// Serve starts the metrics HTTP endpoint. Blocks until the server exits.
// Callers should invoke this on a goroutine.
func Serve(addr string) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", handle)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("/policy/reload", func(w http.ResponseWriter, r *http.Request) {
		if ReloadHandler == nil {
			http.Error(w, "policy engine not active", http.StatusServiceUnavailable)
			return
		}
		ReloadHandler(w, r)
	})
	return http.ListenAndServe(addr, mux)
}

type metricDesc struct {
	name  string
	help  string
	mtype string // "counter" | "gauge"
	value uint64
}

func handle(w http.ResponseWriter, _ *http.Request) {
	metrics := []metricDesc{
		// BPF-side (fentry counters).
		{"s3fs_ebpf_fast_path_hits_total", "Fast-path (fentry admit-hit) VFS getattr count.", "counter", Global.FastPathHits.Load()},
		{"s3fs_ebpf_slow_path_misses_total", "Slow-path (fentry admit-miss) VFS getattr count.", "counter", Global.SlowPathMisses.Load()},
		{"s3fs_ebpf_unknown_fs_total", "Non-s3fs_ebpf superblock touches (noise from other FS).", "counter", Global.UnknownFS.Load()},

		// Path C read-miss upcalls.
		{"s3fs_ebpf_read_miss_upcalls_total", "Read-miss upcalls received by daemon.", "counter", Global.ReadMissUpcalls.Load()},
		{"s3fs_ebpf_read_miss_errors_total", "Read-miss upcalls that failed (S3 Get / admit).", "counter", Global.ReadMissErrors.Load()},

		// Sync loop.
		{"s3fs_ebpf_sync_rounds_total", "Successful bucket-sync rounds.", "counter", Global.SyncRounds.Load()},
		{"s3fs_ebpf_sync_errors_total", "Failed bucket-sync attempts.", "counter", Global.SyncErrors.Load()},
		{"s3fs_ebpf_sync_entries_current", "Entries in the dynamic table after the last sync.", "gauge", Global.SyncEntriesCurrent.Load()},

		// Write-back flush.
		{"s3fs_ebpf_flush_events_total", "Flush events drained from the kmod queue.", "counter", Global.FlushEvents.Load()},
		{"s3fs_ebpf_flush_bytes_total", "Bytes PUT to S3 by the flush loop.", "counter", Global.FlushBytes.Load()},
		{"s3fs_ebpf_flush_errors_total", "PUT failures during flush.", "counter", Global.FlushErrors.Load()},

		// Admit table / arena.
		{"s3fs_ebpf_admits_current", "Entries in the daemon-side admit snapshot.", "gauge", Global.AdmitsCurrent.Load()},
		{"s3fs_ebpf_arena_capacity_bytes", "BPF arena mmap size (constant per run).", "gauge", Global.ArenaCapacityBytes.Load()},
		{"s3fs_ebpf_arena_used_bytes", "High-water AdmitContent offset — best-effort arena usage.", "gauge", Global.ArenaUsedBytes.Load()},

		// Layer 2/3 policy program hook counters.
		{"s3fs_ebpf_policy_hook_invocations_total", "fentry invocations on s3fs_policy_on_admit (counted by the loaded policy BPF program).", "counter", Global.PolicyHookInvocations.Load()},
		{"s3fs_ebpf_policy_last_admit_count", "Snapshot count from the last admit_replace, as recorded by the policy BPF program.", "gauge", Global.PolicyLastAdmitCount.Load()},
		{"s3fs_ebpf_policy_evictions_total", "Admit-table entries removed by policy eviction votes (kmod-side hash_del).", "counter", Global.PolicyEvictionsTotal.Load()},
	}
	for _, m := range metrics {
		fmt.Fprintf(w, "# HELP %s %s\n", m.name, m.help)
		fmt.Fprintf(w, "# TYPE %s %s\n", m.name, m.mtype)
		fmt.Fprintf(w, "%s %d\n", m.name, m.value)
	}
}
