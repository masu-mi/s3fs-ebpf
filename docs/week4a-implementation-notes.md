# Week 4 (a) 実装ノート — arena backed read の技術課題

Week 3 が「VFS を materialise して cat が動くところまで」だったのに対し、Week 4 (a) は **daemon が持つ arena の中身を kmod の `.read` から返す** ためのブリッジ作りの記録。設計上は「mount で map_id を渡し、kmod が arena kernel-VM を直参照する」(design.md §2 の Option 1) が本命だったが、`bpf_arena` の内部実装と 6.12 kernel の制約に阻まれて別経路にピボットした。踏んだ地雷と回避策を一次情報のまま残す。

対応 commit: master ブランチの Week 4 (a) サブトラック (spike + B pivot)。

---

## 0. 対応関係の早見表

| 症状 | 原因 | 対策 | 参照 |
|---|---|---|---|
| `bpf_arena_get_kern_vm_start` が kmod からリンクできない | `__ksymtab_` エントリなし (未 export)、`struct bpf_arena` も forward decl のみ | vmlinux BTF から struct 定義を吸い、kmod 側で redefine → `container_of` で `arena->kern_vm->addr` を抜く。version guard 必須 | §1 |
| `modpost: uses symbol bpf_map_get from namespace BPF_INTERNAL` | 6.12 で `bpf_map_get` が namespace 化 | モジュール側で `MODULE_IMPORT_NS(BPF_INTERNAL)` を宣言 | §2 |
| `no_llseek` undeclared (misc device 定義時) | 6.12 で削除 (`noop_llseek` に統合) | `noop_llseek` に置換 | §2 |
| kmod からの arena kern_vm 直 deref が全 EFAULT | kern_vm range に generic-kernel-context 用 fault handler 未登録。`arena_vm_fault` は user_vm VMA 用のみ | A′ 断念、後述の B (GUP) にピボット | §3 |
| `bpf_arena_alloc_pages(map, hint, 1, ...)` + BPF write でも kern_vm PTE install されず | BPF JIT の addr_space_cast は独自経路で kern_vm 触るが、汎用 kernel access には fault handler が反応しない | 同上、B へ | §4 |
| daemon プロセスが SIGINT でも zombie 化して残る | 背景実行中の子プロセスがまだ arena fd を握って ioctl 待ち | `pkill -9 -f s3fs-ebpf` で強制、あるいは stop 順序 (§停止フロー) 遵守 | §5 |
| GUP path で `pin_user_pages_remote` が呼べない | kernel header namespace 制約 (`FOLL_LONGTERM` 制限、`get_user_pages_remote` の nested lock 要求) | flags=0 で read-only pin、mm ref は事前に `get_task_mm(current)` で確保 | §6 |
| daemon 落とすと mmput の drop 順序で mm 参照リークっぽく見える | admit clear ioctl で `mmput` 発火、prev_mm を spinlock 外で解放する順序に | `s3fs_ebpf_admit_replace` 内で lock 取得 → 差し替え → unlock 後に `mmput(prev_mm)` の 3 段 | §7 |

---

## 1. `struct bpf_arena` の BTF-derived redefine

**症状:** kmod から `bpf_arena_get_kern_vm_start(struct bpf_arena *)` を呼びたい。kernel header (`<linux/bpf.h>`) に prototype はあるが、`__ksymtab_bpf_arena_get_kern_vm_start` は `/proc/kallsyms` に出ない → EXPORT_SYMBOL_GPL されていないため kmod からリンク不可。同 header の `struct bpf_arena` も forward declaration (`struct bpf_arena;`) のみで、`->kern_vm` に触れられない。

**原因:** upstream の `kernel/bpf/arena.c` で `struct bpf_arena` は完全に private (定義は arena.c 内部のみ)。KFunc / BPF 内蔵ヘルパー経由でしか触らせない意図。

**対策:** vmlinux BTF から layout を吸って kmod 側で opaque struct を redefine。バージョンで layout がずれるので `#if LINUX_VERSION_CODE < KERNEL_VERSION(6, 12, 0)` で守る。

```bash
sudo bpftool btf dump file /sys/kernel/btf/vmlinux format c \
    | awk '/^struct bpf_arena[[:space:]]*{/,/^};/'
```

の出力:

```c
struct bpf_arena {
    struct bpf_map map;
    u64 user_vm_start;
    u64 user_vm_end;
    struct vm_struct *kern_vm;
    struct maple_tree mt;
    struct list_head vma_list;
    struct mutex lock;
};
```

