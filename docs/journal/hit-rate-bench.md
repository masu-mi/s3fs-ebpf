# P-07 Hit-rate bench

Compares built-in cache policies on a Zipf-skewed access workload.
Runs via `./scripts/bench-hitrate.sh`.

## ⚠ Why the 2026-09-28 numbers are invalid (historical context)

The 2026-09-28 table (W-TinyLFU 88.5 %, SIEVE 65–66 %, LRU 7.5 %) was
produced with two bugs that made the numbers meaningless:

1. **S-02 (capacity not enforced)**: `S3FS_EVICT_SOFTCAP` was an
   eviction *trigger*, not a capacity cap. Policies that rarely vote
   "evict" held all 30 prewarmed files — the ranking measured
   *eviction frequency*, not victim selection quality.
2. **S-01 (page cache not cleared on evict)**: evicted files remained
   in the kernel page cache, so `.read_iter` served them without
   reaching `.read_folio`, inflating hit counts and starving `on_hit`.

Both bugs are fixed (S-01: `s3fs_evict_page_cache`; S-02:
`capacity_bytes = SOFTCAP × FILE_BYTES`). **The 2026-10-05 results
below supersede all earlier numbers.**

## Setup

- 30 tiny (44 B) files seeded into `s3fs-test` bucket via `mc pipe`.
- 200 accesses in a fixed-seed Zipf(α=0.7) order (deterministic, so
  each policy sees the identical trace). Seed is configurable via
  `SEED=<n>` env var; `bench-hitrate-multi.sh` runs seeds 42, 1337, 7.
- Softcap = 8, `capacity_bytes = 8 × 44 = 352` (enforced true
  byte-cap via S-02). Every policy competes for exactly 8 slots.
- Fresh kmod + mount + daemon cycle per policy — no state leaks.
- Convergence: 4 explicit `POST /policy/eval` calls, 3 s apart,
  drive multi-round eviction cascades without needing bucket-side
  churn.
- **Output columns** (U-01/U-02, 2026-10-11):
  - `folio` — Δ`s3fs_ebpf_folio_hits_total`: kmod `.read_folio` arena
    hits. The true policy signal — only fires when the ino is in the
    admit set and its folio is in the BPF arena.
  - `pc` — page-cache approx: Δread_iter_hits − folio − miss, floored
    at 0. Counts re-reads served from the kernel page cache after a
    prior arena admission (not filtered by the policy).
  - `miss` — Δ`s3fs_ebpf_read_miss_upcalls_total`: upcall misses (ino
    not in admit set → path-C daemon upcall).
  - `err` — count of `cat` calls that returned non-zero. Non-zero means
    the file couldn't be read at all (distinct from a cache miss, which
    still serves the data via upcall). Should stay 0 on a healthy stack.
  - `rate_arena` = folio / N_ACCESS: the policy-controlled hit rate.
  - `rate_total` = (folio + pc) / N_ACCESS: total read-serve rate
    including page-cache assists.
- `adm_mean` / `adm_max` sampled at every `INTERLEAVE=25` accesses;
  all rows show 8.9 / 9 confirming capacity is enforced (never above
  softcap+1 during the first prewarm sync).

## Result (2026-10-05, 3 seeds × capacity mode, post S-01+S-02)

Aggregate over seeds 42, 1337, 7 (each 200 accesses, Zipf α=0.7, 30 files,
softcap=8, capacity_bytes=352). Run with `scripts/bench-hitrate-multi.sh`.

```
policy                  rate_med  rate_min  rate_max  evict_med  adm_mean_med
----------------------  --------  --------  --------  ---------  ------------
s3fifo_ops               39.5%     33.5%     40.0%       121          8.9
wtinylfu_ops             37.0%     35.5%     40.0%       126          8.9
sieve_ops                36.5%     36.0%     40.5%       127          8.9
pressure_ops             36.0%     35.0%     40.5%       128          8.9
size_aware_ops           36.0%     35.0%     40.0%       128          8.9
wtinylfu_full_ops        35.5%     33.5%     41.5%       129          8.9
freq2bit_ops             35.0%     33.0%     41.5%       130          8.9
lru_ops                  35.0%     33.0%     41.0%       130          8.9
observe                  33.5%     32.5%     39.5%       133          8.9
time_decay_ops           33.5%     32.5%     40.0%       133          8.9
```

