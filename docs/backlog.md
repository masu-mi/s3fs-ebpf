# Backlog — プロジェクト残課題の一元リスト

各週次 note (`docs/weekN-*.md`) と設計 doc に散らばっている follow-up を 1 箇所に集約したもの。「今すぐ着手できる小改修」と「Week 5+ の別 track」を **優先度 × カテゴリ** で並べる。

**運用ルール**:
- 完了したら該当行に ✅ を付けて (取り除かない) 出典を追記
- 新規 follow-up は該当週次 note に書いてからここに 1 行反映
- 優先度は **P0 = 次スプリントで着手 / P1 = Week 5 で着手 / P2 = Meetup 後の polish** の 3 段

出典表記: `w4a` = `docs/week4a-implementation-notes.md`、`w4b` = `docs/week4b-implementation-notes.md`、`w4c` = `docs/week4c-implementation-notes.md`、`w4ac` = `docs/week4a-c-session-notes.md`、`ll` = `docs/lessons-learned.md`、`br` = `docs/benchmark-results.md`、`be` = `docs/bench-env-notes.md`、`pe` = `docs/policy-engine-design.md`。

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
| M7 | Week 4 (d) policy engine 実装 | ⏳ 未着手 | M6 の Q 回答後、CEL + built-in eBPF policy の spike |
| M8 | Meetup 登壇 | ⏳ 準備中 | narrative は `motivation.md` + `benchmark-results.md` で揃っている、demo 台本のみ |
| M9 | Week 5+ 分散 / multi-tenant / ML | ⏸ scope 外 | 現行 architecture の限界を洗い出してから (§B/C の項目参照) |

---

## 2. P0 — 次スプリントで着手すべき小改修

「1〜2 日でカタが付き、放置すると実運用で刺さる」もの。

### A. FS 機能 (correctness / user-visible)

- [x] ✅ **A-01** daemon exit 時に SYNC clear ioctl を送る (`w4b §4-1`) — 完 (次 commit)
- [x] ✅ **A-02** admit の cleanup を sync delta と連動 (`w4b §4-2`) — 完 (次 commit)、`pruneAdmitsTo`
- [x] ✅ **A-03** flush PUT 失敗時に dirty flag を戻す (`w4c §4-2`) — 完 (次 commit)、新 ioctl `S3FS_IOC_MARK_DIRTY`
- [x] ✅ **A-04** warmFromS3 の "no S3 key mapping yet — Week 3" ログを削除 (`w4ac §5-2`) — 完 (次 commit)

### B. bench / 環境の使い勝手

- [x] ✅ **B-01** microbench 100k iters が s3fs-fuse で 8 分 (`be §4-1`) — 完 (次 commit)、default 100000 → 5000
- [x] ✅ **B-02** bench 中 SIGINT で microbench が panic (`be §4-1`) — 完 (次 commit)、`mustOp` → `doOp` (error return)
- [x] ✅ **B-03** fuse mount が busy で umount 失敗する対策 (`be §4-2`) — 完 (次 commit)。`scripts/lib/umount.sh` に `umount_hard` を導入 (normal umount → EBUSY 時 `umount -l` fallback + stderr log)。bench-latency / bench-write / bench-3way / bench-scale / bench-hitrate の trap 内 umount を全て helper 経由に統一。tmpfs で busy 状態を再現し fallback → detach、非 mount パスの silent return、通常 umount の silent success を smoke で確認
- [x] ✅ **B-04** `bench/runner.sh` + `bench/fio/*` + `bench/bcc/*` を削除 (`be §4-5`) — 完 (次 commit)、Makefile bench-dry も同時削除
- [x] ✅ **B-05** README.md 再生成 (`be §4-5`) — 完 (次 commit)

### C. Grafana / metric surface

- [x] ✅ **C-04** kmod folio hit counter — 完 (次 commit)。`atomic_long_t s3fs_folio_hits` を read_folio の admit-hit 分岐で increment、`S3FS_IOC_STATS[1]` で export、daemon が `s3fs_ebpf_folio_hits_total` として mirror。Grafana panel 20/21 に true hit ratio (folio_hits / (folio_hits + read_miss_upcalls)) を stat + rate timeseries で追加。fentry-side fast_path_hits (getattr) と kmod-side folio_hits (実 read cache) を切り分けて見えるように。smoke で drop_caches 後の 5 read → folio_hits 3→8、upcall 0 で完全 arena hit を実測

