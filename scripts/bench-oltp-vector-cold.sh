#!/usr/bin/env bash
# bench-oltp-vector-cold.sh — what "cold" costs on each backend.
#   local  : vm.drop_caches=3 only (daemon disk cache / fuse use_cache stay warm)
#   s3     : plus wiped backend caches and a fresh mount/daemon (MinIO must be hit)
# Each rep: prepare the cold state, then ONE oltp run (a=1.1, COLD_OPS ops) and
# ONE vec run, reporting wall time for open+all ops (not just the op loop), so
# a prefetch absorbed by open() is not hidden.  Also dumps kmod fd_buf_bytes and
# upcall counters for ebpf.  Prereq: objects ov_kv.db/ov_vec.bin are seeded by
# bench-oltp-vector.sh (this script seeds them itself when missing).
set -euo pipefail
TOPDIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
source "$TOPDIR/scripts/lib/bench.sh"

MOUNT=${MOUNT:-/mnt/s3demo}; MP_FUSE_RAW=${MP_FUSE_RAW:-/mnt/s3fuse}; MP_FUSE_CACHE=${MP_FUSE_CACHE:-/mnt/s3fuse-cache}
FUSE_CACHE_DIR=${FUSE_CACHE_DIR:-/tmp/s3fs-fuse-cache-ovc}; EBPF_CACHE_DIR=${EBPF_CACHE_DIR:-/tmp/s3fs-ebpf-cache-ovc}
OUT=${OUT:-/tmp/bench-oltp-vector-cold}; REPS=${REPS:-5}; SETTLE=${SETTLE:-8}
COLD_OPS=${COLD_OPS:-500}; COLD_VEC=${COLD_VEC:-30}; SEED=${SEED:-7}
LOAD="$TOPDIR/scripts/lib/oltp-vector-load.py"; PY=$(command -v python3)
DB_OBJ=ov_kv.db; VEC_OBJ=ov_vec.bin
log() { echo "[cold] $*" >&2; }
need_cmds docker jq python3; need_built; minio_require hello.txt
mkdir -p "$OUT"; : >"$OUT/results.jsonl"

if ! docker exec "$MINIO_CONTAINER" mc stat "local/$S3_BUCKET/$DB_OBJ" >/dev/null 2>&1; then
	d=$(mktemp -d)
	"$PY" "$LOAD" gen-sqlite "$d/$DB_OBJ" 32 >/dev/null; "$PY" "$LOAD" gen-vec "$d/$VEC_OBJ" 32
	for o in $DB_OBJ $VEC_OBJ; do docker exec -i "$MINIO_CONTAINER" mc pipe "local/$S3_BUCKET/$o" <"$d/$o" >/dev/null; done
	rm -rf "$d"
fi

FR=0 FC=0
cleanup() { bench_down; }
trap cleanup EXIT

drop() { sync; sudo sysctl -q -w vm.drop_caches=3; }
kstat() { # ebpf only: upcalls + kmod buffer bytes
	sleep 2.5
	curl -s localhost:9600/metrics | awk '$1=="s3fs_ebpf_read_miss_upcalls_total"{u=$2} $1=="s3fs_ebpf_fd_buf_bytes"{b=$2} END{printf "{\"upcalls\":%d,\"kmod_buf\":%d}", u, b}'
}
one() { # backend scenario rep mp
	local b=$1 sc=$2 rep=$3 mp=$4 w res t0 t1 extra=null
	for w in oltp vec; do
		local obj n; [ $w = oltp ] && { obj=$DB_OBJ; n=$COLD_OPS; } || { obj=$VEC_OBJ; n=$COLD_VEC; }
		t0=$(date +%s%N)
		res=$(sudo "$PY" "$LOAD" $w "$mp/$obj" "$n" 1.1 "$SEED")
		t1=$(date +%s%N)
		case $b in ebpf-*) extra=$(kstat) ;; esac
		log "$b $sc #$rep $w wall=$(( (t1 - t0) / 1000000 ))ms $res $extra"
		jq -c --arg b $b --arg s $sc --arg w $w --argjson r $rep --argjson wall "$(( (t1 - t0) / 1000000 ))" --argjson x "$extra" \
			'. + {backend:$b, scenario:$s, workload:$w, rep:$r, wall_ms:$wall, k:$x}' <<<"$res" >>"$OUT/results.jsonl"
	done
}

BACKENDS=${BACKENDS:-$BENCH_BACKENDS_ALL}
up() { bench_up "$1" "$OUT/daemon-$1.log" --sample-every=2s --sync-every=60s --softcap=8 --cache-capacity=0
       [ "$BENCH_KIND" = ebpf ] && sleep "$SETTLE"; return 0; }
for b in $BACKENDS; do
	log "== $b"
	for rep in $(seq "$REPS"); do                 # s3-cold: fresh mount/daemon, empty caches
		up "$b"; drop; one "$b" s3 "$rep" "$BENCH_MP"
	done
	up "$b"                                       # local-cold: backend caches stay warm
	sudo python3 "$LOAD" oltp "$BENCH_MP/$DB_OBJ" 10 1.1 1 >/dev/null; sudo python3 "$LOAD" vec "$BENCH_MP/$VEC_OBJ" 3 1.1 1 >/dev/null
	for rep in $(seq "$REPS"); do drop; one "$b" local "$rep" "$BENCH_MP"; done
	bench_down
done

echo
jq -r '[.workload,.scenario,.backend,.rep,.wall_ms,.open_us,.p50_us,.p99_us,(.k.upcalls//"-"),(.k.kmod_buf//"-")]|@tsv' "$OUT/results.jsonl" \
	| sort -k1,1 -k2,2 -k3,3 -k4,4n | column -t -N workload,scenario,backend,rep,wall_ms,open_us,p50_us,p99_us,upcalls,kmod_buf
