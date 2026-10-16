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

## buf_readers (U8)
- `fd->buf_readers` は `fd->lock` で保護する。`s3fs_ebpf_read_folio` の中で upcall 経路 (U8) に入るときに 1 回だけ ++ し (`buf_reader_active`)、`done:` で -- する。pcache ヒット経路の ++ (U10) は A/B で不要と確認して削除済み (hot path で fd->lock を取らない)。
- 意味: 「この reader は retry や gen=0 fallback で `fd->buf` を後から読む可能性がある」。> 0 の間は reset が buf を free しない。
- upcall のリトライ上限は `S3FS_UPCALL_MAX_RETRIES=3`。timeout (U11) もこの回数に含める。使い切ったら gen=0 fallback に進む。

## reset の条件 (`s3fs_file_data_reset`)
- 呼ばれる経路: `admit_evict_ino` の中で、`admit_drop_all_ranges` が PCACHE entry を落としたと報告したとき (S4: 判定は write_lock 内で atomic に行う) だけ。
- `fd->lock` 下で `!dirty && !flushing && buf_readers == 0` のときに限り、PCACHE entry の再 evict (1 件でも消したら `s3fs_admit_gen++`) → `kvfree(buf)`、`size`/`cap`/`populated_chunks` を 0 にする。
- AC-41: 容量判定 (`live_bytes`) は arena entry のサイズ + pcache ino ごとの `fd->cap` (`s3fs_ino_buf_bytes`、ino 単位で 1 回)。`fd->cap` は確保済み chunk の `cap` の合計。admit 済み chunk のサイズではない。evict 後は before/after の `fd->cap` 差分を解放量として引く (skip_reset なら 0)。
- AC-45 (C1): fd の中身は 4 MiB の chunk の表 (`fd->chunks[]`、`S3FS_CHUNK_SIZE`)。populate は、足りない chunk を `min(4 MiB, max(終端, i_size) - chunk 先頭)` ちょうどで確保する (AC-41: 46 B のファイルは 46 B。AC-46 の倍々と全体の memcpy はなくなり、触った chunk の分だけ確保する)。write は chunk の中で倍々 (最小 4 KiB、上限 4 MiB) に伸ばす (非 pcache の dirty ファイルは最大で 2 倍まで確保する)。確保済み chunk のうち fd->size 以降のバイトは 0 (縮小は境界 chunk の後半を 0 にして、後ろの chunk を free)。setattr の拡張は chunk を確保しない (hole、dirty の間は 0 として読める)。`populated` は signal_flush でも確保済み chunk にだけ立つ (hole は次の read で取り直す)。chunk の上限は `S3FS_MAX_CHUNKS` (1 TiB)、超えたら -EFBIG。
- M1/M2: `fd->cap` を変える箇所は `s3fs_fd_chunk_ensure` と `s3fs_fd_chunk_free` の 2 つだけで、そこで `s3fs_fd_buf_bytes` も同じだけ増減する (file_data_new / write / populate / reset / free / truncate はすべてこの 2 つを経由する)。`s3fs_ebpf_fd_buf_audit` (STATS[12/13]) が全 inode を走査した合計と突き合わせる (I/O が止まっていれば gauge == walk)。走査は `s_inode_list_lock` → `i_lock` だけを取り、admits_lock とは入れ子にしない。
- どれかが偽なら何もしない (skip_reset)。後で再試行する仕組みはなく、次に evict されたときに回収される。inode の `i_size` はクリアしない (Q1)。

## flush pin (W1)
- close/fsync の `s3fs_maybe_enqueue_flush` が `flush_seq++` し、`s3fs_flush_pin` で inode を `ihold` する (fd ごとに最大 1 本、`pin_link` が `s3fs_flush_pins` に載っていれば pin 中)。これがないと drop_caches / reclaim で `evict_inode` が PUT 前の `fd->buf` を捨てる。
- `read_dirty` が `flush_read_seq = flush_seq` を記録。`signal_flush` は `err≠0 || flush_read_seq==flush_seq` のとき `flushing=false`、さらに `err==0 && !dirty` なら unpin (`iput` は fd->lock 解放後)。seq 不一致 = 後続 enqueue が未 PUT なので flushing/pin を維持。err≠0 は flushing を下ろすが pin は保持。
- 残った pin (daemon 不在・PUT 失敗・キュー破棄) は `put_super` の `s3fs_flush_pins_release_all` が解放 (pr_warn)。unpin の所有権はリスト所属で決まるので二重 iput しない。
- AC-48: `admit_replace` は i_size を、中身が変わった entry の range 終端 (`offset + size`) まで **伸ばすだけ** で縮めない。dirty/flushing の inode には触らない。entry は range なので `size` を i_size に書いてはいけない (縮むと W2 が誤ったサイズを entry に書き戻す)。S3 側の縮小は `entries_replace` の dcache shrink → 再 lookup で入る。
- W2: `evict_inode` は通常ファイルの `i_size` を `s3fs_entry_record_size` で local/dyn エントリに書き戻す (`s3fs_entries_mtx`)。local entry は lookup でこの size を返す。daemon の periodic sync (`entries_replace`) が来れば S3 のサイズで上書き。`s3fs_entries_mtx` 保持中に iput するパスを作らないこと (evict から同 mutex を取るため自己デッドロックになる)。
