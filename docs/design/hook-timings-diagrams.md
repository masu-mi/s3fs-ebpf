# キャッシュ操作のフックポイントと重要タイミング別コンポーネント図

s3fs-ebpf の read/write/eviction/durability path を「どの層に
どの操作が来て、何が連鎖するか」でオブジェクト図化したもの。
コード追跡や Meetup で「ここで policy が呼ばれる」を指す用途。

> 2026-10-18 注: 図は arena モード (Path B / C) のもの。pcache モードでは admit が `POPULATE_PAGES` (kmod の chunk + PCACHE entry) になり、`.read_folio` は arena の GUP ではなく kmod の chunk から folio を埋める。policy のフック (末尾の一覧) は両モード共通。pcache モードの流れは `docs/design/pcache-mode.md` §4〜§6。

## 全体コンポーネントの見取り図

```
┌───────────────────────────────────────────────────────────────────────┐
│                             App / userspace                            │
│  (cat, dd, python os.open, rm, mv, fsync)                             │
└──────┬────────────────────────────────────────────────────────────┬───┘
       │  syscall (read/write/open/close/fsync/unlink/rename)       │
       ▼                                                            │
┌──────────────────────────────────────────────────────────────┐    │
│                    Linux VFS + page cache                     │    │
│  simple_getattr / generic_file_read_iter / d_move / etc.      │    │
└──────┬───────────────────────────────────────────────┬────────┘    │
       │ .read_folio / .write / .create /              │            │
       │ .release / .fsync / .unlink / .rename         │            │
       ▼                                               ▼            │
┌───────────────────────────────────────────────────────────┐        │
│                     kmod (s3fs_kfunc.ko)                  │        │
│                                                            │        │
│  ┌────────────┐  ┌──────────────┐  ┌───────────────┐      │        │
│  │ vfs_ops.c  │  │ admit hash   │  │ struct_ops    │      │        │
│  │ file/dir/  │◀▶│ (range list) │◀▶│ policy vtable │─────eBPF     │
│  │ super ops  │  │ ino → range  │  │ on_admit/     │      │  │    │
│  └─────┬──────┘  └──────────────┘  │ on_evict/     │      │  │    │
│        │                            │ on_hit/       │      │  │    │
│        │        ┌──────────────┐   │ on_pressure   │      │  │    │
│        ├────────│ pending list │   └───────┬───────┘      │  │    │
│        │        │ read-miss    │           │              │  │    │
│        │        │ upcall       │           │ KFunc call   │  │    │
│        │        └──────────────┘           ▼              │  │    │
│        │        ┌──────────────┐   ┌───────────────┐      │  │    │
│        ├────────│ flush pending│   │  Layer 0      │      │  │    │
│        │        │ fsync waiter │   │  primitives   │      │  │    │
│        │        └──────────────┘   │ admit_count / │      │  │    │
│        │        ┌──────────────┐   │ softcap /     │      │  │    │
│        └────────│ flush /      │   │ ino_size /    │      │  │    │
│                 │ delete /     │   │ time_ns /     │      │  │    │
│                 │ rename queue │   │ evictions_    │      │  │    │
│                 └──────┬───────┘   │ total         │      │  │    │
│                        │           └───────────────┘      │  │    │
└────────────────────────┼──────────────────────────────────┘  │    │
                         │ /dev/s3fs_ebpf ioctl                │    │
                         ▼                                     │    │
┌───────────────────────────────────────────────────────────┐  │    │
│                     Go daemon (bin/s3fs-ebpf)              │  │    │
│                                                            │  │    │
│  ┌───────────────┐  ┌────────────────┐  ┌─────────────┐  │  │    │
│  │ admit_arena.go│  │ flush_loop.go  │  │ delete_loop │  │  │    │
│  │ setAdmits /   │  │ + coalescer    │  │ rename_loop │  │  │    │
│  │ addAdmit /    │  │ (E-03)         │  │ (E-02)      │  │  │    │
│  │ dropAdmits    │  │                │  │             │  │  │    │
│  │ (D-02)        │  │ WAIT bypass    │  │             │  │  │    │
│  └───────┬───────┘  └────────┬───────┘  └──────┬──────┘  │  │    │
│          │                   │                  │         │  │    │
│  ┌───────▼───────────────────▼──────────────────▼──────┐  │  │    │
│  │ internal/ebpf/loader.go — arena wrapper             │  │  │    │
│  │  AdmitContent (bump alloc + free-list D-02) /        │  │  │    │
│  │  FreeArenaChunk / ArenaFreeStats                     │  │  │    │
│  └───────────────────┬──────────────────────────────────┘  │  │    │
│                      │                                      │  │    │
│  ┌───────────────────▼──────────────────────────────────┐  │  │    │
│  │ internal/s3/client.go — Get / GetRange (D-01) / Put /│  │  │    │
│  │ Delete (E-02) / Rename (E-02)                         │  │  │    │
│  └───────────────────┬──────────────────────────────────┘  │  │    │
│                      │                                      │  │    │
│  ┌───────────────────▼──────────────────────────────────┐  │  │    │
│  │ internal/policyprog — BPF program loader + attach   │──┘  │    │
│  │ POST /policy/reload | POST /policy/eval             │─────┼─────┘
│  └──────────────────────────────────────────────────────┘     │
│  ┌──────────────────────────────────────────────────────┐     │
│  │ internal/policy — CEL engine (prewarm / policy /     │     │
│  │ metric extension)                                     │     │
│  └──────────────────────────────────────────────────────┘     │
└──────────────────────────────────┬────────────────────────────┘
                                   │ HTTP + AWS SDK
                                   ▼
                          ┌──────────────────┐
                          │  S3 / MinIO      │
                          └──────────────────┘
```

