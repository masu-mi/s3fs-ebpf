# Partial admit の内部構造とシーケンス

大 file を chunk 分割して arena に載せる仕組み (D-01 + D-02) を
「どんな型が」「どこから参照されて」「どの順で更新されるか」で
読み解くための資料。

構成:
1. データ構造 (kmod / daemon / arena) の型定義と実物の bytes
2. 参照関係 (どの pointer / offset が何を指すか)
3. シーケンス 4 種:
   - S1 chunk admit (miss → S3 range GET → arena 書込 → hash に追加)
   - S2 range lookup (folio_pos から chunk を選ぶ hash walk)
   - S3 eviction & free (policy vote → hash_del → free-list 返却)
   - S4 multi-range per ino (同 ino に複数 chunk が同居する状態遷移)
4. Invariant 表 (常に守られる制約)

## 1. データ構造

### 1.1 kmod 側

```c
/* 1 admit entry = 1 chunk range. hash bucket は ino で衝突 (=
 * 同 ino の複数 range は同 bucket に連なる)。 */
struct s3fs_admit {
    struct hlist_node node;   /* hash bucket チェーン */
    u64 ino;                  /* dyn entry の ino */
    u64 uaddr;                /* daemon user_vm 内のこの chunk の 開始 */
    u64 size;                 /* chunk 長 (bytes) */
    u64 offset;               /* file 内の chunk 開始 offset (D-01) */
};

#define S3FS_ADMIT_HASH_BITS 6   /* 64 buckets */
static DEFINE_HASHTABLE(s3fs_admits, S3FS_ADMIT_HASH_BITS);
static DEFINE_SPINLOCK(s3fs_admits_lock);
atomic_t s3fs_admits_count;                /* 現エントリ数 */
static struct mm_struct *s3fs_admitted_mm; /* daemon mm ref */
```

**物理レイアウト** (1 entry = 48 bytes、hlist_node は 16 bytes):

```
struct s3fs_admit {
  +0 : hlist_node.next  (pprev linked-list への pointer)
  +8 : hlist_node.pprev
  +16: ino    (u64)
  +24: uaddr  (u64)  ─ daemon 側の user address
  +32: size   (u64)  ─ この range の byte 数
  +40: offset (u64)  ─ file 内 offset (D-01 で追加)
  = 48 bytes
}
```

### 1.2 UAPI (kmod ↔ daemon)

```c
/* /dev/s3fs_ebpf ioctl(ADMIT_ARENA) が受け取る配列要素。
 * kmod は per-entry で hash_add する。 */
struct s3fs_admit_entry {
    __u64 ino;
    __u64 uaddr;
    __u64 size;
    __u64 offset;   /* D-01 で追加、0 = whole-file 相当 */
};
```

### 1.3 daemon 側 admit snapshot

```go
// admitEntry は 1 chunk range を表す (UAPI struct と同じレイアウト)。
type admitEntry struct {
    Ino    uint64
    Uaddr  uint64
    Size   uint64
    Offset uint64
}

var (
    admitMu sync.Mutex
    admits  []admitEntry  // "kmod と同期させたい" scoped-mirror
)
```

daemon 側は snapshot (`admits[]`) を持ち、変更ごとに全体を再送
(`ADMIT_ARENA` ioctl はスナップショット置換セマンティクス)。
ioctl は「set (replace all)」なので snapshot 全部を kmod に渡す。

### 1.4 daemon 側 arena wrapper (D-02 free-list)

```go
type Programs struct {
    arena        *arena.Arena
    cacheMu      sync.Mutex
    cacheKnown   map[uint64]uint32   // ino → arena offset (deprecated for content)
    cacheNextOff uint32              // bump allocator の現在位置
    arenaFree    []arenaChunk        // D-02 free-list
}

type arenaChunk struct {
    offset uint32   // arena base からの byte offset
    size   uint32   // chunk 長
}
```

**arena 内 layout** (concrete example):

