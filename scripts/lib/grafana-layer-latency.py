#!/usr/bin/env python3
"""Build the "S3FS-eBPF Layer latency" Grafana dashboard JSON from bench-layer-latency's CSV.

  python3 -I grafana-layer-latency.py <layer-latency.csv> [--from-ms N --to-ms N] > dashboard.json

The bar charts / table carry the measured numbers inline (TestData "csv_content"), the
time series read Prometheus (daemon read-miss histogram, MinIO TTFB, upcall / GET rates)
and the annotations posted by bench-layer-latency.sh mark which layer was running.
"""
import csv, json, sys, argparse

ap = argparse.ArgumentParser()
ap.add_argument("csv"); ap.add_argument("--from-ms", type=int, default=0); ap.add_argument("--to-ms", type=int, default=0)
a = ap.parse_args()
rows = list(csv.DictReader(open(a.csv)))
TD = {"type": "grafana-testdata-datasource", "uid": "testdata-layer"}
PROM = {"type": "prometheus", "uid": "prometheus"}
LABEL = {
    "ebpf-pcache": "1 page cache",
    "ebpf-pcachedrop": "2 kmod chunk table",
    "ebpf-nocache-diskcache": "3 daemon disk cache",
    "ebpf-nocache": "4 daemon → S3",
    "fuse-cache": "FUSE kernel_cache",
    "fuse-diskcache": "FUSE disk cache",
    "fuse-raw": "FUSE → S3",
}
ORDER = list(LABEL)
def csvfor(size, cols):
    out = ["layer," + ",".join(c[1] for c in cols)]
    for l in ORDER:
        for r in rows:
            if r["layer"] == l and r["size"] == size:
                out.append(LABEL[l] + "," + ",".join(r[c[0]] or "0" for c in cols))
    return "\n".join(out)
def bar(pid, title, size, x, y, smax):
    cols = [("p50_us", "p50"), ("p95_us", "p95"), ("p99_us", "p99")]
    return {"id": pid, "type": "barchart", "title": title, "datasource": TD, "gridPos": {"x": x, "y": y, "w": 8, "h": 11},
            "targets": [{"refId": "A", "datasource": TD, "scenarioId": "csv_content", "csvContent": csvfor(size, cols)}],
            "fieldConfig": {"defaults": {"unit": "µs", "decimals": 1, "custom": {"scaleDistribution": {"type": "log", "log": 10}, "fillOpacity": 85, "lineWidth": 1, "gradientMode": "none", "axisSoftMin": 1, "axisSoftMax": smax}}, "overrides": []},
            "options": {"xField": "layer", "orientation": "horizontal", "showValue": "auto", "barWidth": 0.9, "groupWidth": 0.75, "legend": {"showLegend": True, "placement": "bottom", "displayMode": "list"}, "tooltip": {"mode": "multi", "sort": "none"}, "xTickLabelRotation": 0, "xTickLabelSpacing": 0}}
def ts(pid, title, exprs, unit, x, y, w=12, h=9, desc=""):
    return {"id": pid, "type": "timeseries", "title": title, "description": desc, "datasource": PROM, "gridPos": {"x": x, "y": y, "w": w, "h": h},
            "targets": [{"refId": chr(65 + i), "datasource": PROM, "expr": e, "legendFormat": lf, "interval": "5s"} for i, (e, lf) in enumerate(exprs)],
            "fieldConfig": {"defaults": {"unit": unit, "custom": {"lineWidth": 2, "fillOpacity": 8, "showPoints": "never", "spanNulls": True}}, "overrides": []},
            "options": {"legend": {"showLegend": True, "displayMode": "list", "placement": "bottom"}, "tooltip": {"mode": "multi"}}}
cols = [("layer", "layer"), ("size", "size"), ("p50_us", "p50 µs"), ("p95_us", "p95 µs"), ("p99_us", "p99 µs"), ("upcalls", "upcalls"),
        ("s3_gets", "S3 GETs"), ("daemon_p50_us", "daemon handler p50 µs"), ("daemon_mean_us", "daemon handler mean µs")]
tcsv = ["layer,config,size,p50 µs,p95 µs,p99 µs,upcalls,S3 GETs,daemon handler p50 µs,daemon handler mean µs"]
for s in ("18B", "4MiB", "64MiB"):
    for l in ORDER:
        for r in rows:
            if r["layer"] == l and r["size"] == s:
                tcsv.append(",".join([LABEL[l], l, s] + [r[c[0]] or "-" for c in cols[2:]]))
