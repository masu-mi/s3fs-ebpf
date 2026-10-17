#!/usr/bin/env bash
# scripts/bench-pcache-remeasure.sh — §18.26+: pcache モード (--arena-metadata-only)
# で、arena モード時代の数字 (§11) を測り直し、既存オブジェクトへの途中 write・
# cold read・小さいファイル大量の項目を足す。
#
#   STEPS="A B C D E" で実行する項目を選ぶ (既定は全部)。
#   A  open+read+close p50 (microbench): fuse-raw / ebpf-nocache / fuse-cache / ebpf / mount-s3
#   B  durable write (dd conv=fsync) 1/10/100 MiB、新規ファイル
#   C  既存オブジェクトの途中への 4 KiB write + fsync (1/10/100 MiB)。fuse-raw と比較
#   D  cold read (1 回目) 10/100/300 MiB。fuse-raw / fuse-cache / mount-s3 / ebpf
#   E  小さいファイル 10,000 個 (4 KiB): ls -l / cat 全部 (cold と warm)
#
# 出力は $OUT/*.txt (生の値)。中央値は awk で出す。
set -euo pipefail

TOPDIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
BENCH_BUCKET=${BENCH_BUCKET:-s3fs-bench-s18}
SMALL_BUCKET=${SMALL_BUCKET:-s3fs-bench-small}
S3_BUCKET=$BENCH_BUCKET
# shellcheck source=lib/bench.sh
source "$TOPDIR/scripts/lib/bench.sh"