```
arena base = 0x77bb54000000  (daemon mmap の user_vm 先頭)
      +0     ┌─────────────┐  cacheNextOff (bump 前進位置)
             │ stub area   │
             │ (prewarm    │
             │  content)   │
             ├─────────────┤
             │ range #1    │  ← ino=141 offset=25165824 size=4194304
             │  4 MiB      │       uaddr = base + 0x00A00000
             ├─────────────┤
             │ range #2    │  ← ino=136 offset=0 size=44
             │  44 B       │       uaddr = base + 0x00E00000
             ├─────────────┤
             │ [FREE]      │  ← 過去に admit されて evict された chunk
             │  ~4 MiB     │       arenaFree[] に (offset, size) が入る
             ├─────────────┤
             │ range #3    │
             │  ...        │
             └─────────────┘  cacheNextOff 停止 (D-02 で再利用可能)
```

### 1.5 S3 view の関係

```
File "hugeblob.bin" (50 MiB in S3)
      byte 0 ─────────────────────────────────────── byte 50 MiB
      │        │        │        │  ...   │        │        │
      chunk0   chunk1   chunk2            chunk11  chunk12
      [0,4M)  [4M,8M)  [8M,12M)          [44M,48M) [48M,50M)

現在 arena に載っている range (仮に 3 chunk とする):
  chunk 6:  [24M, 28M) ← daemon が offset=24M で GetRange 済
  chunk 11: [44M, 48M) ← 別の miss で追加 admit
  chunk 2:  [8M, 12M)  ← 別の miss で追加 admit

kmod admit hash (ino=141 = hugeblob.bin):
  bucket = hash(141) % 64
  bucket → entry(off=24M, size=4M, uaddr=U1)
        → entry(off=44M, size=4M, uaddr=U2)
        → entry(off=8M,  size=4M, uaddr=U3)
```

3 chunk 分だけ arena を占有 = 12 MiB。file 全体は 50 MiB だが
残りは load 不要。

## 2. 参照関係

### 2.1 コンポーネント間の pointer / offset 経路

```
[App]                                             [S3]
  │ read(fd, buf, pos)                              ▲
  ▼                                                 │
[VFS folio_pos]                                     │ GetRange
  │                                                 │
  ▼                                                 │
[.read_folio(folio)]                                │
  │                                                 │
  │ admit_snapshot(ino, folio_pos)                  │
  ▼                                                 │
┌────────────────────┐                              │
│ s3fs_admits (hash) │                              │
│  bucket[hash(ino)]:│                              │
│    → admit A       │                              │
│    → admit B       │◀────────────┐                │
│    → admit C       │             │                │
└────────────────────┘             │ add            │
    │ selected entry (B)           │                │
    ▼                              │                │
  {uaddr=B.uaddr + within,         │                │
   avail=B.size - within}          │                │
    │                              │                │
    ▼                              │                │
  fill_from_arena:                 │                │
    GUP(admitted_mm, uaddr) ──┐    │                │
                              │    │                │
                              ▼    │                │
                    ┌────────────────────┐          │
                    │ daemon user_vm     │          │
                    │ (mmap'd arena)     │          │
                    │  [free]            │          │
                    │  [range B bytes]◀──┼──── addAdmit ({Ino, Uaddr,
                    │  [free]            │          │       Size, Offset})
                    │  [range A bytes]   │          │
                    │  [free]            │          │
                    └────────────────────┘          │
                              ▲                     │
                              │                     │
                    ┌─────────────────┐             │
                    │ arena.WriteAt   │             │
                    │ (AdmitContent)  │             │
                    │  bump 側 or     │             │
                    │  free-list reuse│             │
                    └─────────────────┘             │
                              ▲                     │
                              │ body                │
                    ┌─────────────────┐             │
                    │ handleReadMiss  │─────────────┘
                    │ GetRange(key,   │  S3 range header
                    │   chunk_off,    │
                    │   chunk_size)   │
                    └─────────────────┘
```

**主要な "何が何を指すか"**:

| From | To | 意味 |
|---|---|---|
| `admit.uaddr` | daemon mm の user_vm 内 arena page | GUP で kmod 側 page 取得 |
| `admit.offset` | file 内の byte 位置 | folio_pos の判定に使う |
| `admit.size` | この range の byte 数 | avail 計算 |
| `admit.ino` | dyn entry / bootstrap entry | dcache lookup key |
| `arenaChunk.offset` | arena base からの byte | AdmitContent 復元 target |
| `s3fs_admitted_mm` | daemon の mm_struct | GUP mm parameter |
| `cacheNextOff` | arena 空き先頭 | bump allocator |
| `arenaFree[]` | 過去 admit の (offset, size) | reuse pool |

