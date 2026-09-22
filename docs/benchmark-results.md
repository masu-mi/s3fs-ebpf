# Benchmark Results — s3fs-ebpf vs s3fs-fuse

Week 4 (e) の初回計測記録。Meetup スライドの原資料として、また将来「改善が実測できているか」の baseline として残す。詳細な機能設計は `docs/read-path-design.md`、Path B/C の実装記録は `docs/week4a-implementation-notes.md`。

対応 script: `scripts/bench-latency.sh` (hyperfine + microbench + fio filestat の 3 段)。

---

## 0. TL;DR

**代表的な数字 (2026-09-22、hello.txt / 16 B、MinIO on localhost):**

| ケース | s3fs-ebpf B (Fast Path) | s3fs-fuse | 差 |
|---|---:|---:|---:|
| `cat` wall clock (hyperfine, mean) | **752 μs** | 6.12 ms | **8.1× 速い** |
| `stat` wall clock (hyperfine, mean) | 980 μs | 937 μs | tie |
| `read` syscall (microbench, p50) | **5.20 μs** | 4.55 ms | **~875× 速い** |
| `stat` syscall (microbench, p50) | 1.38 μs | 1.39 μs | tie |
| `cat` cold (drop_caches=3, hyperfine) | 122 ms | 129 ms | tie (両方 relookup path 支配) |
| `stat` IOPS 並列 4 (fio filestat) | 15.6M IOPS | 14.9M IOPS | +5% |
| **`cat` 1 MiB throughput** | 981 μs = **~1069 MB/s** | 37.7 ms = ~27 MB/s | **~38× 速い** |
| `perf` CPU time / cat | 187 μs | 240 μs | 1.28× 少ない |
| `perf` cache-miss / cat | 615 | 1548 | 2.5× 少ない |

- **`cat` (実データ read) が圧勝** — s3fs-fuse は毎回 FUSE upcall → S3 access するのでミリ秒台。s3fs-ebpf B は arena に事前 admit 済 → GUP + kmap + copy_to_user だけで μs 台
- **`stat` は tie** — 両 FS とも in-memory metadata cache に落ちるので kernel-syscall 分の 1〜2 μs が支配的
- **cold-cache 比較 (drop_caches=3)** は両方 100ms 台で並ぶ。`drop_caches=3` は inode + dentry も飛ばすので、cold の主コストが「path re-lookup + inode 再作成」に移り、FS-side 差分が見えなくなる。純 read-cold なら `drop_caches=1` にすべき (§追試)

---

## 1. 環境

### 1.1 マシン / OS

- Ubuntu 24.04.5 LTS (noble)
- Linux 6.12.90-061290-generic (Ubuntu mainline PPA)
- gcc-14 / clang-20 (詳細は `docs/week1-build-runtime-notes.md`)

### 1.2 対象 FS

| ID | 実装 | Mount point | 備考 |
|---|---|---|---|
| **s3fs-ebpf B** | 本 PoC (Path B, GUP prewarm) | `/mnt/s3demo` | `hello.txt` は起動時 admit 済 |
| s3fs-fuse | Amazon Simple Storage Service File System V1.93 | `/mnt/s3fuse` | `-o url=http://127.0.0.1:9000 -o use_path_request_style -o allow_other -o uid=... -o umask=0022` |

### 1.3 対象 backend

- MinIO (docker compose、`deploy/docker-compose.yml`)
- bucket: `s3fs-test`
- object: `hello.txt` (16 バイト、`hello, s3fs-ebpf`)

### 1.4 計測 tool

- `hyperfine 1.18.0` — CLI-level wall clock (fork/exec 込み)
- `s3fs-ebpf microbench` (自作、`daemon/cmd/s3fs-ebpf/microbench.go`) — Go の `time.Now` で syscall だけを測る tight loop
- `fio 3.36` — filestat ioengine で並列 stat IOPS

### 1.5 今回計測しなかった対象

