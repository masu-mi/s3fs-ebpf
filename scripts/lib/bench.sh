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
ebpf_mount() {
	local mp=$1
	lsmod | grep -q "^${S3FS_KMOD_NAME} " || sudo insmod "$S3FS_KMOD"
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
	rm -rf "$dir" && mkdir -p "$dir"
	fuse_mount "$mp" -o use_cache="$dir" -o kernel_cache -o max_stat_cache_size=100000
}

# --- misc ---------------------------------------------------------------------

# perf_bin — newest concrete perf (Ubuntu's /usr/bin/perf wrapper fails on
# mainline kernels, same as bpftool), else plain `perf`.
perf_bin() {
	local p
	# shellcheck disable=SC2012
	p=$(ls -1dv /usr/lib/linux-tools/*/perf 2>/dev/null | tail -n1)
	echo "${p:-perf}"
}
