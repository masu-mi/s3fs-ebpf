# G-01 subdir 対応 + F-02 basename 衝突: 実装計画

**Status**: 設計のみ。実装は次スプリント。
**依存関係**: F-02 (basename 衝突) は G-01 が landed 済であることを前提にする。同じ commit で 2 個一緒に解決するのが自然。
**背景**: `docs/journal/backlog-archive.md` の G-01 / F-02、および `docs/journal/week4b-implementation-notes.md §4-4`。

## 1. 現状 (2026-09-29 時点)

- kmod entry table は **フラット** (root=ino 1 直下に `s3fs_dyn_entries[]` を並べるだけ)。`iterate_shared` は "現 dyn の全 entry" を root にぶら下げる。
- S3 key `"a/foo.txt"` はそのまま `name="a/foo.txt"` として登録される。`ls /mnt/s3demo` は `a/foo.txt` を単一ファイル名として見せる (`/` を含んだファイル名は POSIX 的に illegal だが、Linux VFS は kmod が dir_emit する内容をそのまま見せるので "見える" 状態)。
- 実運用では daemon 側 `buildEntriesFromS3` が `path.Base(o.Key)` で最終セグメントだけ取り出すため、`a/foo.txt` と `b/foo.txt` はどちらも `name="foo.txt"` で衝突する — **F-02 の症状**。ino allocator が二人分別 ino を割り当てるので kmod 側では 2 entry として並び、last-write-wins な iterate_shared 順で見える。

## 2. ゴール

**最小要件** (今回スコープ):

1. `ls /mnt/s3demo` が root 直下のみ表示。
2. `ls /mnt/s3demo/sub1` が sub1/ 配下のみ表示。
3. `cat /mnt/s3demo/sub1/foo.txt` が s3://bucket/sub1/foo.txt を PUT/GET。
4. basename 同一 (sub1/foo.txt と sub2/foo.txt) が別 entry として共存できる。
5. subdir は S3 側の key prefix から自動導出 — mkdir/rmdir は **out of scope** (今回 PoC は read-write するが階層自体は S3 by-side が権限を持つ)。

**将来スコープ**: mkdir/rmdir/rename across dirs、`--key-prefix` との相互作用、depth 制限、`readdirplus`。

## 3. 変更ポイント

### 3.1 UAPI (include/s3fs_ebpf_uapi.h)

```diff
 struct s3fs_sync_entry {
     __u64 ino;
+    __u64 parent_ino;  /* new — 0 == root == ino 1 (back-compat) */
     __u64 size;
     __u32 mode;        /* S_IFDIR possible now */
     __u32 name_len;
     char  name[S3FS_SYNC_NAME_MAX + 1];
 };
```

**Back-compat 戦略**: 旧 daemon は `parent_ino=0` を送るので kmod 側は 0 を「root ino (=1) 相当」に読み替える。新 daemon は明示的に parent を書く。

### 3.2 kmod (kmod/vfs_ops.c)

1. `struct s3fs_entry_dyn` に `parent_ino` 追加、`s3fs_entries_replace` は payload をそのまま格納。
2. `s3fs_ebpf_iterate_shared`: 現在は "dyn 全部 + local 全部" を emit。**変更**: 呼び出し元の dir inode ino を parent フィルタとして受け取り、`e->parent_ino == dir->i_ino` (0 は root=1 として扱う) のみ emit。
3. `s3fs_ebpf_lookup`: `find_entry` を parent-aware 化 (`resolve_entry_by_name_in(parent_ino, name, out)`)。同一 basename が別 parent なら別 entry として解決。
4. `s3fs_ebpf_make_inode`: `S_ISDIR(e->mode)` なら `inode->i_op = &s3fs_ebpf_dir_iops` / `inode->i_fop = &simple_dir_operations` (もしくは同名の `s3fs_ebpf_dir_fops`) に dispatch。regular は既存パス。
5. Local entries (`s3fs_local_entry`): `parent_ino` 追加。`.create` は呼び出し元 dir から parent を取る。**mkdir はまだ実装しない** — subdir は sync 経由で外から入る。
6. Attr overrides / admit hash など ino-based の table は変更不要 (ino が unique なら subdir でも壊れない)。

### 3.3 daemon (daemon/cmd/s3fs-ebpf)

