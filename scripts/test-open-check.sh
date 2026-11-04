#!/usr/bin/env bash
# test-open-check.sh — kmod open_check: open(2) without O_CREAT asks S3 (HEAD).
#
# The kmod answers lookup/open from its entry table, which only the daemon's
# sync refreshes.  A key deleted from S3 after the last sync therefore still
# opened fine, and the error showed up later (at the first read, or never when
# the page cache held it).  s3fs-fuse HEADs in its open.  open_check=1 does the
# same: .open sends an S3FS_MISS_FLAG_OPEN_CHECK upcall and returns the
# daemon's error (-ENOENT on 404).  Default 0 keeps the old, HEAD-free open.
#
# Checks (daemon --sync-every=0, so the entry table stays stale):
#   open_check=0: open of a key deleted from S3 succeeds, the read fails EIO (old behaviour)
#   open_check=1: that open fails ENOENT; an existing key opens, reads the right
#                 bytes; O_CREAT on the deleted key still opens (no check);
#                 a new file can be created, written, flushed and reopened;
#                 the daemon counted the non-O_CREAT opens (open_checks_total)
#                 and exactly one missing object.
#
# Usage: sudo ./scripts/test-open-check.sh      Exit: 0 PASS, 1 FAIL.
set -euo pipefail
TOPDIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
S3_BUCKET=${OPEN_CHECK_BUCKET:-s3fs-open-check}
# shellcheck source=lib/bench.sh
source "$TOPDIR/scripts/lib/bench.sh"
MOUNT=${MOUNT:-/mnt/s3demo}
OUT=${OUT:-/tmp/test-open-check}
TEST_NAME=${TEST_NAME:-test-open-check}
METRICS_URL=${METRICS_URL:-http://localhost:9600/metrics}
mkdir -p "$OUT"
trap 'ebpf_open_check 0 >/dev/null 2>&1 || true; ebpf_stop "$MOUNT" >/dev/null 2>&1 || true' EXIT
need_cmds docker curl python3
need_built
docker exec "$MINIO_CONTAINER" mc alias set local http://localhost:9000 minioadmin minioadmin >/dev/null 2>&1
docker exec "$MINIO_CONTAINER" mc rm --recursive --force "local/$S3_BUCKET" >/dev/null 2>&1 || true
docker exec "$MINIO_CONTAINER" mc mb --ignore-existing "local/$S3_BUCKET" >/dev/null
printf 'keep-bytes\n' | docker exec -i "$MINIO_CONTAINER" mc pipe "local/$S3_BUCKET/keep.txt" >/dev/null
printf 'gone-bytes\n' | docker exec -i "$MINIO_CONTAINER" mc pipe "local/$S3_BUCKET/gone.txt" >/dev/null
ebpf_mount "$MOUNT"
ebpf_start_daemon "$OUT/daemon.log" --bucket="$S3_BUCKET" --arena-metadata-only \
    --policy-bpf="builtin:policy_observe_ops" --softcap=8 --cache-capacity=0 \
    --metrics-addr=:9600 --sample-every=1s --sync-every=0
sleep 3
# Both keys are in the kmod's table now; delete one behind its back.
sudo ls "$MOUNT" >/dev/null
docker exec "$MINIO_CONTAINER" mc rm "local/$S3_BUCKET/gone.txt" >/dev/null

counter() { curl -s "$METRICS_URL" | awk -v n="$1" '$1==n { print $2 }'; }
overall=PASS
check() { # name want got
    if [ "$2" = "$3" ]; then echo "$1 PASS ($3)"; else echo "$1 FAIL: want $2, got $3"; overall=FAIL; fi
}
# opn <mode: r|c> <name> — print "<errno>" when open fails, else
# "ok:<first line>" (or "ok:!<errno>" when the open worked but the read failed,
# e.g. EIO for a key that is gone from S3).
opn() {
    sudo python3 - "$1" "$MOUNT/$2" <<'PY'
import errno, os, sys
mode, path = sys.argv[1], sys.argv[2]
try:
    fd = os.open(path, os.O_RDONLY | (os.O_CREAT if mode == 'c' else 0), 0o644)
except OSError as e:
    print(errno.errorcode[e.errno]); sys.exit(0)
try:
    print('ok:' + os.read(fd, 64).decode().strip())
except OSError as e:
    print('ok:!' + errno.errorcode[e.errno])
os.close(fd)
PY
}

check "off: deleted key still opens, fails only at read (old behaviour)" "ok:!EIO" "$(opn r gone.txt)"

ebpf_open_check 1
before=$(counter s3fs_ebpf_open_checks_total)
check "on: deleted key -> ENOENT" "ENOENT" "$(opn r gone.txt)"
check "on: existing key opens and reads" "ok:keep-bytes" "$(opn r keep.txt)"
check "on: O_CREAT is not checked" "ok:!EIO" "$(opn c gone.txt)"
sudo sh -c "printf 'fresh\\n' > '$MOUNT/new.txt'"
check "on: new file created and written" "ok:fresh" "$(opn r new.txt)"
sleep 5   # let the close's PUT land: new.txt is then a clean file the daemon must know
check "on: flushed new file reopens (daemon knows the key)" "ok:fresh" "$(opn r new.txt)"
after=$(counter s3fs_ebpf_open_checks_total)
# gone(r), keep(r), and the reopen after the flush: at least 3 (the O_CREAT
# open and the shell redirect's create are not checked).
check "daemon open_checks_total delta >= 3" "1" "$([ $((after - before)) -ge 3 ] && echo 1 || echo 0)"
check "daemon open_check_missing_total == 1" "1" "$(counter s3fs_ebpf_open_check_missing_total)"
printf 'RESULT name=%s status=%s\n' "$TEST_NAME" "$overall"
[[ "$overall" == PASS ]]
