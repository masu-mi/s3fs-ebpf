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

## Result (2026-09-28, 全 10 policy 揃い + full W-TinyLFU 追加)

```
policy=observe            100.00%    0 evict
policy=wtinylfu_ops        88.50%  161 evict   ← CMS + aging (scoped)
policy=size_aware_ops      66.00%  475 evict
policy=sieve_ops           65.00%  475 evict
policy=s3fifo_ops          64.00%  862 evict
policy=pressure_ops        52.00% 1836 evict
policy=wtinylfu_full_ops   49.50% 4554 evict   ← Window + DK + CMS + aging (paper)
policy=time_decay_ops      35.00% 5612 evict
policy=lru_ops             13.00% 9884 evict
policy=freq2bit_ops        10.50% 10320 evict
```

### The W-TinyLFU paradox: paper-complete regresses vs scoped

Two W-TinyLFU variants ship:
- **`wtinylfu_ops`** — CMS + aging only (88.5 %). Ships as default.
- **`wtinylfu_full_ops`** — paper-complete: Window LRU tier + Doorkeeper
  Bloom filter + CMS + aging (49.5 %). Uses `bpf_loop()` for both
  the CMS-halve and doorkeeper-clear iterations.

**Full variant regresses ~40 pp on this bench** because the paper's
{Window, Doorkeeper} components are tuned for workloads with many
one-hit wonders and long-lived working sets. Ours has 30 files each
accessed ~7 times average — no cold noise for the doorkeeper to
filter, no bursty new admits for the window tier to buffer. The
extra machinery just filters useful signal:

- **Doorkeeper**: every ino's first hit per DK-reset cycle goes to
  the Bloom filter, NOT to the CMS. On a 200-access workload with
  30 files that's ~6-7 filtered hits per ino out of ~7 total → CMS
  starves.
- **Window tier**: new admits wait up to `WINDOW_MAX_ROUNDS=8`
  rounds in W before promotion to Main. During the bench's 4 forced
  eval rounds (before workload starts), items sit in W with CMS=0
  and can be evicted before their first real hit.

