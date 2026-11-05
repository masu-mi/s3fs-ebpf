#!/usr/bin/env bash
# test-parity.sh — differential test: the same operations on s3fs-fuse (the
# reference) and on s3fs-ebpf must give the same result.
#
# Why: "ebpf-nocache is 3.2x faster than fuse-raw" stood for months while the
# two did different work: s3fs-fuse HEADs on every open, s3fs-ebpf answered
# open from its entry table (br 18.51). A speed ratio is only meaningful when
# the observable behaviour matches, so this runs a fixed list of operations on
# both and diffs the outcomes (data read, or the errno).
#
# s3fs-ebpf runs as a user would: default pcache mode, open_check=1 (PARITY_OPEN_CHECK=0
# to see what the old open did), daemon
# --sync-every=0 (so the entry table stays stale unless the test changes it).
# Differences that are known and understood are listed in KNOWN_DIFF with a
# reason (and a backlog id); the test fails on any OTHER difference, and warns
# when a known one has disappeared (remove it from the list then).
#
# Usage: sudo ./scripts/test-parity.sh      Exit: 0 PASS, 1 FAIL.
set -euo pipefail
TOPDIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
S3_BUCKET=${PARITY_BUCKET:-s3fs-parity}
# shellcheck source=lib/bench.sh
source "$TOPDIR/scripts/lib/bench.sh"
OUT=${OUT:-/tmp/test-parity}
TEST_NAME=${TEST_NAME:-test-parity}
SETTLE=${SETTLE:-3}   # seconds for the daemon's async flush / rename / delete to reach S3
mkdir -p "$OUT"
trap 'bench_down >/dev/null 2>&1 || true' EXIT
need_cmds docker python3
need_built
minio_require
mc() { docker exec "$MINIO_CONTAINER" mc "$@"; }
mc alias set local http://localhost:9000 minioadmin minioadmin >/dev/null 2>&1

# op name -> reason (backlog id). Keep this list short: every entry is a gap.
declare -A KNOWN_DIFF=()
[ -f "$TOPDIR/scripts/lib/parity-known-diff.sh" ] && source "$TOPDIR/scripts/lib/parity-known-diff.sh"

seed() {
	mc rm --recursive --force "local/$S3_BUCKET" >/dev/null 2>&1 || true
	mc mb --ignore-existing "local/$S3_BUCKET" >/dev/null
	local k
	for k in keep.txt keep2.txt gone.txt src.txt del.txt; do
		printf 'content-of-%s\n' "$k" | docker exec -i "$MINIO_CONTAINER" mc pipe "local/$S3_BUCKET/$k" >/dev/null
	done
}

run_ops() { # <mountpoint> <outfile>
	sudo env MP="$1" BUCKET="$S3_BUCKET" CT="$MINIO_CONTAINER" SETTLE="$SETTLE" python3 -I - >"$2" <<'PY'
import errno, os, subprocess, time
mp, bucket, ct, settle = os.environ['MP'], os.environ['BUCKET'], os.environ['CT'], float(os.environ['SETTLE'])
def p(n): return os.path.join(mp, n)
def mc(*a): subprocess.run(['docker', 'exec', ct, 'mc', *a], check=True, capture_output=True)
def op(name, fn):
    try: r = fn()
    except OSError as e: r = errno.errorcode.get(e.errno, str(e.errno))
    print('%s\t%s' % (name, r), flush=True)
def rd(n):
    fd = os.open(p(n), os.O_RDONLY)
    try: return os.read(fd, 4096).decode().strip()
    finally: os.close(fd)
def wr(n, flags, data):
    fd = os.open(p(n), flags, 0o644)
    try: os.write(fd, data)
    finally: os.close(fd)

os.listdir(mp)                                   # the listing every user does first
op('open_existing',        lambda: rd('keep.txt'))
op('open_missing',         lambda: rd('no-such-file.txt'))
op('create_excl_existing', lambda: wr('keep.txt', os.O_WRONLY | os.O_CREAT | os.O_EXCL, b'x'))
op('create_write_reopen',  lambda: (wr('new.txt', os.O_WRONLY | os.O_CREAT | os.O_EXCL, b'abc'), time.sleep(settle), rd('new.txt'))[2])
op('trunc_write_reopen',   lambda: (wr('keep2.txt', os.O_WRONLY | os.O_TRUNC, b'x'), time.sleep(settle), rd('keep2.txt'))[2])
op('rename_open_new',      lambda: (os.rename(p('src.txt'), p('dst.txt')), time.sleep(settle), rd('dst.txt'))[2])
op('rename_open_old',      lambda: rd('src.txt'))
op('unlink_open',          lambda: (os.unlink(p('del.txt')), time.sleep(settle), rd('del.txt'))[2])
# Another client deletes / creates a key behind the mount's back.
mc('rm', 'local/%s/gone.txt' % bucket)
op('ext_delete_open',      lambda: rd('gone.txt'))
op('ext_delete_stat',      lambda: (os.stat(p('gone.txt')), 'exists')[1])
op('ext_delete_listdir',   lambda: 'listed' if 'gone.txt' in os.listdir(mp) else 'absent')
subprocess.run('printf "ext\\n" | docker exec -i %s mc pipe local/%s/ext.txt' % (ct, bucket), shell=True, check=True, capture_output=True)
op('ext_create_open',      lambda: rd('ext.txt'))
PY
}

declare -A res
for be in fuse-raw ebpf-pcache; do
	seed
	if [ "$be" = ebpf-pcache ]; then export BENCH_OPEN_CHECK=${PARITY_OPEN_CHECK:-1}; else export BENCH_OPEN_CHECK=0; fi
	bench_up "$be" "$OUT/daemon-$be.log" --sync-every=0
	sleep 3
	run_ops "$BENCH_MP" "$OUT/$be.tsv"
	bench_down
done

overall=PASS
echo "op: fuse-raw vs ebpf-pcache (open_check=${PARITY_OPEN_CHECK:-1})"
while IFS=$'\t' read -r name f; do
	e=$(awk -F'\t' -v n="$name" '$1==n{print $2}' "$OUT/ebpf-pcache.tsv")
	if [ "$f" = "$e" ]; then
		if [ -n "${KNOWN_DIFF[$name]:-}" ]; then echo "$name: same ($f) — WARN: KNOWN_DIFF entry no longer needed"; else echo "$name: same ($f)"; fi
	elif [ -n "${KNOWN_DIFF[$name]:-}" ]; then
		echo "$name: DIFF fuse=$f ebpf=$e — known: ${KNOWN_DIFF[$name]}"
	else
		echo "$name: DIFF fuse=$f ebpf=$e — UNEXPECTED"; overall=FAIL
	fi
done < "$OUT/fuse-raw.tsv"
printf 'RESULT name=%s status=%s\n' "$TEST_NAME" "$overall"
[[ "$overall" == PASS ]]
