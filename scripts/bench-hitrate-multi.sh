#!/usr/bin/env bash
# bench-hitrate-multi.sh — run bench-hitrate.sh across multiple seeds and/or
# Zipf α values, then aggregate results (median ± range per policy).
#
# Usage examples:
#   # default: 3 seeds, single α=0.7
#   ./scripts/bench-hitrate-multi.sh
#
#   # Zipf α sweep (Y-03): 4 α values × 3 seeds
#   ALPHAS="0.5 0.7 0.9 1.3" ./scripts/bench-hitrate-multi.sh
#
#   # single seed, fast sweep
#   SEEDS="42" ALPHAS="0.5 0.7 0.9 1.3" ./scripts/bench-hitrate-multi.sh
#
# Output: MULTI_OUT/<alpha>/seed-<s>/summary.txt  (per-run)
#         MULTI_OUT/aggregate.txt                  (combined tables)

set -euo pipefail

TOPDIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
SEEDS=${SEEDS:-"42 1337 7"}
ALPHAS=${ALPHAS:-"0.7"}          # set to e.g. "0.5 0.7 0.9 1.3" for Y-03 sweep
MULTI_OUT=${MULTI_OUT:-/tmp/bench-hitrate-multi}

mkdir -p "$MULTI_OUT"

log() { echo "[multi-hitrate] $*" >&2; }

# Collect all summary.txt paths grouped by alpha: alpha_outs[alpha]="path1 path2 ..."
declare -A alpha_outs

for alpha in $ALPHAS; do
    alpha_dir="$MULTI_OUT/alpha-$alpha"
    mkdir -p "$alpha_dir"
    alpha_outs[$alpha]=""

    for s in $SEEDS; do
        out="$alpha_dir/seed-$s"
        log "=== α=$alpha seed=$s → $out ==="
        mkdir -p "$out"
        SEED="$s" ZIPF_ALPHA="$alpha" OUT="$out" bash "$TOPDIR/scripts/bench-hitrate.sh"
        alpha_outs[$alpha]+=" $out/summary.txt"
    done
done

# Write aggregate tables, one per α value.
AGG="$MULTI_OUT/aggregate.txt"
: > "$AGG"

python3 - "$AGG" "${!alpha_outs[@]}" <<'PY_EOF'
import sys, re, statistics

agg_path = sys.argv[1]
alpha_keys = sys.argv[2:]  # passed as separate args

# Re-read: we actually get alpha keys, then read them from a shell env trick.
# Rebuild alpha→files mapping by scanning MULTI_OUT passed via the directory
# structure encoded in the aggregate path.
import os

multi_out = os.path.dirname(agg_path)

# Discover all alpha dirs and their summary files.
alpha_dirs = sorted(d for d in os.listdir(multi_out)
                    if d.startswith("alpha-") and
                    os.path.isdir(os.path.join(multi_out, d)))

result_lines = []

for alpha_dir in alpha_dirs:
    alpha = alpha_dir[len("alpha-"):]
    dirpath = os.path.join(multi_out, alpha_dir)
    summaries = []
    for seed_dir in sorted(os.listdir(dirpath)):
        sf = os.path.join(dirpath, seed_dir, "summary.txt")
        if os.path.isfile(sf):
            summaries.append(sf)

    if not summaries:
        continue

    # Parse RESULT lines.
    # Format (Y-01 fix): hit_rate column replaces rate_total
    data = {}
    for f in summaries:
        with open(f) as fh:
            for line in fh:
                m = re.search(
                    r'policy=(\S+)\s+folio=\s*\d+\s+pc=\s*\d+\s+miss=\s*(\d+)\s+err=\s*\d+'
                    r'\s+rate_arena=\s*([\d.]+)%\s+hit_rate=\s*([\d.]+)%'
                    r'\s+evictions=\s*(\d+)\s+adm_mean=\s*([\d.]+)\s+adm_max=\s*(\d+)',
                    line)
                if m:
                    pol, miss, r_arena, hit_rate, evict, amean, amax = m.groups()
                    data.setdefault(pol, []).append(
                        (int(miss), float(hit_rate), float(r_arena),
                         int(evict), float(amean), int(amax)))

    header = (f"\n=== Zipf α={alpha}  seeds={len(summaries)} ===\n"
              f"{'policy':<22} {'miss_med':>8} {'miss_min':>8} {'miss_max':>8}"
              f" {'hit_rate_med':>12} {'rarena_med':>10} {'evict_med':>10}"
              f" {'adm_mean_med':>12}")
    sep = "-" * 102
    rows = []
    for pol, recs in sorted(data.items(), key=lambda x: statistics.median(r[0] for r in x[1])):
        misses    = [r[0] for r in recs]
        hit_rates = [r[1] for r in recs]
        arenas    = [r[2] for r in recs]
        evicts    = [r[3] for r in recs]
        ameans    = [r[4] for r in recs]
        med_miss  = statistics.median(misses)
        med_hr    = statistics.median(hit_rates)
        med_ra    = statistics.median(arenas)
        med_e     = statistics.median(evicts)
        med_am    = statistics.median(ameans)
        rows.append(
            f"{pol:<22} {med_miss:>8.0f}  {min(misses):>8}  {max(misses):>8}"
            f"  {med_hr:>10.1f}%  {med_ra:>9.2f}%  {med_e:>10.0f}  {med_am:>12.1f}")

    result_lines += [header, sep] + rows

output = "\n".join(result_lines) + "\n"
print(output)
with open(agg_path, "w") as f:
    f.write(output)
PY_EOF

log "aggregate written to $AGG"
echo
echo "==== aggregate ($AGG) ===="
cat "$AGG"