- [x] ✅ **C-01** MinIO metric 名 version 依存の確認 (`be §4-3`) — 完、`minio_s3_traffic_{received,sent}_bytes` は現行 MinIO で存在確認 (dashboard 変更不要)
- [x] ✅ **C-02** dashboard `rate(...[30s])` の window を `[1m]` に統一 (`be §4-3`) — 完 (次 commit)。`s3fs-ebpf-overview.json` の 14 箇所 (`sed 's/\[30s\])/[1m])/g'`) を一括置換。5s scrape × 12 サンプル/1m で曲線が安定
- [x] ✅ **C-03** flush errors panel を stat + threshold に (`be §4-4`) — 完 (次 commit)。旧 "Flush events / errors" (panel 8) を "Flush events" (w=8 timeseries) と "Flush errors (last 5m)" (w=4 stat, `increase(...[5m])` + green<1 / red≥1 threshold, `colorMode=background`) の 2 panel に分割。0 が地味に見えないので `> 0` で赤く光る運用に。Grafana API で id=23 が provisioned、version=8 まで bump 済

---

## 3. P1 — Week 5 で着手 (機能拡張)

「架け替えが要る、または 1 週分の作業」もの。

### D. 大 file / 部分 read 対応 (`ll §5.1`)

- [x] ✅ **D-01 (基盤)** admit table range list 化 — 完 (次 commit)。`struct s3fs_admit_entry` に `offset` u64 field を追加、kmod の同名 struct + Go 側 `admitEntry` も並行拡張。`admit_lookup_at_locked(ino, pos)` を新設して `[offset, offset+size)` に pos が入る range を返す方式に、`admit_snapshot()` は range 相対の uaddr + avail を返す形にリファクタ。`.read_folio` は fill_from_arena を pos=0 で呼び出す (range-relative 展開)。同一 ino に対して複数エントリ (hash bucket 内衝突) が持てるようになったため 100 MiB+ blob の partial admit が可能に。i_size は sync entry 由来のみに簡略化 (partial range の size を全 file size と誤認しないため)。backward compat: offset=0 & size=full_file の whole-file admit がそのまま働く
- [x] ✅ **D-01 (実 partial admit)** daemon 側 partial GET — 完 (次 commit)。`s3.Client.GetRange(ctx, key, offset, size)` (minio-go の Range header) を追加、`handleReadMiss` が `entry.Fallback > partialAdmitThreshold=4MB` の場合 4 MB chunk 境界に align して range GET、admit を `Offset=chunkOffset` で登録。smoke: 50 MB `hugeblob.bin` を prewarm 対象外にして、offset 25 MB での dd read が range [25165824,29360128) を partial admit、SHA が原本と MATCH ✓ を確認
- [x] ✅ **D-02 (arena free-list)** arena bytes の再利用機構 — 完 (次 commit)。`Programs.arenaFree []arenaChunk` を追加、`AdmitContent` が (1) 最小 first-fit で free chunk 再利用 + 余りを free-list に戻す、(2) 見つからなければ bump allocator へ、の 2 段選択に。`FreeArenaChunk(offset, size)` API を export、`dropAdmits` が evicted admit の (uaddr - arenaBase, size) を free-list に戻す。metrics に `arena_free_bytes` / `arena_free_chunks` gauge 追加。smoke: 36 admits evict → free_bytes=10,487,187 / free_chunks=36、続く hitrate_5.txt (44B) 新規 admit → free_bytes -44 / chunks -1、arena_used_bytes は変化なし = **高水位を伸ばさず free chunk 再利用** を実測。**sliding window の "free 部分再利用" が動作**、arena が monotonic だった問題は解消。adjacent-chunk merge (compaction) は followup

### E. write path 強化

