#!/usr/bin/env bash
# bench-hitrate-multi.sh — run bench-hitrate.sh with multiple seeds and
# aggregate results (median ± range per policy).
#
# Usage: SEEDS="42 1337 7" ./scripts/bench-hitrate-multi.sh
# Output: /tmp/bench-hitrate-multi/aggregate.txt  (and per-seed dirs)

set -euo pipefail

TOPDIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
SEEDS=${SEEDS:-"42 1337 7"}
MULTI_OUT=${MULTI_OUT:-/tmp/bench-hitrate-multi}

mkdir -p "$MULTI_OUT"

log() { echo "[multi-hitrate] $*" >&2; }

seed_outs=()
for s in $SEEDS; do
    OUT="$MULTI_OUT/seed-$s"
    log "=== seed $s → $OUT ==="
    mkdir -p "$OUT"
    SEED="$s" OUT="$OUT" bash "$TOPDIR/scripts/bench-hitrate.sh"
    seed_outs+=("$OUT/summary.txt")
done

# Aggregate: for each policy compute median miss/rate_total across seeds.
# Parses the U-02 RESULT format:
#   RESULT policy=X  folio=N  pc=N  miss=N  err=N  rate_arena=R%  rate_total=R%  evictions=N  adm_mean=N  adm_max=N  ms/access=N
python3 - "${seed_outs[@]}" <<'PY'
import sys, re, statistics

files = sys.argv[1:]
# policy -> list of (miss, rate_total, rate_arena, evictions, adm_mean, adm_max)
data = {}
for f in files:
    with open(f) as fh:
        for line in fh:
            m = re.search(
                r'policy=(\S+)\s+folio=\s*\d+\s+pc=\s*\d+\s+miss=\s*(\d+)\s+err=\s*\d+'
                r'\s+rate_arena=\s*([\d.]+)%\s+rate_total=\s*([\d.]+)%'
                r'\s+evictions=\s*(\d+)\s+adm_mean=\s*([\d.]+)\s+adm_max=\s*(\d+)', line)
            if m:
                pol, miss, r_arena, r_total, evict, amean, amax = m.groups()
                data.setdefault(pol, []).append(
                    (int(miss), float(r_total), float(r_arena), int(evict), float(amean), int(amax)))

# Sort by median miss ascending (fewer misses = better)
print(f"{'policy':<22} {'miss_med':>8} {'miss_min':>8} {'miss_max':>8} {'rtotal_med':>10} {'rarena_med':>10} {'evict_med':>10} {'adm_mean_med':>12}")
print("-" * 100)
for pol, rows in sorted(data.items(), key=lambda x: statistics.median(r[0] for r in x[1])):
    misses     = [r[0] for r in rows]
    r_totals   = [r[1] for r in rows]
    r_arenas   = [r[2] for r in rows]
    evicts     = [r[3] for r in rows]
    ameans     = [r[4] for r in rows]
    med_miss   = statistics.median(misses)
    med_rt     = statistics.median(r_totals)
    med_ra     = statistics.median(r_arenas)
    med_e      = statistics.median(evicts)
    med_am     = statistics.median(ameans)
    print(f"{pol:<22} {med_miss:>8.0f}  {min(misses):>8}  {max(misses):>8}  {med_rt:>9.1f}%  {med_ra:>9.2f}%  {med_e:>10.0f}  {med_am:>12.1f}")
PY