**縦の関係** (syscall → VFS → kmod → daemon → S3):
- 上から下へ流れるのが read/write/fsync 等の user 発生 event
- 下から上へ流れるのは admit_arena ioctl (daemon → kmod)、signal_flush
  (daemon → kmod)、eviction feedback (kmod → daemon)

**横の関係** (kmod ↔ BPF policy):
- kmod-side call site (on_admit / on_evict / on_hit / on_pressure) が
  struct_ops policy vtable を rcu_dereference で呼ぶ
- policy は KFunc 経由で kmod 内部状態を read (`s3fs_kfunc_*`)

以下、代表 8 タイミングを順に詳細化する。

---

## タイミング 1: mount + prewarm (daemon 起動時)

```
┌──App──┐        ┌──VFS──┐  ┌──kmod──┐        ┌──daemon──┐    ┌──S3──┐
│       │        │       │  │        │        │          │    │      │
│ mount │───────▶│fill_  │─▶│active_ │        │          │    │      │
│ -t    │        │super  │  │sb 設定 │        │          │    │      │
│s3fs_  │        │       │  │        │        │          │    │      │
│ebpf   │        │       │  │        │        │          │    │      │
│       │        │       │  │        │◀───────│fastpath  │    │      │
│       │        │       │  │        │ open   │開始      │    │      │
│       │        │       │  │        │        │          │    │      │
│       │        │       │  │        │        │syncOnce  │───▶│ List │
│       │        │       │  │        │        │◀─────────│    │      │
│       │        │       │  │        │◀───────│SYNC_     │    │      │
│       │        │       │  │        │        │ENTRIES   │    │      │
│       │        │       │  │        │        │          │    │      │
│       │        │       │  │        │        │prewarm() │    │      │
│       │        │       │  │        │        │ per      │───▶│ Get  │
│       │        │       │  │        │        │ entry    │    │      │
│       │        │       │  │        │        │  ↓       │◀───│      │
│       │        │       │  │        │        │AdmitContent (D-02:│  │
│       │        │       │  │        │        │  bump or │  free   │  │
│       │        │       │  │        │        │  reuse)  │  list)  │  │
│       │        │       │  │        │◀───────│ADMIT_    │         │  │
│       │        │       │  │        │        │ARENA     │         │  │
│       │        │       │  │        │        │ ioctl    │         │  │
│       │        │       │  │        │        │          │         │  │
│       │        │       │  │policy  │ ─BPF ─▶│struct_ops│         │  │
│       │        │       │  │on_admit│        │ vtable    │         │  │
│       │        │       │  │/ evict │◀───────│(sieve等)  │         │  │
│       │        │       │  │per ino │        │           │         │  │
└───────┘        └───────┘  └────────┘        └──────────┘         └──┘
```

