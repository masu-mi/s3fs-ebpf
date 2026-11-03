# 学習ガイド: このリポジトリの技術要素を、入門記事と一次資料から順に学ぶ (2026-11-03)

s3fs-ebpf は、次の技術を組み合わせた PoC。初めて読む人が、リポジトリの doc に入る前に「土壌」を作れるように、技術要素ごとに **(1) 何か、(2) この repo のどこで使っているか、(3) 入門記事 (日本語を優先)、(4) 一次資料** を並べた。

- 使い方: §1 の読む順に、必要なところだけ拾う。全部を読む必要はない。各節の「この repo では」の doc が、そのまま実装の説明になっている。
- **リンクの確認状況** (2026-11-03 時点): 表の `✔` は本文または冒頭を取得して、存在と内容を確かめたもの。`○` は検索結果の題名と要約までで、本文は精読していない。リンクは変わるので、切れていたら題名で探し直す。
- **日本語が見つからなかった項目:** bpf_arena、lockdep、kCFI の日本語の入門記事は、検索では見つからなかった (§3 の各節に書いた)。英語の一次資料と、この repo の日本語の doc で補う。見つけたら足す。
- 外部の記事は、この repo の内容の裏付けではなく入門用。この repo の主張の根拠は各 doc の本文 (測定は `journal/`) にある。

---

## 1. 読む順 (土壌づくり)

| 段階 | 読むもの | 目安 |
|---|---|---|
| 0. 全体 | [`../../README.md`](../../README.md) → [`motivation.md`](motivation.md) → [`../design/component-communication.md`](../design/component-communication.md) (mermaid 図) | 全体像。ここだけで「何を作ったか」が分かる |
| 1. 前提 | §3.1 eBPF の基本、§3.2 VFS とファイルシステム、§3.3 FUSE | eBPF と FS を知っていれば飛ばしてよい |
| 2. 中核 | §3.4 bpf_arena、§3.5 KFunc、§3.6 struct_ops | この PoC の主題。発表の流れもこの 3 つ |
| 3. 実装の周辺 | §3.7 Go と cilium/ebpf、§3.8 キャッシュのアルゴリズム、§3.9 pcache (page cache との分担) | 設計の理由 |
| 4. 品質 | §3.10 KUnit、§3.11 lockdep、§3.12 kCFI | カーネルモジュールを安全に書く側 |
| 5. 計測 | §3.13 閉じた環境の MinIO (Silo) + Prometheus / Grafana | 再現と検証 |
| 6. 振り返り | [`retrospective-and-reuse.md`](retrospective-and-reuse.md)、[`meetup-qa.md`](meetup-qa.md) (想定問答 58 問、冒頭に早見表)、[`../design/lessons-cache-fs-layer.md`](../design/lessons-cache-fs-layer.md) | 何がうまくいき、何がうまくいかなかったか |

実装を触る人は、段階 2〜4 の後に [`../kmod-pcache-invariants.md`](../kmod-pcache-invariants.md) と [`getting-started.md`](getting-started.md) へ進む。

## 2. 用語の早見 (1 行)

| 用語 | 1 行 |
|---|---|
| eBPF | カーネル内の安全な仮想マシン。verifier が検査した BPF プログラムをカーネルに差し込む |
| fentry | 関数の入口に BPF を付ける hook。引数が型付きで読める |
| bpf_arena | BPF とユーザー空間が共有する、ページ単位で疎な 4 GiB までのメモリ領域 (Linux 6.9 以降) |
| KFunc | BPF から呼べるように印を付けたカーネル関数。安定した ABI ではない。モジュールからも足せる |
| struct_ops | カーネル (またはモジュール) が定義した関数ポインタの表を、BPF で実装して差し込む仕組み |
| kmod | この PoC の out-of-tree カーネルモジュール。VFS にファイルシステムを登録する |
| pcache | この PoC の方式。ファイルの中身を kmod の chunk と page cache に置く ([`../design/pcache-mode.md`](../design/pcache-mode.md)) |
| KUnit | カーネル内の単体テストの枠組み |
| lockdep | カーネルの実行時のロック依存の検査器。デッドロックになりうる順序を、実際に起きる前に報告する |
| kCFI | 間接呼び出しの先の関数の型をハッシュで検査する CFI (Control Flow Integrity) |
| MinIO / Silo | S3 互換のオブジェクトストア。この repo は閉じたローカル環境 (compose) に 1 台立てて測る |