Per-seed trace-replay reference (cold start, `dev trace-replay`):

| seed | lru | sieve | opt (Belady) | noevict |
|-----:|----:|------:|-------------:|--------:|
| 42   | 40.5% | 46.0% | 61.0% | 85.5% |
| 1337 | 33.0% | 38.0% | 59.5% | 85.0% |
| 7    | 40.5% | 49.5% | 63.0% | 85.5% |

### What the new numbers tell us

**Capacity is enforced.** `adm_mean=8.9`, `adm_max=9` across every
policy and seed — no policy holds more than 9 files, confirming S-02 works.
The old 88.5 % W-TinyLFU result held ≈30 files (no real cap).

**Policy spread is narrow: ~6 pp.** `observe` (FIFO, no ranking) sits
at the bottom (33.5 % median) and `s3fifo` tops at 39.5 %. The gap
is real — policies add ~6 pp — but small because Zipf α=0.7 naturally
concentrates hits on the top 8 files; any policy that doesn't
actively throw them away does similarly well.

**Belady gap is large: ~20 pp.** Live `s3fifo` (39.5 %) vs trace-replay
`opt` (59.5–63 %). This gap is not primarily a policy-selection failure;
it reflects (a) prewarm cost on a cold admit hash that trace-replay
doesn't model and (b) the fact that policy decisions fire per-eval-round,
not per-access — between rounds the admit set is frozen and misses
accumulate. Closing this gap requires higher-frequency eviction triggers
(P-07 backlog) or range-level admit tracking (D-01).

**Seed character drives more variance than policy selection on easy
seeds.** Seed 7 is "easy": Zipf produces a very clean hot set that
every policy protects — all policies cluster within 2 pp (39.5–41.5 %).
Seed 1337 is "harder": more uniform distribution, and `s3fifo`'s
3-tier ghost re-admit wins by ~4 pp over the next group.

### The W-TinyLFU paradox: dramatically smaller in a fair comparison

Two W-TinyLFU variants ship:
- **`wtinylfu_ops`** — CMS + aging only. Median 37.0 %.
- **`wtinylfu_full_ops`** — paper-complete: Window LRU tier + Doorkeeper
  Bloom filter + CMS + aging. Median 35.5 %.

**Full variant now shows only 1.5 pp regression** (vs the 39 pp gap in
the invalid 2026-09-28 run). On seed 7 the full variant actually leads
(41.5 % vs 40.0 %). The old paradox was mostly an artefact of the
capacity bug: the scoped variant happened to evict fewer files, so it
retained the full prewarmed admit set. With true capacity, both variants
compete on equal footing and differ by noise.

The structural reason the doorkeeper + window tier still slightly hurts
on this bench remains valid: 30 files × 7 accesses each, no one-hit
wonders for the doorkeeper to filter, no burst window for W-LRU to
buffer. The paper's components shine on workloads with large cold tail.
See `bpf/policy_wtinylfu_full_ops.bpf.c` header for the tuning knobs
(`WINDOW_MAX_ROUNDS`, `PROMOTE_THRESHOLD`, `DK_RESET_INTERVAL`).

### KFunc-driven policy の見どころ

Layer 0 KFunc (P-08) の shape を demo する 3 policy:

- **`pressure_ops`** (36.0 % med) — `s3fs_kfunc_admit_count()` /
  `softcap()` で SIEVE strictness を切替。live kmod state を read
  する shape の証明。capacity mode では admit_count が常に softcap
  付近に収まるため切替頻度が低下し SIEVE 相当に。
- **`time_decay_ops`** (33.5 % med) — `s3fs_kfunc_time_ns()` で
  `now - last_hit_ns > 5s` を evict。wall clock 支配で hit rate
  最下位グループ。DECAY_NS を長くすれば SIEVE 相当になる見込み。
- **`size_aware_ops`** (36.0 % med) — `s3fs_kfunc_ino_size(ino)` で
  SMALL_THRESHOLD=4KB 未満 / 以上を bias。全 file が 44 B なので
  SIEVE 相当挙動。mixed size workload では BIG_MISS_CAP grace が効く。

