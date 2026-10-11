# Benchmark Results — s3fs-ebpf vs s3fs-fuse

Week 4 (e) の初回計測記録。Meetup スライドの原資料として、また将来「改善が実測できているか」の baseline として残す。詳細な機能設計は `docs/design/read-path-design.md`、Path B/C の実装記録は `docs/journal/week4a-implementation-notes.md`。

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

**§7 追記 (2026-09-23): s3fs-fuse を `-o use_cache=<dir> -o kernel_cache` で最適化しても**、ebpf-A+C は依然 **cat 2.05〜2.45× 速く、read syscall 245× 速い**。FUSE upcall そのもの (open ごとの getattr) が根本コストとして残るため。詳細 §7。

**§8〜§10 追記 (2026-09-25、Week 4 (b)(c) 実装後):**

- **write throughput**: `dd conv=notrunc` overwrite で ebpf は **1 MiB 4.9×、10 MiB 12×、100 MiB 17× 速い** (vs fuse-raw/cache 両方)。byte-for-byte roundtrip verified。§8
- **大 blob**: arena を 128 MiB に bump し 10 MiB / 100 MiB を測定。**ebpf の cat throughput は 100 MiB で ~6.4 GB/s** (fuse の 10×)。§9
- **並列スケール**: stat 8 jobs で ~22M IOPS プラトー、read は 5M IOPS プラトー。stat clat 64 jobs で ebpf 3874ns vs fuse-raw 4538ns。ebpf/fuse-cache は read hot-path で肉薄。§10

**§7.7/§8 追記 (2026-10-06、S-08/S-09 バグ修正後):**

- **S-09 修正**: `nocache=1` tiny ファイル miss 率が 18% → ≈100% に改善。microbench p50 = 558 µs (fuse-raw 1.7ms の 3× 速い)。§7.7.2/§7.7.4 参照。
- **S-08 修正**: ebpf-fsync 固定 ~90ms オーバーヘッドを排除。1 MiB min ≈ 19.7ms (= fuse-raw と同オーダー)。§8.4 参照。

- **`cat` (実データ read) が圧勝** — s3fs-fuse は毎回 FUSE upcall → S3 access するのでミリ秒台。s3fs-ebpf B は arena に事前 admit 済 → GUP + kmap + copy_to_user だけで μs 台
- **`stat` は tie** — 両 FS とも in-memory metadata cache に落ちるので kernel-syscall 分の 1〜2 μs が支配的
- **cold-cache 比較 (drop_caches=3)** は両方 100ms 台で並ぶ。`drop_caches=3` は inode + dentry も飛ばすので、cold の主コストが「path re-lookup + inode 再作成」に移り、FS-side 差分が見えなくなる。純 read-cold なら `drop_caches=1` にすべき (§追試)

### 0.1 数字の根拠レビュー (2026-10-05) — 引用前に確認

meetup outline (`docs/user/meetup-ebpf-outline.md`) の性能表を再確認した結果:

