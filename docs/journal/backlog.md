# Backlog — プロジェクト残課題の一元リスト

各週次 note (`docs/weekN-*.md`) と設計 doc に散らばっている follow-up を 1 箇所に集約したもの。「今すぐ着手できる小改修」と「Week 5+ の別 track」を **優先度 × カテゴリ** で並べる。

**運用ルール**:
- 完了したら該当行に ✅ を付けて (取り除かない) 出典を追記
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

### S. キャッシュ / eviction の正しさ (2026-10-05 レビュー、`as §4-§7`)

背景: softcap (8) は eviction 投票の発動条件で、容量ではない。そのため hit-rate ベンチ (`hr` Validity review) は容量で正規化されておらず、W-TinyLFU 88.5% は容量 8 の Belady 上限 61〜65% を超えている。下の順に着手する。S-01〜S-03 が終わるまで、hit 率を Meetup で数字として出さない。

- [x] ✅ **S-01** 差分 invalidate — 実装済 (617f7ba、実機検証は S-06)。 (`as §4 P7`) — `s3fs_ebpf_admit_replace` は全置換のたびに一覧の全 ino の page cache を破棄している。1 ファイルの miss で、hot なファイルの page cache まで消える。前回の admit と (uaddr, size, offset) が同じ ino は破棄・`i_size_write` を省略する。旧エントリとの比較は staged 作成時に lock 下で行う。**受け入れ条件**: 1 ファイルの miss upcall の後も、他の admit 済みファイルの read が `.read_folio` に来ない (`folio_hits` / `read_miss_upcalls` が増えない)。目安 0.5 日
- [x] ✅ **S-02** バイト容量上限 + victim 選択 — 実装済 (方式 (a) スコア化。module param `capacity_bytes` / `softcap`、daemon `--cache-capacity` / `--softcap`。bench-hitrate は既定で capacity mode。in-flight の ino は evict しない。実機検証は S-06)。 (`as §4 P1-P3, §5`) — 内容は次の 4 点。目安 2〜3 日
  - daemon `--cache-capacity=<bytes>` (既定 = arena サイズ) を ioctl で kmod に渡す
  - kmod は投票の後もまだ上限を超えていれば、policy の順位で上限以下まで削る
  - 順位の取り方: (a) `on_evict` の戻り値をスコア化 (0 = keep, 大きいほど追い出し優先)、または (b) vtable に `select_victim()` を追加。まず (a) で始め、既存 policy の yes/no は「スコア 1」として互換を保つ
  - `s3fs_kfunc_softcap()` は実行時の値を返す。件数上限 (旧 softcap) は module param / ioctl で可変にして残す (既定 8 = デモ値)

  **受け入れ条件**: どの policy でも `adm` のバイト総量が上限以下。bench-hitrate の全行の hit 率が `opt` 以下になる
- [x] ✅ **S-03** page cache hit を policy に供給 — 実装済。`kmod/tree_traversal.c` に `s3fs_kfunc_policy_hit(u64 ino)` KFunc を追加 (`BPF_PROG_TYPE_TRACING` に register)、`bpf/s3fs_fastpath.bpf.c` の `fentry/generic_file_read_iter` hit パス (`offp != NULL`) から呼び出す。page cache 経由の繰り返し read でも policy の `on_hit` が dispatch される。`.read_folio` (arena-hit) で既に `on_hit` を呼んでいるパスでの二重呼び出しはどの policy も idempotent / cap-bounded なので許容。BPF オブジェクト再生成 + kmod ビルド + Go テスト全通過。実機検証は S-06。
- [x] ✅ **S-04** evict 反映の遅れ — 実装済 (8db7c82、push 直前に drain)。 (`as §4 P6`) — daemon の drain は 2 秒周期で、その間の全置換で evict 済み ino が再送・復活する。kmod が直近の evict 済み ino を覚えて再送を弾くか、ADMIT_ARENA ioctl の戻りで evict 結果を即時に返す。目安 0.5〜1 日
- [x] ✅ **S-05** 再計測 (S-01〜S-03 の後) — `hr` / `br` / `mo` を更新済。
  - ✅ `bench-hitrate.sh`: 3 seed × 10 policy (seed 42/1337/7)、`opt`/`adm_*` 併記、中央値とばらつき。`hr §"Result (2026-10-05)"` 参照。
  - ✅ `bench-write.sh`: `ebpf-fsync` 行取得済み。`br §8.2` 参照。
  - ✅ `bench-3way.sh`: `ebpf-nocache` 行取得済み。`br §7.7` 参照。