**要点**:
- `syncEntries → ADMIT_ARENA → admit_replace` の順序で kmod-side hash が
  populate される
- kmod は `admit_replace` で on_admit + per-ino on_evict を BPF policy
  vtable に呼ぶ (softcap 超過なら vote 集計)
- prewarm は default で whole-file GET → AdmitContent は bump 側で
  arena に順に write (D-02 free-list はこの時点では空)

---

## タイミング 2: read hit (whole-file admit)

```
App              VFS              kmod                          BPF
│                │                │                              │
│ read(fd, buf,  │                │                              │
│ 4KB, pos=100)  │                │                              │
├───────────────▶│                │                              │
│                │simple_getattr  │                              │
│                ├──fentry──────▶ │                              │
│                │◀───────────────│fast_path_hits++ (BPF)        │
│                │                │                              │
│                │generic_file_   │                              │
│                │read_iter       │                              │
│                │                │                              │
│                │ page cache miss│                              │
│                │─.read_folio───▶│                              │
│                │  (pos=0)       │                              │
│                │                │admit_snapshot(ino, pos=0)    │
│                │                │  → admit_lookup_at(ino, 0)   │
│                │                │  → range {off=0, size=N} hit │
│                │                │  → uaddr, avail=N            │
│                │                │  → mm=daemon                 │
│                │                │                              │
│                │                │folio_hits++ (atomic)         │
│                │                │                              │
│                │                │s3fs_policy_on_hit(ino) ─────▶│ on_hit
│                │                │(fmod_ret + struct_ops 両方)  │ visited=1
│                │                │                              │ 等
│                │                │                              │
│                │                │fill_from_arena:              │
│                │                │  GUP(mm, uaddr) → page       │
│                │                │  memcpy(folio, page[pgoff],  │
│                │                │         fs_size)             │
│                │                │  unpin                       │
│                │                │                              │
│                │◀───────────────│(folio populated)             │
│                │                │                              │
│◀───────────────│copy_to_user    │                              │
│ buf ready      │                │                              │
```

**要点**:
- fentry (BPF fast_path_hits) は getattr context だけ観測、
  `.read_folio` 側の kmod atomic `folio_hits` が真の hit count
- `admit_snapshot(ino, pos)` は D-01 で range 対応、pos=0 の場合は
  [0, N) が hit
- `on_hit` は BPF policy に per-folio 通知 (SIEVE visited-bit,
  W-TinyLFU CMS bump 等)

---

## タイミング 3: read miss + partial admit (D-01)

