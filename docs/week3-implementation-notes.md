# Week 3 実装ノート — kmod inode ops と daemon prewarm

Week 2 のノート (`week2-implementation-notes.md`) が「BPF/verifier/cilium/ebpf の地雷」なのに対し、Week 3 は **kmod 側で実 VFS ファイルを materialise し、Fast Path Hit を第 1 発目から取れる状態にする** ための実装記録。Linux VFS の inode / dentry / iterate_shared まわりで踏んだ挙動と、daemon 側の prewarm 配線の要点。

---

## 0. スコープと成果

| 項目 | 状態 | 補足 |
|---|---|---|
| kmod が seed 3 オブジェクトを VFS に materialise | ✅ | `ls /mnt/s3demo` に並ぶ |
| lookup + iterate_shared | ✅ | 完全カスタム (`simple_lookup` は使わない) |
| read | ✅ | `cat` が seed content を返す |
| write / truncate / append | ✅ | `echo >`, `echo >>`, `dd`, `>` (truncate) 全て通る |
| S3 write-back | ❌ | in-memory only、Week 4+ |
| ファイル内容の arena バッキング | ❌ | 現在は kmalloc プール、Week 4+ |
| dynamic bucket sync (ioctl) | ❌ | seed table は compile-time 静的、Week 4+ |
| daemon が Fast Path prewarm | ✅ | 起動時に seed 全 ino を admit |
| S3 metadata の real reflection | ✅ | `--s3` で size / mtime を上書き |

---

## 1. 「lookup で materialise」パターン

Week 2 の `s3fs_ebpf_fill_super` は `dir_iops.lookup = simple_lookup` だった。これは常に ENOENT を返すので `ls` は空、`stat X` はエラー。Week 3 で自前 lookup に差し替え:

```c
static struct dentry *s3fs_ebpf_lookup(struct inode *dir,
                                       struct dentry *dentry,
                                       unsigned int flags)
{
    const struct s3fs_entry *e = find_entry(&dentry->d_name);
    struct inode *inode;

    if (!e) {
        d_add(dentry, NULL);   /* negative dentry — キャッシュ */
        return NULL;
    }
    inode = s3fs_ebpf_make_inode(dir->i_sb, e);
    if (!inode)
        return ERR_PTR(-ENOMEM);
    d_add(dentry, inode);
    return NULL;
}
```

**踏んだ点:**

- **`d_add(dentry, NULL)` は必ず呼ぶ** — 呼ばないと VFS が dentry を宙ぶらりんにする。呼ぶと negative dentry がキャッシュされ、同じパスへの再 lookup はカーネル内で即 ENOENT。
- **`s3fs_ebpf_make_inode` は `new_inode()` した瞬間から失敗 branch で `iput()` が必要** — さもなくば inode リーク。lookup が ERR_PTR を返す場合は VFS がハンドルしない。
- **戻り値は `NULL` (成功) か `ERR_PTR(-ERRNO)`**。dentry を返す必要は無い (unfortunately libfs のドキュメントが薄い)。

**参考:** `fs/libfs.c: simple_lookup`, `fs/ramfs/inode.c: ramfs_get_inode`, `Documentation/filesystems/vfs.rst`。

---

## 2. `iterate_shared` の position 管理

Week 2 の dir_fops は `dcache_readdir` を使っていた。dentry cache が空だと (mount 直後 + lookup 前) 何も見えない。自前実装:

```c
static int s3fs_ebpf_iterate_shared(struct file *file, struct dir_context *ctx)
{
    size_t i;

    if (!dir_emit_dots(file, ctx))     /* "." と ".." を出す */
        return 0;

    /* ctx->pos: 0="." 1=".." 2..N+1 = table entries */
    for (i = ctx->pos - 2; i < S3FS_NR_ENTRIES; i++) {
        const struct s3fs_entry *e = &s3fs_entries[i];
        if (!dir_emit(ctx, e->name, strlen(e->name), e->ino, DT_REG))
            break;
        ctx->pos++;
    }
    return 0;
}
```

**踏んだ点:**

