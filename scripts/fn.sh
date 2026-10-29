#!/usr/bin/env bash
# fn.sh — print a single C function (with line numbers) instead of reading
# a whole file.  Intended for the kmod VFS code (kmod/vfs_ops.c is split into
# parts under kmod/vfs/*.inc.c; all of them are searched by default).
#
# Usage:
#   scripts/fn.sh [-f FILE] [-c] <func> [<func>...]   print function body
#   scripts/fn.sh [-f FILE] -l [PATTERN]              list functions (name start end [file])
#
#   -f FILE  source file (default: kmod/vfs_ops.c and kmod/vfs/*.inc.c)
#   -c       also print the comment block directly above the function
#   -l       list functions with line ranges, optionally filtered by grep -E PATTERN
#
# Uses universal-ctags (end: field) when available, otherwise falls back to
# awk (kernel style: definition starts at column 0, body ends at "^}").
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
FILE=""
WITH_COMMENT=0
LIST=0

while getopts "f:cl" opt; do
	case "$opt" in
	f) FILE="$OPTARG" ;;
	c) WITH_COMMENT=1 ;;
	l) LIST=1 ;;
	*) sed -n '2,13p' "$0" >&2; exit 2 ;;
	esac
done
shift $((OPTIND - 1))

if [ -n "$FILE" ]; then
	FILES=("$FILE")
else
	FILES=("$ROOT/kmod/vfs_ops.c" "$ROOT"/kmod/vfs/*.inc.c)
fi
for f in "${FILES[@]}"; do
	[ -f "$f" ] || { echo "fn.sh: no such file: $f" >&2; exit 2; }
done

# Emit "name start end" for every function definition in $FILE.
list_funcs_in() {
	local FILE=$1
	if ctags --options=NONE --version 2>/dev/null | grep -q 'Universal Ctags'; then
		ctags --options=NONE --languages=C --kinds-C=f --fields=+ne \
			-o - "$FILE" 2>/dev/null |
			awk -F'\t' '{
				s = e = "";
				for (i = 4; i <= NF; i++) {
					if ($i ~ /^line:/) s = substr($i, 6);
					if ($i ~ /^end:/)  e = substr($i, 5);
				}
				if (s != "" && e != "") print $1, s, e;
			}'
	else
		awk '
			# candidate definition: column-0, has "name(", not a prototype
			!infn && /^[A-Za-z_].*[A-Za-z_0-9]+[ \t]*\(/ && !/;[ \t]*$/ && !/^(typedef|struct [A-Za-z_0-9]+ *\{|#)/ {
				line = $0
				sub(/\(.*/, "", line)
				n = split(line, w, /[ \t*]+/)
				name = w[n]; start = NR; cand = 1
			}
			cand && /^\{/ { infn = 1; cand = 0 }
			cand && /;[ \t]*$/ { cand = 0 }
			infn && /^\}/ { print name, start, NR; infn = 0 }
		' "$FILE"
	fi
}

# Emit "name start end file" over all of $FILES.
list_funcs() {
	local f
	for f in "${FILES[@]}"; do
		list_funcs_in "$f" | awk -v f="$f" '{ print $1, $2, $3, f }'
	done
}

if [ "$LIST" = 1 ]; then
	if [ $# -gt 0 ]; then
		list_funcs | grep -E -- "$1" || true
	else
		list_funcs
	fi
	exit 0
fi

[ $# -ge 1 ] || { sed -n '2,13p' "$0" >&2; exit 2; }

table="$(list_funcs)"
rc=0
for fn in "$@"; do
	range="$(awk -v n="$fn" '$1 == n { print $2, $3, $4; exit }' <<<"$table")"
	if [ -z "$range" ]; then
		echo "fn.sh: function not found: $fn (try: scripts/fn.sh -l '$fn')" >&2
		rc=1
		continue
	fi
	read -r start end FILE <<<"$range"
	if [ "$WITH_COMMENT" = 1 ]; then
		# Walk upward over a contiguous /* ... */ or // block ending at start-1.
		start="$(awk -v s="$start" '
			{ l[NR] = $0 }
			END {
				i = s - 1
				if (l[i] ~ /\*\/[ \t]*$/) {
					while (i > 0 && l[i] !~ /^[ \t]*\/\*/) i--
					print (i > 0 ? i : s)
				} else {
					while (i > 0 && l[i] ~ /^[ \t]*\/\//) i--
					print i + 1
				}
			}' "$FILE")"
	fi
	echo "== $fn  ${FILE#"$ROOT"/}:$start-$end"
	awk -v s="$start" -v e="$end" 'NR >= s && NR <= e { printf "%6d\t%s\n", NR, $0 } NR > e { exit }' "$FILE"
done
exit "$rc"
