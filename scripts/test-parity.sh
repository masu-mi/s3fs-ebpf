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
	for k in keep.txt keep2.txt gone.txt src.txt del.txt r1.txt w1.txt x1.txt; do
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
# Races the TLA+ model (specs/tla/FileLifecycle.tla) found: no settle between the steps.
def create_rename_quick():                       # model counterexample 2
    wr('q1.txt', os.O_WRONLY | os.O_CREAT | os.O_EXCL, b'q1'); os.rename(p('q1.txt'), p('q2.txt'))
    time.sleep(settle * 2); return rd('q2.txt') + ' | s3:' + ' '.join(sorted(k for k in s3keys() if k.startswith('q')))
def rename_then_reuse_name():                    # model counterexample 3
    os.rename(p('r1.txt'), p('r2.txt')); wr('r1.txt', os.O_WRONLY | os.O_CREAT | os.O_EXCL, b'new-r1')
    time.sleep(settle * 2); return rd('r2.txt') + ' | ' + rd('r1.txt')
def local_rename_reuse_name():                   # model counterexample 3, on a file kmod created itself
    wr('t1.txt', os.O_WRONLY | os.O_CREAT | os.O_EXCL, b'old-t1'); time.sleep(settle)   # flushed: S3 has t1.txt
    os.rename(p('t1.txt'), p('t2.txt')); wr('t1.txt', os.O_WRONLY | os.O_CREAT | os.O_EXCL, b'new-t1')
    time.sleep(settle * 2)
    return rd('t2.txt') + ' | ' + rd('t1.txt') + ' | s3: ' + ' '.join(sorted(k for k in s3keys() if k.startswith('t')))
def fsync_rename_reuse_name():                   # same, but the new file is fsync'ed: that flush is not debounced
    wr('u1.txt', os.O_WRONLY | os.O_CREAT | os.O_EXCL, b'old-u1'); time.sleep(settle)
    os.rename(p('u1.txt'), p('u2.txt'))
    fd = os.open(p('u1.txt'), os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o644)
    os.write(fd, b'new-u1'); os.fsync(fd); os.close(fd)
    time.sleep(settle * 2)
    return rd('u2.txt') + ' | ' + rd('u1.txt') + ' | s3: ' + ' '.join(sorted(k for k in s3keys() if k.startswith('u')))
def unlink_sync_lookup():                        # AC-85: the old name must stay gone across a sync
    wr('v1.txt', os.O_WRONLY | os.O_CREAT | os.O_EXCL, b'v1'); time.sleep(settle)
    os.unlink(p('v1.txt')); time.sleep(settle * 2)       # the daemon deleted it; syncs ran
    return 'stat:' + ('exists' if os.path.exists(p('v1.txt')) else 'absent') + ' listed:' + str('v1.txt' in os.listdir(mp))
def rename_sync_lookup():                        # AC-85: synced file, renamed, then syncs
    os.rename(p('w1.txt'), p('w2.txt')); time.sleep(settle * 2)
    return rd('w2.txt') + ' | old:' + ('exists' if os.path.exists(p('w1.txt')) else 'absent') + ' listed:' + ','.join(sorted(n for n in os.listdir(mp) if n.startswith('w')))
def rename_back():                               # AC-85: mv a c; mv c a
    os.rename(p('x1.txt'), p('x2.txt')); os.rename(p('x2.txt'), p('x1.txt')); time.sleep(settle * 2)
    return rd('x1.txt') + ' | x2:' + ('exists' if os.path.exists(p('x2.txt')) else 'absent')
def s3keys():
    out = subprocess.run(['docker', 'exec', ct, 'mc', 'ls', 'local/%s/' % bucket], capture_output=True, text=True).stdout
    return [l.split()[-1] for l in out.splitlines() if l.strip()]
op('create_rename_quick',     create_rename_quick)
op('rename_then_reuse_name',  rename_then_reuse_name)
op('local_rename_reuse_name', local_rename_reuse_name)
op('fsync_rename_reuse_name', fsync_rename_reuse_name)
op('unlink_sync_lookup',      unlink_sync_lookup)
op('rename_sync_lookup',      rename_sync_lookup)
op('rename_back',             rename_back)
# Another client deletes / creates a key behind the mount's back.
mc('rm', 'local/%s/gone.txt' % bucket)
op('ext_delete_open',      lambda: rd('gone.txt'))
op('ext_delete_stat',      lambda: (os.stat(p('gone.txt')), 'exists')[1])
op('ext_delete_listdir',   lambda: 'listed' if 'gone.txt' in os.listdir(mp) else 'absent')
subprocess.run('printf "ext\\n" | docker exec -i %s mc pipe local/%s/ext.txt' % (ct, bucket), shell=True, check=True, capture_output=True)
op('ext_create_open',      lambda: rd('ext.txt'))
PY
}

# s3fs-ebpf runs twice: --sync-every=0 (the entry table changes only when the test says so)
# and --sync-every=1s (syncs happen between the operations, AC-85); fuse once.
SYNCS=${PARITY_SYNCS:-"0 1s"}
run_one() { # <backend> <sync-every> <out.tsv>
	seed
	if [ "$1" = ebpf-pcache ]; then export BENCH_OPEN_CHECK=${PARITY_OPEN_CHECK:-1}; else export BENCH_OPEN_CHECK=0; fi
	bench_up "$1" "$OUT/daemon-$1-$2.log" --sync-every="$2"
	sleep 3
	run_ops "$BENCH_MP" "$3"
	bench_down
}
run_one fuse-raw 0 "$OUT/fuse-raw.tsv"
for sync in $SYNCS; do run_one ebpf-pcache "$sync" "$OUT/ebpf-pcache-sync$sync.tsv"; done

overall=PASS
for sync in $SYNCS; do
	echo "== op: fuse-raw vs ebpf-pcache (open_check=${PARITY_OPEN_CHECK:-1}, --sync-every=$sync)"
	while IFS=$'\t' read -r name f; do
		e=$(awk -F'\t' -v n="$name" '$1==n{print $2}' "$OUT/ebpf-pcache-sync$sync.tsv")
		if [ "$f" = "$e" ]; then
			if [ -n "${KNOWN_DIFF[$name]:-}" ] && [ "$sync" = 0 ]; then echo "$name: same ($f) — WARN: KNOWN_DIFF entry no longer needed"; else echo "$name: same ($f)"; fi
		elif [ -n "${KNOWN_DIFF[$name]:-}" ]; then
			echo "$name: DIFF fuse=$f ebpf=$e — known: ${KNOWN_DIFF[$name]}"
		else
			echo "$name: DIFF fuse=$f ebpf=$e — UNEXPECTED"; overall=FAIL
		fi
	done < "$OUT/fuse-raw.tsv"
done
printf 'RESULT name=%s status=%s\n' "$TEST_NAME" "$overall"
[[ "$overall" == PASS ]]
