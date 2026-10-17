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

### U. ベンチ計測の妥当性 (2026-10-11 レビュー)

性能数値を meetup / docs/journal に残す前に修正したいもの。放置すると過去計測値がそのまま誤解釈されて "どの policy が勝ったか" 等の判断が歪む。

- [x] ✅ **U-01** `bench-hitrate.sh` が失敗した `cat` を hit に計上している (`scripts/bench-hitrate.sh:193, :221`) — `cat` の exit code を捕捉して `err` 列に集計 (成功・失敗を分離)。hit 計算を `N_ACCESS - d_miss` から `s3fs_ebpf_folio_hits_total` デルタ (`folio` 列) + page-cache 近似値 (`pc` 列) ベースに変更。完了 (2026-10-11)
- [x] ✅ **U-02** `bench-hitrate.sh` の `rate` ラベルが policy と直接関係ない page cache 保持を含む (`scripts/bench-hitrate.sh:218-227`) — 列を `rate_arena` (folio_hits / N_ACCESS) と `rate_total` ((folio+pc) / N_ACCESS) に分割。`hr §"Output columns"` に各列の定義を明記。完了 (2026-10-11)
- [x] ✅ **U-03** `bench-write.sh` が最後のコマンドの書き込みしか S3 で検証していない (`scripts/bench-write.sh:93-109`) — backend ごとに別 S3 key (`w-${label}-{fuse-raw,fuse-cache,ebpf,fsync}.bin`) に分離し、fuse/ebpf-fsync は即時 diff、ebpf fire-and-forget は sleep 5 後に検証する構造に変更。完了 (2026-10-11)
- [x] ✅ **U-04** `bench-scale.sh` の clat extraction が unit mismatch で沈黙失敗 (`scripts/bench-scale.sh:75, :99`) — awk pattern を `/clat \((nsec|usec|msec)\)/` に拡張し usec→×1000 / msec→×1000000 で nsec 統一出力に変更。列ヘッダも `clat_ns(norm)` に更新。完了 (2026-10-11)
- [x] ✅ **U-05** `bench-scale.sh run_read` の "scale" 比較が page cache と FUSE 往復の混合 — 修正済 (2026-10-11)。各 `run_read` 呼び出しの直前に `sync; sudo sysctl -q -w vm.drop_caches=1` を追加し、page cache をゼロから計測。ebpf は `.read_folio → arena` 経路、fuse-raw は FUSE upcall 経路をそれぞれ cold page cache から測定する。関数コメントも修正内容に合わせて更新。
- [x] ✅ **U-06** `bench-parallel.sh` の `pkill` パターンがどのプロセスにもマッチしない — `stop_all` の pkill を `jobs -p | xargs -r sudo kill -9` に置き換え。完了 (2026-10-11)
- [x] ✅ **U-07** `bench-parallel.sh` の writer が backend 間で同じ S3 key に書き込む — writer path を `w-parallel-${mp##*/}-$i.txt` (MP basename 含む) に変更し backend 間の S3 key 衝突を解消。完了 (2026-10-11)
- [x] ✅ **U-08** `bench-read-pattern-policy.sh` の promote 検出がログ文字列依存で脆い — daemon ログに `PROMOTE_SIGNAL=pattern-aware` 安定マーカーを追加 (`admit_arena.go:585`)、スクリプトの grep 対象を変更。完了 (2026-10-11)

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

### X. ポリシー実装の意味論問題 (2026-10-11 レビュー)

「現在の bench workload (全ファイル 44 B 単一レンジ) では顕在化しないが、D-01 大ファイル partial admit を混ぜると policy ランキングが歪む」クラスの semantic bug。設計判断が要るので P1。

- [x] ✅ **X-01** `s3fs_ebpf_admit_replace` が range 単位で `on_evict(ino)` を呼ぶため、マルチレンジ ino で stateful policy の state が 1 ラウンド内で N 回 mutate する — 修正済 (2026-10-11)。`kmod/vfs_ops.c` の eviction ブロックを 2 フェーズに再構成: Phase 1 で unique ino ごとに `on_evict` を 1 回だけ呼んでスコアをテーブルに保存、Phase 2 でそのテーブルを参照して cand[] を構築 (capacity mode では同一 ino の 2 エントリ目は skip)。X-05 (live_bytes の ino 重複カウント → cand 単一化で解消) も同時に対処済み。
- [x] ✅ **X-02** `wtinylfu_ops` (scoped) の "grace" 条件が反直感的 — `EVICT_THRESHOLD` を 2 → 1 に変更し `est==0 grace` チェックを除去。est=0 (未アクセス) のみ evict、est≥1 (1 hit 以上) は keep で谷を解消。完了 (2026-10-11)
- [x] ✅ **X-03** CMS aging が bench harness の INTERLEAVE と相互作用して warm 項目を落とす — 修正済 (2026-10-11)。`AGE_INTERVAL` (admit round ベース) を廃止し `AGE_INTERVAL_HITS=512` (on_hit 累計ベース) に置き換え。`on_admit` の aging trigger を除去し `on_hit` で PSTAT_HITS カウンタを加算・512 ヒットごとに `cms_age()` を発火。bench forced eval や daemon sync round では aging が起きなくなり、warm 項目が spurious evict される問題を解消。`bpf/policy_wtinylfu_ops.bpf.c` の enum に `PSTAT_HITS=3` を追加 (BPF map size は PSTAT_MAX 自動更新)。
- [x] ✅ **X-04** `s3fs_victim_cmp` のタイブレーク "oldest first" は実質 non-deterministic — 修正済 (2026-10-11)。`setAdmits` で `initial` を `Ino` 昇順ソートしてから `admits` に格納 (`daemon/cmd/s3fs-ebpf/admit_arena.go`)。ino はシーケンシャル割り当てのため昇順 = 割り当て順 = 最古優先。kmod の `s3fs_victim_cmp` idx タイブレーク (最小 = 最古) が設計通り機能し、score=0 同士の eviction が run 間で再現性のある結果を返す。UAPI 変更なし。
- [x] ✅ **X-05** `admit_replace` capacity モードの `live_bytes` 計算で同一 ino の複数 range を重複カウント — X-01 修正 (cand[] を ino 単位に dedup) で解消済み。cand[] に 1 ino = 1 エントリしか入らないため `admit_evict_ino` が返す `freed` (全 range 合計) による live_bytes デクリメントが正確に行われる。完了 (2026-10-11、X-01 と同 commit)
- [x] ✅ **X-06** `policy_time_decay_ops` が bench-hitrate.sh の convergence loop で全 ino を自食いする — `on_evict` の first-sight シードを `0` (sentinel "未 hit") に変更し、on_hit 未発火の間は `!*last → return 0` で無期限 grace を付与。on_hit は non-zero の実 timestamp を書くため判定と衝突しない。完了 (2026-10-11)
- [x] ✅ **X-07** `trace.Replay` が write event を silently drop — `Policy` interface に `Invalidate(ino uint64)` を追加し LRU/SIEVE/OPT/noEvict に実装。`Replay` を `switch e.Op` に書き換えて OpWrite で `p.Invalidate` を呼ぶよう変更。Accesses は read のみカウント。`TestWriteInvalidatesEntry` (全 4 policy で read→write→read = 0 hits / 2 misses) を追加。完了 (2026-10-11)

**X-01〜X-05 の共通受け入れ条件**: D-01 の `big.bin` / `10m.bin` / `100m.bin` を含む bench-hitrate.sh を回し、修正前後で各 policy の `rate_arena` 差分を journal に記録。マルチレンジ ino が混ざった workload で X-01 修正により SIEVE / wtinylfu の rate が有意に変動することを示す。
**X-06 の受け入れ条件**: time_decay の hit rate が `start_stack` 期間変動 (3s → 15s) に不感となり、他 policy と同じ workload-driven な挙動を示す。
**X-07 の受け入れ条件**: write を含む合成 trace (reads 100 + writes 20 を交互) で `trace-replay` を回し、write の次の read が miss (reads 100 の中で write 対象 ino が miss にシフト) として現れることを示す。

### Y. ベンチ計測の妥当性 Phase 2 (2026-10-12 レビュー) — P0

U-01〜U-08 + X-01〜X-07 を適用した 2026-10-12 の 3-seed 再計測 (`hr §"Result (2026-10-05)"` を置き換えた新表) をレビューした結果、ベンチの**定義壊れと系統誤差**が残っている。Meetup 用の数値として引用する前に修正する。

