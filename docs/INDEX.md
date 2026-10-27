# docs/ INDEX

docs/ はカテゴリ別のサブディレクトリに分かれている。初訪の読者はまず「どのカテゴリに何があるか」を本索引で把握し、本文は各 doc 冒頭の概要で判断する。相互リンクの真の source of truth は各 doc 本文 (`docs/<subdir>/<file>.md §N` 表記) だが、索引が壊れたら本文の参照も直したいので、追加/削除/移動はこの索引と同時に更新する。

## カテゴリ

| ディレクトリ | 対象読者 | 内容 |
|---|---|---|
| [`user/`](user/) | 使う人 / 発表担当 / 新入 dev | README から辿るユーザー向け (demo, policy の書き方, backend 選択) |
| [`design/`](design/) | 設計を読む dev | アーキテクチャ・コンポーネント間通信・各 subsystem の design |
| [`journal/`](journal/) | 時系列を追う dev | 週次 implementation note / session note / bench 結果 / backlog |
| [`dev-notes/`](dev-notes/) | 実装者 | 横断的な tips (ハマりポイント / upstream 未提供改善案) |
| [`misc/`](misc/) | 必要な時 | 第三者 patch など |

---

## 起点 (まずこの 3 本 + kmod 作業時 1 本)

| doc | 対象読者 | いつ読むか |
|---|---|---|
| [`../README.md`](../README.md) | 新入 dev | PoC の概要 / アーキテクチャ / Quick start / 性能の要約 |
| [`user/getting-started.md`](user/getting-started.md) | 新入 dev | 環境構築 / ビルド / 起動 / テスト / ベンチの手順 |
| [`user/motivation.md`](user/motivation.md) | 全員 | プロジェクトの前提と Meetup narrative (概要と数値) |
| [`design/component-communication.md`](design/component-communication.md) | 全員 | VFS / kmod / BPF / daemon / backend の全 11 通信路の mermaid 図 (最初にこれを見るのが最短) |
| [`kmod-pcache-invariants.md`](kmod-pcache-invariants.md) | kmod 実装者 | pcache のロック順 / R1 / pending key / chunk の pin / reset 条件の要約。`vfs_ops.c` を読む前に |

---

## user/ — ユーザー向け

| doc | 内容 |
|---|---|
| [`user/motivation.md`](user/motivation.md) | 背景 / Meetup narrative / スプリント計画 (§1) |
| [`user/getting-started.md`](user/getting-started.md) | 前提環境 / 環境準備 / ビルド / 起動 / 単体テスト / ベンチ / 可視化 / ディレクトリ構成 |
| [`user/cli-reference.md`](user/cli-reference.md) | `s3fs-ebpf --help` と `make help` の出力 (`make cli-reference` で `cli-reference.md.tmpl` から生成) |
| [`user/demo-scenario.md`](user/demo-scenario.md) | Meetup 35 分発表 script (Demo 0-8 の run / expected / say) |
| [`user/writing-policies.md`](user/writing-policies.md) | 自作 policy の書き方 (Layer 1 CEL / Layer 2 eBPF rebuilt / Layer 3 user eBPF plug) |
| [`user/posix-and-practical-notes.md`](user/posix-and-practical-notes.md) | FS 拡張の POSIX 準拠スコアカード (E-01/02/03, D-01/02) |
| [`user/gcs-backend.md`](user/gcs-backend.md) | GCS backend 対応の使い方と `--backend s3\|gcs` 切替 |
| [`user/related-works.md`](user/related-works.md) | ExtFUSE / RFUSE / Mountpoint との位置付け |
| [`user/meetup-ebpf-outline.md`](user/meetup-ebpf-outline.md) | eBPFミートアップ発表アウトライン 20min (KFunc / struct_ops / Layer 0 KFuncs 中心) |
| [`user/retrospective-and-reuse.md`](user/retrospective-and-reuse.md) | 振り返り: 失敗だったか (うまくいったこと / いかなかったこと)、再利用できる資産、Rust での再実装 / FUSE・fuse-bpf への貢献 / upstream 貢献の現実性と優先順位 (2026-10-22) |
| [`user/meetup-qa.md`](user/meetup-qa.md) | meetup の想定問答 58 問 (全体の設計 / arena と KFunc / struct_ops の vtable と運用 / フックの負担 / FUSE・mount-s3 との比較 / 計測の妥当性 / FS としての振る舞い / 開発と検証 / 振り返り)。冒頭に一言回答の早見表 |

