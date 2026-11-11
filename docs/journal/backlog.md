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

> **2026-10-21 訂正**: 実装 (pcache モード、`--arena-metadata-only`) では arena にメタデータも置かなかった。中身は page cache と kmod の chunk 表、admit のメタデータは kmod の admit hash、policy の状態は BPF map にある。daemon は arena の map を作って mmap するが、read 経路では使わない (`docs/user/meetup-qa.md` Q53)。

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
- [ ] **AD-02** BPF Iterator による arena GC (roadmap §3-4、旧 Q-02)。R-05 と合流。目安 4〜5 日。2026-10-21 訂正: 「pcache モードでは arena がメタデータだけ」は誤りで、pcache モードは arena に何も置かない。GC が要るのは arena モード (中身を arena に置く、今の既定) だけなので、arena モードを残すかを決めてから着手するか閉じる
- [ ] **AD-03** KFunc + in-kernel radix tree による `readdir` 高速化 (roadmap §3-3、旧 Q-03)。AC-55 で SYNC_ENTRIES に索引が入ったので、残る差を測ってから判断する。目安 4〜5 日
- [ ] **AD-04** `fmod_ret` + `cgroup_id()` の per-cgroup policy (roadmap §3-1、旧 Q-04)。R-04 と合流。目安 4〜5 日

### R. 不足フック (`docs/design/ebpf-hook-gaps.md §2.3 / §4`)

2026-10-18 登録。R-01 (`fentry/generic_file_read_iter`) は完了 (archive)。