kmod 側 (`kmod/s3fs_ebpf_dev.c`) では末尾 field は触らないので前半だけ再定義:

```c
struct bpf_arena_layout_6_12 {
    struct bpf_map map;
    u64 user_vm_start;
    u64 user_vm_end;
    struct vm_struct *kern_vm;
    /* maple_tree / list_head / mutex は触らない */
};

static long probe_kvbase(u32 fd) {
    struct bpf_map *map = bpf_map_get(fd);       /* これは export 済 */
    struct bpf_arena_layout_6_12 *arena =
        container_of(map, struct bpf_arena_layout_6_12, map);
    u64 kvbase = (u64)(uintptr_t)arena->kern_vm->addr;
    ...
}
```

**踏んだ点:**
- `struct vm_struct` は `<linux/vmalloc.h>` の public 型なので `->addr` は普通に取れる
- 実測: daemon の `arena.BaseAddr()` = `0x7a5ca837d000` (user VA) に対し `arena->kern_vm->addr` = `0xffffd39d4b8a1000` (canonical kernel VA)、`size` = `4295036928` bytes ≈ 4 GiB (VA 予約サイズ)
- **container_of は正しい pointer を返す** — `user_vm_start` が dmesg で確認でき、daemon の mmap 先と一致
- **ただし kern_vm を deref できるかは別問題** (§3)

**参考:** `kernel/bpf/arena.c` (upstream)、`include/linux/vmalloc.h`。

---

## 2. namespace import と 6.12 の細かい API 変更

**症状 1:** kmod build が `modpost` で以下のエラー:

```
ERROR: modpost: module s3fs_kfunc uses symbol bpf_map_get from namespace BPF_INTERNAL, but does not import it.
```

**原因:** kernel symbol namespaces (5.4+) の運用強化で 6.x のどこかで `bpf_map_get` を `BPF_INTERNAL` namespace に押し込む変更が入った。namespace 化された symbol は明示的に `MODULE_IMPORT_NS` しないと使えない。

**対策:** モジュール定義に一行足す。

```c
MODULE_IMPORT_NS(BPF_INTERNAL);
```

置き場所は `MODULE_LICENSE` などと並ぶ場所 (`kmod/s3fs_kfunc_main.c`)。

**症状 2:** ビルド時に `no_llseek` undeclared。

**原因:** 6.12 で `no_llseek` が削除され、代替は `noop_llseek`。file_operations の `.llseek = no_llseek` を書くと落ちる。

**対策:** miscdevice の `file_operations` を書くとき `.llseek = noop_llseek` にする。あるいは省略 (VFS デフォルトが `default_llseek` に自動フォールバック)。今回は明示的に `noop_llseek` を使った。

---

## 3. Blocker: kmod からの arena kern_vm 直 deref は不能

**症状:** §1 の手順で正しい kern_vm base pointer を kmod で得られる。しかしそれを deref (copy_to_user の src や copy_from_kernel_nofault の対象) にすると **EFAULT (r=-14)**。daemon が事前に arena.WriteAt で書き込んでいる page でも同じ。

**原因の切り分け:**

- `arena_vm_fault` (kernel symbol) は登録されているが、これは **user_vm VMA 用の handler** (`arena_map_mmap` で set)
- kern_vm range (`arena->kern_vm` の `vm_struct`) には **generic-context 用 fault handler が無い**
- BPF プログラムが addr_space_cast 経由で arena を触る際は JIT の runtime helper (`arena_map_direct_value_addr` etc.) を通るので、汎用 kernel メモリアクセスとは別経路
- 結果として kmod (通常の kernel context) から kern_vm を deref すると unmapped page への load となり、fault handler も反応しないため PF fixup 経由で EFAULT

**確認手法 (`kmod/s3fs_ebpf_dev.c` に一時的に埋めた診断):**

```c
u8 probe = 0xEE;
int r = copy_from_kernel_nofault(&probe,
    (void *)(uintptr_t)(kbase + entries[i].arena_off), 1);
pr_info("s3fs_ebpf: probe ino=%llu off=%llu -> r=%d byte=0x%02x\n",
    entries[i].ino, entries[i].arena_off, r, probe);
```

dmesg:

```
s3fs_ebpf: probe ino=100 off=32 -> r=-14 byte=0xee
s3fs_ebpf: probe ino=102 off=80 -> r=-14 byte=0xee
s3fs_ebpf: probe ino=101 off=134 -> r=-14 byte=0xee
```