---

## design/ — 開発者向け設計

| doc | 内容 |
|---|---|
| [`design/design.md`](design/design.md) | 統合設計書 (initial pitch, §0 で現状との相違メモ付き) |
| [`design/component-communication.md`](design/component-communication.md) | 5 層 11 通信路 (T1-T11) の transport / 頻度 / latency / コード位置 + mermaid 図 |
| [`design/dev-s3fs-ebpf-protocol.md`](design/dev-s3fs-ebpf-protocol.md) | `/dev/s3fs_ebpf` の miss イベント (`read(2)`) と 16 個の ioctl の構造体・フィールド・戻り値 |
| [`kmod-pcache-invariants.md`](kmod-pcache-invariants.md) | (kmod 実装者向け) pcache のロック順 / R1 / pending key / chunk の pin / reset 条件の要約。`vfs_ops.c` を読む前に |
| [`design/read-path-design.md`](design/read-path-design.md) | Path A/A′/B/C の design 比較、A′ 断念の記録 |
| [`design/pcache-mode.md`](design/pcache-mode.md) | pcache モード (`--arena-metadata-only`) の全体像: arena モードとの違い、chunk の表、read / eviction / write の流れ、性能と残課題 |
| [`design/pcache-chunk-table.md`](design/pcache-chunk-table.md) | pcache の chunk 表 (`fd->chunks[]`) のレイアウト: 構造体、4 MiB の番地の対応、chunk の状態、admit entry と容量の関係 |
| [`design/cache-algorithms.md`](design/cache-algorithms.md) | LRU / SIEVE / S3-FIFO / W-TinyLFU の図つきの早見と、この PoC での実装の形 |
| [`design/backing-file-tier.md`](design/backing-file-tier.md) | 案 (未実装・未計測): pcache の下にローカル FS の backing file へ素通しする段 (warm) を足し、メモリの上限を緩める設計。fuse-bpf / FUSE passthrough と同じ形。将来の検証項目 8 つ |
| [`design/ac51-partial-modify.md`](design/ac51-partial-modify.md) | AC-51: 既存の大きいオブジェクトの途中の書き換えを multipart + server-side copy にする設計 (実装済み、`--partial-modify`、既定 off) |
| [`design/ac58-pick-victim.md`](design/ac58-pick-victim.md) | AC-58: policy の vtable に `on_insert` / `on_remove` / `pick_victim` を足し、victim の選択を BPF に渡す設計 (実装済み、SIEVE / LRU / S3-FIFO の `*_pick_ops`) |
| [`design/wtinylfu-pick-plan.md`](design/wtinylfu-pick-plan.md) | AC-66: W-TinyLFU を `pick_victim` で動かす計画 (現行とのずれ、BPF 側の設計、kmod に足す `on_size`、手順。未実装) |
| [`design/hit-signal-percpu-accumulation.md`](design/hit-signal-percpu-accumulation.md) | issue #22: hit の信号を `.read_iter` で per-CPU に数え、`admit_replace` の evict の前に policy へ反映する (実装済み、`hit_accum`、既定 on、計測は §18.44〜18.45。再計測の手順と採否の基準つき) |
| [`design/hook-timings-diagrams.md`](design/hook-timings-diagrams.md) | 8 タイミング × 3 path のオブジェクト図 |
| [`design/partial-admit-internals.md`](design/partial-admit-internals.md) | D-01 partial admit / D-02 arena free-list の内部構造 |
| [`design/kmod-daemon-shared-memory.md`](design/kmod-daemon-shared-memory.md) | kmod と daemon の共有メモリ (miscdev の `.mmap`): 仕組み、arena との違い、この実装で使う場合の候補と注意。未実装・未計測 |
| [`design/admit-and-softcap.md`](design/admit-and-softcap.md) | admit / softcap の実際の挙動、キャッシュアルゴリズムとしての問題点 (容量上限なし等)、page cache との役割分担と改善項目 |
| [`design/policy-engine-design.md`](design/policy-engine-design.md) | 4 layer (CEL + built-in eBPF + user eBPF + KFunc) architecture の設計 |
| [`design/policy-attach-comparison.md`](design/policy-attach-comparison.md) | fmod_ret vs struct_ops の attach 方式比較 |
| [`design/trace-harness.md`](design/trace-harness.md) | P-04 ms-scale offline policy simulator |
| [`design/g01-subdir-plan.md`](design/g01-subdir-plan.md) | G-01/F-02 subdir 対応 + basename 衝突の実装計画 (実装済) |
| [`design/ebpf-fs-framework-design.md`](design/ebpf-fs-framework-design.md) | 汎用 eBPF FS framework 化の hook 分離設計 (4 role × 4 軸) + security model |
| [`design/ebpf-fuse-advantages-roadmap.md`](design/ebpf-fuse-advantages-roadmap.md) | FUSE に対する 3 点の構造的優位性の整理と、未実装 4 項目 (cgroup policy / bpf_timer / radix tree readdir / BPF Iterator GC) の実装計画 |
| [`design/ebpf-hook-gaps.md`](design/ebpf-hook-gaps.md) | eBPF 全移行価値の分析 (12 決定点の現状マップ) と、他 VFS/FUSE と比べた不足フックの詳細 (R-01〜R-06)。R-01 `fentry/generic_file_read_iter` は実装済み |
| [`design/read-iter-hook-overhead.md`](design/read-iter-hook-overhead.md) | R-01 read fentry が 64 jobs read を約 19% 落としていた件の調査記録 (経緯・試験と結果)、フックを使うときの注意点、hit 経路のフックの採用判断基準 |

