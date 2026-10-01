# FS 拡張の POSIX 準拠と実用性 — 設計要点

E-01 / E-02 / E-03 / D-01 / D-02 の各拡張で「POSIX 準拠として何を
保証し、S3 という eventually-consistent object store の上でどう
折り合いをつけたか」を 1 枚にまとめた資料。実装細部は各 commit 参照。

- E-01: blocking fsync (commit `e3f1018`)
- E-02: unlink + rename (commit `93f5f10`, `ebb5c6f`)
- E-03: flush coalescing (commit `0049303`)
- D-01: admit range list + partial admit (commit `b0a62c7`, `c419541`)
- D-02: arena free-list (commit `c419541`)

## 一覧: POSIX / 実用性 のスコアカード

| 機能 | POSIX 保証 | 保証しないこと | 実用性 |
|---|---|---|---|
| **read** | ✅ 正しい bytes を返す (folio 単位) | 分散 daemon 間の一貫性 | Path A/B/C の 3 段、hit で IPC 0 |
| **write** | ✅ 書けたら成功、`fd->size` 更新 | close 時点の永続性 (fsync が要る) | scratch buffer + release で PUT |
| **fsync (E-01)** | ✅ **PUT 完了まで blocking**、S3 durable 後 return | daemon crash 中の耐性 (30s timeout) | 91ms/PUT@MinIO、8μs (no-op) |
| **unlink (E-02)** | ✅ 即 dcache から消える、open fd は継続動作可 | 他 daemon の view (S3 eventual) | POSIX rm が通る |
| **rename (E-02)** | ⚠ 同 dir + RENAME_NOREPLACE のみ、原子性なし | atomic rename (S3 に無し) | mv 通る、edge case はコメントに明示 |
| **close** | ✅ fd 解放、`fd->dirty` fire-and-forget PUT | 完了保証 (fsync が要る) | 8μs、非同期 PUT |
| **coalescing (E-03)** | ✅ latest write wins、fsync durability 保持 | 中間 revision の永続化 | 5× write+close → 1 PUT |
| **partial admit (D-01)** | ✅ read 結果 byte-exact (SHA256 match) | file 全体を arena に載せる保証 | 128MiB arena に 100MiB+ blob |
| **arena free-list (D-02)** | ✅ 変わらず (バイト内容不変) | 断片化最小化 (compaction 未) | monotonic 増加を解消 |

## 個別解説

### E-01 blocking fsync — POSIX の中で最も厳しい呼出

**POSIX 要件**: `fsync(2)` は「メモリからストレージへの transfer が
完了するまで block する」。応用は fsync 後に crash しても書いた
データが復元されることを期待する (database WAL、editor autosave)。

**実装**:
```
kmod .fsync
  → flush_pending register (per-ino completion object)
  → flush_enqueue({size, name, flags=FLUSH_WAIT})
  → wait_for_completion_interruptible_timeout(30s)
  → err (0 / -EIO / -EINTR / -ENOMEM) を fsync return に伝搬

daemon
  → drainFlushQueue: WAIT flag セットなら handleFlush 即時 (coalescer bypass)
  → handleFlush の deferred で signalFlush(ino, err) を発火
  → kmod: complete_all(pending) → wait_for_completion が返る
```

**POSIX として保証していること**:
- fsync 返り値 0 なら S3 に PUT 済み (mc cat で観測可能)
- PUT 失敗は fsync に `-EIO` として伝搬 → 応用が retry 判断できる
- 割込 (SIGINT) は `-EINTR`、応用が明示 handle

**保証しないこと**:
- daemon crash して signalFlush が来ない → 30s timeout → `-EIO`
  (POSIX 的には durability failure、fsck 相当は無し)
- fsync 経路とは別に、`close(2)` は非同期 (POSIX 準拠、close で
  durability は保証されない)

### E-02 unlink / rename — S3 に無い操作を emulate

**unlink (POSIX)**: `unlink(2)` は「dentry を消す + link count -1、
open fd は継続動作、link count が 0 になり参照が無くなったら inode
削除」。

**実装**:
```
kmod .unlink
  → delete_enqueue({ino, name})
  → local_entry_remove(name)  (dyn は sync round が cleanup)
  → admit_drop_ino_locked(ino)
  → drop_nlink(inode) + inode_set_ctime_current

daemon runDeleteLoop
  → ioctl(POP_DELETE) → event
  → s3c.Delete(key) [minio-go RemoveObject]
  → S3 側からも消える
```

