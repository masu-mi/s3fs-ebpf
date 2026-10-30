#!/usr/bin/env bash
# bench-oltp-vector.sh — read-only OLTP (SQLite point lookups, plan H6) and
# vector-search (IVF-like mmap reads) loads on s3fs-ebpf (pcache) vs
# s3fs-fuse raw / cache, all against MinIO.  Each workload runs cold (page
# cache dropped) then warm.  Writes are out of scope (E-01 / AC-51).
#
# Prereqs: make compose-up; make all.  Objects total ~64 MiB so they fit the
# 128 MiB arena together with the seeds; they are removed on exit.
set -euo pipefail

TOPDIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
# shellcheck source=lib/bench.sh
source "$TOPDIR/scripts/lib/bench.sh"

MOUNT=${MOUNT:-/mnt/s3demo}
MP_FUSE_RAW=${MP_FUSE_RAW:-/mnt/s3fuse}
MP_FUSE_CACHE=${MP_FUSE_CACHE:-/mnt/s3fuse-cache}
FUSE_CACHE_DIR=${FUSE_CACHE_DIR:-/tmp/s3fs-fuse-cache-ov}
EBPF_CACHE_DIR=${EBPF_CACHE_DIR:-/tmp/s3fs-ebpf-cache-ov}
OUT=${OUT:-/tmp/bench-oltp-vector}
SETTLE=${SETTLE:-10}
DB_MB=${DB_MB:-32}
VEC_MB=${VEC_MB:-32}
OLTP_N=${OLTP_N:-5000}
VEC_N=${VEC_N:-300}
SEED=${SEED:-7}
COLD_DIV=${COLD_DIV:-10}
DB_OBJ=ov_kv.db
VEC_OBJ=ov_vec.bin
LOAD="$TOPDIR/scripts/lib/oltp-vector-load.py"
PY=$(command -v python3)

log() { echo "[oltp-vector] $*" >&2; }

need_cmds docker python3
need_built
minio_require hello.txt

sudo rm -rf "$EBPF_CACHE_DIR"
mkdir -p "$OUT" "$FUSE_CACHE_DIR" "$EBPF_CACHE_DIR"
: >"$OUT/results.jsonl"

FUSE_RAW_STARTED=0 FUSE_CACHE_STARTED=0
cleanup() {
	ebpf_stop "$MOUNT" || true
	[ "$FUSE_CACHE_STARTED" -eq 1 ] && umount_hard "$MP_FUSE_CACHE" || true
	[ "$FUSE_RAW_STARTED" -eq 1 ] && umount_hard "$MP_FUSE_RAW" || true
	docker exec "$MINIO_CONTAINER" mc rm "local/$S3_BUCKET/$DB_OBJ" "local/$S3_BUCKET/$VEC_OBJ" >/dev/null 2>&1 || true
}
trap cleanup EXIT

seed() {
	local d
	d=$(mktemp -d)
	log "generating ${DB_MB} MiB sqlite + ${VEC_MB} MiB vector file"
	"$PY" "$LOAD" gen-sqlite "$d/$DB_OBJ" "$DB_MB" >/dev/null
	"$PY" "$LOAD" gen-vec "$d/$VEC_OBJ" "$VEC_MB"
	local o
	for o in "$DB_OBJ" "$VEC_OBJ"; do
		docker exec -i "$MINIO_CONTAINER" mc pipe "local/$S3_BUCKET/$o" <"$d/$o" >/dev/null
	done
	rm -rf "$d"
}

# run_one <backend> <mp> <workload> <alpha>  — cold then warm, JSON lines out.
run_one() {
	local backend=$1 mp=$2 wl=$3 alpha=$4 obj n phase res
	if [ "$wl" = oltp ]; then obj=$DB_OBJ n=$OLTP_N; else obj=$VEC_OBJ n=$VEC_N; fi
	for phase in cold warm; do
		# cold = the first COLD_DIV-th of the ops after dropping the page cache,
		# so the run is not mostly warm by the end; warm = full run, cache primed.
		local nn=$n
		[ "$phase" = cold ] && { sync; sudo sysctl -q -w vm.drop_caches=3; nn=$((n / COLD_DIV)); }
		res=$(sudo "$PY" "$LOAD" "$wl" "$mp/$obj" "$nn" "$alpha" "$SEED")
		log "$backend $wl a=$alpha $phase $res"
		jq -c --arg b "$backend" --arg w "$wl" --arg a "$alpha" --arg p "$phase" \
			'. + {backend:$b, workload:$w, alpha:$a, phase:$p}' <<<"$res" >>"$OUT/results.jsonl"
	done
}

run_all() {
	local backend=$1 mp=$2 wl a
	for wl in oltp vec; do
		for a in 0 1.1; do run_one "$backend" "$mp" "$wl" "$a"; done
	done
}

seed

log "== s3fs-ebpf (pcache)"
ebpf_mount "$MOUNT"
ebpf_start_daemon "$OUT/daemon.log" --cache-dir="$EBPF_CACHE_DIR" \
	--policy-bpf="builtin:policy_observe" --arena-metadata-only \
	--metrics-addr=:9600 --sample-every=2s --sync-every=60s --softcap=8 --cache-capacity=0
sleep "$SETTLE"
run_all ebpf-pcache "$MOUNT"
ebpf_stop "$MOUNT" || true

log "== s3fs-fuse raw"
fuse_mount_raw "$MP_FUSE_RAW" && FUSE_RAW_STARTED=1
run_all fuse-raw "$MP_FUSE_RAW"
umount_hard "$MP_FUSE_RAW"; FUSE_RAW_STARTED=0

log "== s3fs-fuse cache"
fuse_mount_cache "$MP_FUSE_CACHE" "$FUSE_CACHE_DIR" && FUSE_CACHE_STARTED=1
run_all fuse-cache "$MP_FUSE_CACHE"

echo
jq -r '[.workload,.alpha,.phase,.backend,.ops_s,.p50_us,.p99_us]|@tsv' "$OUT/results.jsonl" \
	| sort -k1,1 -k2,2 -k3,3 -k4,4 | column -t -N workload,alpha,phase,backend,ops_s,p50_us,p99_us
