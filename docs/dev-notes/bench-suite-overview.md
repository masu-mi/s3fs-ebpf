# 性能試験 (A〜H と層ごとのレイテンシ) の概要と、測っている側面

`docs/journal/benchmark-results.md` §18.48 / §18.49 の試験が、**何を測り、何を測らないか**の一覧 (2026-11-03、issue #35)。結果の値はそちらを見る。実行・可視化・検証・起票の手順は `.claude/skills/perf-bench-verify/SKILL.md`。

## 比べる 6 構成

| 名前 | 内容 | 役割 |
|---|---|---|
| fuse-raw | s3fs-fuse、`use_cache` なし、`kernel_cache` なし | キャッシュなしの基準 |
| fuse-diskcache | s3fs-fuse、`use_cache` あり、`kernel_cache` なし | open のたびに page cache を捨て、disk cache から返す。独自キャッシュの比較相手 |
| fuse-cache | s3fs-fuse、`use_cache` + `kernel_cache` + `max_stat_cache_size` | キャッシュをすべて使う基準 |
| ebpf-nocache | kmod `nocache=1` (open のたびに page cache と admit を捨てる)、daemon は `--cache-dir` なし | 構造だけの差 (毎回 upcall → S3) |
| ebpf-pcachedrop | kmod `pcache_drop=1` (open のたびに page cache だけ捨てる。chunk 表と policy は残す)、daemon は `--cache-dir` あり | 独自キャッシュ (chunk 表と policy) だけの効果 |
| ebpf-pcache | 既定 (page cache あり)、daemon は `--cache-dir` あり | 全部入り |

組の読み方 (比は「s3fs-fuse の時間 ÷ s3fs-ebpf の時間」、1 超が s3fs-ebpf の勝ち):

- **fuse-raw 対 ebpf-nocache**: キャッシュを除いた構造の差 (FUSE の往復 4 回 対 共有メモリの upcall、metadata を kmod が持つか)。
- **fuse-diskcache 対 ebpf-pcachedrop**: open のたびに page cache を捨てたうえでの、独自キャッシュ (chunk 表と policy) の差。
- **fuse-cache 対 ebpf-pcache**: 実運用に近い全体の差。

`scripts/lib/bench.sh` の `bench_up <backend> <log> [flags]` が 6 構成 (と、層ごとのレイテンシ用の `ebpf-nocache-diskcache`) を起動する。共通の daemon の flags は `--arena-metadata-only --policy-bpf=builtin:policy_observe_ops --metrics-addr=:9600`、policy の評価は neutral (何も evict しない)。

## A〜H

| 記号 | 測るもの (側面) | 負荷と手順 | 数 | スクリプト |
|---|---|---|---|---|
| **A** | **1 回の操作の固定費**。open + read + close を 18 B のファイルで 1 回あたり何 µs か。FUSE の往復、upcall、page cache hit のどれで折り返すかが p50 に出る。p99 は尾 | `dev microbench --op=read` (warmup 1000 + 5000 回) | 5000 iters | `bench-pcache-remeasure.sh` (STEPS=A) |
| **B** | **書き込みの durable 性**。新規ファイルを `dd conv=fsync` して返るまで。write 経路、flush、PUT (大きいと multipart)、fsync の意味 (返った時点で S3 にあるか) | 1 / 10 / 100 MiB。全 object を sha256 で verify。fsync 直後の HEAD で `Content-Length` を見て「早く返った」回数と、遅れた PUT で空になった回数も数える | n=10 | 同 (STEPS=B) |
| **C** | **既存オブジェクトの途中の書き換え**。1 / 10 / 100 MiB の真ん中へ 4 KiB + fsync。AC-50 (変更前の全体を GET して全体を PUT) と AC-51 (`--partial-modify`: 触れる chunk だけ読み、残りは `UploadPartCopy`)。`partial_modify_min` (16 MiB) 未満は全体 | 毎回別の object (読みは cold)。fsync 後の ETag の変化で完了を見る | n=10 | 同 (STEPS=C) |
| **D** | **cold read のスループット**。空のキャッシュから 1 回目の読み。S3 GET、chunk の populate、daemon の disk cache の write-behind、page cache への取り込み | 10 / 100 / 300 MiB。毎回別の object、構成ごとに daemon / mount を作り直し、`drop_caches=3`。全 object を sha256 で verify。run 間の揺れが大きい (最小・最大も残す)。`D_DROP_EACH=1` で毎回 drop | n=10 | 同 (STEPS=D) |
| **E** | **メタデータと open の積み重ね**。4 KiB のファイル 10,000 個の `ls -l` と `find | xargs cat` (cold = mount 直後、warm = もう一度)。getattr / readdir / open を何万回も払う負荷で、kmod の表を読むだけか、HEAD / GET が要るか | cat の出力全体の sha256 | n=5 | 同 (STEPS=E) |
| **F** | **並列読みの伸び**。64 スレッドが 1 つの小さいファイルを `bs=1` で読み続ける (fio psync、5 s)。read 経路の固定費 (`generic_file_read_iter` の fentry、rwlock / refcount のような共有書き込み) が律速になる最悪ケース。ほぼ page cache hit | `drop_caches=1` してから 5 s | n=5 | `bench-read-64jobs.sh` |
| **G** | **アプリケーションから見た効果**。SQLite の点検索 (OLTP) と IVF 風のベクトル検索 (mmap で page fault)。**warm** = 定常状態 (open は 1 回、page cache hit が支配的)。**cold** = open + 全 op の wall time (s3 = backend の cache を空にして起動し直す、local = page cache だけ drop)。CPU/op と minflt も取る | Zipf α=0 / 1.1、OLTP 20,000 op、ベクトル 3,000 op (cold は 500 / 30 op) | n=5 | `bench-oltp-vector-warm.sh` / `-cold.sh` |
| **H** | **policy の質が FS 全体の差に出るか**。30 ファイル・softcap 8 (capacity mode)・Zipf α=0.7・2000 access で、policy ごとの hit rate (= 1 - upcall / access)。参照は `dev trace-replay` の LRU / SIEVE / OPT。ebpf-pcachedrop と ebpf-pcache の両方で取り、page cache を捨てた構成で policy の差が出るかを見る | `bench-hitrate.sh`。policy は `observe_ops` (何も evict しない基準) と LRU / SIEVE / S3-FIFO / W-TinyLFU の `pick_victim` 版。`PCACHE_DROP=1` で pcachedrop | 5 seed | `bench-hitrate.sh` |
| (追加) | **折り返す層ごとのレイテンシ**。同じ object が page cache / kmod の chunk 表 / daemon の disk cache / S3 のどこで返るかを、18 B / 4 MiB / 64 MiB で p50 / p95 / p99。daemon の処理時間 (histogram)、upcall 数、MinIO の GET 数の増分も記録 (GET 数は行ごとに見る: 層 3 の 64 MiB は disk cache が効かず S3 に行く、backlog AC-75) | `dev microbench --op=read` を 1 層あたり約 12 秒 | 1 run / 層 × サイズ | `bench-layer-latency.sh` (可視化: `grafana-layer-latency.sh`) |

## どの側面にどの試験が効くか

| 側面 | 見る試験 | メモ |
|---|---|---|
| 固定費・往復 (FUSE 対 upcall 対 page cache) | A、層ごとのレイテンシ、F | A の ebpf-pcache (約 3 µs) は page cache hit の基準で、環境の変化に鈍い (対照に使う) |
| メタデータ (getattr / readdir / open の積み重ね) | E (`ls -l`、cat の warm) | kmod が表を持つ分、FUSE の HEAD / getattr 往復がなくなる |
| 書き込みの durable 性・正しさ | B (verify、fsync の早期 return) | 速さより**正しさ**の検査が主。早く返る / 空になるのは不具合 |
| 大きいオブジェクトの更新コスト | C (AC-50 / AC-51) | 16 MiB 未満は全体の読み書き。multipart の `CopyObjectPart` を MinIO の API trace で確かめる |
| 初回アクセス (cold) のスループット | D、G の cold、層 3 / 4 | S3 が速い (ローカル MinIO) と差が縮む。run 間の揺れに注意 |
| 並列スケール | F | page cache hit の最悪ケース。read 経路のフックの固定費が見える |
| アプリ全体への効き方 | G | warm は page cache hit で差がつかないのが通常 (D1)。cold で差が出る |
| キャッシュ policy の効果 | H | page cache があると policy の信号が届かず差が出ない。ebpf-pcachedrop で見る |

## 測っていないこと

- 実際の S3 (遠い、数十 ms) の遅延 (全部ローカル MinIO)。ネットワークが支配的な環境では A / D / E の差が縮むはず。
- 複数プロセスによる同一ファイルの同時 cold read (`where-fs-layer-can-win.md` §6)、容量を絞った実 kmod での policy の差 (P4)、mount-s3 (6 構成に含めない)。
- mmap は `pcache_drop` の対象外 (G のベクトル検索は fd を保持するので、`pcache_drop` の効果は open の回数分だけ)。
- メモリ制約下 (cgroup) の二重保持 (page cache と kmod の chunk の複本)。

## 結果を疑うときの見方

- **対照項目**: page cache だけで終わる項目 (A の ebpf-pcache、F、G の warm) は環境が変わっても動かない。S3 を伴う項目だけが 25% 以上動いたら、ホスト / MinIO を疑う。過去の事例: `mc admin trace` が MinIO のコンテナ内に残り、全 S3 項目が 25〜75% 遅くなって `test-policy-conformance` も落ちた (issue #35)。
- **構成の判定**: upcall 数と MinIO の GET 数が狙った層と合うか (行ごとに。folio hit は sample 間隔遅れなので使わない)。
- **正しさ**: sha256 (D / B / E)、B の「fsync が早く返った」回数が 0。
- 詳しい検証項目は `.claude/skills/perf-bench-verify/SKILL.md` の 4。