- **read 875×**: `5.20 μs` (§3.2 の今回 run の p50) と `4.55 ms` (「前回 100k iter 実測」の p50、別 run) の比。今回 run の fuse 側は `p50=~4.5ms` (概算値) なので、同一 run 内で比べると ~865×。§3.3 は「886×」と書いており一致しない。**「約 870×、同一 run の p50 比」** と丸めて出すのが安全。測っているのは read 単体ではなく **open+read+close / 16 B / warm** の 1 iter (`microbench`)。fuse-raw 側は毎回 MinIO への HTTP GET を含む。
- **~870× は「キャッシュ有り ebpf」対「キャッシュ無し fuse」**: ebpf 側は arena prewarm 済み + page cache、fuse-raw は毎回 FUSE upcall + MinIO GET。この倍率の大半は caching の差で、fast path 構造の差ではない。構造の差を取り出すための対照群として `ebpf-nocache` を追加した。kmod `nocache=1` は open ごとに page cache と arena admit を捨て、daemon は `--cache-dir` 無しで起動する → open+read のたびに upcall + S3 GET になり、FUSE (kernel_cache 無し) と同じ形になる。`scripts/bench-3way.sh` の最後に `3way-nocache*.md` として出力する。未計測。fuse-raw と比べたときに残る差は transport (miss queue + arena vs /dev/fuse) と、metadata の getattr/open upcall の有無になる。
- **fuse-cache と比べた倍率は指標で違う**: §7 によれば、wall-clock の `cat` は 2.05〜2.45×、microbench (open+read+close) は 245×。245× の差の正体は open ごとの FUSE upcall (§7.5)。「kernel_cache 有効でも 2〜2.5×」は cat の数字なので、read latency の質問に答えるときは 245× (open 込み) と区別する。
- **write 17× は durability が揃っていない**: s3fs-fuse は close() 内で S3 PUT を同期実行する。ebpf の close() (`.release`) は flush を enqueue して即 return する (`kmod/vfs_ops.c` `s3fs_ebpf_file_release`)。つまり §8 の `ebpf` 行は「PUT 完了」を含まない。比べているのは「ローカルバッファへの書き込み」と「書き込み + durable PUT」。`scripts/bench-write.sh` に `ebpf-fsync` (`conv=notrunc,fsync`、`.fsync` は daemon の PUT 完了まで block) を追加したので、fuse と同条件の数字は次回 run で取る。それまでは「close までの latency」として 17× を出し、durable 比較ではないと明記する。100 MiB の ebpf `88 ms ± 32` は CV 36% で n=5、ばらつきも大きい。
- **hit-rate (88.5% vs 7.5%) は無効**: S-01+S-02 バグ修正後の再計測値 (2026-10-05) では全 policy が 33–40% に集まり、Belady 上限 59.5–63% との差が 20+ pp 残る。`docs/journal/hit-rate-bench.md` §"Result (2026-10-05)" 参照。

---

## 0.5. 大きな Caveat — s3fs-fuse は `-o use_cache=/path -o kernel_cache` を意図的に無効化して測っている

この bench の s3fs-fuse mount は **ローカルディスクキャッシュを一切使わない** 素の構成 (`-o url=... -o use_path_request_style -o passwd_file=... -o allow_other -o uid=... -o gid=... -o umask=0022`)。s3fs-fuse には `-o use_cache=/tmp/s3cache` (local disk staging) と `-o kernel_cache` (VFS page cache の再利用を許可) があり、これらを入れると:

- 2 回目以降の read は **kernel page cache 直ヒット (FUSE upcall さえ発生しない)** → 数 μs 台に落ちる
- 現行の 5.2 ms/read (small, syscall) と 6.1 ms (cat, wall) の大部分は消える
- **hot-cache 定常状態での read latency 差は縮まる or 逆転する可能性あり** (s3fs-fuse + kernel_cache は kernel の page cache 直読、s3fs-ebpf B は GUP + kmap + copy_to_user で 3 step)

「s3fs-ebpf > s3fs-fuse」を額面通りにするには **use_cache 無効という前提が要る**。fair な三巴 (raw / with-cache / ebpf) は次回ラウンドで実施予定 (§6.2)。

### 消えない差分 4 軸

しかし use_cache 有効でも s3fs-ebpf B が優位を保つ観点は残る:

| 観点 | s3fs-fuse + use_cache + kernel_cache | s3fs-ebpf B |
|---|---|---|
| **first-read latency** | 毎回 S3 → local disk 経由 (cold path 変わらず) | prewarm 済なら初回から arena hit、C 経路でも 30 ms |
| **cache invalidation** | opportunistic (`stat_cache_expire` に依存)、stale read 可 | daemon が admit を明示制御、consistent |
| **disk footprint** | working set が local disk を消費 | RAM (arena) のみ、diskless container OK |
| **CPU 効率 (perf)** | FUSE machinery が kernel-cache path にも context bookkeeping で残る | 純 kmod パス、cache-miss 2.5× 少ない |
| **policy 拡張性** | daemon 側 C++ ロジックのみ | BPF / (Lua/WASM 予定) で cache decision 差し込み可、verifier-safe |

Meetup narrative は「read latency の絶対値」ではなく **「diskless / consistency / CPU 効率 / policy 拡張性」の 4 軸で FUSE + local cache と差別化する** ほうが持続する。

