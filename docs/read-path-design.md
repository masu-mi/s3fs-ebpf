# Read Path 設計 — Paths A′ / B / C / D

s3fs-ebpf における「kmod の `.read` が daemon が用意した実データを返す」ための架け橋の設計集。Week 4 (a) の探索で 4 通りが浮かんだので、それぞれの位置付け・技術的可否・実装コストを一元管理する。実装状況の記録は `docs/week4a-implementation-notes.md`。

---

## 0. 早見表

| Path | 経路 | 状態 | zero-copy | Meetup narrative | 実装コスト |
|---|---|---|---|---|---|
| A′ | mount option + kmod が arena kern_vm 直参照 | ❌ 断念 | ○ | 最強 (design.md §2 直結) | 中 (だが kernel 内部依存で fragile) |
| **B** | kmod が daemon user_vm を GUP + kmap 経由 read | **✅ 実装済** | ○ (物理 page 共有) | 強 | 小 |
| C | kmod read miss → daemon upcall → daemon が S3/arena fetch → admit → kmod retry | 📝 設計スケッチ | △ (fetch は copy 経由) | 中 (Slow Path 完成) | 中 |
| D | upstream に patch を投げて A′ を復活 | 📝 提案 | ◎ (JIT 直参照相当) | 最強 + 上流貢献 | 大 (kernel rebuild + upstream review) |

**現状の推奨**: B が primary、C は Slow Path として補完、D は Meetup 後に upstream 貢献として検討。

---

## 1. Path A′ (dropped) — なぜ動かないか

### 設計意図

- daemon が `bpf_arena` を作成 → mmap → map id を取得
- `mount("none", "/mnt/...", "s3fs_ebpf", "map_id=42")` で kmod に arena を紐付け
- kmod は `bpf_map_get_with_uref(42)` → `struct bpf_arena *` → `->kern_vm->addr` で kernel-VM base を取得
- `.read` は `copy_to_user(ubuf, kern_vm_base + off + pos, ret)` で arena page から直接返す

### 6.12 kernel での壁

| 障害 | 詳細 |
|---|---|
| `bpf_map_get_with_uref` 未 export | `bpf_map_get(fd)` は EXPORT_SYMBOL 済だが `_with_uref` は未 export。fd から取るなら `bpf_map_get` でも十分 (namespace は `BPF_INTERNAL`) |
| `bpf_arena_get_kern_vm_start` 未 export | prototype は `<linux/bpf.h>` にあるが `__ksymtab_` エントリなし。kmod からリンク不可 |
| `struct bpf_arena` opaque | header は forward decl のみ。full layout は `kernel/bpf/arena.c` 内部限定 |
| kern_vm range に fault handler 未登録 | `arena_vm_fault` は user_vm VMA 用のみ。kern_vm 側は BPF JIT 経由アクセス専用の shadow mapping |
| `bpf_arena_alloc_pages` は kern_vm PTE を install しない | maple_tree 登録 + user_vm 側 PTE のみ。kern_vm PTE は BPF 実行時 JIT が addr_space_cast を発火したときだけ install |

**結論:** stock 6.12 kernel + kmod だけでは実現不能。kernel patch (D) が前提となる。

### 部分成果 (spike)

- `struct bpf_arena` の BTF-derived redefine + `container_of` で `arena->kern_vm->addr` を **抜くことはできる**
- 実測: user_vm=0x7a5ca837d000, kern_vm=0xffffd39d4b8a1000, size=4 GiB
- pointer 自体は正しい canonical kernel VA。問題は「deref」の段
- CLI: `sudo ./bin/s3fs-ebpf probe-kvbase` で今も再現可能 (歴史遺物として残置)

---

## 2. Path B (implemented) — GUP over daemon user_vm

### 全体像