- [x] ✅ **S-06** 実機検証 (6.12) — 実施済 (2026-10-07)。
  - ✅ `verifier-demo` KFunc 後半 (`s3fs_kfunc_arena_list_walk`): 100,000 ノード walk、sum=5000050000 一致
  - ✅ `POST /policy/reload` が `BPF_LINK_UPDATE` 経由: dmesg `s3fs_ebpf: policy struct_ops updated (hot swap)` 確認
  - ✅ 非 sleepable struct_ops: SIEVE / S3-FIFO / W-TinyLFU-full いずれも verifier 通過・ロード成功
  - ✅ evict 時の page cache 破棄: 30/30 evict 後の cat が miss 経路を通ることを確認
  - ✅ `nocache=1`: 3x cat で delta_miss=3 (毎 open で S-09 evict_log_push + invalidate_inode_pages2 が動作)
  - ✅ S-01 差分 invalidate: hitrate_20 の miss upcall 後も hitrate_0/1 の page cache 保持 (re-read で upcall なし)
- [x] ✅ **S-08** ebpf-fsync の固定オーバーヘッド (~90 ms) — 修正済 (commit c75247f、2026-10-06)。根本原因: daemon の flush ioctl が PUT 完了を非同期でポーリング (100ms 周期 ticker) していた。修正: `S3FS_IOC_POP_FLUSH_WAIT` blocking ioctl に切り替え、kmod が flush event を受け取り次第即 wake。**結果**: 1 MiB ebpf-fsync min=19.7ms → fuse-raw (19.5ms) と同オーダーに達した。`br §8.4` 参照。
- [x] ✅ **S-09** ebpf-nocache の tiny ファイル miss rate が低い (~18%) — 修正済 (2026-10-06)。根本原因 2 点: (1) kmod の `admit_drop_all_ranges` が evict_log に通知しないため daemon が re-push して admit が復活; (2) `invalidate_mapping_pages` が trylock で active folio を skip。修正: `s3fs_ebpf_file_open` nocache パスに `evict_log_push(ino)` + `invalidate_inode_pages2()` fallback を追加。**結果**: tiny ファイルの microbench miss 率が ~18% → ≈100% に改善、p50 = 558µs (vs fuse-raw 1.7ms で 3× 速い)。`br §7.7` 参照。
- [x] ✅ **S-07** disk cache (A 層, `--cache-dir`) の上限管理 — 実装済。`DiskCache.maxBytes` フィールドを追加、`NewDiskCache` に `maxBytes int64` (0=unlimited) を追加、`Write` 前に `evictIfNeeded` (dir scan → mtime 昇順で古い順に削除) を呼ぶ。`s3.Config` / `gcs.Config` に `MaxCacheBytes` を追加、daemon に `--disk-cache-capacity` フラグ + `S3FS_DISK_CACHE_CAPACITY` env を追加。`TestDiskCache_Eviction` / `TestDiskCache_EvictionDisabled` で動作を pin。

### Q. セキュリティ / コード品質 クイック修正 (2026-10-10 レビュー)

「30 分〜1 時間で終わり、放置するとセキュリティ or 可読性で刺さる」もの。

- [x] ✅ **Q-01** `io.ReadAll` にボディサイズ上限なし (`main.go:567`) — `http.MaxBytesReader(w, r.Body, 1<<20)` を追加。完了 (2026-10-10)
- [x] ✅ **Q-02** allowlist nil 時のサイレント全許可 — `--policy-allowlist` 未指定かつ `file:` policy 使用時に stderr WARN を出力。完了 (2026-10-10)
- [x] ✅ **Q-03** ino state / disk cache ファイルが `0o644` — `0o600` に変更 (`sync_entries.go`, `objstore/cache.go`)。完了 (2026-10-10)
- [x] ✅ **Q-04** S3 key 解決ロジック 3 重コピー — `resolveEventS3Key(ino, parentIno, name)` ヘルパーを `sync_entries.go` に追加、3 箇所を置換。完了 (2026-10-10)
- [x] ✅ **Q-05** arena 高水位追跡スニペット 2 重コピー — `updateArenaHighWater(off, size)` ヘルパーを `admit_arena.go` に追加、2 箇所を置換。完了 (2026-10-10)
- [x] ✅ **Q-06** `drainFlushOnShutdown` の `coal` 引数未使用 — シグネチャと `_ = coal` を除去。完了 (2026-10-10)

### T. コードレビュー起票 (2026-10-06)