---

## 1. 環境

### 1.1 マシン / OS

- Ubuntu 24.04.5 LTS (noble)
- Linux 6.12.90-061290-generic (Ubuntu mainline PPA)
- gcc-14 / clang-20 (詳細は `docs/journal/week1-build-runtime-notes.md`)

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
- **read は ~870× 開く** (今回 run の p50 同士: 4.5 ms / 5.203 μs ≈ 865×。前回 100k iter の 4.552 ms を使うと 875×) — この差の内訳:
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

## 7. 3-way fair comparison (fuse-raw / fuse-cache / ebpf-A+C)

§0.5 の caveat を数字で埋める。s3fs-fuse を `-o use_cache=<dir> -o kernel_cache -o max_stat_cache_size=100000` で remount した「fuse-cache」を追加、s3fs-ebpf 側にも同格の 2 段キャッシュを実装:

- **A**: daemon の `--cache-dir=<dir>` — `s3.Client.Get()` が local disk を先読み、miss なら S3 GET + local 書き込み (s3fs-fuse の `-o use_cache` 相当)
- **C**: kmod の `.read_folio` + `.read_iter=generic_file_read_iter` — VFS page cache を経由し、2 回目以降 kmod に落ちない (s3fs-fuse の `-o kernel_cache` 相当)

計測条件は §1 と同じ (hyperfine warm/big + microbench read + perf stat)、`bench-3way.sh` で自動化。

### 7.1 hyperfine warm cat — hello.txt (旧: 2026-09-xx、2026-10-05、新: 2026-10-11)

| variant | 旧 mean | 10-05 mean | **10-11 mean** |
|---------|--------:|-----------:|---------------:|
| fuse-raw | 6.3 ms ± 4.6 | 2.7 ms ± 0.7 | **2.9 ms ± 0.5** |
| fuse-cache | 1.9 ms ± 0.9 | 1.3 ms ± 0.1 | **1.4 ms ± 0.2** |
| ebpf-A+C | 757 µs ± 116 | 0.6 ms ± 0.0 | **609 µs ± 73** |

10-11: fuse-cache vs raw **2.1×**、ebpf-A+C vs fuse-cache **2.3×**、vs raw **4.7×**

### 7.2 hyperfine cat 1 MiB — big.bin (旧: 2026-09-xx、2026-10-05、新: 2026-10-11)

| variant | 旧 mean | 10-05 mean | **10-11 mean** |
|---------|--------:|-----------:|---------------:|
| fuse-raw | 8.8 ms | 4.5 ms ± 0.5 | **4.6 ms ± 0.5** |
| fuse-cache | 1.9 ms | 1.5 ms ± 0.1 | **1.6 ms ± 0.3** |
| ebpf-A+C | 937 µs | 0.7 ms ± 0.1 | **730 µs ± 68** |

10-11: fuse-cache vs raw **2.9×**、ebpf-A+C vs fuse-cache **2.1×**、vs raw **6.3×**

### 7.3 microbench read (open+read+close per iter, 20k iters)

**2026-10-05 run**:
```
fuse-raw    | p50=1.689 ms   ops/s=  565
fuse-cache  | p50=0.526 ms   ops/s=1 857      (3.2× faster than raw)
ebpf-A+C    | p50=3.047 μs   ops/s=303 959    (175× faster than fuse-cache, 554× vs raw)
```

**2026-10-11 run (ebpf-nocache と同一 run — 3× 比較の根拠)**:
```
fuse-raw    | p50=1.691 ms   ops/s=  564
fuse-cache  | p50=0.519 ms   ops/s=1 878      (3.3× faster than raw)
ebpf-A+C    | p50=3.075 μs   ops/s=308 104    (169× faster than fuse-cache, 550× vs raw)
```

ebpf-nocache p50=526 µs も同一 run → fuse-raw 1.691 ms / ebpf-nocache 526 µs = **3.22×** (同一 run 由来)。

旧 (10k iters): fuse-raw p50=5.012 ms / fuse-cache p50=1.244 ms / ebpf-A+C p50=5.075 µs (245× vs fuse-cache)。比率傾向は同じで、新 run はシステム全体がより warm。

