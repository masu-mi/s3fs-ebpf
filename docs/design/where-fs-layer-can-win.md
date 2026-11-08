# FS 層 (s3fs-ebpf) が s3fs-fuse の use_cache + kernel_cache に勝てる局面と難しさ

2026-10-30 (2026-11-03 に §3.4 と §5 の末尾を追記)。OLTP / ベクトル検索の計測 (`docs/journal/ann-workload-results.md`) を踏まえた整理。数値の出所は同ファイルと `docs/journal/benchmark-results.md`。出典つきの外部情報は末尾の一覧。

## 1. 結論

- **warm の定常状態では、読み取りが page cache のヒットで終わるため、fuse-cache と差がつかない** (SQLite 点検索・IVF 風 mmap 読みとも ±4%、CPU/op も同じ)。ここに勝つ余地はほぼない。
- 勝てているのは、**FUSE の upcall が毎回発生する経路** (open ごとの getattr: microbench で fuse-cache の 170〜245 倍、warm cat で 2.1〜2.4 倍)。ただし実運用のアプリは、ファイルをまとめる・ハンドルを保持する・メタデータを自前で持つため、この経路の比率が低い。
- 残る可能性は **cold の取り込み** と **同一ホスト内の複数プロセスでの共有**。どちらも未実証 (s3 cold の OLTP 1 点で 20% 勝った以外は計測していない)。

## 2. 性能発揮が難しい理由 (計測で分かったこと)

| # | 内容 | 根拠 |
|---|---|---|
| D1 | warm は page cache ヒットで、FS の経路を通らない。勝負の土俵がない | warm 分析: OLTP 275k vs 272k ops/s、vec 76k vs 73k |
| D2 | 現実の負荷では CPU の大半がアプリ側 (SQLite・Python・全ページ走査)。FS の stime は 5〜25% 以下で、FS が速くなっても全体の数 % | rusage (utime 56〜61 ms に対し stime 7〜17 ms) |
| D3 | fuse-raw に対する 2.5〜3 倍は「page cache を持つか」の差で、ebpf 固有ではない | warm 分析 (fuse-cache は raw の 2.5〜3 倍速い) |
| D4 | local cold (page cache だけ冷えた状態) では ebpf が最悪 (OLTP 73 ms vs fuse 43 ms)。ランダム読みなのに 4 MiB の chunk ごとに fill する (1 件 6〜7 ms、9 upcall) | cold 調査 |
| D5 | 最初のアクセスは「pattern-aware full admit」でファイル全体を入れる。判定は先頭 4 KiB 読みの seq_ratio=1.00 だけ。計測では open の中で 34 ms 払い、ループが warm に見える | daemon ログ、cold 調査 |
| D6 | kmod が chunk の複本を持つ (`fd_buf_bytes` 68 MB / 64 MiB)。page cache と二重で、メモリ制約下は不利になりうる (未計測) | cold・warm 調査 |
| D7 | arena は 128 MiB 固定 (BPF compile-time)。数 GB〜数十 GB のモデル重みやデータセットは想定外 | `docs/journal/bench-env-notes.md` ほか |
| D8 | 読み取り専用・不変ファイルに限る。書き込み (fsync・rename) の意味は E-01 / AC-51 の限界のまま | `docs/design/olap-oltp-s3-evaluation-plan.md` §5 E4 |
| D9 | 計測の落とし穴が多い。cold と warm の不揃い、`drop_caches` の値、daemon の disk cache の残り、prime の有無で、結論が逆転した | 本セッションの訂正 (ann-workload-results.md) |

## 3. 残っている可能性

### 3.1 同一ホストの複数プロセスでの共有

| ユースケース | 効く理由 | 出典 |
|---|---|---|
| 推論 pod のモデル重み読み込み (GPU ノードのオートスケール) | 同じノードの複数 pod が同じ重みを重複して S3 から取る。ノードの NVMe にキャッシュする方式が標準 | [1][2][3] |
| 複数ワーカーによる同一データセットの学習・前処理 | ワーカーが同じファイルを読む | [4] |
| ベクトル検索の query ノード | 同じインデックスを複数プロセスが mmap。S3 上のベクトル DB は NVMe キャッシュ前提 | [5][6] |
| 同じホストの並列な分析プロセス (pyarrow・polars・複数の DuckDB) | 自前の永続キャッシュがなく、プロセスごとに別々に読む | 計画書 H2・H3 (推測) |
| バッチのシャード処理 (同じ参照データ・辞書・モデル) | 各タスクが S3 から取り直す | 推測 |

