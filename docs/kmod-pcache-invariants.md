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
- T5: populate が chunk に `populated` を立てるのは、範囲が chunk の先頭から始まり、chunk の終端か eof まで届くときだけ (`s3fs_populate_fills_chunk`)。chunk の途中から始まる範囲は、eof まで届いても立てない (以前は最後の chunk だけこの確認がなく、取得していない前半を 0 として返していた)。daemon の populate は chunk 境界から始まるので、通常の経路では変わらない。
- T2: `gen != 0` のときも `gen == 0` のときも、確保済みの chunk に `populated` フラグが立っていることを要求する (未取得の chunk の 0 を返さないため)。フラグは chunk ごとにあるので、256 MiB を超えるファイルも同じ判定になる (旧: `ci >= 64` は R1 だけに頼っていた)。dirty/flushing のときは見ない (T1): 確保されていない chunk は hole で、0 として読める。`fd->size` より後ろも 0 (truncate が read と競合したとき)。
- EOF: `.read_folio` は i_size より後ろの folio を、upcall も fallback もせずに 0 で埋めて成功させる (filemap_read が i_size で切る)。

## pending のキー
- `s3fs_pending` は `(ino, chunk_offset)` 単位で、chunk は 4 MiB (`S3FS_PCACHE_CHUNK_SHIFT=22`)。hash key は `ino` のみ (P1: 同じ ino の全チャンクが同じ bucket に入る)。照合は `p->ino` と `p->chunk_offset` の両方で行うので、チャンクごとの完了待ちは独立 (U13)。
- refcount は 2 本立て (H-06)。作成時は refs=2 (hash の分 + caller の分)。SIGNAL_ADMIT / shutdown が落とすのは hash の分だけで、waiter は `p->err` を読んだ後に自分の分を `pending_put` で落とす。
- `pending_exists(ino)` は eviction 側のガードで、pending 中の ino を evict 候補から外す。key=`ino` の bucket を走査し、どのチャンクの pending でも見つける。hash key を変えるときはこの前提を崩さないこと。

## 成功 signal の契約 (AC-45 C2)
- `S3FS_IOC_SIGNAL_ADMIT` に `S3FS_SIGNAL_VERIFY_POPULATED` を付けて `err == 0` で呼ぶと、`s3fs_ebpf_signal_admit` は pending (ino, chunk_offset) を見つけたあと、その chunk が確保済みかつ `populated` であることを `fd->lock` 下で確かめる。なければ **pending を完了させず `-EAGAIN`** を返す。pcache モードの daemon (`handleReadMiss`) だけがこのフラグを付ける (arena の admit は chunk を持たない)。
- 理由: 同じ ino の miss を複数の handler が同時に処理し、先に終わった handler が pending を完了させたあとで、遅れて動いている handler は pending に守られないまま evict される。その handler が「成功」を signal すると、reader は何もない状態で起こされ、retry (`S3FS_UPCALL_MAX_RETRIES=3`) を古い signal で使い切って gen=0 フォールバック (chunk なし) で EIO になる。C2 で evict が頻繁になって表に出た。
- 成立条件: reader は **pending を作る前に** chunk を pin する (`readers++`、`s3fs_ebpf_read_folio`。chunk の pin の節)。pending があれば reader は登録済みなので、signal の時点で chunk が populated なら、reader が終わるまで reset に解放されない。
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

## 途中の変更で運ばない部分 (AC-51、`partial_modify`、既定は off)
設計は `docs/design/ac51-partial-modify.md`。kmod は `partial_modify=1` かつ `i_size >= partial_modify_min` (既定 16 MiB) のときだけ、次のように変わる。off のときは上の AC-50 のまま。**daemon の flush は `DIRTY_LAYOUT` で remote があれば multipart (`PutWithCopy`) にする** (`daemon/cmd/s3fs-ebpf/flush_partial.go`)。daemon は `--partial-modify` で、backend が `MultipartCopier` (S3) のときだけこのパラメータを 1 にする。kmod 単独で on にすると、古い daemon の `handleFlush` が fd 全体を `READ_DIRTY` で PUT して remote の chunk を 0 で上書きするので、on にしない。ETag は変更サイクルの最初の load (なければ flush) で `Stat` して pin し、flush 成功で捨てる。412 のときは現在のオブジェクトから remote の範囲を取り直して PUT する。