table = {"id": 4, "type": "table", "title": "All rows (open + read whole file + close; deltas over the run)", "datasource": TD, "gridPos": {"x": 0, "y": 11, "w": 24, "h": 11},
         "targets": [{"refId": "A", "datasource": TD, "scenarioId": "csv_content", "csvContent": "\n".join(tcsv)}],
         "fieldConfig": {"defaults": {"custom": {"align": "auto"}}, "overrides": []}, "options": {"showHeader": True, "cellHeight": "sm"}}
panels = [
    {"id": 100, "type": "text", "title": "", "gridPos": {"x": 0, "y": 0, "w": 24, "h": 3}, "options": {"mode": "markdown", "content":
        "**Where a read turns around.** Same object, answered at a different layer per configuration: 1 page cache → 2 kmod chunk table (no upcall) → 3 daemon disk cache (upcall) → 4 daemon → S3 (MinIO). "
        "`ref` rows are s3fs-fuse. Value = open + read whole file + close, log scale, µs. Numbers: `docs/journal/benchmark-results.md` §18.48 (`scripts/bench-layer-latency.sh`). "
        "Time series below are Prometheus; the shaded bands are the runs (annotations tagged `layer-latency`)."}},
    bar(1, "18 B object (one 4 KiB folio)", "18B", 0, 3, 20000), bar(2, "4 MiB object (one chunk)", "4MiB", 8, 3, 30000), bar(3, "64 MiB object (16 chunks)", "64MiB", 16, 3, 200000),
]
table["gridPos"]["y"] = 14
panels.append(table)
panels += [
    ts(5, "Daemon read-miss handler latency (p50 / p99)", [
        ('histogram_quantile(0.5, sum by (le) (rate(s3fs_ebpf_read_miss_latency_seconds_bucket[15s])))', "p50"),
        ('histogram_quantile(0.99, sum by (le) (rate(s3fs_ebpf_read_miss_latency_seconds_bucket[15s])))', "p99")], "s", 0, 25,
       desc="Daemon-side service time of one upcall (event received → signalAdmit): disk-cache read or S3 GET, plus the admit ioctl. Buckets from 50 µs (issue #35)."),
    ts(6, "kmod: upcalls/s and folio hits/s", [
        ('rate(s3fs_ebpf_read_miss_upcalls_total[15s])', "upcalls/s"), ('rate(s3fs_ebpf_folio_hits_total[15s])', "folio hits/s (chunk table)")], "ops", 12, 25),
    ts(7, "Daemon read-miss handler mean latency", [
        ('rate(s3fs_ebpf_read_miss_latency_seconds_sum[15s]) / rate(s3fs_ebpf_read_miss_latency_seconds_count[15s])', "mean per upcall")], "s", 0, 34,
       desc="sum / count of the daemon's read-miss histogram: exact to the µs, unlike its buckets. MinIO's own TTFB histogram starts at 50 ms, so it cannot resolve local GETs and is not plotted."),
    ts(8, "S3 GetObject rate (MinIO)", [('sum(rate(minio_s3_requests_total{api="getobject"}[15s]))', "GET/s")], "reqps", 12, 34),
]
dash = {"uid": "s3fs-ebpf-layer-latency", "title": "S3FS-eBPF Layer latency", "tags": ["s3fs-ebpf", "latency"], "timezone": "browser", "schemaVersion": 39, "version": 1,
        "editable": True, "refresh": "", "graphTooltip": 1, "panels": panels,
        "time": {"from": "now-1h", "to": "now"},
        "annotations": {"list": [{"name": "Layer runs", "enable": True, "iconColor": "rgba(120,120,255,0.8)", "datasource": {"type": "grafana", "uid": "-- Grafana --"},
                                  "target": {"type": "tags", "tags": ["layer-latency"], "limit": 100, "matchAny": False}}]}}
if a.from_ms and a.to_ms:
    import datetime
    iso = lambda ms: datetime.datetime.fromtimestamp(ms / 1000, datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%S.000Z")
    dash["time"] = {"from": iso(a.from_ms - 30000), "to": iso(a.to_ms + 30000)}
print(json.dumps(dash, ensure_ascii=False, indent=1))