### 3.2 cold の取り込みが支配的な局面

| ユースケース | 内容 | 出典 |
|---|---|---|
| GPU・pod のコールドスタート | 数 GB〜数十 GB の重みを速く入れる。並列 range GET と chunk 大きさが効く | [1][3] |
| コンテナイメージの lazy pull | 必要なブロックだけ先に取る。アクセス密度が約 80% 未満なら lazy が勝つ | [7] |
| サーバレス型ベクトル DB | 初回クエリの p90 が約 1.2 秒、競合の比較では p99 約 4 秒。cache warm の API まである | [6][8][9] |
| スポット・短寿命ノードでの分析 | ノードが毎回 cold で始まる | 推測 |

### 3.3 この PoC の側にある改善の余地 (cold)

- ランダムアクセスのファイルでも最初は full admit になる。判定を seq_ratio の 1 回の観測だけに頼らない案。
- chunk の大きさ (`S3FS_CHUNK_SIZE` はビルド時定数)。simulator では小さい chunk ほど同容量でヒット率が高く、取得バイトも少ない (ann-workload-results.md)。
- chunk 1 件 6〜7 ms の内訳 (daemon の disk 読み、populate の memcpy、signal) は未調査。

### 3.4 2026-11-03 の測り直し: 独自キャッシュ (chunk 表と policy) だけの効果

`docs/journal/benchmark-results.md` §18.48 (同じ日に trace の残りで無効になった最初の run を取り直したもの)。page cache を使う構成 (ebpf-pcache) では warm の読みは page cache のヒットで終わり、独自キャッシュの差が見えない (D1)。そこで kmod に `pcache_drop` を足し、open のたびに page cache だけを捨てて chunk 表と policy は残す構成 (ebpf-pcachedrop) を作った。s3fs-fuse の `use_cache` あり・`kernel_cache` なし (fuse-diskcache) と同じ形で、**この組が独自キャッシュの効果だけを比べる**。

| 場面 | fuse-diskcache | ebpf-pcachedrop | 比 (fuse ÷ ebpf) |
|---|---:|---:|---:|
| open+read+close p50 | 537 µs | 3.8 µs | 140× |
| 小さいファイル 10,000 個の cat (warm) | 6,950 ms | 205 ms | 33.9× |
| 同 `ls -l` (warm) | 1,462 ms | 32 ms | 45.7× |
| cold read 300 MiB | 470.8 ms | 327.6 ms | 1.44× |
| OLTP warm (ops/s、α=0 / 1.1) | 96k / 122k | 244k / 290k | 2.5× / 2.4× |
| ベクトル warm (ops/s、α=0 / 1.1) | 52.3k / 56.0k | 52.0k / 56.5k | 0.99× / 1.01× |
| 64 jobs の 18 B read (IOPS) | 6.12M | 5.88M | 0.96× |

**2026-11-07 追記 (§18.54、v2 再設計の後の再計測)**: 上の表は再設計の前の値。getattr の fentry・ringbuf・arena を外した後は、open+read+close が約 5〜7% 速く (pcache 3.1 → 2.9 µs、pcachedrop 4.3 → 4.1 µs)、64 jobs の read が `ebpf-pcache ÷ fuse-raw` で 0.92〜0.94 → 0.98〜1.00、小さいファイルの warm cat が 144 → 110 ms (pcache)。**cold の cat (10,000 個) は 6.8 → 8.3 秒と +22% 遅くなった** (S3 の仕事は同じ。daemon が ringbuf の event を読まなくなった副作用で、機構は未確認)。