### 2.2 range の "byte 座標系" 3 系統

紛らわしいので明示:

```
座標系 A: file 内 offset (POSIX 的)
  hugeblob.bin: [0, 50 MiB)
  chunk 6 = [25165824, 29360128)      ← admit.offset, admit.offset+admit.size

座標系 B: arena 内 offset (daemon 内)
  arena base = 0x77bb54000000 (これは B の 0 位置)
  chunk 6 が書かれた場所: [0x00A00000, 0x00E00000)
                       = [10 MiB, 14 MiB) 相当
  ← arenaChunk.offset, arenaChunk.offset+arenaChunk.size

座標系 C: user_vm 内 absolute address (kmod GUP が使う)
  admit.uaddr = arena base + arenaChunk.offset
             = 0x77bb54000000 + 0x00A00000
             = 0x77bb54a00000
  ← admit.uaddr, admit.uaddr + admit.size
```

**変換**:
- 座標 A ↔ 座標 C の対応: `chunk 6 の file byte K の user_vm = admit.uaddr + (K - admit.offset)`
- 座標 B ↔ 座標 C: `admit.uaddr = arena.BaseAddr() + arenaChunk.offset`

## 3. シーケンス

### S1: chunk admit (miss → range GET → hash 追加)

前提: hugeblob.bin (50 MiB, ino=141)、arena に何も admit なし。
App が offset 25 MiB から 4 KiB read しようとする。

```
App           kmod                        daemon                    S3
 │             │                            │                        │
 │  read(fd,   │                            │                        │
 │  buf,       │                            │                        │
 │  pos=25M)   │                            │                        │
 ├────────────▶│                            │                        │
 │             │                            │                        │
 │             │.read_folio(folio_pos=25M)  │                        │
 │             │                            │                        │
 │             │admit_snapshot(141, 25M):   │                        │
 │             │  admit_lookup_at_locked:   │                        │
 │             │    hash_for_each_possible( │                        │
 │             │      s3fs_admits, 141) →   │                        │
 │             │      空 (miss)             │                        │
 │             │                            │                        │
 │             │pending_get_or_create(141)  │                        │
 │             │miss_enqueue({141, 25M,4K}) │                        │
 │             │                            │                        │
 │             │wait_for_completion(2s)     │                        │
 │             │                            │                        │
 │             │                          ◀─│miss_reader             │
 │             │                            │reads event             │
 │             │                            │                        │
 │             │                            │handleReadMiss(          │
 │             │                            │  ev={141, 25M, 4K}):    │
 │             │                            │                        │
 │             │                            │ fullSize = 50 MiB       │
 │             │                            │        > 4 MiB          │
 │             │                            │ chunk_off = 25M/4M*4M   │
 │             │                            │           = 24 MiB      │
 │             │                            │ chunk_size = 4 MiB      │
 │             │                            │                        │
 │             │                            │s3c.GetRange(           │
 │             │                            │  "hugeblob.bin",       │
 │             │                            │  24 MiB, 4 MiB)  ─────▶│ Range:
 │             │                            │◀─────── body (4 MiB) ──│ bytes=
 │             │                            │                        │ 24MiB
 │             │                            │AdmitContent(body):     │ -28MiB
 │             │                            │ arenaFree 探索 →       │
 │             │                            │  best-fit なし         │
 │             │                            │ bump: nextOff += 4 MiB │
 │             │                            │ arena.WriteAt(         │
 │             │                            │   body, nextOff)       │
 │             │                            │ return off = 0x00A00000│
 │             │                            │                        │
 │             │                            │uaddr = arena.BaseAddr()│
 │             │                            │      + 0x00A00000      │
 │             │                            │                        │
 │             │                            │addAdmit({              │
 │             │                            │  Ino=141,              │
 │             │                            │  Uaddr=uaddr,          │
 │             │                            │  Size=4 MiB,           │
 │             │                            │  Offset=24 MiB})       │
 │             │                            │ → admits[]に append    │
 │             │                            │ → snap = [..., new]    │
 │             │                            │                        │
 │             │◀─── ADMIT_ARENA ioctl ─────│admitArena(devPath, snap)│
 │             │                            │                        │
 │             │admit_replace(mm, entries): │                        │
 │             │  admit_clear_locked        │                        │
 │             │  hash_add per entry        │                        │
 │             │  atomic_set(count=N)       │                        │
 │             │  → hash bucket(hash(141)): │                        │
 │             │     [new admit entry: {    │                        │
 │             │       ino=141, uaddr,      │                        │
 │             │       size=4M, offset=24M  │                        │
 │             │     }, ...others...]       │                        │
 │             │                            │                        │
 │             │s3fs_policy_on_admit(N)     │                        │
 │             │s3fs_policy_on_evict per ino│                        │
 │             │                            │                        │
 │             │                          ◀─│signalAdmit(141, 0)     │
 │             │signal_admit(141, 0):       │                        │
 │             │  complete_all(pending)     │                        │
 │             │  → wait 起きる             │                        │
 │             │                            │                        │
 │             │admit_snapshot(141, 25M):   │                        │
 │             │  admit_lookup_at_locked:   │                        │
 │             │    bucket walk 発見:       │                        │
 │             │    offset=24M, size=4M     │                        │
 │             │    25M ∈ [24M, 28M) → hit  │                        │
 │             │    within = 25M - 24M = 1M │                        │
 │             │    uaddr = base + 1M       │                        │
 │             │    avail = 4M - 1M = 3M    │                        │
 │             │                            │                        │
 │             │fill_from_arena (pos=0,     │                        │
 │             │  uaddr, asize=3M):         │                        │
 │             │  GUP(mm, uaddr) → page     │                        │
 │             │  memcpy(folio, page + off, │                        │
 │             │         fs_size=4K)        │                        │
 │             │                            │                        │
 │◀────────────│(folio 4K populated)        │                        │
 │  buf ready  │                            │                        │
```

