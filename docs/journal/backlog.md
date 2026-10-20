# Backlog — プロジェクト残課題の一元リスト

各週次 note (`docs/weekN-*.md`) と設計 doc に散らばっている follow-up を 1 箇所に集約したもの。「今すぐ着手できる小改修」と「Week 5+ の別 track」を **優先度 × カテゴリ** で並べる。

**運用ルール**:
- 完了したら該当行に ✅ を付けて出典を追記する。完了して時間が経った項目は [`backlog-archive.md`](backlog-archive.md) に移す (ID と本文はそのまま)
- 2026-10-18: A〜AB 系と AC-01〜AC-49 の完了項目、優先度 view (旧 §7) を archive に移した。開いている項目と AC-50 以降はここに残す。経緯は [`history.md`](history.md)
- 新規 follow-up は該当週次 note に書いてからここに 1 行反映
- 優先度は **P0 = 次スプリントで着手 / P1 = Week 5 で着手 / P2 = Meetup 後の polish** の 3 段

出典表記: `w4a` = `docs/journal/week4a-implementation-notes.md`、`w4b` = `docs/journal/week4b-implementation-notes.md`、`w4c` = `docs/journal/week4c-implementation-notes.md`、`w4ac` = `docs/journal/week4a-c-session-notes.md`、`ll` = `docs/dev-notes/lessons-learned.md`、`br` = `docs/journal/benchmark-results.md`、`be` = `docs/journal/bench-env-notes.md`、`pe` = `docs/design/policy-engine-design.md`、`as` = `docs/design/admit-and-softcap.md`、`hr` = `docs/journal/hit-rate-bench.md`、`mo` = `docs/user/meetup-ebpf-outline.md`。

---

## 1. 大枠マイルストーン

| # | milestone | 状態 | 次アクション |
|---|---|---|---|
| M1 | Week 4 (a) B/C read path | ✅ 完成 | — |
| M2 | Week 4 (b) dynamic bucket sync | ✅ 完成 | — |
| M3 | Week 4 (c) write-back + touch | ✅ 完成 | — |
| M4 | Week 4 (e) bench 一式 | ✅ 完成 | — |
| M5 | 性能可視化 (Grafana + Prometheus) | ✅ 完成 | — |
| M6 | Week 4 (d) policy engine 設計 | ✅ doc 完成 | `pe §6 Q1-Q10` を spike で決定 → 実装 |
| M7 | Week 4 (d) policy engine 実装 | ✅ 完成 | P-01〜P-15 全完了 (CEL Layer1 / eBPF Layer2/3 / KFunc / struct_ops hot swap / hit-rate bench) |
| M8 | Meetup 登壇 | ✅ demo 台本完成・本番待ち | L-01 demo-scenario.md 完成、narrative + bench 数値揃い済み |
| M9 | Week 5+ 分散 / multi-tenant / ML | ⏸ scope 外 | 現行 architecture の限界を洗い出してから (§B/C の項目参照) |

---

## 2. P0 — 次スプリントで着手すべき小改修

「1〜2 日でカタが付き、放置すると実運用で刺さる」もの。開いている項目はない (完了分は archive)。

## 3. P1 — Week 5 で着手 (機能拡張)

「架け替えが要る、または 1 週分の作業」もの。開いている項目はない (完了分は archive)。

## 5. P2 — Meetup 後の polish

「面白いが後回し」もの。

### AB. arena 役割縮退 (metadata-only) 設計改善 (2026-10-13 レビュー) — P1

**背景**: `docs/design/bpf-arena-use-cases.md` のレビュー結果、arena 128MB をバルクページキャッシュとして使う現設計は arena の設計意図 (small, hot metadata) と不整合。改善案A (arena を admit メタデータだけに縮退、ページ実体は Linux page cache に委譲) が最有力。GUP オーバーヘッド (AA-01 残課題) と arena サイズ上限 (大ファイル劣後) の両方を同時解消する見込み。**実装前に 3 段階の検証** (Phase 1 分離測定 → Phase 2 PoC → Phase 3 本格計測) で判断する。

**Phase 1 — 現状の bottleneck 分離測定** (判断材料集め、2〜3 日):