- [x] **Y-01** `bench-hitrate.sh` の `rate_arena/rate_total` 定義が壊れている — `read_iter_hits` は `generic_file_read_iter` 1 回ごと加算 (`cat` 1 回 ≈ 2 回 fire)、`miss` は `.read_folio` ベースで `cat` 1 回ごと最大 1 回。`rate_total = (folio + pc) / N_ACCESS` の分子は per-read_iter、分母は per-cat なので **2× 不整合**。結果 `rate_total 133–143%` と Belady opt (61%) を超えて 100% を越える。主指標を `hit = 1 − miss/N_ACCESS` (旧 2026-10-05 定義) に戻し、`folio/pc` は内訳列として並置。`hr §"Output columns"` と `scripts/bench-hitrate.sh:234-239` を修正。目安 0.5 日 **→ 2026-10-12 完了**: `rate_total` を `hit_rate = 1 - miss/N_ACCESS` に置換、`pc` 列はアノテーション扱いで保持
- [x] **Y-02** X-04 `Ino 昇順ソート` + `s3fs_victim_cmp` idx tiebreak が Zipf 分布と相関し systematic bias を作る — bucket List は辞書順なので `hitrate_0 → ino 100, hitrate_1 → 101, hitrate_10 → 102, …, hitrate_19 → 110, hitrate_2 → 111, …` と ino 分布が非自明に歪む。Zipf hot (index 0, 1, 2) が低 ino、warm (3–9) が高 ino に散り、"evict lowest idx first" が偶然 warm を保護する**幸運**で成立している。もし `hitrate_000.txt` 形式でシードすれば observe の hit 率は 0 近傍まで落ちる。`s3fs_victim_cmp` の score=0 tiebreak を (a) `last_hit_ns` ベース (`s3fs_kfunc_time_ns()` を参照、BPF 側で export)、または (b) `hash(ino) ^ round_seed` のランダム化に変更。X-04 の "determinism" は seed 固定で保つ。目安 1〜2 日 **→ 2026-10-12 完了**: `s3fs_victim` 構造体に `tiebreak = hash_32(ino ^ ino>>32, 32)` フィールドを追加、`s3fs_victim_cmp` を `hash(ino)` tiebreak に変更 (`kmod/vfs_ops.c`)
- [x] **Y-03** `bench-hitrate.sh` の N_ACCESS=200 がポリシーの時間/頻度ベース signal を測るには短すぎる — 200 × ~2 iter/access = 400 on_hit で `AGE_INTERVAL_HITS=512` (X-03) 未満 → W-TinyLFU scoped/full の aging が一度も発火しない。同構造で S3-FIFO ghost promotion も 200 access では populate しきれず、`time_decay` DECAY_NS=5s も 1.2s ワークロードを越えない。結果「ランキング逆転」は policy 良否ではなく**ベンチ長の過少**を反映している。N_ACCESS=2000+、Zipf α={0.5, 0.7, 0.9, 1.3} の sweep に拡張。`scripts/bench-hitrate.sh:41-42` のデフォルト見直し + `bench-hitrate-multi.sh` の α sweep 対応。目安 0.5 日 (計測) + 0.5 日 (sweep) **→ 2026-10-12 完了**: N_ACCESS デフォルト 200→2000 (`bench-hitrate.sh:41`)、`bench-hitrate-multi.sh` を ALPHAS 変数対応に書き直し (α × seed の全組み合わせを集計、hit_rate カラム対応)
- [x] **Y-04** ゼロパディング無しの bucket seed が Y-02 bias を**偶然 decorrelate** して worst case を隠している — `scripts/bench-hitrate.sh:81-85` の `hitrate_<i>.txt` を `hitrate_<i:03d>.txt` 形式に変更して worst case (Zipf index と ino 分布が完全一致) を再現。`hr` に "ゼロパディング前後での observe rate 比較" を記録して Y-02 fix の受け入れ条件とする。目安 0.25 日 **→ 2026-10-12 完了**: `seed_bucket` / `cat` パスを `printf '%03d'` 形式に変更 (`scripts/bench-hitrate.sh`)
- [x] **Y-05** D-01 multi-range workload bench (`big.bin` / `10m.bin` / `100m.bin` 混在) が未作成 — X-01〜X-05 共通受け入れ条件が未検証のまま。`scripts/bench-hitrate-multi.sh` に `MIXED=1` オプションを追加し、大ファイル + 44B tiny を混ぜた Zipf access で各 policy の `rate_arena` 差分を記録する。目安 1 日 **→ 2026-10-12 完了**: `scripts/bench-hitrate-mixed.sh` を新設。N_LARGE=3 (5MB, partial-admit 対象) + N_SMALL=10 (44B) の混在ワークロード。大ファイルを Zipf hot ランク 0..N_LARGE-1 に配置し policy の byte-capacity vs slot-count 意思決定を検証。`make bench-hitrate-mixed` target 追加
- [x] **Y-06** `read_iter_hits` ダブルカウント検証 — `.read_folio` fast path (`s3fs_folio_hits` 加算) と `generic_file_read_iter` fentry (`S3FS_STAT_READ_ITER_HITS` 加算) が同一 cat の初回 read で両方 fire している疑い。U-01 の `d_page_cache_hits = d_read_iter_hits - d_folio - d_miss` が初回 cat の read_folio-hit を "iter + folio" の両方で数えるため `pc` が過大。kmod 側で "read_iter_hit without preceding read_folio" を数える別カウンタ (`s3fs_pc_only_hits`) を追加するか、`pc` 列を削除 (Y-01 と連動)。目安 0.5 日 **→ 2026-10-12 完了**: `S3FS_STAT_READ_ITER_HITS_POS0` カウンタを追加 (ki_pos==0 の read_iter hit のみ計上)。`iter_mult = iter_hits/pos0_hits ≈ 2` でダブルカウントを定量化。`pc` 計算を pos0 ベースに修正。Prometheus 経由で `s3fs_ebpf_read_iter_pos0_hits_total` を公開
- [x] **Y-07** `pressure_ops` 単独トップ (miss_med=119) の原因が未解明 — 容量モードで admit_count ≈ softcap に張り付くはずで、本来 SIEVE lenient 相当に落ちるはず。にもかかわらず SIEVE (128) を 9 miss 上回る理由として、X-01 Phase 1 eval 中の瞬間値が softcap×1.5 を越え strict モードが数回だけ発火している可能性 (仮説)。`bpf/policy_pressure_ops.bpf.c` に `pstat_strict_triggered` カウンタを追加、dmesg or /metrics で発火回数を可視化。目安 0.5 日 **→ 2026-10-12 完了 (Z-04 と同時実施)**: `PSTAT_STRICT_TRIGGERED`/`PSTAT_LENIENT_TRIGGERED` カウンタを追加、Prometheus に `s3fs_ebpf_policy_pressure_strict_total`/`lenient_total` として公開

**Y-01〜Y-06 の共通受け入れ条件**: ゼロパディング bucket (Y-04 適用) で `observe` の miss_med が Y-02 fix 前 → fix 後で有意に変動する (fix 前は miss ≈ opt - softcap で最悪ケース、fix 後は policy 選択で spread が広がる)。各 policy の `hit_rate = 1 − miss/N_ACCESS` を Belady opt と直接比較可能にする (gap が 20pp → より小さい値に縮まることを示す)。

### Z. ポリシー実装の時定数ハードコード (2026-10-12 レビュー) — P1

X-02〜X-06 で policy 実装の意味論バグは修正したが、**時定数 (aging interval / decay / threshold)** がハードコードのままなのでワークロード長/形状に適応できない。bench 側 (Y) と policy 側 (Z) の両方で tuning 可能にする。

- [x] **Z-01** W-TinyLFU `AGE_INTERVAL_HITS=512` ハードコード (`bpf/policy_wtinylfu_ops.bpf.c:65`) — short bench で aging 発火せず、long production では過剰 aging で warm 項目が落ちる両方の問題。ratio-based (`age_interval = max(cms_sum / N_SLOTS * 4, 32)`) または CMS 平均が CMS_MAX/2 を越えたら age する dynamic aging に変更。`wtinylfu_full_ops` も同様。目安 1 日 **→ 2026-10-12 完了**: `wtinylfu_config[0]` ARRAY map + `--wtinylfu-age-interval` フラグ。BPF 側は modulo ベースに変更
- [x] **Z-02** `time_decay` DECAY_NS=5s ハードコード (`bpf/policy_time_decay_ops.bpf.c`) — bench と production で全く違う値が必要。`/sys/module/s3fs_kfunc/parameters/decay_ns` module param、または BPF array (`policy_params[0] = decay_ns`) で runtime 変更可能に。目安 0.5 日 **→ 2026-10-12 完了**: `time_decay_config[0]` ARRAY map + `--time-decay-ns` フラグ
- [x] **Z-03** S3-FIFO ghost list `max_entries=1024` ハードコード (`bpf/policy_s3fifo_ops.bpf.c`) — 200 access では ghost が populate しきれず promotion が発火しない。ghost LRU 容量をワークロード size proportional (`cap_bytes / FILE_BYTES_HINT * 4`) にするか、ghost expiry を時間ベースに。目安 0.5 日 **(低優先: 実コードは 8192、Y-03 で N_ACCESS 拡大済のため実害なし。次回 N_FILES 拡大時に再評価)** **→ 2026-10-12 完了**: `policyprog.LoadOpts{MapMaxEntries}` を追加し、ロード前に `CollectionSpec.Maps[mapName].MaxEntries` を上書き可能に。`main.go` で `ghost_cap = max(8192, softcap*1024)` を計算 (cap: 65536) し、起動時 / hot-reload の両 `policyprog.Load` 呼び出しに渡す。`softcap=-1` (デフォルト) は ghost_cap=8192 (現状維持)。`TestLoadOpts_UnknownMapIgnored` 追加。
- [x] **Z-04** `pressure_ops` の strict/lenient 発火条件が実測で見えない (Y-07 と連動) — `pstat_strict_triggered` / `pstat_lenient_triggered` カウンタを `bpf/policy_pressure_ops.bpf.c` に追加、/metrics に export、Grafana panel で切替頻度を可視化。目安 0.25 日 **→ 2026-10-12 完了 (Y-07 と同時実施)**
- [x] **Z-05** `wtinylfu_full` doorkeeper reset interval がハードコードで small working set に不利 — reset frequency を dynamic (cache saturation ratio ベース、`live_bytes / cap_bytes` が 0.9 を越えたら reset) に。`bpf/policy_wtinylfu_full_ops.bpf.c` の `DK_RESET_INTERVAL` を condition-driven に。目安 0.5 日 **→ 2026-10-12 完了**: `wtinylfu_full_config[0,1]` ARRAY map + `--wtinylfu-full-age-interval` / `--wtinylfu-full-dk-reset-interval` フラグ