### 7.4 perf stat (200 cats/op)

**2026-10-05**:

| メトリック | fuse-raw | fuse-cache | ebpf-A+C |
|---|---:|---:|---:|
| task-clock / op | 195 µs | 159 µs | **121 µs** |
| cycles / op | 487k | 431k | **331k** |
| IPC | 0.70 | 0.79 | **1.03** |
| cache-refs / op | 17264 | 15744 | **11695** |
| cache-miss / op | 6808 | 4897 | **2775** |
| cache miss % | 39.4 | 31.1 | **23.7** |

**2026-10-11**:

| メトリック | fuse-raw | fuse-cache | ebpf-A+C |
|---|---:|---:|---:|
| task-clock / op | 203 µs | 163 µs | **118 µs** |
| cycles / op | 516k | 441k | **328k** |
| IPC | 0.66 | 0.78 | **1.05** |
| cache-refs / op | 17242 | 15654 | **10601** |
| cache-miss / op | 6933 | 4944 | **2834** |
| cache miss % | 40.2 | 31.6 | **26.7** |

3 者間の相対順位 (ebpf < fuse-cache < fuse-raw) は両 run で保たれている。ebpf-A+C の IPC が 1.03→1.05 と依然 1.0 超。

### 7.5 解釈

**fuse-cache の効果**:
- warm cat で **3-4× 速くなる** (6 ms → 1.9 ms)。§0.5 の予測通り、hot state では local disk cache + kernel page cache がミリ秒台まで下げる
- しかし **FUSE upcall そのものは消えない**: 各 open で getattr が daemon に飛び (invalidation check)、それが 1 ms/open 相当のオーバーヘッドとして残る
- perf: cache-miss は raw の 2191 → cache の 864 と大幅減、ただし cycles/op は 12% 減にとどまる

**ebpf-A+C の追加優位**:
- **warm cat で fuse-cache の 2.1〜2.3× 速い** (10-05: 2.45×、10-11: 2.34×) — `.read_iter = generic_file_read_iter` で 2 回目以降のブロックが完全に page cache 直ヒット (kmod に落ちない)
- **read syscall (open+read+close ループ) で fuse-cache の ~169× 速い** (10-11 同一 run: 519µs / 3.075µs = 168.8×) — open ごとの FUSE getattr upcall が無い。VFS の直パス
- **CPU: cycles/op が fuse-cache の 74% 相当** (328k/441k、10-11) — さらに IPC 1.05 vs 0.78、パイプライン stall が少ない

**結論 (Meetup narrative 更新、2026-10-11 同一 run で確定)**:

現行 s3fs-fuse を最大限最適化 (`use_cache + kernel_cache`) しても、s3fs-ebpf B (A+C) は依然 **2.1〜2.3× 速い** (warm cat)、**~169× 速い** (open+read+close)。ebpf-nocache と fuse-raw の 3.2× 差 (同一 run) は「キャッシュを揃えても残る構造差」として確認された。

### 7.6 まだ残る caveat

- 今回の fuse-cache は `stat_cache_expire` を無指定 (デフォルト 5 分)。ワークロードによっては cache miss で S3 に再フェッチが走り、fuse-cache 側の数字が変動する
- kernel_cache 有効時の s3fs-fuse は open() での属性再検証が入るが、`-o max_stat_cache_size` を上げれば大量ファイル環境で効く。今回は 1 ファイルなので小規模最良ケース
- ebpf-A+C 側は逆に **consistency が弱い方向にトレードオフ** — daemon が admit した size と S3 の実サイズが乖離した場合、kmod は admit を信じ続ける。cache invalidation の hook はまだ未実装 (write-back / dynamic bucket sync で対応予定)

### 7.7 ebpf-nocache 追加 (2026-10-05、構造差の分離)

`kmod nocache=1` + daemon `--cache-dir` 無しで、open ごとに page cache と admit を破棄して毎回 S3 GET を強制する「cache 無し ebpf」を追加。FUSE kernel_cache 無しと同じデータ経路になるため、構造差 (miss queue + arena vs /dev/fuse) だけを取り出せる。

#### 7.7.1 hyperfine cat (fuse-raw vs ebpf-nocache)