```text
┌──────────────────────────────────────────────────────────────┐
│                          daemon                              │
│                                                              │
│  arena (BPF map)   ──mmap──→   user_vm [0..N pages]          │
│                                     │                        │
│  S3 Get(body) ──arena.WriteAt(off)──┘                        │
│                                     │                        │
│  admitEntry{ino, uaddr, size}  ──ioctl──→ /dev/s3fs_ebpf     │
└──────────────────────────────────────────────────────────────┘
                                      │
                                      ▼
┌──────────────────────────────────────────────────────────────┐
│                          kmod                                │
│                                                              │
│  S3FS_IOC_ADMIT_ARENA handler:                               │
│    mm = get_task_mm(current)   /* daemon の mm を pin */     │
│    admit_replace(mm, entries[])                              │
│                                                              │
│  hash table:  ino → (uaddr, size)                            │
│  mm ref:      admitted_mm (mmget/mmput で lifecycle 管理)    │
│                                                              │
│  .read(inode, buf, count, pos):                              │
│    admit = lookup(inode->i_ino)                              │
│    if admit:                                                 │
│      pages[1]                                                │
│      pin_user_pages_remote(mm, admit.uaddr & PAGE_MASK,      │
│                             1, 0, pages, NULL)               │
│      kva = kmap_local_page(pages[0])                         │
│      copy_to_user(buf, kva + page_offset + pos, ret)         │
│      kunmap_local(kva)                                       │
│      unpin_user_pages(pages, 1)                              │
│    else:                                                     │
│      fallback to per-inode kmalloc scratch buffer            │
└──────────────────────────────────────────────────────────────┘
```

### なぜ「zero-copy」と呼べるか

- daemon の arena mmap と kmod の GUP pin は **同じ物理 page** を指す
- daemon が `arena.WriteAt(off, data)` で書いた bytes は、そのまま kmod の `copy_to_user` から user に届く (kernel side での memcpy は copy_to_user 1 回のみ、これは avoidable な copy ではない)
- S3 → daemon → arena mmap → GUP → cat の user buffer、という水路の中で **arena→cat 区間には buffer コピーが存在しない**
- `copy_to_user` 自体は user page への書き込みなので、これを「copy」とみなすかは定義次第 (Linux での "zero-copy read" の慣例的定義には合致)

### API とデータ構造

**`include/s3fs_ebpf_uapi.h`**:

```c
#define S3FS_IOC_MAGIC 0xE3

struct s3fs_admit_entry {
    __u64 ino;
    __u64 uaddr;  /* daemon-side user_vm address of the object bytes */
    __u64 size;
};

struct s3fs_admit_arena {
    __u32 count;   /* 0 clears the table */
    __u32 _pad;
    __u64 entries; /* user ptr to admit_entry[] */
};

#define S3FS_IOC_ADMIT_ARENA _IOW(S3FS_IOC_MAGIC, 0x02, struct s3fs_admit_arena)
```

**kmod 側 admit table (`kmod/vfs_ops.c`)**:

```c
struct s3fs_admit {
    struct hlist_node node;
    u64 ino;
    u64 uaddr;
    u64 size;
};

static DEFINE_HASHTABLE(s3fs_admits, 6);        /* 64 buckets */
static DEFINE_SPINLOCK(s3fs_admits_lock);
static struct mm_struct *s3fs_admitted_mm;      /* daemon の mm ref */

int s3fs_ebpf_admit_replace(struct mm_struct *mm,
                             const struct s3fs_admit_entry *entries,
                             u32 count);
```

**daemon 側 admit ioctl (`daemon/cmd/s3fs-ebpf/admit_arena.go`)**:

```go
type admitEntry struct { Ino, Uaddr, Size uint64 }

func admitArena(devPath string, entries []admitEntry) error
```

### Lifecycle

1. daemon `Load()` — arena 作成 + mmap
2. daemon `prewarm()`:
   - S3 Stat → `WarmCache(stub)` (Fast Path 用 32B stub を arena 前半に)
   - S3 Get → `AdmitContent(body)` (実 body を arena に、offset 返す)
   - `admitEntry{ino, uaddr: arena_base + offset, size: len(body)}` を collect
3. daemon `admitArena(dev, admits)` — ioctl
4. kmod ioctl handler:
   - `mm = get_task_mm(current)` — daemon mm を pin (mmget 相当)
   - `s3fs_ebpf_admit_replace(mm, entries, count)` — hash table 差し替え、old mm を mmput
5. `cat` 実行時:
   - VFS → kmod `s3fs_ebpf_file_read`
   - hash lookup で admit hit → `s3fs_admit_read(mm, uaddr, ...)`
   - `pin_user_pages_remote` → `kmap_local_page` → `copy_to_user` → `unpin`
6. daemon 停止時 (SIGINT):
   - defer で `admitArena(dev, nil)` (count=0)
   - kmod は hash を空にし `mmput(admitted_mm)` → daemon の mm 参照 drop
   - 以降の `.read` は kmalloc fallback へ自動切り替わる

### 制約と既知の限界