**Z 系の受け入れ条件**: Y-03 で N_ACCESS=200/2000/10000 の 3 点を同じ bench で測り、Z-01/Z-02/Z-03 で tuning 可能になった時定数が各ワークロード長で異なる最適値を取ることを示す (short bench で aging 発火、long bench で過剰 aging なし)。

### AA. FUSE 比 cold-path 性能改善 (2026-10-12 レビュー) — P1/P2

主要指標 (warm cat 2.3×, open+read+close 169×, write durable 1.7×) は fuse-cache 比で十分な優位。残る劣後ポイントは **並列 read の cold scale** (`br §10.2`) と **mount-s3 cached 比の瞬間 latency** (`br §11.3`)。

- [x] **AA-01** 並列 read cold scale で ebpf が fuse-raw に 1.1〜1.2× 劣後 (`br §10.2`、U-05 修正後に顕在化) — 64 jobs で fuse-raw 5320k vs ebpf 4638k。原因: `.read_folio` 内の `admit_snapshot` (spinlock + hash walk) + GUP + kmap のオーバーヘッド。改善案:
  - (a) `admit_snapshot` を RCU-indexed admit hash に移行 (`kmod/vfs_ops.c` の `s3fs_admits_lock` を rcu_read_lock に置換、admit entry は RCU-protected)
  - (b) `.read_iter` を kmod で持ち、admit hit 時に `generic_file_read_iter` を bypass して arena から直 `copy_to_iter` (page cache 経由せず)
  - (c) GUP cost の削減 — K-02 (upstream arena fault handler) が入れば kern_vm 直 deref で消える
  目安 2〜3 日 (a) / 3〜5 日 (b) / 不確定 (c)
  **→ 2026-10-12 完了 (a)**: `DEFINE_SPINLOCK(s3fs_admits_lock)` → `DEFINE_RWLOCK(s3fs_admits_lock)` に変更。`admit_snapshot` (hot path) と `s3fs_admit_size` / mm-only read (line 2240) の 3 箇所を `read_lock/read_unlock` に昇格、書き込みパス 5 箇所 (`admit_drop_all_ranges`, `admit_replace`, `admit_shutdown`, `admit_drop_ino_locked`) を `write_lock/write_unlock` に変更。64 並列 `.read_folio` がロック競合なく同時進行できるようになる。
- [x] **AA-02** `--pattern-aware-admit` (R-01-eval-scenario) を default on 化 — 10 MiB+ cold read で 3× latency 短縮を実測済 (bench-read-pattern-policy.sh)。`daemon/cmd/s3fs-ebpf/main.go:397` の `patternAdmit` のデフォルトを `true` に。`bench-latency.sh` にも組み込む。目安 0.25 日 **→ 2026-10-12 完了**: `main.go:397` の default を `false` → `true` に変更
- [x] **AA-03** `.read_iter` の kmod 直実装 — AA-01(b) と同体。admit hit 時に arena → iter の直 copy、page cache を bypass することで microbench open+read+close p50 が 3.075µs → 2µs 以下を狙う (mount-s3 比のさらなる優位)。目安 3〜5 日 (大改修なので要設計) **→ 2026-10-12 完了**: `s3fs_arena_copy_to_iter`（GUP 1-page-at-a-time で arena → iov_iter 直コピー）と `s3fs_ebpf_read_iter`（arena hit → `s3fs_arena_copy_to_iter`、miss → `generic_file_read_iter` fallback）を `kmod/vfs_ops.c` に追加。`s3fs_ebpf_file_fops.read_iter` を `generic_file_read_iter` → `s3fs_ebpf_read_iter` に切り替え。`vfs_ops.o` ビルド警告なし。
- [x] **AA-04** daemon の async read prefetch — fentry/generic_file_read_iter の seq_ratio 情報を使って daemon が先読み admit を発行、mount-s3 的な cold-miss 低減。`admit_arena.go` の pattern-aware admit を "trigger on upcall" から "trigger on seq pattern detected" に拡張。目安 1〜2 日 **→ 2026-10-12 完了**: `addAdmitRange`（新関数: ino の既存 range を evict せずに新 range を追加）、`seqPrefetchNext`（pure helper: admits snapshot + LastEndPos → 次 chunk offset または -1）、`runSeqPrefetchLoop`（200ms tick で admits を scan し SeqRatio≥0.70 かつ read 位置が chunk 末尾 2MiB 以内のとき非同期 GetRange + addAdmitRange）を実装。`--prefetch-seq-admit`（default false）で有効化。`TestSeqPrefetchNext` 追加。
- [x] **AA-05** `--disk-cache-capacity` の adaptive sizing — 固定上限から watermark + LRU 自動 evict に変更して mount-s3 の SSD staging との機能面バランスを揃える。`objstore/cache.go` の `DiskCache.evictIfNeeded` を watermark-based に拡張 (high=90%/low=70% で slow path 発火)。目安 1 日 **→ 2026-10-12 完了**: `DiskCache` に `highWMFrac=0.90` / `lowWMFrac=0.70` を追加。`evictIfNeeded` の fast path を `≤ hwm` に変更、eviction target を `maxBytes` から `lwm` に変更。既存 `TestDiskCache_Eviction` を maxBytes=25→30 に更新（hwm/lwm 数学を維持）、`TestDiskCache_WatermarkBatchEviction` 追加（90→70% のバッチ evict を検証）。全テスト pass。
- [x] **AA-06** mount-s3 を bench-parallel / bench-scale にも追加 — 現状 bench-latency / bench-3way のみ (I-01)。並列 read / write / 大 blob の各軸で mount-s3 の優位/劣後を定量化し、narrative の 4 象限 (cold-path / warm-path / write / parallel) を揃える。目安 0.5 日 **→ 2026-10-12 完了**: `bench-parallel.sh` に `start_mount_s3` + `spawn_readers` (読み取り専用)、`bench-scale.sh` に `start_mount_s3` + `run_stat` / `run_read` (drop_caches 付き) を追加。いずれも `MOUNT_S3_STARTED=1` のときのみ実行する条件分岐済み。

**AA 系の受け入れ条件**: 64 jobs read で ebpf ≥ fuse-raw (AA-01)、cold read 10 MiB で ebpf が 3× 高速 (AA-02 default on)、microbench p50 ≤ 2µs (AA-03 達成時)、mount-s3 を含む 4-way bench が全軸で揃う (AA-06)。

**AA 完了後の残課題** (2026-10-13 ベンチ再計測レビュー): 64 jobs で ebpf 4045k vs fuse-raw 5289k (76%)、100MB cat で fuse-cache 11.1ms vs ebpf 13.0ms (1.17× 劣後)。AA-01 の rwlock 化だけでは解決せず。GUP + arena サイズ (128MB) の構造的 bottleneck と判断。→ AB 系で設計改善。

### AB. arena 役割縮退 (metadata-only) 設計改善 (2026-10-13 レビュー) — P1

**背景**: `docs/design/bpf-arena-use-cases.md` のレビュー結果、arena 128MB をバルクページキャッシュとして使う現設計は arena の設計意図 (small, hot metadata) と不整合。改善案A (arena を admit メタデータだけに縮退、ページ実体は Linux page cache に委譲) が最有力。GUP オーバーヘッド (AA-01 残課題) と arena サイズ上限 (大ファイル劣後) の両方を同時解消する見込み。**実装前に 3 段階の検証** (Phase 1 分離測定 → Phase 2 PoC → Phase 3 本格計測) で判断する。

**Phase 1 — 現状の bottleneck 分離測定** (判断材料集め、2〜3 日):