- [x] ✅ **T-01** `pruneAdmitsTo` が arena チャンクを解放しない (arena リーク) — 修正済。`planPruneAdmits` pure 関数を切り出し `pruneAdmitsTo` がそれを呼んで `admitProgs.FreeArenaChunk` に freed ranges を渡すよう変更。回帰テスト 3 件 (`TestPlanPruneAdmits_FreesArenaChunksForDropped` / `_ArenaBaseZeroSkipsFree` / `_KeepAllProducesNoDelta`) を追加。

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

- [x] ✅ **F-01** prewarm を sync 時にも回す (`w4b §4-3`) — 完 (次 commit)。`prewarm()` の per-entry ロジックを `prewarmEntry()` として切り出し、runSyncLoop callback から未 prewarmed な新 ino に対して呼ぶ。`prewarmed` map で idempotent 化、消えた ino は delete。`--prewarm-on-sync` (default true) で無効化可能。startup で warm 済み ino は seed 済のため sync 1 巡目は差分だけ処理。sync 時 replaceAdmit は H-04 fix で serialized push を継承
- [x] ✅ **F-02** basename 衝突対応 (`w4b §4-4`) — 完 (G-01 と同 commit)。`(parent_ino, name)` を unique key として扱う設計変更が G-01 で入ったので、`subA/foo.txt` と `subB/foo.txt` が別 entry として共存。実機 smoke で cat が両方 distinct content ("content-A" / "content-B") を返すことを確認
- [x] ✅ **F-03** BPF `inode_index` と sync の連携 (`w4b §4-5`) — 完 (次 commit)。`prewarmEntry` を "metadata warm (無条件 WarmCache)" と "content admit (policy-gated AdmitContent)" の 2 step に分割。policy=false でも stat が Path A で解決するようになった。sync 経由 F-01 の新 ino も同じ split を通るので、policy-suppressed entry の getattr も fast_hits に加算される

### G. VFS 拡張

- [x] ✅ **G-01** ディレクトリ (subdir) 対応 (`w4b §4-4`) — 完 (次 commit)。UAPI (`s3fs_sync_entry` / `s3fs_flush_event` / `s3fs_delete_event` / `s3fs_rename_event` に `parent_ino` 追加、0=root 後方互換) → kmod (`s3fs_entry_dyn.parent_ino`、`s3fs_local_entry.parent_ino`、`s3fs_entry_snap.parent_ino`、`norm_parent` helper、`resolve_entry_by_name` を parent-aware 化、`emit_one_entry`/`iterate_shared` を `dir->i_ino` で filter、`s3fs_ebpf_make_inode` が S_IFDIR で `dir_iops`/`dir_fops` 分岐、`.create`/`.rename`/`.unlink` が dir 情報を event に載せる、`local_prune_promoted_locked` が (parent,name) 一致で判定) → daemon (`fpebpf.Entry.ParentIno`、`buildEntriesFromS3` が S3 key を分解して synthetic dir entry を挿入、`pathByIno` 逆引き map、`resolveKeyPath` で flush/delete/rename event から full S3 key 復元、prewarm は dir で早期 return) の 5 stage。実機 smoke で `subA/foo.txt` と `subB/foo.txt` 共存 (F-02 同時解決)、cat/write/rename (cross-dir 含む)/unlink 全 path で正しい S3 key、dmesg BUG/WARN なし、既存 flat-bucket flow (hello.txt / big.bin) 温存を確認
- [x] ✅ **G-02** `.setattr` に mode / uid / gid 対応 — 完 (次 commit)。kmod-side に per-ino `s3fs_attr_overrides` (64-bucket hash + spinlock) を追加、`s3fs_ebpf_file_setattr` は ATTR_MODE/UID/GID のいずれかがセットされたら upsert、`s3fs_ebpf_make_inode` が override を再適用するので icache eviction 後の再 lookup で保持される。実機で chmod 0755 + chown 1000:1000 → `drop_caches=2` → stat が保持後の値を返すことを確認 (fix 前は 644/0/0 に revert)。file-type bit は entry 由来を保護、S3 tag への永続化は future work

### H. 環境 lifecycle / 信頼性