| 項目 | 内容 | 対策 |
|---|---|---|
| **PoC は 1 page read only** | `s3fs_admit_read` は max PAGE_SIZE (4 KiB) でクランプ | VFS が再呼び出しするので複数 page は自然に拾える (実装ずみ、テスト未) |
| **ページ跨ぎは分割 read** | 1 回の read で最大 1 page → 大 file は複数 syscall | 実用上は問題なし。Linux の read syscall はもともと partial return が許される |
| **admit の粒度は object 全体** | 1 ino に 1 (uaddr, size) の tuple のみ | 巨大 object を chunk 単位で admit する場合は entry を複数持たせる (ino が同じ複数エントリは現状 first hit 優先) |
| **daemon が死ぬと fallback へ** | daemon が admit_clear ioctl 出さずに落ちても、次に別の daemon が admit ioctl 発行するときに古い mm が置き換わる。それまでは stale mm を持ち続ける | `.release` handler が使えれば on close で clear できる。open/close 毎に ioctl 発行する現行モデルでは trigger できないので、daemon 起動時に一度 `admit_clear` を送る規約にすると安全 |
| **`ls` の i_size** | admit は cached inode の i_size を `ilookup + i_size_write` で更新するが、`ls` が admit 前に走ると 17B (kmod initial) のまま cache に残る | `ls -la` 再実行 (VFS が再 stat する契機) or admit タイミングを mount 直後にする |
| **mm 参照の race (縮小)** | admit_replace は spinlock 内で swap、外で mmput。read path は spinlock 内で mm pointer を copy して外で使う。 その間に別 admit_replace が走ると `mmput(prev)` が発火 → GUP call がまだ `mmget_not_zero` していないので理論上 UAF 可能性 | 現状は race window 小、実用上は問題出ていない。厳密にやるなら read path も spinlock 内で `mmget_not_zero` してから out する |

### 実測 (2026-09-22)

```
[prewarm] ino=100 name=hello.txt size=16 src=s3://hello.txt → stub=0
[prewarm] ino=100 body=16 bytes → content=32 uaddr=0x79a0c7415020
[prewarm] ino=102 name=models_weights.bin size=22 ... → stub=48
[prewarm] ino=102 body=22 bytes → content=80 uaddr=0x79a0c7415050
[prewarm] ino=101 name=world.txt size=12 ... → stub=102
[prewarm] ino=101 body=12 bytes → content=134 uaddr=0x79a0c7415086
[admit] 3 entries via /dev/s3fs_ebpf

$ cat /mnt/s3demo/hello.txt
hello, s3fs-ebpf
$ wc -c /mnt/s3demo/hello.txt
16 /mnt/s3demo/hello.txt      # ← S3 の実バイト (改行なし 16B)

$ sudo pkill -INT -f 's3fs-ebpf fastpath'
$ cat /mnt/s3demo/hello.txt
hello, s3fs-ebpf
$ wc -c /mnt/s3demo/hello.txt
17 /mnt/s3demo/hello.txt      # ← kmod initial_content fallback (改行付き 17B)
```

**dmesg で admit ログ**:

```
s3fs_ebpf: admitted 3 entries mm=00000000eba40f87 (daemon)
s3fs_ebpf: admit cleared         ← daemon SIGINT 後
```

---

## 3. Path C (deferred) — read miss upcall

### 動機

B は「daemon が事前に prewarm した ino だけ」arena バイトを返す。実運用の FS では:

- 新規 ino (bucket に新しく置かれた object) が最初 admit されていない
- admit されていない ino に `cat` が来たら kmod fallback に落ちる → 現状は kmod compile-time initial_content が返るだけ (S3 の実物ではない)

これを解決するのが C。**read miss を daemon が非同期に検出 → S3 fetch → admit → retry** のパイプラインを組む。

### 設計スケッチ

```text
                                                   [1] cat unadmitted ino
kmod                          daemon               ─────────────────────────►
─────────────                 ──────
                                                   ┌──── VFS
.read(inode, ...)                                  │
  admit lookup → miss                              │
  atomic64_inc(&read_misses)                       │
  ringbuf_output(ino, op=READ)  ──────────────►   [2] daemon consumer:
  return -EAGAIN (or wait_for_completion)          │   ev := ringbuf.Read()
                                                   │   body := s3c.Get(ev.ino)
                                                   │   AdmitContent(body)
                                                   │   admitArena([...])
                                                   ┌──── retry (or wait)
                                                   │
                                                   │  .read again
                                                   │    admit lookup → hit
                                                   │    GUP + kmap 経由で返す
                                                   ▼
```

### 実装候補

