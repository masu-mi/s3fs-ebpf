# 知見の整理: S3 の前に FS 層のキャッシュを置く設計で、何が成り立たなかったか

2026-10-30。s3fs-ebpf (kmod の VFS + BPF policy + Go daemon) が、当初の主張だった「s3fs-fuse の use_cache + kernel_cache に対する優位」を、OLTP・ベクトル検索の読み取り負荷で示せなかったことの振り返り。**個別の症状と修正は他の文書にある** (下の「関連文書」)。ここでは、層ごとに **普遍的な知見** と **現状の課題 (このプロジェクト・この時点に固有で、直せる / 状況が変わりうるもの)** を分けて書く。

範囲: 単一の KVM ゲスト (6 vCPU)、MinIO (ローカル)、読み取り専用、単スレッドの Python 負荷、32 MiB 級のファイル。この範囲外の結論は書いていない。

## 0. 要約

| | 内容 |
|---|---|
| 成り立たなかったこと | warm の定常状態の優位。読みは page cache のヒットで終わり、FS 層の経路を通らないため、fuse-cache と ±4% で同じ |
| 成り立っていること | FUSE の upcall が毎回発生する経路 (open ごとの getattr 等) での大きな優位。s3 cold の取り込み (OLTP 1 点で 20%) |
| 未検証の望み | 同一ホストの複数プロセスでの cold 共有、アプリ構造を知った prefetch・admit の差し替え |
| 方法面で最も効いた教訓 | 「どこが違うはずか」を先に決め、その層で、条件をそろえた最良の競合と比べる。cold / warm / prime の定義を揃える |

---

## 1. キャッシュの利用

### 1.1 普遍

- **下に既にあるキャッシュは、足し算ではなく引き算で考える**: OS の page cache があれば、FS 層のキャッシュが足せる価値は、(a) その下の miss の経路 (取り込み方、粒度、先読み)、(b) 複数の利用者での共有、(c) 置換・admit の判断、に限られる。ヒットした読みは、層を足しても速くならない。
- **ヒット経路が同じ実装同士は、定常状態で差が出ない**。差が出るのは、ヒット経路がそもそも違うときだけ (毎回 upcall するか、しないか)。
- **キャッシュの粒度は、アクセスの粒度と合わせる**。4 MiB の粒度でランダムな 4 KiB 読みを cold から受けると、最初の数百回で全 chunk を 1 回ずつ踏み (本 PoC: 9 chunk × 6〜7 ms)、p99 が chunk の取り込み時間になる。粒度が粗いほど、ヒットは増えるが、miss の 1 件が重い。
- **admit・prefetch の判定を、小さな標本で決めない**。先頭 4 KiB の読みだけで「シーケンシャル」と判定して全体を取る、といった判定は、ランダムアクセスのファイルでも通ってしまう。
- **複本を持つと、メモリの勘定が二重になる**。キャッシュ層が下の層と別の複本を持つなら (本 PoC: kmod の chunk と page cache)、メモリ圧下で不利になる。どちらを正とするかを先に決める。
- **不変データの前提は、無効化を不要にして設計を単純にする**。ただし、前提が崩れる運用 (同名の上書き) は対象外と明記する。
- **policy の差は、人気分布と配置 (レイアウト) に支配される**。人気が一様なら policy 間の差はほぼない。グラフ型のデータ (HNSW) では、配置の差が policy の差より桁で大きい。**上界 (OPT) との差の大部分は、アルゴリズムでは埋まらない (未来の情報)**。
- **ヒット率を最適化する前に、miss 1 件のコストを見る**。S3 のように miss が重い下層では、ヒット率の 1 ポイントより、miss の取り込み方 (並列度、粒度) が効く。

### 1.2 現状の課題