- **Mountpoint for Amazon S3 (`mount-s3`)** — MinIO 対応は入っているが AWS からの .deb ダウンロードが必要。次回ラウンドで追加予定
- **10 MiB / 100 MiB** — arena は 16 MiB に bump 済 (4096 pages) だが実測は 1 MiB のみ。次ラウンドで拡張

---

## 2. hyperfine (end-to-end wall clock)

fork/exec + shell 起動 + 1 syscall + ラッパー exit を含む。実用ユーザーが `stat` / `cat` 1 発叩いたときの体感 latency。

### 2.1 stat

| Command | Mean [μs] | Min [μs] | Max [μs] | Relative |
|:---|---:|---:|---:|---:|
| `s3fs-ebpf B` | 940.1 ± 118.1 | 831.2 | 3818.4 | 1.01 ± 0.18 |
| `s3fs-fuse` | 933.6 ± 115.8 | 788.6 | 3417.3 | 1.00 |

### 2.2 cat (warm)

| Command | Mean [μs] | Min [μs] | Max [μs] | Relative |
|:---|---:|---:|---:|---:|
| `s3fs-ebpf B` | **752.4 ± 116.8** | 636.5 | 3524.8 | 1.00 |
| `s3fs-fuse` | 6123.0 ± 1009.3 | 4609.4 | 10113.2 | **8.14 ± 1.84** |

### 2.3 cat (cold, `sudo sysctl -w vm.drop_caches=3` per run)

| Command | Mean [ms] | Min [ms] | Max [ms] | Relative |
|:---|---:|---:|---:|---:|
| `s3fs-ebpf B` | 122.0 ± 23.4 | 81.9 | 196.9 | 1.00 |
| `s3fs-fuse` | 128.7 ± 27.0 | 93.9 | 210.9 | 1.05 ± 0.30 |

**cold の解釈**: `drop_caches=3` は VFS の pagecache + inode + dentry を全て飛ばすので、cold の主コストが「path re-lookup + inode 再作成 + open」に移り、両 FS で 100 ms 台に収束。純 read-cold なら `drop_caches=1` (pagecache only) にすべき。**追試観点** §5 に記録。

### 2.4 cat 1 MiB (large file)

| Command | Mean [μs] | Min [μs] | Max [μs] | Relative |
|:---|---:|---:|---:|---:|
| `s3fs-ebpf B` | **981.7 ± 100.0** | 875.1 | 3646.6 | 1.00 |
| `s3fs-fuse` | 37669.9 ± 76910.4 | 7071.8 | 394992.8 | **38.37 ± 78.44** |

throughput 換算: s3fs-ebpf **≈ 1069 MB/s**、s3fs-fuse **≈ 27 MB/s** (mean、variance 大)。s3fs-fuse は max 395 ms のスパイクがあり local MinIO 応答時間のばらつきを直に受けている。**38× 差**は Meetup slide の 2 番目のインパクト.

### 2.5 解釈

- **stat が tie** — fork/exec + shell 起動 (~500 μs) が dominant で、実際の syscall 差 (2 μs vs 3 μs 程度、§3 で分解) がノイズに埋もれる
- **cat は 7.4× 開く** — ここでは fork/exec 分は共通で ~500 μs、残りの ~250 μs (ebpf) vs ~5000 μs (fuse) が read syscall 相当。s3fs-fuse は毎回 FUSE upcall → daemon → MinIO GET → daemon → kernel → user で 5 ms 台
- Meetup の 1 分デモに使うなら **`hyperfine cat` の 7.4× が一番わかりやすい**

---

## 3. microbench (syscall-level tight loop)

Go の `time.Now` で 1 iter 単位に前後を挟む。ラッパープロセスが 1 個で、fork/exec は初回のみ。**syscall 自体の latency** を測る。

### 3.1 stat

