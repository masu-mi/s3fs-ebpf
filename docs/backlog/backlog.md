# Backlog — プロジェクト残課題の一元リスト

各週次 note (`docs/weekN-*.md`) と設計 doc に散らばっている follow-up を 1 箇所に集約したもの。「今すぐ着手できる小改修」と「Week 5+ の別 track」を **優先度 × カテゴリ** で並べる。

**運用ルール**:
- 完了したら該当行に ✅ を付けて出典を追記する。完了して時間が経った項目は [`backlog-archive.md`](backlog-archive.md) に移す (ID と本文はそのまま)
- 2026-10-18: A〜AB 系と AC-01〜AC-49 の完了項目、優先度 view (旧 §7) を archive に移した。2026-11-17: AC-50〜AC-97 の完了分も archive に移し、ここには**開いている項目だけ**を残した (計測待ちの AC-66 / AC-75 を含む)。経緯は [`history.md`](../journal/history.md)
- 新規 follow-up は該当週次 note に書いてからここに 1 行反映
- 優先度は **P0 = 次スプリントで着手 / P1 = Week 5 で着手 / P2 = Meetup 後の polish** の 3 段

出典表記: `w4a` = `docs/journal/weekly/week4a-implementation-notes.md`、`w4b` = `docs/journal/weekly/week4b-implementation-notes.md`、`w4c` = `docs/journal/weekly/week4c-implementation-notes.md`、`w4ac` = `docs/journal/weekly/week4a-c-session-notes.md`、`ll` = `docs/dev-notes/lessons-learned.md`、`br` = `docs/journal/benchmark-results.md`、`be` = `docs/journal/bench-env-notes.md`、`pe` = `docs/design/policy/policy-engine-design.md`、`as` = `docs/design/policy/admit-and-softcap.md`、`hr` = `docs/journal/hit-rate-bench.md`、`mo` = `docs/user/meetup-ebpf-outline.md`。

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

**背景**: `docs/design/ebpf/bpf-arena-use-cases.md` のレビュー結果、arena 128MB をバルクページキャッシュとして使う現設計は arena の設計意図 (small, hot metadata) と不整合。改善案A (arena を admit メタデータだけに縮退、ページ実体は Linux page cache に委譲) が最有力。GUP オーバーヘッド (AA-01 残課題) と arena サイズ上限 (大ファイル劣後) の両方を同時解消する見込み。**実装前に 3 段階の検証** (Phase 1 分離測定 → Phase 2 PoC → Phase 3 本格計測) で判断する。

> **2026-10-21 訂正**: 実装 (pcache モード、`--arena-metadata-only`) では arena にメタデータも置かなかった。中身は page cache と kmod の chunk 表、admit のメタデータは kmod の admit hash、policy の状態は BPF map にある。daemon は arena の map を作って mmap するが、read 経路では使わない (`docs/user/meetup/meetup-qa.md` Q53)。

**Phase 1 — 現状の bottleneck 分離測定** (判断材料集め、2〜3 日):

- [ ] **AB-03** mount-s3 を同一ワークロードで動かし外部基準の 4-way 比較 — AA-06 の bench 延長で、特に 10MB / 100MB の cold-read を 3 回ずつ (fuse-raw / fuse-cache / ebpf / mount-s3)。eBPF の positive/negative 差分の内訳を明確化。目安 0.5 日 (**AB-01/AB-02 で Gate PASS 済みのため省略可、必要に応じて実施**)

### K. upstream 貢献 (`w4a §9`)

根拠 doc: [`../dev-notes/ebpf-ecosystem-gaps.md`](../dev-notes/ebpf-kernel/ebpf-ecosystem-gaps.md) — BPF_ARENA / STRUCT_OPS / KF_SLEEPABLE の踏んだ地雷を体系化。K-01/02 に加えて cilium/ebpf patch (`../misc/cilium-ebpf-addr-space-cast-marshal.patch`)、struct_ops template マクロ提案、KF_SLEEPABLE matrix doc 提案 (§2-§4) を並置。

