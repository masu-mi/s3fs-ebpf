# Week 4 (c) 実装ノート — write-back to S3

Week 4 (a) の read path、(b) の bucket sync が揃った後の write 側。ここまでは `.write` が kmod の kmalloc scratch (`fd->buf`) を書くだけで、S3 に何も伝わらなかった。(c) は close/fsync でその bytes を daemon に流し、daemon が S3 PUT する片方向書き戻し + `.create` で `touch` を通す回。

対応 commit: master ブランチの Week 4 (c) サブトラック。

---

## 1. 何を変えたか

### UAPI (include/s3fs_ebpf_uapi.h)
- 2 つの新 ioctl:
  - `S3FS_IOC_POP_FLUSH` (`_IOR(0xE3, 0x05, struct s3fs_flush_event)`) — daemon が 1 件 dequeue、空なら `-EAGAIN`
  - `S3FS_IOC_READ_DIRTY` (`_IOWR(0xE3, 0x06, struct s3fs_read_dirty)`) — daemon が ino の `fd->buf` を自分の buffer にコピー
- `struct s3fs_flush_event { ino, size, name_len, name[256] }` — 名前も一緒に流す (create された file はまだ ino allocator に居ないので daemon 側で S3Key に翻訳可能に)
- `struct s3fs_read_dirty { ino, uaddr, size, out_bytes }` — READ_DIRTY の double-way struct (in: uaddr/size、out: out_bytes)

### kmod (vfs_ops.c, vfs_ops.h, s3fs_ebpf_dev.c)
- `s3fs_file_data.dirty` bool を追加、`.write` と `.setattr(ATTR_SIZE)` で set
- 新 flush queue: `list_head` + spinlock、`flush_enqueue()` / `flush_dequeue()`
- 新 file fops:
  - `.release`: dirty なら enqueue (dirty flag は enqueue 時 clear)
  - `.fsync`: 同じ (best-effort、blocking は future)
- 新 dir iops `.create`: 
  - kmod-local ino counter `s3fs_local_next_ino` (1,000,000+ から始めて daemon の allocator と衝突回避)
  - `struct s3fs_local_entry` を `s3fs_local_entries` list_head に追加 (別 mutex 下ではなく既存 `s3fs_entries_mtx` を再利用)
  - `iget_locked` で新 inode 作成、`d_instantiate`
  - 即座に空 flush event を enqueue → daemon が 0-byte PUT
- `find_entry` / `emit_one_entry` / `s3fs_ebpf_entries_replace` を local additions も見るように更新
- `local_prune_promoted_locked()`: SYNC で来た dyn entries に name+ino が一致する local を落とす (双方 emit しない)

### daemon (daemon/cmd/s3fs-ebpf/flush_loop.go, sync_entries.go, main.go, s3/client.go)
- `s3.Client.Put(ctx, key, data)` を新設 (minio-go の `PutObject`)、キャッシュ dir があれば invalidate
- `runFlushLoop`: 100ms ticker で `S3FS_IOC_POP_FLUSH` → `S3FS_IOC_READ_DIRTY` → `s3c.Put()` を回す
- flush 成功後に:
  - `registerIno(s3Key, ino)` で ino allocator に固定 → 次 sync round が同じ ino で dyn に promote
  - `progs.AdmitContent(buf)` + `addAdmit(ino, uaddr, len)` で arena admit も **refresh**
- 起動時 `runFlushLoop` を `runSyncLoop` と並行に起動

---

## 2. なぜこの shape か

### close/fsync に絞った理由
Meetup の narrative は「(a) read が速い / (b) 動的に S3 見える / (c) 書ける」の 3 セットで、(c) は書ける事実 + POSIX 的 durability の妥当な境界を示すのが目的。

代替案:
- **`.write` ごとに upcall**: `dd bs=1` みたいなワークロードで 1 秒に数万回 PUT が走る。実装最小だが perf 最悪
- **定期 flush (dirty poll)**: レイテンシが poll 間隔に依存、非同期性が強すぎて POSIX 期待とズレる
- **close/fsync flush** ← これ。dd/cp/echo `>` みたいな最頻ワークロードで write を 1 回に集約、POSIX fsync が実際に durability boundary を意味する

### `touch` を local additions で実装した理由
`.create` は POSIX 上、成功時に新 inode を返す必要がある (open(2) が続くので)。**同期的に daemon PUT を待つ** と .create が高レイテンシになる。

選んだ形: kmod で ino を先に割当 → local additions に登録 → 即成功 return → daemon は非同期 PUT。
- .create は数 μs (memcpy 相当) で return する
- PUT は 100ms 以内 (次の flush loop tick) に飛ぶ
- 「create 直後 sync round」の race は daemon の ino allocator を **flush 完了時に固定** することで解決 (次 sync が同じ ino を割当 → local_prune_promoted が local を落とす)

Trade-off: PUT 失敗時に inode がローカルには残るが S3 に届かない状態が発生する。PoC scope では "エラー log + stale local を残す" で許容。

### 1,000,000+ の ino 範囲を local に確保
kmod-local ino が daemon-assigned ino (100+) と衝突すると `iterate_shared` が同じ ino で 2 entry emit してしまう。境界を明確にするため 1M 以上に確保。実運用でも 100 万を超える objects になった時点で他の限界に先に当たる (arena 16 MiB 制限、single-thread flush など)。

### admit refresh を flush の中でやる
write 後 read の結果が古いままだと write-back が「書けるが読めない」状態になる。修正:
- daemon 側 `handleFlush` が PUT 成功後に `progs.AdmitContent(buf) + addAdmit()` を発火
- kmod 側 `admit_replace` が `invalidate_mapping_pages(inode->i_mapping, 0, -1)` (fix (kmod) commit で入れた仕組み) で古い folio を drop
- 次 `.read_folio` は fresh arena bytes を返す

