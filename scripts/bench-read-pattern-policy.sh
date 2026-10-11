#!/usr/bin/env bash
# scripts/bench-read-pattern-policy.sh — R-01 pattern-aware admit scenario bench.
#
# Demonstrates how fentry/generic_file_read_iter's sequential-pattern
# signal (s3fs_read_pattern_map) reduces S3 round-trips for large sequential
# reads via --pattern-aware-admit.
#
# Scenario
# --------
# A large file (BIG_FILE, default 10m.bin) is NOT pre-warmed at startup
# (bench-no-large-prewarm.cel returns prewarm=false for files > 4 MiB).
# When the user reads it:
#
#   WITHOUT --pattern-aware-admit (D-01 baseline):
#     Each 4 MiB miss fires a separate GetRange RPC.
#     For a 10 MiB file: 3 upcalls × ~30 ms each ≈ 90 ms cold-read tail.
#
#   WITH    --pattern-aware-admit (R-01):
#     fentry fires before kmod blocks, so seq_count is already ≥ 1 when
#     the first goroutine runs. SeqRatio() = 1.0 → full-file GET on the
#     first miss. Remaining misses are redundant and no-op (ino already
#     admitted). Result: 1 upcall × ~30 ms = 3× lower cold-read tail.
#
# Metrics reported
# ----------------
#   - read-miss upcall count (grep "[upcall] miss ino=")
#   - first cold-read wall-clock time (time cat)
#   - pattern-aware promote log lines ("[upcall] ... → pattern-aware")
#
# Prerequisites:
#   - make compose-up (MinIO up, bucket seeded with big.bin / 10m.bin)
#   - make build (kmod + daemon)

set -euo pipefail

TOPDIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
# shellcheck source=lib/bench.sh
source "$TOPDIR/scripts/lib/bench.sh"

OUT_DIR=${OUT_DIR:-/tmp/bench}
MP_EBPF=/mnt/s3demo
BIG_FILE=${BIG_FILE:-10m.bin}
POLICY="$TOPDIR/policies/bench-no-large-prewarm.cel"

mkdir -p "$OUT_DIR"
log() { echo "[bench-read-pattern] $*" >&2; }

check_prereq() {
	need_built
	minio_require "$BIG_FILE"
	[ -f "$POLICY" ] \
		|| { echo "policy missing: $POLICY"; exit 1; }
}

DAEMON_LOG=""

# start_ebpf <label> [extra-flags...]
# Loads kmod, mounts, starts daemon with no-prewarm-large policy.
# Waits until entry sync is visible so the large file is in the entry
# table but NOT admitted (policy returns prewarm=false for it).
start_ebpf() {
	local label="$1"; shift
	DAEMON_LOG="$OUT_DIR/fastpath-$label.log"
	log "starting daemon label=$label extra='$*'"
	ebpf_mount "$MP_EBPF"
	# Ready = initial sync done, so the entry table includes $BIG_FILE.
	DAEMON_READY_PATTERN='\[sync\] initial:' ebpf_start_daemon "$DAEMON_LOG" \
		--sample-every=300s --policy="$POLICY" --metrics-addr="" "$@"
	# Give the prewarm loop a moment to skip large files.
	sleep 0.5
	log "daemon ready; $BIG_FILE entry live but NOT admitted (prewarm=false)"
}

stop_ebpf() {
	ebpf_stop "$MP_EBPF"
}
trap stop_ebpf EXIT

count_upcalls() {
	grep -c '\[upcall\] miss ino=' "$1" 2>/dev/null || true
}

check_prereq

# ── Phase 1: WITHOUT pattern-aware admit (D-01 chunk baseline) ───────────
log "=== phase 1: WITHOUT --pattern-aware-admit (chunk baseline) ==="
start_ebpf "without"

log "cold-read $BIG_FILE (expect ceil(size/4MiB) upcalls)"
T_WITHOUT=$( { time cat "$MP_EBPF/$BIG_FILE" > /dev/null; } 2>&1 )
sleep 0.5  # let goroutines flush log lines

