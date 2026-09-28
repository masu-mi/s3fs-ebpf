# P-07 Hit-rate bench

Compares SIEVE / LRU / S3-FIFO / observe on a Zipf-skewed access
workload. Runs via `./scripts/bench-hitrate.sh`.

## Setup

- 30 tiny (44 B) files seeded into `s3fs-test` bucket via `mc pipe`.
- 200 accesses in a fixed-seed Zipf(α=0.7) order (deterministic, so
  each policy sees the identical trace).
- Softcap = 8 (kmod-side hardcoded). Working set of 30 forces
  eviction pressure on every policy.
- Fresh kmod + mount + daemon cycle per policy — no state leaks.
- Convergence: 4 explicit `POST /policy/eval` calls, 3 s apart,
  drive SIEVE's grace → clear → evict rounds without needing
  bucket-side churn.
- Miss metric: `s3fs_ebpf_read_miss_upcalls_total` (kmod-side
  `admit_snapshot()` miss → path-C upcall). Rate =
  `(N_ACCESS - Δmisses) / N_ACCESS`.

## Result (2026-09-28)

```
policy=observe         hits=  200  misses=    0  rate=100.00%  evictions=    0
policy=s3fifo_ops      hits=   19  misses=  181  rate=  9.50%  evictions=11111
policy=lru_ops         hits=   24  misses=  176  rate= 12.00%  evictions=10076
policy=sieve_ops       hits=  125  misses=   75  rate= 62.50%  evictions=  468
```

## Reading the numbers

- **observe** never evicts, so every access hits from the initial
  prewarm. That's the theoretical ceiling for this workload.
- **SIEVE** wins by a wide margin (**+50 pp over LRU, +53 pp over
  S3-FIFO**). The single visited-bit + second-chance clock behaves
  correctly for skewed reuse — hot files keep visited=1 and survive
  multiple eviction rounds; the eviction pointer walks past them.
- **LRU** (approximate, admit-order FIFO with `on_hit` refreshing
  the seed epoch) does poorly here because our eval-forced rounds
  bump the global epoch on every `on_admit`, so ages accumulate
  faster than `on_hit` can reset them for the mid-tier files.
  With a real sync-driven epoch (one bump per S3 sync tick, not
  per eval round) LRU would keep more of the working set.
- **S3-FIFO** collapses in the pathological way — the 2-bit
  saturating freq drains almost as fast as it fills under our
  addAdmit-per-miss cascade, and the algorithm degrades to
  admit-order FIFO with a decrement step. Real S3-FIFO expects
  reads to arrive faster than admit_replace rounds, which is the
  opposite of this bench's shape.

The eviction totals (11 k for S3-FIFO / LRU vs 0.5 k for SIEVE)
tell the same story: aggressive policies churn the admit hash
so hard that the workload can't warm any single ino before it's
kicked out again.

## Caveats / non-obvious tuning

1. Softcap = 8 hardcoded in `kmod/vfs_ops.c` (`const u32 softcap
   = 8;`). At a larger softcap, all four policies would trend
   toward `observe`'s 100 %. To move it you also need to raise
   the admit hash's `S3FS_ADMIT_HASH_BITS` (currently 6 → 64
   buckets). See `docs/backlog.md` D-01 for the range-list
   redesign that would remove the fixed cap entirely.

2. `POST /policy/eval` is a bench-only knob. Production
   `admit_replace` fires on bucket delta or read-miss upcall,
   so LRU / S3-FIFO would see roughly one policy round per
   S3 change or per cold read, not four in twelve seconds.
   The observed rates here are worst-case for eviction pressure,
   best-case for showing algorithm differences.

3. The 44 B file size means one folio = one miss. For a 1 MiB
   file the same access would fire ~256 misses, biasing the
   count without changing the rate. Larger files also stress
   the arena's usage tracking — see backlog H-03.

4. Reproducibility: workload sampling uses `random.seed(42)`.
   The same trace runs against every policy so ordering effects
   between runs (e.g. eval-round vs drain-round race) are the
   only source of run-to-run variance. Expect ±5 pp on repeat.

## What this unblocks

- P-07 objective met: numerical evidence that policy choice
  matters — 6× improvement (SIEVE vs S3-FIFO) on a workload
  where the softcap has real bite.
- Meetup demo material: "swap the policy, hit rate goes from
  9 % to 62 %, live." The POST endpoints (`/policy/reload` +
  `/policy/eval`) make that a curl-only demo.

## What this does not cover

- **Real production-ish workload**: continuous reads (millions
  of ops) rather than 200 spaced by admit-round cascades. The
  gap would narrow — SIEVE probably still wins, but by less.
- **Write-heavy patterns**: only reads exercised here.
- **Multi-tenant / range access**: single ino at a time,
  ino ↔ full-file admit shape (P1 backlog D-01 for range).
- **W-TinyLFU / SIEVE with on_hit-driven visited (already have)
  vs SIEVE with per-op hand pointer (would need a global cursor
  in a BPF map)**. Adding W-TinyLFU is the next natural
  extension — the frequency sketch primitive is worth building
  once and reusing across policies.
