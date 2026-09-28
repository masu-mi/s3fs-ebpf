# P-07 Hit-rate bench

Compares built-in cache policies on a Zipf-skewed access workload.
Runs via `./scripts/bench-hitrate.sh`.

## Setup

- 30 tiny (44 B) files seeded into `s3fs-test` bucket via `mc pipe`.
- 200 accesses in a fixed-seed Zipf(α=0.7) order (deterministic, so
  each policy sees the identical trace).
- Softcap = 8 (kmod-side hardcoded). Working set of 30 forces
  eviction pressure on every policy.
- Fresh kmod + mount + daemon cycle per policy — no state leaks.
- Convergence: 4 explicit `POST /policy/eval` calls, 3 s apart,
  drive multi-round eviction cascades without needing bucket-side
  churn.
- Miss metric: `s3fs_ebpf_read_miss_upcalls_total` (kmod-side
  `admit_snapshot()` miss → path-C upcall). Rate =
  `(N_ACCESS - Δmisses) / N_ACCESS`.

## Result (2026-09-28, after full 3-tier S3-FIFO added)

```
policy=observe         hits=  200  misses=    0  rate=100.00%  evictions=    0
policy=freq2bit_ops    hits=   17  misses=  183  rate=  8.50%  evictions=10542
policy=lru_ops         hits=   28  misses=  172  rate= 14.00%  evictions= 9410
policy=sieve_ops       hits=  137  misses=   63  rate= 68.50%  evictions=  328
policy=s3fifo_ops      hits=  123  misses=   77  rate= 61.50%  evictions=  757
```

## Reading the numbers

- **observe** never evicts — theoretical ceiling.
- **SIEVE** (`policy_sieve_ops`): 68.5 %, best of the evicting
  policies. The visited-bit + second-chance clock behaves
  correctly for skewed reuse.
- **S3-FIFO** (`policy_s3fifo_ops`, real 3-tier): 61.5 %. The
  Small/Main/Ghost split matches the published S3-FIFO paper
  (Yang et al SOSP 2023): items enter S, promote to M on re-hit,
  ghost record lets re-admits skip S. Within 7 pp of SIEVE.
- **LRU** (`policy_lru_ops`, approximate admit-order FIFO with
  `on_hit` refreshing the seed epoch): 14 %. Poor because our
  eval-forced rounds bump the global epoch on every `on_admit`,
  so ages accumulate faster than `on_hit` resets. Real
  sync-driven epoch would keep more.
- **freq2bit** (`policy_freq2bit_ops`, single-tier 2-bit
  reference counter): 8.5 %, worst. This is what the file was
  called `policy_s3fifo_*` for during Week 4 (d) — an
  honest-name renaming (Oct 2026) landed once we noticed the
  algorithm had none of the {Small, Main, Ghost} structure that
  makes S3-FIFO S3-FIFO.

Two comparisons that carry the story:

1. **7× improvement from `freq2bit` → `s3fifo` (both roughly the
   same code shape + BPF map density)**. The gain is entirely
   from adding the tier split and the ghost re-admit path —
   evidence that the S3-FIFO paper's core innovation isn't
   incidental.

2. **SIEVE vs real S3-FIFO within 7 pp**. Published benchmarks
   in the S3-FIFO / SIEVE papers show the two trade wins by
   workload — no free lunch. Our micro-workload is short and
   admit-round-dense, which favors SIEVE's round-count-invariant
   clear-on-spare; longer runs with real read/round ratio
   probably close the gap or flip the winner.

The eviction totals (~10 k for LRU/freq2bit vs ~300 for SIEVE
vs ~800 for S3-FIFO) tell the same story: aggressive policies
churn the admit hash so hard that the workload can't warm any
single ino before it's kicked out again.

## Naming honesty (Oct 2026)

Until commit 4e4e3f1 the "S3-FIFO" built-in was actually a
single-tier per-ino 2-bit counter with decrement-on-evict
semantics — closer to Generalized CLOCK than S3-FIFO. It was
kept because it was our first spike and worked for the fmod_ret
vs struct_ops comparison (docs/policy-attach-comparison.md).

We renamed it to `policy_freq2bit(_ops)` so the algorithm name
matches the code, and added a proper 3-tier S3-FIFO under the
S3-FIFO name. The bench above measures both. See:
- `bpf/policy_freq2bit_ops.bpf.c` — single-tier reference counter
- `bpf/policy_s3fifo_ops.bpf.c` — real S/M/G S3-FIFO

## Caveats / non-obvious tuning

1. Softcap = 8 hardcoded in `kmod/vfs_ops.c` (`const u32 softcap
   = 8;`). At a larger softcap all policies trend toward
   `observe`'s 100 %. To move it you also need to raise the
   admit hash's `S3FS_ADMIT_HASH_BITS` (currently 6 → 64
   buckets). See `docs/backlog.md` D-01 for the range-list
   redesign that would remove the fixed cap entirely.

2. `POST /policy/eval` is a bench-only knob. Production
   `admit_replace` fires on bucket delta or read-miss upcall,
   so real policies see roughly one round per S3 change or per
   cold read, not four in twelve seconds. The observed rates
   here are worst-case for eviction pressure, best-case for
   showing algorithm differences.

3. The 44 B file size means one folio = one miss. For a 1 MiB
   file the same access would fire ~256 misses, biasing the
   count without changing the rate. Larger files also stress
   the arena's usage tracking — see backlog H-03.

4. Reproducibility: workload sampling uses `random.seed(42)`.
   The same trace runs against every policy so ordering effects
   between runs (eval-round vs drain-round race) are the only
   source of run-to-run variance. Expect ±5 pp on repeat.

5. Our S3-FIFO does not enforce paper-mandated |S|=10 %,
   |M|=90 % split explicitly. Softcap = 8 bounds the whole admit
   hash; the algorithm's own decisions leave a mix, but tighter
   size control would need per-tier admit accounting in kmod.
   Would be worth adding once softcap becomes tunable.

## What this unblocks

- P-07 objective met: numerical evidence that policy choice
  matters — 8× improvement (SIEVE vs freq2bit) and 7×
  improvement (S3-FIFO vs freq2bit) on a workload where
  softcap has real bite.
- Meetup demo material: "swap the policy, hit rate goes from
  9 % to 69 %, live." The POST endpoints (`/policy/reload` +
  `/policy/eval`) make that a curl-only demo.

## What this does not cover

- **Real production-ish workload**: continuous reads (millions
  of ops) rather than 200 spaced by admit-round cascades. The
  gap between SIEVE and S3-FIFO probably closes; the gap over
  LRU probably widens.
- **Write-heavy patterns**: only reads exercised here.
- **Multi-tenant / range access**: single ino at a time,
  ino ↔ full-file admit shape (P1 backlog D-01 for range).
- **W-TinyLFU / SIEVE with a global hand pointer** (would need
  a shared cursor in a BPF map).
- **Ghost list expiry in S3-FIFO**: our impl leaves ghost
  entries in the state map until the max_entries LRU kicks
  them out. Fine at 8192 entries, but a real deployment would
  want size-bounded ghost tracking.
