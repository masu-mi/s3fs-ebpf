# Week 4 (b) 実装ノート — dynamic bucket sync

Week 4 (a) までは kmod の entry table (`s3fs_entries[]`) と daemon の mirror (`daemon/internal/ebpf/entries.go`) が **どちらも compile-time で固定** で、S3 バケットの内容と一致しないと `ls /mnt/s3demo` が嘘をつく状態だった。Week 4 (b) は「S3 の実状態を daemon が List → kmod に push」の片方向ループを入れて、`ls` / `stat` が常に S3 と一致するようにする回。

対応 commit: master ブランチの Week 4 (b) サブトラック。

---

## 1. 何を変えたか

### UAPI (include/s3fs_ebpf_uapi.h)
- 新 ioctl `S3FS_IOC_SYNC_ENTRIES` を追加 (`_IOW(0xE3, 0x04, struct s3fs_sync_entries)`)
- `struct s3fs_sync_entry { ino, size, mode, name_len, name[256] }` — 1 エントリぶん
- `struct s3fs_sync_entries { count, entries }` — array header

### kmod (kmod/vfs_ops.c + s3fs_ebpf_dev.c)
- `s3fs_entries[]` (const 配列) を `s3fs_bootstrap_entries[]` に rename、`s3fs_dyn_entries` (kmalloc 可変配列) を追加
- 「daemon 未 sync なら bootstrap、sync 済なら dynamic」の 2 レイヤ構造
- `s3fs_ebpf_entries_replace()` / `s3fs_ebpf_entries_shutdown()` を publish
- `find_entry` / `iterate_shared` は snapshot コピー方式 (`struct s3fs_entry_snap`) で lock 下読み → コピー → unlock 後に dir_emit / d_add。`dir_emit` は page fault で sleep する可能性があるので rwlock 不可、mutex ベースにした。
- replace 時に `shrink_dcache_sb(sb)` で dentry cache を flush。細粒度 diff は Week 4+ の polish に回す。
- `pr_debug()` にして 3s 周期の sync ログが dmesg を埋めないように

### daemon (daemon/cmd/s3fs-ebpf/sync_entries.go + main.go)
- `fpebpf.Entries` (`var`) を `Snapshot()` / `Replace()` / `LookupByIno()` の accessor 経由に refactor、内部は mutex + slice
- `assignIno(s3Key)` — プロセス内 first-seen で 100+ を割当。bootstrap 5 エントリ (`hello.txt` ino=100 など) は seed して S3 に同名 key があれば ino が一致するように
- `syncEntriesOnce(ctx, dev, s3c)` — List → build entries → assign inos → SYNC_ENTRIES ioctl → mirror 更新
- `--sync-every=<duration>` flag (default 30s、0 で無効) + `runSyncLoop` で periodic 発火
- fastpath 起動時に initial sync → prewarm → runReadMissLoop → periodic sync loop の順で立ち上げる
- 前後 delta 検知して「6 → 7 entries」だけ log、no-op は silent

---

## 2. なぜこの shape か

### 「replace-all」で「delta append」ではない
kmod 側の admit table と同じパターンを踏襲。理由は同じ:
- 「消えた entry」の扱いを daemon 側で吸収できる (delta path なら kmod で判定要)
- ioctl の semantics が単純 (「今の bucket 全部」以外の状態がない)
- sync 頻度が低い (数秒~数十秒に 1 回) ので replace のコストは無視できる

trade-off: 大バケット (数万 objects) になると 1 sync で数 MB の ioctl になる。Week 4 scope 外だが、その世代になったら `S3FS_IOC_APPEND_ENTRIES` を切って delta にする道は開いてある。

### mutex を選んだ理由 (rwlock 不可)
最初は rwlock で試したが、`iterate_shared` が呼ぶ `dir_emit` → `filldir` → `copy_to_user` が **page fault で sleep する可能性がある**。rwlock の read_lock は preemption を止めるので sleep 禁止 → BUG。

回避策の候補:
1. RCU: reader は rcu_read_lock で OK だが同じく sleep 禁止 (SRCU は sleep OK だが overhead)
2. rwsem: down_read が sleepable。読み側 concurrent 可
3. mutex: 単純だが単一 reader

PoC scale (数〜数百 entries、周期 3s〜30s) では **mutex が最も単純で正しい**。concurrent readers が問題になれば rwsem に上げる。

### `size` field を SYNC_ENTRIES に持たせた理由
最初は ino/mode/name だけで実装したが、smoke test で新規 sync 済 file の `cat` が空 body を返した。原因:
- SYNC で新 inode が作られる
- daemon がまだ admit していない (prewarm は起動時のみ)
- kmod の `s3fs_ebpf_make_inode` は「admit が無く initial_content も NULL」だと `i_size = 0` にする
- `generic_file_read_iter` は i_size=0 の file を short-circuit で return
- → `.read_folio` が呼ばれず Path C の upcall も走らない → cat 空

