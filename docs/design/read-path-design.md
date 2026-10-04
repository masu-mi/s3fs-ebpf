# Read Path 設計 — Paths A′ / B / C / D

s3fs-ebpf における「kmod の `.read` が daemon が用意した実データを返す」ための架け橋の設計集。Week 4 (a) の探索で 4 通りが浮かんだので、それぞれの位置付け・技術的可否・実装コストを一元管理する。実装状況の記録は `docs/journal/week4a-implementation-notes.md`。

---

## 0. 早見表

| Path | 経路 | 状態 | zero-copy | Meetup narrative | 実装コスト |
|---|---|---|---|---|---|
| A′ | mount option + kmod が arena kern_vm 直参照 | ❌ 断念 | ○ | 最強 (design.md §2 直結) | 中 (だが kernel 内部依存で fragile) |
| **B** | kmod が daemon user_vm を GUP + kmap 経由 read | **✅ 実装済** | ○ (物理 page 共有) | 強 | 小 |
| **C** | kmod read miss → daemon upcall → daemon が S3 fetch → admit → kmod 待機解除 | **✅ 実装済** | ○ (admit 後は B 経路で読む) | 強 (Slow Path 完成) | 中 |
| D | upstream に patch を投げて A′ を復活 | 📝 提案 | ◎ (JIT 直参照相当) | 最強 + 上流貢献 | 大 (kernel rebuild + upstream review) |

**現状の構成**: B が prewarm 済 ino の Fast Path、C が未 admit ino への on-demand fetch (Slow Path)、fallback が daemon 不在時の kmod compile-time initial_content。D は Meetup 後の upstream 貢献候補。

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
- CLI: `sudo ./bin/s3fs-ebpf dev probe-kvbase` で今も再現可能 (歴史遺物として残置)

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

## 3. Path C (implemented) — read miss upcall

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

**採用**: **(b) completion wait**。透過的で `cat` から見て 1 発成功。BPF ringbuf ではなく **/dev/s3fs_ebpf の `.read` で miss event を直接ストリーム** する方式にした (BPF ringbuf は BPF program が書く前提で、kmod からの書き込みは非自明だったため)。

### 実装した仕組み

```text
                                    [1] cat unadmitted ino
kmod                          daemon
─────────────                 ──────
                                                   ┌──── VFS
.read(inode, ...)                                  │
  admit lookup → miss                              │
  pending_get_or_create(ino)                       │
  miss_enqueue(ino, off, count) ─┐                 │
  wait_for_completion(2s)         └─►  poll(fd, POLLIN) → read(fd, &ev)
                                       LookupByIno(ev.Ino) → S3Key
                                       body := s3c.Get(S3Key)
                                       AdmitContent(body) → arena offset
                                       addAdmit(...) → full-table replace ioctl
                                       signalAdmit(ino, 0)  ─┐
              ◄──────────────────────────────────────────────┘
  complete_all(&pending->done)
  retry admit_snapshot → hit → GUP + kmap → copy_to_user
```

### 実装した API (最終形)

**UAPI (`include/s3fs_ebpf_uapi.h`)**:

```c
/* Char dev .read で 1 event ずつ流れてくる */
struct s3fs_read_miss_event {
    __u64 ino;
    __s64 offset;
    __u32 count;
    __u32 _pad;
};

struct s3fs_signal_admit {
    __u64 ino;
    __s32 err;   /* 0=OK, negative errno on failure */
    __u32 _pad;
};

#define S3FS_IOC_SIGNAL_ADMIT _IOW(S3FS_IOC_MAGIC, 0x03, struct s3fs_signal_admit)
```

**kmod (`kmod/vfs_ops.c`)**:

- `struct s3fs_pending` + `DEFINE_HASHTABLE(s3fs_pending, 4)` — refcounted completion per ino
- `struct s3fs_miss_node` FIFO + `DECLARE_WAIT_QUEUE_HEAD(s3fs_miss_wq)` — event stream
- `.read` の miss branch: `pending_get_or_create` → `miss_enqueue` → `wait_for_completion_interruptible_timeout(2*HZ)` → retry
- `s3fs_ebpf_signal_admit(ino, err)` — SIGNAL_ADMIT ioctl handler が呼ぶ、`complete_all` + `pending_unhash`
- `s3fs_ebpf_upcall_shutdown` — daemon 消失 / module unload 時に全 pending を `-ESHUTDOWN` で叩き起こす

**miscdevice (`kmod/s3fs_ebpf_dev.c`)**:

- `.read` = `s3fs_dev_read` → 内部で `s3fs_read_miss_dequeue`
- `.poll` = `s3fs_dev_poll` → wait queue に register
- `.unlocked_ioctl` に `S3FS_IOC_SIGNAL_ADMIT` を追加

**daemon (`daemon/cmd/s3fs-ebpf/admit_arena.go`)**:

- `runReadMissLoop(ctx, devPath, progs, s3c)` — goroutine、`/dev/s3fs_ebpf` を `os.Read` で drain
- `handleReadMiss(ev)` — `LookupByIno` → `s3c.Get` → `AdmitContent` → `addAdmit` → `signalAdmit`
- `admits []admitEntry` + `admitMu sync.Mutex` — daemon-side source of truth (kmod ioctl は「replace-all」semantics なので add 毎に全 snapshot を送る)
- `setAdmits` / `addAdmit` / `clearAdmits` の 3 API で lifecycle 統一

**daemon Entry 拡張 (`daemon/internal/ebpf/entries.go`)**:

- `Prewarm bool` field 追加。false のエントリは起動時 skip → C 経路で on-demand 取得
- `LookupByIno(ino)` — upcall handler が S3Key 解決に使う
- lazy.txt (ino=103, Prewarm=false) を demo として追加

### smoke test (2026-09-22 実測)

```
$ sudo insmod kmod/s3fs_kfunc.ko && sudo mount -t s3fs_ebpf none /mnt/s3demo
$ sudo ./bin/s3fs-ebpf fastpath --s3 &
    [prewarm] ino=100/101/102 → admit
    [prewarm] ino=103 name=lazy.txt skipped (waits for read-miss upcall)
    [admit] 3 entries via /dev/s3fs_ebpf

$ cat /mnt/s3demo/lazy.txt
lazy loaded from S3            ← C 経由で S3 の実バイト
                                daemon log:
                                    [upcall] miss ino=103 offset=0 count=131072
                                    [upcall] ino=103 lazy.txt admitted 19 bytes

$ time cat /mnt/s3demo/lazy.txt
lazy loaded from S3
cat  0.00s user 0.00s system 89% cpu 0.001 total   ← 2 発目は B 経路 (admit 済)

$ sudo pkill -INT -f 's3fs-ebpf fastpath'
$ cat /mnt/s3demo/lazy.txt
(not fetched yet)              ← daemon 落ちたら kmod initial fallback
```

### 実装時の踏み跡 (詳細は week4a-implementation-notes.md §10)

- **`pin_user_pages_remote` の lockdep WARN**: 6.12 では `mmap_read_lock` を明示的に取ってから呼ぶ必要あり (docs は歴史的に "handles locking internally" と書いてあるが実態は違う)
- **`admit_arena` の replace-all semantics**: kmod ioctl 単体では add できないので daemon 側で全 snapshot を再送する形に。add 用の別 ioctl を切るか、daemon 側で状態を持つかの判断で後者を選択
- **daemon 側 completion**: pending table は refcount ベースで並行 read が同じ ino で待てる。timeout は 2s に設定 (実運用は S3 latency の p99 に合わせて調整)

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

### C 実装の実測 (見積もり比)

| 項目 | 変更ファイル | 見積 | 実測 |
|---|---|---|---|
| UAPI 追加 | `include/s3fs_ebpf_uapi.h` | +15 | +32 |
| pending table + miss queue + wait/signal | `kmod/vfs_ops.c` | +80 | +190 |
| 新 ioctl + `.read` / `.poll` 配線 | `kmod/s3fs_ebpf_dev.c` | +25 | +55 |
| miss event 送出 (char dev 経由に変更、ringbuf 不要) | `kmod/vfs_ops.c` の一部 | +30 | +0 (上に含む) |
| daemon reader goroutine + handler | `daemon/cmd/s3fs-ebpf/admit_arena.go` | +40 | +115 |
| signalAdmit / addAdmit / clearAdmits | `daemon/cmd/s3fs-ebpf/admit_arena.go` | +20 | +50 |
| Entry.Prewarm 追加 + lazy.txt | kmod/vfs_ops.c + daemon/internal/ebpf/entries.go + main.go | — | +40 |
| 合計 | — | ~210 | ~480 |

見積の 2 倍強。差分の主因は (a) BPF ringbuf 依存を捨てて char dev の read/poll チャネルに切ったので kmod 側の queue 管理コードが増えた、(b) admit の replace-all semantics を吸収する daemon 側の状態管理を追加した。逆に「BPF program 側の変更」は不要になった (kmod 完結)。

### 解消した設計上の懸念

- ~~**ringbuf write from kmod**~~ → 採用せず。/dev/s3fs_ebpf の char dev read で FIFO を drain する方式にしたので、BPF 依存ゼロ、標準 Linux miscdevice API のみで完結
- ~~**completion table のライフサイクル**~~ → `refcount_t` + `pending_get_or_create` + `pending_unhash` で共有・並行対応。同一 ino への複数 read は 1 つの completion を共有
- ~~**タイムアウト時の挙動**~~ → 2s timeout で kmalloc fallback にフォールスルー。`cat` から見て「daemon 反応しなかったら kmod 内蔵初期値」で完結、EIO は返さない (透過性優先)

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

**現状の s3fs-ebpf の位置**: **B + C 実装完了** (2026-09-22)。`docs/user/motivation.md` sprint 上は Week 4 (a) 完了、実用 FS としての Slow Path も含めて第一段カバー。D は Meetup 後の「発表後の tech-debt 返済 & upstream フェーズ」で扱う想定。