**新しく生まれた state**:
- kmod: `s3fs_admits` に 1 entry 追加 (ino=141, offset=24M, size=4M, uaddr=U)
- daemon: `admits[]` に同 entry を append
- arena: `[+0..+4M)` が range #new として使用中
- `cacheNextOff` が +4M 進む
- `arenaFree` は変わらず (bump 経路だったため)

### S2: range lookup (folio_pos から chunk を選ぶ hash walk)

前提: 上記 S1 の後、同じ ino=141 に対して既に 2 chunk が admit
されている状態。App が別 offset (30 MiB) を read しようとする。

kmod hash bucket 状態:

```
s3fs_admits[hash(141)]:
  ┌──────────────────────────────────────┐
  │ entry α: ino=141, off=24M, size=4M  │  ← S1 で追加
  │ entry β: ino=141, off=8M,  size=4M  │  ← 別 miss で追加
  │ entry γ: ino=137, off=0,   size=44   │  ← 別 ino 衝突
  └──────────────────────────────────────┘
```

lookup process:

```
admit_snapshot(ino=141, pos=30 MiB):
  admit_lookup_at_locked(141, 30M):
    hash_for_each_possible(s3fs_admits, a, node, 141):

      /* iteration 1: entry α */
      a->ino == 141 ✓
      pos=30M ∈ [α.off=24M, α.off+α.size=28M) ?
        24M ≤ 30M < 28M ?
        No — 30M ≥ 28M
      continue

      /* iteration 2: entry β */
      a->ino == 141 ✓
      pos=30M ∈ [β.off=8M, β.off+β.size=12M) ?
        8M ≤ 30M < 12M ?
        No — 30M ≥ 12M
      continue

      /* iteration 3: entry γ */
      a->ino == 141 ?
        141 != 137
      continue

    return NULL  ← miss

  → admit_snapshot returns false
  → path C: miss_enqueue + wait for daemon to admit chunk 7 [28M, 32M)
```

**要点**: 
- hash bucket walk は O(bucket 内 entry 数)、通常 数個以内
- 同 bucket に別 ino がいても `a->ino == ino` check で drop
- 該当 range が無ければ path C 発火 → 新 chunk を追加 admit

### S3: eviction & free-list 返却

前提: `s3fs_admits` に 20 entry ある (softcap=8 超過)。policy が
その中の 10 entry に evict vote。