- 勝つのは open が繰り返される経路 (FUSE の往復がなくなる分)。ベクトル検索のように fd を保持して mmap や pread を繰り返す負荷では差がない。OLTP の差 (2.4〜2.5×) は fuse-diskcache の read が毎回 FUSE を通ることによる可能性があるが、切り分けていない。
- 全体 (fuse-cache 対 ebpf-pcache) の warm OLTP / ベクトルは ±3% で、D1 のとおり差がつかない。
- 新規ファイルの durable write は、計測中に fsync が PUT の完了前に返る不具合を見つけて直した (§18.48)。100 MiB の差は 0.97〜1.20× で、以前の「1.37× 速い」は取り下げる。
- **daemon の disk cache (`--cache-dir`) は 4 MiB を超える object には効かない** (§18.49): 大きい object の読みは range GET の経路で、disk cache を読みも書きもしない。chunk 表から落ちたあとの再読みは毎回 S3 に戻り、64 MiB は s3fs-fuse の disk cache (26.2 ms) の 0.58× (45.5 ms)。「S3 → disk cache → kmod」の段が大きい object には 1 段足りない (backlog AC-75)。
- 層ごとのレイテンシ (§18.49、18 B): page cache 2.8 µs、kmod の chunk 表 3.8 µs、daemon の disk cache 62.4 µs、S3 (ローカル MinIO) 543 µs。upcall の固定費は約 59 µs。

## 4. 競合と制約

- 重み・データセット・イメージは、ストリーミングローダ ([1])、Mountpoint の共有キャッシュ ([4])、SOCI ([7]) のように、専用の仕組みがすでにある。
- ベクトル DB は多くが NVMe キャッシュを自前で持つ。LanceDB OSS には組み込みのキャッシュがないが、Enterprise は分散 NVMe キャッシュを持つ ([5])。その場合 FS 層は重複する。
- ベクトル DB のキャッシュがホスト内の別プロセスと共有できるかは、検索では確認できなかった (各 DB のドキュメントで要確認)。
- 小さいファイル多数の読みは、実運用のアプリが避ける形 (まとめる・ハンドルを保持する) で、優位性を出しても実用に結びつかない可能性が高い。

## 5. pcache のアルゴリズム差し替えに未来はあるか

結論: **evict アルゴリズムの差し替えそのものの価値は小さい。価値が残るのは admit・prefetch の差し替えと、page cache 本体を対象にする方向。** 根拠は simulator の結果 (`docs/journal/ann-workload-results.md`、合成 trace、5 seed) と cold 調査。