UPCALLS_WITHOUT=$(count_upcalls "$DAEMON_LOG")
PROMOTES_WITHOUT=$(grep -c 'PROMOTE_SIGNAL=pattern-aware' "$DAEMON_LOG" 2>/dev/null || true)
log "upcalls=$UPCALLS_WITHOUT  promotes=$PROMOTES_WITHOUT"
LOG_WITHOUT="$DAEMON_LOG"
stop_ebpf

# ── Phase 2: WITH pattern-aware admit (R-01) ─────────────────────────────
log "=== phase 2: WITH --pattern-aware-admit (R-01) ==="
start_ebpf "with" "--pattern-aware-admit"

log "cold-read $BIG_FILE (expect 1 upcall — full-file GET on first miss)"
T_WITH=$( { time cat "$MP_EBPF/$BIG_FILE" > /dev/null; } 2>&1 )
sleep 0.5

UPCALLS_WITH=$(count_upcalls "$DAEMON_LOG")
PROMOTES_WITH=$(grep -c 'PROMOTE_SIGNAL=pattern-aware' "$DAEMON_LOG" 2>/dev/null || true)
log "upcalls=$UPCALLS_WITH  promotes=$PROMOTES_WITH"
LOG_WITH="$DAEMON_LOG"
stop_ebpf
trap - EXIT

# ── Get file size from MinIO for reference ────────────────────────────────
FILE_BYTES=$(docker exec "$MINIO_CONTAINER" mc stat "local/$S3_BUCKET/$BIG_FILE" 2>/dev/null \
	| awk '/Size:/{gsub(/[^0-9]/,"",$2); print $2+0}')
FILE_MIB=$(awk "BEGIN{printf \"%.1f\", $FILE_BYTES/1048576}")
EXPECTED_CHUNKS=$(awk "BEGIN{printf \"%d\", int(($FILE_BYTES+4194303)/4194304)}")

# ── Report ────────────────────────────────────────────────────────────────
{
	echo "## R-01 pattern-aware admit scenario: sequential large-file cold-read"
	echo
	echo "file: \`$BIG_FILE\`  size: ${FILE_MIB} MiB"
	echo "policy: \`bench-no-large-prewarm.cel\` (prewarm=false for >4 MiB)"
	echo
	echo "### Upcall count (S3 GetRange/Get RPCs triggered)"
	echo
	echo "| variant | upcalls | promotes | notes |"
	echo "|---|---|---|---|"
	echo "| D-01 chunk (no pattern) | $UPCALLS_WITHOUT | $PROMOTES_WITHOUT | expected ≈ $EXPECTED_CHUNKS chunks × 4 MiB |"
	echo "| R-01 pattern-aware      | $UPCALLS_WITH    | $PROMOTES_WITH    | expected = 1 full-file GET |"
	echo
	echo "### Cold-read wall-clock time"
	echo
	echo '```'
	echo "WITHOUT pattern-aware:"
	echo "$T_WITHOUT"
	echo
	echo "WITH pattern-aware:"
	echo "$T_WITH"
	echo '```'
	echo
	echo "### Interpretation"
	echo "With a 4 MiB chunk size, a ${FILE_MIB} MiB file triggers ≈ $EXPECTED_CHUNKS chunk upcalls"
	echo "on cold sequential read. Each upcall serialises a S3 GetRange RPC (~30 ms on"
	echo "local MinIO; higher on real S3). Pattern-aware collapses this to one full-file"
	echo "GET (single TCP pipeline), reducing cold-read latency proportionally."
	echo
	echo "The fentry hook fires before kmod's .read_folio blocks, so seq_count ≥ 1"
	echo "when the first goroutine runs. SeqRatio() = 1.0 → full-admit promotion on"
	echo "the very first miss — no warm-up reads required."
	echo
	echo "### Daemon log excerpts"
	echo '```'
	echo "--- WITHOUT (chunk upcalls) ---"
	grep '\[upcall\]' "$LOG_WITHOUT" | head -20 || true
	echo
	echo "--- WITH (pattern-aware) ---"
	grep '\[upcall\]' "$LOG_WITH" | head -20 || true
	echo '```'
} >"$OUT_DIR/read-pattern-policy.md"

cat "$OUT_DIR/read-pattern-policy.md" >&2
log "done → $OUT_DIR/read-pattern-policy.md"
