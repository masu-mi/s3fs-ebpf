#!/usr/bin/env bash
# test-policy-conformance.sh — a live policy must behave like its reference.
#
# Runs bench-hitrate.sh (pcache mode, capacity = SOFTCAP seeded files) with a
# per-access hit/miss log for each pick_victim policy, replays the same trace
# through the matching simulator (`dev trace-replay --per-access`, same
# capacity) and compares:
#   hits    |live hits - simulator hits| <= HIT_TOL_PT (3.5) points of N_ACCESS
#   differ  accesses where the two disagree <= a per-policy share of N_ACCESS
# A live policy that keeps or drops the wrong inos shows up in both.  This
# catches what unit tests cannot: a bench capacity one slot short, a policy
# state that drifts from the kmod's (an in-flight miss on_insert'ed, a ghost
# queue bound that is too loose).  All three hid behind a 7-point hit-rate
# gap in issue #22.
#
#   scripts/test-policy-conformance.sh                    # all four policies
#   POLICIES="s3fifo_pick_ops" scripts/test-policy-conformance.sh
#
# Thresholds come from the fixed build (seed 42, hit_accum=1): the hit gap was
# <= 2.2 pt for every policy; the faults above gave 4.2 pt (ghost bound),
# 5.3 pt (in-flight on_insert) and 6 pt (both).  The differing share is a
# per-policy ceiling just above what the build shows (lru 2.7 %, s3fifo
# 9.3 %, wtinylfu 16.5 %, sieve 20.2 %).  sieve and wtinylfu differ from their
# simulators on ~1 access in 5 without losing hit rate: the BPF versions are
# not step-for-step the simulators (open item, issue #22 notes).
set -euo pipefail

TOPDIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$TOPDIR"
POLICIES=${POLICIES:-"lru_pick_ops sieve_pick_ops s3fifo_pick_ops wtinylfu_pick_ops"}
SEED=${SEED:-42}
SOFTCAP=${SOFTCAP:-8}
N_ACCESS=${N_ACCESS:-2000}
HIT_TOL_PT=${HIT_TOL_PT:-3.5}
# The simulators see every access; the old direct on_hit signal drops some
# (one per jiffy per CPU and ino), so the comparison is meaningful only with
# hit_accum=1.  CONF_HIT_ACCUM=0 measures the old signal (gaps up to ~3.3 pt).
CONF_HIT_ACCUM=${CONF_HIT_ACCUM:-1}
OUT=${OUT:-/tmp/test-policy-conformance}
rm -rf "$OUT"
mkdir -p "$OUT"

echo "==> test-policy-conformance (policies: $POLICIES, HIT_ACCUM=$CONF_HIT_ACCUM)"
HIT_ACCUM="$CONF_HIT_ACCUM" PER_ACCESS_LOG="$OUT/live" SEED="$SEED" SOFTCAP="$SOFTCAP" N_ACCESS="$N_ACCESS" \
	POLICIES="$POLICIES" DAEMON_EXTRA_FLAGS="--arena-metadata-only" OUT="$OUT/bench" \
	"$TOPDIR/scripts/bench-hitrate.sh" >"$OUT/bench.log" 2>&1 || {
	tail -20 "$OUT/bench.log" >&2
	echo "RESULT name=test-policy-conformance status=FAIL reason=bench-failed"
	exit 1
}

status=PASS
detail=""
for pol in $POLICIES; do
	sim=${pol%_pick_ops}
	"$TOPDIR/bin/s3fs-ebpf" dev trace-replay --trace="$OUT/bench/trace.jsonl" \
		--softcap="$SOFTCAP" --policies="$sim" --per-access="$OUT/sim" >/dev/null
	case "$sim" in
	lru) dmax=${DIFF_MAX_PCT_LRU:-6} ;;
	s3fifo) dmax=${DIFF_MAX_PCT_S3FIFO:-13} ;;
	wtinylfu) dmax=${DIFF_MAX_PCT_WTINYLFU:-22} ;;
	sieve) dmax=${DIFF_MAX_PCT_SIEVE:-26} ;;
	*) dmax=${DIFF_MAX_PCT:-12} ;;
	esac
	res=$(python3 - "$OUT/live.$pol" "$OUT/sim.$sim" "$HIT_TOL_PT" "$dmax" <<'PY'
import sys
live_f, sim_f, tol, dmax = sys.argv[1], sys.argv[2], float(sys.argv[3]), float(sys.argv[4])
sim = [l.split()[2] for l in open(sim_f)]
prev, live = 0, []
for l in open(live_f):
    m = int(l.split()[2])          # cumulative miss counter after each access
    live.append('M' if m > prev else 'H')
    prev = m
n = min(len(sim), len(live))
lh, sh = live[:n].count('H'), sim[:n].count('H')
diff = sum(1 for i in range(n) if live[i] != sim[i])
gap = abs(lh - sh) * 100.0 / n
dpct = diff * 100.0 / n
ok = gap <= tol and dpct <= dmax
print(f"{'ok' if ok else 'FAIL'} live_hits={lh} sim_hits={sh} gap_pt={gap:.1f} differ={diff} ({dpct:.1f}%)")
PY
)
	echo "  $pol: $res"
	detail="$detail $pol=${res%% *}"
	case "$res" in FAIL*) status=FAIL ;; esac
done
echo "RESULT name=test-policy-conformance status=$status$detail"
[ "$status" = PASS ]
