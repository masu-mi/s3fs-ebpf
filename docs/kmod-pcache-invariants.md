# kmod pcache invariants (kmod/vfs_ops.c)

`vfs_ops.c` を読み直さずに済ませるための要約。関数名は `scripts/fn.sh <name>` で引ける。コードを変えてここと食い違ったら、このメモも更新すること。

## ロック順 (外 → 内)
- `fd->lock` (mutex, per-inode `s3fs_file_data`) → `s3fs_admits_lock` (rwlock)。逆順で取るパスは存在しない (R1 はこれに依存)。
- `s3fs_pending_lock` / `s3fs_miss_lock` / `s3fs_flush_*_lock` は spinlock で末端。保持中に `fd->lock` や `s3fs_admits_lock` を取らない。
- `admit_evict_ino` は `admit_drop_all_ranges` (admits write_lock) を解放した後に `s3fs_evict_page_cache` → `s3fs_file_data_reset` (fd->lock → admits write_lock) を呼ぶ。

## R1 不変条件
- **ino の PCACHE admit entry が hash にあれば、その ino の `fd->buf != NULL`。**
- 保守する側: `s3fs_ebpf_populate_pages` は copy と `populated_chunks` のビットセットに続けて、`fd->lock` を保持したまま `admit_insert_pcache` を呼ぶ。`s3fs_file_data_reset` は `fd->lock` 下で残っている PCACHE entry を再 evict してから buf を free する (drop と reset の間に populate が挟まるケースへの対策)。
- S1: R1 が成り立つのは `fd->lock` 下だけ。`fc_pcache_check` (lockless) から `s3fs_folio_fill_from_kmalloc` が lock を取るまでに `s3fs_admit_gen` が進んでいたら、entry を read_lock で再検証する。`gen == 0` は検証なしの fallback を意味する。
- T2: `gen != 0` かつ chunk index `ci < 64` のときは `populated_chunks` のビットも要求する (reserve の memset で 0 埋めされた未取得 chunk を返さないため)。`ci >= 64` (256 MiB 超) はビットがないので R1 だけに頼る。dirty/flushing のときはビットマップを見ない (T1)。

## pending のキー
- `s3fs_pending` は `(ino, chunk_offset)` 単位で、chunk は 4 MiB (`S3FS_PCACHE_CHUNK_SHIFT=22`)。hash key は `ino ^ (chunk_offset >> 22)`、照合は `p->ino` と `p->chunk_offset` の両方で行う。
- refcount は 2 本立て (H-06)。作成時は refs=2 (hash の分 + caller の分)。SIGNAL_ADMIT / shutdown が落とすのは hash の分だけで、waiter は `p->err` を読んだ後に自分の分を `pending_put` で落とす。
- `pending_exists(ino)` は eviction 側のガードで、pending 中の ino を evict 候補から外す。**注意:** これは key=`ino` の bucket だけを走査する。chunk_offset≠0 の pending は通常別の bucket に入るため見落とす可能性が高い。そのぶんは buf_readers が buf を守る。

## buf_readers (U8/U10)
- `fd->buf_readers` は `fd->lock` で保護する。`s3fs_ebpf_read_folio` の中で pcache 経路 (U10) または upcall 経路 (U8) に入るときに 1 回だけ ++ し (`buf_reader_active`)、`done:` で -- する。
- 意味: 「この reader は retry や gen=0 fallback で `fd->buf` を後から読む可能性がある」。> 0 の間は reset が buf を free しない。
- upcall のリトライ上限は `S3FS_UPCALL_MAX_RETRIES=3`。timeout (U11) もこの回数に含める。使い切ったら gen=0 fallback に進む。

## reset の条件 (`s3fs_file_data_reset`)
- 呼ばれる経路: `admit_evict_ino` の中で、`admit_drop_all_ranges` が PCACHE entry を落としたと報告したとき (S4: 判定は write_lock 内で atomic に行う) だけ。
- `fd->lock` 下で `!dirty && !flushing && buf_readers == 0` のときに限り、PCACHE entry の再 evict (1 件でも消したら `s3fs_admit_gen++`) → `kvfree(buf)`、`size`/`cap`/`populated_chunks` を 0 にする。
- どれかが偽なら何もしない (skip_reset)。後で再試行する仕組みはなく、次に evict されたときに回収される。inode の `i_size` はクリアしない (Q1)。
