#!/usr/bin/env python3
"""Per-operation timing for scripts/bench-fuse-small-files.sh. Writes raw CSV (ns).

  gen <dir> <n> <size>                 n files f00000.. of `size` random bytes
  cat <dir> <n> <out.csv>              open/read/close each file once, in order
  create <dir> <n> <size> <out.csv>    new file: open(O_CREAT|O_EXCL) / write / fsync / close
"""
import os, sys, time


def main():
    cmd, d = sys.argv[1], sys.argv[2]
    n = int(sys.argv[3])
    pc = time.perf_counter_ns
    if cmd == "gen":
        size = int(sys.argv[4])
        os.makedirs(d, exist_ok=True)
        blob = os.urandom(size)
        for i in range(n):
            with open(f"{d}/f{i:05d}", "wb") as f:
                f.write(blob[:size - 8] + i.to_bytes(8, "little"))  # distinct content
    elif cmd == "cat":
        out = open(sys.argv[4], "w")
        out.write("i,open_ns,read_ns,close_ns,total_ns,bytes\n")
        for i in range(n):
            p = f"{d}/f{i:05d}"
            t0 = pc(); fd = os.open(p, os.O_RDONLY)
            t1 = pc(); b = os.read(fd, 1 << 20)
            t2 = pc(); os.close(fd)
            t3 = pc()
            out.write(f"{i},{t1-t0},{t2-t1},{t3-t2},{t3-t0},{len(b)}\n")
        out.close()
    elif cmd == "create":
        size = int(sys.argv[4])
        out = open(sys.argv[5], "w")
        out.write("i,create_ns,write_ns,fsync_ns,close_ns,total_ns\n")
        blob = os.urandom(size)
        os.makedirs(d, exist_ok=True)
        for i in range(n):
            p = f"{d}/n{i:05d}"
            t0 = pc(); fd = os.open(p, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o644)
            t1 = pc(); os.write(fd, blob)
            t2 = pc(); os.fsync(fd)
            t3 = pc(); os.close(fd)
            t4 = pc()
            out.write(f"{i},{t1-t0},{t2-t1},{t3-t2},{t4-t3},{t4-t0}\n")
        out.close()


main()
