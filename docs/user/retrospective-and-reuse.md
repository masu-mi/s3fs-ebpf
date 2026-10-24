# 振り返り: このプロジェクトは失敗か、成果をどう活かすか (Rust での再実装、FUSE への貢献)

2026-10-22。「Rust による再実装や FUSE への貢献などで、このプロジェクトを活かせるか。また失敗だったのか」という問いへの答えをまとめる。リポジトリ内の数字と事実には出典 (`docs/user/meetup-qa.md` の Q 番号、`docs/journal/benchmark-results.md` の §、backlog ID) を付けた。リポジトリの外の状況 (Rust for Linux、aya、fuse-bpf、cache_ext など) は書いた時点での理解で、**この文書では確かめていない**。そうした箇所には「(未確認)」と付けた。手を付ける前に一次情報を見ること。

---

## 1. 結論

- **失敗ではない。** 最初の仮説 (bpf_arena と KFunc を使えば FUSE より速い FS になる) は外れた。しかし PoC の目的は「Linux 6.12 のこの 3 機能で FS を組めるか、どこが効いてどこが効かないか」を試すことで (`README.md`)、その答えは実測付きで出た。仮説の検証としては半分外れ、PoC としては成功、という評価になる。
- **活かす先は、kmod を Rust で書き直すことより、知見を FUSE 周りと「BPF で page cache のポリシーを差し替える」方向に持ち込むことの方が現実的。** Rust は daemon の書き直しなら現実的だが、性能の律速ではないので得るものは小さい。

---

## 2. 何がうまくいき、何がうまくいかなかったか

### 2.1 うまくいったこと

| 成果 | 根拠 |
|---|---|
| 速さの源を特定した。hit のときに daemon へ行かない (open も upcall しない) 構造そのものが効いている | open+read+close は fuse-cache の 172 倍 (§18.27)、小さいファイル 1 万個の warm cat は 93 倍 (§18.31)。arena を使わない pcache モードでも同じ水準 (Q55) |
| struct_ops の vtable の形についての知見。score を返すだけの vtable では順序が伝わらず、BPF 版の hit rate が simulator より低くなる。victim を BPF に選ばせる `pick_victim` にすると改善する | Q16〜Q18。pick_victim 版で旧版より 2〜7 ポイント高い (Q20) |
| 本当の律速を perf と A/B で特定した。64 jobs の並列 read の劣後は、自分で付けた `fentry/generic_file_read_iter` の負担 (約 19%) だった | Q34、§18.16、`docs/design/read-iter-hook-overhead.md` |
| 自前の VFS の race を、不変条件の文書化、修正前に FAIL するテスト、KUnit で止めた | `docs/kmod-pcache-invariants.md`、Q51、`docs/journal/history.md` §4〜§5 |
| 計測の無効化を記録し、数字を置き換えた (hit rate 88.5%、§16 / §17 など) | `docs/journal/history.md` §4.4 |
| upstream への具体的な要望が出た | K-01 / K-02、cilium/ebpf の marshal 修正 (`docs/dev-notes/ebpf-ecosystem-gaps.md`、`docs/misc/`) |

### 2.2 うまくいかなかったこと

| 問題 | 根拠 |
|---|---|
| bpf_arena は性能に効かなかった。kmod からは GUP が要り (1 回約 1.7 µs)、容量はコンパイル時に固定 (128 MiB) | Q10、Q55、§13.1、§18.2 |
| 推測で 3 回直して動かなかった (lock → rwlock、GUP、per-file cache)。AA 系は受け入れ条件を満たさないまま閉じた | `docs/journal/history.md` §2.4、§4.5 |
| 大きいファイルの cold read は続けて読むと 0.80 倍で負け (ホストのメモリ回収待ち) | Q37、§18.39 |
| 自前の VFS (kmod の C は KUnit を除いて約 7,100 行、`vfs_ops.c` だけで約 5,500 行) の正しさを保つのに時間の大半を使った。データ破損と race が続けて出た | `docs/journal/history.md` §4.1〜§4.3。fuse-bpf なら抱えずに済んだコスト (Q3) |
| 本番には使えない。daemon が落ちると queue が失われる、上書きの検知がない、pcache の容量が既定で無制限で memcg に課金されない、など | Q57 |
| 既定のモードが今も arena モードで、計測と修正は pcache モードで行っている | `docs/design/pcache-mode.md` §9 |

### 2.3 評価

- 外れたのは「arena / KFunc が速さの源になる」という手段の仮説。当たったのは「hit の経路でユーザー空間に行かなければ、open を繰り返す使い方で大きく速い」という構造の仮説。後者は FUSE の側でも実現できる (fuse-bpf、Q3) ので、「自前の kmod でなければ無理」とは言えない。
- 失敗談 (AA-03) と採用基準の表 (スライドの takeaway) は、「誤った場所で使い、測った」から言えること。発表の材料としては、ここが一番価値がある。

