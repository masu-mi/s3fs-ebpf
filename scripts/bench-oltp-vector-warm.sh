#!/usr/bin/env bash
# bench-oltp-vector-warm.sh — warm steady state of the OLTP / vector loads per
# backend, with page-fault and CPU counters (rusage of the load process) and,
# for ebpf, the kmod counters before/after, to explain why ebpf-pcache does or
# does not beat s3fs-fuse use_cache+kernel_cache.  Each point: 1 priming pass,
# then REPS timed passes of N ops.
set -euo pipefail
TOPDIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
source "$TOPDIR/scripts/lib/bench.sh"
MOUNT=${MOUNT:-/mnt/s3demo}; MP_FUSE_CACHE=${MP_FUSE_CACHE:-/mnt/s3fuse-cache}; MP_FUSE_RAW=${MP_FUSE_RAW:-/mnt/s3fuse}
FUSE_CACHE_DIR=${FUSE_CACHE_DIR:-/tmp/s3fs-fuse-cache-ovw}; EBPF_CACHE_DIR=${EBPF_CACHE_DIR:-/tmp/s3fs-ebpf-cache-ovw}
OUT=${OUT:-/tmp/bench-oltp-vector-warm}; REPS=${REPS:-5}; SETTLE=${SETTLE:-8}
OLTP_N=${OLTP_N:-20000}; VEC_N=${VEC_N:-3000}; SEED=${SEED:-7}
EBPF_FLAGS=${EBPF_FLAGS:-}   # extra fastpath flags for ebpf variants
LOAD="$TOPDIR/scripts/lib/oltp-vector-load.py"; PY=$(command -v python3)
need_cmds jq python3 docker; need_built
minio_require ov_kv.db ov_vec.bin
mkdir -p "$OUT"; : >"$OUT/results.jsonl"
FC=0 FR=0
cleanup() { ebpf_stop "$MOUNT" || true; [ $FC = 1 ] && umount_hard "$MP_FUSE_CACHE" || true; [ $FR = 1 ] && umount_hard "$MP_FUSE_RAW" || true; }
trap cleanup EXIT
kc() { curl -s localhost:9600/metrics | awk '$1=="s3fs_ebpf_read_miss_upcalls_total"{u=$2} $1=="s3fs_ebpf_folio_hits_total"{h=$2} END{printf "{\"upcalls\":%d,\"folio_hits\":%d}", u, h}'; }
run() { # label mp
	local label=$1 mp=$2 w a rep res k=null
	for w in oltp vec; do for a in 0 1.1; do
		local obj n; [ $w = oltp ] && { obj=ov_kv.db; n=$OLTP_N; } || { obj=ov_vec.bin; n=$VEC_N; }
		sudo "$PY" "$LOAD" $w "$mp/$obj" "$n" "$a" "$SEED" >/dev/null   # prime
		for rep in $(seq "$REPS"); do
			res=$(sudo "$PY" "$LOAD" $w "$mp/$obj" "$n" "$a" "$SEED")
			jq -c --arg b "$label" --arg w $w --arg a $a --argjson r $rep '. + {backend:$b, workload:$w, alpha:$a, rep:$r}' <<<"$res" >>"$OUT/results.jsonl"
		done
	done; done
}
ebpf_variant() { # label flags...
	local label=$1; shift
	sudo rm -rf "$EBPF_CACHE_DIR"; mkdir -p "$EBPF_CACHE_DIR"
	ebpf_mount "$MOUNT"
	ebpf_start_daemon "$OUT/daemon-$label.log" --cache-dir="$EBPF_CACHE_DIR" --policy-bpf=builtin:policy_observe \
		--arena-metadata-only --metrics-addr=:9600 --sample-every=2s --sync-every=60s --softcap=8 --cache-capacity=0 "$@"
	sleep "$SETTLE"
	local before after; before=$(kc); run "$label" "$MOUNT"; after=$(kc)
	echo "$label kmod before=$before after=$after" >>"$OUT/kmod.txt"
	ebpf_stop "$MOUNT" || true
}
: >"$OUT/kmod.txt"
ebpf_variant ebpf-pcache
ebpf_variant ebpf-pcache-nohook --no-read-iter-hook --pattern-aware-admit=false
fuse_mount_cache "$MP_FUSE_CACHE" "$FUSE_CACHE_DIR"; FC=1; run fuse-cache "$MP_FUSE_CACHE"; umount_hard "$MP_FUSE_CACHE"; FC=0
fuse_mount_raw "$MP_FUSE_RAW"; FR=1; run fuse-raw "$MP_FUSE_RAW"
python3 -I - "$OUT/results.jsonl" <<'PY'
import json,sys,statistics as s,collections
d=collections.defaultdict(list)
for l in open(sys.argv[1]):
    r=json.loads(l); d[(r["workload"],r["alpha"],r["backend"])].append(r)
print("%-5s %-4s %-17s %9s %7s %8s %8s %9s %9s %7s"%("wl","a","backend","ops_s","p50us","p99us","minflt/op","utime_ms","stime_ms","cpu/op_us"))
for k in sorted(d):
    v=d[k]; m=lambda f: s.median(x[f] for x in v)
    n=v[0]["n"]
    print("%-5s %-4s %-17s %9.0f %7.1f %8.1f %8.2f %9.0f %9.0f %9.1f"%(*k,m("ops_s"),m("p50_us"),m("p99_us"),m("minflt")/n,m("utime_ms"),m("stime_ms"),(m("utime_ms")+m("stime_ms"))*1000/n))
PY