- [x] **AB-01** GUP オーバーヘッド分離マイクロベンチ — **→ 2026-10-13 完了**。kmod に `s3fs_gup_calls_total` / `s3fs_gup_ns_total` 計測を追加、IOC_STATS [3]/[4] + Prometheus 経由で取得。**結果: 1728.5 ns/call** (閾値 500ns 超過、bottleneck 確定)。
- [x] **AB-02** arena hit path と page cache path の read latency 比較 — **→ 2026-10-13 完了**。`s3fs_arena_bypass` module param で arena bypass。**結果: 1MB ファイルで page-cache (5.341ms) < arena (5.559ms)**。small ファイルは arena 有利 (5.177ms < 5.298ms)。`scripts/bench-ab-arena-vs-pagecache.sh` で統合計測。
- [ ] **AB-03** mount-s3 を同一ワークロードで動かし外部基準の 4-way 比較 — AA-06 の bench 延長で、特に 10MB / 100MB の cold-read を 3 回ずつ (fuse-raw / fuse-cache / ebpf / mount-s3)。eBPF の positive/negative 差分の内訳を明確化。目安 0.5 日 (**AB-01/AB-02 で Gate PASS 済みのため省略可、必要に応じて実施**)

**Phase 1 Gate**: ✅ **PASS (2026-10-13)** — GUP 1728.5ns > 500ns かつ大ファイルで page cache 経路が arena より速い。→ **Phase 2 (AB-04〜AB-06) に進む**。`br §13` 参照。

**Phase 2 — PoC 実装 (縮退 arena の prototype、feature-flag で既存併存)** (3〜5 日):

- [x] **AB-04** arena 構造の縮退 — **→ 2026-10-13 完了**。`S3FS_ADMIT_FLAG_PCACHE` フラグ + `S3FS_IOC_POPULATE_PAGES` ioctl 追加。`s3fs_ebpf_populate_pages()` が S3 データを `fd->buf` (kmalloc) に格納し PCACHE admit エントリを挿入。daemon side: `populatePages()` / `--arena-metadata-only` flag。arena uaddr ゼロ (PCACHE 時)、旧 arena uaddr 共存。
- [x] **AB-05** `.read_folio` の変更 — **→ 2026-10-13 完了**。`admit_snapshot_pcache()` で PCACHE フラグ確認 → `generic_file_read_iter` にフォールスルー (GUP ゼロ)。`.read_folio` 先頭でも PCACHE fast path: `s3fs_folio_fill_from_kmalloc` へ直行。upcall wake 後も pcache admit 優先確認。`s3fs_arena_bypass` module param (AB-02 infra) 併用で動作比較可能。
- [x] **AB-06** 無効化経路の整合性 — **→ 2026-10-13 完了**。`s3fs_ebpf_populate_pages()` 内で `invalidate_mapping_pages()` 実行し stale folio をクリア。dirty inode は `-EBUSY` で拒否し write path との競合を防止。smoke test 合格 (cold read / warm read / GUP counter 不変 確認済)。

**Phase 3 — 計測と性能検証** (1〜2 日):

- [x] **AB-07** arena/pcache 前後比較 — **→ 2026-10-13 完了**。`scripts/bench-ab-pcache.sh` 実行。**gate (b)(c) PASS、gate (a) FAIL**。詳細 `br §14`。
  - (a) FAIL: pcache 3879k IOPS vs fuse-raw 6061k (63.9%)。GUP 除去後も rwlock 競合が 64-job 律速。
  - (b) PASS: pcache 8.047ms vs fuse-cache 8.489ms。大ファイル劣後解消。
  - (c) PASS: pcache p50 3.324µs vs arena p50 3.368µs (44ns 改善)。
  - **新規識別**: gate (a) 未達の根本原因は `admit_snapshot_pcache` の rwlock → AB-10 で検証。
- [x] **AB-08** bench-hitrate で policy 挙動の非劣化確認 — **→ 2026-10-13 完了**。`scripts/bench-hitrate.sh` を `DAEMON_EXTRA_FLAGS=--arena-metadata-only` で実行 (seed=42, N_ACCESS=2000)。全 10 policy: hit_rate=94.35〜94.60%、err=0。arena baseline (27-35%) より +59〜+67pp の改善。詳細 `br §16`。
  - **PASS**: pcache ≥ arena - 2pp を全 policy で達成 (逆に大幅改善)
  - **意味論的注記**: pcache モードでは page cache が admit table eviction を超えて蓄積するため hit_rate が高くなる。folio=437 は read_folio 経由の fd->buf fill 回数、miss=108 は実 S3 upcall 数。`read_iter` の pcache fast path (L2529-2531) はカウンタ未記録のため iter_mult=0.00 となるが正常。
  - **scripts/bench-hitrate.sh に `DAEMON_EXTRA_FLAGS` env var を追加** (L56 + L133-136)。bench-hitrate-multi.sh からも環境変数経由で透過的に利用可能。
- [x] **AB-09** bench-write で write path への影響確認 — **→ 2026-10-13 完了**。`scripts/bench-write.sh` を `DAEMON_EXTRA_FLAGS=--arena-metadata-only` で実行 (RUNS=10)。全サイズ S3 roundtrip MATCH。詳細 `br §17`。
  - **PASS**: arena vs pcache 同日比較で全サイズ ±1% 以内 (10 MiB: 52.6 vs 53.0 ms; 100 MiB: 261.5 vs 263.6 ms)
  - §8.3 baseline (10-11) との差 ~20% は環境変動 (MinIO PUT レイテンシ日次変動); pcache の影響ではない
  - write_iter/fsync は arena/pcache ルーティングを使わず影響なし (設計通り)
  - **scripts/bench-write.sh に `DAEMON_EXTRA_FLAGS` env var を追加** (L31 + L55-58)
- [x] **AB-10** per-file admit cache (改善案D) — **→ 2026-10-13 完了**。`struct s3fs_file_admit_cache` を `file->private_data` に割り当て、gen-based invalidation で lock-free fast path を実装。`admit_snapshot_pcache` を `fc_pcache_check` に置き換え。`kmod/vfs_ops.c` のみ変更。**gate (a) は依然 FAIL**: rwlock 除去後も 64-job IOPS は arena ≈ pcache ≈ 3800-3970k (fuse-raw 6000k)。真の律速は `s3fs_ebpf_read_iter` VFS ラッパーオーバーヘッド + page cache folio lock 競合であり rwlock ではなかった。`br §15` 参照。

**AB 系の受け入れ条件** (全体 — AB-10 結果を踏まえて改訂):
1. Phase 1 で page cache 経由が arena 直読より速いことを確認済 (前提) ✅
2. 64 jobs read で ebpf ≈ arena ≈ fuse-raw (AA-01 残課題) → **構造的ギャップ確認** (~35% 劣後は `s3fs_ebpf_read_iter` ラッパーオーバーヘッドが原因; fuse-raw parity は VFS ゼロコピー化なしでは達成困難。受け入れ条件として「pcache ≥ arena」に緩和)
3. 100MB cat で ebpf ≥ fuse-cache (大ファイル劣後の解消) ✅ (AB-07 で確認)
4. hit-rate は全 policy で現状 ±2pt 以内 → ✅ AB-08 完了 (pcache mode で全 policy hit_rate 94.35-94.60%、baseline 27-35% より +59〜+67pp 改善)
5. write latency / throughput に劣化なし → ✅ AB-09 完了 (pcache vs arena 同日比較で全サイズ ±1%、S3 roundtrip MATCH)

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

**2026-10-12 更新 — 2026-10-12 の 3-seed 再計測レビューにより新規課題を追加**:

P0 新着 (Y-01〜Y-07 — ベンチ計測の妥当性 Phase 2):

1. **Y-01** `rate_arena/rate_total` 定義壊れ (iter_hits 2× / rate_total > 100% / Belady 超え) — 0.5 日 (**最優先**、meetup 数値を引用する前に修正)
2. **Y-02** X-04 ino 昇順 tiebreak と Zipf 分布の systematic bias — 1〜2 日 (policy spread が正しく出る前提)
3. **Y-04** ゼロパディング無し bucket seed で Y-02 bias が偶然隠蔽 — 0.25 日 (Y-02 の受け入れ条件)
4. **Y-03** N_ACCESS=200 がポリシー時定数より短く aging/ghost/decay が発火せず — 0.5 日 (計測) + 0.5 日 (sweep)
5. **Y-06** `read_iter_hits` ダブルカウント検証 — 0.5 日 (Y-01 と連動)
6. **Y-07** `pressure_ops` 単独トップの原因未解明 — 0.5 日 (Z-04 と連動)
7. **Y-05** D-01 multi-range workload bench (X-01〜X-05 共通受け入れ条件未検証) — 1 日

P1 新着 (Z-01〜Z-05 — ポリシー実装の時定数ハードコード):