```
App          VFS         kmod                       daemon          S3
│            │           │                          │               │
│ dd bs=1    │           │                          │               │
│ skip=25MB  │           │                          │               │
│ count=100  │           │                          │               │
├───────────▶│           │                          │               │
│            │read_folio │                          │               │
│            │(pos=25MB) │                          │               │
│            ├──────────▶│                          │               │
│            │           │admit_snapshot(ino, 25MB) │               │
│            │           │  admit_lookup_at → NULL  │               │
│            │           │ (whole file 未 admit)    │               │
│            │           │                          │               │
│            │           │pending_get_or_create(ino)│               │
│            │           │  → new completion        │               │
│            │           │                          │               │
│            │           │miss_enqueue({ino, offset │               │
│            │           │  =25MB, count=4KB})      │               │
│            │           │                          │               │
│            │           │wait_for_completion (2s)  │               │
│            │           │                          │               │
│            │           │                       ◀──│miss_reader    │
│            │           │                          │reads event    │
│            │           │                          │               │
│            │           │                          │handleReadMiss:│
│            │           │                          │ if size > 4MB:│
│            │           │                          │  chunk_off =  │
│            │           │                          │  24MB (align) │
│            │           │                          │ GetRange     ─┼▶│
│            │           │                          │ (24MB, 4MB)   │◀│
│            │           │                          │  body (4MB)   │ │
│            │           │                          │               │ │
│            │           │                          │AdmitContent   │ │
│            │           │                          │(D-02: 空なら  │ │
│            │           │                          │ bump、free-list│ │
│            │           │                          │ あれば reuse) │ │
│            │           │                          │ → off, uaddr  │ │
│            │           │                          │               │ │
│            │           │                          │addAdmit({ino, │ │
│            │           │                          │  uaddr, size= │ │
│            │           │                          │  4MB,         │ │
│            │           │                          │  offset=24MB})│ │
│            │           │◀─────────────────────────│ADMIT_ARENA    │ │
│            │           │(range 追加)              │ ioctl         │ │
│            │           │                          │               │ │
│            │           │policy on_admit/on_evict  │               │ │
│            │           │                          │               │ │
│            │           │                          │signalAdmit    │ │
│            │           │◀─────────────────────────│(ino, 0)       │ │
│            │           │complete_all(pending)     │               │ │
│            │           │  → 起きた                │               │ │
│            │           │                          │               │ │
│            │           │admit_snapshot(ino, 25MB) │               │ │
│            │           │  admit_lookup_at hits    │               │ │
│            │           │ range {off=24MB, size=4MB│               │ │
│            │           │  within=1MB              │               │ │
│            │           │  uaddr=base+1MB          │               │ │
│            │           │  avail=3MB               │               │ │
│            │           │                          │               │ │
│            │           │fill_from_arena (pos=0)   │               │ │
│            │           │  copy 4KB                │               │ │
│            │           │                          │               │ │
│            │◀──────────│(folio populated)         │               │ │
│◀───────────│copy_to_user                          │               │ │
```

**要点**:
- daemon が **path C upcall + partial S3 GET** で 100 MiB 全体を
  fetch せず 4 MiB chunk だけ arena に載せる
- kmod は `admit_snapshot(ino, pos)` で pos が chunk 内かを判定、
  chunk 相対 offset に uaddr を translate
- fill_from_arena は pos=0 相対で range-adjusted uaddr を読む
- 次回 pos が同 chunk 内なら hit、別 chunk (例 30 MB) なら再 miss
  → 別 chunk として追加 admit

---

## タイミング 4: write + close (coalesced) (E-03)

```
App          VFS       kmod                       daemon
│            │         │                          │
│ open       │         │                          │
│ O_TRUNC    │         │                          │
├───────────▶│         │                          │
│            │.create  │                          │
│            ├────────▶│flush_enqueue(0, flags=0) │
│            │         │                          │
│ write 7B   │         │                          │
├───────────▶│         │                          │
│            │.write   │                          │
│            ├────────▶│fd->buf 更新, fd->size=7  │
│            │         │fd->dirty=true            │
│            │         │                          │
│ close      │         │                          │
├───────────▶│         │                          │
│            │.release │                          │
│            ├────────▶│dirty=true → snapshot     │
│            │         │flush_enqueue(7, flags=0) │
│            │         │dirty=false               │
│            │         │                          │
│            │         │                       ◀──│drainFlushQueue
│            │         │                          │ picks event
│            │         │                          │ WAIT=0 →
│            │         │                          │ coal.submit
│            │         │                          │ pending[ino]=ev
│            │         │                          │ timer=200ms
│            │         │                          │
│ (repeat 5x, timer 毎回 reset)                   │
│                                                 │
│ ... 200ms 経過 ...                              │
│                                                 │
│            │         │                       ◀──│timer fires
│            │         │                          │ coal.fire(ino)
│            │         │                          │ handleFlush
│            │         │                          │  readDirty ────┐
│            │         │◀─────────────────────────│  READ_DIRTY    │
│            │         │(fd->buf → daemon buf)    │  ioctl         │
│            │         │                          │◀──────── 7B    │
│            │         │                          │ s3.Put(key,   │
│            │         │                          │        buf)   │───▶ S3
│            │         │                          │◀──────────    │◀───
│            │         │                          │ AdmitContent  │
│            │         │◀─────────────────────────│ addAdmit      │
│            │         │(admit 更新)              │               │
│            │         │                          │ signalFlush   │
│            │         │◀─────────────────────────│(ino, 0)       │
│            │         │(pending なし → ENOENT    │               │
│            │         │  → silent 扱い)          │               │
```