`.write` / `setattr` (サイズ変更) は fd->lock を外したあとに page cache の該当範囲を invalidate / truncate する (読んだあとに書いた範囲が古く見える問題の修正)。fd->lock を持ったまま呼ぶと、folio lock を持つ `.read_folio` との間で逆順になる。
- **dirty な fd の chunk は data / hole / remote のどれか。** remote (`chunks[ci].remote`、`fd->nr_remote` に数える) は、S3 のオブジェクトと同じバイトで fd が持たない chunk。remote があるのは dirty (または flushing) な fd だけ。
- **何を読み込むか (`struct s3fs_mop`):** 変更が決めるバイト範囲 `[t_lo, t_hi)` に触れ、かつ全部は上書きされない chunk だけが「データが要る」(`s3fs_mop_needs_data`)。それ以外は fd に持たず、clean → dirty の瞬間に `s3fs_fd_mark_remote_locked` が remote にする (すでに populated な chunk は data のまま)。write は `t = [min(開始, i_size), 終端)`、縮小は `[新サイズ, 元のサイズ)`、拡張は `[元のサイズ, 新サイズ)`。chunk 全体の範囲で交差を見る (境界の chunk は、元のサイズより手前にあっても触れたと数える)。
- **ready の判定は chunk ごと:** clean な fd は「populated」、dirty な fd は「remote でない」。dirty で `nr_remote == 0` なら何もしない (大きな dirty ファイルの write で全 chunk を走査しない)。pin は `s3fs_mop_needs_data` が選ぶ chunk だけ (acquire と release が同じ関数を使う)。
- **remote が消える所:** `s3fs_fd_store` (書いた chunk は data)、`s3fs_fd_chunk_free` (truncate)、flush 成功 (`signal_flush`、`flush_read_seq == flush_seq` かつ `!dirty` のとき。dirty に戻っていたら remote のまま残す: 次の flush の copy 元になる)。
- **remote の chunk を読む / 書く:** `s3fs_folio_fill_from_kmalloc` の dirty 側は remote の chunk を「データなし」にする。`read_folio` はその chunk に `S3FS_MISS_FLAG_FOR_WRITE` の miss (chunk 全体、Count = i_size までの長さ) を出す。`s3fs_ebpf_populate_pages` は dirty な fd でも、**範囲が remote の chunk を丸ごと (i_size まで) 覆うときだけ** 受け付ける。このとき `fd->size` / `i_size` を動かさず、PCACHE entry を入れず (dirty な fd は capacity の対象外)、store が remote を外す。それ以外の dirty への populate は今までどおり -EBUSY。load が daemon の失敗を受けたら、そのときの状態で chunk が揃っていれば成功とする (同時に来た writer がすでに持っている)。
- **dirty な fd の読み出しは upcall しない (remote を除く):** `s3fs_ebpf_read_folio` は、dirty / flushing な fd で remote でない chunk (持っている chunk と穴) の miss では daemon を呼ばず、fd から直接読む (`gen=0` の fill)。daemon は dirty な fd に populate できず (-EBUSY)、folio ごとに timeout を待っていた (AC-51 の煙テストで、旧終端より先を `drop_caches` 後に読むと数十秒かかった)。
- **flush 側の ioctl:** `S3FS_IOC_DIRTY_LAYOUT` が chunk ごとの状態 (data / hole / remote) と `fd->size` を返し、`flush_read_seq = flush_seq` を記録する (READ_DIRTY と同じく、ここまでの enqueue を覆う)。`S3FS_IOC_READ_DIRTY_RANGE` は `[offset, offset+size)` を読む。data / hole の chunk だけ読むこと (remote は 0 が返る)。

- **ETag の pin (daemon、`objETags`):** 変更サイクルの最初の load (なければ flush) で Stat した ETag。kmod の miss event に `S3FS_MISS_FLAG_PINNED` が立つのは、fd が dirty / flushing か remote chunk を持つとき (中身が pin に依存する)。daemon の load (`getPinned`) は 412 (`ErrSourceChanged`) を受けたとき、(a) flush が終わって pin が新しい ETag になっていれば読み直して 1 回やり直す、(b) PINNED でなければ (放棄された cycle の古い pin) pin を捨てて Stat し直す、(c) PINNED なら本物の競合として EIO。flush の成功は `PutWithCopy` が返す新しい ETag を pin に保存する (remote chunk の中身は同じ)。plain put のときは pin を捨てる。daemon は起動のたびに kmod の `partial_modify` に 0 か 1 を明示的に書く (module は daemon より長生きする)。copier のない backend で remote chunk を持つ fd の flush は、全体 PUT を断る (remote を 0 で上書きするため)。
- フォールバック (412 のあとの全体 PUT) で、現在の object が remote chunk の範囲より短いとき (外部の truncate) は、足りない bytes を 0 で埋めて警告を出す (`getNowClamped` + `fillRange` の非 pinned)。
- **flush 成功で `populated` にしない chunk:** remote の chunk は buffer (`p`) を持っていても、flush が copy したのは object のバイトで buffer ではない。`signal_flush` は `p` を持つ chunk のうち remote でないものだけを populated にする (途中までしか埋まっていない chunk を clean として返さない)。