修正: SYNC_ENTRIES に S3 stat の size を含める。kmod の make_inode 優先順位は
1. admit hit なら admit size (arena の実バイト数)
2. sync entry の size (S3 authoritative)
3. `fd->size` (bootstrap の compile-time placeholder)

これで `ls` の size 表示が正しくなる + i_size > 0 なので .read_folio が呼ばれる → Path C が発火する。

### bootstrap entry を残した理由
kmod がロード直後 (daemon 未接続) でも `ls /mnt/s3demo` が空にならないように。同じく daemon が exit すると `count=0` の SYNC_ENTRIES を送って bootstrap に戻す (planned; 現状は daemon exit で dyn 残る、rmmod で shutdown 経由 flush)。

「S3 と同期していない状態」が観測可能なままなのは PoC としては便利。実運用向けに「daemon 未接続なら EIO」にしたければ bootstrap を消すだけで対応可。

### ino allocator: プロセス内 in-memory
`map[S3Key]ino` を daemon プロセス内で持つ。同じ S3 key は同じ ino に落ちるが、**daemon 再起動で ino がリセット** される。umount + remount でも同じ。file lock を pid + ino で識別する application があるとこれで壊れる可能性があるが、PoC スケールでは許容。

将来的な選択肢:
- ファイル永続化 (`/var/lib/s3fs-ebpf/inos.json`) → hardlink 相当の安定性、atomic write + lock が要る
- hash(S3Key) & 0x7fffffff → 永続だが collision 検出不能 (kmod 側で対処不可なので非採用)

---

## 3. 実測: put/rm の反映時間

`--sync-every=3s` で:

```
[sync] initial: 6 entries          ← 起動時
                                    ← delta test.txt を mc put
[sync] delta: 6 → 7 entries        ← ~3s 以内で反映
                                    ← delta test.txt を mc rm  
[sync] delta: 7 → 6 entries        ← ~3s 以内で反映
```

`ls /mnt/s3demo` の表示、`stat` の size、`cat` の内容 (Path C 経由) すべて sync 済状態を反映する。dmesg WARN ゼロ。

---

## 4. 残 follow-up

このセッションでは扱わなかったが、実装中に見えた課題:

1. **daemon exit 時の SYNC clear が未実装**
   - 現状 daemon が SIGINT で落ちても kmod の dyn entries は残る (rmmod で `s3fs_ebpf_entries_shutdown` が発火するまで)
   - defer で count=0 SYNC を送るのが素直。`clearAdmits` と同じパターン

2. **admit の cleanup が sync と非同期**
   - S3 rm → sync で kmod entry から消える → dentry も消える (shrink_dcache) → 新規 lookup は ENOENT
   - しかし admit table の該当 ino は残る (harmless、参照する dentry が無い)
   - 長時間動かすと admit table が肥大化する。実運用前に「sync の delta で消えた ino は admit_replace snapshot からも除外」する連動が必要

3. **prewarm が initial sync のみ**
   - Path C 経由なら新 entry の cat も動くが 30ms のレイテンシが乗る
   - 大量の新 entry が一気に増えた場合、初回アクセスが全部 Path C で S3 GET が集中する
   - runSyncLoop で「新 entry には prewarm を回す」オプションを足せる (現在は on-demand)

4. **`models/weights.bin` の basename 衝突**
   - 現状 `path.Base(S3Key)` を name にしているので、同名の basename を持つ複数の prefix (`a/foo.txt` と `b/foo.txt`) があると同じ name で lookup が衝突する
   - PoC 用の flat bucket では問題なし。ディレクトリ対応 (Week 4+) で本格対処

5. **BPF prog 側の inode_index (fpebpf.WarmCache) と sync の連携が薄い**
   - fentry の fast_hits 用 stub は prewarm で書いてるが、sync-only の新 entry では書かれない
   - 現状 fast_hits の対象は prewarm 済 entry だけ。sync だけで新 entry を宣言しても fast path はまだ走らない (Path C が admit を発火してから fast path に入る)
   - runSyncLoop から `progs.WarmCache` を呼ぶのが素直だが、tag を「メタデータのみ warm」と「content admit」で分ける整理が要る

---

## 5. Meetup narrative への貢献

Before (Week 4 (a) C まで):
> demo bucket の 5 objects が hardcoded で、daemon と kmod の両方に同じ list を仕込む。運用向けとは呼べない

After (Week 4 (b)):
> daemon が S3 bucket を List → kmod に push を 30s (or 任意) 周期で回す。`mc put` した object が数秒後に `ls /mnt/s3demo` に出て `cat` で読める。`mc rm` すると消える。実運用の FS 挙動に一歩近づいた

Week 4 の 3 段 (a/b/c) のうち read 系が (a) + (b) で揃った形。残るは write-back (c) と policy engine (d)、benchmark 追加 (e)。