Path B/C 経路の read はすでに admit を優先するので、write→read の一貫性がゼロ設定で取れる。

### `ino` != `S3Key` の翻訳: flush event に name を載せた
create 直後の flush では ino が `LookupByIno` に無い (まだ sync mirror に居ない)。daemon 側で `event.Name` を S3Key として使う fallback ロジックにした。flat bucket 前提なので basename == S3Key で成立。ネスト bucket 対応は Week 4+ の polish。

---

## 3. 実測: write-back 経路

### overwrite (16B hello.txt → 24B の異なる文字列)
```
$ echo "OVERWRITTEN CONTENT XYZ" | sudo tee /mnt/s3demo/hello.txt >/dev/null
$ cat /mnt/s3demo/hello.txt                # 即座に新 bytes 返る (admit refresh)
OVERWRITTEN CONTENT XYZ
$ docker exec s3fs-ebpf-minio mc cat local/s3fs-test/hello.txt
OVERWRITTEN CONTENT XYZ
```

daemon log:
```
[flush] ino=100 s3://hello.txt put 24 bytes
```

### touch + write
```
$ sudo touch /mnt/s3demo/newone.txt        # 即成功 (~μs)
$ ls /mnt/s3demo/newone.txt                # そこにある
-rw-r--r-- 1 root root 0 ... newone.txt
$ docker exec ...mc cat .../newone.txt     # 0-byte object 存在
$ echo "created here" | sudo tee /mnt/s3demo/newone.txt >/dev/null
$ cat /mnt/s3demo/newone.txt
created here
$ docker exec ...mc cat .../newone.txt
created here
```

daemon log:
```
[flush] ino=1000000 s3://newone.txt put 0 bytes
[flush] ino=1000000 s3://newone.txt put 13 bytes
[sync] delta: 6 → 7 entries          ← 次 sync で dyn に promote (ino 保持)
```

### 1 MiB round-trip (byte-for-byte)
```
$ sudo dd if=/dev/urandom of=/tmp/1M bs=1M count=1
$ sudo dd if=/tmp/1M of=/mnt/s3demo/1m.bin bs=1M count=1 conv=notrunc
$ docker exec ...mc cat .../1m.bin > /tmp/from_s3
$ diff -q /tmp/1M /tmp/from_s3 && echo MATCH        # MATCH
$ diff -q /tmp/1M <(cat /mnt/s3demo/1m.bin) && echo READBACK MATCH  # MATCH
```

dmesg WARN ゼロ、`invalidate_mapping_pages` の trylock 経路も無警告。

---

## 4. 残 follow-up

このセッションでは扱わなかったが、実装中に見えた課題:

1. **`.fsync` は enqueue するだけで blocking しない**
   - `fsync(2)` を発行しても daemon PUT を待たずに return
   - Application が `fsync → assume durable → 電源断` すると失われる
   - 対処: pending completion + signal_admit と同じパターンで .fsync を blocking にする
   - PoC の close/write 系ワークロードでは実質問題にならないが、durability を主張するなら要修正

2. **PUT 失敗時に dirty flag を戻していない**
   - `.release` で dirty=false にして enqueue、daemon PUT 失敗 → S3 に届かないまま dirty flag は false
   - 次の write が無ければ再送されない
   - 対処: daemon が PUT 失敗時に kmod に「flush failed」ioctl を送って dirty 再セット、または fresh flush event を再 enqueue

3. **`.unlink` / `.rename` 未対応**
   - `rm /mnt/s3demo/hello.txt` は EPERM (dir_iops に .unlink 無い)
   - 実運用には要る。UAPI に `S3FS_IOC_PUSH_DELETE`? sync loop から DELETE / rename push?
   - Scope 外だが Week 4 (d) の policy 対象になり得る (「特定 prefix は削除禁止」など)

4. **local additions の永続化**
   - daemon 再起動 → kmod は同じ local additions のまま (dirty が消える) → daemon PUT の後 registerIno で新 mapping → OK
   - kmod 再起動 (rmmod) → local additions 消滅
   - Application が open 中に kmod rmmod されるとファイルが消える。実運用には要防御

5. **1 write per PUT の効率**
   - dd bs=4K count=256 で 1 MiB 書く場合、close 時に 1 回だけ PUT (256 write は fd->buf に集約) — OK
   - しかし複数 open→write→close を繰り返すシナリオでは PUT 数 = close 数
   - 対処: 「N ミリ秒 dirty のまま or M bytes 溜まったら fsync」相当の coalescing。ただし現状の close-driven でも十分に集約されている

6. **arena capacity と admit refresh の関係**
   - PUT ごとに `AdmitContent` で新 arena 領域を確保する → 古い領域は解放されない (arena はモノリシックな mmap で free 概念が薄い)
   - 何度も write すると arena が線形に増える。現状 16 MiB 上限を超える前に arena full で失敗する
   - 対処: admit 抜けた ino の arena 領域を再利用する alloc スキームか、arena reset の周期実行

---

## 5. Meetup narrative への貢献

Before (Week 4 (b) まで):
> `ls /mnt/s3demo` は S3 と同期。`cat` は arena admit 経由で速い。しかし書けない。

After (Week 4 (c)):
> `echo "hello" > /mnt/s3demo/x`, `touch /mnt/s3demo/new`, `dd if=... of=/mnt/s3demo/y` — 全部 S3 に反映される。write 後 read の consistency は arena admit refresh で保たれる。POSIX write の主要ケースが動く FS になった。

Week 4 は (a)(b)(c) で「読める・見える・書ける」が揃った。残るは (d) policy engine と (e) 追加 bench (write throughput 比較を含む)。
