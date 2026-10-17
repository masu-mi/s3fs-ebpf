# kmod pcache invariants (kmod/vfs_ops.c)

`vfs_ops.c` を読み直さずに済ませるための要約。関数名は `scripts/fn.sh <name>` で引ける。コードを変えてここと食い違ったら、このメモも更新すること。

## ロック順 (外 → 内)
- `fd->lock` (mutex, per-inode `s3fs_file_data`) → `s3fs_admits_lock` (rwlock)。逆順で取るパスは存在しない (R1 はこれに依存)。
- `s3fs_pending_lock` / `s3fs_miss_lock` / `s3fs_flush_*_lock` / `s3fs_flush_pins_lock` は spinlock で末端 (`flush_pins_lock` は fd->lock 下で取る)。保持中に `fd->lock` や `s3fs_admits_lock` を取らない。
- `admit_evict_ino` は `admit_drop_all_ranges` (admits write_lock) を解放した後に `s3fs_evict_page_cache` → `s3fs_file_data_reset` (fd->lock → admits write_lock) を呼ぶ。

## R1 不変条件
- **ino の PCACHE admit entry が hash にあれば、その範囲の各 chunk は確保済み (`chunks[ci].p != NULL`)。** `populated` は別で、T2 が chunk ごとに見る (部分的にしか埋まっていない chunk も entry の範囲に入りうる)。(AC-45 C1 より前は「`fd->buf != NULL`」。fd の中身は連続した 1 本の buf ではなく 4 MiB の chunk の表になった。)
- 例外: `setattr` の縮小 (truncate) は切った先の chunk を free するので、dirty の間は entry の下に chunk がないことがある。chunk へのアクセスはすべて `p != NULL` を確認するので、読むと upcall に戻るだけで壊れない。
- 保守する側: `s3fs_ebpf_populate_pages` は copy と chunk の `populated` フラグのセットに続けて、`fd->lock` を保持したまま `admit_insert_pcache` を呼ぶ。`s3fs_file_data_reset` は `fd->lock` 下で残っている PCACHE entry を再 evict してから chunk を free する (drop と reset の間に populate が挟まるケースへの対策)。
- S1: R1 が成り立つのは `fd->lock` 下だけ。`fc_pcache_check` (lockless) から `s3fs_folio_fill_from_kmalloc` が lock を取るまでに `s3fs_admit_gen` が進んでいたら、entry を read_lock で再検証する。`gen == 0` は検証なしの fallback を意味する。
- T2: `gen != 0` のときも `gen == 0` のときも、確保済みの chunk に `populated` フラグが立っていることを要求する (未取得の chunk の 0 を返さないため)。フラグは chunk ごとにあるので、256 MiB を超えるファイルも同じ判定になる (旧: `ci >= 64` は R1 だけに頼っていた)。dirty/flushing のときは見ない (T1): 確保されていない chunk は hole で、0 として読める。

## pending のキー
- `s3fs_pending` は `(ino, chunk_offset)` 単位で、chunk は 4 MiB (`S3FS_PCACHE_CHUNK_SHIFT=22`)。hash key は `ino` のみ (P1: 同じ ino の全チャンクが同じ bucket に入る)。照合は `p->ino` と `p->chunk_offset` の両方で行うので、チャンクごとの完了待ちは独立 (U13)。
- refcount は 2 本立て (H-06)。作成時は refs=2 (hash の分 + caller の分)。SIGNAL_ADMIT / shutdown が落とすのは hash の分だけで、waiter は `p->err` を読んだ後に自分の分を `pending_put` で落とす。
- `pending_exists(ino)` は eviction 側のガードで、pending 中の ino を evict 候補から外す。key=`ino` の bucket を走査し、どのチャンクの pending でも見つける。hash key を変えるときはこの前提を崩さないこと。