- evict の差は小さい: 人気が一様なら policy 間は 1 ポイント未満。Zipf 1.1・容量 5% で s3fifo / wtinylfu が lru を 2〜6 ポイント上回る程度。HNSW ではレイアウト (bfs / insertion) の差 (GET が約 5 倍) が policy の差より桁で大きい。
- 上界との差は、アルゴリズムでは埋まらない: 容量 25% でも OPT (Belady [10]) との差は 20 ポイント以上。OPT は未来を知っているので、差の大部分は「予測できない部分」。予測を使う研究 (LRB [11]) もあるが、kmod の内部で動かす費用に見合うかは別問題。
- 構造上、信号が少ない: pcache の policy が決めるのは kmod の chunk の保持で、page cache のヒットは policy に届かない (`hit_accum` で一部を再送する設計。issue #22 の調査で効果は小さかった)。
- admit・prefetch のほうが効く: cold 調査では、全体 admit か chunk ごとの fill かの判定が、s3 cold では 20% 勝ち、local cold では 1.7 倍負けを分けた (D4・D5)。汎用の readahead は逆効果 (GET が増え、ヒット率は改善せず、精度 3〜13%) だったが、IVF のクラスタ表や HNSW のレイアウトのように **アプリの構造を知った** prefetch は未検証で、policy をエンジンの外から差し替える価値 (計画書 H4) はここにある。
- 隣接する先行研究: cache_ext [12] は eBPF で Linux の **page cache 本体** の eviction policy を差し替える枠組みで、8 つの policy を実装し、最大 70% のスループット向上と 58% の tail latency 低下を報告している (アプリごとに tune した場合。論文の掲載先のメタデータは情報源ごとに食い違うため、詳細は原典で確認すること)。本 PoC の pcache は page cache とは別に kmod に複本を持つので (D6)、policy を差し替える対象としては、page cache 本体を使う cache_ext のほうが素直。本 PoC が差別化できるのは、S3 という遅い下層を持つ点 (miss が高い: chunk 1 件 6〜7 ms) と、admit・prefetch の判断を daemon (S3 の知識を持つ側) と連携させられる点。
- **2026-11-03 追記 (§18.48): policy の差は FS 全体の hit rate に出る。** ebpf-pcachedrop (open のたびに page cache を捨てる、chunk 表と policy は残す) の hit rate は、policy なし (observe) の 27.8% に対して LRU 39.2%、SIEVE 47.5%、S3-FIFO 49.0%、W-TinyLFU 50.9% (30 ファイル・softcap 8・Zipf α=0.7・2000 access・5 seed で順位が同じ)。差は +11〜23 ポイントで、上の「evict の差は小さい」より大きい。page cache があるとき (ebpf-pcache) も順位は同じで、W-TinyLFU は 57.1% (observe 38.6% より +18.5 pp)。ただし LRU は +0.6 pp で observe と差がなく、observe 自体が seed 間で 29.9〜47.5% と大きくばらつく (原因は未切り分け、backlog AC-74)。page cache のヒットも hit に数えるので、pcachedrop と絶対値は比べられない。なお、同じ日の最初の run (MinIO に `mc admin trace` が残っていた) では「page cache があると順位が崩れる」ように見えたが、環境の異常による見かけだった。
- 注意: 合成 trace・単一ホスト・容量圧力のない実機計測のみ。実データ (計画 P3) と、容量を絞った実 kmod での確認 (P4) は未実施。

## 6. 次に確かめること (優先順)

1. **N プロセスが同じ 64〜256 MiB のファイルを cold から同時に読む** 計測 (fuse-cache・fuse-raw、入れられれば Mountpoint と比較)。完了時間、MinIO への重複 GET 数と取得バイト数、N への伸び方。
2. admit の判定 (seq_ratio のしきい値、ランダム検出後の chunk 径) を A/B で比べる cold 計測。
3. cgroup のメモリ制約下での二重保持 (D6) の影響。
4. 大きいファイル (arena 128 MiB の制約を超える) の扱い。pcache の `--cache-capacity` との関係を確認する。

## 出典

1. [AWS EKS: fast model loading](https://docs.aws.amazon.com/eks/latest/userguide/ml-inference-fast-model-loading.html)
2. [SageMaker HyperPod の model caching](https://aihub.hkuspace.hku.hk/?p=8333)
3. [Alluxio の事例 (推論のコールドスタート)](https://www.alluxio.io/customer-stories/leading-inference-cloud-accelerates-inference-cold-starts-across-regions-and-clouds-with-alluxio)
4. [Mountpoint for Amazon S3 の共有キャッシュ](https://aws.amazon.com/about-aws/whats-new/2024/11/mountpoint-amazon-s3-high-performance-shared-cache)
5. [LanceDB Enterprise features](https://docs.lancedb.com/enterprise/features)
6. [Turbopuffer architecture](https://marketplace.turbopuffer.com/docs/architecture)
7. [SOCI の論文 (arXiv 2607.06868)](https://arxiv.org/abs/2607.06868)
8. [beri.net: Turbopuffer](https://www.beri.net/tools/turbopuffer)
9. [Zilliz のブログ (競合の比較、割り引いて読む)](https://zilliz.com/blog/the-cost-of-consequence-what-no-one-tells-you-about-serverless-vector-databases)

出典のうち、ベンダーのブログ・事例 ([3][8][9]) は参考値。
10. Belady, "A study of replacement algorithms for a virtual-storage computer", IBM Systems Journal 5(2), 1966 (OPT。`cache-algorithms.md` §6 に既出)
11. [Song et al., "Learning Relaxed Belady for Content Distribution Network Caching", NSDI '20](https://www.usenix.org/conference/nsdi20/presentation/song)
12. [cache_ext: Customizing the Page Cache with eBPF (Zarkadas et al.)](https://par.nsf.gov/biblio/10647289-cache_ext-customizing-page-cache-ebpf) — 掲載先のメタデータは情報源で食い違う。[LPC 2025 の発表ページ](https://lpc.events/event/19/contributions/2165/) もある
13. [Bijlani & Ramachandran, "Extension Framework for File Systems in User space (ExtFUSE)", USENIX ATC '19](https://www.usenix.org/conference/atc19/presentation/bijlani)
14. [Vangoor, Tarasov, Zadok, "To FUSE or Not to FUSE: Performance of User-Space File Systems", FAST '17](https://usenix.org/conference/fast17/technical-sessions/presentation/vangoor)
15. [Durner, Leis, Neumann, "Exploiting Cloud Object Storage for High-Performance Analytics", PVLDB 16(11), 2023 (AnyBlob)](https://vldb.org/pvldb/vol16/p2769-durner.pdf)
16. [Chen et al., "SPANN: Highly-efficient Billion-scale Approximate Nearest Neighbor Search", NeurIPS 2021](https://arxiv.org/abs/2111.08566)
