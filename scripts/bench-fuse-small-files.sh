#!/usr/bin/env bash
# bench-fuse-small-files.sh — raw per-operation data for s3fs-fuse (raw / use_cache)
# on (1) reading N small files once each (cold, then warm) and (2) creating N new
# files with write+fsync+close.  Keeps the CSVs and an environment record under
# $OUT so another backend (e.g. s3fs-ebpf) can be compared against the same data.
# Prereqs: make compose-up; /etc/passwd-s3fs; s3fs installed.
set -euo pipefail
TOPDIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
source "$TOPDIR/scripts/lib/bench.sh"

N_READ=${N_READ:-10000}; N_CREATE=${N_CREATE:-1000}; SIZE=${SIZE:-4096}
PREFIX=${PREFIX:-smallfiles}; NEWPFX=${NEWPFX:-fsync_new}
MP_FUSE_RAW=${MP_FUSE_RAW:-/mnt/s3fuse}; MP_FUSE_CACHE=${MP_FUSE_CACHE:-/mnt/s3fuse-cache}
FUSE_CACHE_DIR=${FUSE_CACHE_DIR:-/tmp/s3fs-fuse-cache-sf}
OUT=${OUT:-/tmp/bench-fuse-small-files}
LOAD="$TOPDIR/scripts/lib/small-files-load.py"; PY=$(command -v python3)
log() { echo "[small-files] $*" >&2; }
need_cmds docker python3; need_built 2>/dev/null || true; minio_require hello.txt
mkdir -p "$OUT"

cleanup() {
	umount_hard "$MP_FUSE_CACHE" || true; umount_hard "$MP_FUSE_RAW" || true
	docker exec "$MINIO_CONTAINER" mc rm --recursive --force "local/$S3_BUCKET/$PREFIX/" "local/$S3_BUCKET/$NEWPFX/" >/dev/null 2>&1 || true
	rm -rf "$TOPDIR/.sf-seed" 2>/dev/null || true
}
trap cleanup EXIT

seed() {
	local d=/tmp/sf-seed
	rm -rf "$d"; "$PY" "$LOAD" gen "$d/$PREFIX" "$N_READ" "$SIZE"
	docker exec "$MINIO_CONTAINER" rm -rf /tmp/sf-seed
	docker cp "$d" "$MINIO_CONTAINER:/tmp/sf-seed"
	docker exec "$MINIO_CONTAINER" mc rm --recursive --force "local/$S3_BUCKET/$NEWPFX/" >/dev/null 2>&1 || true
	docker exec "$MINIO_CONTAINER" mc mirror --overwrite "/tmp/sf-seed/$PREFIX" "local/$S3_BUCKET/$PREFIX" >/dev/null
	docker exec "$MINIO_CONTAINER" rm -rf /tmp/sf-seed
	rm -rf "$d"
	log "seeded $N_READ x ${SIZE}B under s3://$S3_BUCKET/$PREFIX/"
}

drop() { sync; sudo sysctl -q -w vm.drop_caches=3; }

run_backend() { # label mp
	local b=$1 mp=$2 o="$OUT/$1"
	mkdir -p "$o"
	drop
	log "$b: cold cat x$N_READ"
	"$PY" "$LOAD" cat "$mp/$PREFIX" "$N_READ" "$o/cat_cold.csv"
	log "$b: warm cat x$N_READ (same mount, page cache kept)"
	"$PY" "$LOAD" cat "$mp/$PREFIX" "$N_READ" "$o/cat_warm.csv"
	drop
	log "$b: cat via 'xargs -n1 cat' (fork+exec per file, after drop_caches)"
	local t0 t1
	t0=$(date +%s%N)
	find "$mp/$PREFIX" -type f -print0 | sort -z | xargs -0 -n1 cat >/dev/null
	t1=$(date +%s%N)
	echo "$((t1 - t0))" >"$o/cat_xargs_wall_ns.txt"
	log "$b: new file create+write+fsync+close x$N_CREATE"
	"$PY" "$LOAD" create "$mp/$NEWPFX" "$N_CREATE" "$SIZE" "$o/create_fsync.csv"
	grep -E " $mp " /proc/mounts >"$o/mount_options.txt" || true
}

{
	echo "date_utc: $(date -u +%FT%TZ)"
	echo "git: $(git -C "$TOPDIR" rev-parse HEAD)"
	echo "kernel: $(uname -srv)"
	echo "cpus: $(nproc)"
	echo "s3fs: $(s3fs --version 2>&1 | head -1)"
	echo "minio_image: $(docker inspect -f '{{.Config.Image}}' "$MINIO_CONTAINER")"
	echo "n_read: $N_READ  n_create: $N_CREATE  size: $SIZE"
	echo "fuse_raw_opts: url=$S3_URL use_path_request_style allow_other umask=0022 (lib/bench.sh fuse_mount)"
	echo "fuse_cache_opts: + use_cache=$FUSE_CACHE_DIR kernel_cache max_stat_cache_size=100000"
} >"$OUT/environment.txt"

seed
log "== fuse-raw"
fuse_mount_raw "$MP_FUSE_RAW"; run_backend fuse-raw "$MP_FUSE_RAW"; umount_hard "$MP_FUSE_RAW"
docker exec "$MINIO_CONTAINER" mc rm --recursive --force "local/$S3_BUCKET/$NEWPFX/" >/dev/null 2>&1 || true
log "== fuse-cache"
fuse_mount_cache "$MP_FUSE_CACHE" "$FUSE_CACHE_DIR"; run_backend fuse-cache "$MP_FUSE_CACHE"

python3 -I - "$OUT" <<'PY'
import csv, sys, os
out = sys.argv[1]
def pct(v, p): v = sorted(v); return v[min(len(v)-1, int(len(v)*p))]
print("%-10s %-14s %6s %10s %10s %10s %10s" % ("backend", "dataset", "n", "total_s", "p50_us", "p99_us", "max_us"))
for b in ("fuse-raw", "fuse-cache"):
    for name, col in (("cat_cold", "total_ns"), ("cat_warm", "total_ns"), ("create_fsync", "total_ns"), ("create_fsync(fsync)", "fsync_ns")):
        f = name.split("(")[0] + ".csv"
        p = os.path.join(out, b, f)
        v = [int(r[col]) for r in csv.DictReader(open(p))]
        print("%-10s %-14s %6d %10.2f %10.1f %10.1f %10.1f" % (b, name, len(v), sum(v)/1e9, pct(v, .5)/1e3, pct(v, .99)/1e3, max(v)/1e3))
    w = int(open(os.path.join(out, b, "cat_xargs_wall_ns.txt")).read())
    print("%-10s %-14s %6s %10.2f" % (b, "cat_xargs", "-", w/1e9))
PY