## 成功 signal の契約 (AC-45 C2)
- `S3FS_IOC_SIGNAL_ADMIT` に `S3FS_SIGNAL_VERIFY_POPULATED` を付けて `err == 0` で呼ぶと、`s3fs_ebpf_signal_admit` は pending (ino, chunk_offset) を見つけたあと、その chunk が確保済みかつ `populated` であることを `fd->lock` 下で確かめる。なければ **pending を完了させず `-EAGAIN`** を返す。pcache モードの daemon (`handleReadMiss`) だけがこのフラグを付ける (arena の admit は chunk を持たない)。
- 理由: 同じ ino の miss を複数の handler が同時に処理し、先に終わった handler が pending を完了させたあとで、遅れて動いている handler は pending に守られないまま evict される。その handler が「成功」を signal すると、reader は何もない状態で起こされ、retry (`S3FS_UPCALL_MAX_RETRIES=3`) を古い signal で使い切って gen=0 フォールバック (chunk なし) で EIO になる。C2 で evict が頻繁になって表に出た。
- 成立条件: reader は **pending を作る前に** `buf_readers++` する (`s3fs_ebpf_read_folio`)。pending があれば reader は登録済みなので、signal の時点で chunk が populated なら、reader が終わるまで reset に解放されない。
- daemon は `-EAGAIN` なら populate をやり直す (最大 `signalVerifyRetries=3`)。使い切ったら検証なしで signal し、reader 側の retry / フォールバックに任せる。

- chunk load (AC-52 手順 3、daemon の `admit_chunkload.go`): pcache モードで大きいファイルが全体 admit になるとき、daemon は 4 MiB ごとの range GET (4 並列) を、miss した chunk から順に **chunk ごとに populate して、その chunk の pending に signal** する。契約は変わらない: signal は chunk ごとに `S3FS_SIGNAL_VERIFY_POPULATED` 付きで、pending のない chunk は ENOENT (無視)、EAGAIN なら再 populate (最大 `signalVerifyRetries`)。reader は先の chunk で miss して別の handler を立てるので、daemon は ino ごとに進行中の load を登録し、後から来た handler はその chunk の完了を待って signal だけを行う (GET を二重に出さない)。load が失敗したら待っている handler に -EIO を返す。先の chunk は pin されていないので shed されうるが、そのときの reader は通常の miss として次の load をやり直す。

## populate 後の capacity 強制 (AC-45 C2)
- pcache の miss (populate ioctl + signal) は kmod の `admit_replace` を呼ばないので、capacity の evict は eval / sync が来るまで動かなかった。daemon は `--cache-capacity` が効いているとき (kmod パラメータ `capacity_bytes` > 0) に限り、**最後の populate のあと、signal の前に** `refreshAdmits` (= `admit_replace` の再実行) を行う (`enforceCapacityAfterPopulate`)。signal の前なので、この miss の chunk は pending に守られる。同時に来た miss は 1 回の追加の pass にまとめる (single-flight)。

## chunk の pin (U8、AC-45 C3 で chunk 単位になった)
- `fd->chunks[ci].readers` は `fd->lock` で保護する (`fd->buf_readers` は廃止)。`s3fs_ebpf_read_folio` は upcall 経路 (U8) に入るとき、**pending を作る前に**、pos を含む chunk を 1 回だけ pin し (`s3fs_fd_chunk_pin`、table の要素がなければ作る。失敗したら pin なしで続ける)、`done:` で unpin する。pcache ヒット経路の pin (U10) は A/B で不要と確認して削除済み (hot path で fd->lock を取らない)。
- 意味: 「この reader は retry や data-based fill で、この chunk を後から読む可能性がある」。pin された chunk は reset / shed が free しない。pending があれば reader は pin 済み (前述の成功 signal の契約はこれに依存)。`s3fs_fd_chunk_free` は `readers` を保つので、pin / unpin の数はずれない。
- data-based fill: 起こされたあと (`upcall_retries > 0`)、entry がなくても chunk が populated ならそこから folio を埋める (`s3fs_folio_fill_from_kmalloc` の `quiet` 版。gen0 の失敗カウンタは増やさない)。capacity の pass が entry だけを先に落としても、無駄な upcall を繰り返さない。埋めたときは、その chunk の PCACHE entry を入れ直す (entry がないまま page cache に folio が残ると、capacity の計算にも evict にも入らない)。
- upcall のリトライ上限は `S3FS_UPCALL_MAX_RETRIES=3`。timeout (U11) もこの回数に含める。使い切ったら gen=0 fallback に進む。