## hit の集計 (issue #22、`hit_accum`、既定 on)
- 設計は `docs/design/hit-signal-percpu-accumulation.md`。`hit_accum=1` (既定) のとき、`.read_iter` は `s3fs_file_data` の per-CPU 値 (`fd->hit`、fd と同じ寿命) を進めるだけで、ロックを取らない。`on_hit` の直接の呼び出し (fentry の KFunc、`.read_folio`、arena 直読み) は何もしない。
- `fd->hit_flushed` / `fd->policy_seen` は **`s3fs_policy_evict_lock` の下でだけ** 触る。`admit_replace` の Phase 1 が `pv || hit_accum` のときにこの mutex を取り、`ilookup` で fd を引いて差分を集め (`iput` は mutex の中、`s3fs_ino_buf_bytes` と同じ)、Phase 1 のあとで `last_tick` の昇順に `on_hit` を呼び、その後で `on_evict` の票を取る。`on_remove` を呼ぶ所は `s3fs_hit_forget` で `policy_seen` を落とす。
- 反映の順序: Phase 1 のループの前に全 ino の差分を集め、既知の ino (`policy_seen`) の hit を先に、新しい ino の hit を `on_insert` のあとに反映する。populate (fd->lock 下) は `hit_base` / `hit_base_set` を書き、`policy_seen` が偽のときの最初の look が読む (READ_ONCE/WRITE_ONCE、lock なし)。`.splice_read` も `s3fs_hit_count` を呼ぶ。
- **policy への通知は `s3fs_policy_track` / `s3fs_policy_untrack` だけが行う** (`on_insert` / `on_size` / `on_remove` を直接呼ばない)。hit の状態を忘れるのは、この `untrack` と `admit_evict_ino` (丸ごとの evict の唯一の出口) だけ。`scripts/check-kmod-invariants.sh` (`make check-invariants`、`make test-regression` に含む) が、それ以外の呼び出しを見つけたら落ちる。
- **pending 中の ino は `on_insert` / collect しない** (`s3fs_policy_trackable`、Phase 1、`pick_victim` を持つ policy)。pending は kmod の丸ごとの evict を止める (shed になる) が、policy がその ino を victim に選ぶと ghost / hand が動いて policy と kmod の状態がずれる。次の round (pending が消えてから) に回す。
- fd->lock は取らない。fd の確保 (`s3fs_file_data_new`) で `alloc_percpu`、解放 (`s3fs_file_data_free`) で `free_percpu`。

