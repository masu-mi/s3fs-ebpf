# kmod と daemon の共有メモリ — 仕組み、arena との違い、この実装で使う場合

2026-10-21。発表準備の Q&A (「kmod と daemon の間で共有メモリを持てるか」) から書いた技術メモ。**未実装・未計測**で、backlog の項目でもない。今の受け渡しは `docs/design/component-communication.md` (T7 / T8)、pcache モードは `docs/design/pcache-mode.md` を参照。

## 1. 結論

- **持てる。** kmod がカーネル側でメモリを確保し、miscdev (`/dev/s3fs_ebpf`) に `.mmap` を実装して daemon に mmap させる。io_uring の SQ / CQ リング、perf のリングバッファ、BPF の ringbuf map と同じ形。
- **arena と違って、kmod は GUP なしで読み書きできる。** メモリの持ち主が kmod なので、kmod は自分のカーネル番地でそのまま触れる。arena は BPF とユーザー空間のメモリで、kmod から見ると「よそのプロセスのユーザーメモリ」になる (`docs/user/meetup-qa.md` Q3)。
- **この実装で効きそうなのは、中身のコピーを 1 回減らすこと。** ただし chunk の確保の仕方 (ちょうどの大きさで確保し、伸びると確保し直す) を変える必要がある (§5)。効果は見積もりだけで、測っていない。

## 2. 今の受け渡し

daemon と kmod の間のデータは、すべて syscall のコピーで渡している。共有メモリはない (miscdev に `.mmap` はない。`vfs_ops.c` の `.mmap = generic_file_mmap` は FS のファイル側)。

| 流れ | 今の手段 | コピー | コード |
|---|---|---|---|
| miss の通知 (kmod → daemon) | miscdev の `read(2)` (blocking) | イベント 1 件 | `runReadMissLoop` (`admit_arena.go`) |
| 中身の受け渡し (daemon → kmod、pcache モード) | ioctl `POPULATE_PAGES` (1 回 64 MiB まで) | `copy_from_user` で chunk へ | `s3fs_ebpf_populate_pages` → `s3fs_fd_store` (`vfs_ops.c`) |
| dirty な中身の読み出し (kmod → daemon、flush) | ioctl `READ_DIRTY` / `READ_DIRTY_RANGE` | `copy_to_user` で chunk から | `s3fs_fd_copy_out_at` |
| 中身の受け渡し (arena モード) | daemon が arena に mmap で書き、`ADMIT_ARENA` で範囲を登録 | kmod が read のたびに GUP で読む | `s3fs_arena_copy_to_iter` |

pcache モードの read では、中身は「daemon のバッファ → chunk (`copy_from_user`) → page cache の folio (`.read_folio` の memcpy) → アプリ (`copy_to_user`)」と 3 回コピーされる。最後の 1 回は FUSE でも同じ。

## 3. 仕組み

1. kmod が領域を確保する。よく使われるのは `vmalloc_user()` (ゼロ埋め、ユーザーへの map 用) と、miscdev の `.mmap` での `remap_vmalloc_range()`。ページ単位で確保するなら `alloc_pages` と `vm_insert_pages()` でもよい。
2. daemon は `/dev/s3fs_ebpf` を `mmap(MAP_SHARED)` する。同じ物理ページが、kmod からは vmalloc の番地、daemon からは自分の VMA の番地で見える。
3. どこに何があるかは、ポインタではなく**領域の先頭からのオフセット**で伝える。番地は両側で違う。
4. 「書いた」「読んだ」の合図は、ioctl / `read(2)` / eventfd で送るか、リングの head / tail を共有して送る (io_uring と同じ)。リングにすると、件数が多いときに syscall を減らせる。

## 4. arena との比較

| | kmod が持つ共有メモリ | bpf_arena |
|---|---|---|
| 持ち主 | kmod (miscdev の `.mmap`) | BPF の map (`BPF_MAP_TYPE_ARENA`) |
| kmod から | 自分のカーネル番地で直接。GUP なし | kern_vm の番地は未 export。今は daemon の user_vm を GUP (1 回約 1.7 µs) |
| daemon から | mmap した番地で直接 | mmap した番地で直接 |
| BPF から | 直接は見えない。KFunc を通す | `__arena` ポインタで直接 (JIT が kern_vm に変換) |
| ポインタ | 共有できない (オフセットで指す) | arena 内のポインタは BPF とユーザー空間で共有できる |
| 大きさ | 確保した分で固定 (作り直しが要る) | 最大 4 GiB の疎な領域、ページ単位で割り当て |
| 向く使い方 | kmod と daemon の間の大量の受け渡し、イベントのリング | BPF とユーザー空間の間で、ポインタを含む構造を共有する |