**(a) Poll + retry (簡易)** — `cat` から見て miss は EIO/EAGAIN で失敗、userspace 側で retry:
- kmod: miss で `-EAGAIN` 返す + ringbuf に read miss event 発火
- daemon: ringbuf consumer が S3 fetch + admit
- 呼び側 (`cat`) は最初 fail するが `for i in 1 2 3; do cat; done` で 2 発目から成功
- **Meetup 向きではない** (透過性がない)、実 FS には不適

**(b) Completion wait (透過)** — kmod が daemon の admit を wait して透過的に成功:
- kmod: miss で ringbuf 発火 + `wait_for_completion_timeout(&ino_completion, HZ)`
- daemon: admit ioctl 完了時に kmod へ `S3FS_IOC_SIGNAL_ADMIT(ino)` で completion を signal
- 完了後 kmod は再度 admit lookup → hit → GUP path
- **透過的** — `cat` は 1 発で成功。ただし latency は S3 fetch 分乗る
- 実装コスト: completion table (ino → struct completion) の管理 + timeout 時の EIO 処理 + admit ioctl の意味拡張

**(c) 上位 VFS を経由した非同期 read (発展)** — Linux 6.5+ の `iomap` or `aio` 経由:
- kmod が `.read_iter` を実装し `IOCB_NOWAIT` を honor
- ino miss なら `-EIOCBQUEUED` を返して非同期完了
- daemon が admit したら completion 発火
- **一番真面目な設計** だが実装工数大、PoC の scope 外

**推奨**: **(b) completion wait** を実装。透過性が上がる代わりに実装は 100 行程度。既存 admit ioctl + BPF ringbuf infrastructure で足りる (新規 kernel API 不要)。

### C の詳細インターフェイス提案

**新 UAPI (`include/s3fs_ebpf_uapi.h`)**:

```c
/*
 * Daemon が S3 fetch + AdmitContent + admit_arena 一式完了時に、
 * 待機中の kmod .read を起こすためのシグナル。
 */
struct s3fs_signal_admit {
    __u64 ino;
    __s32 err;  /* 0=OK, negative errno on failure */
    __u32 _pad;
};

#define S3FS_IOC_SIGNAL_ADMIT _IOW(S3FS_IOC_MAGIC, 0x03, struct s3fs_signal_admit)
```

**kmod 側の追加 (`kmod/vfs_ops.c`)**:

```c
/* Per-miss completion. ino をキーにする in-flight table. */
struct s3fs_pending_read {
    struct hlist_node node;
    u64 ino;
    struct completion done;
    int err;
    refcount_t refs;
};

static DEFINE_HASHTABLE(s3fs_pending, 4);   /* 16 buckets、同時 miss は少数想定 */
static DEFINE_SPINLOCK(s3fs_pending_lock);

/* .read の miss branch */
if (!admit) {
    p = pending_get_or_create(inode->i_ino);   /* new or existing entry, refcount++ */
    emit_read_miss_event(inode->i_ino);        /* ringbuf 経由 */
    ret = wait_for_completion_interruptible_timeout(&p->done, HZ);
    if (ret <= 0) {
        pending_put(p);
        return ret == 0 ? -ETIMEDOUT : -EINTR;
    }
    if (p->err) { pending_put(p); return p->err; }
    pending_put(p);
    /* retry: admit table 再検索して arena path へ */
    goto retry_admit_lookup;
}
```

**daemon 側の追加**:

```go
// ringbuf consumer 内、既存の warmFromS3 と同じ位置
if ev.Op == fpebpf.OpRead {
    body, err := s3c.Get(ctx, entryS3Key(ev.Ino))
    if err != nil {
        signalAdmit(dev, ev.Ino, -EIO)   // 失敗も通知
        continue
    }
    off, _ := progs.AdmitContent(body)
    admitArena(dev, []admitEntry{{Ino: ev.Ino, Uaddr: base+off, Size: len(body)}})
    signalAdmit(dev, ev.Ino, 0)          // 完了通知
}
```

### C 実装の見積もり

| 項目 | 変更ファイル | 行数目安 |
|---|---|---|
| UAPI 追加 | `include/s3fs_ebpf_uapi.h` | +15 |
| pending table + wait/signal | `kmod/vfs_ops.c` | +80 |
| SIGNAL_ADMIT ioctl handler | `kmod/s3fs_ebpf_dev.c` | +25 |
| ringbuf に read miss event 発火 | `kmod/vfs_ops.c` (bpf helper 経由 or 直接 kmalloc + memcpy) | +30 |
| daemon consumer 拡張 | `daemon/cmd/s3fs-ebpf/main.go` | +40 |
| signalAdmit wrapper | `daemon/cmd/s3fs-ebpf/admit_arena.go` | +20 |
| 合計 | — | ~210 |

