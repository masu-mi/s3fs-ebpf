# shellcheck shell=bash
# scripts/lib/bench.sh — lifecycle helpers shared by scripts/bench-*.sh.
# Sourced after the caller sets TOPDIR. Each bench keeps its own flags,
# mount points and log() prefix; this file only owns the mechanics that
# had drifted between copies (pkill pattern, MinIO object check, fuse
# options, teardown order).
#
# Tunables (env):
#   MINIO_CONTAINER  s3fs-ebpf-minio      S3_BUCKET  s3fs-test
#   S3_URL           http://127.0.0.1:9000
#   S3FS_PASSWD_FILE /etc/passwd-s3fs
#   DAEMON_READY_TIMEOUT  seconds to wait for the ready log line (15)
#   DAEMON_READY_PATTERN  grep -E pattern marking "ready" ('\[admit\] ')

[[ -n "${_S3FS_LIB_BENCH:-}" ]] && return 0
_S3FS_LIB_BENCH=1

: "${TOPDIR:?TOPDIR must be set before sourcing lib/bench.sh}"
# shellcheck source=umount.sh
source "$TOPDIR/scripts/lib/umount.sh"

S3FS_BIN="$TOPDIR/bin/s3fs-ebpf"
S3FS_KMOD="$TOPDIR/kmod/s3fs_kfunc.ko"
S3FS_KMOD_NAME=s3fs_kfunc
MINIO_CONTAINER=${MINIO_CONTAINER:-s3fs-ebpf-minio}
S3_BUCKET=${S3_BUCKET:-s3fs-test}
S3_URL=${S3_URL:-http://127.0.0.1:9000}
S3FS_PASSWD_FILE=${S3FS_PASSWD_FILE:-/etc/passwd-s3fs}
DAEMON_READY_TIMEOUT=${DAEMON_READY_TIMEOUT:-15}
DAEMON_READY_PATTERN=${DAEMON_READY_PATTERN:-'\[admit\] '}

# fast[p]ath: the character class matches the daemon's cmdline
# ("fastpath") but not the literal pattern in pkill's own argv — or in the
# `sudo pkill ...` parent — so pkill never signals itself. A self-SIGKILL
# made bash print a "Killed" job notice with \r that corrupted bench output.
DAEMON_PATTERN='s3fs-ebpf fast[p]ath'

# --- prerequisites ----------------------------------------------------------

# need_cmds <cmd...> — exit with an install hint when any is missing.
need_cmds() {
	local t missing=()
	for t in "$@"; do
		command -v "$t" >/dev/null 2>&1 || missing+=("$t")
	done
	[[ ${#missing[@]} -eq 0 ]] && return 0
	echo "missing: ${missing[*]} — run 'make setup-bench-env' (or 'make doctor')" >&2
	exit 1
}

# need_built — kmod + daemon binaries exist.
need_built() {
	[[ -f "$S3FS_KMOD" ]] || { echo "kmod not built — run 'make kmod' (or 'make doctor')" >&2; exit 1; }
	[[ -x "$S3FS_BIN" ]]  || { echo "daemon not built — run 'make daemon'" >&2; exit 1; }
}

# minio_require [object...] — MinIO container up and each object present
# in $S3_BUCKET. mc inside the container has no credentials by default, so
# it talks anonymously and `mc ls` exits 0 even for missing objects; set
# the alias and use `mc stat`, which fails reliably.
minio_require() {
	need_cmds docker
	docker inspect "$MINIO_CONTAINER" >/dev/null 2>&1 \
		|| { echo "$MINIO_CONTAINER container missing — run 'make compose-up'" >&2; exit 1; }
	docker exec "$MINIO_CONTAINER" mc alias set local http://localhost:9000 \
		minioadmin minioadmin >/dev/null 2>&1
	# A leftover `mc admin trace` (killing the `docker exec` client does not stop it inside the
	# container) keeps MinIO publishing every request to a trace subscriber: every S3 path got
	# 25-75 % slower and timing-sensitive tests (test-policy-conformance) failed (issue #35).
	local stray
	stray=$(docker top "$MINIO_CONTAINER" -o pid,args 2>/dev/null | grep -c 'mc admin trace' || true)
	[ "${stray:-0}" -eq 0 ] || {
		echo "$stray 'mc admin trace' process(es) run inside $MINIO_CONTAINER and slow every S3 request;" \
			"stop them (docker top $MINIO_CONTAINER; sudo kill <pid>)" >&2
		exit 1
	}
	local obj
	for obj in "$@"; do
		docker exec "$MINIO_CONTAINER" mc stat "local/$S3_BUCKET/$obj" >/dev/null 2>&1 \
			|| { echo "s3://$S3_BUCKET/$obj missing in MinIO — run 'make compose-up' to re-seed" >&2; exit 1; }
	done
}

# mc_cat <object> — print an object's body (for roundtrip checks).
mc_cat() {
	docker exec "$MINIO_CONTAINER" mc cat "local/$S3_BUCKET/$1"
}

# --- s3fs-ebpf stack ----------------------------------------------------------

daemon_running() { pgrep -f "$DAEMON_PATTERN" >/dev/null 2>&1; }

# daemon_kill [SIGNAL] — default INT (graceful: drains the flush queue).
daemon_kill() {
	sudo pkill "-${1:-INT}" -f "$DAEMON_PATTERN" 2>/dev/null || true
}

# daemon_wait_exit [seconds] — wait for the daemon to finish its SIGINT
# drain before the mount is pulled from under it.
daemon_wait_exit() {
	local deadline=$((SECONDS + ${1:-5}))
	while daemon_running && ((SECONDS < deadline)); do sleep 0.2; done
}

# ebpf_mount <mp> — insmod (if needed) + mount -t s3fs_ebpf (if needed).
# ebpf_hit_params — write HIT_ACCUM / HIT_REARM_JIFFIES / HIT_REPLAY_MAX
# (when set) to the kmod's hit-signal module parameters (issue #22,
# docs/design/hit-signal-percpu-accumulation.md).  Unset = keep the
# kmod default.  Same mechanism as ebpf_nocache.
ebpf_hit_params() {
	local p=/sys/module/${S3FS_KMOD_NAME}/parameters
	[ -n "${HIT_ACCUM:-}" ] && echo "$HIT_ACCUM" | sudo tee "$p/hit_accum" >/dev/null
	[ -n "${HIT_REARM_JIFFIES:-}" ] && echo "$HIT_REARM_JIFFIES" | sudo tee "$p/hit_rearm_jiffies" >/dev/null
	[ -n "${HIT_REPLAY_MAX:-}" ] && echo "$HIT_REPLAY_MAX" | sudo tee "$p/hit_replay_max" >/dev/null
	return 0
}

ebpf_mount() {
	local mp=$1
	lsmod | grep -q "^${S3FS_KMOD_NAME} " || sudo insmod "$S3FS_KMOD"
	ebpf_hit_params
	mountpoint -q "$mp" || { sudo mkdir -p "$mp"; sudo mount -t s3fs_ebpf none "$mp"; }
}

# ebpf_start_daemon <logfile> [fastpath flags...] — kill any stale daemon,
# start `fastpath --s3 <flags>` in the background and wait until the log
# matches $DAEMON_READY_PATTERN (prewarm's "[admit] " by default; override
# per call: DAEMON_READY_PATTERN='\[sync\] initial:' ebpf_start_daemon ...).
# The log is written by the calling user's shell.
ebpf_start_daemon() {
	local logf=$1; shift
	daemon_kill KILL
	sleep 0.5
	# shellcheck disable=SC2024  # the log is meant to be user-owned
	sudo -E "$S3FS_BIN" fastpath --s3 "$@" >"$logf" 2>&1 &
	local deadline=$((SECONDS + DAEMON_READY_TIMEOUT))
	until grep -qE "$DAEMON_READY_PATTERN" "$logf" 2>/dev/null; do
		if ((SECONDS >= deadline)); then
			echo "[bench] daemon not ready after ${DAEMON_READY_TIMEOUT}s — continuing; see $logf" >&2
			break
		fi
		sleep 0.2
	done
	sleep 0.3
}

# ebpf_stop <mp> — SIGINT daemon, wait for drain, umount, rmmod.
ebpf_stop() {
	daemon_kill INT
	daemon_wait_exit 5
	umount_hard "$1"
	# Retries: the BPF progs' module reference is dropped asynchronously
	# after the daemon exits (see scripts/kmod-unload.sh).
	"$TOPDIR/scripts/kmod-unload.sh" "$S3FS_KMOD_NAME" || true
}

# ebpf_nocache <0|1> — toggle the kmod's FUSE-raw-like read mode (drop
# page cache + arena admit on every open; see kmod/vfs_ops.c
# s3fs_ebpf_file_open). Resets on rmmod.
ebpf_nocache() {
	echo "$1" | sudo tee "/sys/module/${S3FS_KMOD_NAME}/parameters/nocache" >/dev/null
}

# ebpf_pcache_drop <0|1> — toggle the kmod's pcache_drop mode (drop only the
# page cache on every open; admit + chunk table stay; see
# kmod/vfs/aops_read.inc.c s3fs_ebpf_file_open). nocache wins when both are
# set. Resets on rmmod.
ebpf_pcache_drop() {
	echo "$1" | sudo tee "/sys/module/${S3FS_KMOD_NAME}/parameters/pcache_drop" >/dev/null
}

# fd_buf_audit — print one `DETAIL fd_buf_audit ...` line comparing the M1
# gauge (incremental) with M2's walk over live inodes and the pcache share
# the AC-41 capacity loop charges. Call before ebpf_stop, with I/O idle.
# The daemon polls kmod every 2 s, so wait for two identical samples.
# status: OK (gauge == walk), DRIFT (stable but unequal), NA (no daemon or
# old kmod). Report-only: never changes the caller's exit status.
fd_buf_audit() {
	local url=${METRICS_URL:-http://localhost:9600/metrics}
	local prev="" cur="" out g w p i
	for i in 1 2 3 4 5 6; do
		out=$(curl -sf "$url" 2>/dev/null) || break
		cur=$(printf '%s\n' "$out" | awk '
			$1 == "s3fs_ebpf_fd_buf_bytes"        { g = $2 }
			$1 == "s3fs_ebpf_fd_buf_walk_bytes"   { w = $2 }
			$1 == "s3fs_ebpf_fd_buf_pcache_bytes" { p = $2 }
			END { if (g != "" && w != "" && p != "") print g, w, p }')
		[[ -n "$cur" && "$cur" == "$prev" ]] && break
		prev=$cur
		sleep 2.2
	done
	if [[ -z "$cur" ]]; then
		printf 'DETAIL fd_buf_audit status=NA\n'
		return 0
	fi
	read -r g w p <<<"$cur"
	printf 'DETAIL fd_buf_audit status=%s gauge=%d walk=%d pcache=%d nonpcache=%d drift=%d\n' \
		"$([[ "$g" == "$w" ]] && echo OK || echo DRIFT)" \
		"$g" "$w" "$p" $((w - p)) $((g - w))
}

# --- s3fs-fuse (A/B reference) ------------------------------------------------

# fuse_mount <mp> [extra s3fs args...] — mount $S3_BUCKET with the common
# options. Callers decide whether to umount/reuse an existing mount first.
fuse_mount() {
	local mp=$1; shift
	[[ -s "$S3FS_PASSWD_FILE" ]] \
		|| { echo "$S3FS_PASSWD_FILE missing — run 'make setup-bench-env'" >&2; return 1; }
	sudo mkdir -p "$mp"
	sudo s3fs "$S3_BUCKET" "$mp" \
		-o url="$S3_URL" -o use_path_request_style \
		-o passwd_file="$S3FS_PASSWD_FILE" -o allow_other \
		-o uid="$(id -u)" -o gid="$(id -g)" -o umask=0022 \
		"$@"
}

# fuse_mount_raw <mp> — fresh no-cache mount.
fuse_mount_raw() {
	umount_hard "$1"
	fuse_mount "$1"
}

# fuse_mount_cache <mp> <cache-dir> — fresh use_cache + kernel_cache mount
# (the fair "best case" FUSE baseline, see lessons-learned.md).
fuse_mount_cache() {
	local mp=$1 dir=$2
	umount_hard "$mp"
	sudo rm -rf "$dir" && mkdir -p "$dir"
	fuse_mount "$mp" -o use_cache="$dir" -o kernel_cache -o max_stat_cache_size=100000
}

# fuse_mount_diskcache <mp> <cache-dir> — fresh use_cache mount WITHOUT
# kernel_cache (fuse-diskcache, issue #35): the page cache is dropped on every
# open and the disk cache serves the read. The counterpart of ebpf-pcachedrop.
fuse_mount_diskcache() {
	local mp=$1 dir=$2
	umount_hard "$mp"
	sudo rm -rf "$dir" && mkdir -p "$dir"
	fuse_mount "$mp" -o use_cache="$dir" -o max_stat_cache_size=100000
}

# --- the six configurations of issue #35 ---------------------------------------
#   fuse-raw         s3fs-fuse, no use_cache, no kernel_cache
#   fuse-diskcache   s3fs-fuse, use_cache, no kernel_cache
#   fuse-cache       s3fs-fuse, use_cache + kernel_cache + max_stat_cache_size
#   ebpf-nocache     kmod nocache=1, daemon without --cache-dir
#   ebpf-pcachedrop  kmod pcache_drop=1, daemon with --cache-dir (chunk table + policy kept)
#   ebpf-pcache      defaults (page cache on), daemon with --cache-dir
#   (layer-latency only) ebpf-nocache-diskcache: nocache=1 + daemon --cache-dir, i.e. every
#                    open upcalls and the daemon answers from its disk cache, not from S3
BENCH_BACKENDS_ALL="fuse-raw fuse-diskcache fuse-cache ebpf-nocache ebpf-pcachedrop ebpf-pcache"
BENCH_MP_EBPF=${BENCH_MP_EBPF:-/mnt/s3demo}
BENCH_MP_FUSE=${BENCH_MP_FUSE:-/mnt/s3fuse-bench}
BENCH_FUSE_CACHE_DIR=${BENCH_FUSE_CACHE_DIR:-/tmp/s3fs-fuse-cache-bench}
BENCH_EBPF_CACHE_DIR=${BENCH_EBPF_CACHE_DIR:-/tmp/s3fs-ebpf-cache-bench}
BENCH_MP=""        # mount point of the backend last brought up
BENCH_KIND=""      # fuse | ebpf

# bench_down — stop whatever bench_up started (every mount point of the six).
bench_down() {
	ebpf_nocache 0 2>/dev/null || true
	ebpf_pcache_drop 0 2>/dev/null || true
	echo 0 | sudo tee "/sys/module/${S3FS_KMOD_NAME}/parameters/partial_modify" >/dev/null 2>&1 || true
	ebpf_stop "$BENCH_MP_EBPF" >/dev/null 2>&1 || true
	umount_hard "$BENCH_MP_FUSE"
	BENCH_MP=""
}

# bench_up <backend> <daemon-log> [daemon flags...] — fresh mount (and daemon) of one
# configuration with EMPTY caches (disk cache dir wiped, daemon restarted). Sets
# BENCH_MP / BENCH_KIND. S3_BUCKET must be set (s3fs-fuse and the daemon use it).
bench_up() {
	local be=$1 logf=$2; shift 2
	bench_down
	case $be in
	fuse-raw)
		fuse_mount_raw "$BENCH_MP_FUSE"; BENCH_KIND=fuse; BENCH_MP=$BENCH_MP_FUSE ;;
	fuse-diskcache)
		fuse_mount_diskcache "$BENCH_MP_FUSE" "$BENCH_FUSE_CACHE_DIR"; BENCH_KIND=fuse; BENCH_MP=$BENCH_MP_FUSE ;;
	fuse-cache)
		fuse_mount_cache "$BENCH_MP_FUSE" "$BENCH_FUSE_CACHE_DIR"; BENCH_KIND=fuse; BENCH_MP=$BENCH_MP_FUSE ;;
	ebpf-nocache|ebpf-nocache-diskcache|ebpf-pcachedrop|ebpf-pcache)
		ebpf_mount "$BENCH_MP_EBPF"
		local cd=()
		if [ "$be" != ebpf-nocache ]; then
			sudo rm -rf "$BENCH_EBPF_CACHE_DIR"; mkdir -p "$BENCH_EBPF_CACHE_DIR"
			cd=(--cache-dir="$BENCH_EBPF_CACHE_DIR")
		fi
		ebpf_start_daemon "$logf" --bucket="$S3_BUCKET" --arena-metadata-only "${cd[@]}" \
			--policy-bpf=builtin:policy_observe_ops --metrics-addr=:9600 "$@"
		case $be in
			ebpf-nocache|ebpf-nocache-diskcache) ebpf_nocache 1 ;;
			ebpf-pcachedrop) ebpf_pcache_drop 1 ;;
		esac
		BENCH_KIND=ebpf; BENCH_MP=$BENCH_MP_EBPF ;;
	*) echo "bench_up: unknown backend $be" >&2; return 1 ;;
	esac
}