## Result (2026-10-12, 3 seeds × capacity mode、U+X 修正後) ← 最新正式結果

`scripts/bench-hitrate-multi.sh` (seeds 42/1337/7) を U-01/U-02 スクリプト修正 +
X-01〜X-07 policy 意味論修正後に再計測。**2026-10-05 の結果を置き換える正規版。**

```
policy                 miss_med  miss_min  miss_max  rtotal_med  rarena_med  evict_med  adm_mean_med
----------------------------------------------------------------------------------------------------
pressure_ops                119       118       126      140.5%       2.00%        119           8.9
sieve_ops                   128       119       130      136.0%       2.00%        128           8.9
size_aware_ops              128       119       130      136.0%       2.00%        128           8.9
wtinylfu_full_ops           129       117       133      135.5%       2.00%        129           8.9
freq2bit_ops                130       117       134      135.0%       1.50%        130           8.9
lru_ops                     130       118       134      135.0%       1.50%        130           8.9
observe                     133       121       135      133.5%       1.50%        133           8.9
s3fifo_ops                  133       120       137      133.5%       1.50%        133           8.9
wtinylfu_ops                133       120       135      133.5%       1.50%        133           8.9
time_decay_ops              133       121       135      133.5%       1.50%        133           8.9
```

ソート基準: miss_med 昇順（少ないほど良い）。

Per-seed trace-replay reference (cold start):

| seed | lru   | sieve | opt   | noevict |
|-----:|------:|------:|------:|--------:|
| 42   | 40.5% | 46.0% | 61.0% | 85.5%   |
| 1337 | 33.0% | 38.0% | 59.5% | 85.0%   |
| 7    | 40.5% | 49.5% | 63.0% | 85.5%   |

### 主要な変化（2026-10-05 比）

**pressure_ops が単独トップ** (miss_med=119、14 miss 差)。2026-10-05 では
s3fifo_ops がトップ (rate_total=39.5 %) だったが、X-01/X-03/X-06 修正後は
observe と同率最下位 (miss_med=133) に落ちた。

- **X-03**: CMS aging が admit round 駆動から on_hit 累計 (AGE_INTERVAL_HITS=512)
  駆動に変更 → この 200-access ショートワークロードでは aging がほぼ発火せず、
  wtinylfu_ops / wtinylfu_full_ops の frequency 信号が弱くなった
- **X-01**: admit_replace 2-phase 化で s3fifo の ghost list 更新タイミングが変わり
  ショートワークロードで ghost re-admit の恩恵が出にくくなった
- **X-06**: time_decay first-sight シード 0 化で初期期間の全 ino 自食いが解消された
  が、DECAY_NS=5s は依然このワークロードには短すぎる → 最下位グループ

**spread が縮小**: 旧 6 pp (33.5-39.5 %) → 新 19 miss 差 (119-133)。相対値では
pressure_ops の優位は明確 (14 miss = 10.5 %)。

**seed 7 (easy seed) でも差が残る**: 旧では全 policy が 39.5-41.5 % に集束
していたが、今回も pressure_ops (miss=119) vs observe (miss=121) で 2 miss 差。
"easy seed での収束" は主として page cache 効果であり policy の差は miss 数に残る。

---

## Result (2026-10-11, seed=42、U-01/U-02 + X-01〜X-07 修正後) ← 2026-10-12 で置き換え済み

単一 seed=42 の再計測。bench スクリプトと全 policy 実装に修正が入ったため、
2026-10-05 の 3-seed 集計とは独立した参照値として記録する。

```
policy              folio   pc   miss  err  rate_arena  rate_total  evictions  adm_mean  adm_max  ms/access
------------------  -----  ---   ----  ---  ----------  ----------  ---------  --------  -------  ---------
pressure_ops            2  284    114    0       1.00%     143.00%        114       8.9        9       6.02
size_aware_ops          3  279    118    0       1.50%     141.00%        118       8.9        9       6.10
wtinylfu_full_ops       0  271    129    0       0.00%     135.50%        129       8.9        9       6.07
freq2bit_ops            0  270    130    0       0.00%     135.00%        130       8.9        9       6.18
lru_ops                 0  270    130    0       0.00%     135.00%        130       8.9        9       6.19
sieve_ops               2  268    130    0       1.00%     135.00%        130       8.9        9       6.16
observe                 0  267    133    0       0.00%     133.50%        133       8.9        9       6.68
s3fifo_ops              0  267    133    0       0.00%     133.50%        133       8.9        9       6.28
wtinylfu_ops            0  267    133    0       0.00%     133.50%        133       8.9        9       6.21
time_decay_ops          0  267    133    0       0.00%     133.50%        133       8.9        9       6.12
```