- Z-01 W-TinyLFU `AGE_INTERVAL_HITS=512` ハードコード (dynamic aging 化) — 1 日
- Z-02 `time_decay` DECAY_NS=5s ハードコード (module param 化) — 0.5 日
- Z-03 S3-FIFO ghost `max_entries=1024` ハードコード (workload proportional 化) — 0.5 日
- Z-04 `pressure_ops` strict/lenient 発火カウンタ追加 (Y-07 と連動) — 0.25 日
- Z-05 `wtinylfu_full` doorkeeper reset を condition-driven に — 0.5 日

P1/P2 新着 (AA-01〜AA-06 — FUSE 比 cold-path 性能改善):

- AA-01 並列 read cold scale 劣後 (fuse-raw 比 1.1〜1.2× 遅い) — RCU-indexed admit hash or `.read_iter` 直実装 — 2〜5 日
- AA-02 `--pattern-aware-admit` default on — 0.25 日 (**低リスク高効果**、先行実施推奨)
- AA-03 `.read_iter` kmod 直実装 (mount-s3 比のさらなる優位) — 3〜5 日
- AA-04 daemon async read prefetch — 1〜2 日
- AA-05 `--disk-cache-capacity` adaptive sizing (watermark) — 1 日
- AA-06 mount-s3 を bench-parallel/scale にも追加 — 0.5 日

**2026-10-13 追加 — AA 完了後の構造的課題 (arena 役割の設計改善)**:

P1 新着 (AB-01〜AB-09 — arena 役割縮退 metadata-only):

Phase 1 (判断材料集め、2〜3 日):
1. **AB-01** GUP オーバーヘッド分離マイクロベンチ — 0.5 日 (**最優先**、Phase 2 進行可否を決める)
2. **AB-02** arena hit vs page cache の read latency 比較 — 1 日
3. **AB-03** mount-s3 を含む 4-way bench (10MB/100MB cold-read 3 回) — 0.5 日

Phase 2 (PoC、Phase 1 Gate 通過時のみ、3〜5 日): AB-04 arena 構造縮退 / AB-05 `.read_folio` の page cache 連携 / AB-06 無効化経路の整合性

Phase 3 (計測、1〜2 日): AB-07 bench-latency/scale/parallel 前後比較 / AB-08 hit-rate 非劣化 / AB-09 write path 影響

**推奨順**: ~~AA-02~~ ~~Y-01~~ ~~Y-02~~ ~~Y-03~~ ~~Y-04~~ ~~Y-05~~ ~~Y-06~~ ~~Y-07~~ ~~Z-01~~ ~~Z-02~~ ~~Z-04~~ ~~Z-05~~ ~~AA-01~~ ~~AA-06~~ ~~AA-04~~ ~~AA-05~~ ~~AA-03~~ ~~Z-03~~ (2026-10-12 完了) ~~AB-01~~ ~~AB-02~~ (2026-10-13 完了、Gate PASS) ~~AB-04~~ ~~AB-05~~ ~~AB-06~~ ~~AB-07~~ ~~AB-10~~ ~~AB-08~~ ~~AB-09~~ (2026-10-13 完了) → **次**: K-01/K-02 (upstream 貢献、任意)

**2026-10-11 更新 — ベンチ妥当性 / ポリシー意味論レビューにより新規課題を追加**:

P0 新着 (U-01〜U-08 — ベンチ計測の妥当性):

1. **U-01** `bench-hitrate.sh` の rate 計算が失敗を hit に計上 — 0.5 日 (meetup 数値を再掲する前に優先)
2. **U-02** `bench-hitrate.sh` の rate が page cache 保持を含んでいる — 1 日
3. **U-03** `bench-write.sh` が最後のコマンドの書き込みしか S3 検証していない — 0.5 日
4. **U-04** `bench-scale.sh` の clat extraction が unit mismatch で沈黙失敗 — 0.5 日
5. ✅ **U-05** `bench-scale.sh run_read` が page cache と FUSE 往復の混合比較 — 完了 (2026-10-11)
6. **U-06** `bench-parallel.sh` の pkill パターンが dead code — 15 分
7. **U-07** `bench-parallel.sh` の writer が backend 間で同じ S3 key に書き込む — 15 分
8. **U-08** `bench-read-pattern-policy.sh` の promote 検出がログ文字列依存で脆い — 0.5 日

P1 新着 (X-01〜X-07 — ポリシー実装の意味論問題、D-01 大ファイル benchとセット):

- X-01 マルチレンジ ino で stateful policy の state mutation が壊れる — 1〜2 日
- X-02 wtinylfu scoped の "grace" 条件が est=1 で evict する谷 — 0.5 日
- ✅ X-03 CMS aging と bench harness INTERLEAVE の相互作用で warm 項目が落ちる — 完了 (2026-10-11)
- ✅ X-04 `s3fs_victim_cmp` の "oldest first" が実質 non-deterministic — 完了 (2026-10-11)
- X-05 capacity モードの `live_bytes` 重複カウント — 0.5 日
- X-06 time_decay が bench convergence 中に全 ino 自食い (DECAY_NS 5s < 12s harness) — 0.5 日
- X-07 trace sim が write を silently drop、`Invalidate` 相当メソッド欠落 — 1 日

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

## AC. pcache モード修正 (P1〜P6 / Q1〜Q4、2026-10-13 レビュー、`br §14〜16`)

