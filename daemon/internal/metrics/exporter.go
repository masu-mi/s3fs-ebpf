// Package metrics exposes a Prometheus scrape endpoint on :9600.
// Hand-rolled text exposition (no client_golang dependency) —
// counters/gauges live in the Counters struct below, histograms in
// histogram.go. Both render in this file's handle(). Migrate to
// client_golang when we need labeled cardinality or exemplars; the
// current shape is enough for PoC observability.
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

	// E-03 flush coalescer.
	FlushCoalesced      atomic.Uint64 // counter, submitted to debounce
	FlushCoalescedFired atomic.Uint64 // counter, PUTs issued (should be < Coalesced when bursts hit)

	// admit table (arena content admission) — daemon-owned snapshot.
	AdmitsCurrent atomic.Uint64 // gauge, len(admits) after last replace

	// arena backing store.
	ArenaCapacityBytes atomic.Uint64 // gauge, arena.Size() (constant per run)
	ArenaUsedBytes     atomic.Uint64 // gauge, high-water AdmitContent offset
	ArenaFreeBytes     atomic.Uint64 // gauge, D-02 free-list total
	ArenaFreeChunks    atomic.Uint64 // gauge, D-02 free-list length

	// Layer 2/3 policy program (BPF fentry on s3fs_policy_on_admit).
	// Sourced from the loaded .bpf.o's policy_stats map on every
	// sample tick. Zero when no --policy-bpf is active.
	PolicyHookInvocations atomic.Uint64 // counter
	PolicyLastAdmitCount  atomic.Uint64 // gauge, last admit_replace snapshot count

	// Cumulative admit-table evictions driven by the policy program
	// (fmod_ret / struct_ops vote → kmod hash_del). Sourced from
	// S3FS_IOC_STATS[0].
	PolicyEvictionsTotal atomic.Uint64 // counter

	// Z-04: pressure_ops strict/lenient branch fire counters.
	// Only non-zero when policy_pressure_ops is the loaded policy.
	// Strict: visited=1 AND high-pressure → evict (skip second chance).
	// Lenient: visited=1 AND normal pressure → keep (standard SIEVE clear).
	PolicyPressureStrictTotal  atomic.Uint64 // counter
	PolicyPressureLenientTotal atomic.Uint64 // counter

	// kmod-side .read_folio arena hits — the "true" cache hit count
	// (as opposed to fentry-side FastPathHits which measures getattr
	// hits on the BPF-side inode_index). Sourced from
	// S3FS_IOC_STATS[1]. Divide by (this + ReadMissUpcalls) for the
	// real kmod cache hit ratio.
	FolioHits atomic.Uint64 // counter

	// AdmitInvalidations: page-cache invalidations admit_replace did
	// for new or moved ranges (S3FS_IOC_STATS[2], S-01). Should track
	// real content changes, not the number of full-list re-sends.
	AdmitInvalidations atomic.Uint64 // counter

	// AB-01: GUP (pin_user_pages_remote) call count and accumulated
	// latency from s3fs_arena_copy_to_iter. Sourced from
	// S3FS_IOC_STATS[3]/[4]. bench-ab-arena-vs-pagecache.sh reads
	// these to compute mean GUP ns/call without bpftrace.
	GupCallsTotal atomic.Uint64 // counter
	GupNsTotal    atomic.Uint64 // counter

	// T4: .read_folio calls that exhausted S3FS_UPCALL_MAX_RETRIES with
	// the daemon alive (capacity eviction won every retry window).
	// Sourced from S3FS_IOC_STATS[5]. Non-zero in the evict-race report
	// means -EIO from capacity pressure, not data corruption.
	UpcallRetryExhausted atomic.Uint64 // counter

	// U11: upcall waits that timed out (remaining==0) before daemon signal.
	// Caused by goroutine-pool back-pressure in runReadMissLoop.
	// Sourced from S3FS_IOC_STATS[6].
	UpcallTimeouts atomic.Uint64 // counter

	// D-diag: gen=0 fallback failure reason breakdown.
	// Sourced from S3FS_IOC_STATS[7/8/9].
	Gen0FailBufNull atomic.Uint64 // counter — buf==NULL
	Gen0FailBitmap  atomic.Uint64 // counter — bitmap bit not set
	Gen0FailOther   atomic.Uint64 // counter — pos/ci range

	// M1: kmod fd->buf memory vs capacity_bytes.
	FdBufBytes   atomic.Uint64 // gauge, bytes allocated for fd->buf
	ResetSkipped atomic.Uint64 // counter, resets that deferred the buf free

	// M2: S3FS_IOC_STATS[12/13]. Walk = sum of fd->cap over live inodes
	// (must equal FdBufBytes at rest); Pcache = what the AC-41 capacity
	// loop charges (sum of fd->cap over distinct pcache inos).
	FdBufWalkBytes   atomic.Uint64 // gauge
	FdBufPcacheBytes atomic.Uint64 // gauge

	// R-01: fentry/generic_file_read_iter counters. ReadIterHits counts
	// reads where the ino was already in inode_index (prewarm hit);
	// ReadIterMisses counts reads where the ino was unknown (triggers
	// a daemon upcall via the events ringbuf, op=S3FS_OP_READ).
	ReadIterHits   atomic.Uint64 // counter
	ReadIterMisses atomic.Uint64 // counter
	// Y-06: subset of ReadIterHits where ki_pos==0 (first read per
	// file open). ReadIterHits/ReadIterHitsPos0 ≈ 2 confirms the
	// data+EOF double-fire that inflated rate_total before Y-01.
	ReadIterHitsPos0 atomic.Uint64 // counter
}