```
kmod                     BPF policy              daemon
 │                         │                       │
 │admit_replace 途中で:    │                       │
 │ for i in 0..count:      │                       │
 │  fr_vote = policy_on_   │                       │
 │   evict(entries[i].ino) │──────BPF── on_evict ─▶│
 │                         │  (visited/freq 判定)  │
 │  so_vote = policy_ops_  │                       │
 │   eval_evict(...) ──────▶│                       │
 │                         │◀─────── 1 (evict) ────│
 │                         │                       │
 │  if count > softcap &&  │                       │
 │     vote != 0:          │                       │
 │   spin_lock(admits_lock)│                       │
 │   hash_del(&entry.node) │                       │
 │   atomic_dec(count)     │                       │
 │   kfree(entry)          │                       │
 │   spin_unlock           │                       │
 │   evict_log_push(ino)   │                       │
 │   evictions_total++     │                       │
 │                         │                       │
 │(10 entry 削除完了)      │                       │
 │                         │                       │
 │                         │                     ◀─│drainEvictionsLoop
 │DRAIN_EVICTIONS ioctl ── │                       │(2s tick)
 │→ [ino_1..ino_10]        │                       │
 │◀────────────────────────│                       │
 │                         │                       │
 │                         │                       │dropAdmits(
 │                         │                       │  evicted, progs):
 │                         │                       │ arenaBase =
 │                         │                       │   progs.Arena().
 │                         │                       │   BaseAddr()
 │                         │                       │
 │                         │                       │ for each admits[i]:
 │                         │                       │  if evicted ∋ Ino:
 │                         │                       │   freedRanges +=
 │                         │                       │    {uaddr - arenaBase,
 │                         │                       │     size}
 │                         │                       │  else:
 │                         │                       │   filtered += admits[i]
 │                         │                       │
 │                         │                       │ admits = filtered
 │                         │                       │ (mutex 外で:)
 │                         │                       │ for r in freedRanges:
 │                         │                       │  progs.FreeArenaChunk(
 │                         │                       │    r.offset, r.size)
 │                         │                       │
 │                         │                       │AdmitContent 内部で:
 │                         │                       │  arenaFree に append
 │                         │                       │  metrics update
```

**新しく生まれた state**:
- kmod: `s3fs_admits` から 10 entry 減、`atomic_admits_count` -= 10
- daemon: `admits[]` から 10 entry 減
- `progs.arenaFree` に 10 chunk が append (合計 sum bytes は evicted 分)
- 次の AdmitContent がまず arenaFree を探す (best-fit)、reuse 成立

### S4: multi-range per ino の状態遷移

同じ ino=141 (50 MiB blob) に対する chunk の増減例。

```
時刻 T0: 何も admit されていない
  s3fs_admits[hash(141)] = []
  arena_used = 0
  arenaFree = []

────

時刻 T1: App が offset 25M を read → chunk 6 [24M,28M) を admit
  s3fs_admits[hash(141)] = [ {141, U_A, 4M, 24M} ]
  arena_used = 4 MiB
  arenaFree = []

────

時刻 T2: App が offset 10M を read → chunk 2 [8M,12M) を追加 admit
  s3fs_admits[hash(141)] = [
    {141, U_A, 4M, 24M},
    {141, U_B, 4M,  8M},          ← 新規、bump alloc
  ]
  arena_used = 8 MiB
  arenaFree = []

────

時刻 T3: policy が chunk 6 に vote → hash_del + free-list 返却
  s3fs_admits[hash(141)] = [
    {141, U_B, 4M,  8M},
  ]
  arena_used = 8 MiB (monotonic, 変わらず)
  arenaFree = [ {U_A - base, 4M} ]
  (metrics: arena_free_bytes = 4 MiB)

────

時刻 T4: App が offset 40M を read → chunk 10 [40M, 44M) を admit
  AdmitContent: arenaFree に 4M が 1 個 (best-fit) → reuse
  bump 側 (cacheNextOff) は動かない
  s3fs_admits[hash(141)] = [
    {141, U_B, 4M,  8M},
    {141, U_A, 4M, 40M},          ← 再利用: U_A は T1 と同じ address!
  ]
  arena_used = 8 MiB (変わらず)
  arenaFree = []                 (再利用消費)

────

時刻 T5: App が offset 5M で 100 bytes 読む (chunk 1 [4M,8M) が要る)
  admit_snapshot(141, 5M):
    entry {141, 8M, 4M, U_B}: 5M ∈ [8M, 12M)? No
    entry {141, 40M, 4M, U_A}: 5M ∈ [40M, 44M)? No
    → miss
  path C 発火 → 新 chunk [4M, 8M) を bump alloc
  s3fs_admits[hash(141)] = [ 3 entries ]
  arena_used = 12 MiB
```