| # | 内容 | 状態 |
|---|---|---|
| C1 | chunk が 4 MiB 固定 (`S3FS_CHUNK_SIZE` はビルド時定数)。ランダム読みの local cold で fuse の約 1.7 倍遅い | 再ビルドで 2〜3 値を比べる計画 (`ann-workload-measurement-plan.md` Q6) |
| C2 | pattern-aware full admit の判定が seq_ratio の 1 回の観測 (先頭読み) に頼る | 未改善。A/B 計測の候補 |
| C3 | kmod の chunk が page cache と二重 (`fd_buf_bytes` 68 MB / 64 MiB) | メモリ制約下の影響は未計測 |
| C4 | arena が 128 MiB 固定 (BPF compile-time)。GB 級のファイルは想定外 | pcache の `--cache-capacity` との関係を要確認 |
| C5 | policy が見る信号が少ない (page cache のヒットは届かない)。`hit_accum` は一部を再送するだけ | 設計上の制約。効果は小さい (issue #22) |
| C6 | 汎用の readahead は逆効果 (GET が増え、ヒット率は改善せず、精度 3〜13%) | アプリ構造を知った prefetch は未検証 |

---

## 2. 設計判断

### 2.1 普遍

- **競合は「最良構成」と比べる**。素の fuse-raw への優位 (2.5〜3 倍) は、「page cache を持つか」の差で、自分の固有の優位ではない。比べる相手は use_cache + kernel_cache、対照群は nocache、の両方を置く。
- **優位性の主張を、最も単純な競合と、定常状態で、最初に確かめる**。本 PoC は、設計と実装に大きく投資したあとで、warm が同率だと分かった。`olap-oltp-s3-evaluation-plan.md` の「ゲートで安く潰す」を、最初の設計のときから適用すべきだった。
- **ミクロな倍率は、実際の負荷での比率で重み付けする** (Amdahl)。open+read+close の microbench で 170〜245 倍でも、実運用のアプリは「まとめる・ハンドルを保持する・メタデータを自前で持つ」ので、その経路が支配しない。**rusage の utime / stime で、FS が動かせる上限を先に見積もる** (本負荷: stime は 5〜25% 以下)。
- **「通信をなくす」優位は、データが既に近くにあるなら消える**。IPC 0 回は、page cache のヒットが元からしていることと同じ。
- **FS 層は「見えない」ことが長所であり短所でもある**。無改造のクライアントが使える一方、アプリ側の構造 (どのファイルが次に読まれるか) は知らない。アプリの構造を使う優位 (push-down、prefetch) を狙うなら、情報の渡し方 (ヒント API) を設計の中に入れる。
- **「差し替えられる」ことは、「差し替える価値がある」ことではない**。policy のホットスワップは基盤として有用だが、価値の源泉は policy の内容 (何を見て何を決めるか) にある。

### 2.2 現状の課題

| # | 内容 |
|---|---|
| C7 | `docs/user/related-works.md` と `docs/design/ebpf-fuse-advantages-roadmap.md` の優位性の記述は、warm の定常状態には当てはまらない (本文書の §0 と、`where-fs-layer-can-win.md` を参照する注記を追加した) |
| C8 | 当初の「Push-down (Parquet の row group や Bloom filter をカーネルで評価する)」は未実装で、今回の計測でも検証していない |
| C9 | 書き込み (fsync、rename、部分変更) の意味は E-01 / AC-51 の限界のまま。OLTP の書き込みは対象外 |

---

## 3. 先行研究

### 3.1 普遍

| 分野 | 要点 | 出典 |
|---|---|---|
| FUSE の性能 | FUSE の劣化は負荷に依存する。スループットで知覚できない程度から −83% まで、レイテンシで増えないものから 4 倍まで。多くのデータ集約型の負荷では有意な影響がない | [Vangoor et al., FAST '17][r14] |
| FUSE の高速化 | ExtFUSE: kernel に小さな handler (BPF) を置き、キャッシュヒット時の往復を省く。RFUSE: 往復自体を ring buffer で速くする | [ExtFUSE, USENIX ATC '19][r13]、RFUSE (FAST '24) |
| eBPF でのキャッシュ policy | cache_ext: Linux の page cache 本体の eviction policy を eBPF で差し替える。複本を作らずに本体を対象にする | [cache_ext][r12] |
| 置換アルゴリズム | LRU / SIEVE / S3-FIFO / W-TinyLFU / OPT。学習で OPT を近似する LRB もある | `cache-algorithms.md` §6、[LRB, NSDI '20][r11] |
| S3 への読み | クエリエンジン側に download manager を持ち、並列 range GET で高スループットを出す (AnyBlob) | [Durner et al., PVLDB 2023][r15] |
| S3 を前提にしたキャッシュ・共有 | Mountpoint の共有キャッシュ、ノードの NVMe キャッシュ (モデル重み・ベクトル DB) | `where-fs-layer-can-win.md` §3 と出典 |
| ANN の配置 | 重心表をメモリ、ポスティングリストをディスクに置く構成 (SPANN)。配置・レイアウトが局所性を決める | [SPANN, NeurIPS '21][r16] |

- **先行研究を読むときは、「何を最良の競合としているか」を見る**。ExtFUSE も RFUSE も、素の FUSE に対する改善を示しており、use_cache + kernel_cache との比較ではない。本 PoC の優位性の主張は、この違いを見落としていた。
- **既に同じ位置を占めている仕組みを、先に調べる**。cache_ext は page cache 本体に policy を差し込み、複本を持たない。本 PoC の pcache は別の複本を持つので (C3)、同じ policy 差し替えの価値を狙うなら、cache_ext のほうが素直。

### 3.2 現状の課題

| # | 内容 |
|---|---|
| C10 | `related-works.md` は ExtFUSE を「SOSP '19」としていたが、正しくは USENIX ATC '19 (修正した)。RFUSE との比較も、use_cache + kernel_cache の条件は含まない |
| C11 | cache_ext、Mountpoint の共有キャッシュ、cache_httpfs、Alluxio などとの定量的な比較はない |
| C12 | ベクトル DB (LanceDB、Turbopuffer など) のキャッシュが、同一ホストの別プロセスと共有できるかは未確認 |

---

## 4. 各層の実装のコツと注意点

層ごとの一次情報は `lessons-learned.md`、`kmod-pcache-invariants.md`、`design-review-checklist.md` にある。ここでは **今回の調査で新しく分かったもの** を中心に書く。

### 4.1 kmod (VFS・pcache)

**普遍**
- ファイルの内容を kmod が保持するなら、その寿命を inode の寿命に結びつけない設計か、結びつけるなら計測がそれを前提にする。`drop_caches=1` は page cache だけを落とし、`=2` / `=3` は inode を evict して kmod の chunk を free する。
- 不変条件はロック順と一緒に文書にする (`kmod-pcache-invariants.md`)。

**現状の課題**
- evict 後の再取り込みは chunk ごとの upcall (C1)。inode の evict で、取り込んだ内容が消える (計測で cold が 2 通りになった原因)。

### 4.2 BPF (policy・hook)

**普遍**
- policy が見る信号は、miss と一部のヒットだけ。信号を増やす方法しだいで read 経路のコストが変わる。fentry で毎回 `on_hit` を呼ぶ方式 (`hit_accum=0`) は policy なし比で −5〜7% の IOPS、`.read_iter` で per-CPU に数えて evict の前にまとめて反映する方式 (`hit_accum=1`) は +0.6〜0.8% で policy なしと同等だった (`benchmark-results.md` §18.44)。効果とコストを A/B で測ってから採用する。
- BPF 版の policy は、Go の simulator と 1 手ずつ比べる (`design-review-checklist.md` §5)。simulator は ms 単位で回るので、アルゴリズムの比較は simulator で行い、実機は少数点の確認に使う。

**現状の課題**
- C5 (信号の少なさ)。`hit_accum` の hit rate への上乗せは小さい (+0.6〜+3.0 pt、負になる policy もあり、ノイズは約 ±3 pt)。live と simulator の差 (8〜11 pt) は、25 アクセスごとの評価の粒度ではなく、bench の capacity のずれ (7 個分と 8 個分) と `pick_victim` まわりの不具合 2 件 (in-flight の miss の `on_insert`、S3-FIFO の ghost の下限) が主因だった。修正後は ±3 pt で一致する (`benchmark-results.md` §18.44)。

### 4.3 daemon (upcall・admit・disk cache)

**普遍**
- admit の判定は、観測が少ない最初の読みで決めない。やり直しのきく粒度 (chunk ごと) に倒すか、観測を増やしてから決める。
- 取り込みは並列 range GET が効く (全体 admit は 4 並列)。粒度と並列度は一緒に設計する。
- ディスクキャッシュは、ETag・size を観測と照合しないと、同じ key を別の内容で置いたときに古い body を返す (AC-40 で対処)。

**現状の課題**
- C2。chunk 1 件 6〜7 ms の内訳 (disk 読み、populate の memcpy、signal) は未調査。
- daemon の disk cache が、計測の「cold」に残る (local cold と s3 cold を分けて測る必要がある)。

### 4.4 計測

**普遍**
- **cold と warm の定義を、構成ごとにそろえる**: (a) page cache だけ冷やす、(b) 構成のキャッシュも消して作り直す、の 2 種類に分け、どちらかを明記する。
- **タイマーの外で起きる処理に注意する**: open の中の全体 admit (34 ms) がタイマーの外に出ると、ループが warm に見える。open を含めた合計と、ループだけの両方を出す。
- **warm は prime してから測る**: 「cold の 1/10 のあとの全 n 回」は、構成ごとに warm になる残りが違う。1 回 prime して、複数回の中央値と min–max を出す。
- **負荷自体がキャッシュを温めていないか確認する**: `SELECT count(*)` は全走査して、ファイル全体を温めた。
- **`drop_caches` の値を明記する** (§4.1)。
- **rusage (minflt、utime、stime) を一緒に出す**: FS が動かせる上限の見積もりになる (§2.1)。
- **N=3 回のばらつきが大きい項目は参考値として扱う**。
- **誤りを見つけたら、結論の文書にも訂正を入れる**: `ann-workload-results.md` の cold・warm の読みは、後の節で訂正した。

**現状の課題**
- バケットの残骸 (hitrate_* など) や、arena の 128 MiB に収まらない object があると、bench が失敗する (`bench-env-notes.md`、memory の既知の落とし穴)。
- 単一ホスト・MinIO・単スレッドの Python に限られる。多プロセス・多スレッド・実 S3 の計測はない。

---

## 5. 次に確かめること

`where-fs-layer-can-win.md` §6 を参照 (N プロセスの同時 cold 読み、admit 判定の A/B、メモリ制約下の二重保持、大きいファイルの扱い)。

## 関連文書

- `docs/design/where-fs-layer-can-win.md` — 勝てる局面と難しさ、ユースケースの収集、policy 差し替えの将来
- `docs/journal/ann-workload-results.md` — simulator と実機の計測結果
- `docs/dev-notes/lessons-learned.md` — 実装で踏んだ地雷 (kmod・VFS・bench)
- `docs/design/design-review-checklist.md` — 設計・実装・測定の検討観点
- `docs/journal/history.md` — 開発史とトラブル
- `docs/design/cache-algorithms.md` — LRU / SIEVE / S3-FIFO / W-TinyLFU の早見

[r11]: https://www.usenix.org/conference/nsdi20/presentation/song
[r12]: https://par.nsf.gov/biblio/10647289-cache_ext-customizing-page-cache-ebpf
[r13]: https://www.usenix.org/conference/atc19/presentation/bijlani
[r14]: https://usenix.org/conference/fast17/technical-sessions/presentation/vangoor
[r15]: https://vldb.org/pvldb/vol16/p2769-durner.pdf
[r16]: https://arxiv.org/abs/2111.08566