**POSIX として保証**:
- 即 lookup で ENOENT (dcache から消える)
- open fd は継続動作 (Linux VFS が inode 保持)
- link count が 0 で generic_delete_inode → evict_inode 発火

**保証しないこと**:
- 分散: 他 daemon が sync round 前にリストを見ると deleted key が
  まだ見える (S3 eventual)
- daemon crash 中に unlink → kmod queue が失われる → S3 上の
  object 残存 (persistent delete log は backlog H-01)
- bootstrap file (`hello.txt` 等 kmod compile-time) の unlink は
  local list に無いので dyn 側だけ削除、次 mount で復活

**rename (POSIX)**: `rename(2)` は "atomic move"。同 fs 内なら
「同一 inode に別 name を紐付けて古い name を消す」を atomic に。

**S3 の壁**: S3 には rename op が無い。CopyObject + Delete で
emulate するしかない → **atomicity が失われる**。

**実装**:
```
kmod .rename
  → flags != 0 & RENAME_NOREPLACE 以外は EINVAL
  → old_dir != new_dir は EXDEV (flat namespace)
  → rename_enqueue({ino, old_name, new_name})
  → local_entry_rename (local list の name field 上書き)
  → inode_set_ctime_current
  → VFS が d_move で dcache を rewire

daemon runRenameLoop
  → ioctl(POP_RENAME) → both names
  → s3c.Rename: mc.CopyObject(old→new) + mc.RemoveObject(old)
  → registerIno(newKey, ino)  (以後の PUT は新 key で)
```

**POSIX として保証**:
- 同 dir 内の rename は成功 (`mv old new` 動作)
- ctime 更新 (`stat` で確認可能)
- inode 番号は保持 (open fd の identity 変わらず)

**保証しないこと**:
- **atomicity**: CopyObject 成功 + Delete 失敗の window で old/new
  両方存在。POSIX の atomic rename は不能
- cross-directory rename → `EXDEV` (subdir 未対応、G-01 まで)
- `RENAME_EXCHANGE` / `WHITEOUT` → `EINVAL`
- 同名上書き (`new` が既存) は S3 の CopyObject が上書きする挙動に
  依存、original が消えるまでの window で読者が古い版を見る可能性

### E-03 flush coalescing — 実用性最適化 (POSIX 侵害なし)

**動機**: `touch` を N 回連打すると N 回の PUT。実質的な最終状態は
1 つなのに帯域/コストを N 倍消費。POSIX に coalescing を禁じる
文言は無いが、fsync semantics を壊さないよう慎重な設計が要る。

**実装**:
```
kmod .fsync: FLUSH_WAIT flag を立てて enqueue
kmod .release/.create: flag=0 で enqueue (coalescable)

daemon drainFlushQueue:
  if WAIT flag set:
    coal.CancelIno(ino)         ← stale pending drop (bug fix)
    handleFlush 即時
  else:
    coal.submit(ino, event)      ← 200ms debounce

coalescer:
  submit: 既存 pending あれば event 置換 + timer.Reset
          (最新 event が勝つ、fd->size は同 file session 内 monotonic
           増加なので古い event を破棄しても損失なし)
  fire (timer): handleFlush で PUT
  CancelIno: fsync 前に古い pending を drop、fsync PUT を後から
             coalescer PUT が上書きしないように

Shutdown: 全 pending を synchronous drain (ctx-cancel でデータ損失なし)
```

**POSIX として保証**:
- fsync は latency 影響なし (bypass path)
- fsync 後の close は fd->dirty=false で kmod no-op、上書き無し
- 最終 write が S3 に到達する (latest wins)
- flush_events counter で観測可能

**保証しないこと**:
- **中間 revision の永続化**: `write "A" + close + write "B" + close`
  を 200ms 以内にやると "A" は S3 に届かず "B" だけ残る。応用が
  中間 state を残したいなら明示 fsync が要る
- **PUT の順序保証**: coalescer は per-ino だが、複数 ino が同時に
  fire するとネットワーク的な順序は非決定
- **debounce window** = 200ms 固定。workload によっては短すぎ/長
  すぎ (autotune は未実装)

### D-01 admit range list + partial admit — 大 blob 対応