1. `buildEntriesFromS3` を書き直し:
   - S3 List の結果を key 文字列で受け取り、`strings.Split(key, "/")` で階層を分解。
   - 各中間ディレクトリに **synthetic dir entry** を作る (parent_ino, ino, name, mode=S_IFDIR|0755)。dir ino は `assignIno("dir:"+fullpath)` などプレフィクス付きキーで割り当てて file ino と衝突しないようにする。
   - Leaf file entry は `parent_ino = 該当 dir の ino`、`name = path.Base(key)` で登録。
2. `entriesToSyncPayload` は新 UAPI struct に parent_ino をセット。
3. sync entry の順序: dir が leaf より先に emit されるように `sort.Slice` で `parent_ino` 昇順 → `name` 昇順。kmod 側 hash 化した parent 参照でも順序不変性が要る場合は明示 topological sort。
4. flush loop (write path): `.rename` / `.unlink` / `.release` の event は既に basename しか持たないので、full path を復元するために **daemon side で ino → full-path の逆引き map** を持つ。`registerIno` は現在 basename ↔ ino だけ; これに parent_ino も食わせる。
5. Prewarm hook (F-01): dir entry は content admit しない (skip on S_ISDIR)。

### 3.4 F-02 basename 衝突

G-01 の parent_ino が入れば自動解決。基本の invariant は `(parent_ino, name)` が unique key。sync 時に daemon が `(parent, name)` map を dup check → 重複時は warning + 最新のみ採用 (もしくは ENAMETOOLONG-風の別 error) にする。

## 4. 移行 (backward compat) 手順

1. UAPI struct を拡張したら kmod ABI が変わる — insmod 側と userspace daemon がバージョン揃わないと struct サイズミスマッチ。`S3FS_UAPI_VERSION` 定数を bumped し、SYNC_ENTRIES ioctl の struct にバージョン field を先頭に載せて kmod が rejection できるようにする。
2. 既存 flat-bucket bench (hello.txt など root 直下のみ) は parent_ino=0 のまま送れば従来通り動くべき。
3. bench-latency / bench-write / bench-hitrate は subdir を触らないので変更不要。bench-3way は fuse-cache mount と `find` 系を追加した場合のみ影響。

## 5. リスク・注意点

- **iterate_shared のフィルタ**: `ls -R` のような再帰的な walker が root だけを見て "無限ディレクトリツリー" に感じないよう、dir の st_size / nlink は正しく計算する必要がある。VFS が nlink==2 の empty dir を "空" と判定して subdir を traverse しないケースあり。
- **dcache**: dir と file が同じ basename を持つケース (`foo/` と `foo`) は S3 由来では稀だが、shrink_dcache_sb で resolve 順が変わりうる。同一 (parent_ino, name) を dyn table 内で dup チェックする。
- **既存 test**: `TestPlanReplaceAdmit_*` などは entry.Ino だけ触っているので影響なし。`admit_arena_test.go` の N=64 stress も subdir 由来の parent 依存はない。
- **kmod build**: 現状の `.setattr` は inode 単位で override するので S_IFDIR inode でも `chmod 755` は動くはず。UAPI 拡張の際は `struct s3fs_sync_entry` の layout が壊れるので kmod と daemon を同時に rebuild する運用にする (README にも一行足す)。

## 6. 実装順序 (推奨)

1. UAPI 拡張 (parent_ino 追加) + version bump — 1 commit
2. kmod side の entry table 拡張 + iterate_shared filter + lookup 変更 — 1 commit (回帰テスト: bootstrap flat mount で hello.txt が引き続き見える)
3. daemon side sync entry で parent_ino を埋める + dir synthesis — 1 commit (smoke: `sub1/foo.txt` を bucket に置いて mount 経由で cat 成功、ls /mnt/s3demo/sub1 で foo.txt のみ表示)
4. write path (flush event の full-path 復元) — 1 commit
5. basename 衝突テスト (sub1/foo.txt と sub2/foo.txt を共存確認) — F-02 として cover

## 7. 見送った代替案

- **daemon 側で `sub/foo.txt` を basename に含める (現状の "見た目 subdir" を保持)**: dir_iops が使えないので `ls` の表示上でしか subdir っぽくならず、`cat /mnt/s3demo/sub/foo.txt` は EINVAL。fmount の便利さがない。
- **s3fs-fuse の `-o use_rrs` と同等の flat-bucket mode を daemon flag に**: 既存 flow 完全温存だが subdir 要件を全く満たさない。