## reset の条件 (`s3fs_file_data_reset`)
- 呼ばれる経路: `admit_evict_ino` の中で、`admit_drop_all_ranges` が PCACHE entry を落としたと報告したとき (S4: 判定は write_lock 内で atomic に行う) だけ。
- `fd->lock` 下で `!dirty && !flushing` のとき、この ino の PCACHE entry を全部落とし (1 件でも消したら `s3fs_admit_gen++`)、**`readers == 0` の chunk だけを free** する (C3)。pin された chunk が 1 つでも残れば `fd->size` と table を保ち (`s3fs_reset_skipped++`)、1 つも残らなければ table を free して `size = 0`。
- AC-41: 容量判定 (`live_bytes`) は arena entry のサイズ + pcache ino ごとの `fd->cap` (`s3fs_ino_buf_bytes`、ino 単位で 1 回)。`fd->cap` は確保済み chunk の `cap` の合計。admit 済み chunk のサイズではない。evict 後は before/after の `fd->cap` 差分を解放量として引く (skip_reset なら 0)。
- AC-45 (C1): fd の中身は 4 MiB の chunk の表 (`fd->chunks[]`、`S3FS_CHUNK_SIZE`)。populate は、足りない chunk を `min(4 MiB, max(終端, i_size) - chunk 先頭)` ちょうどで確保する (AC-41: 46 B のファイルは 46 B。AC-46 の倍々と全体の memcpy はなくなり、触った chunk の分だけ確保する)。write は chunk の中で倍々 (最小 4 KiB、上限 4 MiB) に伸ばす (非 pcache の dirty ファイルは最大で 2 倍まで確保する)。確保済み chunk のうち fd->size 以降のバイトは 0 (縮小は境界 chunk の後半を 0 にして、後ろの chunk を free)。setattr の拡張は chunk を確保しない (hole、dirty の間は 0 として読める)。`populated` は signal_flush でも確保済み chunk にだけ立つ (hole は次の read で取り直す)。chunk の上限は `S3FS_MAX_CHUNKS` (1 TiB)、超えたら -EFBIG。
- M1/M2: `fd->cap` を変える箇所は `s3fs_fd_chunk_ensure` と `s3fs_fd_chunk_free` の 2 つだけで、そこで `s3fs_fd_buf_bytes` も同じだけ増減する (file_data_new / write / populate / reset / free / truncate はすべてこの 2 つを経由する)。`s3fs_ebpf_fd_buf_audit` (STATS[12/13]) が全 inode を走査した合計と突き合わせる (I/O が止まっていれば gauge == walk)。走査は `s_inode_list_lock` → `i_lock` だけを取り、admits_lock とは入れ子にしない。
- dirty / flushing のときは何もしない。pin で残った chunk は、reader が unpin したあと、次に evict されたときに回収される (entry を入れ直しているので capacity の計算に入る)。inode の `i_size` はクリアしない (Q1)。
- AC-45 C3 (shed): capacity の pass は、miss が in-flight (`pending_exists`) の ino を丸ごとは evict しない (待っている reader の chunk と entry を守る)。代わりに `s3fs_ino_shed_unpinned` で、`readers == 0` の chunk を、重なる PCACHE entry を先に落として free し、その範囲の page cache を落とす。capacity より大きいファイルを順に読んでも、読み終わった chunk が溜まらない。daemon は、capacity より大きいファイルを全体 admit (pattern-aware promote) にしない (`fullAdmitExceedsCapacity`、全体を populate してから evict しても意味がない)。

