# AC-75 / AC-76 で分かったこと (daemon の disk cache と chunk 表の read)

層ごとのレイテンシ (`br §18.49`) で見つかった 2 件の対処で得た知見。手順は `.claude/skills/kmod-change-verify/SKILL.md` と `.claude/skills/perf-bench-verify/SKILL.md`。

## AC-75: 4 MiB を超える object に daemon の disk cache が効かない (PR #42)

- 原因: 4 MiB 超の object は chunk ごとの `GetRange` で取り、`GetRange` は disk cache を読まず書かなかった (cache は object 全体がキー)。64 MiB は upcall 288 回に対して S3 GET が 1136 回。
- 対処: ETag が分かっているとき、chunk を `(key, offset, size)` 単位で cache する。仕様は `docs/design/disk-cache-range.md`。
- 効果: daemon 層で、64 MiB を 4 MiB ずつ 2 回読むと 2 回目の S3 GET が 0 回 (`TestGetRange_LargeObjectSecondReadNoGET`)。**kmod 経由のレイテンシは未測定** (元は 45.5 ms で s3fs-fuse の disk cache の 0.58 倍)。
- 知見:
  - 全体 `Get` と違い、chunk の cache は ETag を照合しないと由来を確かめられない。If-Match で取り、読むときは header の ETag と観測中の ETag を照合する。ETag が未観測なら cache しない。
  - chunk 列は列挙できないので、`Put` / `Delete` / `Rename` は key の観測 ETag を忘れる方式で止める (古い chunk は読まれて捨てられるか eviction で消える)。
  - write-behind が枠いっぱいのとき書き込みを**捨てる**設計だと、64 MiB (16 chunk 同時) の 2 回目が S3 GET に戻る。待つ設計にした。
  - 数字が不自然に揃っている行 (層 3 と層 4 が 45.5 ms で同じ) は、その行の counter (S3 GET 数) を見る。
  - 未了: GCS の `GetRange`、kmod 経由の再測定。

## AC-76: chunk 表からの read が page cache より遅い (PR #43)

- 原因: この FS の sb は専用の bdi を持たず、`noop_backing_dev_info` (`ra_pages == 0`) を使っていた。readahead が一度も呼ばれず、cold な sequential read は 4 KiB の folio ごとに `.read_folio` (`page_cache_sync_ra` が 1 ページずつ)。
- 試したこと:
  1. bdi + `.readahead` (4 KiB の folio のまま): `.read_folio` は 0 回になったが**レイテンシは変わらなかった** (64 MiB: 21.5 → 24.5 ms)。
  2. large folio + `.readahead`: 4 MiB 1.09 → 0.74 ms、64 MiB 21.5 → 16.1 ms (page cache は 0.29 / 6.8 ms)。
- 内訳 (`ebpf-pcachedrop`、64 MiB の `perf`): folio の確保 + page cache への挿入 約 22%、folio への memcpy 約 14%、`copy_to_user` 約 12%、open ごとの `invalidate_inode_pages2` 約 10% (ベンチの仕組み)。kmod 側のロックと hit 計上は小さい。page cache を捨てて入れ直す費用が支配的で、folio の数を減らして初めて縮んだ。
- 知見:
  - `readahead_count()` / `readahead_pos()` は直前に取った folio の分だけ遅れる。覗き見ずに `readahead_folio()` のループで `folio_pos()` を使う (覗き見の版は NULL を触って oops した)。
  - `kmap_local_folio(folio, 0)` は 1 ページしか map しない。large folio では `memcpy_to_folio` / `folio_zero_range`。
  - `file->f_ra.ra_pages` は `->open` のあとで上書きされる。窓は bdi で決める。
  - 効くか分からない変更は、入れる前に `perf` で割合を見る。割合が動かなければ効いていない。
  - hit の計上が folio 単位なので、large folio では `folio_hits` が減る。
  - 未了: `make test-lockdep` (lockdep 付きカーネルが要る)。

## 切り分けの方法 (perf / bpftrace)

- 走行中に `sudo $P record -a -g -F 2000 -o <out> -- sleep 6`、`sudo $P report -i <out> --children --sort sym -g none | grep -E "<関数>"`。`--children` の割合が、その関数の下の合計。比べる関数: `filemap_read`、`page_cache_ra_order`、`read_pages`、`invalidate_inode_pages2`、`__memcpy`、`_copy_to_iter`、`filemap_add_folio`。
- 呼び出し回数と状態: `sudo timeout 20 bpftrace -e 'kprobe:<kmod の関数>{@a=count();} fentry:vmlinux:page_cache_sync_ra{@ra=lhist(args->ractl->ra->ra_pages,0,2048,256);} interval:s:4{exit();}'`。kmod の関数は kprobe、BTF のあるカーネル関数は fentry ("No BTF found" になる関数もある)。AC-76 は readahead が 0 回で、`ra_pages` の分布が [0,256) だけだったことで原因に気づいた。

## 環境の落とし穴

- kmod が oops すると、`rmmod` も `rmmod -f` も効かず、再起動が要る (今回はホストを止めた)。再起動後は docker のコンテナが止まっているので `make compose-up`。
- oops の読み方: 最初の `RIP: 0010:<関数>+0x..` だけ読み、`addr2line -e kmod/s3fs_kfunc.ko -f -i <nm の関数先頭 + offset>` で行に落とす。後続の "scheduling while atomic"、"Fixing recursive fault but reboot is needed" は連鎖。
- `perf` は kernel 版が合わず、`/usr/lib/linux-tools/<版>/perf` を直接指定する。
- `bench-layer-latency.sh` はリポジトリのルートから実行する (bpf の相対パス)。`timeout` を付ける。
- `/tmp` は再起動で消える。ベンチの出力は scratchpad に置く。