- [ ] **AB-03** mount-s3 を同一ワークロードで動かし外部基準の 4-way 比較 — AA-06 の bench 延長で、特に 10MB / 100MB の cold-read を 3 回ずつ (fuse-raw / fuse-cache / ebpf / mount-s3)。eBPF の positive/negative 差分の内訳を明確化。目安 0.5 日 (**AB-01/AB-02 で Gate PASS 済みのため省略可、必要に応じて実施**)

### K. upstream 貢献 (`w4a §9`)

根拠 doc: [`../dev-notes/ebpf-ecosystem-gaps.md`](../dev-notes/ebpf-ecosystem-gaps.md) — BPF_ARENA / STRUCT_OPS / KF_SLEEPABLE の踏んだ地雷を体系化。K-01/02 に加えて cilium/ebpf patch (`../misc/cilium-ebpf-addr-space-cast-marshal.patch`)、struct_ops template マクロ提案、KF_SLEEPABLE matrix doc 提案 (§2-§4) を並置。

- [ ] **K-01** `bpf_arena_get_kern_vm_start` の EXPORT_SYMBOL_GPL 提案 — 詳細 [`../dev-notes/ebpf-ecosystem-gaps.md §1.2`](../dev-notes/ebpf-ecosystem-gaps.md#12-改善案)
  当てれば kmod 側で BTF-derived struct redefine 不要になる。
- [ ] **K-02** arena kern_vm range 用の generic-context fault handler 追加提案 — 詳細 [`../dev-notes/ebpf-ecosystem-gaps.md §1.2`](../dev-notes/ebpf-ecosystem-gaps.md#12-改善案)
  当てれば A′ (kern_vm 直 deref) 経路が復活、B の GUP overhead が消える (数百ns オーダー改善見込み)。

### AD. FUSE に対する優位性の未実装項目 (`docs/design/ebpf-fuse-advantages-roadmap.md §3`)

2026-10-18 登録。roadmap の初版は Q-01〜Q-04 と書いていたが、Q 系 (セキュリティ修正、archive) と重なるので AD に振り直した。順序と見積もりは roadmap §4。

- [ ] **AD-01** `bpf_timer` による自律 eviction (roadmap §3-2、旧 Q-01)。daemon の push を待たずに容量を守る基盤。目安 3〜4 日
- [ ] **AD-02** BPF Iterator による arena GC (roadmap §3-4、旧 Q-02)。R-05 と合流。pcache モードでは arena がメタデータだけになったので、着手前に必要性を見直す。目安 4〜5 日
- [ ] **AD-03** KFunc + in-kernel radix tree による `readdir` 高速化 (roadmap §3-3、旧 Q-03)。AC-55 で SYNC_ENTRIES に索引が入ったので、残る差を測ってから判断する。目安 4〜5 日
- [ ] **AD-04** `fmod_ret` + `cgroup_id()` の per-cgroup policy (roadmap §3-1、旧 Q-04)。R-04 と合流。目安 4〜5 日

### R. 不足フック (`docs/design/ebpf-hook-gaps.md §2.3 / §4`)

2026-10-18 登録。R-01 (`fentry/generic_file_read_iter`) は完了 (archive)。

- [ ] **R-02** `struct_ops` に `on_read_miss(ino, size, rand_ratio)` を追加し、miss の upcall 前に refuse できるようにする。R-01 のデータが溜まってから
- [ ] **R-03** `fentry/d_lookup` で lookup を観測し、ネガティブキャッシュの頻度を取る
- [ ] **R-04** `lsm/inode_permission` の per-cgroup policy (AD-04 と合流)
- [ ] **R-05** `iter/bpf_map_elem` の arena GC (AD-02 と合流)
- [ ] **R-06** `fentry/filemap_fault` で mmap のアクセスを観測する。read 経路のフックと同じく、hit 経路の負担は `docs/design/read-iter-hook-overhead.md` §5 の基準で見る

## 6. スコープ外 (明示的 non-goal)

**Week 5+ か別 project でしか扱わない**もの。ここに書いておくことで「なぜやらないか」の説明を省ける:

- 分散 (複数 daemon 間の共有 policy state) — `pe §7`
- multi-tenancy (per-mount policy) — `pe §7`
- ML training loop (features 収集は視野、learning 自体は out of scope) — `pe §7`
- Lua / WASM の採用 — `pe §7` (task D 発生時のみ再評価)
- 独自 DSL 設計 — `pe §7` (CEL borrow)
- write-through / write-around semantics 選択 (現状 write-back のみ) — `pe §7`
- user eBPF program の marketplace / registry (allowlist ベースの手動 install で足りる) — `pe §7`

---

## AC. pcache モード修正 (P1〜P6 / Q1〜Q4、2026-10-13 レビュー、`br §14〜16`)

| # | 内容 | 優先度 | 状態 |
|---|---|---|---|
| AC-06 | P6: 64-job 律速の特定 (perf c2c / lockstat / generic_file_read_iter 比較) | P2 | ⏳ 未着手 |
| AC-50 | S3 にあるファイルを一度も読まずに途中へ write / truncate すると、読んでいない範囲が 0 のまま flush (PUT) され、オブジェクトが壊れる (上書きは書いた範囲まで切り詰め、縮小は 0、`: > f` は S3 に届かない)。open が中身を読み込まないため | P1 | ✅ 修正 (`br §18.22`): clean → dirty の最初の変更の前に、残る部分を FOR_WRITE の upcall で読み込む。取れなければ -EIO。test-write-existing 追加 (pcache / arena × 6 シナリオ、修正前 10/12 FAIL → 全 PASS) |
| AC-51 | 既存の大きいオブジェクトの途中の書き換えが遅い (100 MiB の 4 KiB 書き換えで全体を load + PUT、fuse-raw の 0.41×)。変更のない chunk を fd に持たず ETag 付きの copy 元 (remote) として記録し、flush は multipart + `UploadPartCopy` にする | P1 | ✅ 実装 (`br §18.36`、設計 `docs/design/ac51-partial-modify.md`): `--partial-modify` (既定 off、S3 のみ。GCS は `UploadPartCopy` がなく全体 PUT のまま)。100 MiB の途中書き換えは fuse-raw 並み。不変条件は `kmod-pcache-invariants.md` に追記済み |
| AC-52 | 大きいファイルの cold read が遅い (300 MiB で fuse-raw の 0.34×)。cache-dir 書き込みが読みの経路に乗る、GET が直列 | P1 | ✅ 一部達成 (`br §18.34`〜`§18.38`): cache-dir を write-behind、chunk 単位の並列 load、worker-local の get+populate、16 MiB range GET。300 MiB は中央値 0.80× (目標 0.9×) で未達、最小値は同等。残りは MinIO 側の揺れとホストのメモリ回収 (`br §18.39`)。cache-dir は大きい全体 admit では書かれない |
| AC-53 | `test-write-existing-capacity` が修正前のコード (`84ebf2f` の直前) で FAIL するかを確かめていない | P2 | ✅ 確認 (2026-10-17): `84ebf2f` の worktree で `kmod/vfs_ops.c` だけ `84ebf2f~1` に戻して `test-write-existing-capacity` を回すと、round 1 の `shrink2` で S3 / re-read とも FAIL (conc1〜4 は PASS)。修正後の kmod は全件 PASS。テストは修正前のコードを検出できる。1 回しか回していない (残りの round は FAIL 時の待ちが長く打ち切った)。注意: worktree には `bpf/*.o` (git 管理外) をコピーしないと daemon が `policy_observe` を見つけられず、修正後でも全件 FAIL になる |
| AC-54 | `s3fs_fd_modify_ready_locked` / `s3fs_ebpf_file_write` のコメントが「unpin してから lock を取る」と古い | P2 | ✅ 修正 (5369bf6) |
| AC-55 | SYNC_ENTRIES の lookup が線形探索で、上限 65536 は暫定 (50k 件の `ls -l` cold 15 s) | P2 | ✅ 修正 (`br §18.40`): (parent_ino, name) / ino の hash と (parent_ino, 並び順) の整列配列。lookup O(1)、readdir O(log n)。50k 件 cold 212 ms、100k 件 379 ms。上限は 262144 に引き上げ、メモリ保護として残す。`entries_replace` が vmalloc の表を `kfree` していた oops も `kvfree` で修正 |
| AC-56 | AC-48 の `admit_replace` の `i_size_write` が、同時に来た truncate を巻き戻す窓 (`s3fs_fd_buf_owned` の lockless 確認と書き込みの間) | P3 | ✅ 修正: 確認と `i_size_write` を `fd->lock` 下で行う (`s3fs_inode_grow_size_unowned`、admits_lock は解放済みでロック順は守られる)。窓が狭く再現するテストは作っていない。`test-regression` 17 件 PASS |
| AC-57 | dirty な fd の PCACHE エントリが穴の上に入り、容量の数え方 (`live_bytes`) を水増しする | P3 | ✅ 修正: 原因は AC-45 C3 の entry 入れ直し (`quiet` の data-based fill) が dirty / flushing な fd の穴でも entry を入れること。clean かつ chunk が populated のときだけ入れる。再現テストは作っていない。`test-regression` 17 件 PASS |
| AC-58 | policy の vtable に `pick_victim(n)` を足し、victim の選択を BPF に渡す (kmod は insert / access / remove を通知、容量超過で `pick_victim` を呼ぶ。使えない ino は既定の score 順に戻す)。まず SIEVE で実機の hit rate (so-fix のモデルで 46.8% / 現行 33.3%) と `pick_victim` / lock / on_hit のコストを測り、`docs/design/` に残す。アプリのヒントは map 経由 (キー ino、更新は daemon の API / xattr、fadvise / madvise の fentry で拾う案) | P1 | ✅ (`br §18.41` / `§18.42`、設計 `docs/design/ac58-pick-victim.md`): vtable に `on_insert` / `on_remove` / `pick_victim` を追加。`policy_sieve_pick_ops` (FIFO + hand)、`policy_lru_pick_ops` (厳密な LRU)、`policy_s3fifo_pick_ops` (S / M / G の 3 queue) を実装。3 seed で旧版より高い (S3-FIFO 36.7〜40.0%、LRU 34.9〜37.5%、SIEVE 35〜37%)。read 経路は 64 jobs で policy なしより −4.6% (旧 SIEVE は −14.8%)。score 型の policy (freq2bit / time_decay / size_aware / pressure) は移行不要、W-TinyLFU は見送り。残: 候補が数千のときの `pick_victim` の時間、W-TinyLFU、アプリのヒント |
| AC-59 | (レビュー HIGH) remote の区間がどれも 5 MiB 未満 (`[R,D,R,D]` など) だと `PlanParts` が `ErrNoCopy` を返し、`tryFlushPartial` が `handled=true` のまま EIO を返して、close / fsync のたびに同じ失敗を繰り返す | P1 | ✅ 修正: `flushPartial` が `ErrNoCopy` を受けたら全体を組み立てて plain put (remote の bytes は pin の object から、変わっていれば現在の object から)。`TestFlushPartial_NoCopyRunFallsBackToPut` (`R D R D` / `D R D R D`) |
| AC-60 | (レビュー MED + LOW 8) 古い ETag pin が、変更サイクルを放棄した後も残り、以後の load が 412 で EIO になる (daemon の再起動まで) | P1 | ✅ 修正: kmod の miss に `S3FS_MISS_FLAG_PINNED` (fd が dirty / flushing / remote を持つ) を足し、daemon の `getPinned` は PINNED でなければ pin を捨てて Stat し直し 1 回やり直す。PINNED は本物の競合として EIO のまま。`TestGetPinned`。`DIRTY_LAYOUT` は `flush_read_seq` を更新する副作用があり load 経路から呼べないので、フラグにした。未解決: 外部の truncate で object が短くなり remote chunk の範囲が末尾を超えるときは、フォールバックの GET も失敗する |
| AC-61 | (レビュー MED) flush 中の load が、`Complete` 後の ETag (E2) に対して古い pin (E1) で GET して 412 → EIO | P1 | ✅ 修正: `PutWithCopy` が返す新しい ETag を pin に保存 (plain put では pin を捨てる)。`getPinned` は 412 のとき pin が変わっていれば読み直してやり直す |
| AC-62 | (レビュー MED) kmod の `partial_modify` が 0 に戻らず、GCS など copier のない backend や古い daemon で remote chunk を持つ dirty file が全体 PUT になり、remote の範囲が 0 で書かれる | P1 | ✅ 修正: daemon は起動のたびに 0 か 1 を明示的に書く。copier がないのに remote chunk を持つ flush は EIO で断る (dirty のまま) |
| AC-63 | (レビュー LOW〜MED) flush 成功で、remote の chunk (途中までしか埋まっていない buffer を持つ) まで `populated` になり、0 や途中までの中身が clean な chunk として read に返る | P2 | ✅ 修正: `signal_flush` は remote でない chunk だけを populated にする。**end-to-end のテストでは判別できない**: flush 成功の直後に daemon が `dropAdmitIno` でその ino の未 pin の chunk を reset で free するので、誤って立った `populated` が読み出しに使われる窓がない (旧 kmod でも `test-write-partial` のシナリオ 7 は PASS)。シナリオ 7 は「buffer を持つ remote chunk の flush」の経路のカバレッジとして残した |
| AC-64 | (レビュー LOW 6, 7) AC-58: `on_remove` が capacity のパスからしか呼ばれず、BPF 側に古い ino が残る。`pick_victim` が候補にない ino を返すと `break` して残りが score 順になる。また u64 を返す member を `int BPF_PROG` で定義していた | P3 | ✅ 修正: 候補にない ino は `on_remove` して聞き直す。戻り値は `__u64 BPF_PROG`。`test-pick-victim-stale` (回帰テストに追加): 旧コードでは削除した ino が policy の `idx` に残って FAIL、修正後は PASS |
| AC-65 | (レビュー LOW 9) `flushPartial` は multipart を始める前に upload の part をすべてメモリに持つ。fill と upload は 120 s の ctx を共有する (以前の全体 PUT より悪くはない) | P3 | ✅ 修正: `objstore.Part` に `Fill` (遅延生成) を足し、`PutWithCopy` の worker が upload の直前に生成する。メモリに載る upload part は同時に最大 4 つ (以前は全部)。flush の timeout は 120 s に object 8 MiB あたり 1 s を足す。`Fill` が `ErrSourceChanged` を返せば source 変更として扱う。`TestFlushPartial` に「part が eager に埋められていない」確認を追加 |
| AC-66 | W-TinyLFU を AC-58 の `pick_victim` で動かす (window + SLRU + sketch + admission。populate 後の admission は「候補を victim にする」で表し、pending で shed になった ino は limbo で区画を覚える)。1 段目は件数ベースで kmod 変更なし、2 段目で vtable 末尾に `on_size(ino, bytes)` を足して予算をバイトにする | P2 | 実装 (2026-10-20、ライブ計測は未): `policy_wtinylfu_pick_ops`、kmod の `on_size`、trace-replay の `wtinylfu` / `wtinylfu-clock` / `s3fifo`、`make test-wtinylfu-harness` (verifier と Go の模型の一致)。trace-replay では 30 files / 容量 8 で S3-FIFO より上、大きい容量では 1〜4 ポイント下 (`docs/design/wtinylfu-pick-plan.md` §9)。実機 (1 回): `make test-regression` 全 PASS、`bench-hitrate` で S3-FIFO 37.80% に対し 38.55% (§9.4)。残: bench-hitrate の 3 seed、mixed、policy-cost |

### AC-06 調査計画 (未着手)
- `perf c2c record -a -- fio ...` (64 jobs, shared small file) でキャッシュライン競合の特定
- `perf record -g` + `lockstat` でロック競合の確認
- 比較対象: `.read_iter = generic_file_read_iter` 直結版 (admit 判定なし) での IOPS 計測

## 8. 参照

- `docs/journal/week4a-implementation-notes.md §9` / `§10`
- `docs/journal/week4a-c-session-notes.md §5`
- `docs/journal/week4b-implementation-notes.md §4`
- `docs/journal/week4c-implementation-notes.md §4`
- `docs/dev-notes/lessons-learned.md §5`
- `docs/journal/benchmark-results.md §6`
- `docs/journal/bench-env-notes.md §4`
- `docs/design/policy-engine-design.md §6` / `§7` / `§9`
- `docs/design/admit-and-softcap.md §4` / `§5` / `§7` (S 系)