- **`dir_emit_dots` は自分で `ctx->pos` を +2 する** — ドキュメントに明記されていないが、`fs/libfs.c` を読むと分かる。手動で +2 すると "." が 2 回出る。
- **`dir_emit` が `false` を返したら break する** — ユーザー空間 buffer が満杯のサイン。次回 `iterate_shared` が同じ `ctx->pos` で呼ばれる。
- **`ctx->pos` を entry 出力後にインクリメント** — 出力する前にインクリメントしてしまうと、次回再開時にそのエントリをスキップする。
- **`DT_REG` / `DT_DIR` を正しく渡す** — ls が `?` タイプになる (unknown) 原因。

---

## 3. 書き換え可能な in-kernel コンテンツ

file_operations に `.read` / `.write` を素直に実装。per-inode で growable buffer を持ち、`inode->i_private` に格納:

```c
struct s3fs_file_data {
    struct mutex lock;
    u8    *buf;
    size_t size;
    size_t cap;
};
```

**踏んだ点:**

- **`i_private` の解放は `.evict_inode`**。`.destroy_inode` は VFS 内部呼び出しで、super_ops で hook するのは `.evict_inode` の方が明快。`truncate_inode_pages_final(&inode->i_data)` → `clear_inode(inode)` → `kfree` の順。
- **`i_size` は 2 通り更新方法がある**: `inode->i_size = ...` と `i_size_write(inode, ...)`。read/write が並行して走る可能性がある実装 (今回) は `i_size_write` を使う (32-bit ホスト対応の atomicity)。
- **mtime の更新は 6.7 で API 分割**: `simple_inode_init_ts` は 6.7+、それ以前は `inode->i_atime = ... = current_time(inode)`。write path でも `inode_set_mtime_to_ts` / `inode_set_ctime_current` を使う (`.mtime` 直代入が deprecate されている)。
- **`krealloc(NULL, ...)` は `kmalloc` と等価** → 初回 write の branch を持たなくて済む。
- **`O_APPEND` は `file->f_flags` を明示的に見る** — VFS 側では `*pos = i_size` に強制しないため、write ハンドラで判定する。

---

## 4. `.setattr` を実装しないと `truncate` が動かない

`echo "brand new" > /mnt/s3demo/world.txt` は open(O_TRUNC) → write の 2 段。O_TRUNC は VFS 内部で `.setattr(iattr with ATTR_SIZE=0)` を呼ぶ。実装が無いと `-EPERM` で返り、write が「まだ元の内容の後ろに追加」してしまう。

**修正:**

```c
static int s3fs_ebpf_file_setattr(struct mnt_idmap *idmap,
                                  struct dentry *dentry, struct iattr *attr)
{
    int err = setattr_prepare(idmap, dentry, attr);
    if (err) return err;
    if (attr->ia_valid & ATTR_SIZE) {
        /* fd->buf を伸縮させて fd->size と i_size を揃える */
    }
    setattr_copy(idmap, inode, attr);
    mark_inode_dirty(inode);
    return 0;
}
```

**踏んだ点:**

- **`setattr_prepare` を必ず先に呼ぶ** — 権限チェック + 保護属性 (immutable など) の確認をやってくれる。
- **`ATTR_SIZE` 判定を忘れると `chmod` / `chown` が反映されない** — `setattr_copy` を最後に呼ばないと VFS の inode に反映されない。
- **`mnt_idmap` 引数は 6.3 で追加された**。6.3 未満をサポートする場合は `#if LINUX_VERSION_CODE` で分岐が必要 (今のところ 6.12+ 前提なので無視)。

---

## 5. daemon 側の seed テーブルミラー

kmod の `s3fs_entries[]` (compile-time) と daemon の Go 側 seed を独立に持つ。将来的には ioctl 経由で kmod が渡す形にしたいが今週は文字列一致で運用。

`daemon/internal/ebpf/seed.go`:

```go
var Seeds = []SeedEntry{
    {Ino: 100, Name: "hello.txt",           S3Key: "hello.txt",
     Mode: 0o100644, Fallback: 17},
    ...
}
```

**規約:**