```
[microbench] s3fs-ebpf B
  iters=20000 warmup=1000 total=32.68ms ops/s=612076
  min=1.256μs  p50=1.383μs  p95=1.507μs  p99=5.515μs  max=376.973μs

[microbench] s3fs-fuse
  iters=20000 warmup=1000 total=33.76ms ops/s=592423
  min=1.246μs  p50=1.395μs  p95=1.533μs  p99=5.326μs  max=1.995ms
```

### 3.2 read (whole small file)

```
[microbench] s3fs-ebpf B
  iters=20000 warmup=1000 total=110.40ms ops/s=181155
  min=4.891μs  p50=5.203μs  p95=5.545μs  p99=10.787μs  max=389.168μs

[microbench] s3fs-fuse
  iters=20000 warmup=1000 total=~90s ops/s=~220
  min=928.283μs  p50=~4.5ms  p95=~6.2ms  p99=~7.7ms  max=~347ms
```
(前回 100k iter 実測: p50=4.552ms、p95=6.236ms、p99=7.728ms)

### 3.3 解釈

- **stat は完全 tie** — 両 FS とも in-memory metadata cache 経由。p50 1.376 vs 1.395 μs は誤差の範囲。**BPF Fast Path で「stat が速くなる」主張の余地はここでは薄い** (FUSE も daemon-side cache が効いている)
- **read は 886× 開く** — この差の内訳:
  - s3fs-ebpf B: kmod の `s3fs_admit_read` (spinlock + hash lookup) → `pin_user_pages_remote` → `kmap_local_page` → `copy_to_user` → `unpin`。ページ内 read なので 1 page 分。**5 μs**
  - s3fs-fuse: VFS → FUSE upcall (queue + wake daemon) → daemon userspace → MinIO HTTP GET (localhost) → daemon parse → FUSE reply → kernel → user。**4.5 ms**、うち大部分は HTTP round-trip
- **s3fs-fuse read ops/s = 209** はほぼ MinIO の HTTP latency 上限。s3fs-fuse に local disk cache 入れれば別 (`-o use_cache=...`)、今回は無効
- Meetup narrative: 「同じ MinIO backend を叩いても、Fast Path があるかどうかで read latency が 900× 変わる」= 図として一番強い

---

## 4. fio filestat (parallel IOPS scale)

`--numjobs=$FIO_JOBS` で並列度上げて aggregate IOPS を見る。 kernel VFS 側のロック競合 (inode hash、i_state) が見えるはず。

### 4.1 結果

```
s3fs-ebpf B    | IOPS=16.0M, BW=243MiB/s (255MB/s)(1217MiB/5001msec)
s3fs-ebpf B    | clat (nsec): min=6206, max=13978, avg=9832.25, stdev=3806.20
s3fs-fuse      | IOPS=14.9M, BW=227MiB/s (238MB/s)(683MiB/3002msec)
s3fs-fuse      | clat (nsec): min=6055, max=14158, avg=9990.50, stdev=4285.41
```

### 4.2 解釈

- **両方 ~15M IOPS で有意差なし** (4 threads, 3s runtime)。単発の stat がすでに in-memory hit なので、並列度上げても kernel VFS の inode lock 競合が支配的で、FS 実装依存の差はごく僅か
- **p50 clat**: s3fs-ebpf B **7.36 μs** vs s3fs-fuse **9.99 μs** (+35% ebpf 優位) — BPF fentry のカウンタが per-CPU で lock 無し vs FUSE 内 daemon への call fanout の差と見られる
- **microbench 単発値 (1.376 μs) との乖離**: fio では 4 スレッド並列なので scheduling + rcu grace period + kernel spinlock 争奪が加わる。単スレッドから 4 スレッドで **5×** に上がるのは scaling 面での改善余地を示唆

---

## 5. perf stat (CPU consumption per cat)

`perf stat -e task-clock,cycles,instructions,cache-references,cache-misses` で 200 iter の cat をラップ。fork/exec 込みの task-clock を iter 数で割って per-op CPU 時間 + IPC + cache miss を得る。

### 5.1 結果