## flush pin (W1)
- close/fsync の `s3fs_maybe_enqueue_flush` が `flush_seq++` し、`s3fs_flush_pin` で inode を `ihold` する (fd ごとに最大 1 本、`pin_link` が `s3fs_flush_pins` に載っていれば pin 中)。これがないと drop_caches / reclaim で `evict_inode` が PUT 前の `fd->buf` を捨てる。
- `read_dirty` が `flush_read_seq = flush_seq` を記録。`signal_flush` は `err≠0 || flush_read_seq==flush_seq` のとき `flushing=false`、さらに `err==0 && !dirty` なら unpin (`iput` は fd->lock 解放後)。seq 不一致 = 後続 enqueue が未 PUT なので flushing/pin を維持。err≠0 は flushing を下ろすが pin は保持。
- W3: fsync の waiter (`s3fs_flush_pending`) を起こすのも、同じ seq の条件を満たす signal だけ (`err≠0 || flush_read_seq==flush_seq`)。seq 不一致の成功 signal は、fsync が書く前に読んだ PUT なので、waiter には触らず 0 を返す (fsync 自身の WAIT event の signal が起こす)。以前は close(2) の PUT の完了で、fsync がデータを読まれる前に 0 を返していた。残る窓: fsync の WAIT event の read_dirty の後に古い PUT の signal が来ると、seq が一致するので起こしてしまう (signal が seq を運ばないため)。
- `put_super` の `flush_queue_drain` は fsync queue を shutdown にする (`POP_FLUSH_WAIT` が -ENODEV)。`fill_super` が `flush_wait_reopen` で戻す。戻さないと rmmod しない再 mount で daemon の fsync loop が即終了していた。
- 残った pin (daemon 不在・PUT 失敗・キュー破棄) は `put_super` の `s3fs_flush_pins_release_all` が解放 (pr_warn)。unpin の所有権はリスト所属で決まるので二重 iput しない。
- AC-48: `admit_replace` は i_size を、中身が変わった entry の range 終端 (`offset + size`) まで **伸ばすだけ** で縮めない。dirty/flushing の inode には触らない。entry は range なので `size` を i_size に書いてはいけない (縮むと W2 が誤ったサイズを entry に書き戻す)。S3 側の縮小は `entries_replace` の dcache shrink → 再 lookup で入る。
- AC-56: `admit_replace` が i_size を伸ばすときの「dirty/flushing でない」確認と `i_size_write` は `fd->lock` 下 (`s3fs_inode_grow_size_unowned`)。write / truncate が dirty と i_size を `fd->lock` 下で変えるので、確認と書き込みの間に挟まった truncate を巻き戻さない。呼び出し時に admits_lock は解放済み (fd->lock が外側)。
- AC-57: dirty / flushing な fd に PCACHE entry を入れない。AC-45 C3 の entry の入れ直し (`s3fs_folio_fill_from_kmalloc` の `quiet`) は、clean な fd で chunk が確保済みかつ `populated` のときだけ (dirty の穴は `buf_valid` が true になるが、entry を入れると容量を水増しする)。
- W2: `evict_inode` は通常ファイルの `i_size` を `s3fs_entry_record_size` で local/dyn エントリに書き戻す (`s3fs_entries_mtx`)。local entry は lookup でこの size を返す。daemon の periodic sync (`entries_replace`) が来れば S3 のサイズで上書き。`s3fs_entries_mtx` 保持中に iput するパスを作らないこと (evict から同 mutex を取るため自己デッドロックになる)。
- F1: `flush_enqueue` は、WAIT (fsync) の event を積むとき、同じ ino の通常 queue の未処理 item を捨てる。WAIT queue は即座に処理され、通常 queue は daemon の ticker (100 ms) と debounce (200 ms) を通るため、`.create` が積む 0 byte の placeholder が fsync の PUT の後に PUT され、オブジェクトが 0 byte になっていた。PUT は実行時に buffer を読むので、WAIT の event が捨てた item の分も含む。

## policy の pick_victim (AC-58)
- 設計は `docs/design/ac58-pick-victim.md`。vtable の `on_insert` / `on_remove` / `pick_victim` は、**vtable が `pick_victim` を持つときだけ**、`s3fs_ebpf_admit_replace` の eviction block が `s3fs_policy_evict_lock` (mutex) を取って呼ぶ。この mutex は `fd->lock` / `s3fs_admits_lock` より外側 (admit_replace は他のロックを持たずに入る)。mutex の中で `admit_evict_ino` (fd->lock → admits write_lock) を呼ぶ。
- 容量超過のとき、まず `pick_victim` が返した ino を (`cand[]` にあり、未試行なら) `s3fs_capacity_evict_one` で処理する。`cand[]` にない ino が返ったら (別の経路で evict 済み)、`on_remove` を呼んで聞き直す (AC-64)。0 / 試行済みが返ったら、残りは score 順 (`on_evict` + tiebreak) に戻る。pending 中の ino は丸ごとは evict せず shed になる (AC-45 C3 のまま)。`on_remove` を呼ぶのは、丸ごと evict したときと、上の聞き直しのときだけ。
- AC-66: `on_size(ino, bytes)` は Phase 1 で `on_insert` の直後に、同じ mutex の中で呼ぶ (`capacity_bytes > 0` のときだけ)。bytes は live_bytes と同じ数え方 (arena は entry の合計、pcache は `s3fs_ino_buf_bytes`)。`s3fs_ino_buf_bytes` は `ilookup` + `READ_ONCE(fd->cap)` で fd->lock を取らない。