OUT=${OUT:-/tmp/bench-pcache-remeasure}
STEPS=${STEPS:-"A B C D E"}
MP_EBPF=/mnt/s3demo
MP_FUSE_RAW=/mnt/s3fuse
MP_FUSE_CACHE=/mnt/s3fuse-cache
MP_MOUNT_S3=/mnt/s3-mountpoint
FUSE_CACHE_DIR=/tmp/s3fs-fuse-cache-s18
EBPF_CACHE_DIR=/tmp/s3fs-ebpf-cache-s18
MOUNT_S3_CACHE_DIR=/tmp/mount-s3-cache-s18
MICRO_ITERS=${MICRO_ITERS:-5000}
RUNS_W=${RUNS_W:-5}
RUNS_COLD=${RUNS_COLD:-3}
SMALL_N=${SMALL_N:-10000}
MIB=$((1024 * 1024))
METRICS_URL=${METRICS_URL:-http://localhost:9600/metrics}

mkdir -p "$OUT"
log() { echo "[s18b] $*" >&2; }
need_cmds docker dd sha256sum hyperfine
need_built
mc() { docker exec "$MINIO_CONTAINER" mc "$@"; }
minio_require
now() { echo "$EPOCHREALTIME"; }
# ms_since <start>
ms_since() { awk -v a="$1" -v b="$EPOCHREALTIME" 'BEGIN{printf "%.1f", (b-a)*1000}'; }
median() { sort -n | awk '{a[NR]=$1} END{ if(NR==0){print "n/a"} else if(NR%2){print a[(NR+1)/2]} else {printf "%.1f\n",(a[NR/2]+a[NR/2+1])/2} }'; }
minimum() { sort -n | head -1; }

seed_bucket() { mc rm --recursive --force "local/$1" >/dev/null 2>&1 || true; mc mb --ignore-existing "local/$1" >/dev/null; }
put_random() { # <bucket> <key> <MiB> [localfile-out]
	local f; f=$(mktemp -p "$OUT" seed.XXXXXX)
	dd if=/dev/urandom of="$f" bs=1M count="$3" iflag=fullblock 2>/dev/null
	docker exec -i "$MINIO_CONTAINER" mc pipe "local/$1/$2" < "$f" >/dev/null
	[ -n "${4:-}" ] && mv "$f" "$4" || rm -f "$f"
}

stop_everything() {
	ebpf_nocache 0 2>/dev/null || true
	ebpf_stop "$MP_EBPF" >/dev/null 2>&1 || true
	for m in "$MP_FUSE_CACHE" "$MP_FUSE_RAW" "$MP_MOUNT_S3"; do umount_hard "$m"; done
}
trap stop_everything EXIT

start_fuse()  { sudo rm -rf "$FUSE_CACHE_DIR"; fuse_mount_raw "$MP_FUSE_RAW"; fuse_mount_cache "$MP_FUSE_CACHE" "$FUSE_CACHE_DIR"; }
start_mount_s3() { sudo rm -rf "$MOUNT_S3_CACHE_DIR"; mount_s3_mount "$MP_MOUNT_S3" "$MOUNT_S3_CACHE_DIR"; }
start_ebpf() { # <log> [flags]
	local l=$1; shift
	ebpf_mount "$MP_EBPF"
	ebpf_start_daemon "$OUT/$l" --bucket="$S3_BUCKET" --arena-metadata-only --sample-every=60s --sync-every=0 "$@"
}

# ── A: open+read+close ────────────────────────────────────────────────────────
step_A() {
	log "A: open+read+close p50"
	: > "$OUT/A.txt"
	stop_everything
	seed_bucket "$S3_BUCKET"
	printf 'Hello, s3fs-ebpf!\n' | docker exec -i "$MINIO_CONTAINER" mc pipe "local/$S3_BUCKET/hello.txt" >/dev/null
	start_fuse
	cat "$MP_FUSE_CACHE/hello.txt" >/dev/null
	"$S3FS_BIN" dev microbench --op=read --path="$MP_FUSE_RAW/hello.txt" --iters="$MICRO_ITERS" --label='fuse-raw' | tee -a "$OUT/A.txt" >&2
	"$S3FS_BIN" dev microbench --op=read --path="$MP_FUSE_CACHE/hello.txt" --iters="$MICRO_ITERS" --label='fuse-cache' | tee -a "$OUT/A.txt" >&2
	if mount_s3_available && start_mount_s3; then
		cat "$MP_MOUNT_S3/hello.txt" >/dev/null
		"$S3FS_BIN" dev microbench --op=read --path="$MP_MOUNT_S3/hello.txt" --iters="$MICRO_ITERS" --label='mount-s3' | tee -a "$OUT/A.txt" >&2
		umount_hard "$MP_MOUNT_S3"
	fi
	# ebpf (pcache, cache-dir) then ebpf-nocache (no cache-dir, kmod nocache=1)
	sudo rm -rf "$EBPF_CACHE_DIR"; mkdir -p "$EBPF_CACHE_DIR"
	start_ebpf A-ebpf.log --cache-dir="$EBPF_CACHE_DIR"
	cat "$MP_EBPF/hello.txt" >/dev/null
	"$S3FS_BIN" dev microbench --op=read --path="$MP_EBPF/hello.txt" --iters="$MICRO_ITERS" --label='ebpf-pcache' | tee -a "$OUT/A.txt" >&2
	start_ebpf A-nocache.log
	ebpf_nocache 1
	"$S3FS_BIN" dev microbench --op=read --path="$MP_EBPF/hello.txt" --iters="$MICRO_ITERS" --label='ebpf-nocache' | tee -a "$OUT/A.txt" >&2
	echo "read_miss_upcalls_total: $(curl -s $METRICS_URL | awk '$1=="s3fs_ebpf_read_miss_upcalls_total"{print $2}')" | tee -a "$OUT/A.txt" >&2
	ebpf_nocache 0
	stop_everything
}

# ── B: durable write (new file) ───────────────────────────────────────────────
step_B() {
	log "B: durable write, new file"
	: > "$OUT/B.txt"
	stop_everything
	seed_bucket "$S3_BUCKET"
	start_fuse
	sudo rm -rf "$EBPF_CACHE_DIR"; mkdir -p "$EBPF_CACHE_DIR"
	start_ebpf B-ebpf.log --cache-dir="$EBPF_CACHE_DIR"
	local sz be mp i t key ok
	for sz in 1 10 100; do
		dd if=/dev/urandom of="$OUT/payload-$sz" bs=1M count=$sz iflag=fullblock 2>/dev/null
		local want; want=$(sha256sum < "$OUT/payload-$sz" | awk '{print $1}')
		for be in fuse-raw fuse-cache ebpf; do
			case $be in fuse-raw) mp=$MP_FUSE_RAW;; fuse-cache) mp=$MP_FUSE_CACHE;; ebpf) mp=$MP_EBPF;; esac
			for i in $(seq 0 "$RUNS_W"); do   # run 0 is a warmup
				key="neww-$sz-$be-$i.bin"
				t=$(now)
				sudo dd if="$OUT/payload-$sz" of="$mp/$key" bs=1M conv=fsync 2>/dev/null
				local ms; ms=$(ms_since "$t")
				[ "$i" -gt 0 ] && echo "$sz $be $ms" >> "$OUT/B.txt"
			done
			ok=FAIL
			[ "$(mc cat "local/$S3_BUCKET/neww-$sz-$be-$RUNS_W.bin" | sha256sum | awk '{print $1}')" = "$want" ] && ok=PASS
			echo "verify $sz $be $ok" >> "$OUT/B.txt"
		done
	done
	stop_everything
	for sz in 1 10 100; do for be in fuse-raw fuse-cache ebpf; do
		printf 'B write %3s MiB %-10s median=%s ms min=%s ms\n' "$sz" "$be" \
			"$(awk -v s=$sz -v b=$be '$1==s&&$2==b{print $3}' "$OUT/B.txt" | median)" \
			"$(awk -v s=$sz -v b=$be '$1==s&&$2==b{print $3}' "$OUT/B.txt" | minimum)"
	done; done | tee "$OUT/B.summary" >&2
	grep verify "$OUT/B.txt" >&2
}