- [x] ✅ **E-01** `.fsync` を blocking にする — 完 (次 commit)。read-miss と同 pattern (`struct s3fs_flush_pending` + `wait_for_completion_interruptible_timeout` + `S3FS_IOC_SIGNAL_FLUSH`) で fsync が daemon PUT 完了まで blocking 化。`.release` は fire-and-forget のまま (close(2) を hang させない)。daemon は PUT 各 return path で signal_flush ioctl、-ENOENT は fire-and-forget path として silent 扱い。smoke で 27B write → fsync=91ms (blocked)、no-op fsync=8μs (dirty=false 早期 return) の 10000× 差を実測。3 mode 化 (eager / sync-fsync / sync-write) は未実装、現在 sync-fsync のみ POSIX 相当
- [x] ✅ **E-02** `.unlink` + `.rename` 対応 — 完 (次 commit)。unlink 側は kmod に delete queue + S3FS_IOC_POP_DELETE (0x0B)、`.unlink` op が local remove + admit drop + enqueue + drop_nlink、daemon runDeleteLoop が s3c.Delete。rename 側は同 pattern で rename queue + S3FS_IOC_POP_RENAME (0x0C) + `.rename` op (RENAME_NOREPLACE のみ、cross-dir EXDEV)、daemon runRenameLoop が s3c.Rename (server-side CopyObject + Delete)、ino → S3Key mapping 更新。smoke で unlink (touch+fsync → S3 に landed → rm → S3 から消失) + rename (before.txt → after.txt を S3 側で server-side 移動) の両 E2E path 検証
- [x] ✅ **E-03** flush coalescing — 完 (次 commit)。daemon 側 per-ino debounce coalescer (default 200 ms) を追加、rapid open+write+close 連打を 1 PUT にまとめる。UAPI struct s3fs_flush_event の空き _pad を `flags` に流用、`S3FS_FLUSH_FLAG_WAIT` bit で fsync 経路 (blocking waiter) を marking、daemon はこの flag で coalescer bypass + `CancelIno` により pending の古い event を破棄 (stale event が fsync bytes を上書きするのを防ぐ)。coalesced/fired カウンタを /metrics に追加 (差 = "PUTs saved"). smoke で 5× write+close → 6 event coalesce → 1 PUT (`iter 4` = latest wins)、fsync + close → immediate PUT (`fsync data preserved` 保持) を両方実測

### F. dynamic sync 強化 (`w4b §4`)

- [ ] **F-01** prewarm を sync 時にも回す (`w4b §4-3`)  
  現在 startup のみ。新 entry の初回 cat が 30ms Path C になる。runSyncLoop に `--prewarm-new-entries` オプション。
- [ ] **F-02** basename 衝突対応 (`w4b §4-4`)  
  `a/foo.txt` と `b/foo.txt` が同 name で衝突。dir 対応 (下記 G-01) と一括で。
- [ ] **F-03** BPF `inode_index` と sync の連携 (`w4b §4-5`)  
  fentry の fast_hits stub が prewarm 済 entry しか書かれない。sync round で `progs.WarmCache` を呼ぶ。tag を「metadata warm」と「content admit」で分離。

### G. VFS 拡張

- [ ] **G-01** ディレクトリ (subdir) 対応 (`w4b §4-4`)  
  現在 root 直下のみ。entry table に mode で `S_IFDIR` を持たせ、`s3fs_ebpf_make_inode` を dir 分岐、`s3fs_ebpf_dir_iops` を各 dir inode に付ける。sync entry の shape に `parent_ino` 追加。
- [ ] **G-02** `.setattr` に mode / uid / gid 対応  
  現状 ATTR_SIZE のみ。chmod / chown を通したい。POSIX 志向でなくても metadata 保持は S3 tag に載せる余地あり。

### H. 環境 lifecycle / 信頼性

- [ ] **H-01** kmod 再起動時の local additions 永続化 (`w4c §4-4`)  
  現状 rmmod で local additions が消える。application が open 中に rmmod されるとファイルが消える。永続化 (ファイル or /var/lib) を検討。
- [ ] **H-02** ino allocator の永続化 (`ll §5.3`)  
  daemon 再起動で S3Key ↔ ino がリセット。application が pid + ino で lock を識別するとその瞬間に壊れる。`/var/lib/s3fs-ebpf/inos.json` に atomic write + lock。
