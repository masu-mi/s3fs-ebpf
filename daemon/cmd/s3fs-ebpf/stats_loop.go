// Periodic stats / eviction-drain loops and counter printing for fastpath.

package main

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/bbsakura/s3fs-ebpf/daemon/internal/metrics"
	"github.com/bbsakura/s3fs-ebpf/daemon/internal/policyprog"
)

// samplePolicyStats mirrors the loaded policy BPF program's counters
// into metrics.Global on a ticker. Uses lookups from the shared map
// (see bpf/policy_observe_ops.bpf.c PSTAT_* indices).
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
func runStatsLoop(ctx context.Context, devPath string, every time.Duration) {
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
			// kmod-side ioctl stats.
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
						metrics.Global.ReadIterHits.Store(vals[3])
					}
					if len(vals) >= 5 {
						metrics.Global.ReadIterHitsPos0.Store(vals[4])
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

		}
	}
}