# ── C: 4 KiB into the middle of an existing object ───────────────────────────
step_C() {
	log "C: 4 KiB write into an existing object"
	: > "$OUT/C.txt"
	stop_everything
	seed_bucket "$S3_BUCKET"
	local sz be i
	for sz in 1 10 100; do for be in fuse-raw ebpf; do for i in $(seq 0 "$RUNS_W"); do
		put_random "$S3_BUCKET" "mid-$sz-$be-$i.bin" "$sz"
	done; done; done
	head -c 4096 /dev/urandom > "$OUT/patch4k"
	start_fuse
	sudo rm -rf "$EBPF_CACHE_DIR"; mkdir -p "$EBPF_CACHE_DIR"
	start_ebpf C-ebpf.log --cache-dir="$EBPF_CACHE_DIR"
	for sz in 1 10 100; do for be in fuse-raw ebpf; do
		local mp=$MP_FUSE_RAW; [ "$be" = ebpf ] && mp=$MP_EBPF
		for i in $(seq 0 "$RUNS_W"); do
			local t ms rc=0
			t=$(now)
			sudo dd if="$OUT/patch4k" of="$mp/mid-$sz-$be-$i.bin" bs=4096 count=1 seek=$((sz * MIB / 2 / 4096)) conv=notrunc,fsync 2>/dev/null || rc=$?
			ms=$(ms_since "$t")
			local got; got=$(mc stat "local/$S3_BUCKET/mid-$sz-$be-$i.bin" 2>/dev/null | awk '/^Size/{print $3, $4}' | tr -d '\r')
			[ "$i" -gt 0 ] && echo "$sz $be $ms rc=$rc size_after='$got'" >> "$OUT/C.txt"
		done
	done; done
	stop_everything
	for sz in 1 10 100; do for be in fuse-raw ebpf; do
		printf 'C mid-write %3s MiB %-9s median=%s ms min=%s ms\n' "$sz" "$be" \
			"$(awk -v s=$sz -v b=$be '$1==s&&$2==b{print $3}' "$OUT/C.txt" | median)" \
			"$(awk -v s=$sz -v b=$be '$1==s&&$2==b{print $3}' "$OUT/C.txt" | minimum)"
	done; done | tee "$OUT/C.summary" >&2
	grep -c "rc=0" "$OUT/C.txt" >&2 || true
	awk '{print $1,$2,$4,$5,$6}' "$OUT/C.txt" | sort | uniq -c >&2
}