---

## journal/ — 作業記録・報告

### 週次 implementation notes (時系列)

| doc | 内容 |
|---|---|
| [`journal/issue22-hit-accum-investigation.md`](journal/issue22-hit-accum-investigation.md) | issue #22 (`hit_accum`) の調査まとめ: 効果 (read 経路のコスト、hit rate への上乗せは小さい)、途中で見つけた 3 つのずれ (bench の capacity、in-flight の miss の `on_insert`、S3-FIFO の ghost の下限)、見つけ方、間違えたこと (2026-10-27)。再発防止の `make check-invariants` / `make test-policy-conformance` を含む |
| [`journal/history.md`](journal/history.md) | 開発史: 年表、backlog の系列ごとの課題、トラブル (データ破損 / race / oops / 計測の無効化 / 外れた仮説) と教訓 |
| [`journal/early-walkthroughs.md`](journal/early-walkthroughs.md) | 以前 README にあった Week 1〜3 の動作確認手順 (test-arena / fastpath / verifier-demo / s3-list) とスプリントのロードマップ |
| [`journal/week1-build-runtime-notes.md`](journal/week1-build-runtime-notes.md) | Week 1: kernel 6.12 + clang-20 + kmod build 疎通 |
| [`journal/week2-implementation-notes.md`](journal/week2-implementation-notes.md) | Week 2: MinIO + s3-list/stat 実装、verifier 地雷 |
| [`journal/week3-implementation-notes.md`](journal/week3-implementation-notes.md) | Week 3: arena/GUP プロトタイプ、VFS materialise |
| [`journal/week4a-implementation-notes.md`](journal/week4a-implementation-notes.md) | Week 4 (a): Path B (GUP prewarm) + arena kern_vm 断念の 1 次情報 |
| [`journal/week4a-c-session-notes.md`](journal/week4a-c-session-notes.md) | Week 4 (a-c) session: Path C (read-miss upcall) 実装と追加課題 |
| [`journal/week4b-implementation-notes.md`](journal/week4b-implementation-notes.md) | Week 4 (b): dynamic bucket sync + basename 衝突 |
| [`journal/week4c-implementation-notes.md`](journal/week4c-implementation-notes.md) | Week 4 (c): E-01/02/03 (fsync blocking / unlink / rename / coalescer) |

### Bench 結果

| doc | 内容 |
|---|---|
| [`journal/benchmark-results.md`](journal/benchmark-results.md) | Meetup 用の headline 数値 + 3-way fair bench (ebpf vs fuse-raw vs fuse-cache) |
| [`journal/hit-rate-bench.md`](journal/hit-rate-bench.md) | Zipf workload の policy hit-rate 実測 (2026-09-28 の数値は容量が効いておらず無効。2026-10-05 以降の capacity mode が正。`pick_victim` 版の比較は `journal/benchmark-results.md` §18.41〜18.43) |
| [`journal/bench-env-notes.md`](journal/bench-env-notes.md) | bench 環境 caveats、Grafana metric surface、`make bench*` の修正記録 |