---

## 3. 技術要素ごとの入門と一次資料

### 3.1 eBPF の基本 (verifier、BTF、CO-RE、map)

**この repo では:** BPF のプログラムは policy とキャッシュ判定に使う。採用した機能 (fentry / arena / ringbuf / per-CPU / LRU hash / struct_ops / KFunc / CO-RE / `bpf_loop`) の用途と落とし穴は [`../design/ebpf-features-guide.md`](../design/ebpf-features-guide.md)、policy の書き方は [`writing-policies.md`](writing-policies.md)、verifier の地雷は [`../dev-notes/lessons-learned.md`](../dev-notes/lessons-learned.md)。

| 種類 | 資料 | 確認 |
|---|---|---|
| 入門 (日本語) | [eBPF - 入門概要 編 (Zenn, hidenori3)](https://zenn.dev/hidenori3/articles/e1352e8cfeb2af): 歴史、構成、ツール、制約 | ✔ |
| 入門 (日本語) | [eBPF Deep Dive: Architecture and Safety Mechanisms (Speaker Deck, takehaya)](https://speakerdeck.com/takehaya/ebpf-deep-dive-architecture-and-safety-mechanisms): ロードから verifier、JIT、attach、map まで | ✔ |
| 入門 (日本語) | [eBPFとは何か：Linuxカーネルを安全に拡張する仕組み (Zenn, su8)](https://zenn.dev/su8/articles/af9e346a80e598): CO-RE と verifier の検査項目 | ○ |
| 一次資料 | [What is eBPF? (ebpf.io)](https://ebpf.io/what-is-ebpf/) | ✔ |
| 一次資料 | [eBPF Docs (docs.ebpf.io)](https://docs.ebpf.io/linux/): map type、program type、kfunc の個別ページ | ✔ |
| 一次資料 | [BPF のカーネル文書 (docs.kernel.org/bpf)](https://docs.kernel.org/bpf/index.html): verifier、BTF、libbpf、maps、kfuncs への入口 | ✔ |

### 3.2 VFS とカーネルモジュールのファイルシステム

**この repo では:** kmod が `super_operations` / `inode_operations` / `file_operations` / `address_space_operations` を実装して VFS に直接登録する (FUSE を経由しない)。read は `.read_iter` と `.read_folio`、書き込みは flush の queue。[`../design/pcache-mode.md`](../design/pcache-mode.md)、[`../design/pcache-chunk-table.md`](../design/pcache-chunk-table.md)、[`../kmod-pcache-invariants.md`](../kmod-pcache-invariants.md) (ロック順など)、[`../design/dev-s3fs-ebpf-protocol.md`](../design/dev-s3fs-ebpf-protocol.md) (kmod と daemon の ioctl)。

| 種類 | 資料 | 確認 |
|---|---|---|
| 入門 (日本語) | VFS の 4 つのオブジェクト (superblock / inode / dentry / file) の解説記事は多いが、「カーネルモジュールで FS を自作する」日本語の手順書は検索では見つからなかった | - |
| 一次資料 | [Overview of the Linux Virtual File System (docs.kernel.org)](https://docs.kernel.org/filesystems/vfs.html) | 未取得 |
| 一次資料 | カーネルのソース (`include/linux/fs.h`) を、使うカーネルの版で直接読むのが確実 | - |

> 「未取得」は、URL が一般的な形で、取得ツールで確かめていないもの。

### 3.3 FUSE と、比較の相手 (s3fs-fuse、mount-s3)

**この repo では:** 比較の基準は s3fs-fuse の `use_cache` + `kernel_cache`。FUSE の往復のコスト、勝てる場面と勝てない場面は [`related-works.md`](related-works.md)、[`../design/where-fs-layer-can-win.md`](../design/where-fs-layer-can-win.md)、基準データは [`../journal/fuse-baseline-raw-data.md`](../journal/fuse-baseline-raw-data.md)。

| 種類 | 資料 | 確認 |
|---|---|---|
| 入門 (日本語) | [Filesystem in Userspace (FUSE) のカーネルとデーモン間の通信 (Qiita, tkusumi)](https://qiita.com/tkusumi/items/6dc204ba964264c72a9a): `/dev/fuse` 上の要求と応答を strace で見る | ✔ |
| 入門 (日本語) | [Rustで学ぶFUSE (1) リードオンリーなファイルシステムの実装 (Qiita, narumatt)](https://qiita.com/narumatt/items/2dfc3aedf3aafd459e81) (2017 年頃の記事で、crate は古い) | ○ |
| 入門 (日本語) | [GoでFUSEを使ってS3をマウントする (Zenn, nobonobo)](https://zenn.dev/nobonobo/articles/d4851717d85fe9) | ○ |
| 一次資料 | [FUSE Passthrough (docs.kernel.org)](https://docs.kernel.org/filesystems/fuse/fuse-passthrough.html): backing file へ素通しする仕組み。[`../design/backing-file-tier.md`](../design/backing-file-tier.md) の案の元 | ✔ |
| 一次資料 | [libfuse](https://github.com/libfuse/libfuse) (`example/hello.c` が最小の例) | 未取得 |
| 論文 | To FUSE or Not to FUSE (FAST '17): [発表ページ](https://www.usenix.org/conference/fast17/technical-sessions/presentation/vangoor) | [`../design/lessons-cache-fs-layer.md`](../design/lessons-cache-fs-layer.md) の参考文献より |

### 3.4 bpf_arena

**この repo では:** 既定の arena モードでファイルの中身を arena に置く。kmod から見た arena は「daemon のユーザーメモリ」で、読むたびに GUP (約 1.7 µs) が要ったため、pcache モードに移った。
読む順: [`../design/bpf-arena-use-cases.md`](../design/bpf-arena-use-cases.md) (向く用途と向かない用途) → [`../dev-notes/arena-access-paths.md`](../dev-notes/arena-access-paths.md) (誰がどう読むか、図) → [`../dev-notes/arena-address-translation.md`](../dev-notes/arena-address-translation.md) (`addr_space_cast` と JIT) → [`../dev-notes/go-and-bpf-arena.md`](../dev-notes/go-and-bpf-arena.md) (Go から使うコツ) → [`../dev-notes/cilium-ebpf-arena-and-struct-ops.md`](../dev-notes/cilium-ebpf-arena-and-struct-ops.md) (課題 C1〜C9)。

| 種類 | 資料 | 確認 |
|---|---|---|
| 入門 (日本語) | 検索では見つからなかった | - |
| 入門 (英語) | [eBPF Tutorial by Example: BPF Arena for Zero-Copy Shared Memory (DEV, yunwei37)](https://dev.to/yunwei37/ebpf-tutorial-by-example-bpf-arena-for-zero-copy-shared-memory-nf): pointer ベースの構造を map の index なしで作る | ✔ |
| 解説 | [A look at what's possible with BPF arenas (LWN, 2025-05-13)](https://lwn.net/Articles/1019885): 使い道と制約 (kernel pointer を置けない) | ✔ |
| 一次資料 | [BPF_MAP_TYPE_ARENA (docs.ebpf.io)](https://docs.ebpf.io/linux/map-type/BPF_MAP_TYPE_ARENA/)、kfunc は [`bpf_arena_alloc_pages`](https://docs.ebpf.io/linux/kfuncs/bpf_arena_alloc_pages/) / [`bpf_arena_free_pages`](https://docs.ebpf.io/linux/kfuncs/bpf_arena_free_pages/) | ✔ |
| 一次資料 | `kernel/bpf/arena.c` と、導入時の commit `317460317a02` ("Introduce bpf_arena") を、使うカーネルの版で読む。`docs.kernel.org` には専用の頁が見つからなかった (`/bpf/map_arena.html` は 404) | - |

### 3.5 KFunc

**この repo では:** 判断の hot path で使う関数 (`Layer 0 KFuncs`) を kmod が KFunc として提供し、BPF から呼ぶ。KF_SLEEPABLE の呼び出し側の制約が地雷になった。[`../design/policy-engine-design.md`](../design/policy-engine-design.md) (4 層の構成)、[`../dev-notes/ebpf-ecosystem-gaps.md`](../dev-notes/ebpf-ecosystem-gaps.md) §3 (KF_SLEEPABLE)、[`meetup-qa.md`](meetup-qa.md) (arena と KFunc の節)。

| 種類 | 資料 | 確認 |
|---|---|---|
| 入門 (日本語) | [eBPF を拡張したい話 -カーネルモジュールで KFunc を追加する- (Speaker Deck, masu_mi)](https://speakerdeck.com/masu_mi/ebpf-o-kakuchou-shitai-hanashi-kaneru-mojuru-de-kfunc-o-tsuika-suru): eBPF Japan Meetup #2 (2024-12-06)。この repo の作者の発表 | ✔ |
| 入門 (日本語) | [eBPFを使ったオレオレカーネル拡張入門 (Speaker Deck, Keisuke Nishimura)](https://speakerdeck.com/mu_mu_mu/ebpfwoshi-tutaoreorekanerukuo-zhang-ru-men): Kernel/VM 探検隊 #16。自作の program type を足す | ✔ |
| 一次資料 | [BPF Kernel Functions (kfuncs) (docs.kernel.org)](https://docs.kernel.org/bpf/kfuncs.html): 定義、引数の注釈、`KF_ACQUIRE` などのフラグ、安定した ABI ではないこと | ✔ |
| 一次資料 | [KFuncs (docs.ebpf.io)](https://docs.ebpf.io/linux/concepts/kfuncs/) | ✔ |

### 3.6 struct_ops

**この repo では:** policy (on_admit / on_evict / on_hit / on_insert / on_remove / pick_victim など) の vtable を kmod が定義し、BPF で実装して `BPF_LINK_UPDATE` 1 回で差し替える。fmod_ret 版は削除した。
読む順: [`../dev-notes/struct-ops-kmod-glue.md`](../dev-notes/struct-ops-kmod-glue.md) (kmod 側の glue が何のためか) → [`../dev-notes/struct-ops-data-members.md`](../dev-notes/struct-ops-data-members.md) (関数ポインタ以外の member) → [`../design/ac58-pick-victim.md`](../design/ac58-pick-victim.md) (vtable の粒度) → [`../design/policy-attach-comparison.md`](../design/policy-attach-comparison.md) (fmod_ret との比較) → [`../dev-notes/cilium-ebpf-arena-and-struct-ops.md`](../dev-notes/cilium-ebpf-arena-and-struct-ops.md) §2。

| 種類 | 資料 | 確認 |
|---|---|---|
| 入門 (英語のスライド) | [Learning sched_ext: BPF extensible scheduler class (Speaker Deck, shun159)](https://speakerdeck.com/shun159/learning-sched-ext-bpf-extensible-scheduler-class): struct_ops の代表例の sched_ext の解説 (英語のスライド) | ✔ |
| 入門 (日本語) | [eBPF Deep Dive (Speaker Deck, takehaya)](https://speakerdeck.com/takehaya/ebpf-deep-dive-architecture-and-safety-mechanisms): struct_ops 専用ではなく、eBPF 全体 | ✔ |
| 入門 (英語) | [eBPF Tutorial: Extending Kernel Subsystems with BPF struct_ops (DEV, yunwei37)](https://dev.to/yunwei37/ebpf-tutorial-extending-kernel-subsystems-with-bpf-structops-2n3g): 最小のカーネルモジュールで struct_ops を公開し、BPF で実装する。この repo と同じ形 | ✔ |
| 入門 (英語) | [StructOps Internals in ebpf-go (Speaker Deck, shun159)](https://speakerdeck.com/shun159/structops-internals-in-ebpf-go): cilium/ebpf が struct_ops を load する流れ | ✔ |
| 一次資料 | [BPF_MAP_TYPE_STRUCT_OPS (docs.ebpf.io)](https://docs.ebpf.io/linux/map-type/BPF_MAP_TYPE_STRUCT_OPS/) | ✔ |
| 一次資料 | [Extensible Scheduler Class (docs.kernel.org)](https://docs.kernel.org/scheduler/sched-ext.html): struct_ops を使う最大の例 | ✔ |

> カーネル文書 (docs.kernel.org) に struct_ops 専用の頁は見つからなかった (`/bpf/bpf_struct_ops.html` は 404)。モジュールから struct_ops を提供する形は、`net/ipv4/bpf_tcp_ca.c` (`tcp_congestion_ops`) と、selftests の `bpf_testmod` が実物 (どちらも使うカーネルの版で読む)。

### 3.7 Go (daemon) と cilium/ebpf

**この repo では:** daemon は Go。BPF の load と attach は cilium/ebpf、`bpf2go` で `.o` を埋め込む。arena の mmap は自前、struct_ops の差し替えは raw syscall。依存の落とし穴は [`../dev-notes/cilium-ebpf-arena-and-struct-ops.md`](../dev-notes/cilium-ebpf-arena-and-struct-ops.md)、Go と arena の相性は [`../dev-notes/go-and-bpf-arena.md`](../dev-notes/go-and-bpf-arena.md)。

| 種類 | 資料 | 確認 |
|---|---|---|
| 一次資料 | [ebpf-go Documentation (ebpf-go.dev)](https://ebpf-go.dev/): ロード、グローバル変数、オブジェクトの寿命、feature 検出 | ✔ |
| 一次資料 | [cilium/ebpf (GitHub)](https://github.com/cilium/ebpf)、[Go Reference](https://pkg.go.dev/github.com/cilium/ebpf) | ✔ (ebpf-go.dev からの link) |
| 入門 (英語) | [StructOps Internals in ebpf-go (Speaker Deck)](https://speakerdeck.com/shun159/structops-internals-in-ebpf-go) | ✔ |

### 3.8 キャッシュのアルゴリズム (LRU / SIEVE / S3-FIFO / W-TinyLFU)

**この repo では:** policy を struct_ops で差し替えて hit rate を比べる。図つきの早見は [`../design/cache-algorithms.md`](../design/cache-algorithms.md)、計測は [`../journal/benchmark-results.md`](../journal/benchmark-results.md) §18.41〜18.43、simulator は [`../design/trace-harness.md`](../design/trace-harness.md)。

| 種類 | 資料 | 確認 |
|---|---|---|
| 論文 | SIEVE (NSDI '24): [発表ページ](https://www.usenix.org/conference/nsdi24/presentation/zhang-yazhuo) | [`../design/cache-algorithms.md`](../design/cache-algorithms.md) の参考文献より |
| 論文 | S3-FIFO (SOSP '23): [PDF](https://junchengyang.com/publication/sosp23-s3fifo.pdf)、[著者の解説](https://blog.junchengyang.com/system/cache/2023/08/01/s3fifo) | 同上 |
| 論文 | TinyLFU: [arXiv 1512.00727](https://arxiv.org/abs/1512.00727)。実装は [Caffeine](https://github.com/ben-manes/caffeine) | 同上 |
| 関連 | cache_ext (page cache を eBPF で拡張): [論文ページ](https://par.nsf.gov/biblio/10647289-cache_ext-customizing-page-cache-ebpf) | [`../design/lessons-cache-fs-layer.md`](../design/lessons-cache-fs-layer.md) の参考文献より |

### 3.9 page cache と GUP (pcache の背景)

**この repo では:** 中身を kmod の chunk と page cache に置く pcache モード。chunk の pin、reset の条件は [`../kmod-pcache-invariants.md`](../kmod-pcache-invariants.md)。arena モードで kmod が daemon のメモリを `pin_user_pages_remote` で読んだ理由は [`../dev-notes/arena-access-paths.md`](../dev-notes/arena-access-paths.md)。

| 種類 | 資料 | 確認 |
|---|---|---|
| 一次資料 | [Page Cache / address_space (docs.kernel.org)](https://docs.kernel.org/filesystems/vfs.html) の `address_space_operations` の節 | 未取得 |
| 一次資料 | GUP は `mm/gup.c` と、`Documentation/core-api/pin_user_pages.rst` (使うカーネルの版で読む) | - |

### 3.10 KUnit

**この repo では:** daemon も root も使わずに、kmod のデータ構造と VFS の振る舞い (chunk 表、admit 表、pending、`kern_mount` した FS の read / write、policy の vtable の dispatch) を `make test-kunit` で単体試験する。kmod のソースを kernel の tree に組み込み、UML (既定) か QEMU で動かす。構成とテストで見つけたバグは [`../dev-notes/kunit.md`](../dev-notes/kunit.md)。

| 種類 | 資料 | 確認 |
|---|---|---|
| 入門 (日本語) | [Linuxカーネルのユニットテスト機構KUnitを活用するためのメモ (Qiita, n01e0, 2020)](https://qiita.com/n01e0/items/22ce59508408df6ce8c3): `kunit.py` の実行、テストの追加、`KUNIT_CASE` と `kunit_suite` (5.5 当時の記事) | ✔ |
| 一次資料 | [KUnit - Linux Kernel Unit Testing (docs.kernel.org)](https://docs.kernel.org/dev-tools/kunit/index.html): Getting Started と Using KUnit への入口 | ✔ |

### 3.11 lockdep

**この repo では:** `CONFIG_PROVE_LOCKING` つきのカーネルを build して、回帰テスト一式を通す (`make test-lockdep`)。ロック順の規則は [`../kmod-pcache-invariants.md`](../kmod-pcache-invariants.md)、手順と読み方は [`../dev-notes/lockdep-kernel.md`](../dev-notes/lockdep-kernel.md)。性能はこのカーネルでは測らない。

| 種類 | 資料 | 確認 |
|---|---|---|
| 入門 (日本語) | lockdep を主題にした日本語の記事は検索では見つからなかった。ロックの関数全般は [Linuxのロック共通関数とspinlock (Qiita, juntaki)](https://qiita.com/juntaki/items/5048f4bbe9127aead7e3) が `lock_acquire()` の周辺に触れている | ○ |
| 入門 (英語) | [lockdep for pthreads (Ceph blog)](https://ceph.io/en/news/blog/2008/lockdep-for-pthreads/): 「保持中のロックを記録して、依存グラフの循環を調べる」という考え方 | ○ |
| 一次資料 | [Runtime locking correctness validator (docs.kernel.org)](https://docs.kernel.org/locking/lockdep-design.html): lock class、使用状態、逆順の検出、再帰する read lock | ✔ |

### 3.12 kCFI

**この repo では:** struct_ops の `cfi_stubs` が、kCFI が有効なカーネルで必要になる (無効でも stub は要る)。命令列と BPF との関係は [`../dev-notes/kcfi-basics.md`](../dev-notes/kcfi-basics.md)、glue の全体は [`../dev-notes/struct-ops-kmod-glue.md`](../dev-notes/struct-ops-kmod-glue.md)。

| 種類 | 資料 | 確認 |
|---|---|---|
| 入門 (日本語) | 検索では見つからなかった | - |
| 解説 (英語) | [FineIBT Updated For Linux (Phoronix)](https://www.phoronix.com/news/FineIBT-Linux-Updated): kCFI と Intel IBT を組み合わせた FineIBT の位置づけ | ○ (取得は 403) |
| 論文 | [Drop the ROP: Fine-Grained Control-Flow Integrity for the Linux Kernel (Black Hat Asia 2017, PDF)](https://www.blackhat.com/docs/asia-17/materials/asia-17-Moreira-Drop-The-Rop-Fine-Grained-Control-Flow-Integrity-For-The-Linux-Kernel-wp.pdf) | ○ |
| 一次資料 | [Control Flow Integrity (Clang)](https://clang.llvm.org/docs/ControlFlowIntegrity.html): `-fsanitize=kcfi` の節がある | ✔ |

### 3.13 閉じた環境の MinIO (Silo) と、Prometheus / Grafana

**この repo では:** 外の S3 や GCS には出ずに、手元の docker compose に S3 互換のオブジェクトストア 1 台と、Prometheus / Grafana / node_exporter を立てて、すべての測定をその中で行う。MinIO は 2026-10 時点で community イメージの匿名 pull を制限しているため、MinIO の AGPL-3.0 fork の **Pigsty Silo** (`docker.io/pgsty/silo:latest`) に切り替えてある。CLI、環境変数、health endpoint、port は MinIO と同じ。
手順は [`getting-started.md`](getting-started.md)、性能試験の意味と見方は [`../dev-notes/bench-suite-overview.md`](../dev-notes/bench-suite-overview.md)、環境の注意点は [`../journal/bench-env-notes.md`](../journal/bench-env-notes.md)、実行と検証の手順は `.claude/skills/perf-bench-verify/SKILL.md`。

| 種類 | 資料 | 確認 |
|---|---|---|
| 入門 (日本語) | [ローカルS3代替としてのMinIO (Zenn, take5553, 2026-05)](https://zenn.dev/take5553/articles/minio-local-s3-cheatsheet): Docker、コンソール、`mc`、AWS CLI、endpoint だけで S3 と切り替える | ✔ |
| 入門 (日本語) | [MinIOによるローカルS3環境の構築 (Zenn, rescuenow)](https://zenn.dev/rescuenow/articles/54d54469aa2349)、[MinIO、知らないうちに終了していた (Qiita, kuma_3838)](https://qiita.com/kuma_3838/items/b9073cce25766508485a) (配布状況の変化と代替) | ○ |
| 一次資料 | [MinIO AIStor Documentation](https://docs.min.io/community/minio-object-store/index.html) (community 版の頁は AIStor の文書に置き換わっている) | ✔ |
| 一次資料 | [Silo (Pigsty の MinIO fork)](https://silo.pgsty.com/) | 未取得 (`getting-started.md` に記載のもの) |

---

## 4. もっと追うときの入口

- 発表の想定問答 ([`meetup-qa.md`](meetup-qa.md)): 設計の理由、arena と KFunc、struct_ops の運用、FUSE との比較、計測の妥当性を 58 問で。
- 開発史 ([`../journal/history.md`](../journal/history.md)): 年表、トラブルと教訓。
- 設計のチェックリスト ([`../design/design-review-checklist.md`](../design/design-review-checklist.md)): 見つけた不具合から、検討の観点を整理。
- doc の全体の地図は [`../INDEX.md`](../INDEX.md)。

## 5. このガイドの更新

- 外部リンクを足すときは、取得して中身を確かめ、確認の欄に `✔` / `○` / `未取得` を付ける。日本語の記事が見つかったら、「見つからなかった」と書いた節に足す。
- 題名しか分からないものは `○`。精読していない記事を、この repo の主張の根拠にしない。