---

## 3. 再利用できるもの

| 資産 | 置き場所 | 持ち出せる先 |
|---|---|---|
| 「hit なら open / lookup も daemon に送らない」構造と、その効果の実測 | Q33、Q42、§18.27、§18.31 | FUSE / fuse-bpf の議論の根拠 |
| eviction の vtable の設計 (`on_insert` / `on_remove` / `pick_victim`) と、score 型が負ける理由 | `docs/design/ac58-pick-victim.md`、Q16〜Q20 | BPF で page cache のポリシーを差し替える仕組み一般 |
| 汎用関数への fentry の hot path での負担と、その測り方 (bpf_stats、並列の A/B、perf) | `docs/design/read-iter-hook-overhead.md`、Q30、Q31 | BPF を hot path に置くすべての場面 |
| backing file の段の設計 (FUSE passthrough と同じ形、未実装) | `docs/design/backing-file-tier.md`、Q50 | FUSE passthrough / fuse-bpf と同じ構造なので、FUSE 側で同じことをする設計の比較対象 |
| ポリシー simulator と trace の再生 | `docs/design/trace-harness.md` | カーネル版と simulator のずれの検証 |
| arena の仕組みと、kmod から使えない理由 | Q10、`docs/dev-notes/ebpf-ecosystem-gaps.md` §1 | arena の使いどころの説明、K-01 / K-02 |
| 不変条件の文書化と、修正前 FAIL のテストの進め方 | `docs/kmod-pcache-invariants.md`、Q51 | どの言語で書き直すときにも要る |

---

## 4. 方向ごとの現実性

### 4.1 daemon を Rust で書き直す (libbpf-rs または aya + tokio + S3 SDK)

- **現実的。** daemon は Go で約 14,000 行 (vendor を除く)。S3 との通信、bucket の同期、write-back、BPF のロード、CEL は Rust にも相当するものがある (CEL は cel-rust などがある。未確認)。
- **性能はほぼ変わらない見込み。** 速さの源は hit で daemon に行かないことで、daemon は miss と write-back にしか出てこない。cold read の負けもホストのメモリ回収待ちで、daemon の経路ではない (§18.39)。
- **BPF のローダーの選び方:** struct_ops と arena は libbpf の対応が一番早い。libbpf-rs は libbpf を包んでいるのでそのまま使えるはず。aya は自前のローダーで、struct_ops と arena への対応は弱いと理解している (未確認)。libbpf を使えば、cilium/ebpf の `addr_space_cast` の marshal 問題を vendor で回避する必要もなくなる (Q52)。
- **得るもの:** 学習、型による状態遷移の表現 (write-back の queue など)。発表の主題 (eBPF) からは外れる。

### 4.2 BPF プログラムを Rust で書く (aya-ebpf)

- **勧めない。** struct_ops の BPF 側と、arena のアドレス空間 (`__arena`、`addr_space_cast`) は clang の C で書くのが前提で、Rust のツールチェーンでの対応は弱いと理解している (未確認)。ポリシーの BPF は短いので、C のままで困らない。

### 4.3 kmod を Rust for Linux で書き直す

- **今のところ非現実的。** 理由は 3 つ。
  1. ファイルシステムを書くための VFS の Rust 抽象化は、upstream にはまだ揃っていないと理解している (未確認。2024 年までに RFC は出たが、mainline には入っていなかった)。out-of-tree で bindings を自分で書くことになる。
  2. この kmod が使う `register_bpf_struct_ops`、KFunc の登録 (`BTF_KFUNCS_START`)、miscdev の ioctl、page cache の操作の Rust の口も、自分で用意することになる (未確認)。
  3. 今回の主なバグの多くは Rust でも防げない。R1 (populate と evict の race)、AC-50 (未読部分が 0 で PUT)、F1 (古い 0 byte の PUT が後から着地) は、ロック順と設計の前提の問題だった。Rust が防げるのは AC-55 (kvcalloc を kfree で解放) や、解放後の参照の一部 (W1) の種類。
- **やるなら:** 全体ではなく、`tree_traversal.c` (KFunc、約 300 行) のような小さい部分から始めて、Rust for Linux の現状を確かめる学習課題にする。

### 4.4 FUSE / fuse-bpf / FUSE passthrough に貢献する

