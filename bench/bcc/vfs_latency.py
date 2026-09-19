#!/usr/bin/env python3
"""
vfs_latency.py — histogram of VFS entry-point latencies via BCC.

Traces vfs_read, vfs_getattr, vfs_open and prints a log2 histogram every
INTERVAL seconds. Intended to compare s3fs_ebpf vs. s3fs-fuse.

Usage:
    sudo ./vfs_latency.py [--interval 5] [--functions vfs_read,vfs_getattr]
"""
from __future__ import annotations

import argparse
import sys

try:
    from bcc import BPF  # type: ignore
except ImportError:
    sys.stderr.write("bcc python bindings are required. See docs/README env setup.\n")
    sys.exit(1)


PROG = r"""
#include <uapi/linux/ptrace.h>

BPF_HASH(start, u32, u64);
BPF_HISTOGRAM(dist);

int trace_entry(struct pt_regs *ctx) {
    u32 tid = bpf_get_current_pid_tgid();
    u64 ts = bpf_ktime_get_ns();
    start.update(&tid, &ts);
    return 0;
}

int trace_return(struct pt_regs *ctx) {
    u32 tid = bpf_get_current_pid_tgid();
    u64 *tsp = start.lookup(&tid);
    if (!tsp) return 0;
    u64 delta = bpf_ktime_get_ns() - *tsp;
    start.delete(&tid);
    dist.increment(bpf_log2l(delta / 1000)); // microseconds
    return 0;
}
"""


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--interval", type=int, default=5)
    ap.add_argument(
        "--functions",
        default="vfs_read,vfs_getattr,vfs_open",
        help="comma-separated kernel symbols to trace",
    )
    args = ap.parse_args()

    b = BPF(text=PROG)
    for fn in args.functions.split(","):
        fn = fn.strip()
        b.attach_kprobe(event=fn, fn_name="trace_entry")
        b.attach_kretprobe(event=fn, fn_name="trace_return")

    print(f"Tracing {args.functions}. Ctrl-C to stop.")
    try:
        while True:
            b.perf_buffer_poll(timeout=args.interval * 1000)
            print("\n--- latency (us) ---")
            b["dist"].print_log2_hist("us")
            b["dist"].clear()
    except KeyboardInterrupt:
        return 0


if __name__ == "__main__":
    sys.exit(main())
