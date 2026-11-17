#!/usr/bin/env bash
# gen-man.sh <outdir> — man pages for `s3fs-ebpf`, gzip-compressed.
#
#   s3fs-ebpf.1                  man/s3fs-ebpf.1 (hand-written overview)
#   s3fs-ebpf-<command>.1        generated from `s3fs-ebpf <command> --help`,
#                                so the flags cannot drift from the binary
# The command list is read from the top-level help ("  name   description").
set -euo pipefail
SCRIPTS_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TOP="$(cd "$SCRIPTS_DIR/.." && pwd)"
OUT=${1:?usage: gen-man.sh <outdir>}
BIN="${BIN:-$TOP/bin/s3fs-ebpf}"
[[ -x "$BIN" ]] || { echo "ERROR: $BIN missing (make daemon)" >&2; exit 1; }
mkdir -p "$OUT"

# roff-safe: escape backslashes, protect leading . and '
roff() { sed -e 's/\\/\\e/g' -e 's/^\([.'"'"']\)/\\\&\1/' -e 's/-/\\-/g'; }

gzip -9n -c "$TOP/man/s3fs-ebpf.1" >"$OUT/s3fs-ebpf.1.gz"

mapfile -t cmds < <("$BIN" --help 2>&1 | sed -n 's/^  \([a-z][a-z0-9-]*\)  .*/\1/p')
[[ ${#cmds[@]} -gt 0 ]] || { echo "ERROR: no commands found in 's3fs-ebpf --help'" >&2; exit 1; }
for c in "${cmds[@]}"; do
    desc=$("$BIN" --help 2>&1 | sed -n "s/^  $c  *//p" | head -n1)
    {
        echo ".TH S3FS-EBPF-$(tr a-z A-Z <<<"$c") 1 \"\" \"s3fs-ebpf\" \"User Commands\""
        echo ".SH NAME"
        echo "s3fs-ebpf-$c \\- $(roff <<<"$desc")"
        echo ".SH SYNOPSIS"
        echo ".B s3fs-ebpf $c"
        echo ".RI [ options ]"
        echo ".SH OPTIONS"
        echo ".nf"
        "$BIN" "$c" --help 2>&1 | roff
        echo ".fi"
        echo ".SH SEE ALSO"
        echo ".BR s3fs-ebpf (1)"
    } | gzip -9n >"$OUT/s3fs-ebpf-$c.1.gz"
done
echo "  man: $(( ${#cmds[@]} + 1 )) pages in $OUT" >&2