# bench_env_report — conditions for the record (date, kernel, commit, options).
bench_env_report() {
	echo "date_utc: $(date -u +%FT%TZ)"
	echo "uname: $(uname -srv)"
	echo "git: $(git -C "$TOPDIR" rev-parse HEAD)"
	echo "cpus: $(nproc)  mem: $(free -m | awk '/Mem:/{print $2}') MiB"
	echo "s3fs: $(s3fs --version 2>&1 | head -1)"
	echo "fuse-raw:       s3fs <bucket> (no use_cache, no kernel_cache)"
	echo "fuse-diskcache: s3fs -o use_cache=<dir> -o max_stat_cache_size=100000 (no kernel_cache)"
	echo "fuse-cache:     s3fs -o use_cache=<dir> -o kernel_cache -o max_stat_cache_size=100000"
	echo "ebpf-*:         fastpath --arena-metadata-only --policy-bpf=builtin:policy_observe_ops (+ --cache-dir except nocache)"
	echo "ebpf-nocache:   kmod nocache=1;  ebpf-pcachedrop: kmod pcache_drop=1;  ebpf-pcache: defaults"
	echo "partial_modify: daemon default (on); partial_modify_min default"
}

# --- Mountpoint for Amazon S3 (I-01) -----------------------------------------

# mount_s3_available — true if mount-s3 is in PATH.
mount_s3_available() { command -v mount-s3 >/dev/null 2>&1; }

