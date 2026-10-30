# s3fs-fuse の生データ記録 (小ファイル 10,000 cat、new file + fsync、64 jobs 並列 read)

2026-10-30。他の構成 (s3fs-ebpf など) と比べるための **基準データ** の記録。生データ (ns 単位の CSV と fio の JSON) は `docs/journal/data/` にあり、同じスクリプトで同じ形式のデータを取り直せる。解釈は最小限にとどめ、数値と条件を残す。

環境 (全計測共通): KVM ゲスト 6 vCPU、Linux 6.12.0-061200-generic、s3fs V1.93 (GnuTLS)、MinIO (`pgsty/silo:latest`、同一ホスト)、fio 3.36。git は 93d912a 時点のスクリプト。詳細は各ディレクトリの `environment.txt`。

構成:
- fuse-raw: `s3fs <bucket> <mp> -o url=http://127.0.0.1:9000 -o use_path_request_style -o allow_other -o umask=0022` (キャッシュなし)
- fuse-cache: 上に `-o use_cache=<dir> -o kernel_cache -o max_stat_cache_size=100000`
- マウントオプションの実体は `data/fuse-small-files-20261030/*/mount_options.txt`

## 1. 小ファイル 10,000 個の cat (各ファイル 4 KiB)

- スクリプト: `scripts/bench-fuse-small-files.sh` (`scripts/lib/small-files-load.py`)
- データ: `data/fuse-small-files-20261030/{fuse-raw,fuse-cache}/{cat_cold,cat_warm}.csv` (列: i, open_ns, read_ns, close_ns, total_ns, bytes)、`cat_xargs_wall_ns.txt`
- 手順: 毎回新しくマウント (fuse-cache は cache dir も新規) → `drop_caches=3` → 10,000 個を順に open/read/close (cold) → 同じマウントで同じ並びをもう一度 (warm) → `drop_caches=3` → `find | sort | xargs -n1 cat` (fork + exec 込みの壁時計)。

| backend | 計測 | 合計 (s) | p50 (µs) | p99 (µs) | max (µs) |
|---|---|---|---|---|---|
| fuse-raw | cold (open+read+close) | 25.97 | 2500 | 3831 | 24044 |
| fuse-raw | warm | 22.81 | 2160 | 3500 | 20205 |
| fuse-raw | `xargs -n1 cat` (壁時計) | 31.83 | - | - | - |
| fuse-cache | cold | 27.96 | 2674 | 4011 | 201401 |
| fuse-cache | warm | 9.92 | 974 | 1437 | 5445 |
| fuse-cache | `xargs -n1 cat` (壁時計) | 20.93 | - | - | - |

- 1 回ずつの計測 (繰り返しなし)。fuse-cache の cold の max 201 ms は 1 件の外れ値。
- fuse-cache の warm でも 1 ファイルあたり約 1 ms かかる。page cache (kernel_cache) があっても、open ごとの getattr の往復が残る。fuse-raw は warm でも cold と近い (毎回 MinIO に取りに行く)。

## 2. 新規ファイルの作成 + fsync (各 4 KiB、1,000 個)

- スクリプト: 同上
- データ: `data/fuse-small-files-20261030/{fuse-raw,fuse-cache}/create_fsync.csv` (列: i, create_ns, write_ns, fsync_ns, close_ns, total_ns)
- 手順: 1 ファイルごとに `open(O_CREAT|O_EXCL|O_WRONLY)` → `write(4 KiB)` → `fsync` → `close`。

| backend | 項目 | 合計 (s) | p50 (µs) | p99 (µs) | max (µs) |
|---|---|---|---|---|---|
| fuse-raw | 1 ファイル全体 | 4.52 | 4366 | 6291 | 20133 |
| fuse-raw | うち fsync | 1.78 | 1693 | 2509 | 8676 |
| fuse-cache | 1 ファイル全体 | 4.63 | 4458 | 6446 | 21689 |
| fuse-cache | うち fsync | 1.77 | 1696 | 2533 | 7416 |

- 1 回ずつの計測。raw と cache で差がない (書き込み系では `kernel_cache` の効果が小さい。`benchmark-results.md` の write の節と同じ傾向)。
- fsync は 1 ファイルあたり約 1.7 ms で、全体の約 40%。残りは create と close。内訳の列は CSV にある。

## 3. 64 jobs 並列 read

- スクリプト: `scripts/bench-read-64jobs.sh`。`bench-ab-pcache.sh` の AB-07 の基準 (a) と同じ fio の指定 (psync、bs=1、size=1、64 threads、`hello.txt` (18 B) への読み、5 秒、time_based、`--thread`) で、JSON を残す。5 回ずつ。
- データ: `data/read-64jobs-20261030/<backend>.<rep>.json` (fio の `--output-format=json`)、`daemon-*.log` は付けていない (サイズのため。必要なら再実行)。
- 各回の前に `vm.drop_caches=1`。その前に 1 回 cat して miss の経路を通す。ebpf 側は `--cache-capacity=0 --softcap=8`、pcache は `--arena-metadata-only`、nohook は `--no-read-iter-hook --pattern-aware-admit=false` を追加。

| backend | IOPS 中央値 | min | max | clat p99 (µs) | fuse-raw 比 |
|---|---|---|---|---|---|
| fuse-raw | 6,062,094 | 5,996,744 | 6,134,435 | 0.8 | 1.000 |
| fuse-cache | 6,121,454 | 5,770,456 | 6,136,981 | 0.8 | 1.010 |
| ebpf-arena | 4,176,823 | 4,133,550 | 4,187,901 | 1.6 | 0.689 |
| ebpf-pcache (hook あり) | 5,754,988 | 5,709,514 | 5,816,015 | 0.9 | 0.949 |
| ebpf-pcache-nohook | 6,049,477 | 6,020,342 | 6,112,755 | 0.9 | 0.998 |

- fuse 側は、読みが page cache のヒットで終わるので (18 B の 1 ファイルを 64 threads で読む)、raw と cache で差がない。これは「FUSE の upcall が走らない経路」の値。
- ebpf-pcache は read_iter の hook (約 5%) の分だけ下がり、hook を外すと fuse-raw と同じ (0.998)。AB-07 の基準 (a) の取り決め (`NOHOOK_TOLERANCE=0.98`、hook のコストは別に `hook_ratio` として報告) と整合する。
- ebpf-arena は 0.69 倍 (arena モードの read 経路は pcache より遅い)。

## 再現

```
scripts/bench-fuse-small-files.sh   # OUT=/tmp/bench-fuse-small-files (N_READ, N_CREATE, SIZE を env で変更可)
scripts/bench-read-64jobs.sh        # OUT=/tmp/bench-read-64jobs (JOBS, RUNTIME, REPS を env で変更可)
```

前者は MinIO のバケットに `smallfiles/` と `fsync_new/` を作り、終了時に消す。後者は `hello.txt` が必要 (`make compose-up`)。両方とも kmod / daemon (`make all`) と `/etc/passwd-s3fs` が必要で、他のベンチとマウントポイントを共有するので同時に走らせない。
