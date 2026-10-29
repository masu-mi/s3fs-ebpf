// Periodic stats / eviction-drain loops and counter printing for fastpath.

package main

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	fpebpf "github.com/bbsakura/s3fs-ebpf/daemon/internal/ebpf"

	"github.com/bbsakura/s3fs-ebpf/daemon/internal/metrics"
	"github.com/bbsakura/s3fs-ebpf/daemon/internal/policyprog"
)

// samplePolicyStats mirrors the loaded policy BPF program's counters
// into metrics.Global on a ticker. Uses lookups from the shared map
// (see bpf/policy_observe.bpf.c PSTAT_* indices).
func samplePolicyStats(ctx context.Context, mu *sync.Mutex, cur **policyprog.Program, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			// Re-resolve every tick under the reload lock: a hot
			// reload closes the old program's maps, so a captured
			// pointer would read a closed (or reused) fd.
			mu.Lock()
			if p := *cur; p != nil {
				if v, err := p.ReadStat(policyprog.StatInvocations); err == nil {
					metrics.Global.PolicyHookInvocations.Store(v)
				}
				if v, err := p.ReadStat(policyprog.StatLastCount); err == nil {
					metrics.Global.PolicyLastAdmitCount.Store(v)
				}
				// Z-04: pressure_ops strict/lenient counters.
				// Silently ignored for other policies (out-of-range → error).
				if v, err := p.ReadStat(policyprog.StatPressureStrictTotal); err == nil {
					metrics.Global.PolicyPressureStrictTotal.Store(v)
				}
				if v, err := p.ReadStat(policyprog.StatPressureLenientTotal); err == nil {
					metrics.Global.PolicyPressureLenientTotal.Store(v)
				}
			}
			mu.Unlock()
		}
	}
}

// runStatsLoop consolidates the three independent per-source sampling
// goroutines (BPF counters, kmod ioctl stats, arena free-list) into a
// single ticker loop (W-03). All three sources are sampled on every
// tick at the sampleEvery interval. Opening the device fd once
// (T-03 pattern) is handled inside the loop.
func runStatsLoop(ctx context.Context, progs *fpebpf.Programs, devPath string, every time.Duration) {
	// Open the device fd once for kmod-stats ioctls. Failure is
	// non-fatal (kmod stats will be zero in /metrics) so the daemon
	// starts even without a real device (tests / offline mode).
	var devFd int = -1
	if f, err := os.OpenFile(devPath, os.O_RDWR, 0); err == nil {
		defer f.Close()
		devFd = int(f.Fd())
	} else {
		fmt.Fprintf(os.Stderr, "[stats] open %s: %v (kmod stats disabled)\n", devPath, err)
	}

	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			// Source 1: BPF per-CPU counter map.
			printCounters(progs, "sample")

			// Source 2: kmod-side ioctl stats.
			if devFd >= 0 {
				if vals, err := readStatsOn(devFd); err == nil {
					if len(vals) >= 1 {
						metrics.Global.PolicyEvictionsTotal.Store(vals[0])
					}
					if len(vals) >= 2 {
						metrics.Global.FolioHits.Store(vals[1])
					}
					if len(vals) >= 3 {
						metrics.Global.AdmitInvalidations.Store(vals[2])
					}
					if len(vals) >= 4 {
						metrics.Global.GupCallsTotal.Store(vals[3])
					}
					if len(vals) >= 5 {
						metrics.Global.GupNsTotal.Store(vals[4])
					}
					if len(vals) >= 6 {
						metrics.Global.UpcallRetryExhausted.Store(vals[5])
					}
					if len(vals) >= 7 {
						metrics.Global.UpcallTimeouts.Store(vals[6])
					}
					if len(vals) >= 8 {
						metrics.Global.Gen0FailBufNull.Store(vals[7])
					}
					if len(vals) >= 9 {
						metrics.Global.Gen0FailBitmap.Store(vals[8])
					}
					if len(vals) >= 10 {
						metrics.Global.Gen0FailOther.Store(vals[9])
					}
					if len(vals) >= 11 {
						metrics.Global.FdBufBytes.Store(vals[10])
					}
					if len(vals) >= 12 {
						metrics.Global.ResetSkipped.Store(vals[11])
					}
					if len(vals) >= 14 {
						metrics.Global.FdBufWalkBytes.Store(vals[12])
						metrics.Global.FdBufPcacheBytes.Store(vals[13])
					}
					if len(vals) >= 15 {
						metrics.Global.HitsReplayed.Store(vals[14])
					}
				}
			}

			// Source 3: arena free-list gauge.
			if progs != nil {
				bytes, chunks := progs.ArenaFreeStats()
				metrics.Global.ArenaFreeBytes.Store(bytes)
				metrics.Global.ArenaFreeChunks.Store(uint64(chunks))
			}
		}
	}
}