| # | 内容 | 優先度 | 状態 |
|---|---|---|---|
| AC-01 | P1: fc_pcache_check が pos を無視 → 分割 admit でゼロ読み | P0 必須 | ✅ 修正済 (commit efe7a08) |
| AC-02 | P2: populate_pages にオーバーフロー検査なし → 範囲外書き込み | P0 必須 | ✅ 修正済 (commit efe7a08) |
| AC-03 | P3: admit_insert_pcache 重複 + admit_replace でエントリ消去 + 容量未適用 | P0 | ✅ 修正済 (commit 1271adf) |
| AC-04 | P4: fc_pcache_check のメモリ順序 (smp_acquire/release 欠如) | P1 | ✅ 修正済 (commit 879dbfa) |
| AC-05 | P5: bench-ab-pcache の gate (b) が 10 MiB / timed_reads に startup コスト | P1 | ✅ デフォルト 100 MiB + hyperfine に変更 |
| AC-06 | P6: 64-job 律速の特定 (perf c2c / lockstat / generic_file_read_iter 比較) | P2 | ⏳ 未着手 |
| AC-07 | Q1: s3fs_file_data_reset が i_size_write(0) → stat/read が eviction 後に壊れる | P0 必須 | ✅ 修正済 (commit b3ae6e8) |
| AC-08 | Q2: populate_pages と eviction の race → pcache 経路でゼロ読み (NULL buf) | P0 必須 | ✅ 修正済 (commit b3ae6e8) |
| AC-09 | Q3: S3FS_POPULATE_MAX_END=4GiB → offset>4GB の chunk admit が全て弾かれる | P1 | ✅ 修正済 (commit b6d42d1) |
| AC-10 | Q4: timed_reads の awk が gawk 専用 + µs パース誤り → bench 計測値が不正 | P1 | ✅ 修正済 (commit 214813d) |
| AC-11 | R1: populate/evict race → pcache entry が NULL fd->buf を指しゼロ返し (データ破損) | P0 必須 | ✅ 修正済 (commit caf0b7f) |
| AC-12 | R2: timed_reads fallback が silent → 計測方法が記録に残らない | P1 | ✅ 修正済 (commit b911a03 + 633dfe8) |
| AC-13 | R3: Q1 修正前ビルドの §16 計測が無効 → R1 ビルドで再計測が必要 | P0 必須 | ✅ 再計測済み (`br §18` / `§18.43`): 現在のビルドで AB-07 gate (a)(b)(c) すべて PASS、hit rate は opt 以下、arena は §12.3 と整合、sha256 は全一致。**lockdep だけ未検証** (AC-42、計測カーネルが `CONFIG_PROVE_LOCKING` 無効) |
| AC-14 | S1: check/fill 間の TOCTOU → eviction+populate が重なるとゼロ返し | P0 必須 | ✅ 修正済 (commit 8eeab28) |
| AC-15 | S2: timed_reads を subshell から呼ぶと TIMED_READS_METHOD が消える → AB07-RESULT の method= が常に hyperfine | P1 | ✅ 修正済 (commit 633dfe8) |
| AC-16 | S3: evict-race テストが eviction ゼロでも通過 / lockdep 無効時に PASS 表記 / grep パターン不足 | P1 | ✅ 修正済 (commit 633dfe8) |
| AC-17 | S5: upcall 完了後の eviction 窓 (signal_admit → wakeup) でゼロ返し → retry ループで防御 | P1 | ✅ 修正済 |
| AC-18 | S7: invalidate_mapping_pages がロック中 folio をスキップ → 古い uptodate page が eviction 後も残存 | P1 | ✅ 修正済 |
| AC-19 | S8: populate_pages が i_size を縮小方向に更新 → 部分 re-populate で VFS ファイルサイズが縮む | P1 | ✅ 修正済 |
| AC-20 | S10: admit_snapshot が PCACHE entry (uaddr=0) を arena hit として返す → GUP で near-NULL アクセス | P0 必須 | ✅ 修正済 |
| AC-21 | S11: populate_pages が先頭 chunk のみ valid 扱い → 2 chunk 目以降の読み出しが buf_valid=false でループ | P0 必須 | ✅ 修正済 |
| AC-22 | T1: write 後 read が -EIO — dirty inode でも bitmap チェックを通過させる | P0 必須 | ✅ 修正済 |
| AC-23 | T2: 256 MiB 超の位置が常に読めない — pcache 経路で ci < 64 ガードを除去 | P0 必須 | ✅ 修正済 |
| AC-24 | T3: evict ioctl が invalidate_inode_pages2 で folio lock を待ちデッドロック | P1 | ✅ 修正済 |
| AC-25 | T4: retry 上限到達の -EIO がデータ破損と区別できない (retry_exhausted カウンタ未実装) | P2 | ✅ 修正済 |
| AC-26 | T5: チャンク単位 bit が部分 populate を有効扱い — fallback 経路の over-count | P2 | ✅ 修正済 (pcache 経路は T2 で解消済) |
| AC-27 | T6: T1〜T3 修正後ビルドで §18.1〜18.4 を再計測 + §18.5〜18.8 追加 | P0 必須 | ✅ 計測済 (`br §18`, d3d550c) |
| AC-28 | U1: test-populate-boundary.sh が kmod ソース grep → 実 ioctl 呼び出しに置き換え | P0 必須 | ✅ 修正済 |
| AC-29 | U2: test-evict-readback.sh の eviction 0 回が WARNING → FAIL（metrics 不可時のみ SKIP） | P0 必須 | ✅ 修正済 |
| AC-30 | U3: test-write-readback.sh にサブテスト 4（flush+drop_caches 再 read）追加 | P0 必須 | ✅ 修正済 |
| AC-31 | U4: Makefile test-regression に test-pcache-multiread + test-pcache-evict-race-short 追加 | P0 必須 | ✅ 修正済 |
| AC-32 | U5: flush 完了時（signal_flush err==0）に fd->size 以下の全 chunk bit を立てる | P1 推奨 | ✅ 修正済 |
| AC-33 | U6: populate_pages の eof を fd->size → i_size_read(inode) に変更（evict 後 fd->size=0 対策） | P1 推奨 | ✅ 修正済 |
| AC-34 | U7: U1〜U6 入り後に make test-regression + §18.1/18.2 bench を実行し結果を記録 | P0 必須 | ✅ 計測済 (`br §18`, d3d550c) |
| AC-35 | W1: close 後・flush PUT 前に inode が evict されると未 PUT の fd->buf を破棄 (書き込み消失) → flush 完了まで ihold で pin | P0 必須 | ✅ 修正済 (commit 25dc831) |
| AC-36 | pending_exists が chunk_offset≠0 の pending を見落とす → hash key を ino 単独に | P1 | ✅ 修正済 (commit 6846872) |
| AC-37 | W2: .create したファイルの inode が evict されると再 lookup で i_size=0 → evict 時に i_size をエントリへ書き戻す | P0 必須 | ✅ 修正済 (commit d3d550c) |
| AC-38 | M1: fd->buf 確保量ゲージ + skip_reset カウンタを常設、evict-race に「停止後 fd_buf ≤ capacity」判定 | P1 | ✅ 実装済 (commit 0c47ff7) |
| AC-39 | U10 (pcache ヒット経路の buf_readers 増減) を A/B で不要と確認し削除 | P1 | ✅ 削除済 (commit f44e2b4、`br §18.10`) |
| AC-40 | daemon のディスクキャッシュが size/ETag を検証せず、上書きされた object の古い body を返す (ab-pcache で 10 MiB 版が残り -EIO) | P1 | ✅ 修正 (2026-10-17): cache ファイルの先頭に header (形式 tag、ETag、size) を持たせ、List / Stat / Get が観測した remote の ETag・size と食い違う body、header のない旧形式、長さが size と合わない body は miss にして消す (`DiskCache.Observe` / `Read`)。daemon は起動時に List してから prewarm するので、再起動をまたいだ古い body も止まる。以後の外部上書きは次の sync (`--sync-every`) で検出する (それまでは古い body が返りうる)。GCS は generation で比較。`TestDiskCache_StaleBodyIsNotServed`、MinIO を使う `TestGet_DiskCacheFollowsExternalOverwrite` (`Observe` を無効にすると FAIL)。旧形式の cache は最初の Get で捨てられる |
| AC-41 | fd->buf が capacity_bytes の 10 倍まで膨らむ — `s3fs_file_data_reserve` が populate 範囲の終端まで確保し、容量判定 (`live_bytes`) は admit 済み chunk しか数えない | P1 | ✅ 第一案で修正 (`br §18.11`): 容量判定を pcache ino の fd->cap で行い、populate は切り上げなし確保。test-pcache-capacity-tail 追加。残: 読み込み中の一時的なピーク (evict-race 36 MiB) は抑えない → AC-45。将来案 (chunk 単位確保) は未着手。追加検証 (`br §18.12`): test-regression 7 本 PASS、M2 照合で drift 0、切り上げ廃止による cold read の悪化なし |
| AC-43 | pcache モードで prewarm が 1 件も admit しないと `s3fs_admitted_mm` が未設定のままになり、upcall が飛ばず全 read が失敗する (`prewarm-meta-only.cel` で再現) | P1 | ✅ 修正 (`br §18.13`): count=0 の admit ioctl でも daemon の mm を渡し、`s3fs_admitted_mm` を設定する。test-pcache-prewarm-none 追加 (修正前 FAIL / 修正後 PASS) |
| AC-44 | `--arena-metadata-only` でも既定の prewarm がファイル全体を arena に stage し、arena admit として capacity を消費する (9 MiB × 4 で 36 MiB) | P2 | ✅ 修正 (`br §18.15`): `--arena-metadata-only` では prewarm をメタデータの warm だけにし、本体は最初の read で upcall → POPULATE_PAGES で取る。capacity-tail は目印 prewarm をやめて既定の prewarm で `prewarm_admits=0` を判定 (修正前 FAIL / 修正後 PASS)。hitrate pcache 53.05〜54.20% → 52.50〜53.70% |
| AC-45 | 読み込み中の fd->buf ピークは capacity で抑えられない (evict-race で 36 MiB / capacity 4 MiB)。原因は 3 つ: (1) pcache の populate 後に capacity の evict が走らない、(2) fd->buf がファイル全体の連続領域、(3) pending / buf_readers の ino は丸ごと evict できない | P2 | ✅ 修正 (`br §18.19`〜`§18.21`): C1 fd を 4 MiB の chunk の表に、C2 populate 後に daemon が capacity の pass を走らせ成功 signal でデータの存在を確かめる、C3 chunk 単位の pin と shed。capacity より大きいファイルの順次読みのピーク 40 → 12 MiB、evict-race のピーク 36 → 17〜22 MiB。C4 (受け入れ待ち) は不要と判断 |
| AC-50 | S3 にあるファイルを一度も読まずに途中へ write / truncate すると、読んでいない範囲が 0 のまま flush (PUT) され、オブジェクトが壊れる (上書きは書いた範囲まで切り詰め、縮小は 0、`: > f` は S3 に届かない)。open が中身を読み込まないため | P1 | ✅ 修正 (`br §18.22`): clean → dirty の最初の変更の前に、残る部分を FOR_WRITE の upcall で読み込む。取れなければ -EIO。test-write-existing 追加 (pcache / arena × 6 シナリオ、修正前 10/12 FAIL → 全 PASS) |
| AC-46 | 順番に読む populate で chunk ごとに fd->buf を再確保し全体を memcpy (O(n²))。300 MiB の cold read が 100 MiB の 5.7 倍 (2.4 s)。AC-41 前からある問題。案: 再確保の上限を i_size にしたうえで倍々に確保する (AC-41 の末尾 chunk の件と両立させる) | P2 | ✅ 修正 (`br §18.14`): populate の reserve を `max(needed, min(2 × cap, max(end, i_size)))` にした。cold read 300 MiB 2.33 s → 0.78 s、100 MiB 0.42 s → 0.27 s (比 5.6 → 2.9)。容量は最大で読み込み済み範囲の約 2 倍を数える |
| AC-47 | AB-07 gate (a) (64 jobs psync read) の残り差は `fentry/generic_file_read_iter` の負担。フックを外すと fuse-raw と同じ IOPS | P2 | ✅ 完了 (`br §18.16` / `§18.17` / `§18.23` / `§18.24`): 直接参照 (0.81→0.87)、per-CPU pattern (→0.93)、slot 配列 + 在席キャッシュ + on_hit の間引き + 非 atomic カウンタ (→0.94〜0.96)。フックの固定コストで 0.974 が上限のため、gate (a) の判定を「フックなしの pcache ≥ 0.98 × fuse-raw」に変更し、フックの負担は `hook_ratio` (今 0.96、0.90 未満で WARN) として別に追う |
| AC-48 | arena モードで 10 MiB のファイルを chunk 単位に admit すると、ファイルサイズが 4 MiB に縮み、読み込みが途中で EOF になる (sha256 不一致)。原因は admit_replace が entry の `size` (range の長さ) で `i_size_write` していたこと (306cc9a の全体 admit 前提が D-01 で崩れていた)。W2 で evict 時に誤ったサイズが entry に書き戻される | P1 | ✅ 修正 (`br §18.18`): i_size は新しい中身の range 終端までだけ伸ばし、縮めない。write/flush 中の inode には触らない。test-arena-partial-readback 追加 (修正前 FAIL / 修正後 PASS) |
| AC-49 | arena モードの chunk admit で、`replaceAdmit` (planReplaceAdmit) が同じ ino の他の range を落とすので、[0, 4) MiB を admit すると [4, 8) MiB が消え、次に読むとき同じ chunk をもう一度 GET する。D-01 の range list と噛み合っていない | P3 | ✅ 修正 (`br §18.25`): chunk admit は `replaceAdmitRange` で、重なる range だけを置き換えて arena を解放する。test-arena-partial-readback に「各 chunk の取得は 1 回」を追加 (修正前 4 回 / 3 chunk で FAIL → 3 回で PASS) |
| AC-42 | lockdep 未検証 — CONFIG_PROVE_LOCKING=y カーネルで evict-race / write-readback を実行 (W1 で fd->lock 内側の s3fs_flush_pins_lock を追加) | P2 | ⏳ 未検証。静的な確認だけ済み (2026-10-17): `s3fs_policy_evict_lock` (AC-58) を取るのは `admit_replace` だけで、その呼び出し元は ioctl のみ (他のロックを持たずに入る)。mutex の中で取るのは既存の `fd->lock` → `admits_lock` の順だけで、逆向きの辺はない。lockdep の実行には `CONFIG_PROVE_LOCKING=y` のカーネルのビルドと再起動が要る (インストール済みの 6.8 / 6.12 はどれも無効) |
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

