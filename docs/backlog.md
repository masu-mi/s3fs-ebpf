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
- [ ] **B-03** fuse mount が busy で umount 失敗する対策 (`be §4-2`)  
  bench script の trap で `umount -l` を fallback に。子 process が fuse fd を握っているケースで発生。
- [x] ✅ **B-04** `bench/runner.sh` + `bench/fio/*` + `bench/bcc/*` を削除 (`be §4-5`) — 完 (次 commit)、Makefile bench-dry も同時削除
- [x] ✅ **B-05** README.md 再生成 (`be §4-5`) — 完 (次 commit)

### C. Grafana / metric surface

- [x] ✅ **C-01** MinIO metric 名 version 依存の確認 (`be §4-3`) — 完、`minio_s3_traffic_{received,sent}_bytes` は現行 MinIO で存在確認 (dashboard 変更不要)
- [ ] **C-02** dashboard `rate(...[30s])` の window を `[1m]` に統一 (`be §4-3`)  
  Prometheus scrape 5s に対して 30s window は 6 サンプルで荒れる。1m に上げるか scrape interval を 1s に下げる。
- [ ] **C-03** flush errors panel を stat + threshold に (`be §4-4`)  
  errors line が 0 が normal なので、`> 0` で赤く光る stat 型が気付きやすい。

---

## 3. P1 — Week 5 で着手 (機能拡張)

「架け替えが要る、または 1 週分の作業」もの。

### D. 大 file / 部分 read 対応 (`ll §5.1`)

- [ ] **D-01** admit table を range list 化  
  現在の shape は `1 ino → 1 uaddr + size`。100 MiB 超の blob で破綻。`[(offset, uaddr, size), ...]` の list に拡張、`.read_folio` が folio 位置を含む range だけ pin する形に。
- [ ] **D-02** arena を sliding window にする  
  D-01 と対で、arena は「working set 分だけ載せる、他は demand load」。eviction policy が要る (→ policy engine と統合)。

### E. write path 強化

- [ ] **E-01** `.fsync` を blocking にする (`w4c §4-1`, `ll §5.2`)  
  現状は enqueue するだけで PUT を待たない。pending completion + signal_admit のパターンで blocking 化。3 mode (eager / synchronous fsync / synchronous write) を選択可能に。
- [ ] **E-02** `.unlink` / `.rename` 対応 (`w4c §4-3`)  
  現状 `rm /mnt/s3demo/hello.txt` は EPERM。UAPI に `S3FS_IOC_PUSH_DELETE` を切って sync loop から S3 DELETE。rename は S3 に無いので copy + delete で emulate、edge case 多い。
- [ ] **E-03** flush の coalescing (`w4c §4-5`)  
  現状 close ごとに 1 PUT。「N ms dirty のまま or M bytes 溜まったら fsync」相当の coalescer。close-driven でも実用上は問題ない (優先度 low)。

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
- [ ] **H-04** admit の snapshot re-push が atomic でない (`w4ac §5-3`)  
  ioctl 中に別 goroutine が addAdmit すると古い snapshot に append する race window。upcall goroutine 単一で走る現状は safe だが、複数並行時に壊れる。single-writer 制約を作る。
- [ ] **H-05** daemon miss reader の SIGINT clean shutdown (`w4ac §5-4`)  
  `os.File.Read` が blocking 中に fd close されると Go は EPIPE / EBADF を返すはずだが、シナリオによっては hangup 可能。長時間稼働 test で挙動確認。
- [ ] **H-06** pending の shutdown race (`w4ac §5-5`)  
  refcount 設計はテスト範囲で問題ないが、極限並行下で `hash_del` と complete のタイミング race。stress test で verify。

---

## 4. P1 — Week 4 (d) policy engine 実装 (`pe §6 §9`)

design doc (`pe`) は完成、CEL + built-in eBPF + user eBPF + KFunc の 3 層 architecture 決定済。実装は下記の順で:

- [x] ✅ **P-01** `pe Q3` CEL syntax spike — 完 (commit 8cbc8d1)。`daemon/internal/policy/` + `policies/default.cel` を追加、`--policy` flag、prewarm hook が entry サイズ / 名前で判定
- [x] ✅ **Layer 2 wiring** (P-01 の後続) — 完 (次 commit)。kmod に `s3fs_policy_on_admit` fentry anchor、`bpf/policy_observe.bpf.c` を出荷、daemon `--policy-bpf=builtin:policy_observe` で load + attach、policy_stats を /metrics に mirror
- [x] ✅ **Layer 3 wiring** (P-01 の後続) — 完 (次 commit)。`--policy-bpf=file:<path>` で任意 .bpf.o を load、SEC 命名で attach、smoke で /tmp/user_policy.bpf.o の load + counter 増加を確認
- [ ] **P-02** `pe Q4` in-flight semantics 検証 — `bpf_link_update` での差替え瞬間の event handling
- [x] ✅ **P-03 (part)** CEL Layer 1 → Layer 2 program select — 完 (次 commit)。`policy: "builtin:policy_s3fifo_ops"` を CEL 側に書いて daemon が起動時に eval、`--policy-bpf` flag より優先で attach。1 .cel file で prewarm + policy 選択の統合が実現。`metric()` CEL 拡張関数は残
- [ ] **P-03 (rest)** `pe Q6` metric API — policy 内 `metric("name", value)` の CEL 拡張関数、Prometheus export path (label 対応が要)
- [ ] **P-04** `pe Q8` trace harness — event stream record 形式の設計
- [ ] **P-05** `pe Q10` user eBPF 署名検証 — allowlist + SHA256 fingerprint + `load-bpf` API
- [x] ✅ **P-06 (S3-FIFO 部分)** built-in policy として S3-FIFO を出荷 — 完 (commit 6c29b2e / c62d8dc)。`bpf/policy_s3fifo.bpf.c` (fmod_ret) + `bpf/policy_s3fifo_ops.bpf.c` (struct_ops) の 2 版
- [x] ✅ **P-06 (LRU 部分)** LRU-like (admit-order FIFO) を追加 — 完 (次 commit)。`bpf/policy_lru_ops.bpf.c` に struct_ops で。global epoch counter + per-ino seed で age-based eviction (LRU_CAP_AGE=3)。同 commit で kmod call site の bug fix (struct_ops on_admit/pressure が呼ばれていなかった)。smoke で "struct_ops voted evict on 11/14 entries" 実測。W-TinyLFU / SIEVE も同 template で追加可能
- [ ] **P-07** hit-rate 比較 bench を Week 4 (e) に追加 (`pe §7 backlog`)
- [ ] **P-08** KFunc set の 1 次実装 (統計 accessor / primitive 操作 / signal detector) (`pe §4.5.1 Layer 0`)
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
- [ ] **I-05** latency histogram を Prometheus に export (現状 counter/gauge のみ、`prometheus/client_golang` 移行が要る)

### J. metric / observability 深化 (`be §4-3, §4-4`)

- [ ] **J-01** `client_golang` に移行して `HistogramVec` 追加 (read/write p50/p95/p99)
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

- [ ] **L-01** Meetup 登壇 script の起草 (`motivation.md §7-9` の内容ベース)
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