// drainEvictionsLoop polls S3FS_IOC_DRAIN_EVICTIONS and prunes the
// daemon-side admit snapshot to match. Without this, daemon re-sends
// evicted inos on the next admit_replace and kmod evicts them again
// in a tight loop — visible in Grafana as admits_current staying
// high while policy_evictions_total climbs monotonically.
//
// D-02: `progs` is threaded so dropAdmits can return arena chunks
// to the free-list (fpebpf.Programs.FreeArenaChunk).
func drainEvictionsLoop(ctx context.Context, devPath string, progs *fpebpf.Programs, every time.Duration) {
	// T-03: open once and reuse the fd across ticks instead of
	// open/close on every 2s poll.
	f, err := os.OpenFile(devPath, os.O_RDWR, 0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[evict-drain] open %s: %v\n", devPath, err)
		return
	}
	defer f.Close()

	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			evicted, overflow, err := drainEvictionsOn(int(f.Fd()))
			if err != nil {
				continue
			}
			if overflow > 0 {
				fmt.Fprintf(os.Stderr, "[evict-drain] kmod queue overflow=%d (may need larger buffer)\n",
					overflow)
			}
			if n := dropAdmits(evicted, progs); n > 0 {
				fmt.Printf("[evict-drain] dropped %d admits fed back from kmod\n", n)
			}
		}
	}
}

func printCounters(progs *fpebpf.Programs, label string) {
	c, err := progs.ReadCounters()
	if err != nil {
		fmt.Fprintf(os.Stderr, "read counters: %v\n", err)
		return
	}
	// Mirror the BPF-side counters into the metrics endpoint. Store
	// (not Add) because the BPF map is the source of truth — the
	// per-CPU aggregate is what we want to expose, not the delta.
	metrics.Global.FastPathHits.Store(c[fpebpf.StatFastHits])
	metrics.Global.SlowPathMisses.Store(c[fpebpf.StatSlowMisses])
	metrics.Global.UnknownFS.Store(c[fpebpf.StatUnknownFS])
	metrics.Global.ReadIterHits.Store(c[fpebpf.StatReadIterHits])
	metrics.Global.ReadIterMisses.Store(c[fpebpf.StatReadIterMisses])
	metrics.Global.ReadIterHitsPos0.Store(c[fpebpf.StatReadIterHitsPos0])
	fmt.Printf("[counters/%s] fast_hits=%d slow_misses=%d unknown_fs=%d read_iter_hits=%d read_iter_misses=%d iter_pos0=%d\n",
		label,
		c[fpebpf.StatFastHits], c[fpebpf.StatSlowMisses], c[fpebpf.StatUnknownFS],
		c[fpebpf.StatReadIterHits], c[fpebpf.StatReadIterMisses],
		c[fpebpf.StatReadIterHitsPos0])
}

func opName(op uint32) string {
	switch op {
	case fpebpf.OpGetattr:
		return "getattr"
	case fpebpf.OpLookup:
		return "lookup"
	case fpebpf.OpRead:
		return "read"
	default:
		return fmt.Sprintf("op#%d", op)
	}
}