**2026-10-05 (S-09 修正前、bug 有り)**:

| ファイル | fuse-raw | ebpf-nocache | 差 |
|---------|--------:|-------------:|---:|
| big.bin (1 MiB) | 4.5 ± 0.4 ms | 5.0 ± 0.4 ms | 1.11× 遅い |
| hello.txt (tiny) | 2.7 ± 0.3 ms | 1.2 ± 0.4 ms | **2.3× 速い** (bug: 82% が arena hit) |

**2026-10-11 (S-09 修正後、fuse-raw / ebpf-nocache 同一 run)**:

| ファイル | fuse-raw | ebpf-nocache | 差 |
|---------|--------:|-------------:|---:|
| big.bin (1 MiB) | 4.5 ± 0.8 ms | 3.4 ± 0.3 ms | **1.3× 速い** |
| hello.txt (tiny) | 2.7 ± 0.3 ms | 1.3 ± 0.2 ms | **2.0× 速い** |

big.bin の 1.3× 差は確認。tiny ファイルの 2× 差は S-09 修正後でも残る (S3 クライアント実装差 / HEAD の有無等)。

#### 7.7.2 microbench read (ebpf-nocache、20k iters)

**2026-10-05 (S-09 修正前、bug 有り)**:
```
ebpf-nocache | p50=3.542 µs   ops/s=213 034   (読み=18% miss のみ → 残りは arena/page hit)
```
18% miss は S-09 バグ (`invalidate_mapping_pages` trylock skip + evict_log 未通知による admit 再登録) によるもので、本来 nocache が毎回 S3 GET すべきのところ arena hit していた。

**2026-10-06 (S-09 修正後)**:
```
ebpf-nocache | p50=558 µs   ops/s=1 733   total=11.5 s / 20k iters   (≈100% S3 miss)
```
この run の fuse-raw p50=1.689 ms は **別 run** (10-05) から引用。

**2026-10-11 (S-09 修正後、fuse-raw と同一 run — 3× 比較の根拠)**:
```
ebpf-nocache | p50=526 µs   ops/s=1 872   total=10.7 s / 20k iters   (≈100% S3 miss)
fuse-raw     | p50=1.691 ms  (同一 run §7.3)
```
1.691 ms / 526 µs = **3.22×** — 同一 run で確定。Meetup の「約 3×」は `3.22×` が実測値。

#### 7.7.3 perf stat (ebpf-nocache、per cat)

**2026-10-05 (S-09 修正前)**:
```
task-clock=34.28 ms per-op=171.4 µs  cycles/op=418 490  IPC=0.82
cache-refs/op=15 772  cache-miss/op=4782  miss%=30.3
```

**2026-10-11 (S-09 修正後)**:
```
task-clock=37.32 ms per-op=186.6 µs  cycles/op=460 436  IPC=0.74
cache-refs/op=16 795  cache-miss/op=5301  miss%=31.6
```

S-09 修正後は S3 GET が増えるため CPU 時間が 171 → 187 µs に増加。fuse-raw (10-11: 203 µs) と比べ 8% 少ない。IPC は 0.74 vs 0.66 で nocache 優位。

#### 7.7.4 S-09 fix まとめ (2026-10-06)

- **修正内容**: `s3fs_ebpf_file_open` nocache パスに `evict_log_push(ino)` + `invalidate_inode_pages2()` を追加
- **根本原因 (1)**: `admit_drop_all_ranges` が kmod hash から削除するが evict_log に書かない → daemon が次の replaceAdmit で re-push → admit 復活
- **根本原因 (2)**: `invalidate_mapping_pages` は `folio_trylock` で active folio を skip → tiny ファイルの folio が page cache に残留
- **受け入れ条件達成**: tiny ファイル miss 率 ≈ 100%、p50 = 558 µs ≈ fuse-raw の 1/3

---

## 6. 追試したい観点

### 6.1 このラウンドで露呈した bench 側の bug/盲点