`0xEE` は sentinel (書き込まれなかった証拠)、`r=-14` は EFAULT。**アクセス自体が届いていない**。

**対策:** kmod からの arena 直参照は諦める。B (GUP) にピボット (§5)。

**副次的発見:**
- `/proc/kallsyms | grep arena.*fault` は `arena_vm_fault` 1 個だけ返す (kern_vm 用 handler が存在しない証拠)
- `bpf_arena_get_kern_vm_start` / `bpf_arena_get_user_vm_start` は kernel text にあるが `__ksymtab_` にない (未 export)

---

## 4. `bpf_arena_alloc_pages` + BPF write でも kern_vm PTE は install されない

**症状:** §3 の EFAULT を回避する希望として「BPF プログラム側で `bpf_arena_alloc_pages` を呼んで両側の PTE を install → daemon の user_vm write は install 済 page への書き込み → kmod の kern_vm read も通るはず」を試したが、**やはり全 EFAULT**。

**詳細な試行:**

1. `SEC("syscall")` プログラム `s3fs_arena_prealloc` を追加:
   ```c
   extern void *bpf_arena_alloc_pages(void *map, void *addr, u32 page_cnt,
                                       int node_id, u64 flags) __ksym;
   
   SEC("syscall")
   int s3fs_arena_prealloc(struct s3fs_test_args *ctx) {
       void *p = bpf_arena_alloc_pages(&s3fs_arena,
           (void*)(unsigned long)ctx->arena_base_uaddr, ctx->idx, -1, 0);
       if (!p) return -2;
       /* さらに念のため BPF write で touch */
       __u8 __arena *b = (__u8 __arena *)p;
       for (__u32 i = 0; i < ctx->idx && i < 16; i++)
           b[i * 4096] = 0;
       ctx->out_ino = (u64)(unsigned long)p;
       return 0;
   }
   ```
2. daemon 側で prewarm 前に `PreallocArenaPages(1)` を呼び、その後 arena.WriteAt で content を書く
3. 再び kmod の probe を走らせる → **依然 EFAULT**

**原因の推測:**
- `bpf_arena_alloc_pages` は物理 page を確保して maple_tree に登録し、`arena_vma_fault` が userspace mmap fault 時に参照できるようにするだけ
- kern_vm PTE の install は JIT 経路で addr_space_cast が発火したときに arena 内部で行われる
- 汎用 kernel access (copy_to_user 等) は JIT 経路を通らないので install trigger 未発生
- 結果、`bpf_arena_alloc_pages` の返り値は user_vm view の address であり、kern_vm 側の PTE には触っていない

**教訓:** arena の kern_vm 側 memory は「BPF 実行時 context 専用の shadow mapping」であり、外部の kernel code から汎用的にアクセスする API は現状 (6.12) 提供されていない。docs/deisign.md §2 の Option 1 は kernel patch 前提でしか成立しない。

---

## 5. Path B (GUP) へのピボット

**戦略:** arena の kern_vm 側は諦め、daemon の user_vm 側 (arena mmap の userspace ビュー) を **kmod から `pin_user_pages_remote` で pin** して読む。

**なぜこれで動くか:**
- daemon の arena user_vm には `arena_vma_fault` が VMA fault handler として登録されている (`arena_map_mmap` で set)
- GUP は user VMA を辿るときこの fault handler を呼び出す → page が faulted in される
- pin された struct page は `kmap_local_page` で kernel VA が取れる
- そこから `copy_to_user` すれば、daemon の arena mmap と同じ物理 page を経由してユーザに届く

**arena 内部詳細に一切触らない** のがポイント。`struct bpf_arena` の layout も、`bpf_arena_get_kern_vm_start` の export も要らない。

**kmod 側の read パス骨格 (`kmod/vfs_ops.c: s3fs_admit_read`):**

```c
static ssize_t s3fs_admit_read(struct mm_struct *mm, u64 uaddr, u64 asize,
                                char __user *user_buf, size_t count, loff_t *pos)
{
    struct page *page = NULL;
    long pinned;
    void *kva;
    u64 abs_uaddr = uaddr + (u64)*pos;
    unsigned long page_offset = abs_uaddr & (PAGE_SIZE - 1);
    size_t max_in_page = PAGE_SIZE - page_offset;
    ssize_t ret = min_t(size_t, count, asize - (u64)*pos);

    ret = min_t(size_t, ret, max_in_page);

    pinned = pin_user_pages_remote(mm, abs_uaddr & PAGE_MASK, 1,
                                    0 /* read-only */, &page, NULL);
    if (pinned != 1)
        return pinned < 0 ? pinned : -EFAULT;

    kva = kmap_local_page(page);
    if (copy_to_user(user_buf, (u8 *)kva + page_offset, ret))
        ret = -EFAULT;
    kunmap_local(kva);

    unpin_user_pages(&page, 1);
    if (ret > 0)
        *pos += ret;
    return ret;
}
```

