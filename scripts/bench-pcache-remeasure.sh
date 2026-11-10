#!/usr/bin/env bash
# scripts/bench-pcache-remeasure.sh — §18.26+: pcache モード (--arena-metadata-only)
# で、arena モード時代の数字 (§11) を測り直し、既存オブジェクトへの途中 write・
# cold read・小さいファイル大量の項目を足す。
#
#   STEPS="A B C D E" で実行する項目を選ぶ (既定は全部)。
#   BACKENDS で構成を選ぶ (既定は issue #35 の 6 構成:
#     fuse-raw fuse-diskcache fuse-cache ebpf-nocache ebpf-pcachedrop ebpf-pcache)。
#   A  open+read+close p50 / p99 (microbench)
#   B  durable write (dd conv=fsync) 1/10/100 MiB、新規ファイル (RUNS_W 回、既定 10)
#   C  既存オブジェクトの途中への 4 KiB write + fsync (1/10/100 MiB)
#      (AC-51 の multipart flush は既定で有効。C_EXTRA=--partial-modify=false で AC-50 の全体 GET+PUT を測る)
#   D  cold read (1 回目) 10/100/300 MiB (RUNS_COLD 回、既定 10)。全ての object を sha256 で照合する
#      (D_DROP_EACH=1 で毎回 drop_caches する: §18.39)
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
BACKENDS=${BACKENDS:-$BENCH_BACKENDS_ALL}
MP_EBPF=/mnt/s3demo
MP_FUSE_RAW=/mnt/s3fuse
MP_FUSE_CACHE=/mnt/s3fuse-cache
MP_MOUNT_S3=/mnt/s3-mountpoint
FUSE_CACHE_DIR=/tmp/s3fs-fuse-cache-s18
EBPF_CACHE_DIR=/tmp/s3fs-ebpf-cache-s18
MOUNT_S3_CACHE_DIR=/tmp/mount-s3-cache-s18
MICRO_ITERS=${MICRO_ITERS:-5000}
RUNS_W=${RUNS_W:-10}
RUNS_COLD=${RUNS_COLD:-10}
E_REPS=${E_REPS:-5}
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
# stats on a column of numbers on stdin: median, q1, q3, min, max (ms)
stats() { sort -n | awk '{a[NR]=$1} END{
	if (NR==0) { print "n/a"; exit }
	m=(NR%2)?a[(NR+1)/2]:(a[NR/2]+a[NR/2+1])/2
	i1=int((NR-1)*0.25)+1; i3=int((NR-1)*0.75)+1
	printf "n=%d median=%.1f q1=%.1f q3=%.1f min=%.1f max=%.1f", NR, m, a[i1], a[i3], a[1], a[NR] }'; }
median() { sort -n | awk '{a[NR]=$1} END{ if(NR==0){print "n/a"} else if(NR%2){print a[(NR+1)/2]} else {printf "%.1f\n",(a[NR/2]+a[NR/2+1])/2} }'; }

# head_field <key> <header> — one response header of a HEAD on the object (SigV4, ~2 ms).
head_field() {
	curl -sI --aws-sigv4 "aws:amz:us-east-1:s3" --user "${S3_ACCESS_KEY:-minioadmin}:${S3_SECRET_KEY:-minioadmin}" \
		"$S3_URL/$S3_BUCKET/$1" | tr -d '\r' | awk -F': ' -v f="$2" 'tolower($1)==tolower(f){print $2}'
}
# wait_object <key> <header> <value-that-means-done> — spin until the HEAD header equals it
# (max 120 s). fsync of the s3fs-ebpf mount can return before the PUT has landed (found while
# verifying B on 67e97b1 and on this tree), so "durable" is timed to the complete object too.
wait_object() {
	local dl=$((SECONDS + 120))
	until [ "$(head_field "$1" "$2")" = "$3" ]; do ((SECONDS > dl)) && return 1; done
	return 0
}

seed_bucket() { mc rm --recursive --force "local/$1" >/dev/null 2>&1 || true; mc mb --ignore-existing "local/$1" >/dev/null; }
put_random() { # <bucket> <key> <MiB> [localfile-out]
	local f; f=$(mktemp -p "$OUT" seed.XXXXXX)
	dd if=/dev/urandom of="$f" bs=1M count="$3" iflag=fullblock 2>/dev/null
	docker exec -i "$MINIO_CONTAINER" mc pipe "local/$1/$2" < "$f" >/dev/null
	[ -n "${4:-}" ] && mv "$f" "$4" || rm -f "$f"
}