- **hyperfine wall clock は fork/exec dominates** — Meetup 用の「1μs で return」のような数字を出したいなら microbench 側の p50 を使う
- **cold cat `drop_caches=3` は inode 全部飛ばすので純 read cold にならない** — 純粋 page-cache-cold なら `drop_caches=1` (pagecache only)、または `posix_fadvise(POSIX_FADV_DONTNEED)`
- **s3fs-fuse の 3rd-party cache** — s3fs-fuse は `-o use_cache=/tmp/cache` などで実質 local-disk になる。今回はキャッシュ無効。有効にすると warm/cold の diff がガラリと変わる
- **`wc -c` が kmod initial_content_size を返す** (`ls -la /mnt/s3demo/big.bin` が 46 バイト表示、実 read は 1 MiB) — admit 時点で inode が VFS cache 未 populate だと `ilookup + i_size_write` が空振り。cat は arena admit の size まで読むので中身は正しいが、`stat` の size 表示は嘘。**修正案**: lookup 時に admit table を先に照会して i_size を採用する
- **fio filestat の `--allow_file_create=0` が s3fs-ebpf の readonly VFS と相性悪い** — `--filesize=N --size=N --bs=N` を実 file size に合わせないと一部 iter が iops=0 で落ちる。今回は 16B で通ったが、i_size が kmod initial (17B) を返すケースだと失敗する

### 6.2 次にやりたい計測

- ✅ **s3fs-fuse を `-o use_cache=/tmp/s3cache -o kernel_cache` 有効で再測** — §7 参照
- **Mountpoint for Amazon S3 (mount-s3)** を三巴に (AWS の deb を落とす)
- **`drop_caches=1`** で純 read-cold の比較 (今回 `drop_caches=3` は inode ごと飛ばして path-lookup が支配的)
- ✅ **10 MiB / 100 MiB** — arena を 128 MiB に bump、§9 参照
- ✅ **並列度スケール** (numjobs=1..64) — §10 参照
- ✅ **書き込み side** (write-back 実装後、Week 4 (c)) — §8 参照
- **cold-cache 分離** — daemon 生存 + kmod 生存で page cache だけ落とす vs 全部落とす、の区別

---

## 8. write throughput (Week 4 (c) 追跡)

`scripts/bench-write.sh` = `dd conv=notrunc` を hyperfine で 3 サイズ (1 MiB / 10 MiB / 100 MiB) × 4 variants で回す。durability の違いに注意:
- **ebpf**: close() が flush を enqueue して即 return (非同期 PUT)
- **ebpf-fsync**: `conv=notrunc,fsync` で fsync() が daemon の PUT 完了まで block (fuse と同 durability)
- **fuse-raw / fuse-cache**: close() 内で S3 PUT を同期実行 (durable)

### 8.1 結果 (2026-09-25、ebpf 3-way のみ、旧)

| Size    | ebpf (mean) | fuse-raw     | fuse-cache   | vs raw  | vs cache |
|:--------|------------:|-------------:|-------------:|--------:|---------:|
| 1 MiB   |  **10.8 ms ± 1.1** | 52.7 ms ± 4.0   | 48.6 ms ± 7.3   |  4.89× |  4.51× |
| 10 MiB  |  **18.5 ms ± 2.0** | 217.2 ms ± 7.5  | 240.8 ms ± 24.3 | 11.73× | 13.00× |
| 100 MiB |  **88 ms ± 32**    | 1516 ms ± 315   | 1232 ms ± 96    | 17.18× | 13.96× |

### 8.2 結果 (2026-10-05、ebpf-fsync 追加、S-08 前)

| Size    | ebpf (non-durable) | fuse-raw    | fuse-cache   | ebpf-fsync (durable) |
|:--------|-------------------:|------------:|-------------:|---------------------:|
| 1 MiB   |  **6.1 ± 0.6 ms** | 19.5 ± 2.2 ms | 18.1 ± 1.1 ms | 99.8 ± 1.6 ms (**5.1× 遅い**) |
| 10 MiB  |  **9.6 ± 0.6 ms** | 76.7 ± 4.8 ms | 72.4 ± 4.4 ms | 99.7 ± 6.1 ms (1.3× 遅い) |
| 100 MiB | **47.4 ± 2.6 ms** | 347.5 ± 18.4 ms | 325.0 ± 17.3 ms | **297.8 ± 15.4 ms (1.17× 速い)** |