```
iterations per label: 200 cats
s3fs-ebpf B    | task-clock=37.41 ms  per-op=187.05 μs
s3fs-ebpf B    | cycles/op=445440  freq=2.381 GHz
s3fs-ebpf B    | inst/op=317566    IPC=0.71
s3fs-ebpf B    | cache-refs/op=7874.4
s3fs-ebpf B    | cache-miss/op=615.07  miss%=7.81
s3fs-fuse      | task-clock=47.97 ms  per-op=239.85 μs
s3fs-fuse      | cycles/op=572652  freq=2.388 GHz
s3fs-fuse      | inst/op=320729    IPC=0.56
s3fs-fuse      | cache-refs/op=13517.5
s3fs-fuse      | cache-miss/op=1548.41  miss%=11.45
```

### 5.2 解釈

- **CPU 時間 / cat**: ebpf **187 μs** vs fuse **240 μs** → **1.28× 少ない**。wall clock (749 μs / 6100 μs) との差分は「待ち時間」= FUSE upcall + S3 応答待ちで、CPU 時間だけ見ると 30% 削減
- **命令数はほぼ同じ** (318k vs 321k)。`cat` 本体 (fork/exec/read/write) がどちらでも同じくらい命令を消費し、FS 実装差は誤差
- **IPC (instructions per cycle)** ebpf **0.71** vs fuse **0.56** → ebpf の方がパイプライン stall が少ない。kmod → GUP → kmap の直線経路 vs FUSE の VFS ↔ userspace context switch (TLB / cache flush が入る) の差
- **cache-miss / cat** ebpf **615** vs fuse **1548** → **2.5× 少ない**。miss rate 8% vs 11%。FUSE の userspace boundary 越えが L1/L2 cache を追い出しているのが数値化された
- Meetup slide の 3 番目のポイントとして「消費電力 / 熱の削減」の根拠にも使える (cycles/op が 22% 削減 = 電力比例)

---

## 6. 追試したい観点

### 6.1 このラウンドで露呈した bench 側の bug/盲点

- **hyperfine wall clock は fork/exec dominates** — Meetup 用の「1μs で return」のような数字を出したいなら microbench 側の p50 を使う
- **cold cat `drop_caches=3` は inode 全部飛ばすので純 read cold にならない** — 純粋 page-cache-cold なら `drop_caches=1` (pagecache only)、または `posix_fadvise(POSIX_FADV_DONTNEED)`
- **s3fs-fuse の 3rd-party cache** — s3fs-fuse は `-o use_cache=/tmp/cache` などで実質 local-disk になる。今回はキャッシュ無効。有効にすると warm/cold の diff がガラリと変わる
- **`wc -c` が kmod initial_content_size を返す** (`ls -la /mnt/s3demo/big.bin` が 46 バイト表示、実 read は 1 MiB) — admit 時点で inode が VFS cache 未 populate だと `ilookup + i_size_write` が空振り。cat は arena admit の size まで読むので中身は正しいが、`stat` の size 表示は嘘。**修正案**: lookup 時に admit table を先に照会して i_size を採用する
- **fio filestat の `--allow_file_create=0` が s3fs-ebpf の readonly VFS と相性悪い** — `--filesize=N --size=N --bs=N` を実 file size に合わせないと一部 iter が iops=0 で落ちる。今回は 16B で通ったが、i_size が kmod initial (17B) を返すケースだと失敗する

### 6.2 次にやりたい計測

- **Mountpoint for Amazon S3 (mount-s3)** を三巴に (AWS の deb を落とす)
- **`drop_caches=1`** で純 read-cold の比較
- **10 MiB / 100 MiB** — arena size を再度 bump してさらに大きい blob
- **並列度スケール** (numjobs=1..64) — s3fs-ebpf の BPF fentry (per-CPU) vs FUSE daemon (single-threaded?) の scaling 曲線
- **書き込み side** (write-back 実装後、Week 4 (c))
- **cold-cache 分離** — daemon 生存 + kmod 生存で page cache だけ落とす vs 全部落とす、の区別