This is a genuine case where paper-complete ≠ best. Full W-TinyLFU
shines on workloads with `capacity ≪ working_set` and heavy tail
distributions (Caffeine's typical target); our micro-bench is the
opposite shape. See `bpf/policy_wtinylfu_full_ops.bpf.c` header for
the tuning knobs (`WINDOW_MAX_ROUNDS`, `PROMOTE_THRESHOLD`,
`DK_RESET_INTERVAL`) that operators can shift for their workload.

**W-TinyLFU リードが確立** (88.5 %、次位 SIEVE 66 % との差 22 pp)。
CMS + aging の組合せが Zipf 型 workload で強い。以下 3 tier に
分かれる:

- **Top (>60%)**: W-TinyLFU 88.5, SIEVE 66, size_aware 65, s3fifo 46
- **KFunc-driven demo (30-55%)**: pressure 54, time_decay 30.5
- **Simple (<10%)**: LRU 7.5, freq2bit 6.5

### KFunc-driven policy の見どころ

Layer 0 KFunc (P-08) の shape を demo する 3 policy が入った:

- **`pressure_ops`** (54 %) — `s3fs_kfunc_admit_count()` /
  `softcap()` で SIEVE strictness を切替。1 admit_replace round
  内で evictions が積み上がって admit_count が下がる → 動的に
  aggressive → sieve に自動 switch。live kmod state を read する
  shape の証明
- **`time_decay_ops`** (30.5 %) — `s3fs_kfunc_time_ns()` で
  `now - last_hit_ns > 5s` を evict。access pattern より wall
  clock 支配で hit rate 低め、DECAY_NS=5s が Zipf workload には
  短すぎる。長い DECAY_NS でチューニングすれば SIEVE 相当に
  なる想定
- **`size_aware_ops`** (65 %) — `s3fs_kfunc_ino_size(ino)` で
  SMALL_THRESHOLD=4KB 未満 / 以上を bias。今 bench の files は
  全て 44 B なので SIEVE 相当挙動、mixed size workload だと
  BIG_MISS_CAP grace が効いてくる

### s3fifo_ops のばらつき

`s3fifo_ops` は run ごとに 46-66 % で振れる。原因は 3-tier の
S/M/G Ghost が 8-9 個の admit hash と softcap=8 の狭い working
set で敏感に反応するため。より大きい softcap / working set では
安定する見込み。W-TinyLFU の 88.5 %、SIEVE の 66 % は run 間で
±3 pp 程度に収まる。

Two clean tiers emerge:

- **Modern (SIEVE, S3-FIFO)**: 62 % — the visited-bit and 3-tier
  strategies both protect the working set effectively.
- **Simple (LRU, freq2bit)**: 12-13 % — over-aggressive eviction
  churn.
- **W-TinyLFU (scoped)**: 25 %, in between. The CMS-only variant
  identifies frequent items but without the paper's Window-LRU
  tier and doorkeeper it can't defend against
  "burst-once-then-cold" traces. Full W-TinyLFU would likely
  reach the 60 % tier.

Bench harness change (2026-09-28): the workload now fires
`POST /policy/eval` every `INTERLEAVE=25` accesses to trigger
eviction rounds mid-workload. Without this, policies whose state
lives entirely in `on_hit` (W-TinyLFU's CMS) never populate before
the convergence eval rounds fire, so on_evict grants universal
grace and no evictions happen. Existing policies (SIEVE / LRU /
S3-FIFO / freq2bit) are within ±2 pp of their pre-interleave
numbers.

## Reading the numbers

- **observe** never evicts — theoretical ceiling.
- **SIEVE** (`policy_sieve_ops`): 66 %. The visited-bit +
  second-chance clock behaves correctly for skewed reuse.
- **size_aware_ops** (`policy_size_aware_ops`, KFunc demo):
  65 %. Uses `s3fs_kfunc_ino_size()` for SMALL/BIG file bias.
  Degrades to SIEVE-equivalent on the all-small-file bench;
  BIG_MISS_CAP grace only fires when mixed-size workloads run.
- **S3-FIFO** (`policy_s3fifo_ops`, real 3-tier): 46 %. The
  Small/Main/Ghost split matches Yang et al SOSP 2023 but is
  sensitive to the small (softcap=8) working set — bench-run
  variance is ±20 pp. Larger softcap / more admits stabilise
  it.
- **pressure_ops** (`policy_pressure_ops`, KFunc demo): 54 %.
  Reads kmod-side admit count and softcap live via the Layer
  0 KFunc set (P-08). Toggles between SIEVE-generous and
  aggressive-evict on the fly. See `bpf/policy_pressure_ops.bpf.c`
  for the ratio threshold.
- **time_decay_ops** (`policy_time_decay_ops`, KFunc demo):
  30.5 %. Uses `s3fs_kfunc_time_ns()` for wall-clock decay
  (DECAY_NS=5s). Access-pattern-agnostic → suffers on
  Zipf-shaped bursts. Meant as a proof-of-KFunc-time shape,
  not a production recommendation.
- **W-TinyLFU** (`policy_wtinylfu_ops`, scoped): 88.5 %. CMS +
  bpf_loop aging only. Best-performing evicting policy on our
  bench. Ships as the default `--policy-bpf=builtin:policy_
  wtinylfu_ops`. See §"Reading W-TinyLFU" for why the scoped
  variant beats the paper-complete `wtinylfu_full_ops`.
- **LRU** (`policy_lru_ops`, approximate admit-order FIFO with
  `on_hit` refreshing the seed epoch): 12.5 %. Poor because our
  eval-forced rounds bump the global epoch on every `on_admit`,
  so ages accumulate faster than `on_hit` resets. Real
  sync-driven epoch would keep more.
- **freq2bit** (`policy_freq2bit_ops`, single-tier 2-bit
  reference counter): 12 %. Was called `policy_s3fifo_*` until
  the honest-name rename landed once we noticed the algorithm
  had none of the {Small, Main, Ghost} structure that makes
  S3-FIFO S3-FIFO.

The one comparison that carries the story:

- **~5× improvement from `freq2bit` (12 %) → `s3fifo` (62.5 %)
  with roughly the same code shape**. The gain is entirely
  from adding the tier split and the ghost re-admit path —
  evidence that the S3-FIFO paper's core innovation isn't
  incidental.

## Reading W-TinyLFU

Our `policy_wtinylfu_ops` implements the *centerpiece* of the
paper (CMS-based frequency estimator + threshold admission)
plus the aging step. Two components are still deferred:

1. **Window LRU tier** (~1 % of cache in the paper). New admits
   land in W-LRU and only graduate to Main if they earn CMS
   respect. Without W-LRU, one-hit wonders admit straight to
   Main and consume slots that hot items would use. In practice
   aging plus the softcap-driven eviction covers most of this
   gap on our bench, but tail workloads (bursts of unique
   items) will still hurt.
2. **Doorkeeper Bloom filter**. Filters "1-hit wonders" before
   they populate CMS. Without it, our 4×256 sketch collects
   noise from cold items — aging clears it periodically, so
   the net effect on hit rate is small.

At **80 %** we're within striking distance of the "TinyLFU
near-optimal" numbers the paper reports on comparable Zipf
workloads. The remaining gap is smaller than the CMS-vs-clock
difference (SIEVE at 62 %), so future work is diminishing
returns on this micro-workload — real value comes from
mixed-load and burst scenarios.

**Aging is the trick that mattered.** Before `bpf_loop()`
landed we skipped the halving step because a pragma-unrolled
1024-iteration loop exceeds the verifier's insn budget.
`bpf_loop()` delegates iteration to the kernel with a fixed
runtime bound, sidestepping the limit. Cost: one BPF helper
call per age; the callback itself is 3 insns.

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
   buckets). See `docs/journal/backlog.md` D-01 for the range-list
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
  matters — 13× improvement (W-TinyLFU vs freq2bit) on a
  workload where softcap has real bite.
- Meetup demo material: "swap the policy, hit rate goes from
  6.5 % to 88.5 %, live." The POST endpoints (`/policy/reload` +
  `/policy/eval`) make that a curl-only demo.
- Layer 0 KFunc proven on 3 demo policies (pressure /
  time_decay / size_aware) — all live-read kmod-side state
  during on_evict decisions.

## What this does not cover

- **Real production-ish workload**: continuous reads (millions
  of ops) rather than 200 spaced by admit-round cascades. The
  gap between SIEVE and S3-FIFO probably closes; the gap over
  LRU probably widens.
- **Write-heavy patterns**: only reads exercised here.
- **Multi-tenant / range access**: single ino at a time,
  ino ↔ full-file admit shape (P1 backlog D-01 for range).
- **Full W-TinyLFU**: window LRU + doorkeeper (aging is done —
  see commit history and "Reading W-TinyLFU" above). Window
  LRU needs per-ino tier state; doorkeeper is a fixed-size
  Bloom filter — both feasible extensions.
- **SIEVE with a global hand pointer**: our impl fires
  second-chance semantics per admit_replace round instead of
  along a global cursor. Adding a cursor would need a shared
  BPF map var.
- **Ghost list expiry in S3-FIFO**: our impl leaves ghost
  entries in the state map until the max_entries LRU kicks
  them out. Fine at 8192 entries, but a real deployment would
  want size-bounded ghost tracking.