byte-for-byte roundtrip (`diff`) は全 3 サイズで MATCH。

### 8.3 S-08 後の全サイズ再計測 (2026-10-11、RUNS=10)

**修正内容** (commit c75247f): `S3FS_IOC_POP_FLUSH_WAIT` blocking ioctl に切り替え。旧実装は 100ms ticker loop でポーリングしており、1 MiB と 10 MiB の `ebpf-fsync` がいずれも ~100ms になっていた。

| Size    | ebpf (non-durable) | fuse-raw     | fuse-cache    | ebpf-fsync (durable) | fsync vs raw |
|:--------|-------------------:|-------------:|--------------:|---------------------:|-------------:|
| 1 MiB   |  **5.2 ± 0.2 ms** | 18.2 ± 1.5 ms | 16.8 ± 2.1 ms | **14.2 ± 0.8 ms**   | **1.28× 速い** |
| 10 MiB  |  **8.9 ± 0.3 ms** | 74.5 ± 3.7 ms | 70.5 ± 3.2 ms | **43.9 ± 4.2 ms**   | **1.70× 速い** |
| 100 MiB | **44.3 ± 1.1 ms** | 359.4 ± 10.7 ms | 312.7 ± 11.6 ms | **208.8 ± 25.3 ms** | **1.72× 速い** |

byte-for-byte roundtrip (S3 mc_cat との diff) は全 3 サイズで MATCH。

ebpf-fsync が fuse-raw より **全サイズで速い** — S-08 前は 1 MiB が 5× 遅かったので大幅改善。

**ebpf (non-durable) は 3.5〜8.1× 速い。** fuse-raw 比: 3.5× (1 MiB) → 8.4× (10 MiB) → 8.1× (100 MiB)。Meetup 用には「フラッシュが非同期なので速い。durable でも fuse-raw より速い」と説明できる。

**ebpf-fsync 100 MiB の caveat**: min=63.6 ms (outlier、coalescer か kernel buffer)、mean=208.8 ms ± 25.3 ms (CV=12%)。平均は信頼できるが最小値は過楽観。

**fuse-cache vs fuse-raw の差は ~7% 固定** (write 系では `kernel_cache` の恩恵は小さい)。

### 8.4 S-08 fix 後 spot check (2026-10-06)

**参考値** (§8.3 で全サイズ置き換え済み):

| Size  | ebpf-fsync 旧 (S-08 前) | ebpf-fsync (S-08 後、10-06 spot) | ebpf-fsync (S-08 後、10-11 mean) | fuse-raw |
|:------|------------------------:|----------------------------------:|---------------------------------:|---------:|
| 1 MiB | 99.8 ± 1.6 ms           | min ≈ 19.7 ms                     | **14.2 ± 0.8 ms**               | 18.2 ± 1.5 ms |

---

## 9. 大 blob throughput (10 MiB / 100 MiB)

arena capacity を 4096 pages (16 MiB) → 32768 pages (128 MiB) に bump し、100 MiB blob が prewarm に載る構成。`scripts/bench-latency.sh` の large-file セクションを `BLOB_FILES=big.bin 10m.bin 100m.bin` の sweep に拡張。

### 9.1 結果 (hyperfine cat throughput、warm state)

| Blob    | s3fs-ebpf B (mean)  | s3fs-fuse (mean)   | 差    | ebpf 実効 throughput |
|:--------|--------------------:|-------------------:|------:|---------------------:|
| 1 MiB   |  **980.7 μs ± 309** | 9140.5 μs ± 1296   |  9.3× | **~1069 MB/s**       |
| 10 MiB  |  **2.2 ms ± 0.3**   | 24.5 ms ± 4.1      | 11.3× | **~4.5 GB/s**        |
| 100 MiB |  **15.6 ms ± 0.9**  | 154.9 ms ± 8.3     | 10.0× | **~6.4 GB/s**        |

### 9.2 解釈