**要点**:
- 同一 ino に対して bucket チェーンが伸びていく (時刻 T2 で 2 個)
- Eviction で 1 entry だけ消える (時刻 T3、他 range はそのまま)
- free-list reuse で arena capacity を使い回す (時刻 T4)
- 未 admit の chunk へ read → 新規 miss (時刻 T5)

## 4. Invariant 表 (常に守られる制約)

| # | 制約 | Enforcer | 破ったら |
|---|---|---|---|
| I1 | admit entry の [offset, offset+size) は file 内 valid 範囲 | daemon が chunk 計算時に file size clamp | 範囲外読出 → EIO |
| I2 | 同 ino の複数 entry の range は overlap しない (と daemon が仮定) | daemon が chunk 境界を align (chunk_offset = pos/CHUNK*CHUNK) | admit_lookup_at で複数 hit → 先勝ち (不具合) |
| I3 | admit.uaddr は arena 範囲内 (base ≤ uaddr < base+arena.Size()) | daemon AdmitContent で arena.WriteAt 前に境界確認 | GUP fault |
| I4 | admit.size ≤ arena.Size() | AdmitContent の "size 超過" check → ErrCacheFull | admit reject |
| I5 | arenaFree に返した chunk と live admit の (uaddr, size) が overlap しない | dropAdmits が admits[] から entry 消す前 に FreeArenaChunk 予約 | double-write / stale bytes |
| I6 | arena_used_bytes は monotonic (D-02 の semantics 上) | AdmitContent が bump 側 だけ nextOff 更新、reuse では動かさない | metric 意味不明 |
| I7 | s3fs_admits_count と実 hash 数が一致 | hash_add / _del の pair で atomic_inc / _dec | KFunc admit_count 誤値 |
| I8 | s3fs_admitted_mm は daemon 生存中 pin | admit_replace が count>0 で mm 上書き、count=0 では旧 mm を残す | GUP EFAULT (E-01 で修正済) |

## 5. 参考: 実測 layout の観察方法

```bash
# kmod-side counters
curl -s http://localhost:9600/metrics | grep -E "admits_current|folio_hits|arena_(used|free)"

# BPF struct_ops policy が観測する Layer 0 KFunc:
# - s3fs_kfunc_admit_count() が I7 の atomic を read

# dmesg で admit_replace の観測 (kmod pr_info)
sudo dmesg -T | grep -E "admitted [0-9]+ entries|voted evict|admit cleared"

# daemon-side arena stats (5s tick)
curl -s http://localhost:9600/metrics | grep arena_
```

Meetup の live demo で「同 ino に複数 range が同居して arena に
実データが載っている」ことを見せたい時は、大 file (50 MiB+) を
prewarm 対象外にして、複数 offset を dd で読むと dmesg + /metrics
の両方に痕跡が出る。

## 参照

- `docs/journal/backlog.md` D-01 / D-02 の commit ref (`b0a62c7` / `c419541`)
- `docs/design/hook-timings-diagrams.md` タイミング 3 (read miss + partial admit)、6 (eviction + arena free)
- `docs/user/posix-and-practical-notes.md` §D-01 / §D-02 の POSIX 準拠論点
- `kmod/vfs_ops.c`: `s3fs_admit` / `admit_lookup_at_locked` / `admit_snapshot`
- `daemon/internal/ebpf/loader.go`: `AdmitContent` / `FreeArenaChunk` / `ArenaFreeStats`
- `daemon/cmd/s3fs-ebpf/admit_arena.go`: `handleReadMiss` / `dropAdmits`
- `include/s3fs_ebpf_uapi.h`: `struct s3fs_admit_entry`