# ── D: cold read ──────────────────────────────────────────────────────────────
step_D() {
	log "D: cold read"
	: > "$OUT/D.txt"
	stop_everything
	seed_bucket "$S3_BUCKET"
	local sz i
	declare -A WANT
	for sz in 10 100 300; do for i in $(seq 1 "$RUNS_COLD"); do
		put_random "$S3_BUCKET" "cold-$sz-$i.bin" "$sz" "$OUT/cold-$sz-$i.local"
		WANT[$sz-$i]=$(sha256sum < "$OUT/cold-$sz-$i.local" | awk '{print $1}')
		rm -f "$OUT/cold-$sz-$i.local"
	done; done
	local be mp
	for be in fuse-raw fuse-cache mount-s3 ebpf; do
		stop_everything
		sync; sudo sysctl -q -w vm.drop_caches=3
		case $be in
			fuse-raw) fuse_mount_raw "$MP_FUSE_RAW"; mp=$MP_FUSE_RAW;;
			fuse-cache) sudo rm -rf "$FUSE_CACHE_DIR"; fuse_mount_cache "$MP_FUSE_CACHE" "$FUSE_CACHE_DIR"; mp=$MP_FUSE_CACHE;;
			mount-s3) start_mount_s3 || continue; mp=$MP_MOUNT_S3;;
			ebpf) sudo rm -rf "$EBPF_CACHE_DIR"; mkdir -p "$EBPF_CACHE_DIR"
			      start_ebpf D-ebpf.log --cache-dir="$EBPF_CACHE_DIR"; mp=$MP_EBPF;;
		esac
		for sz in 10 100 300; do for i in $(seq 1 "$RUNS_COLD"); do
			local t ms; t=$(now)
			cat "$mp/cold-$sz-$i.bin" > /dev/null
			ms=$(ms_since "$t")
			echo "$sz $be $ms" >> "$OUT/D.txt"
		done; done
		# content check (warm) on the first object of each size
		for sz in 10 100 300; do
			local h; h=$(sha256sum < "$mp/cold-$sz-1.bin" | awk '{print $1}')
			echo "verify $sz $be $([ "$h" = "${WANT[$sz-1]}" ] && echo PASS || echo FAIL)" >> "$OUT/D.txt"
		done
	done
	stop_everything
	for sz in 10 100 300; do for be in fuse-raw fuse-cache mount-s3 ebpf; do
		printf 'D cold %3s MiB %-10s median=%s ms min=%s ms\n' "$sz" "$be" \
			"$(awk -v s=$sz -v b=$be '$1==s&&$2==b{print $3}' "$OUT/D.txt" | median)" \
			"$(awk -v s=$sz -v b=$be '$1==s&&$2==b{print $3}' "$OUT/D.txt" | minimum)"
	done; done | tee "$OUT/D.summary" >&2
	grep verify "$OUT/D.txt" >&2
}

# ── E: many small files ──────────────────────────────────────────────────────
step_E() {
	log "E: $SMALL_N x 4 KiB files"
	: > "${E_OUT:-$OUT/E.txt}"
	stop_everything
	S3_BUCKET=$SMALL_BUCKET
	if [ -z "${E_SKIP_SEED:-}" ]; then
	seed_bucket "$SMALL_BUCKET"
	local d="$OUT/small"; rm -rf "$d"; mkdir -p "$d"
	head -c $((SMALL_N * 4096)) /dev/urandom | split -b 4096 -a 5 -d - "$d/f"
	docker exec "$MINIO_CONTAINER" rm -rf /tmp/small
	docker cp "$d" "$MINIO_CONTAINER:/tmp/small"
	mc cp --recursive /tmp/small/ "local/$SMALL_BUCKET/" >/dev/null
	fi
	log "bucket has: $(mc ls "local/$SMALL_BUCKET/" | wc -l) objects"
	local be mp t ms
	for be in ${E_BACKENDS:-fuse-raw fuse-cache mount-s3 ebpf}; do
		stop_everything
		sync; sudo sysctl -q -w vm.drop_caches=3
		case $be in
			fuse-raw) fuse_mount_raw "$MP_FUSE_RAW"; mp=$MP_FUSE_RAW;;
			fuse-cache) sudo rm -rf "$FUSE_CACHE_DIR"; fuse_mount_cache "$MP_FUSE_CACHE" "$FUSE_CACHE_DIR"; mp=$MP_FUSE_CACHE;;
			mount-s3) start_mount_s3 || continue; mp=$MP_MOUNT_S3;;
			ebpf) sudo rm -rf "$EBPF_CACHE_DIR"; mkdir -p "$EBPF_CACHE_DIR"
			      start_ebpf E-ebpf.log --cache-dir="$EBPF_CACHE_DIR" --softcap=20000; mp=$MP_EBPF;;
		esac
		local ph
		for ph in ls-cold cat-cold ls-warm cat-warm; do
			t=$(now)
			case $ph in
				ls-*) timeout 900 ls -l "$mp" | wc -l > "$OUT/E.lines" ;;
				cat-*) timeout 1800 bash -c "find '$mp' -maxdepth 1 -type f -name 'f*' -print0 | sort -z | xargs -0 cat | wc -c" > "$OUT/E.bytes" ;;
			esac
			ms=$(ms_since "$t")
			echo "$be $ph $ms files=$(cat "$OUT/E.lines" 2>/dev/null) bytes=$(cat "$OUT/E.bytes" 2>/dev/null)" | tee -a "${E_OUT:-$OUT/E.txt}" >&2
		done
	done
	stop_everything
	S3_BUCKET=$BENCH_BUCKET
}

for s in $STEPS; do "step_$s"; done
log "done. $OUT"