- [ ] **K-01** `bpf_arena_get_kern_vm_start` の EXPORT_SYMBOL_GPL 提案 — 詳細 [`../dev-notes/ebpf-ecosystem-gaps.md §1.2`](../dev-notes/ebpf-kernel/ebpf-ecosystem-gaps.md#12-改善案)
  当てれば kmod 側で BTF-derived struct redefine 不要になる。
- [ ] **K-02** arena kern_vm range 用の generic-context fault handler 追加提案 — 詳細 [`../dev-notes/ebpf-ecosystem-gaps.md §1.2`](../dev-notes/ebpf-kernel/ebpf-ecosystem-gaps.md#12-改善案)
  当てれば A′ (kern_vm 直 deref) 経路が復活、B の GUP overhead が消える (数百ns オーダー改善見込み)。

### AD. FUSE に対する優位性の未実装項目 (`docs/design/ebpf/ebpf-fuse-advantages-roadmap.md §3`)

2026-10-18 登録。roadmap の初版は Q-01〜Q-04 と書いていたが、Q 系 (セキュリティ修正、archive) と重なるので AD に振り直した。順序と見積もりは roadmap §4。

- [ ] **AD-01** `bpf_timer` による自律 eviction (roadmap §3-2、旧 Q-01)。daemon の push を待たずに容量を守る基盤。目安 3〜4 日
- [ ] **AD-02** BPF Iterator による arena GC (roadmap §3-4、旧 Q-02)。R-05 と合流。目安 4〜5 日。2026-10-21 訂正: 「pcache モードでは arena がメタデータだけ」は誤りで、pcache モードは arena に何も置かない。GC が要るのは arena モード (中身を arena に置く、今の既定) だけなので、arena モードを残すかを決めてから着手するか閉じる
- [ ] **AD-03** KFunc + in-kernel radix tree による `readdir` 高速化 (roadmap §3-3、旧 Q-03)。AC-55 で SYNC_ENTRIES に索引が入ったので、残る差を測ってから判断する。目安 4〜5 日
- [ ] **AD-04** `fmod_ret` + `cgroup_id()` の per-cgroup policy (roadmap §3-1、旧 Q-04)。R-04 と合流。目安 4〜5 日

### R. 不足フック (`docs/design/ebpf/ebpf-hook-gaps.md §2.3 / §4`)

2026-10-18 登録。R-01 (`fentry/generic_file_read_iter`) は完了 (archive)。

- [ ] **R-02** `struct_ops` に `on_read_miss(ino, size, rand_ratio)` を追加し、miss の upcall 前に refuse できるようにする。R-01 のデータが溜まってから
- [ ] **R-03** `fentry/d_lookup` で lookup を観測し、ネガティブキャッシュの頻度を取る
- [ ] **R-04** `lsm/inode_permission` の per-cgroup policy (AD-04 と合流)
- [ ] **R-05** `iter/bpf_map_elem` の arena GC (AD-02 と合流。arena モードだけの話、AD-02 の訂正を参照)
- [ ] **R-06** `fentry/filemap_fault` で mmap のアクセスを観測する。read 経路のフックと同じく、hit 経路の負担は `docs/design/ebpf/read-iter-hook-overhead.md` §5 の基準で見る

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

## AC. 開いている AC 項目 (完了分は [`backlog-archive.md`](backlog-archive.md))

| # | 内容 | 優先度 | 状態 |
|---|---|---|---|
| AC-06 | P6: 64-job 律速の特定 (perf c2c / lockstat / generic_file_read_iter 比較) | P2 | ⏳ 未着手 |
| AC-66 | W-TinyLFU を AC-58 の `pick_victim` で動かす (window + SLRU + sketch + admission。populate 後の admission は「候補を victim にする」で表し、pending で shed になった ino は limbo で区画を覚える)。1 段目は件数ベースで kmod 変更なし、2 段目で vtable 末尾に `on_size(ino, bytes)` を足して予算をバイトにする | P2 | 実装 (2026-10-20、ライブ計測は未): `policy_wtinylfu_pick_ops`、kmod の `on_size`、trace-replay の `wtinylfu` / `wtinylfu-clock` / `s3fifo`、`make test-wtinylfu-harness` (verifier と Go の模型の一致)。trace-replay では 30 files / 容量 8 で S3-FIFO より上、大きい容量では 1〜4 ポイント下 (`docs/design/plans/wtinylfu-pick-plan.md` §9)。実機 (1 回): `make test-regression` 全 PASS、`bench-hitrate` で S3-FIFO 37.80% に対し 38.55% (§9.4)。残: bench-hitrate の 3 seed、mixed、policy-cost |
| AC-75 | daemon の `--cache-dir` (disk cache) が **4 MiB (`partialAdmitThreshold`) を超える object には効かない**: 大きい object は chunk ごとの `GetRange` (`serveChunkLoad`) で取り、`GetRange` は disk cache を読みも書きもしない (cache は object 全体がキー)。kmod の chunk 表から落ちると、`--cache-dir` があっても毎回 S3 に戻る。64 MiB の「disk cache からの再読み」は upcall 288 回に対して S3 GET 1136 回で、45.5 ms (daemon の処理 24.0 ms)。s3fs-fuse の disk cache は 64 MiB でも効き 26.2 ms (0.58×)。層ごとのレイテンシの層 3 の行の GET 数で発見 (`br §18.49`) | P1 | 🔧 実装済み・未計測。`s3.Client.GetRange` が ETag 既知のとき `(key, offset, size)` 単位で disk cache を読み書きする (If-Match で取るので、cache の chunk は header の ETag の bytes。`List`/`Stat` で ETag が変わると読まない、`Put`/`Delete`/`Rename` は ETag を忘れる)。書き込みは write-behind で、枠が埋まっていれば待つ (落とすと次の読みが S3 GET になるため)。単体: `TestDiskCache_Range`、MinIO 付き: `TestGetRange_DiskCache`。回帰テスト `TestGetRange_LargeObjectSecondReadNoGET` (64 MiB の 2 回目の S3 GET が 0 回) と仕様 `docs/design/disk-cache-range.md` を追加。**残り**: ① kmod 経由の実機確認 (`bench-layer-latency.sh` の層 3 の再計測)、② GCS の `GetRange` は generation 固定で対応 (2026-11-10、tag の単体テストのみ、実 GCS では未確認) |
| AC-69 | (future work) `admit_replace` が入れ替えのたびに admit 表の全 ino に `on_evict` (pick_victim 型では `on_insert` も) を呼び、容量を超えていない回も結果を捨てている。kmod の重複除去は線形探索で O(N²)。hit の read 経路ではなく、miss と sync の経路のコスト。計測: ino 数 100 / 1,000 / 10,000 で `admit_replace` 1 回の時間と BPF の合計、miss の latency。改善: 超過時だけ `on_evict`、`on_insert` は新しい ino だけ、重複除去をハッシュに。詳細 `docs/design/policy/ac58-pick-victim.md` §8 | P2 | ⏳ 未着手 (2026-10-26 登録、コードを読んだだけで未計測) |
| AC-73 | `test-policy-conformance` が環境のタイミングに敏感: ホストが遅いと s3fifo / wtinylfu の live hits が simulator より 3.5〜12 pt 多くなって FAIL する (変更前の master でも同じ)。通る / 通らないが環境次第だと、回帰として信用できない | P2 | ⏳ 未着手。試験の前に対照 (A の fuse-raw / ebpf-nocache の p50 など) で環境を確かめて SKIP にする、または sync / eval の round が access に対して決定的になるようにする (`SYNC_EVERY=3600s` は入っている) |
| AC-74 | 性能試験 H: ebpf-pcache の `observe_ops` (何も evict しない基準) の hit rate が seed 間で大きくばらつく (四分位 38.5〜44.8%、最小〜最大 29.9〜47.5%)。ebpf-pcachedrop では 27.2〜28.6% と安定。LRU は observe と +0.6 pp で差がない (SIEVE / S3-FIFO / W-TinyLFU は +10〜19 pp)。page cache のヒットが hit に混ざることと、policy が evict した ino の page cache を落とす処理の寄与を切り分けていない | P2 | ⏳ 未着手。ebpf-pcache で observe の seed ごとの hit を分解 (page cache hit / chunk 表 hit / upcall)、`hit_accum` の replay 数と evict された ino の page cache の drop 数を数える (`br §18.48`) |
| AC-79 | open の存在確認の頻度を下げる mode (**調査: 将来実装の可能性の判断**): `open_check` の毎回 HEAD (425 µs) と確認なし (3 µs) の間が未実装・未測定。候補 (A) 確認結果を ino ごとに `ttl_ms` だけ再利用する (保証は「TTL 以内の削除までは古い結果が見える」。s3fs-fuse の stat cache の期限と同じ種類)、(B) TTL 超過時は先に進めて非同期に再確認し、404 なら次の open から失敗させる (最初の 1 回が古い結果になり A より弱い)、(C) 変更の通知を受けた分だけ無効化する (AC-80) | P2 | ⏳ 未着手。計画: ① (A) を kmod の module param `open_check_ttl_ms` (0 = 毎回、既存の `open_check` と併用) として足す。ino ごとの最終確認時刻は `fd` (`s3fs_file_data`) に持ち、hit_accum と同様に lockless で読む。② TTL = 0 / 10 ms / 100 ms / 1 s で A (キャッシュあり同士) の p50 と silo への HEAD 数を測る (`br §18.52` の表を埋める)。③ 遠い S3 を `tc netem delay 12.5ms 2.5ms` (`br §18.50`) で模擬し、TTL ごとの効きを見る。④ 正確性: 削除してから TTL 以内の open が成功する窓を、回帰試験 (`test-open-check` の拡張) で上限つきで確かめる。不変条件 (`docs/kmod-pcache-invariants.md` の pending のキー) は変えない見込み |
| AC-80 | サーバー側の更新 (他のクライアントによる PUT / DELETE) を daemon から kmod に伝える経路 (**調査: 将来実装の可能性の判断**): 今は daemon の周期 sync (`--sync-every`) が entry table を丸ごと入れ替えるだけで、sync の間の削除は open_check の HEAD でしか分からない。候補: (a) bucket notification (MinIO / silo の webhook・queue) を daemon が受けて、該当 ino の確認済み印と chunk を無効化する、(b) arena (daemon と BPF の共有メモリ) に ino ごとの世代番号 (generation) を書き、kmod / BPF の hit 経路が open で読んで TTL 内でも古い結果を捨てる、(c) 周期 sync を差分にして ETag の変化だけを送る | P2 | ⏳ 未着手。計画: ① 通知の遅れと取りこぼし (at-least-once か) を silo で確かめる。取りこぼしても TTL (AC-79) で上限が付く構成にする。② arena に世代番号を書く経路の費用を測る (open の hit で 1 回読むだけなら 3 µs に足されるのは数十 ns の見込み、未測定)。③ AC-81 の policy 記述と合わせて、ポリシーごとに「通知を信じる / TTL / 毎回確認」を選べるかを判断する |
| AC-81 | open の確認と更新通知の方針を YAML などで宣言する policy (**調査: 将来実装の可能性の判断**): 今のキャッシュ policy は BPF struct_ops (`pick_victim` など、`docs/dev-notes/ebpf-kernel/struct-ops-data-members.md`) で eviction の順位だけを決める。「どの key (prefix / サイズ / 拡張子) は open で毎回 HEAD、どれは `ttl_ms` 秒、どれは通知を信じて確認しない」を、daemon が読む YAML で書き、daemon が kmod の module param / ino ごとの設定 (arena 経由) に落とす形が候補。例: `rules: [{match: {prefix: models/}, open_check: {ttl: 60s}}, {match: {prefix: tmp/}, open_check: always}]`。struct_ops の `flags` (AC-77) は eviction 側の宣言で、open の確認は別の軸 | P2 | ⏳ 未着手。判断の材料: ① AC-79 の TTL ごとの効き (遠い S3 で何割の open が HEAD を省けるか)。② ino ごとの設定を arena に置くときの kmod 側の読み方 (BPF の hot path に入れず、open の 1 回だけ読む)。③ YAML の変更を反映する経路は既存の policy の hot swap (`BPF_LINK_UPDATE`、`test-policy-reload`) に倣えるか。④ 効きが小さい (HEAD の割合が低い) なら実装せず、AC-78 の既定を決めて閉じる |
| AC-83 | `test-parity` (s3fs-fuse との差分テスト) を足したので、既知の差は `scripts/lib/parity-known-diff.sh` に理由つきで並べ、それ以外の差は FAIL にする。今の既知の差は AC-80 (外部の更新が次の sync まで見えない) と AC-86 だけ。**未カバーの操作**: 並行 open (同じ ino に複数の open_check)、大きいファイルの途中 write 後の read-back、readdir の分割読み、chmod / utimens / symlink、O_APPEND、sync を有効にした状態 (`--sync-every` 既定) での外部更新の遅れの上限 | P2 | ⏳ 未着手。操作を足すたびに差が出たら、直すか既知の差に理由つきで置く |
| AC-89 | **[2026-11-12 追記: 残りの +8〜11% の原因 (先読みが自己クロックでなく、hit で窓が進まなかった) を修正。32 MiB の cold read は無制限に対して約 +4% (`br §18.63`)。]** **[2026-11-11 追記: 先読みの窓の補充が 4 chunk に 1 回だった原因を見つけて修正。32 MiB の cold read は +25〜28% → +8〜11% (`br §18.62`)。]** capacity モードで 100 MiB を、容量 32 MiB で cold read すると 242〜252 ms で、evict なしの 140〜156 ms より +65% 遅い (chunk ごとに容量パスが走る分)。`br §18.55` | P3 | 🔶 窓を 3 に (式 cap/4MiB/2−1)、100 MiB / 32 MiB の cold read は無制限の +25〜30% (`br §18.56`)。**GET をまとめる案は 1 / 2 / 3 chunk とも遅くなった (§18.58)** ので、コードの変更はなし。これ以上は GET のまとめでは縮まない (別の手が要る: 容量内の先読みの上限を上げる = 容量の設計、または chunk の並列 upcall) |
| AC-93 | object staging の残り: ① 外部の更新は TTL 5 秒の間は古い写しを返しうる (daemon に通知がない)。ETag を listing から持って照合できるか ② 名前順以外 (ロケールの照合、`find` の readdir 順、ランダム) の予測 ③ 大きい directory での index の memory (50 万件で off) ④ `ls -l` の後の `cat` など、stat 済みの inode を使った先読み。`docs/design/object-staging.md` | P3 | ⏳ 未着手 |
| AC-98 | 公式相当の .deb (Debian / Ubuntu 公式、PPA、社外向け apt リポジトリ) は未対応。今の `make deb` は手元・社内向けで、`debian/`・lintian・クリーンビルド・DKMS・ライセンスが無い。足りないものと作業順は `docs/dev-notes/process/deb-packaging-official.md` | P3 | ⏸ **条件付き保留 (着手しない)**。次のいずれかが起きたら着手: ① 社外 (他組織・一般) に配ると決めた ② `LICENSE` が決まった ③ 対象ディストリの標準カーネルが 6.12 以上になった (kmod の下限) ④ 利用者が DKMS / apt リポジトリを求めた。着手時は文書の §6 の順で。確認: `debian/` を作ってクリーンな chroot でビルドが通るか、lintian 警告が 0 か |

### AC-06 調査計画 (未着手)
- `perf c2c record -a -- fio ...` (64 jobs, shared small file) でキャッシュライン競合の特定
- `perf record -g` + `lockstat` でロック競合の確認
- 比較対象: `.read_iter = generic_file_read_iter` 直結版 (admit 判定なし) での IOPS 計測

## 8. 参照

- `docs/journal/weekly/week4a-implementation-notes.md §9` / `§10`
- `docs/journal/weekly/week4a-c-session-notes.md §5`
- `docs/journal/weekly/week4b-implementation-notes.md §4`
- `docs/journal/weekly/week4c-implementation-notes.md §4`
- `docs/dev-notes/lessons-learned.md §5`
- `docs/journal/benchmark-results.md §6`
- `docs/journal/bench-env-notes.md §4`
- `docs/design/policy/policy-engine-design.md §6` / `§7` / `§9`
- `docs/design/policy/admit-and-softcap.md §4` / `§5` / `§7` (S 系)