### C の未解決検討事項

- **ringbuf write from kmod**: 通常 ringbuf は BPF program が書く。kmod から直接書く場合、`bpf_ringbuf_output` の kernel-side 実体 (`__bpf_ringbuf_reserve` + `bpf_ringbuf_submit`) が export 済かを確認する必要あり。未 export ならおそらく別チャネル (netlink or 独自 char dev read syscall) が必要
- **completion table のライフサイクル**: 同 ino に複数 read が並行して来た場合、pending entry を共有すべきか個別に作るか (refcount で共有が正解)
- **タイムアウト時の挙動**: `-ETIMEDOUT` を返すか、daemon 不在なら kmod fallback (kmalloc) にフォールスルーするか (後者の方が Meetup 訴求は柔らかい)

---

## 4. Path D (upstream) — kernel patch で A′ を復活させる

### 提案する patch

**Patch 1 (小): `bpf_arena_get_kern_vm_start` の export**

```diff
--- a/kernel/bpf/arena.c
+++ b/kernel/bpf/arena.c
@@ -...  bpf_arena_get_kern_vm_start
 u64 bpf_arena_get_kern_vm_start(struct bpf_arena *arena)
 {
        return arena ? (u64)(long)arena->kern_vm->addr : 0;
 }
+EXPORT_SYMBOL_GPL(bpf_arena_get_kern_vm_start);
+EXPORT_SYMBOL_GPL(bpf_arena_get_user_vm_start);
```

これだけでは足りない (kern_vm PTE 問題は残る) が、`struct bpf_arena` の内部 layout に触らなくて済むので kmod 側の fragility は下がる。

**Patch 2 (中): kern_vm range 用 fault handler の追加**

```diff
--- a/kernel/bpf/arena.c
+++ b/kernel/bpf/arena.c
@@  arena creation
        arena->kern_vm = get_vm_area(size, VM_ARENA);
+       arena->kern_vm->flags |= VM_ARENA;
+       arena->kern_vm->page_fault = arena_kern_vm_fault;
```

具体的な API 変更は kernel 側 vm_struct の拡張が要るので大掛かり。alternatively、kmod が呼ぶ helper `bpf_arena_fault_in_range(arena, off, size)` を新設 + EXPORT_SYMBOL_GPL する方が現実的:

```c
/* 新設 */
int bpf_arena_fault_in_range(struct bpf_arena *arena, u64 uvm_off, u64 size)
{
    /* Iterate pages, alloc_page + insert into kern_vm PTEs */
    ...
}
EXPORT_SYMBOL_GPL(bpf_arena_fault_in_range);
```

これがあれば kmod は admit ioctl 時に「事前 fault-in」を呼ぶだけで、以降の read は直接 kern_vm を deref できる。

### Patch を当てる場合の作業

1. Linux 6.12 のソースを取得 (Ubuntu mainline kernel PPA から `.deb` ソース、あるいは `git clone linux-stable`)
2. 上記 patch を当てる
3. `.config` を現行 kernel と揃えて make (30 分〜1 時間)
4. `.deb` を作成 (`make deb-pkg`) して `dpkg -i` で置き換え
5. reboot
6. 現行 A′ コード (probe_kvbase 経由の container_of hack) を差し替えて再テスト

### Meetup / OSS 貢献としての価値

- upstream に投げれば「BPF arena を汎用共有メモリとして使う」ユースケースが正式にサポートされる第一歩になる
- s3fs-ebpf は「BPF arena を FUSE 置き換えの共有 fabric として使う」最初の実例なので、投稿 motivation にできる
- LKML への RFC → maintainer レビューで受け入れられるかは別問題 (「BPF 内部 API を汎用化するのは反対」と言われる可能性あり)

---

## 5. 選び方の指針

| ケース | 推奨 |
|---|---|
| Meetup までに動くデモが必要 | **B のみ** (現状) |
| 実用 FS として使いたい | B + C (upcall 経由で on-demand fetch) |
| upstream 貢献ネタも欲しい | B を primary にしつつ D の RFC を並行 |
| kernel を触りたくない、依存も減らしたい | B のみで完結、C は運用要件次第 |
| latency を極限まで削りたい | D (kernel patch) が確定路線 |

**現状の s3fs-ebpf の位置**: **B** が primary、`docs/motivation.md` sprint 上は Week 4 (a) 完了。C は Week 4 の中盤〜終盤、D は Meetup 後の「発表後の tech-debt 返済 & upstream フェーズ」で扱う想定。