// Global is a package-level sink so any component can bump counters
// without threading a handle. Bumped from goroutines; atomics only.
var Global Counters

// ReloadHandler is the daemon-supplied POST /policy/reload handler
// (registered by fastpath at startup). Left nil when no policy engine
// is active; the endpoint returns 503.
var ReloadHandler http.HandlerFunc

// EvalHandler is the daemon-supplied POST /policy/eval handler. It
// forces a fresh admit_replace so the loaded policy program runs
// another eviction round even when steady state (no admissions, no
// evictions) would otherwise idle. Left nil when policy engine is
// inactive.
var EvalHandler http.HandlerFunc

// makeAuthWrap returns a middleware factory that enforces Bearer token auth.
// When token is empty the returned wrapper is a no-op so unauthenticated
// deployments (no --metrics-token flag) continue to work unchanged.
func makeAuthWrap(token string) func(http.HandlerFunc) http.HandlerFunc {
	if token == "" {
		return func(h http.HandlerFunc) http.HandlerFunc { return h }
	}
	want := "Bearer " + token
	return func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != want {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			h(w, r)
		}
	}
}

// Serve starts the metrics HTTP endpoint. Blocks until the server exits.
// Callers should invoke this on a goroutine.
//
// When token is non-empty, /policy/reload and /policy/eval require the
// caller to present "Authorization: Bearer <token>". /metrics and /healthz
// remain unauthenticated so Prometheus can scrape without credentials.
func Serve(addr, token string) error {
	authWrap := makeAuthWrap(token)

	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", handle)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("/policy/reload", authWrap(func(w http.ResponseWriter, r *http.Request) {
		if ReloadHandler == nil {
			http.Error(w, "policy engine not active", http.StatusServiceUnavailable)
			return
		}
		ReloadHandler(w, r)
	}))
	mux.HandleFunc("/policy/eval", authWrap(func(w http.ResponseWriter, r *http.Request) {
		if EvalHandler == nil {
			http.Error(w, "policy engine not active", http.StatusServiceUnavailable)
			return
		}
		EvalHandler(w, r)
	}))
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

		// E-03: coalescer counters.
		{"s3fs_ebpf_flush_coalesced_total", "Flush events accepted by the debounce coalescer (excludes fsync FLUSH_WAIT bypass).", "counter", Global.FlushCoalesced.Load()},
		{"s3fs_ebpf_flush_coalesced_fired_total", "Coalesced flush events that debounced through to a PUT. Coalesced - Fired = 'PUTs saved by coalescing'.", "counter", Global.FlushCoalescedFired.Load()},

		// Admit table / arena.
		{"s3fs_ebpf_admits_current", "Entries in the daemon-side admit snapshot.", "gauge", Global.AdmitsCurrent.Load()},
		{"s3fs_ebpf_arena_capacity_bytes", "BPF arena mmap size (constant per run).", "gauge", Global.ArenaCapacityBytes.Load()},
		{"s3fs_ebpf_arena_used_bytes", "High-water AdmitContent offset — best-effort arena usage.", "gauge", Global.ArenaUsedBytes.Load()},
		{"s3fs_ebpf_arena_free_bytes", "Bytes in the arena free-list (D-02 — evicted admits return here for reuse).", "gauge", Global.ArenaFreeBytes.Load()},
		{"s3fs_ebpf_arena_free_chunks", "Number of ranges in the arena free-list.", "gauge", Global.ArenaFreeChunks.Load()},

		// Layer 2/3 policy program hook counters.
		{"s3fs_ebpf_policy_hook_invocations_total", "fentry invocations on s3fs_policy_on_admit (counted by the loaded policy BPF program).", "counter", Global.PolicyHookInvocations.Load()},
		{"s3fs_ebpf_policy_last_admit_count", "Snapshot count from the last admit_replace, as recorded by the policy BPF program.", "gauge", Global.PolicyLastAdmitCount.Load()},
		{"s3fs_ebpf_policy_evictions_total", "Admit-table entries removed by policy eviction votes (kmod-side hash_del).", "counter", Global.PolicyEvictionsTotal.Load()},
		// Z-04: pressure_ops diagnostic counters (0 when a different policy is loaded).
		{"s3fs_ebpf_policy_pressure_strict_total", "pressure_ops: on_evict took strict path (visited=1, high-pressure → evict). Z-04.", "counter", Global.PolicyPressureStrictTotal.Load()},
		{"s3fs_ebpf_policy_pressure_lenient_total", "pressure_ops: on_evict took lenient path (visited=1, normal pressure → keep). Z-04.", "counter", Global.PolicyPressureLenientTotal.Load()},

		// True kmod-side cache hit — read_folio served from arena.
		{"s3fs_ebpf_folio_hits_total", "kmod-side .read_folio calls served from arena (true cache hit). Divide by (this + read_miss_upcalls_total) for hit ratio.", "counter", Global.FolioHits.Load()},
		{"s3fs_ebpf_admit_invalidations_total", "Page-cache invalidations by admit_replace for new or moved admit ranges (unchanged ranges keep their page cache).", "counter", Global.AdmitInvalidations.Load()},

		// AB-01: GUP latency diagnostics from s3fs_arena_copy_to_iter.
		// gup_calls_total / gup_ns_total → mean ns/call. 0 when arena_bypass=1.
		{"s3fs_ebpf_gup_calls_total", "AB-01: pin_user_pages_remote calls in s3fs_arena_copy_to_iter (1 per page per arena read).", "counter", Global.GupCallsTotal.Load()},
		{"s3fs_ebpf_gup_ns_total", "AB-01: accumulated pin_user_pages_remote latency in nanoseconds. Divide by gup_calls_total for mean ns/call.", "counter", Global.GupNsTotal.Load()},

		// T4: capacity-pressure -EIO distinguisher. Non-zero means eviction
		// outpaced the retry loop (capacity=1 chunk + many readers). Incremented
		// when S3FS_UPCALL_MAX_RETRIES were exhausted with the daemon alive.
		{"s3fs_ebpf_upcall_retry_exhausted_total", "T4: .read_folio calls where S3FS_UPCALL_MAX_RETRIES were exhausted (daemon alive but eviction won every retry window). Non-zero means -EIO from capacity pressure, not data corruption.", "counter", Global.UpcallRetryExhausted.Load()},
		{"s3fs_ebpf_upcall_timeout_total", "U11: .read_folio upcall waits that timed out before daemon signal (goroutine-pool back-pressure). U11 retries these instead of falling to gen=0 with NULL buf.", "counter", Global.UpcallTimeouts.Load()},
		{"s3fs_ebpf_gen0_fail_buf_null_total", "D-diag: gen=0 fallback failures where fd->buf==NULL (never populated or eviction cleared buf before U10 set buf_readers).", "counter", Global.Gen0FailBufNull.Load()},
		{"s3fs_ebpf_gen0_fail_bitmap_total", "D-diag: gen=0 fallback failures where bitmap bit not set (chunk not yet populated in fd->buf).", "counter", Global.Gen0FailBitmap.Load()},
		{"s3fs_ebpf_gen0_fail_other_total", "D-diag: gen=0 fallback failures for other reasons (pos >= fd->size or ci >= 64).", "counter", Global.Gen0FailOther.Load()},
		{"s3fs_ebpf_fd_buf_bytes", "M1: bytes allocated for kmod per-inode fd->buf. capacity_bytes bounds admit entries only; compare against it to spot buffers outliving their admit.", "gauge", Global.FdBufBytes.Load()},
		{"s3fs_ebpf_fd_buf_walk_bytes", "M2: sum of fd->cap over live inodes, recomputed per poll. Differs from s3fs_ebpf_fd_buf_bytes at rest only if the gauge drifted.", "gauge", Global.FdBufWalkBytes.Load()},
		{"s3fs_ebpf_fd_buf_pcache_bytes", "M2: fd->buf bytes the capacity loop charges (sum of fd->cap over distinct pcache inos).", "gauge", Global.FdBufPcacheBytes.Load()},
		{"s3fs_ebpf_reset_skipped_total", "M1: fd->buf frees deferred because buf_readers > 0 (U8/U10).", "counter", Global.ResetSkipped.Load()},

		// R-01: fentry/generic_file_read_iter access-pattern observer.
		{"s3fs_ebpf_read_iter_hits_total", "generic_file_read_iter calls where ino was in inode_index (fires ~2× per cat: data + EOF).", "counter", Global.ReadIterHits.Load()},
		{"s3fs_ebpf_read_iter_misses_total", "generic_file_read_iter calls where ino was unknown; triggers daemon prewarm upcall.", "counter", Global.ReadIterMisses.Load()},
		// Y-06: pos==0 subset. hits_total / pos0_hits_total ≈ 2 confirms data+EOF double-fire.
		{"s3fs_ebpf_read_iter_pos0_hits_total", "read_iter hits where ki_pos==0 (first read per open); ≈ N_ACCESS in bench. Y-06 diagnostic.", "counter", Global.ReadIterHitsPos0.Load()},
	}
	for _, m := range metrics {
		fmt.Fprintf(w, "# HELP %s %s\n", m.name, m.help)
		fmt.Fprintf(w, "# TYPE %s %s\n", m.name, m.mtype)
		fmt.Fprintf(w, "%s %d\n", m.name, m.value)
	}
	// Histograms live in their own type — see histogram.go. Emit
	// after the flat counters/gauges so the text-format parser
	// order matches Prometheus's expected grouping.
	ReadMissLatency.writeTo(w)
	FlushLatency.writeTo(w)

	// Custom gauges emitted by CEL policy `metric(name, value)`
	// calls. Bounded to CustomGaugeCap distinct names; ordered by
	// name for stable scrape output. See metrics/custom.go.
	writeCustom(w)

	// J-02: Go runtime health — goroutines, heap, GC. Named to match
	// client_golang's go_* series so stock Grafana dashboards apply.
	writeRuntime(w)
}
