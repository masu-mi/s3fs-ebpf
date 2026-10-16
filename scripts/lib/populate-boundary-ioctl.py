#!/usr/bin/env python3
"""
populate-boundary-ioctl.py — exercise S3FS_IOC_POPULATE_PAGES boundary guards.

Opens /dev/s3fs_ebpf (needs root + kmod loaded) and fires the ioctl with four
argument sets that hit each guard condition.  The first three must return EINVAL;
the fourth (geometrically valid, unknown ino) must return something other than
EINVAL, confirming the guards do not fire.

Exit codes:
  0 — all checks PASS
  1 — at least one check FAIL
  2 — device not accessible; test caller should treat as SKIP
"""
import fcntl
import struct
import os
import sys

DEV = "/dev/s3fs_ebpf"

# _IOW(S3FS_IOC_MAGIC=0xE3, 0x0E, struct{4×u64=32B})
# _IOC(IOC_WRITE=1, type=0xE3, nr=0x0E, size=32)
# = (1<<30) | (32<<16) | (0xE3<<8) | 0x0E = 0x4020E30E
IOC_POPULATE = 0x4020E30E

FMT = "=QQQQ"  # ino, offset, size, data_ptr — four u64, native byte order
assert struct.calcsize(FMT) == 32

U64_MAX = (1 << 64) - 1
MiB = 1 << 20
GiB = 1 << 30
EINVAL = 22


def call_ioctl(fd, ino, offset, size, data_ptr):
    buf = struct.pack(FMT, ino, offset, size, data_ptr)
    try:
        fcntl.ioctl(fd, IOC_POPULATE, buf, False)
        return 0
    except OSError as e:
        return e.errno


def main():
    try:
        fd = os.open(DEV, os.O_RDWR)
    except PermissionError:
        print(f"[ioctl-boundary] SKIP: cannot open {DEV} — need root", file=sys.stderr)
        sys.exit(2)
    except OSError as e:
        print(f"[ioctl-boundary] SKIP: {DEV}: {e}", file=sys.stderr)
        sys.exit(2)

    # (label, ino, offset, size, data_ptr, expected_errno or None-means-not-EINVAL)
    cases = [
        ("size=0",         0, 0,             0,            0, EINVAL),
        ("overflow",       0, U64_MAX - 1,   16,           0, EINVAL),
        ("size>64MiB",     0, 0,             64 * MiB + 1, 0, EINVAL),
        ("5GiB+4MiB",      0, 5 * GiB,       4 * MiB,      0, None),
    ]

    results = []
    for name, ino, offset, size, dptr, expected in cases:
        got = call_ioctl(fd, ino, offset, size, dptr)
        if expected is not None:
            ok = (got == expected)
            detail = f"errno={got}, want {expected}"
        else:
            ok = (got != EINVAL)
            detail = f"errno={got}, must not be EINVAL={EINVAL}"
        results.append((name, ok, detail))

    os.close(fd)

    overall = True
    for name, ok, detail in results:
        tag = "PASS" if ok else "FAIL"
        print(f"  ioctl-boundary {name}: {tag}  ({detail})")
        if not ok:
            overall = False

    return 0 if overall else 1


if __name__ == "__main__":
    sys.exit(main())
