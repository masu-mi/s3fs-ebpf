#!/usr/bin/env bash
# scripts/bench-hitrate.sh — hit-rate comparison across Layer 2 policies.
#
# What it measures: given a Zipf-skewed access pattern over N > softcap
# files, how much of a policy's admit set survives long enough to serve
# hits vs how much gets evicted before re-use? The differentiator is the
# on_hit hook (recency / freq / visited bit) — before that landed all
# policies collapsed to grace-round FIFO.
#
# Prereqs (script does not manage): MinIO up (`make compose-up`),
# kmod built (`make kmod`), daemon built (`make daemon`).
#
# What it does: for each policy in the loop, fully cycles the kmod +
# mount + daemon so state doesn't leak between runs, seeds
# hitrate_<N>.txt into the bucket, runs the workload, delta-reads
# /metrics for hits & misses, prints a comparison table.

set -euo pipefail

TOPDIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
MOUNT=${MOUNT:-/mnt/s3demo}
METRICS_URL=${METRICS_URL:-http://localhost:9600/metrics}
BUCKET=${BUCKET:-s3fs-test}
N_FILES=${N_FILES:-30}          # working set size (softcap=8 → 22 evict candidates)
N_ACCESS=${N_ACCESS:-400}       # workload length
ZIPF_ALPHA=${ZIPF_ALPHA:-0.9}   # skew — smaller = more uniform
# Softcap converges from N → 8 in 3 sync rounds (grace + clear +
# evict), so we need >=3 sync-every intervals of settle. Also lets
# evict-drain feedback loop remove evicted inos from daemon snapshot
# before we start measuring.
SETTLE=${SETTLE:-15}            # seconds between prewarm and workload
SYNC_EVERY=${SYNC_EVERY:-3s}    # daemon --sync-every value
POST=${POST:-8}                 # counter mirror lag after workload
INTERLEAVE=${INTERLEAVE:-25}    # POST /policy/eval every N accesses (0 disables)
POLICIES=${POLICIES:-observe freq2bit_ops lru_ops sieve_ops s3fifo_ops wtinylfu_ops wtinylfu_full_ops pressure_ops time_decay_ops size_aware_ops}
OUT=${OUT:-/tmp/bench-hitrate}

mkdir -p "$OUT"
log() { echo "[hitrate] $*" >&2; }

# --- prereq --------------------------------------------------------------

need() { command -v "$1" >/dev/null || { echo "missing $1"; exit 1; }; }
need docker
need curl
need python3
docker inspect s3fs-ebpf-minio >/dev/null 2>&1 \
	|| { echo "s3fs-ebpf-minio not running (make compose-up)"; exit 1; }
[ -f "$TOPDIR/kmod/s3fs_kfunc.ko" ] || { echo "kmod not built (make kmod)"; exit 1; }
[ -x "$TOPDIR/bin/s3fs-ebpf" ]      || { echo "daemon not built (make daemon)"; exit 1; }

# --- bucket seed ---------------------------------------------------------

# We stage into a bucket that has _only_ hitrate_<N>.txt so the daemon's
# entry list matches the working set exactly. Existing files (hello,
# big.bin, etc.) would still admit and skew softcap accounting.
seed_bucket() {
	log "clearing bucket $BUCKET and seeding $N_FILES hitrate files"
	docker exec s3fs-ebpf-minio mc rm --recursive --force \
		"local/$BUCKET" >/dev/null 2>&1 || true
	docker exec s3fs-ebpf-minio mc mb --ignore-existing \
		"local/$BUCKET" >/dev/null
	# One .txt per iteration index, distinct content so a hit is
	# obviously the cached copy.
	local i
	for i in $(seq 0 $((N_FILES-1))); do
		printf 'hitrate file %02d line1\nhitrate file %02d line2\n' "$i" "$i" \
			| docker exec -i s3fs-ebpf-minio \
				mc pipe "local/$BUCKET/hitrate_$i.txt" >/dev/null
	done
}

# --- workload ------------------------------------------------------------

# Emits N_ACCESS integers in [0, N_FILES) sampled from a Zipf(alpha)
# distribution with a fixed seed so all policies see the identical
# trace. First few indices dominate — that's the "hot working set"
# the policies compete over.
gen_workload() {
	python3 - "$N_FILES" "$N_ACCESS" "$ZIPF_ALPHA" <<'PY'
import random, sys
n, k, alpha = int(sys.argv[1]), int(sys.argv[2]), float(sys.argv[3])
random.seed(42)  # deterministic
w = [1.0/((i+1)**alpha) for i in range(n)]
tot = sum(w)
cum = []
c = 0.0
for x in w:
    c += x/tot
    cum.append(c)
for _ in range(k):
    r = random.random()
    lo, hi = 0, n-1
    while lo < hi:
        mid = (lo+hi)//2
        if cum[mid] < r: lo = mid+1
        else: hi = mid
    print(lo)
PY
}

# --- lifecycle -----------------------------------------------------------

cleanup() {
	sudo pkill -INT -f 's3fs-ebpf fastpath' 2>/dev/null || true
	# give SIGINT time to drain; then umount + rmmod
	local i
	for i in 1 2 3 4 5; do
		pgrep -f 's3fs-ebpf fastpath' >/dev/null || break
		sleep 0.5
	done
	mountpoint -q "$MOUNT" && sudo umount "$MOUNT" 2>/dev/null || true
	lsmod | grep -q '^s3fs_kfunc' && sudo rmmod s3fs_kfunc 2>/dev/null || true
}
trap cleanup EXIT

# Fresh cycle for each policy so admit hash / policy state don't leak.
start_stack() {
	local pol=$1
	cleanup
	sudo insmod "$TOPDIR/kmod/s3fs_kfunc.ko"
	sudo mkdir -p "$MOUNT"
	sudo mount -t s3fs_ebpf none "$MOUNT"
	LD_LIBRARY_PATH=$HOME/.local/lib sudo -E \
		"$TOPDIR/bin/s3fs-ebpf" fastpath --s3 \
		--policy-bpf="builtin:policy_${pol}" \
		--metrics-addr=:9600 \
		--sample-every=2s --sync-every="$SYNC_EVERY" \
		>"$OUT/daemon-${pol}.log" 2>&1 &
	# Wait for prewarm-complete: daemon logs "[admit] N entries".
	local i
	for i in $(seq 1 60); do
		grep -q '\[admit\] ' "$OUT/daemon-${pol}.log" && break
		sleep 0.2
	done
	log "$pol: prewarm done, forcing 4 policy eval rounds to converge below softcap"
	# SIEVE needs 3 rounds (grace → clear → evict); LRU / freq2bit
	# / S3-FIFO converge faster but do no harm here. Sleep between so the
	# evict-drain feedback loop (2s interval) removes evicted inos
	# from the daemon snapshot before the next round.
	local i
	for i in 1 2 3 4; do
		curl -s -X POST "${METRICS_URL%/metrics}/policy/eval" >/dev/null
		sleep 3
	done
	log "$pol: convergence done, settling ${SETTLE}s"
	sleep "$SETTLE"
}

read_counter() {
	local name=$1
	curl -s "$METRICS_URL" | awk -v n="$name" '$1==n { print $2 }'
}

# --- one policy run ------------------------------------------------------

run_one() {
	local pol=$1
	log "==== policy: $pol ===="
	start_stack "$pol"

	# What we actually measure: read_miss_upcalls fires when kmod-side
	# admit_snapshot() returns false in .read_folio and we go to path C
	# (daemon upcall + re-admit). This is the true cache miss, whereas
	# fast_path_hits/slow_path_misses count fentry hits on the BPF-side
	# inode_index which stays populated even after admit eviction.
	local b_miss b_reload b_evict b_admits
	b_miss=$(read_counter s3fs_ebpf_read_miss_upcalls_total)
	b_reload=$(read_counter s3fs_ebpf_read_miss_errors_total)
	b_evict=$(read_counter s3fs_ebpf_policy_evictions_total)
	b_admits=$(read_counter s3fs_ebpf_admits_current)
	log "$pol: baseline miss=$b_miss err=$b_reload evict=$b_evict admits_current=$b_admits"

	log "$pol: running $N_ACCESS accesses (with in-workload eval every $INTERLEAVE)"
	local start_ns end_ns i=0
	start_ns=$(date +%s%N)
	while read -r idx; do
		# `cat` triggers open+getattr+read → fast_hits (fentry on
		# simple_getattr) if in admit, slow_misses otherwise. Errors
		# ignored: after eviction the first re-cat blocks on upcall,
		# but doesn't fail.
		sudo cat "$MOUNT/hitrate_${idx}.txt" >/dev/null 2>&1 || true
		i=$((i + 1))
		# Interleave policy eval rounds so per-hit signal (CMS bump,
		# visited bit, freq counter) has a chance to influence
		# eviction decisions. Without this, policies that populate
		# state only on hit (W-TinyLFU) never see the eviction path
		# after convergence — the state was empty when the last
		# forced-eval round ran, so on_evict granted universal grace.
		if [ "$INTERLEAVE" -gt 0 ] && [ $((i % INTERLEAVE)) -eq 0 ]; then
			curl -s -X POST "${METRICS_URL%/metrics}/policy/eval" >/dev/null
		fi
	done < <(gen_workload)
	end_ns=$(date +%s%N)

	log "$pol: workload done, settling ${POST}s for counter mirror"
	sleep "$POST"

	local f_miss f_reload f_evict
	f_miss=$(read_counter s3fs_ebpf_read_miss_upcalls_total)
	f_reload=$(read_counter s3fs_ebpf_read_miss_errors_total)
	f_evict=$(read_counter s3fs_ebpf_policy_evictions_total)

	local d_miss d_evict d_hits total rate ms_per
	d_miss=$((f_miss - b_miss))
	d_evict=$((f_evict - b_evict))
	d_hits=$((N_ACCESS - d_miss))
	total=$N_ACCESS
	if [ "$total" -gt 0 ]; then
		rate=$(awk "BEGIN{printf \"%.2f\", 100*$d_hits/$total}")
	else
		rate="-"
	fi
	ms_per=$(awk "BEGIN{printf \"%.2f\", ($end_ns-$start_ns)/1000000/$N_ACCESS}")

	# One line per policy — the comparison table.
	printf 'RESULT policy=%-14s  hits=%5d  misses=%5d  rate=%6s%%  evictions=%5d  ms/access=%s\n' \
		"$pol" "$d_hits" "$d_miss" "$rate" "$d_evict" "$ms_per" | tee -a "$OUT/summary.txt"
}

# --- main ----------------------------------------------------------------

: > "$OUT/summary.txt"
seed_bucket

echo "=================================================================="
echo "hit-rate bench — $N_FILES files, Zipf α=$ZIPF_ALPHA, $N_ACCESS accesses"
echo "=================================================================="
for pol in $POLICIES; do
	run_one "$pol"
done

echo
echo "==== summary ($OUT/summary.txt) ===="
cat "$OUT/summary.txt"