**要点**:
- 5 iterations で 6 event (1 create + 5 release) が発生、200ms
  debounce で最新のみが PUT
- **coalescer.CancelIno** が fsync path から呼ばれると (別タイミング)
  pending 破棄
- fsync flag (WAIT) は bypass path、この diagram には出ない

---

## タイミング 5: fsync (blocking) (E-01)

```
App          VFS        kmod                        daemon         S3
│            │          │                           │              │
│ fsync(fd)  │          │                           │              │
├───────────▶│          │                           │              │
│            │.fsync    │                           │              │
│            ├─────────▶│flush_pending_get_or_      │              │
│            │(pos=0..N)│  create(ino) → completion │              │
│            │          │                           │              │
│            │          │fd->dirty=true →          │              │
│            │          │  size=N snapshot          │              │
│            │          │  dirty=false              │              │
│            │          │                           │              │
│            │          │flush_enqueue(N,           │              │
│            │          │   flags=FLUSH_WAIT)       │              │
│            │          │                           │              │
│            │          │wait_for_completion_       │              │
│            │          │  interruptible_timeout    │              │
│            │          │  (30s)                    │              │
│            │          │                        ◀──│drainFlushQueue│
│            │          │                           │ picks event  │
│            │          │                           │ WAIT set:    │
│            │          │                           │ coal.CancelIno│
│            │          │                           │ (pending 破棄)│
│            │          │                           │              │
│            │          │                           │ handleFlush  │
│            │          │                           │  readDirty ──┐
│            │          │◀──────────────────────────│  READ_DIRTY  │
│            │          │(fd->buf → daemon buf)     │  ioctl       │
│            │          │                           │◀─────── N B  │
│            │          │                           │              │
│            │          │                           │ s3.Put ──────┼▶ PUT
│            │          │                           │  (blocking)  │◀
│            │          │                           │ (~90ms MinIO)│
│            │          │                           │              │
│            │          │                           │ addAdmit     │
│            │          │◀──────────────────────────│  ADMIT_ARENA │
│            │          │(admit 更新、次 read が    │              │
│            │          │ 新 bytes を返す)          │              │
│            │          │                           │              │
│            │          │                           │ signalFlush ─┐
│            │          │◀──────────────────────────│  SIGNAL_FLUSH│
│            │          │pending 見つかる           │  ioctl       │
│            │          │target->err = 0            │              │
│            │          │complete_all(pending)      │              │
│            │          │flush_pending_unhash       │              │
│            │          │                           │              │
│            │          │(wait_for_completion 返る) │              │
│            │          │err = 0                    │              │
│            │◀─────────│                           │              │
│◀───────────│(fsync return 0)                      │              │
```

**要点**:
- FLUSH_WAIT flag が daemon 側で coalescer bypass + CancelIno を
  引き起こす → pending の stale event が fsync bytes を上書きする
  bug を防止
- `signal_flush` が per-ino completion を wake、`fsync` return 値
  として PUT err を伝搬 (POSIX 要件)