Trace-replay reference (seed=42, cold start):

| policy   | hits | misses | rate  |
|----------|-----:|-------:|------:|
| lru      | 81   | 119    | 40.5% |
| sieve    | 92   | 108    | 46.0% |
| opt      | 122  | 78     | 61.0% |
| noevict  | 171  | 29     | 85.5% |

### 2026-10-05 からの変化

**policy ランキングが逆転した。** 2026-10-05 では s3fifo_ops がトップ (39.5 %)
だったが、今回は observe と同点最下位 (miss=133)。反対に pressure_ops と
size_aware_ops が最上位に浮上した (miss=114, 118)。

原因: X-01〜X-07 policy 意味論バグ修正の影響。とくに:

- **X-03** (CMS aging を admit round 駆動 → on_hit 累計駆動に変更) で
  `wtinylfu_ops` / `wtinylfu_full_ops` の spurious aging が解消。
  今回 `wtinylfu_ops` が miss=133 に落ちたのは、aging 閾値
  (AGE_INTERVAL_HITS=512) がこのショートワークロード (200 アクセス) では
  ほぼ発火しないため、CMS が rank signal として機能しにくい状況になった。
- **X-01** (admit_replace 2-phase 化) で s3fifo の ghost list 構築タイミングが
  変わり、ショートワークロードでは ghost re-admit の恩恵が出にくくなった。
- **X-06** (time_decay first-sight シード 0 化) は今回も miss=133 で不変。

**`rate_arena` が全 policy でほぼゼロ** (folio=0〜3)。44 B ファイルは
prewarm → eval → 15 s settle の間にカーネル page cache に温まるため、
実ワークロードでは `.read_folio` (arena path) より `.read_iter` (page cache)
が先に serve する。`miss` カウントが policy 評価の主要指標となる。

**3-seed 再集計は未取得**。上記は seed=42 単発のため variance は不明。
`bench-hitrate-multi.sh` による多 seed 再計測が meetup 発表前に必要。

## Reading the numbers

All miss_med values are medians over 3 seeds (42/1337/7). Because every
policy holds 8–9 files (adm_mean=8.9) the rows are truly comparable.
Lower miss_med = better (fewer S3 upcalls fired per 200 accesses).

- **observe** (miss_med=133): never votes to evict — FIFO fallback. Tied
  at the bottom with s3fifo/wtinylfu/time_decay; confirms that passive
  observation alone provides no victim-selection signal.
- **SIEVE** (`policy_sieve_ops`, miss_med=128): visited-bit +
  second-chance clock. Tied with size_aware. Reliable 2nd-tier baseline.
- **size_aware_ops** (`policy_size_aware_ops`, KFunc demo, miss_med=128):
  uses `s3fs_kfunc_ino_size()` for SMALL/BIG bias. Identical to SIEVE on
  this all-44B bench; mixed-size workloads see the difference.
- **S3-FIFO** (`policy_s3fifo_ops`, real 3-tier, miss_med=133): dropped
  to bottom after X-01/X-03 fixes. Ghost re-admit requires enough accesses
  to populate the ghost map and trigger re-admits — a 200-access workload
  is too short for the S/M/G structure to pay off.
- **pressure_ops** (`policy_pressure_ops`, KFunc demo, miss_med=119):
  **new single leader.** Reads `s3fs_kfunc_admit_count()` / `softcap()`
  live; strictness switch fires regularly when admit_count drifts above
  softcap during eviction cascades. Proves the KFunc-live-state shape with
  real performance benefit.