**mm reference lifecycle:**
- admit ioctl の handler が `get_task_mm(current)` で daemon mm を acquire
- admit hash table が非空の間、この mm 参照を保持
- admit clear (count=0) or module unload で `mmput`

**smoke:**
```
cat /mnt/s3demo/hello.txt
→ "hello, s3fs-ebpf"          (16 バイト、S3 の実物)
sudo pkill -INT -f 's3fs-ebpf fastpath'
cat /mnt/s3demo/hello.txt
→ "hello, s3fs-ebpf\n"        (17 バイト、kmod initial_content の fallback)
```

**Meetup narrative 保持:** daemon の arena と kmod の read path が同一物理 page を共有する zero-copy パイプライン。arena が「daemon と kmod の共有メモリ fabric」であることは変わらず、単に触り方が「JIT 経由」から「GUP 経由」に変わっただけ。

---

## 6. GUP path の実装で気づいた小さな点

- **`pin_user_pages_remote(..., locked=NULL)`** は内部で `mmap_read_lock(mm)` を取ってくれる。呼び出し側で明示ロック不要
- **`FOLL_LONGTERM` フラグは不要** — per-read の短命 pin なので `gup_flags=0` で十分。LONGTERM は DAX などで制約がある
- **`kmap_local_page` は non-highmem では NOP** に近い (直接 kernel VA を返す) が、正しく `kunmap_local` する規約は守る (highmem 環境で必要)
- **ページ跨ぎは 1 page でクランプして VFS の再呼び出しに任せる** — PoC 対象のオブジェクトが全て < PAGE_SIZE なので実質発火しないが、実装は将来の複数 page 対応を意識しない
- **`s3fs_active_sb` 経由の `ilookup` + `i_size_write`** で admit 時に cached inode の size を更新するようにした。ただし現状 `ls` は admit 前に走ることが多く、i_size が 17 (kmod initial) のまま cache されるケースが残る。`cat` は問題ないが `stat` の size 表示は fallback 側の値になりうる — 次で fix

---

## 7. daemon zombie 化と mm 参照 lifecycle の注意

**症状:** daemon を `pkill -INT` で落とそうとして pgrep で pid を取ったのに、少し経つと同じ pid の zombie が残っているように見える。

**原因:** background 実行 (`... &`) された daemon は subshell の子プロセスなので、親 shell が exit しない限り reap されない。`pkill -INT` は SIGINT は届いても、シェルによる reap は別。

**対策:**
- 開発中は `pkill -9 -f 's3fs-ebpf'` で強制終了 (kill signal は Go の signal handler を経由しないので即死)
- CI / Meetup デモでは `wait` を明示的に呼ぶか、systemd unit にする
- 「stop 順序」は Week 3 §7 と同じ: `pkill -INT daemon` → `umount` → `rmmod`

**mm 参照の drop 順序 (`kmod/vfs_ops.c: s3fs_ebpf_admit_replace`):**

```c
spin_lock(&s3fs_admits_lock);
admit_clear_locked();                /* 古い hash entries を kfree */
prev_mm = s3fs_admitted_mm;
s3fs_admitted_mm = count ? mm : NULL;
spin_unlock(&s3fs_admits_lock);

if (prev_mm)
    mmput(prev_mm);                  /* lock 外で drop (mmput は sleep 可) */
```

`mmput` は last-put で `exit_mmap` を経由することがあり sleep するので、spinlock 内では呼べない。lock 内で pointer を退避 → unlock → drop、の 3 段は Linux kernel の他 subsystem でも定番パターン。

---

## 8. Meetup 主張とのマッピング

Week 4 (a) B が終わって、Meetup スライドで示せる 1 分デモは以下:

