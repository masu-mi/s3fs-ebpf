#!/usr/bin/env python3
"""Read-only OLTP / vector-search style load for scripts/bench-oltp-vector.sh.

  gen-sqlite <path> <mb>        build a SQLite DB (kv table, ~mb MiB)
  gen-vec    <path> <mb>        build a flat cluster file of random bytes
  oltp <path> <n> <alpha> <seed>   point lookups (Zipf over keys), prints JSON
  vec  <path> <n> <alpha> <seed>   IVF-like queries (nprobe whole clusters
                                   + rerank reads) over mmap, prints JSON
Latencies are per operation (oltp) / per query (vec), in microseconds.
"""
import bisect, json, mmap, os, random, sqlite3, sys, time

ROW = 480  # payload bytes per row
CLUSTER = 128 * 1024
NPROBE, RERANK, VEC = 8, 10, 512


def zipf_sampler(n, alpha, rng):
    if alpha == 0:
        return lambda: rng.randrange(n)
    cdf, t = [], 0.0
    for i in range(n):
        t += 1.0 / (i + 1) ** alpha
        cdf.append(t)
    perm = list(range(n))
    rng.shuffle(perm)
    return lambda: perm[min(bisect.bisect_left(cdf, rng.random() * t), n - 1)]


def pct(xs, p):
    xs = sorted(xs)
    return xs[min(len(xs) - 1, int(len(xs) * p))]


def summarize(lat, wall, extra):
    return json.dumps({"n": len(lat), "ops_s": round(len(lat) / wall, 1),
                       "p50_us": round(pct(lat, .5), 1), "p99_us": round(pct(lat, .99), 1),
                       "max_us": round(max(lat), 1), **extra})


def main():
    cmd, path = sys.argv[1], sys.argv[2]
    if cmd == "gen-sqlite":
        mb = int(sys.argv[3])
        if os.path.exists(path):
            os.remove(path)
        db = sqlite3.connect(path)
        db.execute("PRAGMA page_size=4096")
        db.execute("PRAGMA journal_mode=DELETE")
        db.execute("CREATE TABLE kv(k INTEGER PRIMARY KEY, v BLOB)")
        rows = mb * 1024 * 1024 // (ROW + 16)
        rng = random.Random(1)
        db.executemany("INSERT INTO kv VALUES(?,?)", ((i, rng.randbytes(ROW)) for i in range(rows)))
        db.commit()
        db.execute("VACUUM")
        db.close()
        print(rows)
    elif cmd == "gen-vec":
        mb = int(sys.argv[3])
        rng = random.Random(2)
        with open(path, "wb") as f:
            for _ in range(mb * 1024 * 1024 // CLUSTER):
                f.write(rng.randbytes(CLUSTER))
    elif cmd == "oltp":
        n, alpha, seed = int(sys.argv[3]), float(sys.argv[4]), int(sys.argv[5])
        rng = random.Random(seed)
        # immutable=1: the file never changes under us (H6), so no lock/journal files.
        db = sqlite3.connect(f"file:{path}?mode=ro&immutable=1", uri=True)
        db.execute("PRAGMA cache_size=-512")  # 512 KiB: force page reads through the FS
        rows = db.execute("SELECT max(k)+1 FROM kv").fetchone()[0]  # O(log n); count(*) would scan and warm the whole file
        pick = zipf_sampler(rows, alpha, rng)
        lat = []
        t0 = time.perf_counter()
        for _ in range(n):
            k = pick()
            s = time.perf_counter()
            db.execute("SELECT v FROM kv WHERE k=?", (k,)).fetchone()
            lat.append((time.perf_counter() - s) * 1e6)
        print(summarize(lat, time.perf_counter() - t0, {"rows": rows}))
    elif cmd == "vec":
        n, alpha, seed = int(sys.argv[3]), float(sys.argv[4]), int(sys.argv[5])
        rng = random.Random(seed)
        with open(path, "rb") as f:
            mm = mmap.mmap(f.fileno(), 0, prot=mmap.PROT_READ)
        nclu = len(mm) // CLUSTER
        pick = zipf_sampler(nclu, alpha, rng)
        lat = []
        t0 = time.perf_counter()
        for _ in range(n):
            s = time.perf_counter()
            acc = 0
            for _ in range(NPROBE):
                c = pick()
                acc += sum(mm[c * CLUSTER:(c + 1) * CLUSTER:4096])  # touch every page
            for _ in range(RERANK):
                o = rng.randrange(len(mm) // VEC) * VEC
                acc += mm[o:o + VEC][0]
            lat.append((time.perf_counter() - s) * 1e6)
        print(summarize(lat, time.perf_counter() - t0, {"clusters": nclu}))


main()