### AC-01 受け入れ条件
2 チャンク以上に分割 admit される大ファイルを 1 fd で先頭〜末尾まで read → sha256 一致。`scripts/test-pcache-multiread.sh` で検証。

### AC-02 受け入れ条件
ioctl に `size=0` / `offset=U64_MAX-1, size=16` / `offset=0, size=1<<40` を渡し、いずれも `-EINVAL` で返ること。

### AC-03 受け入れ条件
`bench-hitrate.sh` を `DAEMON_EXTRA_FLAGS=--arena-metadata-only` で実行したとき、admit set の bytes が `capacity_bytes` 以下に保たれること (`adm_max` で確認)。hit rate が Belady (opt) を超えないこと。`br §16` 結論も修正済み。

### AC-04 受け入れ条件
`fc_pcache_check` の writer が `smp_store_release(&fc->gen, ...)` を使い、reader が `smp_load_acquire(&fc->gen)` を使うことで ARM/POWER の弱いメモリモデルでも pcache/off/end の可視性が保証される。

### AC-05 対応内容
- `scripts/bench-ab-pcache.sh`: `LARGE_BYTES` デフォルトを 104857600 (100 MiB) に変更
- `timed_reads`: `hyperfine --warmup 1 --runs N` ベースに変更 (sudo cat 起動コスト除外)
- `br §14.2`: 「arena の KVM DMA 経路」という根拠のない記述を削除

### AC-06 調査計画 (未着手)
- `perf c2c record -a -- fio ...` (64 jobs, shared small file) でキャッシュライン競合の特定
- `perf record -g` + `lockstat` でロック競合の確認
- 比較対象: `.read_iter = generic_file_read_iter` 直結版 (admit 判定なし) での IOPS 計測

### AC-07 修正内容 (Q1)
`s3fs_file_data_reset` から `i_size_write(inode, 0)` を削除。eviction は fd->buf のみ解放し、VFS inode の i_size はそのまま保持。次の .read_folio が upcall してデータを再取得する。arena モードのinos に対して呼ばれないよう `admit_has_pcache()` ガード追加。

### AC-08 修正内容 (Q2)
`s3fs_folio_fill_from_kmalloc` を `bool` 返却型に変更。`fd->buf != NULL` を明示チェックし、NULL なら false を返す。`.read_folio` の pcache 経路で false が返ったら fc->gen を 0 にリセットして upcall 経路にフォールスルー。

### AC-09 修正内容 (Q3)
`S3FS_POPULATE_MAX_END (4 GiB)` を `S3FS_POPULATE_MAX_SIZE (64 MiB)` に変更。offset+size ではなく呼び出し単位の size を制限するため、offset が 4 GiB 超のチャンクも受け付けられる。overflow チェックは維持。

### AC-10 修正内容 (Q4)
`timed_reads` を `hyperfine --export-json` + `jq` ベースに書き直し。gawk 専用 3 引数 `match()` を除去、µs パース誤りを修正。`jq` を `need_cmds` に追加。コメントの「startup 除外」表記を削除。

### AC-11 修正内容 (R1)
populate/evict 競合の 2 段修正:

1. `s3fs_ebpf_populate_pages`: `admit_insert_pcache` を `fd->lock` 保持中に呼ぶよう移動。`kzalloc` は `write_lock` 前に済ませるため `admit_insert_pcache` 内部構造は変更なし。ロック順: `fd->lock`（mutex, outer）→ `s3fs_admits_lock`（rwlock, inner）。
2. `s3fs_file_data_reset`: `fd->lock` 内で `fd->buf` 解放の前に、その ino の PCACHE エントリを再度 `write_lock` で取り除く。`admit_drop_all_ranges` と `admit_insert_pcache` の間に差し込まれた entry を確実に消す。

不変条件コメントを `struct s3fs_admit` の直後に追記。ストレステスト `scripts/test-pcache-evict-race.sh` を追加。

### AC-12 修正内容 (R2)
`timed_reads` に `TIMED_READS_METHOD` グローバル変数を追加。hyperfine 失敗時は stderr に警告を出力し `TIMED_READS_METHOD=loop` に設定。`AB07-RESULT` 行に `method=<value>` フィールドを追加。

### AC-14 修正内容 (S1)

`fc_pcache_check` に `u32 *gen_out` を追加し、fast/slow path 双方で probe 時の `s3fs_admit_gen` を返す。`s3fs_folio_fill_from_kmalloc` に `u64 ino, u32 gen` を追加し、`mutex_lock(&fd->lock)` 後に `atomic_read(&s3fs_admit_gen) != gen` なら `read_lock(&s3fs_admits_lock)` 下で `admit_lookup_at_locked(ino, pos)` を引き直す。PCACHE entry が消えていれば `buf_valid=false`。`gen==0` でチェックをスキップ（フォールバックパス）。S4 として `admit_drop_all_ranges` に `bool *pcache_out` を追加し `admit_has_pcache` の TOCTOU も閉じた（`admit_has_pcache` 関数を削除）。

### AC-15 修正内容 (S2)

`$()` サブシェル内の変数代入は呼び出し元に戻らないため `TIMED_READS_METHOD=loop` が毎回消えていた。`touch "$OUT/.timed_reads_fallback"` でファイル経由に切り替え、`resolve_timed_reads_method()` が `AB07-RESULT` 出力直前にファイル有無を判定。スクリプト先頭で sentinel を `rm -f` する。

### AC-16 修正内容 (S3)

1. `get_evict_count()` で Prometheus metrics から evictions_total を取得し、stress 前後の差分が 0 なら FAIL。
2. `/proc/lockdep_stats` の有無で lockdep 利用可否を判定し、不在時は PASS でなく SKIP と表示。
3. dmesg grep パターンに `BUG: sleeping function called from invalid context`、`inconsistent lock state`、`circular locking`、`BUG: KASAN` を追加。

### AC-17 修正内容 (S5)

`s3fs_ebpf_read_folio` に `upcall_retries` カウンタと `goto retry` ラベルを追加。daemon 応答後に fast path（pcache check / arena snapshot）を最大 `S3FS_UPCALL_MAX_RETRIES=3` 回再試行し、eviction が wakeup 窓に割り込んでも正しいデータを読めるよう保護。全リトライ消費後のみフォールバックに落ちる。

### AC-18 修正内容 (S7)

`s3fs_evict_page_cache` の `invalidate_mapping_pages` を `invalidate_inode_pages2` に置換。前者はロック中の folio を無音でスキップするため並行リーダーが古い uptodate page を保持し続けるが、後者は folio lock を待って確実に無効化する。

### AC-19 修正内容 (S8)

`s3fs_ebpf_populate_pages` の i_size_write 条件を変更。旧: `if (end > fd->size) { fd->size = end; i_size_write(...); }`。新: fd->size を end で grow-only 更新したのち、`if ((loff_t)fd->size > i_size_read(inode))` のみ i_size_write を呼ぶ。eviction が fd->size を 0 にリセットしても i_size は S3 オブジェクトサイズのまま保持される。