- [ ] **H-03** arena gc (`w4c §4-6`, `ll §5.4`, `be §4-1`)  
  AdmitContent の high-water offset が monotonic 増加。「差し替えられた古い arena offset」を daemon 側で free list に戻す、または arena reset の periodic 実行。
- [x] ✅ **H-04** admit の snapshot re-push が atomic でない (`w4ac §5-3`) — 完 (次 commit)。`admitMu` を ioctl 完了まで保持する single-writer 制約に、`setAdmits` / `replaceAdmit` / `refreshAdmits` / `clearAdmits` / `dropAdmits` / `pruneAdmitsTo` の全 6 mutator を refactor。snapshot copy も撤去 (lock を持っている間は直接 admits slice を ioctl に渡せる)。`admitArena` を package var 化してテストで capturing fake に差し替え、`TestReplaceAdmit_ConcurrentPushIsSerialized` (N=64 goroutine × replaceAdmit を 5 count 実行、`-race`) で "全 push が admitMu 保持下" + "snapshot サイズ単調増加" + "最終 admits に N ino 全部揃う" を pin
- [x] ✅ **H-05** daemon miss reader の SIGINT clean shutdown (`w4ac §5-4`) — 完 (次 commit)。`runReadMissLoop` の shutdown goroutine に `readMissShutdownWarn=5s` watchdog を追加、`f.Close()` 後に main loop が 5s 以内に return しない場合 stderr に SHUTDOWN WARN を吐く (kmod .release 側のバグを observable に)。`TestOSFileCloseWakesBlockedRead` (os.Pipe pair) で "blocked Read が Close 500ms 以内に io.EOF/os.ErrClosed で wake" を回帰テストで pin
- [ ] **H-06** pending の shutdown race (`w4ac §5-5`)  
  refcount 設計はテスト範囲で問題ないが、極限並行下で `hash_del` と complete のタイミング race。stress test で verify。

---

## 4. P1 — Week 4 (d) policy engine 実装 (`pe §6 §9`)

design doc (`pe`) は完成、CEL + built-in eBPF + user eBPF + KFunc の 3 層 architecture 決定済。実装は下記の順で:

- [x] ✅ **P-01** `pe Q3` CEL syntax spike — 完 (commit 8cbc8d1)。`daemon/internal/policy/` + `policies/default.cel` を追加、`--policy` flag、prewarm hook が entry サイズ / 名前で判定
- [x] ✅ **Layer 2 wiring** (P-01 の後続) — 完 (次 commit)。kmod に `s3fs_policy_on_admit` fentry anchor、`bpf/policy_observe.bpf.c` を出荷、daemon `--policy-bpf=builtin:policy_observe` で load + attach、policy_stats を /metrics に mirror
- [x] ✅ **Layer 3 wiring** (P-01 の後続) — 完 (次 commit)。`--policy-bpf=file:<path>` で任意 .bpf.o を load、SEC 命名で attach、smoke で /tmp/user_policy.bpf.o の load + counter 増加を確認
- [x] ✅ **P-02 (basic)** hot reload endpoint + in-flight semantics — 完 (次 commit)。POST /policy/reload 追加、新 program を先に attach → 旧 close の順、kmod unreg guard で "same vtable のみ clear" にして空 vtable 状態を防止。smoke で S3-FIFO → LRU が atomic swap、curl 一発で切替できる Meetup demo 用素材が揃った。P-02 の残: link.Update ベース (現状 close+re-attach、bpf_link_update 対応は cilium/ebpf 側 struct_ops 制限)
- [x] ✅ **P-03 (part)** CEL Layer 1 → Layer 2 program select — 完 (次 commit)。`policy: "builtin:policy_s3fifo_ops"` を CEL 側に書いて daemon が起動時に eval、`--policy-bpf` flag より優先で attach。1 .cel file で prewarm + policy 選択の統合が実現。`metric()` CEL 拡張関数は残
- [x] ✅ **P-03 (rest)** CEL `metric()` 拡張 — 完 (次 commit)。`metric(name, value) → bool` を CEL env に register。value は int/uint/double を受けて float64 に coerce、name は最大 CustomGaugeCap=32 個で cardinality bounded。gauge は `s3fs_ebpf_policy_metric{name="<name>"}` として /metrics に emit、caller は && chain で side-effect として組めば良い。`policies/default.cel` に例として `metric("last_entry_size", entry.size)` を追加、smoke で `s3fs_ebpf_policy_metric{name="last_entry_size"} 44` を実測
- [x] ✅ **P-04** trace harness (offline simulator) — 完 (次 commit)。`daemon/internal/trace/` に JSON Lines trace format + Zipf generator + strict LRU / paper-SIEVE の Go simulator を実装、`s3fs-ebpf trace-replay` CLI で ms-scale の policy 比較が可能に (200 events で 1ms、10000 events で 9ms、bench の 30s より 3000× 高速)。docs/trace-harness.md に format spec + sim ≠ BPF の divergence 説明 (LRU: sim 40% vs bench 7.5%、SIEVE: sim 45% vs bench 66%) + roadmap を書き起こし。kmod-side ring buffer recorder は P-04 followup として残り
- [x] ✅ **P-05** user eBPF 署名検証 — 完 (次 commit)。`daemon/internal/policyprog/allowlist.go` に SHA256 allowlist reader + `Verify()`。`--policy-allowlist=<path>` flag、無指定は permissive dev mode、指定時は strict enforcement (未 listed の `file:` 系 load を 403 reject、`builtin:` は bypass)。startup 時の allowlist load 数を stdout に、match / miss を stderr にログ。smoke で unlisted rejection と listed acceptance の両 path 検証。`policies/allowlist.example.txt` template 追加
- [x] ✅ **P-05 (CLI)** operator UX 補完 — 完 (次 commit)。`s3fs-ebpf hash-policy <path>...` subcommand を追加、`sha256sum` 互換 output format で allowlist.txt に直接 append 可能。`policyprog.HashFile()` を wrap するだけの薄い CLI
- [x] ✅ **P-06 (S3-FIFO 部分)** built-in policy として S3-FIFO を出荷 — 完 (commit 6c29b2e / c62d8dc)。`bpf/policy_s3fifo.bpf.c` (fmod_ret) + `bpf/policy_s3fifo_ops.bpf.c` (struct_ops) の 2 版
- [x] ✅ **P-06 (LRU 部分)** LRU-like (admit-order FIFO) を追加 — 完 (次 commit)。`bpf/policy_lru_ops.bpf.c` に struct_ops で。global epoch counter + per-ino seed で age-based eviction (LRU_CAP_AGE=3)。同 commit で kmod call site の bug fix (struct_ops on_admit/pressure が呼ばれていなかった)。smoke で "struct_ops voted evict on 11/14 entries" 実測。W-TinyLFU / SIEVE も同 template で追加可能
- [x] ✅ **P-06 (SIEVE 部分)** SIEVE (NSDI 2024) を追加 — 完 (次 commit)。`bpf/policy_sieve_ops.bpf.c` に struct_ops で。per-ino 1-bit visited + second-chance clock semantics: first sight → seed=1 (grace)、on_evict で visited=1 → clear to 0 (second chance)、visited=0 → EVICT。smoke で round-by-round 挙動を実測 (round 1: 11 admit → 0 vote, round 2: 12 admit → 0 silent vote, round 3: 13 admit → 12/13 evict)。W-TinyLFU は P-07 の hit-rate 比較後に追加
- [x] ✅ **P-14 on_hit hook** — 完 (次 commit)。vtable に 4 番目のメソッド `on_hit(ino)` 追加、`read_folio` の admit-hit 分岐から fmod_ret + struct_ops 両 path で dispatch。SIEVE: visited=1 復活、LRU: seed=current_epoch (age reset)、S3-FIFO: freq++ (up to 3)、observe: PSTAT_INVOCATIONS 加算。smoke で SIEVE の on_hit → 次 evict round で visited=1 → clear+keep、その後の cat 無ければ visited=0 → EVICT を alternating pattern (13→2→13→2→3→14→2→5) で観測。cat 継続の hello.txt は 4 round evict 生存、cat 停止した他 ino は round 3 で evict 実測
- [x] ✅ **P-07** hit-rate 比較 bench — 完 (次 commit)。`scripts/bench-hitrate.sh` + `POST /policy/eval` endpoint + kmod-side mm ref fix + `docs/hit-rate-bench.md`。Zipf α=0.7 over 30 files で SIEVE 62.5% / LRU 12% / S3-FIFO 9.5% (observe baseline 100%) 実測、SIEVE が最も working set を守れる証拠。P-14 on_hit で算法差が数値に反映されたことを確認
- [x] ✅ **P-15 rename freq2bit + full 3-tier S3-FIFO** — 完 (次 commit)。旧 policy_s3fifo は tier 分離のない 2-bit counter で S3-FIFO 論文と別物だったため `policy_freq2bit(_ops)` に honest rename。新規 `policy_s3fifo_ops.bpf.c` を Small/Main/Ghost 3-tier + freq counter + ghost promotion で論文準拠に実装。同 workload で S3-FIFO 61.5% (旧 freq2bit の 8.5% から 7× 改善)、SIEVE 68.5% と ±10% 内に収まり論文と整合
- [x] ✅ **P-06 (W-TinyLFU 部分)** W-TinyLFU の scoped-down 実装 — 完 (次 commit)。`bpf/policy_wtinylfu_ops.bpf.c` に CMS (4 hash × 256 slot、4-bit counter) + threshold admission。paper の {Window LRU tier, Doorkeeper, Aging} は omit — aging は BPF verifier が 1024-slot loop を reject するため (bpf_loop 未使用)。同 bench で 25%、freq2bit/LRU (~12%) と SIEVE/S3-FIFO (~62%) の中間に位置、full impl なら 60% tier 到達見込み。bench harness に `INTERLEAVE=25` (workload 中の POST /policy/eval) を追加 — on_hit で state populate する policy が eviction round を見られるようにした
- [x] ✅ **P-06 (W-TinyLFU aging)** bpf_loop() で CMS aging を復活 — 完 (次 commit)。`age_slot_cb` callback + `bpf_loop(CMS_W*CMS_D, cb, NULL, 0)` で verifier の 1M insn budget を回避 (kernel 5.17+ 依存)。AGE_INTERVAL=4 admit round ごとに全 counter を >>=1。同 bench で **25% → 80%** に跳ね上がり、SIEVE (62%) / S3-FIFO (65.5%) を上回って evicting policy 中の best に到達。paper 準拠の TinyLFU に一気に近付いた。W-LRU tier と Doorkeeper は残るが CMS+aging が主役の証明が取れた
- [x] ✅ **P-06 (W-TinyLFU 完全)** Window LRU tier + Doorkeeper 追加 — 完 (次 commit)。`bpf/policy_wtinylfu_full_ops.bpf.c` に paper 準拠 4 段 (Window + Doorkeeper + CMS + Aging) 実装。per-ino tier state (W/M)、4 hash Bloom filter DK、DK reset を CMS aging から decouple。**bench 結果は 49.5% と scoped 88.5% から regress** — Window tier が forced eval で items を evict、doorkeeper が small workload の useful signal を filter する paradox。docs/hit-rate-bench.md に "The W-TinyLFU paradox" 節を追加し、paper-complete ≠ best on this bench の workload sensitivity を明示。scoped が default、full は 参照実装として並置
- [x] ✅ **P-08** KFunc set の 1 次実装 — 完 (次 commit)。`s3fs_kfunc_admit_count()` (現 admit hash size)、`s3fs_kfunc_softcap()` (kmod-side const 8) を TRACING + STRUCT_OPS + SYSCALL prog types に register。kmod に `atomic_t s3fs_admits_count` を導入し admit_replace / evict path で updates。`bpf/policy_pressure_ops.bpf.c` を demo として追加 — pressure_ratio ≥ 1.5x で SIEVE の second-chance を skip する adaptive 版。bench で 52% hit + 30 admits → 19 evict (アクティブに aggressive 停止する挙動を dmesg で観測)
- [x] ✅ **P-08 拡張** KFunc set 追加 — 完 (次 commit)。`s3fs_kfunc_evictions_total()` (累計 eviction 数、atomic_long_read) と `s3fs_kfunc_time_ns()` (ktime_get_boottime_ns) を追加、TRACING + STRUCT_OPS の両 register セットに揃える。`bpf/policy_time_decay_ops.bpf.c` を time-based decay LRU demo として追加 — per-ino last_hit_ns を on_hit で更新、on_evict で `now - last_hit > 5s` なら vote evict。smoke で 35 admit → 7s 後の eval round で 35/35 全て time decay 発動を dmesg 確認
- [x] ✅ **P-08 拡張 (ino_size)** per-ino KFunc の 1 個目 — 完 (次 commit)。`s3fs_kfunc_ino_size(u64 ino)` が admit hash lookup で size を返す。`kmod/vfs_ops.c` に `s3fs_admit_size()` helper (spinlock 保持中の bucket walk) を切って exposed function として提供、tree_traversal.c の KFunc がそれを wrap。struct_ops + tracing の両 set に register。`bpf/policy_size_aware_ops.bpf.c` を demo として追加 — SIEVE 相当だが SMALL_THRESHOLD=4KB 未満のみ通常 evict、以上は BIG_MISS_CAP=2 rounds のグレース。verifier 通過 + 35/35 evict (44B files で small path) を smoke 確認
- [x] ✅ **P-09** eviction hook (on_evict / on_pressure) を kmod に追加 — 完 (commit 6c29b2e)。`s3fs_policy_on_evict(ino)` / `s3fs_policy_on_pressure(count, threshold)` の 2 hook を追加、admit_replace が毎 entry / capacity 越えで発火。BPF 側は evict_vote map に vote 書込 (verifier が fentry return を 0 固定強制するため side channel)。daemon-side vote 消費は future work
- [x] ✅ **P-10 fmod_ret 化** — 完 (次 commit)。register_btf_fmodret_id_set で hook を fmod_ret 対象に、`return 1` が kmod call site の retval に naturally 反映。side-channel map 削除、smoke で "policy voted evict on 11/12 entries" 観測
- [x] ✅ **P-11 struct_ops 化** — 完 (commit c62d8dc)。`struct s3fs_policy_ops` を kmod に定義 + `register_bpf_struct_ops`、BPF は SEC(".struct_ops.link") vtable で書く。rcu_dereference で atomic hot swap。smoke で "struct_ops voted evict on 11/12 entries" 観測。fmod_ret と struct_ops の比較 doc は docs/policy-attach-comparison.md
- [x] ✅ **P-12 vote 消費 (actual eviction)** — 完 (commit 8563639)。admit_replace が struct_ops eval_evict の retval を消費し、softcap 超過時に admit hash から実 hash_del。s3fs_evictions_total atomic counter + S3FS_IOC_STATS ioctl で daemon に export、metrics.PolicyEvictionsTotal → Prometheus。smoke で 11 entries が softcap=8 で evicted、`s3fs_ebpf_policy_evictions_total 11` を実測
- [x] ✅ **P-13 eviction feedback loop** — 完 (次 commit)。kmod に S3FS_EVICT_LOG_CAP=512 の ring buffer + S3FS_IOC_DRAIN_EVICTIONS ioctl、daemon の drainEvictionsLoop が 2s ごとに読んで admits snapshot から drop。oscillation (daemon 再送 → kmod 再 evict) 解消、smoke で kmod 9 evict → daemon `[evict-drain] dropped 9 admits fed back` → `admits_current 12→3` 実測

