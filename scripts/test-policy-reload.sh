#!/usr/bin/env bash
# test-policy-reload.sh — POST /policy/reload is one BPF_LINK_UPDATE (issue #35).
#
#   1. Start under policy_observe_ops, readers cat a file in a loop (nocache=1 so every
#      open reaches .read_folio and the policy hooks).
#   2. Reload through several struct_ops policies and back, N times.  Every reload must
#      return 200 and reads must never fail or return wrong bytes.
#   3. A fentry / fmod_ret style (non struct_ops) object must be refused (HTTP 400) and
#      the previous policy must keep running.
#   4. dmesg must show "policy struct_ops updated (hot swap)" for every reload and no
#      "registered" / "unregistered (superseded" line (the old attach+close path).
#
# Usage: scripts/test-policy-reload.sh     Exit 0 = PASS, 1 = FAIL.
set -euo pipefail

TOPDIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
# shellcheck source=lib/bench.sh
source "$TOPDIR/scripts/lib/bench.sh"

MOUNT=${MOUNT:-/mnt/s3demo}
OUT=${OUT:-/tmp/test-policy-reload}
N=${N:-10}
S3_BUCKET=${POLICY_RELOAD_BUCKET:-s3fs-policy-reload}
TEST_NAME=${TEST_NAME:-test-policy-reload}
METRICS=${METRICS:-http://localhost:9600}
POLICIES=(policy_lru_ops policy_sieve_ops policy_freq2bit_ops policy_observe_ops)
mkdir -p "$OUT"
READERS=()
cleanup() {
	touch "$OUT/stop"
	for p in "${READERS[@]}"; do wait "$p" 2>/dev/null || true; done
	ebpf_stop "$MOUNT" 2>/dev/null || true
}
trap cleanup EXIT
need_cmds docker curl sha256sum
need_built
rm -f "$OUT/stop" "$OUT"/reader-*.err

docker exec "$MINIO_CONTAINER" mc alias set local http://localhost:9000 minioadmin minioadmin >/dev/null 2>&1
docker exec "$MINIO_CONTAINER" mc rm --recursive --force "local/$S3_BUCKET" >/dev/null 2>&1 || true
docker exec "$MINIO_CONTAINER" mc mb --ignore-existing "local/$S3_BUCKET" >/dev/null
head -c $((2 * 1024 * 1024)) /dev/urandom > "$OUT/a.bin"
WANT=$(sha256sum < "$OUT/a.bin" | cut -d' ' -f1)
docker exec -i "$MINIO_CONTAINER" mc pipe "local/$S3_BUCKET/a.bin" < "$OUT/a.bin" >/dev/null

ebpf_mount "$MOUNT"
ebpf_start_daemon "$OUT/daemon.log" --bucket="$S3_BUCKET" --arena-metadata-only \
	--policy-bpf=builtin:policy_observe_ops --softcap=64 --cache-capacity=0 \
	--metrics-addr=:9600 --sample-every=1s --sync-every=0
sleep 4
ebpf_nocache 1
sudo dmesg -C

reader() {
	local id=$1 bad=0 n=0
	while [ ! -e "$OUT/stop" ]; do
		got=$(sudo sha256sum < "$MOUNT/a.bin" 2>/dev/null | cut -d' ' -f1) || got=""
		n=$((n + 1))
		[ "$got" = "$WANT" ] || bad=$((bad + 1))
	done
	echo "$n $bad" > "$OUT/reader-$id.res"
}
for i in 1 2 3 4; do reader "$i" & READERS+=($!); done

overall=PASS
bad() { echo "FAIL: $*" >&2; overall=FAIL; }

reloads=0
for round in $(seq "$N"); do
	for pol in "${POLICIES[@]}"; do
		code=$(curl -s -o "$OUT/reload.out" -w '%{http_code}' -X POST --data "builtin:$pol" "$METRICS/policy/reload")
		[ "$code" = 200 ] || bad "reload to $pol: HTTP $code ($(cat "$OUT/reload.out"))"
		reloads=$((reloads + 1))
	done
done

# A non struct_ops object must be refused with 400.  Any non-struct_ops .bpf.o will do.
if [ -f "$TOPDIR/bpf/s3fs_fastpath.bpf.o" ]; then
	code=$(curl -s -o "$OUT/reload-bad.out" -w '%{http_code}' -X POST --data "file:$TOPDIR/bpf/s3fs_fastpath.bpf.o" "$METRICS/policy/reload")
	[ "$code" = 400 ] || bad "non-struct_ops reload: HTTP $code, want 400 ($(cat "$OUT/reload-bad.out"))"
fi
code=$(curl -s -o /dev/null -w '%{http_code}' -X POST --data "builtin:policy_sieve_ops" "$METRICS/policy/reload")
[ "$code" = 200 ] || bad "reload after the refused one: HTTP $code"
reloads=$((reloads + 1))

touch "$OUT/stop"
for p in "${READERS[@]}"; do wait "$p" 2>/dev/null || true; done
READERS=()
total=0; badreads=0
for f in "$OUT"/reader-*.res; do read -r n b < "$f"; total=$((total + n)); badreads=$((badreads + b)); done
echo "reloads=$reloads reads=$total bad_reads=$badreads"
[ "$badreads" -eq 0 ] || bad "$badreads reads failed or returned wrong bytes"
[ "$total" -gt 0 ] || bad "no reads ran"

swaps=$(sudo dmesg | grep -c 'policy struct_ops updated (hot swap)' || true)
other=$(sudo dmesg | grep -cE 'policy struct_ops registered|unregistered \(superseded' || true)
echo "dmesg: hot_swap=$swaps registered/superseded=$other"
[ "$swaps" -ge "$reloads" ] || bad "hot swap log lines $swaps < reloads $reloads"
[ "$other" -eq 0 ] || bad "old attach+close path logged $other lines"

docker exec "$MINIO_CONTAINER" mc rb --force "local/$S3_BUCKET" >/dev/null 2>&1 || true
printf 'RESULT name=%s status=%s reloads=%s reads=%s bad_reads=%s\n' "$TEST_NAME" "$overall" "$reloads" "$total" "$badreads"
[[ "$overall" == FAIL ]] && exit 1 || exit 0