- `Ino` は kmod のテーブルと一致必須 (kmod が inode->i_ino に代入する値)
- `Name` は kmod の dentry 名 (Fast Path 側は使わないが人間可読ログ用)
- `S3Key` は S3 側のオブジェクトキー。kmod の Name とは異なる (`models_weights.bin` ↔ `models/weights.bin` のような alias)
- `Fallback` は S3 が使えない場合の size (kmod の compile-time seed size と一致)

Week 4 で kmod → daemon の動的同期チャンネル (ioctl / netlink / debugfs) を張れば、この二重定義は解消できる。

---

## 6. Prewarm フロー

```
fastpath 起動
    ↓
progs, _ := ebpf.Load()             // arena mmap + BPF load
    ↓
for _, seed := range Seeds:
    if s3c != nil:
        meta = s3c.Stat(seed.S3Key)  // real S3 metadata
        stub = InodeStub{seed.Ino, meta.Size, meta.LastModified, seed.Mode}
    else:
        stub = InodeStub{seed.Ino, seed.Fallback, now, seed.Mode}
    progs.WarmCache(stub)            // arena に write + inode_index に install
    ↓
fentry attach
    ↓
ringbuf consumer loop
```

**踏んだ点:**

- **prewarm が fentry attach より先** — 順序が逆だと prewarm 中に fentry が走って余分な miss を数える。
- **S3 stat がタイムアウトした場合の fallback** — ネット切れや MinIO 未起動で prewarm が丸ごと止まらないよう、seed 単位で catch して fallback size を使う。
- **arena は 4 KiB (1 page)** — 3 seed entries なので余裕。溢れたら `ErrCacheFull`。

---

## 7. モジュール unload の落とし穴

`sudo rmmod s3fs_kfunc` は以下の順序で失敗する:

1. マウントされている間 → `Module s3fs_kfunc is in use`  
2. マウント解除しても直後は BPF プログラムがまだ arena map を参照 → `is in use`
3. daemon プロセスを SIGINT → arena fd がクローズ → rmmod OK

**手順:**

```bash
sudo pkill -INT -f 's3fs-ebpf fastpath'   # daemon 停止
sudo umount /mnt/s3demo
sudo rmmod s3fs_kfunc
```

順序を変えると `-EBUSY` が返る。CI で自動化する場合は各ステップ後に `lsof` / `fuser -m` で参照カウントが 0 になるのを待つ。

---

## 8. Meetup 主張とのマッピング

Week 3 が終わって、Meetup スライドで示せる 1 分デモは以下:

```text
# ターミナル 1: kmod + mount + fastpath 起動
sudo insmod kmod/s3fs_kfunc.ko
sudo mount -t s3fs_ebpf none /mnt/s3demo
sudo ./bin/s3fs-ebpf fastpath --s3 --sample-every=1s
    [prewarm] ino=100 ... src=s3://hello.txt → offset=0
    [prewarm] ino=102 ... src=s3://models/weights.bin → offset=32
    [prewarm] ino=101 ... src=s3://world.txt → offset=64

# ターミナル 2
ls /mnt/s3demo             # 3 files
cat /mnt/s3demo/hello.txt  # returns content
for i in 1 2 3; do stat /mnt/s3demo/*.txt > /dev/null; done

# ターミナル 1 に戻る
    [counters/sample] fast_hits=9 slow_misses=0
```

「daemon が起動時に S3 metadata を bpf_arena に注ぐ → 以後 stat は IPC ゼロで返る」を 30 秒で見せられる。

---

## 9. 残タスク

- **Week 4 (a)**: file 内容の arena バッキング。kmod が KFunc 経由で arena の kernel-VM base を取得して読み書きに使う。ここが「Fast Path が実データを返す」の最終形。
- **Week 4 (b)**: write-back to S3。dirty page 追跡と async upload。
- **Week 4 (c)**: Lua/WASM policy engine。cache TTL / prefetch / eviction のポリシー分離。
- **Week 4 (d)**: benchmark。`fio filestat` と `time stat -c '%s' ...` で s3fs-fuse / Mountpoint との比較を取る。
