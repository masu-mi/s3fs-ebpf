#!/usr/bin/env bash
# test-policy-data-members.sh — struct_ops DATA members (flags / name) and validate(), on a real
# kernel (docs/dev-notes/struct-ops-data-members.md).
#
# The kmod's s3fs_policy_ops carries, besides the hooks, `flags` and `name`.  The kernel accepts a
# non-function member only if the type's init_member takes it, so this checks the whole path:
# BPF initializer -> libbpf -> bpf(BPF_MAP_UPDATE_ELEM) -> init_member -> validate -> reg / update.
#
#   1. policy_observe_ops (.name = "observe")      -> dmesg "registered (... name=observe flags=0x0)"
#   2. reload policy_test_named_ops (name + flag)  -> 200, dmesg "updated (hot swap) name=named_ok flags=0x1"
#   3. reload each REFUSED fixture                 -> not 200, no "updated" line, the active policy stays
#        badflags    (unknown flag bit)       init_member -> -EINVAL
#        badname     (name has a space)       init_member -> -EINVAL
#        noonhit_hit (NO_ON_HIT + on_hit)     validate    -> -EINVAL
#        pickonly    (pick_victim alone)      validate    -> -EINVAL
#   4. reload policy_observe_ops again             -> 200, "updated ... name=observe"
#   Reads of a file succeed (sha256) after every step.
#
# Usage: sudo -E scripts/test-policy-data-members.sh      Exit 0 = PASS, 1 = FAIL, 2 = prereq missing.
set -euo pipefail

TOPDIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
# shellcheck source=lib/bench.sh
source "$TOPDIR/scripts/lib/bench.sh"

MOUNT=${MOUNT:-/mnt/s3demo}
OUT=${OUT:-/tmp/test-policy-data-members}
S3_BUCKET=${DATA_MEMBERS_BUCKET:-s3fs-policy-data-members}
TEST_NAME=${TEST_NAME:-test-policy-data-members}
METRICS=${METRICS:-http://localhost:9600}
mkdir -p "$OUT"
cleanup() { ebpf_stop "$MOUNT" 2>/dev/null || true; }
trap cleanup EXIT
need_cmds docker curl sha256sum
need_built
for o in policy_observe_ops policy_test_named_ops policy_test_badflags_ops policy_test_badname_ops \
	policy_test_noonhit_hit_ops policy_test_pickonly_ops; do
	[ -f "$TOPDIR/bpf/$o.bpf.o" ] || { echo "bpf/$o.bpf.o missing — run make bpf" >&2; exit 2; }
done

mc() { docker exec "$MINIO_CONTAINER" mc "$@"; }
mc alias set local http://localhost:9000 minioadmin minioadmin >/dev/null 2>&1
mc rm --recursive --force "local/$S3_BUCKET" >/dev/null 2>&1 || true
mc mb --ignore-existing "local/$S3_BUCKET" >/dev/null
head -c 65536 /dev/urandom > "$OUT/a.bin"
WANT=$(sha256sum < "$OUT/a.bin" | awk '{print $1}')
docker exec -i "$MINIO_CONTAINER" mc pipe "local/$S3_BUCKET/a.bin" < "$OUT/a.bin" >/dev/null

ebpf_mount "$MOUNT"
sudo dmesg -C
ebpf_start_daemon "$OUT/daemon.log" --bucket="$S3_BUCKET" --arena-metadata-only \
	--policy-bpf=builtin:policy_observe_ops --softcap=8 --cache-capacity=0 \
	--metrics-addr=:9600 --sample-every=1s --sync-every=0
sleep 3

overall=PASS
bad() { echo "FAIL: $*" >&2; overall=FAIL; }
swaps() { sudo dmesg | grep -c 'policy struct_ops updated (hot swap)' || true; }
reads_ok() { [ "$(sudo sha256sum < "$MOUNT/a.bin" | awk '{print $1}')" = "$WANT" ] || bad "read after $1: sha256 differs"; }
reload() { # <policy> -> prints the HTTP code, body in $OUT/reload.out
	curl -s -o "$OUT/reload.out" -w '%{http_code}' -X POST --data "builtin:$1" "$METRICS/policy/reload"
}

# 1. the policy loaded at start
sudo dmesg | grep -q 'policy struct_ops registered (ops=.* name=observe flags=0x0)' \
	|| bad "start: no 'registered (… name=observe flags=0x0)' in dmesg"
reads_ok start

# 2. a valid reload carries its data with the vtable
n0=$(swaps)
code=$(reload policy_test_named_ops)
[ "$code" = 200 ] || bad "reload named_ops: HTTP $code ($(cat "$OUT/reload.out"))"
sudo dmesg | grep -q 'policy struct_ops updated (hot swap) name=named_ok flags=0x1' \
	|| bad "named_ops: no 'updated (hot swap) name=named_ok flags=0x1' in dmesg"
[ "$(swaps)" -eq $((n0 + 1)) ] || bad "named_ops: expected one more hot swap"
reads_ok named_ops

# 3. refused ones: not 200, nothing swapped, the named policy stays
for fx in badflags badname noonhit_hit pickonly; do
	n=$(swaps)
	code=$(reload "policy_test_${fx}_ops")
	if [ "$code" = 200 ]; then
		bad "$fx: reload returned 200, the kernel should have refused it"
	else
		echo "$fx: refused (HTTP $code): $(tail -c 110 "$OUT/reload.out" | tr '\n' ' ')"
		# the reason is -EINVAL from init_member / validate, not some other load failure
		grep -qi 'invalid argument' "$OUT/reload.out" || bad "$fx: refused, but not with EINVAL"
	fi
	[ "$(swaps)" -eq "$n" ] || bad "$fx: a hot swap happened although the load was refused"
	reads_ok "$fx"
done
code=$(reload policy_test_named_ops)       # still the named one: updating to itself works, proving the link survived
[ "$code" = 200 ] || bad "link broken after refused loads: reload named_ops HTTP $code"

# 4. back to observe
code=$(reload policy_observe_ops)
[ "$code" = 200 ] || bad "reload observe_ops: HTTP $code"
sudo dmesg | grep -q 'policy struct_ops updated (hot swap) name=observe flags=0x0' \
	|| bad "observe_ops: no 'updated (hot swap) name=observe flags=0x0' in dmesg"
reads_ok observe

mc rb --force "local/$S3_BUCKET" >/dev/null 2>&1 || true
printf 'RESULT name=%s status=%s\n' "$TEST_NAME" "$overall"
[[ "$overall" == FAIL ]] && exit 1 || exit 0
