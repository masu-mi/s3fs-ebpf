#!/usr/bin/env bash
# 隔離した使い捨てコンテナで最新の Chrome を動かし、ページの screenshot / ARIA snapshot / 操作結果を取る。
# usage: browse.sh [-o OUTDIR] [-a user:pass] [-s SCRIPT.js] URL [STEPS.json]
#   OUTDIR  成果物の置き場 (既定 ./browse-out)
#   -a      Basic 認証
#   -s      カスタム script (コンテナ内で page を渡して実行。SKILL.md の「カスタム script」)
# localhost / 127.0.0.1 の URL はコンテナからホストへ届くよう host.docker.internal に書き換える。
# 終了時に scratch・コンテナを必ず消す (ホストに npm / ブラウザを入れない)。
set -euo pipefail
OUT=./browse-out AUTH= SCRIPT=
while getopts o:a:s: o; do case $o in o) OUT=$OPTARG ;; a) AUTH=$OPTARG ;; s) SCRIPT=$OPTARG ;; *) exit 2 ;; esac; done
shift $((OPTIND - 1))
[ $# -ge 1 ] || { echo "usage: $0 [-o OUTDIR] [-a user:pass] [-s SCRIPT.js] URL [STEPS.json]" >&2; exit 2; }
URL=$1 STEPS=${2:-}

# ランタイム: BROWSER_RUNTIME で指定、なければ podman (rootless) → docker → nerdctl の順で最初に見つかったもの。
RT=${BROWSER_RUNTIME:-}
if [ -z "$RT" ]; then
  for c in podman docker nerdctl; do RT=$(command -v "$c" || true); [ -z "$RT" ] || break; done
fi
[ -n "$RT" ] || { echo "コンテナランタイム (podman / docker / nerdctl) がない。SKILL.md の qemu 手順を使うか、撮影を諦める" >&2; exit 3; }
echo "runtime: $RT" >&2

HERE=$(cd "$(dirname "$0")" && pwd)
W=$(mktemp -d /tmp/browse.XXXXXX)
CID="browse-$$"
# rootless podman はコンテナの root = ホストの自分。chown すると subuid の所有になって消せなくなるので、しない。
# docker / rootful では root 所有のファイルを自分に戻す (戻さないと scratch を消せない)。
CHOWN=1
[ "$(basename "$RT")" != podman ] || [ "$("$RT" info --format '{{.Host.Security.Rootless}}' 2>/dev/null)" != true ] || CHOWN=
cleanup() { "$RT" rm -f "$CID" >/dev/null 2>&1 || true; rm -rf "$W"; }
trap cleanup EXIT

cp "$HERE/browse.js" "$W/"
[ -z "$STEPS" ] || cp "$STEPS" "$W/steps.json"
[ -z "$SCRIPT" ] || cp "$SCRIPT" "$W/custom.js"

"$RT" run --rm --name "$CID" --ipc=host --security-opt no-new-privileges \
  --add-host host.docker.internal:host-gateway \
  -e URL="$URL" -e AUTH="$AUTH" -e OUT=/work/out -e HOST_UID="${CHOWN:+$(id -u)}" -e HOST_GID="${CHOWN:+$(id -g)}" \
  -e STEPS="${STEPS:+/work/steps.json}" -e SCRIPT="${SCRIPT:+/work/custom.js}" \
  -v "$W:/work" -w /work mcr.microsoft.com/playwright:latest \
  bash -c 'trap "[ -z \"$HOST_UID\" ] || chown -R $HOST_UID:$HOST_GID /work/out 2>/dev/null" EXIT
           npm i --no-save --prefix /tmp/pw playwright >/dev/null 2>&1
           /tmp/pw/node_modules/.bin/playwright install chrome >/dev/null 2>&1
           NODE_PATH=/tmp/pw/node_modules node browse.js'

mkdir -p "$OUT"
cp -r "$W/out/." "$OUT/"
echo "-> $OUT: $(ls "$OUT" | tr '\n' ' ')"