# mount_s3_mount <mp> <cache-dir> — mount $S3_BUCKET with Mountpoint for
# Amazon S3.  Credentials come from $MOUNT_S3_ACCESS_KEY /
# $MOUNT_S3_SECRET_KEY (defaults to MinIO admin creds).  Endpoint from
# $S3_URL.  Requires mount-s3 ≥ 1.0 (AWS Mountpoint).
mount_s3_mount() {
	local mp=$1 cache_dir=$2
	umount_hard "$mp"
	sudo mkdir -p "$mp" "$cache_dir"
	sudo env \
		AWS_ACCESS_KEY_ID="${MOUNT_S3_ACCESS_KEY:-minioadmin}" \
		AWS_SECRET_ACCESS_KEY="${MOUNT_S3_SECRET_KEY:-minioadmin}" \
		mount-s3 "$S3_BUCKET" "$mp" \
			--endpoint-url "$S3_URL" \
			--allow-other \
			--cache "$cache_dir" \
			--region us-east-1 \
		|| return 1
}

# mount_s3_umount <mp>
mount_s3_umount() { umount_hard "$1"; }

# --- misc ---------------------------------------------------------------------

# perf_bin — newest concrete perf (Ubuntu's /usr/bin/perf wrapper fails on
# mainline kernels, same as bpftool), else plain `perf`.
perf_bin() {
	local p
	# shellcheck disable=SC2012
	p=$(ls -1dv /usr/lib/linux-tools/*/perf 2>/dev/null | tail -n1)
	echo "${p:-perf}"
}