stop_everything() { bench_down; }
trap stop_everything EXIT
start_be() { # <backend> <log> — fresh mount + empty caches; sets BENCH_MP
	bench_up "$1" "$OUT/$2" --sample-every=60s --sync-every=0 "${@:3}"
}

# ── A: open+read+close ────────────────────────────────────────────────────────
step_A() {
	log "A: open+read+close p50/p99"
	: > "$OUT/A.txt"
	stop_everything
	S3_BUCKET=$BENCH_BUCKET
	seed_bucket "$S3_BUCKET"
	printf 'Hello, s3fs-ebpf!\n' | docker exec -i "$MINIO_CONTAINER" mc pipe "local/$S3_BUCKET/hello.txt" >/dev/null
	local be
	for be in $BACKENDS; do
		start_be "$be" "A-$be.log"
		cat "$BENCH_MP/hello.txt" >/dev/null
		local u0 u1
		u0=$({ curl -s $METRICS_URL 2>/dev/null || true; } | awk '$1=="s3fs_ebpf_read_miss_upcalls_total"{print $2+0}')
		"$S3FS_BIN" dev microbench --op=read --path="$BENCH_MP/hello.txt" --iters="$MICRO_ITERS" --label="$be" | tee -a "$OUT/A.txt" >&2
		u1=$({ curl -s $METRICS_URL 2>/dev/null || true; } | awk '$1=="s3fs_ebpf_read_miss_upcalls_total"{print $2+0}')
		[ "$BENCH_KIND" = ebpf ] && echo "upcalls[$be]: ${u0:-0} -> ${u1:-0}" | tee -a "$OUT/A.txt" >&2
		# open_check=1 (BENCH_OPEN_CHECK): one S3 HEAD per open, counted by the daemon.
		[ "$BENCH_KIND" = ebpf ] && echo "open_checks[$be]: $({ curl -s $METRICS_URL 2>/dev/null || true; } | awk '$1=="s3fs_ebpf_open_checks_total"{print $2+0}')" | tee -a "$OUT/A.txt" >&2
	done
	stop_everything
}

# ── B: durable write (new file) ───────────────────────────────────────────────
step_B() {
	log "B: durable write, new file"
	: > "$OUT/B.txt"
	stop_everything
	S3_BUCKET=$BENCH_BUCKET
	seed_bucket "$S3_BUCKET"
	local sz be i t key ok
	for sz in 1 10 100; do dd if=/dev/urandom of="$OUT/payload-$sz" bs=1M count=$sz iflag=fullblock 2>/dev/null; done
	for be in $BACKENDS; do
		start_be "$be" "B-$be.log"
		for sz in 1 10 100; do
			local want; want=$(sha256sum < "$OUT/payload-$sz" | awk '{print $1}')
			for i in $(seq 0 "$RUNS_W"); do   # run 0 is a warmup
				key="neww-$sz-$be-$i.bin"
				t=$(now)
				sudo dd if="$OUT/payload-$sz" of="$BENCH_MP/$key" bs=1M conv=fsync 2>/dev/null
				local ms du early=0; ms=$(ms_since "$t")
				[ "$(head_field "$key" content-length)" = "$((sz * MIB))" ] || early=1   # fsync returned before the PUT landed
				wait_object "$key" content-length "$((sz * MIB))" || echo "WARN object $key never completed" >&2
				du=$(ms_since "$t")
				[ "$i" -gt 0 ] && echo "$sz $be $ms $du $early" >> "$OUT/B.txt"
			done
			ok=PASS   # every object, not only the last
			for i in $(seq 0 "$RUNS_W"); do
				[ "$(mc cat "local/$S3_BUCKET/neww-$sz-$be-$i.bin" | sha256sum | awk '{print $1}')" = "$want" ] || ok=FAIL
			done
			echo "verify $sz $be $ok" >> "$OUT/B.txt"
		done
	done
	stop_everything
	for sz in 1 10 100; do for be in $BACKENDS; do
		printf 'B write %3s MiB %-15s fsync: %s\n' "$sz" "$be" "$(awk -v s=$sz -v b=$be '$1==s&&$2==b{print $3}' "$OUT/B.txt" | stats)"
		printf 'B write %3s MiB %-15s object-complete: %s  fsync-returned-early=%s/%s\n' "$sz" "$be" "$(awk -v s=$sz -v b=$be '$1==s&&$2==b{print $4}' "$OUT/B.txt" | stats)" \
			"$(awk -v s=$sz -v b=$be '$1==s&&$2==b&&$5==1' "$OUT/B.txt" | wc -l)" "$(awk -v s=$sz -v b=$be '$1==s&&$2==b' "$OUT/B.txt" | wc -l)"
	done; done | tee "$OUT/B.summary" >&2
	grep verify "$OUT/B.txt" >&2
}

