#!/usr/bin/env bash
# grafana-layer-latency.sh — import the "S3FS-eBPF Layer latency" dashboard from a
# bench-layer-latency.sh result (issue #35). Creates the TestData datasource the bar
# charts / table use when it is missing, then POSTs the dashboard (overwrite).
#
# Usage: scripts/grafana-layer-latency.sh [layer-latency.csv]   GRAFANA_URL=http://localhost:3000
set -euo pipefail
TOPDIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
CSV=${1:-/tmp/bench-layer-latency/layer-latency.csv}
G=${GRAFANA_URL:-http://localhost:3000}
AUTH=${GRAFANA_AUTH:-admin:admin}
[ -s "$CSV" ] || { echo "no $CSV — run scripts/bench-layer-latency.sh first" >&2; exit 2; }

if ! curl -sf -u "$AUTH" "$G/api/datasources/uid/testdata-layer" >/dev/null; then
	curl -sf -u "$AUTH" -H 'Content-Type: application/json' -X POST "$G/api/datasources" \
		-d '{"name":"TestData (layer latency)","type":"grafana-testdata-datasource","uid":"testdata-layer","access":"proxy"}' >/dev/null
	echo "created datasource testdata-layer" >&2
fi
# The runs' time window, from the annotations the bench posted.
win=$(curl -s -u "$AUTH" "$G/api/annotations?tags=layer-latency&limit=200" | python3 -I -c '
import json,sys
a=json.load(sys.stdin)
print(min(x["time"] for x in a), max(x["timeEnd"] for x in a)) if a else print("0 0")')
read -r from to <<<"$win"
python3 -I "$TOPDIR/scripts/lib/grafana-layer-latency.py" "$CSV" --from-ms "$from" --to-ms "$to" > "${TMPDIR:-/tmp}/layer-latency-dashboard.json"
python3 -I -c '
import json,sys
print(json.dumps({"dashboard": json.load(open(sys.argv[1])), "overwrite": True, "message": "layer latency (issue #35)"}))' \
	"${TMPDIR:-/tmp}/layer-latency-dashboard.json" > "${TMPDIR:-/tmp}/layer-latency-post.json"
curl -sf -u "$AUTH" -H 'Content-Type: application/json' -X POST "$G/api/dashboards/db" -d @"${TMPDIR:-/tmp}/layer-latency-post.json"
echo
echo "$G/d/s3fs-ebpf-layer-latency"
