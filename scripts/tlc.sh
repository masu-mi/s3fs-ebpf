#!/usr/bin/env bash
# scripts/tlc.sh — run the TLC model checker (TLA+) in a throwaway container.
#
#   scripts/tlc.sh specs/tla/FileLifecycle.tla specs/tla/FileLifecycle.base.ReadOK.cfg
#   scripts/tlc.sh --all              # every specs/tla/*.cfg, one line each
#
# The container has no network and sees the spec directory read-only; the
# tla2tools.jar is downloaded once into ~/.cache/s3fs-ebpf/tlc and must match
# the pinned sha256 (it runs as a program, so it is never used unverified).
# No Java on the host is needed. Exit: 0 = no violation, 12 = TLC found a
# violation (the trace is printed), other = error.
set -euo pipefail
TOPDIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
TLA_VERSION=${TLA_VERSION:-v1.8.0}
TLA_SHA256=${TLA_SHA256:-e20447644bfabd29d6bd0fc2c0eb1473fb301c1a0e4dfe1961938ce5a191edbf}
IMAGE=${TLC_IMAGE:-docker.io/library/eclipse-temurin:21-jre}
CACHE=${TLC_CACHE:-$HOME/.cache/s3fs-ebpf/tlc}
JAR=$CACHE/tla2tools-$TLA_VERSION.jar
# TLC_RUNTIME overrides; else the first runtime that already has the image, else the first runtime.
RUNTIME=${TLC_RUNTIME:-$(for r in podman docker nerdctl; do command -v "$r" >/dev/null && "$r" image inspect "$IMAGE" >/dev/null 2>&1 && { echo "$r"; break; }; done)}
RUNTIME=${RUNTIME:-$(for r in podman docker nerdctl; do command -v "$r" >/dev/null && { echo "$r"; break; }; done)}
[ -n "$RUNTIME" ] || { echo "tlc.sh: no container runtime (podman/docker/nerdctl)" >&2; exit 2; }

if [ ! -f "$JAR" ]; then
	mkdir -p "$CACHE"
	curl -fsSL -o "$JAR.part" "https://github.com/tlaplus/tlaplus/releases/download/$TLA_VERSION/tla2tools.jar"
	got=$(sha256sum "$JAR.part" | awk '{print $1}')
	[ "$got" = "$TLA_SHA256" ] || { rm -f "$JAR.part"; echo "tlc.sh: tla2tools.jar sha256 $got != pinned $TLA_SHA256" >&2; exit 2; }
	mv "$JAR.part" "$JAR"
fi

tlc() { # <tla> <cfg> — paths relative to the repo root
	local work states meta
	work=$(mktemp -d)
	trap 'rm -rf "$work"' RETURN
	meta=$work/states
	"$RUNTIME" run --rm --user "$(id -u):$(id -g)" --network none --read-only --tmpfs /tmp \
		-v "$TOPDIR/$(dirname "$1"):/spec:ro" -v "$JAR:/tla2tools.jar:ro" -v "$work:/work" \
		"$IMAGE" java -XX:+UseSerialGC -cp /tla2tools.jar tlc2.TLC \
		-workers 2 -noGenerateSpecTE -metadir /work/states -config "/spec/$(basename "$2")" "/spec/$(basename "$1")" 2>&1
}

if [ "${1:-}" = --all ]; then
	# Each cfg's first line says what TLC must report: "\* EXPECT: ok" or "\* EXPECT: violated".
	# A variant that is meant to fail is a record of a known counterexample; a mismatch fails the run.
	rc=0
	for cfg in "$TOPDIR"/specs/tla/*.cfg "$TOPDIR"/specs/tla/*/*.cfg; do
		cfg_rel=${cfg#"$TOPDIR"/}
		tla="$(dirname "$cfg_rel")/$(basename "$cfg" | sed 's/\..*//').tla"
		want=$(sed -n '1s/^.*EXPECT: *//p' "$cfg")
		out=$(tlc "$tla" "$cfg_rel" || true)
		if grep -q "Invariant .* is violated" <<<"$out"; then got=violated; r="violated ($(grep -o 'Invariant [A-Za-z]* is violated' <<<"$out" | head -1 | awk '{print $2}'))"
		elif grep -q "Model checking completed. No error" <<<"$out"; then got=ok; r="ok ($(grep -o '[0-9]* distinct states found' <<<"$out" | head -1))"
		else got=error; r="ERROR"; fi
		verdict=PASS; [ "$got" = "$want" ] || { verdict="FAIL (expected ${want:-?})"; rc=1; }
		printf '%-56s %-34s %s\n' "$(basename "$cfg")" "$r" "$verdict"
	done
	exit $rc
fi
out=$(tlc "$1" "$2" || true)
echo "$out"
grep -q "Model checking completed. No error" <<<"$out" && exit 0
grep -q "is violated" <<<"$out" && exit 12
exit 1
