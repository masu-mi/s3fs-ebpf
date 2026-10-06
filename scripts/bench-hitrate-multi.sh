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

# Aggregate: for each policy compute median rate across seeds.
python3 - "${seed_outs[@]}" <<'PY'
import sys, re, statistics

files = sys.argv[1:]
# policy -> list of (rate, evictions, adm_mean, adm_max)
data = {}
for f in files:
    with open(f) as fh:
        for line in fh:
            m = re.search(r'policy=(\S+)\s+hits=\s*\d+\s+misses=\s*\d+\s+rate=\s*([\d.]+)%\s+evictions=\s*(\d+)\s+adm_mean=\s*([\d.]+)\s+adm_max=\s*(\d+)', line)
            if m:
                pol, rate, evict, amean, amax = m.groups()
                data.setdefault(pol, []).append((float(rate), int(evict), float(amean), int(amax)))

print(f"{'policy':<22} {'rate_med':>8} {'rate_min':>8} {'rate_max':>8} {'evict_med':>10} {'adm_mean_med':>12} {'adm_max_med':>12}")
print("-" * 90)
for pol, rows in sorted(data.items(), key=lambda x: -statistics.median(r[0] for r in x[1])):
    rates = [r[0] for r in rows]
    evicts = [r[1] for r in rows]
    ameans = [r[2] for r in rows]
    amaxs  = [r[3] for r in rows]
    med_r  = statistics.median(rates)
    med_e  = statistics.median(evicts)
    med_am = statistics.median(ameans)
    med_ax = statistics.median(amaxs)
    print(f"{pol:<22} {med_r:>7.1f}%  {min(rates):>7.1f}%  {max(rates):>7.1f}%  {med_e:>10.0f}  {med_am:>12.1f}  {med_ax:>12.0f}")
PY