- **time_decay_ops** (`policy_time_decay_ops`, KFunc demo, miss_med=133):
  uses `s3fs_kfunc_time_ns()` for wall-clock decay (DECAY_NS=5s). Bottom
  group — DECAY_NS is too short for this workload pacing (≈6 ms/access ×
  200 = 1.2 s total, so most inos never reach the 5 s threshold). Proof-
  of-KFunc-time shape, not a production recommendation.
- **W-TinyLFU** (`policy_wtinylfu_ops`, scoped, miss_med=133): fell to
  bottom group after X-03 fix. CMS aging (AGE_INTERVAL_HITS=512) does
  not fire in 200 accesses, so frequency signal is absent; degrades to
  FIFO-like.
- **W-TinyLFU full** (`policy_wtinylfu_full_ops`, miss_med=129): window
  LRU + doorkeeper help slightly vs scoped variant (129 vs 133). Full
  variant leads on seed 7 (miss=117).
- **LRU** (`policy_lru_ops`, miss_med=130): seed-epoch FIFO with `on_hit`
  refresh. Close to freq2bit; both in the 130 miss band.
- **freq2bit** (`policy_freq2bit_ops`, miss_med=130): single-tier 2-bit
  counter. Renamed from the old `policy_s3fifo_*`; see §"Naming honesty".

**Policies add up to 14 miss reduction over `observe` (miss_med 133→119).**
That equals 14 fewer S3 upcalls per 200 accesses. The remaining gap to
Belady (miss 78–81 simulated) reflects the
per-round eviction granularity rather than victim selection quality.

## Reading W-TinyLFU

Our `policy_wtinylfu_ops` implements the *centerpiece* of the
paper (CMS-based frequency estimator + threshold admission)
plus the aging step. Two components are still deferred:

1. **Window LRU tier** (~1 % of cache in the paper). Without W-LRU,
   one-hit wonders admit straight to Main. In practice aging plus
   capacity-driven eviction covers most of this on our bench, but
   tail workloads (bursts of unique items) will still hurt.
2. **Doorkeeper Bloom filter**. Filters "1-hit wonders" before they
   populate CMS. On this bench (30 files × ~7 hits each) there are
   no true one-hit wonders, so the doorkeeper starves CMS rather
   than helping — which is why `wtinylfu_full_ops` edges below the
   scoped variant on most seeds.

**Aging is the trick that mattered.** Before `bpf_loop()` landed we
skipped the halving step because a pragma-unrolled 1024-iteration loop
exceeds the verifier's insn budget. `bpf_loop()` delegates iteration
to the kernel with a fixed runtime bound. Cost: one BPF helper call
per age; the callback itself is 3 insns.

The eviction totals tell the same story as the old numbers but with
smaller absolute values: ~130 for LRU/freq2bit, ~120 for S3-FIFO,
~127 for SIEVE — all similar because capacity mode forces every policy
to evict roughly `N_ACCESS - SOFTCAP` times.

## Naming honesty (Oct 2026)

Until commit cb9f201 the "S3-FIFO" built-in was actually a
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

4. Reproducibility: `SEED` env var controls workload sampling
   (default 42). `bench-hitrate-multi.sh` runs seeds 42, 1337, 7
   and reports median/min/max per policy. Observed inter-seed
   variance: up to ±8 pp (s3fifo on easy vs hard seeds).

5. Our S3-FIFO does not enforce paper-mandated |S|=10 %,
   |M|=90 % split explicitly. Softcap = 8 bounds the whole admit
   hash; the algorithm's own decisions leave a mix, but tighter
   size control would need per-tier admit accounting in kmod.
   Would be worth adding once softcap becomes tunable.

## What this unblocks

- P-07 objective met: numerical evidence that policy choice
  matters — `observe` (no ranking) 33.5 % vs `s3fifo` 39.5 % vs
  trace-replay `opt` 59.5–63 %, all at true 8-slot capacity.
  Policies add ~6 pp over FIFO; the remaining 20+ pp gap to Belady
  is eviction-granularity, not victim-selection.
- Meetup demo material: "swap the policy via a curl, see the hit
  rate change live." The POST endpoints (`/policy/reload` +
  `/policy/eval`) make that a curl-only demo. Present
  `observe → s3fifo` (+6 pp per run) or the trace-replay `opt`
  bound as the aspirational target.
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