- 30s timeout で daemon 死亡検出 → `-EIO`

---

## タイミング 6: policy eviction + arena free (D-02)

```
Daemon           kmod                           BPF policy
│                │                              │
│ /policy/eval   │                              │
│  or admit push │                              │
├───────────────▶│                              │
│                │admit_replace(snap):          │
│                │ admit_clear_locked           │
│                │  (all hash_del + count=0)    │
│                │ hash_add per entry           │
│                │ count = N                    │
│                │                              │
│                │s3fs_policy_on_admit(N, hint) ─▶ CMS aging /
│                │                              │  epoch bump 等
│                │                              │
│                │ if N > softcap(8):            │
│                │  s3fs_policy_on_pressure ────▶  (advisory)
│                │                              │
│                │  for each i in entries:      │
│                │   s3fs_policy_on_evict(ino) ─▶ vote (1=evict)
│                │                              │  KFunc: admit_
│                │                              │  count, softcap
│                │                              │  ino_size 等
│                │                              │
│                │   if vote != 0 && count>cap: │
│                │    hash_del + atomic_dec     │
│                │    evict_log_push(ino)       │
│                │    evictions_total++         │
│                │                              │
│                │(policy evicted M entries)    │
│                │                              │
│                │                          ◀───│drainEvictionsLoop
│                │DRAIN_EVICTIONS ioctl ─────── │  (2s tick)
│                │→ [ino_1..ino_M]              │
│                │                              │
│◀───────────────│                              │
│dropAdmits(     │                              │
│  evicted, progs)                              │
│                │                              │
│  for each admit in snapshot:                  │
│   if ino ∈ evicted:                           │
│    FreeArenaChunk(uaddr - base, size) ── D-02 │
│    admits[]から除去                          │
│                                              │
│ metrics.ArenaFreeBytes += N                  │
│ metrics.ArenaFreeChunks += M                 │
│                                              │
│ (Grafana: arena_free_bytes/chunks 動く)     │
```

**要点**:
- kmod-side eviction は BPF policy の vote で駆動、evict_log に
  ino を積んで daemon に feedback
- daemon 側 dropAdmits が **arena bytes を free-list に返却**、
  次の AdmitContent が re-use
- BPF policy の on_evict は KFunc (Layer 0) 経由で live kmod 状態を
  読める (admit_count, ino_size 等)

---

## タイミング 7: delete (unlink) (E-02)

```
App          VFS        kmod                       daemon          S3
│            │          │                          │               │
│ rm file    │          │                          │               │
├───────────▶│          │                          │               │
│            │.unlink   │                          │               │
│            ├─────────▶│                          │               │
│            │          │delete_enqueue({ino,     │               │
│            │          │  name})                  │               │
│            │          │                          │               │
│            │          │local_entry_remove(name)  │               │
│            │          │  (local list から drop)  │               │
│            │          │                          │               │
│            │          │admit_drop_ino_locked     │               │
│            │          │  (admit hash から drop、 │               │
│            │          │   arena bytes は残るが   │               │
│            │          │   evict-drain 経路が後で │               │
│            │          │   free-list に返す)      │               │
│            │          │                          │               │
│            │          │drop_nlink(inode)         │               │
│            │          │inode_set_ctime_current   │               │
│            │          │                          │               │
│            │◀─────────│                          │               │
│◀───────────│(rm success)                         │               │
│            │          │                       ◀──│runDeleteLoop  │
│            │          │POP_DELETE ioctl          │(100ms tick)   │
│            │          │→ {ino, name}             │               │
│            │          │                          │handleDelete:  │
│            │          │                          │ resolve key   │
│            │          │                          │ (LookupByIno  │
│            │          │                          │  or name)     │
│            │          │                          │ s3c.Delete ──▶│ DELETE
│            │          │                          │◀──────────    │◀
│            │          │                          │ 完了 log      │
```

**要点**:
- kmod 側は VFS 応答を先に返す (unlink は POSIX 的 non-blocking)、
  daemon 側 S3 DELETE は非同期