**動機**: kmod arena は 128 MiB 固定 (BPF map の実装制約)。1 file
を必ず arena に丸ごと載せる shape だと 100 MiB+ blob で破綻。

**設計**:
- 同一 ino に**複数** admit entry を hash bucket に持てるように
  (`struct s3fs_admit_entry` に `offset` field)
- kmod `admit_lookup_at(ino, pos)` が `[offset, offset+size)` に
  pos が入る range を返す
- daemon は miss offset 周辺の chunk (4 MiB) だけ range GET

**POSIX として保証** (これは重要):
- 読み出し bytes は S3 原本と **byte-exact** (SHA256 match 済)
- file 全体の一貫した view (`stat` で正しい `i_size` を返す —
  sync entry の authoritative size を使う)
- POSIX が read の "全 file が memory に載ってる保証" を要求して
  ないので、部分 load でも準拠

**保証しないこと**:
- range 境界を跨ぐ folio: 現在 `avail < fs_size` で残りは zero-fill、
  正しく次 range に接ぐ実装は followup
  (単一 folio 4KB は chunk 4MB 境界にまず跨がないので実害少)
- prewarm 時の partial: 現在 `prewarm()` は whole-file GET のまま
  なので、default CEL prewarm 閾値 (32 MiB) 以下は arena 直入り、
  以上は path C で partial admit 経路に

### D-02 arena free-list — sliding window の基礎

**動機**: eviction で admit hash から entry は消えるが、arena bytes
はそのまま。high-water offset は monotonic 増加 → 長時間稼働で
arena 枯渇。

**設計**:
```
Programs.arenaFree []arenaChunk  ← evict した range を保持

AdmitContent(data):
  1. free-list に data.size 以上の chunk があれば first-fit reuse
     (最小 fit を選んで余りは free-list に戻す)
  2. 無ければ bump allocator (cacheNextOff) を進める

drainEvictionsLoop → dropAdmits(evicted, progs):
  各 evicted admit の (uaddr - arenaBase, size) を FreeArenaChunk
```

**保証**:
- 変わらず正しい bytes を返す (arena byte 内容は変わらない)
- arena capacity 到達までは write-and-forget、free-list で再利用可能
- metrics `arena_free_bytes` / `arena_free_chunks` で可視化

**保証しないこと**:
- adjacent chunk merge (compaction) 未実装 → 断片化は live evict
  数で bounded、定期 compaction sweep は followup
- ABA-safe な free-list: 現在 mutex + slice、多量並列 evict では
  contention の可能性

## 実用性 — production に向けた既知 gap

短くまとめる:

1. **daemon crash resilience**: kmod-side queue (flush/delete/rename)
   は memory-only。daemon crash で失う。persistent log は H-01。
2. **atomic rename**: S3 が無い以上 fundamental。分散 lock / lease
   ベースの pseudo-atomic は別 layer で。
3. **cross-directory** (subdir): G-01 で subdir 対応後に rename の
   EXDEV を external copy に格上げ。
4. **error surface**: 現在 `-EIO` / `-EINTR` / `-ENOMEM` / `-ENOENT`
   / `-ENODATA` のみ。SLA 側で S3 の詳細 errno (throttle / access
   denied) を区別したい場合は daemon 側 mapping 追加。
5. **write-through / write-around 選択**: 現在 write-back only。
   `docs/design/policy-engine-design.md §7` の non-goal。
6. **read amplification**: partial admit の chunk size (4 MiB) は
   固定。random small read で無駄が出る。sequential prefetch や
   adaptive chunk sizing は followup。
7. **arena capacity autotune**: 128 MiB 固定。workload に応じた
   動的拡張は BPF 側の変更が要る (bpf_arena_alloc_pages 拡張)。

## Meetup narrative 上の位置付け

Week 4 まで: 「PoC — read が動く、write は best-effort」
E-01 〜 D-02 で: 「PoC → shell-scriptable POSIX FS」

具体的に何が変わったか:
- `mv`, `rm`, `fsync -f` が通る (E-01/E-02)
- 100 MiB+ blob を 128 MiB arena で扱える (D-01/D-02)
- rapid touch/close の PUT 爆発が消える (E-03)

これは「PoC の実用範囲を拡げる週」で、Meetup narrative の
"policy engine" 主軸を補強する形。
