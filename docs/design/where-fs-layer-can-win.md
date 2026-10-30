# FS 層 (s3fs-ebpf) が s3fs-fuse の use_cache + kernel_cache に勝てる局面と難しさ

2026-10-30。OLTP / ベクトル検索の計測 (`docs/journal/ann-workload-results.md`) を踏まえた整理。数値の出所は同ファイルと `docs/journal/benchmark-results.md`。出典つきの外部情報は末尾の一覧。

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