- [x] ✅ **H-01** kmod 再起動時の local additions 永続化 (`w4c §4-4`) — 完 (次 commit、部分)。`runFlushLoop` の ctx.Done 分岐で `drainFlushOnShutdown` (10s budget、synchronous handleFlush) を実行し、SIGINT 時点で kmod flush queue に既に enqueue 済み (直近の .release / .fsync 由来) の write が確実に PUT されるようにした。**未対応**: 現在 open 中で close されていない dirty 書き込みは対象外 (kmod 側の per-inode dirty list が別途必要)、およびハード kernel panic 経路。真の "kmod 再起動横断の永続化" は future work
- [x] ✅ **H-02** ino allocator の永続化 (`ll §5.3`) — 完 (次 commit)。`--ino-state=<path>` (env `S3FS_INO_STATE`) を追加、path 指定時は起動時に JSON をロードして inoByKey にマージ + nextIno を最大 ino+1 に前進、以降 assignIno / registerIno の mutation ごとに tmp+rename で atomic 書き込み。3 unit test で round-trip / missing file (first run) / disabled (no side effect) を pin。application が pid+ino で lock を識別しても daemon 再起動で ino が変わらない
- [x] ✅ **H-03** arena gc (`w4c §4-6`, `ll §5.4`, `be §4-1`) — 完 (次 commit)。`coalesceArenaFree` を pure function として追加、`FreeArenaChunk` と AdmitContent の split-tail path から呼ぶ。sort by offset → 隣接 chunk merge → 末尾 run が bump allocator に abut したら cacheNextOff に折り返し。5 unit test で adjacent merge / bump 折り返し / 完全 unwind / 空 / unsorted 入力を pin。D-02 の free-list を fragmentation-bounded にし、長時間稼働で "free_chunks の膨張 → best-fit scan cost 増大" が起きなくなる
- [x] ✅ **H-04** admit の snapshot re-push が atomic でない (`w4ac §5-3`) — 完 (次 commit)。`admitMu` を ioctl 完了まで保持する single-writer 制約に、`setAdmits` / `replaceAdmit` / `refreshAdmits` / `clearAdmits` / `dropAdmits` / `pruneAdmitsTo` の全 6 mutator を refactor。snapshot copy も撤去 (lock を持っている間は直接 admits slice を ioctl に渡せる)。`admitArena` を package var 化してテストで capturing fake に差し替え、`TestReplaceAdmit_ConcurrentPushIsSerialized` (N=64 goroutine × replaceAdmit を 5 count 実行、`-race`) で "全 push が admitMu 保持下" + "snapshot サイズ単調増加" + "最終 admits に N ino 全部揃う" を pin
- [x] ✅ **H-05** daemon miss reader の SIGINT clean shutdown (`w4ac §5-4`) — 完 (次 commit)。`runReadMissLoop` の shutdown goroutine に `readMissShutdownWarn=5s` watchdog を追加、`f.Close()` 後に main loop が 5s 以内に return しない場合 stderr に SHUTDOWN WARN を吐く (kmod .release 側のバグを observable に)。`TestOSFileCloseWakesBlockedRead` (os.Pipe pair) で "blocked Read が Close 500ms 以内に io.EOF/os.ErrClosed で wake" を回帰テストで pin
- [x] ✅ **H-06** pending の shutdown race (`w4ac §5-5`) — 完 (次 commit)。`s3fs_ebpf_upcall_shutdown` に refcount 不変条件のコメント (caller 側 ref が生きているので shutdown が hash ref を落としても waiter の p->err load は safe) を追加。`scripts/stress-shutdown.sh` で 200×16=3200 concurrent cat + 途中 SIGINT + dmesg BUG/KASAN/WARN 監視 + rmmod (5s retry) の smoke を実装、実機で PASS。将来 kmod-side 変更後の回帰検知に使う

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
- [x] ✅ **P-07** hit-rate 比較 bench — 完 (次 commit)。`scripts/bench-hitrate.sh` + `POST /policy/eval` endpoint + kmod-side mm ref fix + `docs/journal/hit-rate-bench.md`。Zipf α=0.7 over 30 files で SIEVE 62.5% / LRU 12% / S3-FIFO 9.5% (observe baseline 100%) 実測、SIEVE が最も working set を守れる証拠。P-14 on_hit で算法差が数値に反映されたことを確認
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

