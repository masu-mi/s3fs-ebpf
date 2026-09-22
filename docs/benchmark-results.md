# Benchmark Results — s3fs-ebpf vs s3fs-fuse

Week 4 (e) の初回計測記録。Meetup スライドの原資料として、また将来「改善が実測できているか」の baseline として残す。詳細な機能設計は `docs/read-path-design.md`、Path B/C の実装記録は `docs/week4a-implementation-notes.md`。

対応 script: `scripts/bench-latency.sh` (hyperfine + microbench + fio filestat の 3 段)。

---

## 0. TL;DR

**代表的な数字 (2026-09-22、hello.txt / 16 B、MinIO on localhost):**

| ケース | s3fs-ebpf B (Fast Path) | s3fs-fuse | 差 |
|---|---:|---:|---:|
| `cat` wall clock (hyperfine, mean) | **749 μs** | 5.54 ms | **7.4× 速い** |
| `stat` wall clock (hyperfine, mean) | 940 μs | 934 μs | tie |
| `read` syscall (microbench, p50) | **5.14 μs** | 4.55 ms | **886× 速い** |
| `stat` syscall (microbench, p50) | 1.376 μs | 1.395 μs | tie |
| `stat` IOPS 並列 4 (fio filestat) | **15.6M IOPS** (7.36 μs avg) | 14.9M IOPS (9.99 μs avg) | +5% |

- **`cat` (実データ read) が圧勝** — s3fs-fuse は毎回 FUSE upcall → S3 access するのでミリ秒台。s3fs-ebpf B は arena に事前 admit 済 → GUP + kmap + copy_to_user だけで μs 台
- **`stat` は tie** — 両 FS とも in-memory metadata cache に落ちるので kernel-syscall 分の 1〜2 μs が支配的
- **並列 stat scaling** — fio 4 スレッドで両方 ~15M IOPS。単発 stat が cache 化されているので lock 競合はごく軽微。s3fs-ebpf B が microbench 単発では tie でも fio では僅かに優位 (BPF fentry は per-CPU counter で lock 無し vs FUSE の userspace daemon への call fanout)

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
- **cold read** — daemon 再起動 + kernel page cache drop で「S3 まで往復する」ケース。s3fs-fuse も内部キャッシュを持つので公平な "cold" 状態を作るのが難しく、今回は steady-state に絞った
- **large object (MB オーダー)** — 現行 arena が 1 page (4 KiB) 制約。write-back (Week 4 (c)) 時に arena サイズ拡張と併せて計測

---

## 2. hyperfine (end-to-end wall clock)

fork/exec + shell 起動 + 1 syscall + ラッパー exit を含む。実用ユーザーが `stat` / `cat` 1 発叩いたときの体感 latency。

### 2.1 stat

| Command | Mean [μs] | Min [μs] | Max [μs] | Relative |
|:---|---:|---:|---:|---:|
| `s3fs-ebpf B` | 940.1 ± 118.1 | 831.2 | 3818.4 | 1.01 ± 0.18 |
| `s3fs-fuse` | 933.6 ± 115.8 | 788.6 | 3417.3 | 1.00 |

### 2.2 cat

| Command | Mean [μs] | Min [μs] | Max [μs] | Relative |
|:---|---:|---:|---:|---:|
| `s3fs-ebpf B` | **749.3 ± 155.6** | 645.6 | 8529.8 | 1.00 |
| `s3fs-fuse` | 5540.2 ± 691.1 | 4578.7 | 11038.6 | **7.39 ± 1.79** |

### 2.3 解釈

- **stat が tie** — fork/exec + shell 起動 (~500 μs) が dominant で、実際の syscall 差 (2 μs vs 3 μs 程度、§3 で分解) がノイズに埋もれる
- **cat は 7.4× 開く** — ここでは fork/exec 分は共通で ~500 μs、残りの ~250 μs (ebpf) vs ~5000 μs (fuse) が read syscall 相当。s3fs-fuse は毎回 FUSE upcall → daemon → MinIO GET → daemon → kernel → user で 5 ms 台
- Meetup の 1 分デモに使うなら **`hyperfine cat` の 7.4× が一番わかりやすい**

---

## 3. microbench (syscall-level tight loop)

Go の `time.Now` で 1 iter 単位に前後を挟む。ラッパープロセスが 1 個で、fork/exec は初回のみ。**syscall 自体の latency** を測る。

### 3.1 stat

```
[microbench] s3fs-ebpf B
  iters=100000 warmup=1000 total=155.65ms ops/s=642459
  min=1.243μs  p50=1.376μs  p95=1.482μs  p99=2.737μs  max=1.047ms

[microbench] s3fs-fuse
  iters=100000 warmup=1000 total=161.28ms ops/s=620030
  min=1.242μs  p50=1.395μs  p95=1.516μs  p99=3.605μs  max=2.393ms
```

### 3.2 read (whole small file)

```
[microbench] s3fs-ebpf B
  iters=100000 warmup=1000 total=545.56ms ops/s=183297
  min=4.734μs  p50=5.139μs  p95=5.655μs  p99=11.237μs  max=1.518ms

[microbench] s3fs-fuse
  iters=100000 warmup=1000 total=7m57.61s ops/s=209
  min=928.283μs  p50=4.552ms  p95=6.236ms  p99=7.728ms  max=347.649ms
```

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
s3fs-ebpf B    | IOPS=15.6M, BW=237MiB/s (249MB/s)(712MiB/3001msec)
s3fs-ebpf B    | clat (nsec): min=4856, max=9794, avg=7358.25, stdev=2051.50
s3fs-fuse      | IOPS=14.9M, BW=227MiB/s (238MB/s)(683MiB/3002msec)
s3fs-fuse      | clat (nsec): min=6055, max=14158, avg=9990.50, stdev=4285.41
```

### 4.2 解釈

- **両方 ~15M IOPS で有意差なし** (4 threads, 3s runtime)。単発の stat がすでに in-memory hit なので、並列度上げても kernel VFS の inode lock 競合が支配的で、FS 実装依存の差はごく僅か
- **p50 clat**: s3fs-ebpf B **7.36 μs** vs s3fs-fuse **9.99 μs** (+35% ebpf 優位) — BPF fentry のカウンタが per-CPU で lock 無し vs FUSE 内 daemon への call fanout の差と見られる
- **microbench 単発値 (1.376 μs) との乖離**: fio では 4 スレッド並列なので scheduling + rcu grace period + kernel spinlock 争奪が加わる。単スレッドから 4 スレッドで **5×** に上がるのは scaling 面での改善余地を示唆

---

## 5. 追試したい観点

### 5.1 このラウンドの盲点

- **hyperfine wall clock は fork/exec dominates** — Meetup 用の「1μs で return」のような数字を出したいなら microbench 側の p50 を使う
- **kernel page cache の効き目** — 2 回目以降の `cat` は VFS の generic page cache に乗る可能性あり (s3fs-ebpf の read が 1 回で完了するので、kernel の page cache に入る)。次回 `posix_fadvise(POSIX_FADV_DONTNEED)` で drop してから測る
- **s3fs-fuse の 3rd-party cache** — s3fs-fuse は `-o use_cache=/tmp/cache` などで実質 local-disk になる。今回はキャッシュ無効

### 5.2 次にやりたい計測

- `perf stat -e cycles,instructions,cache-misses` で CPU 消費 breakdown
- `strace -c` で syscall count / time
- Mountpoint for S3 (mount-s3) を三巴に
- 大サイズ (1 MiB, 100 MiB) の cat/write (write-back 実装後)
- 並列度スケール (numjobs=1..64) をグラフに