---

## 5. P2 — Meetup 後の polish

「面白いが後回し」もの。

### I. bench 追試 (`br §6.2`, `be §4`)

- [ ] **I-01** Mountpoint for Amazon S3 (`mount-s3`) を三巴に追加
- [ ] **I-02** `drop_caches=1` で純 read-cold の比較 (現状 `drop_caches=3` は inode 全部飛ばして path lookup が支配的)
- [ ] **I-03** cold-cache 分離 (daemon 生存 + kmod 生存で page cache だけ落とす vs 全部落とす)
- [ ] **I-04** fuse-cache を bench-latency にも追加 (現状 bench-3way のみ)
- [x] ✅ **I-05** latency histogram を Prometheus に export — 完 (次 commit)。`daemon/internal/metrics/histogram.go` に hand-rolled Prometheus histogram を追加、`ReadMissLatency` が handleReadMiss の全 return path (成功 + ENOENT/EIO/ENODATA) を timing。Grafana dashboard に p50/p95/p99 timeseries + heatmap の 2 panel 追加。client_golang 全面移行は未 (P-J-01 参照)

### J. metric / observability 深化 (`be §4-3, §4-4`)

- [x] ✅ **J-01 (part)** hand-rolled histogram で read-miss latency を計測 — 完 (I-05 と同 commit)。`s3fs_ebpf_read_miss_latency_seconds` histogram (13 bucket、[1ms..10s +Inf]) を Grafana dashboard に描画。client_golang 全面移行は残り (label 対応 / exemplar 使用時に着手予定)
- [x] ✅ **J-01 (flush)** 対称に flush latency も histogram 化 — 完 (次 commit)。`FlushLatency` を追加して `handleFlush` を defer で計測 (readDirty + S3 Put + arena admit refresh の合計)。Grafana に p50/p95/p99 timeseries + heatmap の 2 panel 追加。smoke で 10 flush events (touch + write × 5) の分布 (2 in ≤5ms, 8 in 5-25ms) を実測
- [ ] **J-02** `runtime/metrics` を expose (Go GC pause、goroutine 数)
- [ ] **J-03** CPU% panel に nice / softirq / irq / steal を追加 (BPF fentry が softirq に載っているか観察)
- [ ] **J-04** write throughput panel に req/s + bytes/req を legend で分解
- [ ] **J-05** fuse-bench と ebpf-bench を同時実行して 1 グラフで比較 (bench harness の並列化)