- [x] ✅ **I-01** Mountpoint for Amazon S3 (`mount-s3`) を三巴に追加 — 実装済。`scripts/lib/bench.sh` に `mount_s3_available` / `mount_s3_mount` / `mount_s3_umount` ヘルパーを追加。`bench-latency.sh` / `bench-3way.sh` に `MOUNT_S3_STARTED` フラグ付き条件 arm を追加 (mount-s3 未インストール時は skip with warning)。
- [x] ✅ **I-02** `drop_caches=1` で純 read-cold の比較 — 実装済。`bench-latency.sh` §1c / `bench-3way.sh` §2b に `--prepare 'sudo sysctl -q -w vm.drop_caches=1'` セクションを追加。コメントに「dentry/inode は残るため path lookup ノイズが消える」を明記。
- [x] ✅ **I-03** cold-cache 分離 — 実装済。`bench-latency.sh` §1d / `bench-3way.sh` §2c に cold-cache isolation セクションを追加。upcalls before/after delta を出力し「0 = arena が page cache 非依存で folio 補充」を検証できるようにした。
- [x] ✅ **I-04** fuse-cache を bench-latency にも追加 — 実装済。`bench-latency.sh` に `FUSE_CACHE_STARTED` フラグ付き `start_fuse_cache` を追加、全 6 セクション (hyperfine warm stat/cat/cold, microbench, perf, fio, large file) に fuse-cache arm を追加。
- [x] ✅ **I-05** latency histogram を Prometheus に export — 完 (次 commit)。`daemon/internal/metrics/histogram.go` に hand-rolled Prometheus histogram を追加、`ReadMissLatency` が handleReadMiss の全 return path (成功 + ENOENT/EIO/ENODATA) を timing。Grafana dashboard に p50/p95/p99 timeseries + heatmap の 2 panel 追加。client_golang 全面移行は未 (P-J-01 参照)

### J. metric / observability 深化 (`be §4-3, §4-4`)

- [x] ✅ **J-01 (part)** hand-rolled histogram で read-miss latency を計測 — 完 (I-05 と同 commit)。`s3fs_ebpf_read_miss_latency_seconds` histogram (13 bucket、[1ms..10s +Inf]) を Grafana dashboard に描画。client_golang 全面移行は残り (label 対応 / exemplar 使用時に着手予定)
- [x] ✅ **J-01 (flush)** 対称に flush latency も histogram 化 — 完 (次 commit)。`FlushLatency` を追加して `handleFlush` を defer で計測 (readDirty + S3 Put + arena admit refresh の合計)。Grafana に p50/p95/p99 timeseries + heatmap の 2 panel 追加。smoke で 10 flush events (touch + write × 5) の分布 (2 in ≤5ms, 8 in 5-25ms) を実測
- [x] ✅ **J-02** `runtime/metrics` を expose (Go GC pause、goroutine 数) — 完 (次 commit)。`daemon/internal/metrics/runtime.go` に `writeRuntime` を追加、`go_goroutines` / `go_threads` / `go_memstats_{alloc,sys,heap_objects,next_gc}_bytes` / `go_gc_runs_total` / `go_gc_pause_{seconds_total,last_seconds}` を Prometheus text 出力 (client_golang 名前互換)。Grafana panel 24/25/26 追加、`{job="s3fs_ebpf_daemon"}` filter で prometheus/node_exporter 自身の runtime を除外。`TestWriteRuntime_EmitsExpectedSeries` で 8 系列の HELP/TYPE/sample 出力を pin。実機で `go_goroutines 17`, `go_gc_pause_last_seconds 0.000122` を実測
- [x] ✅ **J-03** CPU% panel に nice / softirq / irq / steal を追加 — 完 (次 commit)。既存 panel 11 (Host CPU%) に 4 mode 追加、legend 7 系列に。BPF fentry が softirq に載っているかや、共有 VM の steal を bench 時に観察できる
- [x] ✅ **J-04** write throughput panel に req/s + bytes/req を legend で分解 — 完 (次 commit)。panel 6 の単一 legend "PUT bytes/s" を 3 系列 (bytes/s、req/s、mean bytes/req) に分解。events が上がって bytes が上がらなければ小 file 連発、逆なら大 blob と切り分けられる。fieldConfig の byName override で req/s と bytes/req に別 unit
- [x] ✅ **J-05** fuse-bench と ebpf-bench を同時実行して 1 グラフで比較 — 完 (次 commit)。`scripts/bench-parallel.sh` に 3 backend (ebpf / fuse-cache / fuse-raw) 並列 read/write 実行を実装、`timeout` + watchdog + jobs -p ポーリングで hang をブロック、metric snapshot t0/t1 で差分を出力。10s smoke で ebpf-read 1159 ops/worker vs fuse-cache 798 vs fuse-raw 442、ebpf-write 1141 ops/writer (E-03 coalescer で PUT は 1 event, 64B のみ) を実測。Grafana 上で MinIO ingress/egress + Go runtime + ebpf 側 counter が同 timeline で overlay 観測可

### R. R-01 fentry/generic_file_read_iter 評価 (`hook-gaps §3`)

