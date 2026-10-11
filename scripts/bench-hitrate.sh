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
#
# Capacity: by default the kmod runs in capacity mode (S-02) with
# CAPACITY_BYTES = SOFTCAP * FILE_BYTES, i.e. exactly SOFTCAP of the
# equal-size bench files. Policies then only rank victims and kmod evicts
# until the set fits, so every row is a SOFTCAP-slot hit rate and is
# comparable with the `opt` (Belady) reference at the end, which replays
# the identical trace ($OUT/trace.jsonl) through `dev trace-replay`.
# CAPACITY_BYTES=0 reproduces the old legacy vote mode, where SOFTCAP is
# only an eviction trigger and a policy that rarely votes "evict" keeps
# more files admitted (rates can then exceed `opt`). Each row also
# reports the admit-set size seen during the workload (adm_mean /
# adm_max) as a check.

set -euo pipefail

TOPDIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
# shellcheck source=lib/bench.sh
source "$TOPDIR/scripts/lib/bench.sh"
MOUNT=${MOUNT:-/mnt/s3demo}
METRICS_URL=${METRICS_URL:-http://localhost:9600/metrics}
BUCKET=${BUCKET:-s3fs-test}
N_FILES=${N_FILES:-30}          # working set size (softcap=8 → 22 evict candidates)
# Defaults match the recorded results in docs/journal/hit-rate-bench.md
# and `trace-replay`'s --zipf default.
N_ACCESS=${N_ACCESS:-200}       # workload length
ZIPF_ALPHA=${ZIPF_ALPHA:-0.7}   # skew — smaller = more uniform
SEED=${SEED:-42}                # random seed for workload generation
SOFTCAP=${SOFTCAP:-8}           # slots; passed to kmod (--softcap) and the reference sims
FILE_BYTES=44                   # size of each seeded hitrate_<N>.txt (see seed_bucket)
CAPACITY_BYTES=${CAPACITY_BYTES:-$((SOFTCAP * FILE_BYTES))}  # 0 = legacy vote mode
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

need_cmds docker curl python3
need_built
minio_require

# --- bucket seed ---------------------------------------------------------

# We stage into a bucket that has _only_ hitrate_<N>.txt so the daemon's
# entry list matches the working set exactly. Existing files (hello,
# big.bin, etc.) would still admit and skew softcap accounting.
seed_bucket() {
	log "clearing bucket $BUCKET and seeding $N_FILES hitrate files"
	docker exec "$MINIO_CONTAINER" mc rm --recursive --force \
		"local/$BUCKET" >/dev/null 2>&1 || true
	docker exec "$MINIO_CONTAINER" mc mb --ignore-existing \
		"local/$BUCKET" >/dev/null
	# One .txt per iteration index, distinct content so a hit is
	# obviously the cached copy.
	local i
	for i in $(seq 0 $((N_FILES-1))); do
		printf 'hitrate file %02d line1\nhitrate file %02d line2\n' "$i" "$i" \
			| docker exec -i "$MINIO_CONTAINER" \
				mc pipe "local/$BUCKET/hitrate_$i.txt" >/dev/null
	done
}

# --- workload ------------------------------------------------------------

# Emits N_ACCESS integers in [0, N_FILES) sampled from a Zipf(alpha)
# distribution with a fixed seed so all policies see the identical
# trace. First few indices dominate — that's the "hot working set"
# the policies compete over.
gen_workload() {
	python3 - "$N_FILES" "$N_ACCESS" "$ZIPF_ALPHA" "$SEED" <<'PY'
import random, sys
n, k, alpha, seed = int(sys.argv[1]), int(sys.argv[2]), float(sys.argv[3]), int(sys.argv[4])
random.seed(seed)
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
	ebpf_stop "$MOUNT"
}
trap cleanup EXIT

# Fresh cycle for each policy so admit hash / policy state don't leak.
start_stack() {
	local pol=$1
	cleanup
	ebpf_mount "$MOUNT"
	# Ready = prewarm complete: daemon logs "[admit] N entries".
	ebpf_start_daemon "$OUT/daemon-${pol}.log" \
		--policy-bpf="builtin:policy_${pol}" \
		--metrics-addr=:9600 \
		--sample-every=2s --sync-every="$SYNC_EVERY" \
		--softcap="$SOFTCAP" --cache-capacity="$CAPACITY_BYTES"
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
	local b_miss b_reload b_evict b_admits b_folio_hits b_read_iter_hits
	b_miss=$(read_counter s3fs_ebpf_read_miss_upcalls_total)
	b_reload=$(read_counter s3fs_ebpf_read_miss_errors_total)
	b_evict=$(read_counter s3fs_ebpf_policy_evictions_total)
	b_admits=$(read_counter s3fs_ebpf_admits_current)
	b_folio_hits=$(read_counter s3fs_ebpf_folio_hits_total)
	b_read_iter_hits=$(read_counter s3fs_ebpf_read_iter_hits_total)
	log "$pol: baseline miss=$b_miss err=$b_reload evict=$b_evict admits_current=$b_admits folio=$b_folio_hits iter=$b_read_iter_hits"
	# Effective cache size: admit-set samples taken at every interleave
	# point and at the end of the workload.
	local adm_sum=0 adm_n=0 adm_max=0 adm
	sample_admits() {
		adm=$(read_counter s3fs_ebpf_admits_current)
		adm=${adm:-0}
		adm=${adm%.*}
		adm_sum=$((adm_sum + adm))
		adm_n=$((adm_n + 1))
		[ "$adm" -gt "$adm_max" ] && adm_max=$adm
		return 0
	}

	log "$pol: running $N_ACCESS accesses (with in-workload eval every $INTERLEAVE)"
	local start_ns end_ns i=0 errs=0
	start_ns=$(date +%s%N)
	while read -r idx; do
		# `cat` triggers open+getattr+read → folio_hit if in arena,
		# upcall miss otherwise. Track non-zero exit so U-01 errs
		# column shows real failures, not misses miscounted as hits.
		if ! sudo cat "$MOUNT/hitrate_${idx}.txt" >/dev/null 2>&1; then
			errs=$((errs + 1))
		fi
		i=$((i + 1))
		# Interleave policy eval rounds so per-hit signal (CMS bump,
		# visited bit, freq counter) has a chance to influence
		# eviction decisions. Without this, policies that populate
		# state only on hit (W-TinyLFU) never see the eviction path
		# after convergence — the state was empty when the last
		# forced-eval round ran, so on_evict granted universal grace.
		if [ "$INTERLEAVE" -gt 0 ] && [ $((i % INTERLEAVE)) -eq 0 ]; then
			sample_admits
			curl -s -X POST "${METRICS_URL%/metrics}/policy/eval" >/dev/null
		fi
	done < "$OUT/workload.txt"
	end_ns=$(date +%s%N)
	sample_admits

	log "$pol: workload done, settling ${POST}s for counter mirror"
	sleep "$POST"

	local f_miss f_reload f_evict f_folio_hits f_read_iter_hits
	f_miss=$(read_counter s3fs_ebpf_read_miss_upcalls_total)
	# shellcheck disable=SC2034  # kept for symmetry with b_reload
	f_reload=$(read_counter s3fs_ebpf_read_miss_errors_total)
	f_evict=$(read_counter s3fs_ebpf_policy_evictions_total)
	f_folio_hits=$(read_counter s3fs_ebpf_folio_hits_total)
	f_read_iter_hits=$(read_counter s3fs_ebpf_read_iter_hits_total)

	local d_miss d_evict d_folio_hits d_read_iter_hits d_page_cache_hits total rate_arena rate_total ms_per
	d_miss=$((f_miss - b_miss))
	d_evict=$((f_evict - b_evict))
	d_folio_hits=$((f_folio_hits - b_folio_hits))
	d_read_iter_hits=$((f_read_iter_hits - b_read_iter_hits))
	# page-cache approx: iter_hits that were neither arena hits nor upcall misses.
	# Can be slightly negative due to counter-read races; floor at 0.
	d_page_cache_hits=$(( d_read_iter_hits - d_folio_hits - d_miss ))
	[ "$d_page_cache_hits" -lt 0 ] && d_page_cache_hits=0
	total=$N_ACCESS
	if [ "$total" -gt 0 ]; then
		rate_arena=$(awk "BEGIN{printf \"%.2f\", 100*$d_folio_hits/$total}")
		rate_total=$(awk "BEGIN{printf \"%.2f\", 100*($d_folio_hits+$d_page_cache_hits)/$total}")
	else
		rate_arena="-"
		rate_total="-"
	fi
	ms_per=$(awk "BEGIN{printf \"%.2f\", ($end_ns-$start_ns)/1000000/$N_ACCESS}")

	local adm_mean
	adm_mean=$(awk "BEGIN{printf \"%.1f\", $adm_sum/($adm_n>0?$adm_n:1)}")

	# folio=arena hits; pc=page-cache approx; rate_arena=folio/N; rate_total=(folio+pc)/N.
	printf 'RESULT policy=%-14s  folio=%5d  pc=%5d  miss=%5d  err=%3d  rate_arena=%6s%%  rate_total=%6s%%  evictions=%5d  adm_mean=%5s  adm_max=%3d  ms/access=%s\n' \
		"$pol" "$d_folio_hits" "$d_page_cache_hits" "$d_miss" "$errs" "$rate_arena" "$rate_total" "$d_evict" "$adm_mean" "$adm_max" "$ms_per" | tee -a "$OUT/summary.txt"
}

# Reference bounds on the exact trace the live runs used. trace-replay
# starts cold while the live bench starts prewarmed, so a warm start can
# add at most SOFTCAP hits (the first SOFTCAP distinct files) to `opt`.
reference_bounds() {
	{
		echo
		echo "REFERENCE (dev trace-replay on $OUT/trace.jsonl, softcap=$SOFTCAP, cold start;"
		echo "           live runs used capacity_bytes=$CAPACITY_BYTES — 0 means legacy mode, not slot-bounded)"
		echo "           prewarmed live runs: opt bound += at most $SOFTCAP/$N_ACCESS)"
		"$S3FS_BIN" dev trace-replay --trace="$OUT/trace.jsonl" \
			--softcap="$SOFTCAP" --policies=lru,sieve,opt,noevict
	} | tee -a "$OUT/summary.txt"
}

# --- main ----------------------------------------------------------------

: > "$OUT/summary.txt"
seed_bucket

# Materialise the trace once: the live loop and the reference sims read
# the same file, and it is kept with the results for later replay.
gen_workload > "$OUT/workload.txt"
awk '{ printf "{\"ts\":%d,\"ino\":%d,\"op\":\"read\"}\n", (NR-1)*1000000, 100+$1 }' \
	"$OUT/workload.txt" > "$OUT/trace.jsonl"

echo "=================================================================="
echo "hit-rate bench — $N_FILES files, Zipf α=$ZIPF_ALPHA, $N_ACCESS accesses, softcap=$SOFTCAP capacity_bytes=$CAPACITY_BYTES seed=$SEED"
echo "=================================================================="
for pol in $POLICIES; do
	run_one "$pol"
done
reference_bounds

echo
echo "==== summary ($OUT/summary.txt) ===="
cat "$OUT/summary.txt"