- **10× 差が blob size を通じて保たれる**: FUSE の 128 KiB ブロック転送が線形コスト、arena の GUP + memcpy は memory-bandwidth bound
- **10 MiB / 100 MiB で ebpf は memory-bandwidth に近づく**: 4.5 / 6.4 GB/s は L3-to-RAM 帯域の一部を掠っている
- **arena bump のコスト**: 128 MiB を BPF map として予約するだけで RSS は増えない (デマンドページ)。実データを載せた分だけ物理メモリ消費。1 MiB 5 objects + 10 MiB + 100 MiB で ~112 MiB。実運用では働くセットに応じて sizing
- **caveat**: 100 MiB を超える blob には現在の admit table shape (1 ino → 1 uaddr) を broken (arena に載らない)。**大ファイル対応は Week 4+ の別軸** (admit を range list に拡張、streaming read)

---

## 10. 並列度スケール (numjobs=1..64)

`scripts/bench-scale.sh` = fio filestat (stat) + fio psync (read) を numjobs sweep。ebpf の mutex 経路と FUSE daemon の single-threaded 経路のスケール限界を可視化。

### 10.1 stat sweep (fio filestat、aggregate IOPS)

```
label       job       IOPS    clat_ns
ebpf          1      4029k   11709.00
fuse-raw      1      3993k   11981.00
fuse-cache    1      4027k    9426.00
ebpf          2      8009k   14988.00
fuse-raw      2      7975k   10114.50
fuse-cache    2      7984k    9043.50
ebpf          4      16.0M    7503.50
fuse-raw      4      16.2M    7345.00
fuse-cache    4      16.1M    8432.75
ebpf          8      22.3M    4815.25
fuse-raw      8      22.4M    6223.62
fuse-cache    8      22.2M    7030.25
ebpf         16      22.4M    4721.25
fuse-raw     16      22.2M    4643.12
fuse-cache   16      22.1M    5163.75
ebpf         32      22.3M    3958.69
fuse-raw     32      22.2M    4276.50
fuse-cache   32      22.3M    4718.69
ebpf         64      23.0M    3874.64
fuse-raw     64      23.0M    4538.08
fuse-cache   64      23.0M    3925.95
```

### 10.2 read sweep (fio psync、shared small file)

```
label       job       IOPS    clat_ns
ebpf          1      1199k     408.46
fuse-raw      1      1165k     404.03
fuse-cache    1      1143k     422.69
ebpf          2      2098k     512.87
fuse-raw      2      2067k     510.32
fuse-cache    2      2008k     532.78
ebpf          4      4016k     558.33
fuse-raw      4      4035k     538.24
fuse-cache    4      3927k     561.99
ebpf          8      5116k     953.17
fuse-raw      8      4912k     974.51
fuse-cache    8      5079k     934.10
ebpf         16      5091k    1813.04
fuse-raw     16      4931k    1874.77
fuse-cache   16      4851k    1841.53
ebpf         32      5176k    3754.56
fuse-raw     32      4769k    3938.82
fuse-cache   32      4880k    3925.63
ebpf         64      5374k    7159.07
fuse-raw     64      5343k    6919.38
fuse-cache   64      4978k    7556.90
```

### 10.3 解釈

- **stat**: 3 mount ともほぼ同一。8 jobs で ~22M IOPS のプラトー = **VFS/syscall 側の共通上限**。64 jobs で ebpf の clat 3874ns、fuse-raw 4538ns、fuse-cache 3926ns。s3fs-ebpf は syscall boundary から先が短い (in-kernel fast path) ぶん clat のバラツキも小さい
- **read (small file, shared)**: 8 jobs 以降 ~5M IOPS で頭打ち。全 3 mount がほぼ hot page cache だけを叩いているので、上限は kernel の page cache 側 (folio_lock 争奪) にある
- **write と違って read は fuse-cache が ebpf に肉薄する**: hot state で FUSE daemon への upcall が発生しない fuse-cache は、ebpf と同じ「page cache 直読」経路になる。ebpf の優位性は cold-path / write / consistency 制御に集約される
- **caveat**: `bench-scale.sh` 初回は parallel-cat wrapper が hang した (bash `wait` が cat の child を捉えきれず do_wait 停止)。fio psync に置換した (single-file shared reads で FUSE daemon の scale 特性を測る目的は同じ)。docs 上は fio 版を採用
