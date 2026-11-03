#!/usr/bin/env python3
"""Helpers for scripts/bench-layer-latency.sh (run with python3 -I).

  snap   <metrics-url> <minio-metrics-url>   one-line JSON: daemon histogram, upcalls, folio hits, MinIO GetObject count
  delta  <snap-before> <snap-after> <micro-output-file> <layer> <size> <iters>   one CSV row
"""
import json, re, sys, urllib.request

def get(url):
    try:
        return urllib.request.urlopen(url, timeout=5).read().decode()
    except Exception:
        return ""

def snap(murl, minio):
    t = get(murl)
    h = {}
    for m in re.finditer(r's3fs_ebpf_read_miss_latency_seconds_bucket\{le="([^"]+)"\}\s+(\S+)', t):
        h[m.group(1)] = float(m.group(2))
    def one(name):
        m = re.search(r'^' + name + r'\s+(\S+)', t, re.M)
        return float(m.group(1)) if m else 0.0
    mt = get(minio)
    gets = sum(float(m.group(1)) for m in re.finditer(r'^minio_s3_requests_total\{[^}]*api="getobject"[^}]*\}\s+(\S+)', mt, re.M))
    return {"h": h, "sum": one("s3fs_ebpf_read_miss_latency_seconds_sum"), "count": one("s3fs_ebpf_read_miss_latency_seconds_count"),
            "upcalls": one("s3fs_ebpf_read_miss_upcalls_total"), "folio": one("s3fs_ebpf_folio_hits_total"), "gets": gets}

def quant(dh, q):
    bs = sorted(((float("inf") if k == "+Inf" else float(k)), v) for k, v in dh.items())
    total = bs[-1][1] if bs else 0
    if total <= 0:
        return None
    rank = q * total; prev_b, prev_c = 0.0, 0.0
    for b, c in bs:
        if c >= rank:
            if b == float("inf"):
                return prev_b
            if c == prev_c:
                return b
            return prev_b + (b - prev_b) * (rank - prev_c) / (c - prev_c)
        prev_b, prev_c = b, c
    return prev_b

def unit_us(s):
    m = re.match(r"([\d.]+)(ns|µs|us|ms|s)$", s)
    v = float(m.group(1)); u = m.group(2)
    return v * {"ns": 1e-3, "µs": 1, "us": 1, "ms": 1e3, "s": 1e6}[u]

def delta(a, b, micro, layer, size, iters):
    A, B = json.load(open(a)), json.load(open(b))
    txt = open(micro).read()
    m = re.search(r"min=(\S+)\s+p50=(\S+)\s+p95=(\S+)\s+p99=(\S+)\s+max=(\S+)", txt)
    mn, p50, p95, p99, mx = (unit_us(m.group(i)) for i in range(1, 6))
    ops = float(re.search(r"ops/s=(\d+)", txt).group(1))
    dh = {k: B["h"].get(k, 0) - A["h"].get(k, 0) for k in B["h"]}
    n = B["count"] - A["count"]
    h50 = quant(dh, .5); h99 = quant(dh, .99)
    mean = (B["sum"] - A["sum"]) / n * 1e6 if n > 0 else None
    f = lambda x: "" if x is None else "%.1f" % x
    print(",".join([layer, size, str(iters), "%.2f" % p50, "%.2f" % p95, "%.2f" % p99, "%.2f" % mn, "%.2f" % mx, "%.0f" % ops,
                    "%d" % (B["upcalls"] - A["upcalls"]), "%d" % (B["folio"] - A["folio"]), "%d" % (B["gets"] - A["gets"]),
                    "%d" % n, f(h50 * 1e6 if h50 is not None else None), f(h99 * 1e6 if h99 is not None else None), f(mean)]))

if __name__ == "__main__":
    if sys.argv[1] == "snap":
        print(json.dumps(snap(sys.argv[2], sys.argv[3])))
    else:
        delta(*sys.argv[2:])