## 既存ファイルへの最初の変更 (AC-50)
- `open(2)` は中身を読み込まず、flush は fd の全体を PUT する。だから **clean → dirty の最初の変更** (write / truncate) の前に、変更で残る部分を fd に読み込む (`s3fs_fd_load_for_modify`)。そうしないと、途中への上書きはオブジェクトを書いた範囲まで切り詰め、縮小は 0 を作り、`: > f` は S3 に届かなかった。
- 読み込む範囲: `[0, min(need_end, i_size))` のうち populated でない chunk。ただし呼び出し側が全部上書きする chunk (write の範囲に丸ごと入る chunk) は読まない。write は `need_end = i_size`、縮小は新しいサイズ、拡張は元のサイズ、0 への truncate は何も読まない。
- 読み込みの upcall は `S3FS_MISS_FLAG_FOR_WRITE` 付きの miss event。daemon は arena モードでも POPULATE_PAGES で chunk を fd に入れ、`S3FS_SIGNAL_VERIFY_POPULATED` 付きで signal する。待つ側は read_folio と同じく、**先に chunk を pin してから pending を作る**。daemon がいない、または chunk を取れないときは **-EIO で変更を断る** (取れない部分を残したまま flush すると、オブジェクトを壊すため)。
- clean な fd の論理サイズは `i_size` (S3 のサイズ)。`fd->size` は取得済みの範囲しか表さない。最初の変更で `fd->size = max(fd->size, i_size)` にしてから適用する。setattr は、サイズが変わるとき (`new_size != 現在のサイズ`) は必ず dirty にする (0 への truncate も PUT される)。
- すでに dirty / flushing の fd は内容を持っているので、読み込まない。
- **pin は書き込みが dirty にするまで持ち続ける**: `s3fs_fd_load_for_modify` は必要な chunk を最初にまとめて pin し (`struct s3fs_mpin`)、読み込みが終わっても unpin せずに返す。write / setattr は `fd->lock` 下で `s3fs_fd_modify_ready_locked` を確かめ、通ったらその lock を離さずに `s3fs_mpin_release_locked` → 適用 → dirty。足りなければ lock を離して読み込みからやり直す (最大 `S3FS_LOAD_MAX_TRIES`)。pin が無いと shed / reset が chunk を free し、穴が 0 のまま flush される。capacity より大きいファイルの変更では、必要な chunk を全部持つので一時的に capacity を超える (書き込み用 buffer なので許容)。
- 読み込みの miss event の `Count` は「chunk が i_size まで持つべき長さ」(`min(4 MiB, i_size - chunk 先頭)`)。daemon が持つオブジェクトのサイズ (`Fallback`) は append + flush のあと sync まで古いので、daemon は `Count` が長ければそれに合わせる。短いと chunk が populated にならず、-EIO になる。
- 同時に来た 2 つの write: 後の loader は daemon から -EBUSY (dirty な fd は populate しない) を受けるが、fd がすでに dirty なら 0 を返して続ける (-EIO にしない)。

## flush pin (W1)
- close/fsync の `s3fs_maybe_enqueue_flush` が `flush_seq++` し、`s3fs_flush_pin` で inode を `ihold` する (fd ごとに最大 1 本、`pin_link` が `s3fs_flush_pins` に載っていれば pin 中)。これがないと drop_caches / reclaim で `evict_inode` が PUT 前の `fd->buf` を捨てる。
- `read_dirty` が `flush_read_seq = flush_seq` を記録。`signal_flush` は `err≠0 || flush_read_seq==flush_seq` のとき `flushing=false`、さらに `err==0 && !dirty` なら unpin (`iput` は fd->lock 解放後)。seq 不一致 = 後続 enqueue が未 PUT なので flushing/pin を維持。err≠0 は flushing を下ろすが pin は保持。
- 残った pin (daemon 不在・PUT 失敗・キュー破棄) は `put_super` の `s3fs_flush_pins_release_all` が解放 (pr_warn)。unpin の所有権はリスト所属で決まるので二重 iput しない。
- AC-48: `admit_replace` は i_size を、中身が変わった entry の range 終端 (`offset + size`) まで **伸ばすだけ** で縮めない。dirty/flushing の inode には触らない。entry は range なので `size` を i_size に書いてはいけない (縮むと W2 が誤ったサイズを entry に書き戻す)。S3 側の縮小は `entries_replace` の dcache shrink → 再 lookup で入る。
- W2: `evict_inode` は通常ファイルの `i_size` を `s3fs_entry_record_size` で local/dyn エントリに書き戻す (`s3fs_entries_mtx`)。local entry は lookup でこの size を返す。daemon の periodic sync (`entries_replace`) が来れば S3 のサイズで上書き。`s3fs_entries_mtx` 保持中に iput するパスを作らないこと (evict から同 mutex を取るため自己デッドロックになる)。
- F1: `flush_enqueue` は、WAIT (fsync) の event を積むとき、同じ ino の通常 queue の未処理 item を捨てる。WAIT queue は即座に処理され、通常 queue は daemon の ticker (100 ms) と debounce (200 ms) を通るため、`.create` が積む 0 byte の placeholder が fsync の PUT の後に PUT され、オブジェクトが 0 byte になっていた。PUT は実行時に buffer を読むので、WAIT の event が捨てた item の分も含む。