- **一番筋がよい。** この PoC の構造の仮説 (hit なら open も daemon に送らない) は、FUSE の側で実現する方が「自前の VFS を抱えない」ので筋がよい (Q3)。持ち込めるのは次のもの。
  - open / lookup / getattr の往復が、小さいファイルを大量に開く用途でどれだけ効くかの実測 (172 倍、93 倍、3.3 倍。条件は §18.27、§18.31)。
  - backing file の段の設計 (`docs/design/backing-file-tier.md`)。FUSE passthrough (6.9) は read / write を素通しするが、open の往復は残る (Q2)。
  - eviction を struct_ops で差し替える形と、vtable の形の知見 (Q16〜Q20)。fuse-bpf の BPF は経路の振り分け役で、キャッシュの判断はしない (Q4)。
- **入り方:** fuse-bpf は upstream に入っていない (Q3。2024 年以降の状況は未確認)。いきなりパッチを送るより、fsdevel / bpf の ML の議論に計測データ付きで参加する、FUSE passthrough + ローカルのディスクキャッシュで同じベンチを取って比べる、のが入口。
- **最初の一歩の案:** s3fs-fuse の代わりに、FUSE passthrough を使う daemon (libfuse の passthrough 例など) で §18.27 / §18.31 のベンチを取り、「open の往復が残るとどれだけ違うか」を数字にする。これは backing file の段 (Q50) の見込みの検証にもなる。

### 4.5 BPF で page cache のポリシーを差し替える研究との接続

- cache_ext (SOSP '25、eBPF でカーネルの page cache の eviction を差し替える研究) は、struct_ops でポリシーを差し込み、追い出しの順序を BPF 側のリストで持たせる方向だと理解している (未確認)。Q16〜Q18 の「score ではなく順序を渡す口が要る」という結論と同じ方向なので、比べて発表の補足にできる。
- この PoC の pick_victim は、自前の FS の chunk に対する eviction で、カーネル全体の page cache ではない。比べるときはこの違いを明記する。

### 4.6 小さな upstream 貢献

| 項目 | 内容 | 見込み |
|---|---|---|
| cilium/ebpf の marshal 修正 | `addr_space_cast` の Offset が 0 に潰れる (`docs/misc/cilium-ebpf-addr-space-cast-marshal.patch`) | 済み: 自分で出し、cilium/ebpf の main に `cc4947e` として取り込まれた (2026-10-24 に確認。リリースはまだ)。いまの依存 `v0.22.1-0.20261008…` はこれを含む |
| K-01 `bpf_arena_get_kern_vm_start` の export | 1 行の変更 | 低い。in-tree に使う側のない、out-of-tree の kmod のための export は通りにくい (一般的な傾向。未確認) |
| K-02 kern_vm の generic context 用 fault handler | 約 50 行の見込み | 低い。arena の設計の前提 (BPF の実行中だけの shadow mapping) に踏み込む。強い use case が要る (`docs/dev-notes/ebpf-ecosystem-gaps.md` §1.2) |

> **2026-10-24 追記 (7.2 のソースで確認、試してはいない)**: 7.2 で「bpf/arena: Direct kernel-side access」が入った。BPF から呼ばれた kfunc は、渡された arena のポインタを kern_vm の番地のまま読み書きでき、未割り当てのページに触れると `bpf_arena_handle_page_fault` (x86 / arm64 の fault 経路) が scratch page を張って続ける。ただし `bpf_prog_find_from_stack()` で BPF プログラムが stack にいるときだけ働くので、kmod の read 経路のように BPF の外から読む K-01 / K-02 の用途は残る。`bpf_arena_map_kern_vm_start()` / `bpf_prog_arena()` も足されたが、EXPORT_SYMBOL はされていない。

---

## 5. 優先順位の案

1. 発表では、この振り返り (失敗ではなく、手段の仮説が外れ、構造の仮説が当たった) をまとめの節で話す。スライドは「振り返り」の 1 枚 (`retro`)。
2. ~~cilium/ebpf の marshal 修正の upstream の状況を確かめ、直っていなければ PR を出す。~~ 済み (自分の修正が main に `cc4947e` で入っている、2026-10-24 確認)。
3. FUSE passthrough を使う daemon で同じベンチを取り、open の往復の差を数字にする (4.4)。
4. その数字を持って、fuse-bpf / FUSE passthrough の議論に参加するか判断する。
5. Rust は、daemon の書き直しを学習として、または kmod の小さい部分の試作として、余力があれば。

## 関連

- `docs/user/meetup-qa.md` Q2〜Q4 (FUSE / fuse-bpf との比較)、Q50 (backing file の段)、Q55〜Q58 (振り返りと今後)
- `docs/journal/history.md` (経緯、トラブル、教訓)
- `docs/design/backing-file-tier.md`、`docs/design/ac58-pick-victim.md`、`docs/design/read-iter-hook-overhead.md`
- `docs/dev-notes/ebpf-ecosystem-gaps.md` (K-01 / K-02 の根拠)