- 次の sync round で S3 List が deleted key を返さなくなる →
  dyn entry からも消える
- open fd を持ってた場合は inode が evict されないので、fd の read
  は arena bytes / fd->buf を返し続ける (POSIX 準拠)

---

## タイミング 8: rename (E-02)

```
App          VFS         kmod                       daemon          S3
│            │           │                          │               │
│ mv old new │           │                          │               │
├───────────▶│           │                          │               │
│            │.rename    │                          │               │
│            ├──────────▶│                          │               │
│            │(same dir, │flags check:              │               │
│            │ NOREPLACE)│  RENAME_EXCHANGE → EINVAL│               │
│            │           │  cross-dir → EXDEV       │               │
│            │           │                          │               │
│            │           │rename_enqueue({ino,     │               │
│            │           │  old_name, new_name})    │               │
│            │           │                          │               │
│            │           │local_entry_rename        │               │
│            │           │  (local list name 上書き)│               │
│            │           │                          │               │
│            │           │inode_set_ctime_current   │               │
│            │           │                          │               │
│            │◀──────────│                          │               │
│            │d_move     │                          │               │
│◀───────────│(mv success)                          │               │
│            │           │                       ◀──│runRenameLoop  │
│            │           │POP_RENAME ioctl          │               │
│            │           │→ {ino, old, new}         │               │
│            │           │                          │handleRename:  │
│            │           │                          │ s3c.Rename:   │
│            │           │                          │  CopyObject ─▶│ COPY
│            │           │                          │◀──────────    │◀
│            │           │                          │  RemoveObject ▶│ DEL
│            │           │                          │◀──────────    │◀
│            │           │                          │ registerIno   │
│            │           │                          │ (newKey, ino) │
│            │           │                          │ 以後の PUT は │
│            │           │                          │ new key で    │
```

**要点**:
- **atomicity 無し**: CopyObject 成功 + RemoveObject 失敗の window で
  old / new 両方存在。roll back 手段なし
- kmod-side は VFS 応答を先に返す (mv は POSIX 的 non-blocking)
- registerIno で ino ↔ S3Key mapping を更新、後続の flush や
  delete が正しい key で S3 に届く

---

## 全体 flow の 3 つの分岐点

1. **admit_snapshot(ino, pos) が hit するか miss するか** (kmod .read_folio):
   - hit: fill_from_arena (path B)
   - miss + daemon alive: miss_enqueue + wait (path C)
   - miss + daemon dead: kmalloc fallback (zero page)

2. **flush event の flags (E-03)**:
   - FLUSH_WAIT set: coalescer bypass + CancelIno + immediate PUT
   - flags=0: coalescer.submit → debounce

3. **AdmitContent の allocator (D-02)**:
   - free-list first-fit fit → reuse
   - fit なし → bump allocator (cacheNextOff 前進)
   - 両方失敗 → ErrCacheFull

これらの分岐が「hot path で BPF policy が呼ばれる回数」「S3 traffic」
「arena 消費」を左右する。Grafana の 22 panel + histogram はこの
分岐が支配する挙動を可視化する構成。

## 参考: フックポイント一覧 (kmod → BPF)

| kmod call site | BPF hook | 発火頻度 | 用途 |
|---|---|---|---|
| `admit_replace` 開始 | `on_admit(count, hint)` | admit_replace ごと 1 回 | CMS aging / stat 更新 |
| `admit_replace` (count > softcap) | `on_pressure(count, threshold)` | 圧力越え 1 回 | 事前通知 (advisory) |
| `admit_replace` per entry | `on_evict(ino)` | admit 数 (最大 hash size) | 1 = vote evict, 0 = keep |
| `.read_folio` (admit hit) | `on_hit(ino)` | per folio | freq/visited-bit 更新 |
| `fentry/generic_file_read_iter` (page cache hit) | `on_hit(ino)` (KFunc `s3fs_kfunc_policy_hit` 経由、S-03) | per read (CPU と ino ごとに jiffy 1 回に間引き、AC-47) | page cache で返る read も policy に見せる |
| `admit_replace` の容量パス (Phase 1) | `on_insert(ino)` (struct_ops のみ、AC-58) | 候補 ino ごと (冪等) | policy の FIFO / queue に候補を入れる |
| `admit_replace` (容量超過) | `pick_victim()` (struct_ops のみ、AC-58) | evict する件数ぶん | 次に捨てる ino を返す。0 なら score 順に戻る |
| `admit_replace` (evict 後) | `on_remove(ino)` (struct_ops のみ、AC-58) | evict した ino ごと | policy の構造から外す |