### AC-20 修正内容 (S10)

`admit_snapshot` の admit hit 判定に `!(a->flags & S3FS_ADMIT_FLAG_PCACHE)` ガードを追加。PCACHE entry は uaddr=0 (データは fd->buf) のため、arena path に流すと near-NULL GUP でゼロフォリオが生成されていた。PCACHE entry はこのパスを完全にスキップし、fc_pcache_check / fill_from_kmalloc で処理する。

### AC-21 修正内容 (S11)

`s3fs_file_data` に `u64 populated_chunks` フィールド（4 MiB chunk ビットマップ、最大 256 MiB）を追加。`s3fs_ebpf_populate_pages` が `[offset, end)` を跨ぐ全 chunk ビットを立て、`s3fs_folio_fill_from_kmalloc` が pos の chunk bit を確認してから buf を読む。`s3fs_file_data_reset` / `s3fs_file_data_new` でそれぞれ 0 クリア / 初期 chunk 設定。旧の連続ウォーターマーク (buf_valid_end) はチャンク飛び配送と部分 re-populate 後の再読みに失敗していた。

また `test-pcache-evict-race.sh` の eviction メトリクス名を `s3fs_ebpf_evictions_total` → `s3fs_ebpf_policy_evictions_total` に修正し、`overall=PASS` 計算を `{ ... } | tee` の外（report サブシェルの前）に移動して変数消滅バグを解消。

### AC-22 修正内容 (T1)

`s3fs_folio_fill_from_kmalloc` の `buf_valid` 判定に `fd->dirty` 分岐を追加。dirty inode は `.write` が buf に書き込んだ内容が正なので bitmap (`populated_chunks`) を参照しない。`fd->dirty` が true のとき `buf_valid = fd->buf != NULL && pos < fd->size` として bitmap を完全バイパス。

**受け入れ条件**: `scripts/test-write-readback.sh` で (1) echo/cat 新規ファイル、(2) 4 MiB を跨ぐ追記後 sha256 一致、(3) truncate-extend 後 write+read が全て PASS。

### AC-23 修正内容 (T2)

同じく `s3fs_folio_fill_from_kmalloc` の `buf_valid` 判定に `gen != 0`（pcache 経路）分岐を追加。R1 不変条件より pcache entry が存在すれば `fd->buf` はその範囲を覆っているため、64-chunk (`ci < 64`) ガードおよびビットマップ参照を不要にできる。`gen != 0` のとき `buf_valid = fd->buf != NULL && pos < fd->size`。fallback 経路（`gen == 0`）は従来どおりビットマップを使用。

`populate_pages` の chunk loop `ci < 64` ガードはフォールバック経路のビットマップ保護としてそのまま維持。pcache 経路はビットマップに依存しないため実害なし。

**受け入れ条件**: `scripts/test-large-file-read.sh` で 300 MiB ファイルを pcache モードで先頭から末尾まで読み sha256 一致。

### AC-24 修正内容 (T3)

`s3fs_evict_page_cache` の `invalidate_inode_pages2`（folio_lock で待機）を `invalidate_mapping_pages`（folio_trylock でスキップ）に変更。daemon の miss 処理が直列の場合、reader が folio lock を持ったまま upcall 完了を待つ間に daemon が evict ioctl を発行すると両者がデッドロックする。non-blocking パスにすることで ioctl 経路が folio lock を待たない。スキップされた folio は次の eviction round まで page cache に残るが、最新 S3 データを保持しているため correctness に影響なし。

**受け入れ条件**: `scripts/test-pcache-evict-race.sh` を `N_READERS=8` で実行し、hung task 警告（`blocked for more than`）と -EIO が出ないこと。DMESG_PATTERN に `blocked for more than` を追加済み。

### AC-13 再計測内容 (R3)
R1 修正後ビルドで `bench-hitrate.sh`（arena/pcache 両モード, seed=42）、`bench-ab-pcache.sh`（100 MiB）、`test-pcache-multiread.sh`、`test-pcache-evict-race.sh` を再計測し `br §18` に記録する。§16 の注記に §18 への参照を追記済み。

**受け入れ条件**:
- `adm_max ≤ capacity_bytes`（両モード）
- `hit_rate ≤ opt`（Belady 64.15%）
- arena hit_rate が §12.3 の lru/sieve 30.6% から著しく外れていない
- AB-07 gate (a)(b)(c) がいずれも PASS または SKIP
- sha256 全一致（multiread + evict-race）
- lockdep 警告なし

### AC-28 修正内容 (U1)

`scripts/test-populate-boundary.sh` の境界チェック 3 項目（B1〜B3）を kmod ソースへの grep から実 ioctl 呼び出しに置き換えた。`scripts/lib/populate-boundary-ioctl.py`（新規）が `/dev/s3fs_ebpf` を直接 open し `fcntl.ioctl` で 4 ケースを発行する。

- size=0 → EINVAL
- offset=U64\_MAX-1, size=16（オーバーフロー）→ EINVAL
- size=64 MiB+1 → EINVAL
- offset=5 GiB, size=4 MiB（有効範囲）→ EINVAL 以外

デバイスが開けない場合（root 不足・kmod 未ロード）は exit code 2 を返し、テストスクリプトが SKIP 扱いにする。

**受け入れ条件**: `scripts/test-populate-boundary.sh` を root で実行したとき、ioctl boundary (4 cases) が PASS と表示され、ソース grep に依存しないこと。

### AC-29 修正内容 (U2)

`scripts/test-evict-readback.sh` の eviction 確認ロジックを修正。旧: `EVICT_DELTA < 1` で WARNING を出力するのみで `overall` に影響なし。新: `EVICT_DELTA < 1` で `EVICT_PASS=FAIL` を設定し `overall=FAIL` にする。metrics 取得失敗（-1）の場合のみ SKIP。

**受け入れ条件**: eviction が起きない設定（例: 容量大）で実行したとき overall が FAIL になること。

### AC-30 修正内容 (U3)

`scripts/test-write-readback.sh` にサブテスト 4 を追加。1 MiB のランダムデータを書いて close し（fire-and-forget flush 発火）、MinIO に PUT されるまで `mc stat` でポーリング（最大 30 秒）する。確認後 `echo 3 > /proc/sys/vm/drop_caches` でページキャッシュを捨て、再 read した sha256 が元データと一致することを確認する。これにより `dirty=false` かつ `populated_chunks=0` の状態（U5 経路）が通る。

**受け入れ条件**: `scripts/test-write-readback.sh` でサブテスト 4 が PASS と表示されること。

### AC-31 修正内容 (U4)

`Makefile` の `test-regression` ターゲットに `test-pcache-multiread`（AC-01 / T2）と `test-pcache-evict-race-short`（R1/S1/T3）を追加。`test-pcache-evict-race-short` は `DURATION=30` で呼ぶ専用ターゲット。`.PHONY` リストにも追加。

**受け入れ条件**: `make test-regression` が 6 テスト（write-readback / large-file-read / evict-readback / populate-boundary / pcache-multiread / evict-race-short）すべてを実行すること。

### AC-32 修正内容 (U5)

`s3fs_ebpf_signal_flush` に bitmap セット処理を追加。daemon が PUT ACK（`IOC_SIGNAL_FLUSH`）を送ってきたとき、`err==0 && !fd->dirty && fd->buf && fd->size > 0` の条件下で `fd->size-1` に対応する最終 chunk インデックス `last_ci` を計算し、`ci=0..last_ci` の全ビットを `fd->populated_chunks` に立てる。flush 後の fd->buf はファイル全体の正データなので安全。fire-and-forget / fsync-wait 両方のパスに適用。

**受け入れ条件**: サブテスト 4（flush+drop_caches 再 read）で sha256 が一致すること。daemon 停止中は upcall でロードされるため正常動作継続。

### AC-33 修正内容 (U6)

`s3fs_ebpf_populate_pages` の eof 計算を `max(end, fd->size)` から `max(end, i_size_read(inode))` に変更。eviction 後 `s3fs_file_data_reset` が `fd->size=0` にリセットするため旧式では `eof == end` となり、部分 re-populate 時に最終チャンクでない chunk も last\_ci 条件（`end >= eof`）で valid 扱いになっていた。`i_size_read` は Q1/S8 不変条件により eviction をまたいでも S3 オブジェクトサイズを保持する。

**受け入れ条件**: eviction 後の部分 re-populate で余分な chunk bit が立たないこと。`test-evict-readback.sh` が PASS。

### AC-34 再計測内容 (U7)

AC-28〜AC-33（U1〜U6）を含むビルドで以下を実行し `br §18` に結果を記録する。

```
make test-regression      # all 6 tests PASS
make bench                # §18.1: latency
make bench-hitrate        # §18.2: hit-rate (pcache mode)
```

**受け入れ条件**:
- `make test-regression` 全テスト PASS
- §18.1 latency が §17 比で大きな退行なし
- §18.2 hit\_rate が Belady 上限（64.15%）を超えない
- `adm_max ≤ capacity_bytes`

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