```text
# ターミナル 1: kmod + mount + fastpath + admit
sudo insmod kmod/s3fs_kfunc.ko
sudo mount -t s3fs_ebpf none /mnt/s3demo
sudo ./bin/s3fs-ebpf fastpath --s3 --sample-every=1s
    [prewarm] ino=100 name=hello.txt size=16 src=s3://hello.txt → stub=0
    [prewarm] ino=100 body=16 bytes → content=32 uaddr=0x...
    ...
    [admit] 3 entries via /dev/s3fs_ebpf

# ターミナル 2
cat /mnt/s3demo/hello.txt     # "hello, s3fs-ebpf" (16B、newline 無し = S3 の実物)
wc -c /mnt/s3demo/hello.txt   # 16

# 対比: daemon を止めると
sudo pkill -INT -f 's3fs-ebpf fastpath'
cat /mnt/s3demo/hello.txt     # "hello, s3fs-ebpf\n" (17B = kmod initial_content の fallback)
wc -c /mnt/s3demo/hello.txt   # 17
```

「daemon が起動時に S3 body を bpf_arena mmap に注ぐ → kmod の .read が GUP で同じ物理 page を掴んで zero-copy で返す → daemon を落とすと kmod initial fallback に切り替わる」を 30 秒で見せられる。

---

## 9. 残タスク

- **Week 4 (a) C**: ✅ 完成 (§10 参照)
- **Week 4 (b)**: ✅ 完成 (`docs/week4b-implementation-notes.md`)
- **Week 4 (c)**: ✅ 完成 (`docs/week4c-implementation-notes.md`)
- **Week 4 (d)**: Lua/WASM policy engine
- **Week 4 (e)**: benchmark。s3fs-fuse / Mountpoint との `stat` + `cat` latency 比較
- **D (upstream patch)**: `bpf_arena_get_kern_vm_start` の EXPORT_SYMBOL_GPL、または kern_vm range 用 fault handler の追加。当てれば A′ 経路が復活し、B の GUP overhead が消える (数百ns オーダーの改善見込み)

---

## 10. Path C (read upcall) 実装で踏んだ点

Path C の設計は `docs/read-path-design.md §3` にある。ここでは実装中に踏んだ地雷だけを残す。

### 10.1 `pin_user_pages_remote` の lockdep WARN

**症状:** B で使っていた `pin_user_pages_remote(mm, addr, 1, 0, &page, NULL)` を C の同じ path でも呼んだら (実際は C 経由で admit された ino を .read するときに B path を通るので同じ関数) dmesg に WARN のスタックトレースが出る:

```
WARNING: CPU: 2 PID: ... at include/linux/rwsem.h:80 __gup_longterm_locked+0x7d3/0x9f0
Call Trace:
 pin_user_pages_remote+0x73/0xf0
 s3fs_admit_read+0xa1/0x170 [s3fs_kfunc]
 s3fs_ebpf_file_read+0x3a2/0x5a0 [s3fs_kfunc]
 vfs_read+0xf6/0x390
```

**原因:** 6.12 の `pin_user_pages_remote` は `mmap_read_lock(mm)` が既に取られていることを前提にする内部 assert が入っている。ドキュメント (kernel-doc および LWN の解説) は「internally handles locking」と書いてあるが実態は違う。`locked` out-parameter を NULL にした場合も、内部の `int local_locked = 1;` (「lock 済」を意味する) と共に下位関数へ渡されるので、lock を取っていないと WARN する。

**対策 (`kmod/vfs_ops.c: s3fs_admit_read`):**

```c
int locked = 1;
mmap_read_lock(mm);
pinned = pin_user_pages_remote(mm, abs_uaddr & PAGE_MASK, 1,
                                0 /* read-only */, &page, &locked);
if (locked)
    mmap_read_unlock(mm);
```

- `mmap_read_lock` を明示取得
- `locked` を out-parameter として渡す。関数内部で drop + reacquire されうるので、返り値見て自分で unlock
- 一発目の smoke で WARN 消失を確認 (dmesg | grep WARN が空)

### 10.2 `admit_arena` ioctl の replace-all semantics

**症状:** C の upcall handler で `admitArena(devPath, []admitEntry{{Ino: 103, ...}})` と 1 件だけ渡したら、既に prewarm 済の 100/101/102 が **消えた**。

**原因:** kmod 側 `s3fs_ebpf_admit_replace` が **table 全置換** semantics だった。`hash_clear + hash_add for each entry` なので、`count=1` で呼ぶと過去の hash entries が消える。

**対策 (`daemon/cmd/s3fs-ebpf/admit_arena.go`):** daemon 側で source of truth を持ち、常に全 snapshot を送る:

```go
var admits []admitEntry
var admitMu sync.Mutex

func addAdmit(devPath string, entry admitEntry) error {
    admitMu.Lock()
    admits = append(admits, entry)
    snap := append([]admitEntry(nil), admits...)
    admitMu.Unlock()
    return admitArena(devPath, snap)
}
```

- `setAdmits` (初期化)、`addAdmit` (追加)、`clearAdmits` (全消し) の 3 API に整理
- kmod ioctl 側は現状のまま (replace-all のシンプルな semantics を維持)
- 別案: `S3FS_IOC_APPEND_ARENA` ioctl を切って kmod 側で merge する。将来的にはこちらが素直だが、PoC scope で daemon-side 管理を選んだ

### 10.3 pending completion の refcount lifecycle

**症状:** 同 ino に複数 read が並行して届いたとき、pending entry を「共有する」か「個別に作る」かで挙動が変わる。個別だと event queue に同じ ino の miss event が何度も積まれて daemon が S3 Get を重複実行する。

**対策 (`kmod/vfs_ops.c`):** `refcount_t` ベースの共有:

```c
struct s3fs_pending {
    struct hlist_node node;
    u64 ino;
    struct completion done;
    int err;
    refcount_t refs;
};

static struct s3fs_pending *pending_get_or_create(u64 ino) {
    /* Lookup + refcount_inc, or allocate with refcount=2 (hash + caller) */
    ...
}

static void pending_put(struct s3fs_pending *p) {
    if (refcount_dec_and_test(&p->refs)) kfree(p);
}

static void pending_unhash(struct s3fs_pending *p) {
    /* hash_del + pending_put; called once by SIGNAL_ADMIT */
    ...
}
```

- 2 度目以降の miss は既存 pending にぶら下がる → daemon への miss event は 1 回だけ
- `complete_all` (not `complete`) を使うので待機中の全 read が一斉に起きる
- `signal_admit` が hash から抜いて 1 つ ref を落とす、待機側は自身の ref を drop

### 10.4 char dev の event stream 設計判断

**なぜ BPF ringbuf を使わなかったか:** BPF ringbuf は BPF program 側が `bpf_ringbuf_reserve/submit` で書く前提。kmod から直接書く場合は `__bpf_ringbuf_reserve` を呼ぶ必要があるが、これは export されていない可能性が高い (未確認)。仮に呼べても、arena namespace / verifier context の複雑さが増す。

**採用した方式:** `/dev/s3fs_ebpf` の `.read` と `.poll` を miscdevice fops に配線 → 内部に `LIST_HEAD` + `wait_queue_head` を持ち、`s3fs_read_miss_dequeue` で 1 event 単位で drain。daemon 側は `os.File.Read` を loop するだけ。

**利点:**
- 完全に BPF から独立、標準 Linux miscdevice API のみ
- daemon は `select(2)` / `poll(2)` が使える (Go の runtime も自然に対応)
- 追加チャネル (netlink socket etc.) を張るより 1 char dev で ioctl/read/poll 全部済む

### 10.5 daemon shutdown 時の未 completion pending

**症状:** daemon が SIGINT で落ちるとき、kmod にまだ待機中の pending がある可能性 (upcall event を送ったが daemon が読む前 or 処理中に SIGINT)。

**対策 (`kmod/vfs_ops.c: s3fs_ebpf_upcall_shutdown`):** admit clear と同じタイミングで pending 全部を `-ESHUTDOWN` で `complete_all`。待機中の `.read` は syscall から `-ESHUTDOWN` (`-108`) を返す。cat の場合はこれを EIO 相当として扱う。実用的にはこの状況は「daemon が死んだので cat も失敗」で許容範囲。

- `s3fs_ebpf_admit_shutdown` から `s3fs_ebpf_upcall_shutdown` を呼ぶ形で lifecycle 統合
- daemon 再起動時に stale な pending が残らない

### 10.6 実測 timing

| ケース | 経路 | 実測 time cat |
|---|---|---|
| prewarm 済 hello.txt (2 回目以降) | B: admit hit → GUP | ~0.001s |
| unadmitted lazy.txt (初回) | C: miss → daemon Get → admit → retry → GUP | ~30ms (MinIO on localhost) |
| unadmitted lazy.txt (2 回目) | B: admit hit → GUP | ~0.001s |

C 初回に load される 30ms のほぼ全部は S3 Get の RTT。upcall + admit + signal のコード自体のオーバーヘッドは実質ゼロに近い (dmesg / daemon log の flush 時間で見えない)。