### 運用

| doc | 内容 |
|---|---|
| [`journal/backlog.md`](journal/backlog.md) | 開いている follow-up と最近の項目 (AC-50 以降) |
| [`journal/backlog-archive.md`](journal/backlog-archive.md) | 完了した古い backlog 項目 (A〜AB 系、AC-01〜AC-49)。必要なときだけ読む |

---

## dev-notes/ — 開発者向け tips

| doc | 内容 |
|---|---|
| [`dev-notes/lessons-learned.md`](dev-notes/lessons-learned.md) | Week 1-4 横断の実装ハマりポイント (arena / new_inode / invalidate deadlock / rwlock sleep / pending race) |
| [`dev-notes/build-env-dependencies.md`](dev-notes/build-env-dependencies.md) | ビルド・検証環境の依存箇所 (カーネル版 / ヘッダ / コンパイラ / ツール / テストツール) と解消方法。`make doctor` の ID 対応表 |
| [`dev-notes/kunit.md`](dev-notes/kunit.md) | kmod の KUnit テスト (`make test-kunit`): 実行方法、suite の対象、テストで見つけて直したバグ |
| [`dev-notes/ebpf-ecosystem-gaps.md`](dev-notes/ebpf-ecosystem-gaps.md) | BPF_ARENA / STRUCT_OPS / KF_SLEEPABLE の upstream 未提供・改善案 (K-01/02 の根拠) |
| [`dev-notes/arena-address-translation.md`](dev-notes/arena-address-translation.md) | arena のポインタがどこで kern_vm の番地になるか (BPF の load、kfunc の `__arena` 引数、x86 / arm64 JIT の命令)。6.12 / 7.2 / 7.3 の違い |
| [`dev-notes/arena-access-paths.md`](dev-notes/arena-access-paths.md) | arena の 1 ページを daemon / BPF / KFunc / kmod がそれぞれどう読むか (GUP、ページのコピー回数) を図で比較。6.12 と 7.2〜7.3 の差分 |
| [`dev-notes/struct-ops-kmod-glue.md`](dev-notes/struct-ops-kmod-glue.md) | struct_ops を kmod から提供するときの glue (cfi_stubs / verifier_ops / init / reg / unreg / update / 呼び出し側) が何のために要るか。fmod_ret との行数の差の理由 |
| [`dev-notes/kcfi-basics.md`](dev-notes/kcfi-basics.md) | kCFI の基本動作 (x86 の命令列、FineIBT) と、BPF prog / struct_ops の trampoline にハッシュを付ける仕組み。cfi_stubs が要る理由 |
| [`dev-notes/cilium-ebpf-arena-and-struct-ops.md`](dev-notes/cilium-ebpf-arena-and-struct-ops.md) | cilium/ebpf から bpf_arena を使うときの課題 (C1〜C9) と回避策、struct_ops の使い方 (kmod / BPF / daemon)、Linux と cilium/ebpf への要望の表 (2026-10-24 時点の最新版) |

> 新規環境構築手順は `user/getting-started.md` と `Makefile` (`doctor` / `setup-kernel` / `setup-toolchain` / `setup-kbuild`) に正本があり、依存箇所ごとの症状と解消は `dev-notes/build-env-dependencies.md`、週 1 の `journal/week1-build-runtime-notes.md` に初回踏破時のメモが残っている。

---

## misc/ — その他

| doc | 内容 |
|---|---|
| [`misc/cilium-ebpf-addr-space-cast-marshal.patch`](misc/cilium-ebpf-addr-space-cast-marshal.patch) | cilium/ebpf に投げる予定の marshal 修正 patch。vendor 済み詳細は `daemon/third_party/ebpf/README-s3fs-patch.md` |

---

## 更新運用

- doc を**追加**したら本索引にも 1 行反映。無ければ検索経由でしか辿れない。
- doc を**リネーム / 移動**したら `git grep -n '<old-path>' docs/ README.md` で参照サイトを全部書き換える。
- 相互リンクは `docs/<subdir>/<file>.md §N` の形で書き、他 doc から辿れるようにする (anchor は heading 変更で壊れるので使わない)。
- カテゴリに迷ったら: README から直リンクされるものは `user/`、設計を純粋に説明するなら `design/`、時系列で起きたことなら `journal/`、横断 tips なら `dev-notes/`。