- [ ] **R-02** `struct_ops` に `on_read_miss(ino, size, rand_ratio)` を追加し、miss の upcall 前に refuse できるようにする。R-01 のデータが溜まってから
- [ ] **R-03** `fentry/d_lookup` で lookup を観測し、ネガティブキャッシュの頻度を取る
- [ ] **R-04** `lsm/inode_permission` の per-cgroup policy (AD-04 と合流)
- [ ] **R-05** `iter/bpf_map_elem` の arena GC (AD-02 と合流。arena モードだけの話、AD-02 の訂正を参照)
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
| AC-53 | `test-write-existing-capacity` が修正前のコード (`a14204c` の直前) で FAIL するかを確かめていない | P2 | ✅ 確認 (2026-10-17): `a14204c` の worktree で `kmod/vfs_ops.c` だけ `a14204c~1` に戻して `test-write-existing-capacity` を回すと、round 1 の `shrink2` で S3 / re-read とも FAIL (conc1〜4 は PASS)。修正後の kmod は全件 PASS。テストは修正前のコードを検出できる。1 回しか回していない (残りの round は FAIL 時の待ちが長く打ち切った)。注意: worktree には `bpf/*.o` (git 管理外) をコピーしないと daemon が `policy_observe` を見つけられず、修正後でも全件 FAIL になる |
| AC-54 | `s3fs_fd_modify_ready_locked` / `s3fs_ebpf_file_write` のコメントが「unpin してから lock を取る」と古い | P2 | ✅ 修正 (1649af5) |
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
| AC-67 | pcache モードで中身の prewarm ができない (AC-44 でメタデータだけにした。inode がないので POPULATE_PAGES が -ENOENT) | P2 | 実装 (`--pcache-prewarm`、既定 off、`docs/design/pcache-mode.md` §4.1): daemon が policy で選んだファイルを起動後にマウント経由で読む。kmod は変えない。Go の単体テストのみ実行。`make test-pcache-prewarm` と、prewarm 後の 10 MiB の途中書き換え (§18.36) の計測は実機で未実施 |
| AC-68 | 起動時以外の prewarm: 読まれたファイルの次の兄弟を先読みしたい (学習データの shard、連番のログ) | P2 | 実装 (CEL `prefetch_next: <int>`、`docs/design/pcache-mode.md` §4.1): 最初の chunk の miss のあと、同じディレクトリの名前順で次の N 個を AC-67 の worker で読む。worker 自身の miss からは連鎖しない。Go の単体テストのみ実行。`make test-prefetch-next` と効果の計測は実機で未実施。残: fadvise(WILLNEED) の fentry、ジョブ開始 (exec / cgroup) の契機、HTTP API |
| AC-69 | (future work) `admit_replace` が入れ替えのたびに admit 表の全 ino に `on_evict` (pick_victim 型では `on_insert` も) を呼び、容量を超えていない回も結果を捨てている。kmod の重複除去は線形探索で O(N²)。hit の read 経路ではなく、miss と sync の経路のコスト。計測: ino 数 100 / 1,000 / 10,000 で `admit_replace` 1 回の時間と BPF の合計、miss の latency。改善: 超過時だけ `on_evict`、`on_insert` は新しい ino だけ、重複除去をハッシュに。詳細 `docs/design/ac58-pick-victim.md` §8 | P2 | ⏳ 未着手 (2026-10-26 登録、コードを読んだだけで未計測) |
| AC-70 | 新規の大きいファイルの `fsync` が PUT の完了前に返る。さらに、遅れて発火した 0 byte の PUT が完成したオブジェクトを空にする (性能試験 B の verify で発見。100 MiB を `dd conv=fsync` した直後の HEAD で `Content-Length` が 0 になったのは master で 12 回中 9 回)。原因: `.create` の 0 byte プレースホルダ event を ticker が pop して coalescer (200 ms) に預け、fsync の WAIT event は `runFlushWaitLoop` が処理するがその預かり分を取り消さない (kmod の F1 は kmod の queue の item だけ)。預かり分が WAIT の PUT の最中か後に発火して `ev.Size`(0) の PUT を行い、その `READ_DIRTY` が flush_seq を覆ったことにして waiter も早く起こす | P0 | ✅ 修正 (`daemon/cmd/s3fs-ebpf/flush_order.go`: WAIT の pop で預かり分を取り消し、WAIT より前に pop された非 WAIT event を捨て、同じ ino の `handleFlush` を直列化)。回帰テスト `make test-fsync-large-new-file` (修正前 9/12 回 FAIL → 修正後 PASS) と `flush_order_test.go`。`docs/kmod-pcache-invariants.md` F1 (daemon 側)、`br §18.48` |
| AC-71 | (AC-70 の残る窓) kmod の `READ_DIRTY` は、`cap < fd->size` でもコピーを短く切るだけで `flush_read_seq` を「覆った」と更新していた。daemon は buffer を event の size (queue した時点の size、古いことがある) で作るので、古い event の読みが fsync の waiter を PUT の前に起こし、PUT 自体も短かった。AC-70 の修正 (daemon の直列化) で窓は狭まったが、根は残っていた | P2 | ✅ 修正: `s3fs_ebpf_read_dirty` は `cap >= fd->size` のときだけ claim し、足りなければ何もコピーせず -ENOSPC (`out_bytes` に必要なサイズ、`dirty_layout` と同じ形)。daemon は `readDirtyAll` で報告されたサイズで読み直す (最大 3 回、足りなければ PUT せず dirty を戻して -EIO)。UAPI の構造体は変えない (seq を運ぶ案は不要)。回帰テスト: KUnit `s3k_vfs_read_dirty_short_buffer_claims_nothing` (修正なしで FAIL → 修正後 PASS)、`flush_handle_test.go`、`make test-fsync-large-new-file`。`docs/kmod-pcache-invariants.md` W3 |
| AC-72 | 性能試験の環境の衛生: 残った `mc admin trace` (コンテナ内) が MinIO を遅くし、S3 を伴う全項目が 25〜30% 遅くなり `test-policy-conformance` も落ち、さらに「page cache があると policy の順位が崩れる」という誤った結論まで出た。`docker exec` のクライアントを kill してもコンテナ内の `mc` は止まらない。気づくまでの §18.48 / §18.49 の値は全部無効で、測り直した | P1 | ✅ `scripts/lib/bench.sh` の `minio_require` が残りを検出して止まる。手順は `.claude/skills/perf-bench-verify/SKILL.md` §0 (対照項目で環境の異常を見分ける、ディスクの空き 20% 以上)。`br §18.48` |
| AC-73 | `test-policy-conformance` が環境のタイミングに敏感: ホストが遅いと s3fifo / wtinylfu の live hits が simulator より 3.5〜12 pt 多くなって FAIL する (変更前の master でも同じ)。通る / 通らないが環境次第だと、回帰として信用できない | P2 | ⏳ 未着手。試験の前に対照 (A の fuse-raw / ebpf-nocache の p50 など) で環境を確かめて SKIP にする、または sync / eval の round が access に対して決定的になるようにする (`SYNC_EVERY=3600s` は入っている) |
| AC-74 | 性能試験 H: ebpf-pcache の `observe_ops` (何も evict しない基準) の hit rate が seed 間で大きくばらつく (四分位 38.5〜44.8%、最小〜最大 29.9〜47.5%)。ebpf-pcachedrop では 27.2〜28.6% と安定。LRU は observe と +0.6 pp で差がない (SIEVE / S3-FIFO / W-TinyLFU は +10〜19 pp)。page cache のヒットが hit に混ざることと、policy が evict した ino の page cache を落とす処理の寄与を切り分けていない | P2 | ⏳ 未着手。ebpf-pcache で observe の seed ごとの hit を分解 (page cache hit / chunk 表 hit / upcall)、`hit_accum` の replay 数と evict された ino の page cache の drop 数を数える (`br §18.48`) |
| AC-75 | daemon の `--cache-dir` (disk cache) が **4 MiB (`partialAdmitThreshold`) を超える object には効かない**: 大きい object は chunk ごとの `GetRange` (`serveChunkLoad`) で取り、`GetRange` は disk cache を読みも書きもしない (cache は object 全体がキー)。kmod の chunk 表から落ちると、`--cache-dir` があっても毎回 S3 に戻る。64 MiB の「disk cache からの再読み」は upcall 288 回に対して S3 GET 1136 回で、45.5 ms (daemon の処理 24.0 ms)。s3fs-fuse の disk cache は 64 MiB でも効き 26.2 ms (0.58×)。層ごとのレイテンシの層 3 の行の GET 数で発見 (`br §18.49`) | P1 | 🔧 実装済み・未計測。`s3.Client.GetRange` が ETag 既知のとき `(key, offset, size)` 単位で disk cache を読み書きする (If-Match で取るので、cache の chunk は header の ETag の bytes。`List`/`Stat` で ETag が変わると読まない、`Put`/`Delete`/`Rename` は ETag を忘れる)。書き込みは write-behind で、枠が埋まっていれば待つ (落とすと次の読みが S3 GET になるため)。単体: `TestDiskCache_Range`、MinIO 付き: `TestGetRange_DiskCache`。回帰テスト `TestGetRange_LargeObjectSecondReadNoGET` (64 MiB の 2 回目の S3 GET が 0 回) と仕様 `docs/design/disk-cache-range.md` を追加。**残り**: ① kmod 経由の実機確認 (`bench-layer-latency.sh` の層 3 の再計測)、② GCS の `GetRange` は generation 固定で対応 (2026-11-10、tag の単体テストのみ、実 GCS では未確認) |
| AC-76 | kmod の chunk 表からの read (`.read_folio`) が page cache より遅い: 4 MiB で 1.08 ms 対 0.32 ms (3.4 倍)、64 MiB で 20.8 ms 対 7.4 ms (2.8 倍)、スループットは約 3.2 GB/s 対 9.0 GB/s。folio ごとの copy と、page cache を捨てて入れ直す分と見られる (`br §18.49`)。小さい読み (upcall の固定費 約 59 µs を避ける) には効くが、大きい読みでは page cache のまま返すほうが速い | P2 | 🔧 対処済み (2026-11-04、PR #43)。`ebpf-pcachedrop` を `perf` で分解: ① この FS は専用の bdi を持たず `ra_pages == 0` なので readahead が一度も呼ばれず、4 KiB の folio ごとに `.read_folio` だった (`page_cache_sync_ra` が 1 ページずつ)。② `super_setup_bdi` + `ra_pages` = 4 MiB と `.readahead` (1 回の `fd->lock` で chunk 内の folio をまとめて埋める) を試すと `.read_folio` は 0 回になったが、**レイテンシは変わらなかった** (64 MiB: 21.5 → 24.5 ms、4 MiB: 1.09 → 1.13 ms、ノイズの範囲)。③ 内訳 (64 MiB): folio の alloc + page cache への insert 約 22%、`copy_to_user` 約 12%、folio への memcpy 約 14%、`pcache_drop` の open ごとの `invalidate_inode_pages2` 約 10%。page cache を捨てて入れ直す分が支配的で、kmod 側のロック / hit 計上は小さい。④ 4 KiB folio のままでは縮まらない。⑤ **large folio + `.readahead` で対処** (`read_large_folios` 既定 on、`docs/kmod-pcache-invariants.md`): fill を `memcpy_to_folio` / `folio_zero_range` にし、`mapping_set_large_folios` と専用 bdi (`ra_pages` 4 MiB) を入れた。`ebpf-pcachedrop`: 4 MiB 1.09 → 0.74 ms、64 MiB 21.5 → 16.1 ms (page cache は 0.29 / 6.8 ms で、差は 3.7× / 2.8× → 2.5× / 2.4×)。`make test-regression` 28 件 PASS、dmesg 異常なし。**残り**: lockdep 付きカーネルでの確認 (`make test-lockdep`)、`pcache_drop` の invalidate 約 10% はベンチの仕組み |
| AC-77 | struct_ops のデータメンバ `flags` (`S3FS_POLICY_F_NO_ON_HIT`) は宣言と検査だけで、read の経路の最適化には使っていない (`docs/dev-notes/struct-ops-data-members.md` §4)。`on_hit` を持たない policy でも、hit の集計 (issue #22 の `hit_accum`) と `s3fs_policy_ops_eval_hit` の呼び出しが毎回走る | P2 | ✅ 実装 (2026-11-10): `s3fs_policy_skip_hit` (reg / update / unreg で更新) が真なら `s3fs_hit_count` は read pattern だけ数えて返る。KUnit `s3k_vfs_hit_count_skipped_no_on_hit`、不変条件メモ更新。実機の計測は未 |
| AC-78 | open の存在確認 (kmod `open_check`) の既定が 0 のまま: O_CREAT でない open は S3 に聞かず entry table だけで成功し、sync の後に S3 から消えた key でも open できる (エラーは最初の read、page cache にあれば出ない)。s3fs-fuse は open のたびに HEAD する (キャッシュありでも 1000 回で HEAD 1001、`br §18.51` `§18.52`)。`open_check=1` は実装・回帰試験済み (`make test-open-check`) だが、cache hit の open が約 420 µs (S3 の HEAD 1 往復) になり、キャッシュあり同士の優位が約 170× から約 1.2× に縮む | P1 | ✅ 既定を 1 にした (2026-11-10、kmod の module param。性能試験の harness は `BENCH_OPEN_CHECK` で明示するので従来どおり 0)。TTL つき (AC-79) は別途。以前の記述: 決めるまで「キャッシュあり同士の 170×」を headline にするときは「open で S3 を確認しない設計の値」と書く。HEAD が約 1% 余分に数えられる件 (6000 open に対し 6078〜6092) の原因も未切り分け |
| AC-79 | open の存在確認の頻度を下げる mode (**調査: 将来実装の可能性の判断**): `open_check` の毎回 HEAD (425 µs) と確認なし (3 µs) の間が未実装・未測定。候補 (A) 確認結果を ino ごとに `ttl_ms` だけ再利用する (保証は「TTL 以内の削除までは古い結果が見える」。s3fs-fuse の stat cache の期限と同じ種類)、(B) TTL 超過時は先に進めて非同期に再確認し、404 なら次の open から失敗させる (最初の 1 回が古い結果になり A より弱い)、(C) 変更の通知を受けた分だけ無効化する (AC-80) | P2 | ⏳ 未着手。計画: ① (A) を kmod の module param `open_check_ttl_ms` (0 = 毎回、既存の `open_check` と併用) として足す。ino ごとの最終確認時刻は `fd` (`s3fs_file_data`) に持ち、hit_accum と同様に lockless で読む。② TTL = 0 / 10 ms / 100 ms / 1 s で A (キャッシュあり同士) の p50 と silo への HEAD 数を測る (`br §18.52` の表を埋める)。③ 遠い S3 を `tc netem delay 12.5ms 2.5ms` (`br §18.50`) で模擬し、TTL ごとの効きを見る。④ 正確性: 削除してから TTL 以内の open が成功する窓を、回帰試験 (`test-open-check` の拡張) で上限つきで確かめる。不変条件 (`docs/kmod-pcache-invariants.md` の pending のキー) は変えない見込み |
| AC-80 | サーバー側の更新 (他のクライアントによる PUT / DELETE) を daemon から kmod に伝える経路 (**調査: 将来実装の可能性の判断**): 今は daemon の周期 sync (`--sync-every`) が entry table を丸ごと入れ替えるだけで、sync の間の削除は open_check の HEAD でしか分からない。候補: (a) bucket notification (MinIO / silo の webhook・queue) を daemon が受けて、該当 ino の確認済み印と chunk を無効化する、(b) arena (daemon と BPF の共有メモリ) に ino ごとの世代番号 (generation) を書き、kmod / BPF の hit 経路が open で読んで TTL 内でも古い結果を捨てる、(c) 周期 sync を差分にして ETag の変化だけを送る | P2 | ⏳ 未着手。計画: ① 通知の遅れと取りこぼし (at-least-once か) を silo で確かめる。取りこぼしても TTL (AC-79) で上限が付く構成にする。② arena に世代番号を書く経路の費用を測る (open の hit で 1 回読むだけなら 3 µs に足されるのは数十 ns の見込み、未測定)。③ AC-81 の policy 記述と合わせて、ポリシーごとに「通知を信じる / TTL / 毎回確認」を選べるかを判断する |
| AC-81 | open の確認と更新通知の方針を YAML などで宣言する policy (**調査: 将来実装の可能性の判断**): 今のキャッシュ policy は BPF struct_ops (`pick_victim` など、`docs/dev-notes/struct-ops-data-members.md`) で eviction の順位だけを決める。「どの key (prefix / サイズ / 拡張子) は open で毎回 HEAD、どれは `ttl_ms` 秒、どれは通知を信じて確認しない」を、daemon が読む YAML で書き、daemon が kmod の module param / ino ごとの設定 (arena 経由) に落とす形が候補。例: `rules: [{match: {prefix: models/}, open_check: {ttl: 60s}}, {match: {prefix: tmp/}, open_check: always}]`。struct_ops の `flags` (AC-77) は eviction 側の宣言で、open の確認は別の軸 | P2 | ⏳ 未着手。判断の材料: ① AC-79 の TTL ごとの効き (遠い S3 で何割の open が HEAD を省けるか)。② ino ごとの設定を arena に置くときの kmod 側の読み方 (BPF の hot path に入れず、open の 1 回だけ読む)。③ YAML の変更を反映する経路は既存の policy の hot swap (`BPF_LINK_UPDATE`、`test-policy-reload`) に倣えるか。④ 効きが小さい (HEAD の割合が低い) なら実装せず、AC-78 の既定を決めて閉じる |
| AC-82 | flush の PUT のあと、daemon が mode に関係なく中身を arena にコピーして `ADMIT_ARENA` で admit し直す (`flush_loop.go` の `handleFlushWith` の末尾)。pcache モード (`--arena-metadata-only`) でも同じで、kmod は `SIGNAL_FLUSH` の成功で chunk の `populated` を立てるだけで PCACHE entry は入れ直さないため、**書いた直後の read は arena のコピーを GUP で読む** (なければ S3 への upcall。コードの読み、実機では未確認)。代償: (1) 余分な memcpy と arena の消費 (新規の 100 MiB の fsync は 100 MiB のコピーが入る。`br §18.48` の B に含まれている、未計測)、(2) arena (128 MiB) が一杯で `ErrCacheFull` になると、PUT が成功していても `FlushErrors` を増やす (metric の help は "PUT failures")、(3) pcache モードの設計 (中身は chunk、arena は使わない) と食い違う。改善案: kmod が `SIGNAL_FLUSH` の成功で、`populated` な chunk に PCACHE entry を入れる (R1 / pin の不変条件に注意、`docs/kmod-pcache-invariants.md`)。そうすれば daemon の arena への書き直しを `arenaMetadataOnlyEnabled` で省ける。計測: 書いた直後の read の遅延と S3 の GET 数、B (1 / 10 / 100 MiB)、`flush_errors_total` | P2 | P2 | ✅ 対応 (2026-11-05、v2 Phase 2): daemon の arena への書き直しを削除し、kmod が `SIGNAL_FLUSH` の成功で populated な chunk に PCACHE entry を入れる。回帰は `make test-regression`。**書いた直後の read の遅延と S3 の GET 数、新規 100 MiB の fsync の計測は未了** |
| AC-83 | `test-parity` (s3fs-fuse との差分テスト) を足したので、既知の差は `scripts/lib/parity-known-diff.sh` に理由つきで並べ、それ以外の差は FAIL にする。今の既知の差は AC-80 (外部の更新が次の sync まで見えない) と AC-86 だけ。**未カバーの操作**: 並行 open (同じ ino に複数の open_check)、大きいファイルの途中 write 後の read-back、readdir の分割読み、chmod / utimens / symlink、O_APPEND、sync を有効にした状態 (`--sync-every` 既定) での外部更新の遅れの上限 | P2 | ⏳ 未着手。操作を足すたびに差が出たら、直すか既知の差に理由つきで置く |
| AC-84 | flush と rename の処理順が保証されない (`flush_loop` と `delete_loop` の rename は別の ticker)。TLA+ の反例 (`docs/design/file-lifecycle-state-machine.md` 反例 2、3): ① 作成直後に rename すると rename が先に処理されて (元 key が S3 にない) dropped、後の flush が旧名で PUT される: S3 は旧名、ユーザーは新名。**実機で再現** (`test-parity` `create_rename_quick`: s3fs-fuse は `q2.txt`、s3fs-ebpf は `q1.txt`)。② rename の直後に同じ名前で作ると、flush が旧 ino の object を上書きし、続く rename が新しい内容を移して元の名前を消す (データ損失。ino ごとの順序づけでは防げない。AC-85 に隠れて実機では未再現) | P1 | ✅ 修正 (2026-11-05)。kmod が flush / rename / delete の全イベントに通し番号 `seq` を付け (`s3fs_evseq_lock` の下で採番と `list_add`)、daemon は 3 つのキューを seq 順にマージして 1 本のループで処理する (`event_order.go` の `orderedDrain`)。rename / delete の前には coalescer の保留中の flush を全部出す (`Coalescer.FireAll`)。fsync (WAIT) の flush は自分の ゴルーチンで即時に処理されるので、PUT の前に `orderedBefore(seq)` で自分より前のイベントを処理する。`ebpf` のイベント構造体に `seq` を追加 (ioctl 番号が変わる。kmod と daemon は一緒に build)。TLA+ の `keys-serialized` が実装。回帰: `TestDrainOrdered*`、`TestFireAll*`、`make test-parity` の `create_rename_quick` と `fsync_rename_reuse_name` (**反例 3 のデータ損失を実機で再現: 修正前は ENOENT、修正後は s3fs-fuse と一致**)。残る: ② の rename 直後の同名作成は、synced なファイルでは AC-85 (EEXIST) が先に失敗する |
| AC-85 | rename した直後に、synced なファイル (S3 の listing から来た dyn entry) の旧名で作成すると EEXIST、旧名の open が成功して renamed ファイルの内容を返す (s3fs-fuse は作成が成功、open は ENOENT)。原因: `s3fs_ebpf_rename` は kmod 自身が作った local entry の名前しか即座に更新せず (`local_entry_rename`)、dyn entry は次の sync まで旧名のまま (`unlink` も同じ)。`test-parity` の `rename_open_old` / `rename_then_reuse_name`。AC-86 を直したので、これまで EIO に隠れていた旧名の open が露出した | P1 | ✅ 修正 (2026-11-05)。**tombstone**: rename / unlink した (親, 名前, ino) を `s3fs_hidden_entries` に覚え、lookup と readdir の dyn の結果から外す。rename の新名は local entry (S3 由来のファイルでも)。tombstone の取り消しと local entry の昇格は、daemon が sync の前に測る「適用済みの番号 W」(`appliedSeq`、`S3FS_IOC_SYNC_ENTRIES` の `applied_seq`) 以下の seq のときだけ。設計と検証: `docs/design/kmod-entry-table-state-machine.md`、`specs/tla/KmodEntries.tla` (TLA+ が 3 つの設計の穴を見つけた: sync で全部消す / listing に無ければ消す / 昇格が即時)。回帰: `make test-parity` (`rename_open_old`、`rename_then_reuse_name`、`unlink_sync_lookup`、`rename_sync_lookup`、`rename_back`。`--sync-every=0` と `1s` の両方)、`TestAppliedSeqStopsBeforeUnfinishedFlush`。修正前のコードで `rename_sync_lookup` / `rename_back` が FAIL することを確認。**未実施**: KUnit (UML のツリーなし)。残る: 失敗した daemon の DELETE / rename で tombstone が残り続ける (メモリは操作の数まで、AC-80 の通知で解消する余地) ／ PR #45 のレビューで直したもの: 保留中の flush が残るのに追跡を外す (`untrackFlush` を seq 付きに)、sync が mirror の記録を消す (`ReplaceApplied`)、再起動後に W が 0 に戻る (kmod のイベント番号を STATS slot 15 で読む)、readdir の重複、enqueue 失敗時に消えない tombstone、シャットダウン時の barrier 欠落、無関係な flush の debounce まで崩す barrier (`FireMatching`)、fsync が ticker の lock を待つ (per-event の lock)。**残る既知の制約**: tombstone がある間の readdir は呼び出しごとにディレクトリを走査する (10,000 件で数百 ms 見込み、未計測)。|
| AC-86 | rename した、まだ一度も読んでいないファイルを open すると EIO (s3fs-fuse は新しい名前なら内容、古い名前なら ENOENT)。`test-parity` の `rename_open_new` / `rename_open_old`。`open_check` の 0 / 1 に依らず出る。TLA+ の反例 1: 作成して flush した ino も mirror が学ばない (`ReadOK`、`docs/design/file-lifecycle-state-machine.md`)。修正案 `RecordKeys` (flush / rename が mirror も更新する) はモデルで確認済み。原因の見立て: `handleRename` が S3 の Rename と `registerIno` だけで、daemon の entry mirror (`fpebpf` の `S3Key`) を更新しないので、read miss の GET が削除済みの旧 key に行く (`open_check` の `localKeys` は新 key を持つが、read の経路は使っていない) | P1 | ✅ 修正 (2026-11-05)。flush / rename が daemon の entry mirror を更新する (`fpebpf.RecordKey`、`handleFlush` / `handleRename`)。open_check 用の `localKeys` は mirror に一本化して削除。回帰: `TestOpenCheckUsesRecordedKeys`、`make test-parity` (`rename_open_new` が s3fs-fuse と一致、修正前は EIO)。**旧名の open は AC-85 (別の原因)** |
| AC-87 | upcall の待機がシグナルで中断されると、cold の read が -EIO になる (`read_folio` の `wait_for_completion_interruptible_timeout` が負の値を返しても、`> 0` にも `== 0` にも当たらず、chunk のない fallback fill に落ちた)。Go の runtime は SIGURG を数 ms ごとに送るので、`dev microbench --op=read` を `nocache=1` で回すと 100 回前後の read で必ず失敗する。Phase 3b までは nocache の `.open` が chunk を残して fallback が拾い、見えなかった。3c (#55) で `.open` が chunk を解放して顕在化し、master にマージされていた。`br §18.54` | P1 | ✅ 修正 (`a33f10f`、待機を killable に。回帰テスト `test-nocache-signal` は修正前の master で warmup #91 に EIO で落ちる)。nocache 以外の cold read が同じ経路で失敗するかは再現を確かめていない |
| AC-88 | v2 再設計の後に、小さいファイル 10,000 個の cold cat (E) が 6.8 → 8.3 秒 (+22%) と遅くなった (ebpf-pcache / nocache / pcachedrop の 3 構成、fuse の対照は同じ)。S3 の HEAD / GET (各 10,000) と upcall の数は同じで、daemon の handler の平均が約 600 → 約 800 µs。二分探索で境目は `db42b4e` (BPF の ringbuf と daemon の `ino=1` の warm の削除)。`684cd5d` に「ringbuf を読む goroutine だけを外す」変更を入れると同じ遅さになる。fentry が未知の ino ごとに投げた event で daemon が起こされていたのが、upcall の往復の wake-up を速くしていた、という読み (perf に `pv_native_safe_halt`) だが**未確認**。`drop_caches` と `tee` を入れない手動の手順では差が出ない。`br §18.54` | P2 | ✅ 解決 (staging): E の cat-cold が 8.6 s → 2.2 s (4.0×)。GET が handler の 89% だったので、名前順の miss で次の `depth` 個を先に並列 GET する (`docs/design/object-staging.md`、`br §18.57`)。haltpoll / 実機での確認は不要になった |
| AC-89 | capacity モードで 100 MiB を、容量 32 MiB で cold read すると 242〜252 ms で、evict なしの 140〜156 ms より +65% 遅い (chunk ごとに容量パスが走る分)。`br §18.55` | P3 | 🔶 窓を 3 に (式 cap/4MiB/2−1)、100 MiB / 32 MiB の cold read は無制限の +25〜30% (`br §18.56`)。**GET をまとめる案は 1 / 2 / 3 chunk とも遅くなった (§18.58)** ので、コードの変更はなし。これ以上は GET のまとめでは縮まない (別の手が要る: 容量内の先読みの上限を上げる = 容量の設計、または chunk の並列 upcall) |
| AC-90 | `s3fs_ebpf_capacity_pass` の時間が、キャッシュ済み inode の数に対して超線形: p50 は 100 件で 49 µs、1,000 件で 730 µs、10,000 件で 36.3 ms (100 倍で約 740 倍)。`sb->s_inodes` の収集か policy への受け渡しのどちらかが O(n²) 気味。`br §18.55` | P2 | ✅ 解決: 線形化 (10,000 件 36.3 → 4.5 ms)。`br §18.55` 続き |
| AC-91 | `scripts/bench-policy-cost.sh` が `set -e` のため、最初の arm (none) の後で終わる (2 つ目の arm の daemon が起動する前)。`br §18.55` | P3 | ✅ 解決: パイプに `|| true` |
| AC-92 | `test-policy-conformance` の wtinylfu が、v2 の 3e (A) の後、確率的に FAIL した (v2 の前は 0/10、3e の後は約 4〜5 割)。原因: evict した ino の page cache が `invalidate_mapping_pages` に飛ばされて取り残され、chunk がないので誰も落とさず、以後その ino が page cache から読まれ続ける。`br §18.55` 続き | P2 | ✅ 解決: 取り残された ino を `s3fs_pc_retry[]` に覚え、次の pass の先頭で落とし直す。conformance 12/12 が決定的、`test-regression` 全件 PASS |
| AC-94 | v2 Phase 4 (queue 統合): kmod → daemon の要求は 1 本の `reqq`、待ち手は 1 つの `s3fs_inflight`、完了は 1 本の `DONE` | P3 | ✅ 完了 (2026-11-10、`docs/design/redesign-v2-simplify.md` §9): ioctl 16 → 10。flush の枠 (F-1) は入れていない (名前が古いまま 1 件に畳まれる恐れ。モデルは enqueue ごとの message で検証)。残り: `flush_pins` の統合、Q-4 (device close で in-flight を失敗させる) |
| AC-95 | 旧プロトコルの `MARK_DIRTY` と `SIGNAL_FLUSH(err)` が別 ioctl で、間に close が割り込むと flushing=0 / dirty=0 で request だけが残る。その窓で read miss が古い S3 の内容で fd->buf を上書きすると、続く flush が古い内容を PUT しうる (モデル FE-7。実機では再現していない、窓は 2 つの ioctl の間) | P2 | ✅ `DONE(FLUSH, err)` が同じ `fd->lock` で dirty を戻して flushing を下ろす。KUnit `s3k_vfs_done_err_restores_dirty` |
| AC-96 | clean でも flush が飛行中の file への `fsync` が、S3 に届く前に 0 を返す (close(2) が flush を積んで dirty を落とし、別の fd の fsync は `!dirty` で即 return)。**実機で再現** (`make test-fsync-inflight`: 6/6 FAIL)。モデル FE-5 / FE-6 | P1 | ✅ fsync が待ち手を登録して自分の WAIT request を積む (`flush_seq++`)。`test-fsync-inflight` 6/6 PASS、KUnit `s3k_vfs_fsync_of_clean_file_waits_for_inflight_flush` |
| AC-93 | object staging の残り: ① 外部の更新は TTL 5 秒の間は古い写しを返しうる (daemon に通知がない)。ETag を listing から持って照合できるか ② 名前順以外 (ロケールの照合、`find` の readdir 順、ランダム) の予測 ③ 大きい directory での index の memory (50 万件で off) ④ `ls -l` の後の `cat` など、stat 済みの inode を使った先読み。`docs/design/object-staging.md` | P3 | ⏳ 未着手 |
| AC-97 | C の 10 MiB (既存 object の途中への 4 KiB + fsync) が fuse に負けていた (ebpf 57〜64 ms、fuse 44〜50 ms)。原因: load-for-write が chunk ごとの直列の upcall (10 MiB = 3 往復)、10 MiB の PUT が 1 本 | P2 | ✅ 修正 (`br §18.60`、`d547f74`): chunk の upcall を 8 chunk 先まで先に queue し、10 MiB 以上の PUT を並列 part にした。C 10 MiB は 63.2 → 50.8 ms (fuse と同等)。回帰: `make test-regression` 全件 PASS。**残り**: ① 並列 part と先行発行数の値を振っていない ② MinIO が遅い時間帯が連続して出る揺れの原因が未切り分け ③ `partial_modify_min` (16 MiB) を下げた測定が未実施 |

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
