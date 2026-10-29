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
# Thresholds come from the fixed build (seed 42, hit_accum=1, SYNC_EVERY=3600s,
# deterministic, with rare outliers): hit gap <= 0.5 pt for every policy (the faults above gave 4.2 pt for
# the ghost bound, 5.3 pt for the in-flight on_insert, ~6 pt for both).  The differing
# share is a ceiling just above what the build shows: lru 1.8 %, sieve 0 %,
# s3fifo 6.4 %, wtinylfu 6.2 %.  The remaining s3fifo / wtinylfu differences are the
# parts the simulators do not model: the kmod's rounds (a miss is tracked one round
# later), the pending shed, and wtinylfu's limbo / rejected queues.
set -euo pipefail

TOPDIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$TOPDIR"
POLICIES=${POLICIES:-"lru_pick_ops sieve_pick_ops s3fifo_pick_ops wtinylfu_pick_ops"}
SEED=${SEED:-42}
SOFTCAP=${SOFTCAP:-8}
N_ACCESS=${N_ACCESS:-2000}
HIT_TOL_PT=${HIT_TOL_PT:-3.0}
# The simulators see every access; the old direct on_hit signal drops some
# (one per jiffy per CPU and ino), so the comparison is meaningful only with
# hit_accum=1.  CONF_HIT_ACCUM=0 measures the old signal (gaps up to ~3.3 pt).
CONF_HIT_ACCUM=${CONF_HIT_ACCUM:-1}
# The daemon re-sends the bucket listing every --sync-every and runs an admit_replace
# round on each tick; those rounds fall at arbitrary moments between accesses and
# moved s3fifo's hit count by up to +-5 pt from run to run.  With the tick out of the
# way the run is deterministic (934 hits in 3 of 3 runs).
export SYNC_EVERY=${SYNC_EVERY:-3600s}
OUT=${OUT:-/tmp/test-policy-conformance}
rm -rf "$OUT"
mkdir -p "$OUT"

# A lockdep / debug kernel runs the kmod several times slower and the live hit
# counts drift away from the simulator (3.8-5.7 pt on 6.12.0-s3fs-lockdep, where
# the same build gives 0.2-2.2 pt on a normal kernel): the comparison is a timing
# test, not a lock test.  `make test-lockdep` has its own verdict; skip here.
if [ "${CONFORMANCE_ON_LOCKDEP:-0}" != 1 ] && sudo test -e /proc/lockdep_stats 2>/dev/null; then
	echo "RESULT name=test-policy-conformance status=SKIP reason=lockdep-kernel (CONFORMANCE_ON_LOCKDEP=1 to run)"
	exit 0
fi

ATTEMPTS=${ATTEMPTS:-3}

# run_bench <policies>: the live run, per-access log in $OUT/live.<policy>
run_bench() {
	# bench-hitrate.sh APPENDS to the per-access log: without this a retry would judge
	# the first attempt's first 2000 lines again and always repeat its verdict.
	local p
	for p in $1; do rm -f "$OUT/live.$p"; done
	HIT_ACCUM="$CONF_HIT_ACCUM" PER_ACCESS_LOG="$OUT/live" SEED="$SEED" SOFTCAP="$SOFTCAP" N_ACCESS="$N_ACCESS" \
		POLICIES="$1" DAEMON_EXTRA_FLAGS="--arena-metadata-only" OUT="$OUT/bench" \
		"$TOPDIR/scripts/bench-hitrate.sh" >"$OUT/bench.log" 2>&1 || {
		tail -20 "$OUT/bench.log" >&2
		echo "RESULT name=test-policy-conformance status=FAIL reason=bench-failed"
		exit 1
	}
}

# judge <policy>: prints "ok|FAIL live_hits=.. sim_hits=.. gap_pt=.. differ=.. (..%)"
judge() {
	local pol=$1 sim dmax trace
	sim=${pol%_pick_ops}
	# the kernel's own inode numbers (the W-TinyLFU sketch hashes them)
	trace="$OUT/bench/trace-ino-$pol.jsonl"
	[ -s "$trace" ] || trace="$OUT/bench/trace.jsonl"
	"$TOPDIR/bin/s3fs-ebpf" dev trace-replay --trace="$trace" \
		--softcap="$SOFTCAP" --policies="$sim" --per-access="$OUT/sim" >/dev/null
	case "$sim" in
	lru) dmax=${DIFF_MAX_PCT_LRU:-4} ;;
	s3fifo) dmax=${DIFF_MAX_PCT_S3FIFO:-9} ;;
	wtinylfu) dmax=${DIFF_MAX_PCT_WTINYLFU:-9} ;;
	sieve) dmax=${DIFF_MAX_PCT_SIEVE:-2} ;;
	*) dmax=${DIFF_MAX_PCT:-12} ;;
	esac
	python3 - "$OUT/live.$pol" "$OUT/sim.$sim" "$HIT_TOL_PT" "$dmax" <<'PY'
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
}

echo "==> test-policy-conformance (policies: $POLICIES, HIT_ACCUM=$CONF_HIT_ACCUM, up to $ATTEMPTS attempts)"
# A real fault (wrong ghost bound, an in-flight miss on_insert'ed) fails every attempt;
# a timing outlier (s3fifo up to +4.7 pt, wtinylfu +2.5 pt, a few runs in ten: the daemon's
# 2 s eviction-drain / stats ticks fall between accesses) does not repeat.
pending=$POLICIES
declare -A result
for attempt in $(seq 1 "$ATTEMPTS"); do
	run_bench "$pending"
	still=""
	for pol in $pending; do
		res=$(judge "$pol")
		result[$pol]="$res"
		echo "  $pol (attempt $attempt): $res"
		case "$res" in FAIL*) still="$still $pol" ;; esac
	done
	pending=${still# }
	[ -z "$pending" ] && break
done

status=PASS
detail=""
for pol in $POLICIES; do
	detail="$detail $pol=${result[$pol]%% *}"
	case "${result[$pol]}" in FAIL*) status=FAIL ;; esac
done
echo "RESULT name=test-policy-conformance status=$status$detail"
[ "$status" = PASS ]
