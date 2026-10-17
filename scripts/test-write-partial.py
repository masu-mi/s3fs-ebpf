#!/usr/bin/env python3
"""AC-51 scenarios for test-write-partial.sh (run as root on the mount).

usage: test-write-partial.py <mount> <bucket> <minio-container> <seed|run> <dir>
`seed` (before the mount: the daemon lists the bucket once) puts the objects
and keeps their bytes in <dir>; `run` does the scenarios.
Every scenario seeds its own object, edits it through the mount, closes, and
compares the object in MinIO and a re-read through the mount (without
drop_caches) with the locally computed bytes.
"""
import ctypes, fcntl, hashlib, os, struct, subprocess, sys, time

mount, bucket, cont, mode, tmp = sys.argv[1:6]
NAMES = ['mid.bin', 'edges.bin', 'size.bin', 'aligned.bin', 'race.bin', 'race2.bin', 'popu.bin']
M = 1 << 20
SIZE = 40 * M


def mc_put(name, data):
    subprocess.run(["docker", "exec", "-i", cont, "mc", "pipe", f"local/{bucket}/{name}"],
                   input=data, check=True, stdout=subprocess.DEVNULL)


def mc_get(name):
    return subprocess.run(["docker", "exec", cont, "mc", "cat", f"local/{bucket}/{name}"],
                          capture_output=True).stdout


def readall(fd):
    out, off = bytearray(), 0
    while True:
        b = os.pread(fd, M, off)
        if not b:
            return bytes(out)
        out += b
        off += len(b)


def settle(name, want, what):
    deadline = time.time() + 60
    while time.time() < deadline:
        if hashlib.sha256(mc_get(name)).digest() == hashlib.sha256(want).digest():
            break
        time.sleep(1)
    else:
        return fail(what, "S3 object differs")
    with open(f"{mount}/{name}", "rb") as f:
        if readall(f.fileno()) != want:
            return fail(what, "re-read through the mount differs")
    print(f"PASS {what}", flush=True)
    return True


def fail(what, why):
    print(f"FAIL {what}: {why}", flush=True)
    return False


def seed(name):
    return bytearray(open(f"{tmp}/{name}", "rb").read())


def patch(want, off, data):
    if off + len(data) > len(want):
        want += bytes(off + len(data) - len(want))
    want[off:off + len(data)] = data


if mode == "seed":
    for n in NAMES:
        d = os.urandom(SIZE)
        open(f"{tmp}/{n}", "wb").write(d)
        mc_put(n, d)
    sys.exit(0)

ok = True

# 1. one unaligned write in the middle, then read the range back at once
name = "mid.bin"; want = seed(name)
fd = os.open(f"{mount}/{name}", os.O_RDWR)
p = os.urandom(4096)
a = os.pread(fd, 8192, 20 * M)                  # cache the range first (stale-page check)
os.pwrite(fd, p, 20 * M + 100); patch(want, 20 * M + 100, p)
if os.pread(fd, 8192, 20 * M) != bytes(want[20 * M:20 * M + 8192]):
    ok = fail("mid write", "read after write is stale")
if os.pread(fd, M, 3 * M) != bytes(want[3 * M:4 * M]):
    ok = fail("mid write", "read of a remote range differs")
os.close(fd)
ok &= settle(name, bytes(want), "mid write")

# 2. start, end, and a write across a chunk boundary in one fd
name = "edges.bin"; want = seed(name)
fd = os.open(f"{mount}/{name}", os.O_RDWR)
for off in (0, SIZE - 4096, 4 * M - 2048, 16 * M - 1):
    d = os.urandom(4096 if off != 16 * M - 1 else 1)
    os.pwrite(fd, d, off); patch(want, off, d)
os.close(fd)
ok &= settle(name, bytes(want), "start / end / boundary writes")

# 3. shrink, extend, append past the old end
name = "size.bin"; want = seed(name)
fd = os.open(f"{mount}/{name}", os.O_RDWR)
os.ftruncate(fd, 30 * M + 5); del want[30 * M + 5:]
os.ftruncate(fd, 45 * M); want += bytes(45 * M - len(want))
r = os.urandom(3 * M); os.pwrite(fd, r, 44 * M); patch(want, 44 * M, r)
os.close(fd)
ok &= settle(name, bytes(want), "shrink / extend / append")

# 4. a write that covers whole chunks (nothing loaded)
name = "aligned.bin"; want = seed(name)
fd = os.open(f"{mount}/{name}", os.O_RDWR)
d = os.urandom(8 * M); os.pwrite(fd, d, 8 * M); patch(want, 8 * M, d)
os.close(fd)
ok &= settle(name, bytes(want), "chunk-aligned write")

# 5. the object is replaced while the fd is dirty after a load (pinned ETag):
#    the unchanged parts come from the new object, the loaded chunk is ours.
name = "race.bin"; old = seed(name)
fd = os.open(f"{mount}/{name}", os.O_RDWR)
p = os.urandom(4096); os.pwrite(fd, p, 5 * M + 100)
new = bytearray(os.urandom(SIZE)); mc_put(name, bytes(new))
want = bytearray(new)
mine = bytearray(old[4 * M:8 * M]); mine[M + 100:M + 100 + 4096] = p
want[4 * M:8 * M] = mine
os.close(fd)
ok &= settle(name, bytes(want), "external overwrite after a load (412 fallback)")

# 6. same, without a load: the ETag is taken at flush time
name = "race2.bin"; seed(name)
fd = os.open(f"{mount}/{name}", os.O_RDWR)
d = os.urandom(4 * M); os.pwrite(fd, d, 8 * M)
new = bytearray(os.urandom(SIZE)); mc_put(name, bytes(new))
want = bytearray(new); patch(want, 8 * M, d)
os.close(fd)
ok &= settle(name, bytes(want), "external overwrite before an aligned flush")

# 7. A chunk whose buffer is only partly filled goes through a partial flush.
#    POPULATE_PAGES for [12, 15) MiB of chunk 3 with only the first MiB filled
#    (the rest of the buffer is zeros): the chunk is not populated.  A write
#    elsewhere makes it remote (it keeps its buffer) and the flush copies the
#    object's bytes; the whole file must read back right.
#    NOTE (AC-63): this does NOT tell the fix from the bug.  signal_flush used
#    to mark such a chunk populated, but the daemon drops the ino's admit and
#    resets its unpinned chunks right after the flush, so the zeros are never
#    served; with the old kmod this scenario passes too.  It is kept as
#    coverage of the path (remote chunk with a buffer, flush, re-read).
name = "popu.bin"; want = seed(name)
ino = os.stat(f"{mount}/{name}").st_ino
dev = os.open("/dev/s3fs_ebpf", os.O_RDWR)
buf = ctypes.create_string_buffer(bytes(want[12 * M:13 * M]) + bytes(2 * M), 3 * M)
try:
    fcntl.ioctl(dev, 0x4020E30E, struct.pack("=QQQQ", ino, 12 * M, 3 * M, ctypes.addressof(buf)), False)
except OSError as e:
    ok = fail("half-filled chunk", f"POPULATE_PAGES: {e}")
os.close(dev)
fd = os.open(f"{mount}/{name}", os.O_RDWR)
d = os.urandom(4096); os.pwrite(fd, d, 100); patch(want, 100, d)
os.close(fd)
ok &= settle(name, bytes(want), "half-filled remote chunk (partial flush path)")

print("RESULT", "PASS" if ok else "FAIL")
sys.exit(0 if ok else 1)
