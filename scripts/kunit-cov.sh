#!/usr/bin/env bash
# kunit-cov.sh — run the KUnit suites with clang coverage and write an lcov
# file for kmod/ (the test files themselves are left out).
#
# Usage: KSRC=/path/to/linux-6.12+ scripts/kunit-cov.sh [out.lcov] [kunit.py args...]
#   default out: coverage/kmod.lcov
#
# Needs clang, lld, llvm-profdata, llvm-cov and the profile runtime
# (Ubuntu: sudo apt install clang lld llvm libclang-rt-18-dev).  Override
# the tool names with LLVM_PROFDATA / LLVM_COV.  See COVERAGE=1 in
# scripts/kunit.sh for why this is clang and not gcov, and without KASAN.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
KSRC="${KSRC:?set KSRC to a Linux source tree (>= 6.12)}"
KSRC="$(cd "$KSRC" && pwd)"
OUT="${1:-$ROOT/coverage/kmod.lcov}"
(($#)) && shift
LLVM_PROFDATA="${LLVM_PROFDATA:-llvm-profdata}"
LLVM_COV="${LLVM_COV:-llvm-cov}"
BUILD_DIR="${BUILD_DIR:-$KSRC/.kunit-s3fs-um-cov}"
export BUILD_DIR

COVERAGE=1 KSRC="$KSRC" "$ROOT/scripts/kunit.sh" "$@"

PROFRAW="$BUILD_DIR/kunit.profraw"
[[ -s "$PROFRAW" ]] || { echo "kunit-cov.sh: no profile at $PROFRAW" >&2; exit 1; }
"$LLVM_PROFDATA" merge -sparse "$PROFRAW" -o "$BUILD_DIR/kunit.profdata"

DIR="$KSRC/fs/s3fs_ebpf_kunit"
srcs=()
for f in "$DIR"/*.c; do
	case "$f" in *_kunit.c | *.mod.c) ;; *) srcs+=("$f") ;; esac
done
# vfs_ops.c #includes the parts in kmod/vfs/*.inc.c, so the compiler records
# their code under the real path of the repo, not under $DIR: list them too, or
# the report loses most of the kmod.
REAL_ROOT="$(cd "$ROOT" && pwd -P)"
for f in "$REAL_ROOT"/kmod/vfs/*.inc.c; do
	[[ -e "$f" ]] && srcs+=("$f")
done
mkdir -p "$(dirname "$OUT")"
# The sources are symlinks into kmod/; report them under the repo path.
# (llvm-cov report prints the paths without the leading slash once the files
# no longer share a directory.)
relpaths() { sed -e "s#${DIR#/}/#kmod/#" -e "s#${REAL_ROOT#/}/##"; }
"$LLVM_COV" export -format=lcov -instr-profile="$BUILD_DIR/kunit.profdata" \
	"$BUILD_DIR/linux" -sources "${srcs[@]}" |
	sed -e "s#^SF:$DIR/#SF:kmod/#" -e "s#^SF:$REAL_ROOT/#SF:#" >"$OUT"
"$LLVM_COV" report -instr-profile="$BUILD_DIR/kunit.profdata" \
	"$BUILD_DIR/linux" -sources "${srcs[@]}" | relpaths
echo "kunit-cov.sh: wrote $OUT"