# ── C: 4 KiB into the middle of an existing object ───────────────────────────
step_C() {
	log "C: 4 KiB write into an existing object"
	: > "$OUT/C.txt"
	stop_everything
	S3_BUCKET=$BENCH_BUCKET
	seed_bucket "$S3_BUCKET"
	local sz be i
	for sz in 1 10 100; do for be in $BACKENDS; do for i in $(seq 0 "$RUNS_W"); do
		put_random "$S3_BUCKET" "mid-$sz-$be-$i.bin" "$sz"
	done; done; done
	head -c 4096 /dev/urandom > "$OUT/patch4k"
	for be in $BACKENDS; do
		start_be "$be" "C-$be.log" ${C_EXTRA:-}
		for sz in 1 10 100; do
			for i in $(seq 0 "$RUNS_W"); do
				local t ms du rc=0 etag0 early=0
				etag0=$(head_field "mid-$sz-$be-$i.bin" etag)
				t=$(now)
				sudo dd if="$OUT/patch4k" of="$BENCH_MP/mid-$sz-$be-$i.bin" bs=4096 count=1 seek=$((sz * MIB / 2 / 4096)) conv=notrunc,fsync 2>/dev/null || rc=$?
				ms=$(ms_since "$t")
				[ "$(head_field "mid-$sz-$be-$i.bin" etag)" != "$etag0" ] || early=1
				until [ "$(head_field "mid-$sz-$be-$i.bin" etag)" != "$etag0" ]; do
					[ "$(ms_since "$t" | cut -d. -f1)" -gt 120000 ] && break
				done
				du=$(ms_since "$t")
				local got; got=$(mc stat "local/$S3_BUCKET/mid-$sz-$be-$i.bin" 2>/dev/null | awk '/^Size/{print $3, $4}' | tr -d '\r')
				[ "$i" -gt 0 ] && echo "$sz $be $ms rc=$rc size_after='$got' durable=$du early=$early" >> "$OUT/C.txt"
			done
		done
	done
	stop_everything
	for sz in 1 10 100; do for be in $BACKENDS; do
		printf 'C mid-write %3s MiB %-15s fsync: %s\n' "$sz" "$be" "$(awk -v s=$sz -v b=$be '$1==s&&$2==b{print $3}' "$OUT/C.txt" | stats)"
		printf 'C mid-write %3s MiB %-15s object-replaced: %s  fsync-returned-early=%s/%s\n' "$sz" "$be" \
			"$(awk -v s=$sz -v b=$be '$1==s&&$2==b{split($7,a,"=");print a[2]}' "$OUT/C.txt" | stats)" \
			"$(awk -v s=$sz -v b=$be '$1==s&&$2==b&&$8=="early=1"' "$OUT/C.txt" | wc -l)" "$(awk -v s=$sz -v b=$be '$1==s&&$2==b' "$OUT/C.txt" | wc -l)"
	done; done | tee "$OUT/C.summary" >&2
	awk '{print $1,$2,$4,$5,$6}' "$OUT/C.txt" | sort | uniq -c >&2
}

# ── D: cold read ──────────────────────────────────────────────────────────────
step_D() {
	log "D: cold read"
	: > "$OUT/D.txt"
	stop_everything
	S3_BUCKET=$BENCH_BUCKET
	seed_bucket "$S3_BUCKET"
	local sz i be
	declare -A WANT
	for sz in 10 100 300; do for i in $(seq 1 "$RUNS_COLD"); do
		put_random "$S3_BUCKET" "cold-$sz-$i.bin" "$sz" "$OUT/cold-$sz-$i.local"
		WANT[$sz-$i]=$(sha256sum < "$OUT/cold-$sz-$i.local" | awk '{print $1}')
		rm -f "$OUT/cold-$sz-$i.local"
	done; done
	for be in $BACKENDS; do
		sync; sudo sysctl -q -w vm.drop_caches=3
		start_be "$be" "D-$be.log"
		sync; sudo sysctl -q -w vm.drop_caches=3
		for sz in 10 100 300; do for i in $(seq 1 "$RUNS_COLD"); do
			local t ms
			[ -n "${D_DROP_EACH:-}" ] && { sync; sudo sysctl -q -w vm.drop_caches=3; }
			t=$(now)
			cat "$BENCH_MP/cold-$sz-$i.bin" > /dev/null
			ms=$(ms_since "$t")
			echo "$sz $be $ms" >> "$OUT/D.txt"
		done; done
		# content check of every object (not timed)
		local bad=0 n=0
		for sz in 10 100 300; do for i in $(seq 1 "$RUNS_COLD"); do
			n=$((n + 1))
			[ "$(sha256sum < "$BENCH_MP/cold-$sz-$i.bin" | awk '{print $1}')" = "${WANT[$sz-$i]}" ] || bad=$((bad + 1))
		done; done
		echo "verify $be objects=$n bad=$bad $([ "$bad" = 0 ] && echo PASS || echo FAIL)" >> "$OUT/D.txt"
	done
	stop_everything
	for sz in 10 100 300; do for be in $BACKENDS; do
		printf 'D cold %3s MiB %-15s %s\n' "$sz" "$be" "$(awk -v s=$sz -v b=$be '$1==s&&$2==b{print $3}' "$OUT/D.txt" | stats)"
	done; done | tee "$OUT/D.summary" >&2
	grep verify "$OUT/D.txt" >&2
}