AC-58 の 3 つは vtable が `pick_victim` を持つときだけ、`s3fs_policy_evict_lock` の下で呼ばれる (`docs/design/ac58-pick-victim.md`)。

## 参考: KFunc (Layer 0) 一覧

| KFunc | 型 | 意味 |
|---|---|---|
| `s3fs_kfunc_admit_count()` | u32 | 現 admit hash size |
| `s3fs_kfunc_softcap()` | u32 | module param `softcap` (既定 8) |
| `s3fs_kfunc_capacity_bytes()` | u64 | module param `capacity_bytes` (0 = 容量上限なし) |
| `s3fs_kfunc_evictions_total()` | u32 | atomic_long_read (2^32 wrap) |
| `s3fs_kfunc_time_ns()` | u64 | ktime_get_boottime_ns |
| `s3fs_kfunc_ino_size(ino)` | u64 | admit lookup の size (D-01 後は range 1 個の size) |
| `s3fs_kfunc_policy_hit(ino)` | int | fentry (tracing) から policy の `on_hit` を呼ぶ (S-03) |

## 参考: ioctl 一覧 (daemon → kmod, kmod → daemon)

| ioctl | 方向 | 用途 |
|---|---|---|
| `S3FS_IOC_PROBE_KVBASE` (0x01) | daemon | arena kern_vm base 取得 |
| `S3FS_IOC_ADMIT_ARENA` (0x02) | daemon | admit_replace 起動 |
| `S3FS_IOC_SIGNAL_ADMIT` (0x03) | daemon | read-miss waiter を wake |
| `S3FS_IOC_SYNC_ENTRIES` (0x04) | daemon | dyn entry table を更新 |
| `S3FS_IOC_POP_FLUSH` (0x05) | daemon | flush event drain |
| `S3FS_IOC_READ_DIRTY` (0x06) | daemon | fd->buf を daemon side buf に copy |
| `S3FS_IOC_MARK_DIRTY` (0x07) | daemon | fd->dirty 復元 (PUT 失敗時 retry 用) |
| `S3FS_IOC_STATS` (0x08) | daemon | kmod side counter 読み出し |
| `S3FS_IOC_DRAIN_EVICTIONS` (0x09) | daemon | evicted ino ring buffer drain |
| `S3FS_IOC_SIGNAL_FLUSH` (0x0A) | daemon | fsync waiter を wake (E-01) |
| `S3FS_IOC_POP_DELETE` (0x0B) | daemon | delete event drain (E-02) |
| `S3FS_IOC_POP_RENAME` (0x0C) | daemon | rename event drain (E-02) |
| `S3FS_IOC_POP_FLUSH_WAIT` (0x0D) | daemon | fsync の flush event を blocking で pop (S-08) |
| `S3FS_IOC_POPULATE_PAGES` (0x0E) | daemon | pcache モードの admit (fd の chunk にコピー + PCACHE entry、AB-04) |
| `S3FS_IOC_DIRTY_LAYOUT` (0x0F) | daemon | dirty file の chunk 状態 (data / hole / remote) を読む (AC-51) |
| `S3FS_IOC_READ_DIRTY_RANGE` (0x10) | daemon | offset 指定の READ_DIRTY (AC-51) |