### K. upstream 貢献 (`w4a §9`)

- [ ] **K-01** `bpf_arena_get_kern_vm_start` の EXPORT_SYMBOL_GPL 提案  
  当てれば kmod 側で BTF-derived struct redefine 不要になる。
- [ ] **K-02** arena kern_vm range 用の generic-context fault handler 追加提案  
  当てれば A′ (kern_vm 直 deref) 経路が復活、B の GUP overhead が消える (数百ns オーダー改善見込み)。

### L. narrative / docs

- [x] ✅ **L-01** Meetup 登壇 script の起草 — 完 (次 commit)。`docs/demo-scenario.md` に Demo 0-8 の完全 script (35 min 想定タイム配分 + 各節 run / expected / say + 質疑応答想定 + 会場で刺さらなかった時の fallback)。demo 手順を再現可能な shell command 列で書き起こしてある
- [ ] **L-02** README.md 再生成 (B-05 と同項目)
- [ ] **L-03** 各 doc の相互リンク整理 (現状ハイパーリンクが手書きで壊れやすい)

---

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

## 7. 優先度 view (次 N 個)

「今日〜明日で何を触るか」目線での並び:

1. **A-01** daemon exit 時の SYNC clear (5 min の 1 行修正)
2. **B-05** README 再生成 (1 command)
3. **A-04** 古い warmFromS3 log 削除 (log noise 除去)
4. **B-02** microbench panic → error return (bench 全体の robustness UP)
5. **C-01** MinIO metric 名の実確認 (panel 12 が正しく描画されるように)
6. **B-01** MICRO_ITERS の default 引下げ (bench の開発サイクル短縮)
7. **A-03** flush PUT 失敗時の dirty flag 復旧 (write path の robustness)
8. **A-02** admit と sync delta 連動 (admit hash 肥大化の予防)
9. **B-04** bench/runner.sh 削除 (dead code cleanup)
10. **P-01** CEL syntax spike (Week 4 (d) 第一歩)

10 個で 1〜2 スプリント分。P-01 以降が Week 4 (d) 本流。

---

## 8. 参照

- `docs/week4a-implementation-notes.md §9` / `§10`
- `docs/week4a-c-session-notes.md §5`
- `docs/week4b-implementation-notes.md §4`
- `docs/week4c-implementation-notes.md §4`
- `docs/lessons-learned.md §5`
- `docs/benchmark-results.md §6`
- `docs/bench-env-notes.md §4`
- `docs/policy-engine-design.md §6` / `§7` / `§9`