- [x] ✅ **R-01-eval-overhead** R-01 hook overhead bench — 完 (本 commit)。`scripts/bench-read-iter.sh` を追加。fentry hook の有無 (`--no-read-iter-hook` フラグで切替) で read microbench の p50/p95/p99 を比較。LRU_HASH lookup + 2 atomic increment の理論オーバーヘッドが < 1 μs であることを定量化。
- [x] ✅ **R-01-eval-scenario** pattern-aware admit (sequential 大ファイル最適化) — 完 (本 commit)。`admit_arena.go` の `handleReadMiss` に `patternAwareAdmitEnabled` ガードを追加: `SeqRatio() ≥ 0.70` の場合に `GetRange(chunk)` ではなく `Get(全体)` を発行する R-01 最適化。`--pattern-aware-admit` フラグで有効化。評価スクリプト `scripts/bench-read-pattern-policy.sh` + `policies/bench-no-large-prewarm.cel` で upcall 削減 (10 MiB file: 3 upcalls → 1) + cold-read latency 3× 短縮 (3 × 30 ms → 1 × 30 ms) を実測可能。fentry が `.read_folio` block 前に発火するため初回 miss 時点で `seq_count ≥ 1` が保証され、warmup reads 不要。

### T. コードレビュー起票 — polish (2026-10-06)

- [x] ✅ **T-02** `dropAdmitsLocked` 戻り値の意味ズレ — 修正済。`dropped` カウンタを別途管理し `len(freedRanges)` ではなく実際に削除した ino 数を返すよう変更。回帰テスト `TestDropAdmitsLocked_ReturnsDroppedCount` を追加。
- [x] ✅ **T-03** `drainEvictions` / `readStats` がポーリング毎に fd open/close — 修正済。`drainEvictionsOn(devFd int)` / `readStatsOn(devFd int)` を追加し、`drainEvictionsLoop` / `sampleKmodStats` が起動時に 1 回 open して各ティックで再利用するよう変更。既存の `drainEvictions(devPath)` / `readStats(devPath)` は `reconcileEvictionsLocked` 等の呼び出し元のため保持。
- [x] ✅ **T-04** `planReplaceAdmit` が複数 range 持つ ino の旧チャンクを 1 個しか解放しない (latent arena リーク) — 修正済 (commit cda8595)。`freed []arenaChunkPair` を返す形に変更し全マッチを `append`。`TestPlanReplaceAdmit_MultiRangeFreesAllChunks` で回帰テスト追加。
- [x] ✅ **T-05** `arenaChunkPair.offset/size` が `uint32` で arena 4 GiB 超に対応できない (latent truncation) — 修正済 (commit 2ab0c57)。`arenaChunkPair`, `arenaChunk`, `cacheNextOff`, `FreeArenaChunk`, `AdmitContent` を uint64 に昇格。BPF map (`s3fs_inode_index`) の value 型 `__u32` 制約は WarmCache の call site で `uint32(nextOff)` キャストを保持しコメントを追記。

### Q. セキュリティ強化 (P1 — 2026-10-10 レビュー)

設計上の判断が必要で Q-01〜06 より工数がかかるもの。