「kmod と daemon」の間なら kmod の共有メモリ、「BPF とユーザー空間」の間なら arena、と分けて考える。3 者 (kmod、BPF、daemon) で同じメモリを共有する正規の手段は今はない (kmod から arena を使えない理由は `docs/user/meetup-qa.md` Q3 と backlog K-01 / K-02)。

## 5. 設計の注意

- **daemon が書いた値は信用しない。** daemon はいつでも書き換えられるので、kmod は長さやオフセットを `READ_ONCE` で一度ローカルにコピーしてから検証し、その値だけを使う (二度読むと、検証と使用の間に変わる)。中身のバイト列はそのままでよい。
- **順序**: 「データを書いてから tail を進める」を release、「tail を読んでからデータを読む」を acquire にする (`smp_store_release` / `smp_load_acquire`、Go 側は `sync/atomic`)。
- **所有権**: 領域のある部分を「今どちらが書いてよいか」を決めて、受け渡しのたびに渡す。kmod が chunk を evict して別の ino に使い直す間に、daemon が古い指示で書き込むと、別のファイルの中身を壊す。スロットごとに世代番号を持たせ、kmod は populate のときに世代を確かめる (今の S1 の世代確認と同じ考え方、`docs/kmod-pcache-invariants.md`)。
- **寿命**: daemon が munmap や終了しても、ページは mapping と kmod の参照がなくなるまで残る。daemon の再起動で新しい領域を作るか、同じ領域を使い直すかを決める。
- **メモリ**: map したページは swap されない。大きさはホストのメモリに合わせて決め、`capacity_bytes` の数え方に入れる (容量の既定値の課題は `docs/journal/benchmark-results.md` §18.39.1)。
- **権限**: `/dev/s3fs_ebpf` を開けるのは daemon (root) だけ、という今の前提を保つ。

## 6. この実装で使うなら

候補を効果の大きい順に並べる。どれも測っていない。

| 候補 | 何が減るか | 変えるもの | 見込み |
|---|---|---|---|
| A. chunk を共有領域に置く | populate の `copy_from_user` (中身 1 回分) と、flush の `copy_to_user` | chunk の確保。今は `kvmalloc` でちょうどの大きさに取り、伸びると確保し直して memcpy する (`s3fs_fd_chunk_ensure`)。共有するなら 4 MiB 固定のスロットの pool にし、所有権と世代を足す | 大きいファイルの cold read。300 MiB なら memcpy 1 回分で、メモリ帯域を 10 GB/s とすると 30 ms 程度 (見積もり)。今の cold read は 230〜385 ms (§18.37〜18.39) |
| B. miss / flush のイベントをリングにする | イベントごとの `read(2)` / ioctl | miscdev の read と POP 系の ioctl | miss は S3 の GET (ms 単位) が支配するので、効果は小さい。イベントが多い `ls -l` や小さいファイルの cold で効くかは要計測 |
| C. 統計やヒントの表を共有する | `S3FS_IOC_STATS` などの ioctl | 読み取り専用の領域を足す | 頻度が低いので、今の ioctl で困っていない |

A の代わりに失うもの:
- 46 B のファイルに 46 B だけ確保する、今の「ちょうどの大きさ」(AC-41 / AC-45 C1)。固定スロットは小さいファイルが多いとメモリを無駄にする。小さいファイルは今の `kvmalloc` のまま、大きいファイルの chunk だけ pool から取る、という分け方が要る。
- chunk の不変条件 (R1、pin、shed、reset) に所有権の受け渡しが加わる。`docs/kmod-pcache-invariants.md` を同じ変更で更新する。
- page cache の folio へのコピー (`.read_folio`) は残る。page cache を飛ばす経路 (AA-03) に戻すのではない。

## 7. 関連

- `docs/design/pcache-mode.md` — chunk の表と populate の流れ
- `docs/design/component-communication.md` — T7 (ioctl)、T8 (miscdev の read)
- `docs/user/meetup-qa.md` Q3 (kmod が kern_vm を使えない理由)、Q13 (arena は今どこで使われているか)
- `docs/design/bpf-arena-use-cases.md` — arena が向く使い方