# ── E: many small files ──────────────────────────────────────────────────────
step_E() {
	log "E: $SMALL_N x 4 KiB files, $E_REPS reps"
	: > "${E_OUT:-$OUT/E.txt}"
	stop_everything
	S3_BUCKET=$SMALL_BUCKET
	if [ -z "${E_SKIP_SEED:-}" ]; then
	seed_bucket "$SMALL_BUCKET"
	local d="$OUT/small"; rm -rf "$d"; mkdir -p "$d"
	head -c $((SMALL_N * 4096)) /dev/urandom | split -b 4096 -a 5 -d - "$d/f"
	( cd "$d" && cat f* | sha256sum | awk '{print $1}' ) > "$OUT/E.want"
	docker exec "$MINIO_CONTAINER" rm -rf /tmp/small
	docker cp "$d" "$MINIO_CONTAINER:/tmp/small"
	mc cp --recursive /tmp/small/ "local/$SMALL_BUCKET/" >/dev/null
	rm -rf "$d"
	fi
	log "bucket has: $(mc ls "local/$SMALL_BUCKET/" | wc -l) objects"
	local be mp t ms rep
	for rep in $(seq "$E_REPS"); do for be in $BACKENDS; do
		sync; sudo sysctl -q -w vm.drop_caches=3
		start_be "$be" "E-$be.log" --softcap=20000 ${E_EXTRA:-}
		sync; sudo sysctl -q -w vm.drop_caches=3
		mp=$BENCH_MP
		local ph
		for ph in ls-cold cat-cold ls-warm cat-warm; do
			t=$(now)
			case $ph in
				ls-*) timeout 1800 ls -l "$mp" | wc -l > "$OUT/E.lines" ;;
				cat-*) timeout 3600 bash -c "find '$mp' -maxdepth 1 -type f -name 'f*' -print0 | sort -z | xargs -0 cat | tee >(sha256sum > '$OUT/E.sha') | wc -c" > "$OUT/E.bytes" ;;
			esac
			ms=$(ms_since "$t")
			local sha=-
			case $ph in cat-*) sleep 1; sha=$([ "$(awk '{print $1}' "$OUT/E.sha")" = "$(cat "$OUT/E.want" 2>/dev/null)" ] && echo ok || echo MISMATCH) ;; esac
			echo "$be $ph $ms sha=$sha files=$(cat "$OUT/E.lines" 2>/dev/null) bytes=$(cat "$OUT/E.bytes" 2>/dev/null) rep=$rep" | tee -a "${E_OUT:-$OUT/E.txt}" >&2
		done
	done; done
	stop_everything
	S3_BUCKET=$BENCH_BUCKET
	for be in $BACKENDS; do for ph in ls-cold cat-cold ls-warm cat-warm; do
		printf 'E %-15s %-9s %s\n' "$be" "$ph" "$(awk -v b=$be -v p=$ph '$1==b&&$2==p{print $3}' "${E_OUT:-$OUT/E.txt}" | stats)"
	done; done | tee "$OUT/E.summary" >&2
}

{ bench_env_report; echo "steps=$STEPS backends=$BACKENDS runs_w=$RUNS_W runs_cold=$RUNS_COLD e_reps=$E_REPS micro_iters=$MICRO_ITERS small_n=$SMALL_N"; } > "$OUT/environment.txt"
for s in $STEPS; do "step_$s"; done
log "done. $OUT"