- [x] ✅ **Q-07** `/policy/reload` と `/policy/eval` に認証なし — `--metrics-token` フラグ (env `S3FS_METRICS_TOKEN`) を追加。セットされた場合 `Authorization: Bearer <token>` を要求するミドルウェアを `metrics.Serve` に組み込み。`/metrics` / `/healthz` は認証不要のまま。起動時に token 未設定の場合は WARN を出力。実装: `metrics/exporter.go` + `main.go`。完了 2026-10-10。
- [x] ✅ **Q-08** `builtin:` プレフィックスが allowlist SHA256 検証を完全バイパス — `Resolve` に name バリデーション (`/`, `\`, `..` を拒否) とパス containment チェック (解決パスが `bpf/` 以下であることを検証) を追加。実装: `policyprog/loader.go`。完了 2026-10-10。
- [x] ✅ **Q-09** アリーナベースアドレスを stdout に出力 — fastpath 起動ログ・prewarm ログ・upcall ログから `uaddr=0x...` / `arena base=0x...` を除去。dev subcommand (`[B]` ログ) は残す。実装: `main.go` + `admit_arena.go`。完了 2026-10-10。

### V. 性能改善 (P1 — 2026-10-10 レビュー)

- [x] ✅ **V-01** `admitArenaIoctl` / `signalAdmit` が呼び出し毎にデバイス fd を開閉 — `openSharedDev` / `devFileOrOpen` を追加し `runFastpath` 起動時に 1 回だけ open。`admitArenaIoctl` / `signalAdmit` は shared fd を利用し、nil 時のみ per-call open (テスト fallback)。実装: `admit_arena.go` + `main.go`。完了 2026-10-10。
- [x] ✅ **V-02** `saveInoState` が `assignIno` のたびに JSON 書き込み — `assignIno` を dirty flag セットのみに変更し、`syncEntriesOnce` の末尾で `flushInoState()` を 1 回呼ぶ方式に変更。`registerIno` (rename 時のみ) は即時保存を維持。実装: `sync_entries.go`。完了 2026-10-10。
- [x] ✅ **V-03** プリウォームが逐次実行 — `prewarm` を `errgroup.WithContext` + `SetLimit(8)` で並列化。arena/BPF 操作は `Programs.cacheMu` 内で直列化済み。実装: `main.go`。完了 2026-10-10。
- [x] ✅ **V-04** `AdmitContent` の free-list が O(n) 線形 best-fit スキャン — `arenaFree` をサイズ昇順ソート済みスライスとして管理し、`sort.Search` で O(log n) best-fit ルックアップに変更。実装: `ebpf/loader.go`。完了 2026-10-10。
- [x] ✅ **V-05** `coalesceArenaFree` が毎回 `sort.Slice` — `insertArenaFreeBySize` ヘルパーで binary-insert し、挿入時はサイズ順を維持 (sort.Slice 不要)。coalesce は offset 順ソート→マージ→サイズ順復元の 2 sort だが頻度は低い。実装: `ebpf/loader.go`。完了 2026-10-10。
- [x] ✅ **V-06** 読み取りミス goroutine が上限なし — `sem := make(chan struct{}, 16)` セマフォで spawn を `readMissWorkers=16` に制限。実装: `admit_arena.go`。完了 2026-10-10。

### W. リファクタリング / 設計改善 (P1 — 2026-10-10 レビュー)

- [x] ✅ **W-01** `WarmCache` 戻り値が `uint32`、`AdmitContent` は `uint64` の型不統一 — `WarmCache` 戻り値を `uint64` に変更。`arena.Size()` も `uint32` → `uint64` に昇格し不要な型変換を除去。実装: `ebpf/loader.go` + `arena/arena.go` + `main.go`。完了 2026-10-10。
- [x] ✅ **W-02** ino アロケーター (`inoByKey` / `pathByIno` / `nextIno`) がパッケージグローバル — `InoAllocator` 型に集約し `inoAlloc` 変数として管理。全メソッドを型のレシーバに移し、外部 API はパッケージレベルの thin wrapper を維持。`CountMappings()` を追加して `main.go` の直接アクセスを排除。テストは `inoAlloc = newInoAllocator()` でリセット。実装: `sync_entries.go` + `sync_entries_test.go` + `main.go`。完了 2026-10-10。
- [x] ✅ **W-03** 3 つの stats サンプリング goroutine が独立ティッカーで走る — `sampleCounters` / `sampleKmodStats` / `sampleArenaFree` を `runStatsLoop` に統合。1 つのティッカーで 3 ソースをまとめてサンプリング。device fd は内部で 1 回 open (T-03 パターン)。`samplePolicyStats` は条件付き起動のため別 goroutine のまま。実装: `main.go`。完了 2026-10-10。

### K. upstream 貢献 (`w4a §9`)

根拠 doc: [`../dev-notes/ebpf-ecosystem-gaps.md`](../dev-notes/ebpf-ecosystem-gaps.md) — BPF_ARENA / STRUCT_OPS / KF_SLEEPABLE の踏んだ地雷を体系化。K-01/02 に加えて cilium/ebpf patch (`../misc/cilium-ebpf-addr-space-cast-marshal.patch`)、struct_ops template マクロ提案、KF_SLEEPABLE matrix doc 提案 (§2-§4) を並置。

- [ ] **K-01** `bpf_arena_get_kern_vm_start` の EXPORT_SYMBOL_GPL 提案 — 詳細 [`../dev-notes/ebpf-ecosystem-gaps.md §1.2`](../dev-notes/ebpf-ecosystem-gaps.md#12-改善案)
  当てれば kmod 側で BTF-derived struct redefine 不要になる。
- [ ] **K-02** arena kern_vm range 用の generic-context fault handler 追加提案 — 詳細 [`../dev-notes/ebpf-ecosystem-gaps.md §1.2`](../dev-notes/ebpf-ecosystem-gaps.md#12-改善案)
  当てれば A′ (kern_vm 直 deref) 経路が復活、B の GUP overhead が消える (数百ns オーダー改善見込み)。

### V2. 性能改善 polish (P2 — Meetup 後)

- [x] ✅ **V2-01** `DiskCache.evictIfNeeded` が書き込みごとに `os.ReadDir` — `*int64` アトミックカウンタ `usedBytes` を `DiskCache` に追加。`NewDiskCache` が既存ディレクトリを 1 回 scan して初期値を設定。`Write` は成功後に `atomic.AddInt64` で加算、`Invalidate` は削除前に減算。`evictIfNeeded` は fast path (カウンタ + newBytes ≤ cap → I/O ゼロ return) と slow path (ReadDir + 再同期 + 最古から削除) に分離。`TestDiskCache_UsedBytesTracking` / `TestDiskCache_UsedBytesSeededFromDisk` で pin。完了 2026-10-10。

### L. narrative / docs

- [x] ✅ **L-01** Meetup 登壇 script の起草 — 完 (次 commit)。`docs/user/demo-scenario.md` に Demo 0-8 の完全 script (35 min 想定タイム配分 + 各節 run / expected / say + 質疑応答想定 + 会場で刺さらなかった時の fallback)。demo 手順を再現可能な shell command 列で書き起こしてある
- [x] ✅ **L-02** README.md 再生成 (B-05 と同項目) — 完 (次 commit)。`make readme` で envsubst 再走、`s3-list` / `s3-stat` help に "--backend s3\|gcs (S3-compatible endpoint or Google Cloud Storage)" が反映される 3 行差分だけ吸収
- [x] ✅ **L-03** 各 doc の相互リンク整理 (現状ハイパーリンクが手書きで壊れやすい) — 完 (次 commit)。docs/ を broken-link audit、`docs/design/deisign.md` の typo を `docs/design/design.md` に rename して 3 サイト (README.md / policy-engine-design.md / week4a-implementation-notes.md) の参照を全て更新、`docs/INDEX.md` (トピック別索引) を新設して README から link、"追加/リネーム時に何をすればいいか" の運用手順も同 doc に書き起こし

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

**2026-10-10 更新 — コードベース全体レビューにより新規課題を追加**:

P0 新着 (Q-01〜Q-06 — セキュリティ/コード品質クイック修正):

1. **Q-03** ino state / disk cache ファイルパーミッション (`0o644` → `0o600`) — 30 分
2. **Q-01** `io.ReadAll` ボディサイズ上限 — 30 分
3. **Q-02** allowlist nil 時の WARN 出力 — 30 分
4. **Q-04** S3 key 解決ロジック 3 重コピー解消 — 1 時間
5. **Q-05** arena 高水位追跡スニペット 2 重コピー解消 — 30 分
6. **Q-06** `drainFlushOnShutdown` 未使用 `coal` 引数除去 — 30 分

P1 新着 (Q-07〜09, V-01〜06, W-01〜03) → **全完了 2026-10-10**:

- ✅ セキュリティ: Q-07 エンドポイント認証 / Q-08 builtin: バイパス強化 / Q-09 アリーナアドレスログ削除
- ✅ 性能: V-01 admit fd 再利用 / V-02 saveInoState debounce / V-03 並列プリウォーム / V-04 free-list O(log n) 化 / V-05 coalesce sort 削減 / V-06 goroutine pool
- ✅ リファクタリング: W-01 WarmCache 型統一 / W-02 InoAllocator 型集約 / W-03 stats サンプラ統合

P2: V2-01 DiskCache サイズ追跡改善 ✅ 完了 2026-10-10

**2026-10-07 更新 — S-06/T-04/T-05/S-09 完了**:

残り: なし (P0/P1 の全タスク完了、新規課題は上記)

完了済 (2026-10-06〜07):
- ✅ T-01 `pruneAdmitsTo` arena リーク
- ✅ T-02 `dropAdmitsLocked` 戻り値の意味ズレ
- ✅ T-03 fd per-call overhead
- ✅ T-04 `planReplaceAdmit` 複数 range arena リーク (commit cda8595)
- ✅ T-05 `arenaChunkPair` uint32 → uint64 昇格 (commit 2ab0c57)
- ✅ S-08 ebpf-fsync ~90ms 固定オーバーヘッド (commit c75247f)
- ✅ S-09 nocache tiny ファイル miss 率 ~18% (commit a6a6757)

S-01〜S-05/S-07 は実装済 ✅ 参照。

以下は旧リスト (A/B/C 系は完了済):

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

- `docs/journal/week4a-implementation-notes.md §9` / `§10`
- `docs/journal/week4a-c-session-notes.md §5`
- `docs/journal/week4b-implementation-notes.md §4`
- `docs/journal/week4c-implementation-notes.md §4`
- `docs/dev-notes/lessons-learned.md §5`
- `docs/journal/benchmark-results.md §6`
- `docs/journal/bench-env-notes.md §4`
- `docs/design/policy-engine-design.md §6` / `§7` / `§9`
- `docs/design/admit-and-softcap.md §4` / `§5` / `§7` (S 系)
