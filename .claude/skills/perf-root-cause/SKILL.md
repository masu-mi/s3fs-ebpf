---
name: perf-root-cause
description: 遅い行・不審な値の原因を perf / bpftrace で切り分ける。「なぜ遅い」「perf record」「bpftrace」「効いたか確かめて」で使う。変更を入れる前に。
---

# 性能の原因切り分け (perf / bpftrace)

**変更を入れる前に内訳を取る。** 推測で入れた変更 (AC-76 の readahead) は、`.read_folio` を 0 回にしても時間が変わらなかった。事例と手順の原典: `docs/dev-notes/ac75-ac76-lessons.md`。測定そのものと環境の衛生は `perf-bench-verify` §0 / §4b。

## 手順

1. **再現する行を 1 つに絞って長く走らせる**: 例 `LAYERS=<層> SIZES=<サイズ> TARGET_SEC=40 OUT=<scratch>/x timeout 420 scripts/bench-layer-latency.sh`。**リポジトリのルートから** (bpf の相対パス)。`timeout` を付ける。
2. **走行中に perf で採る**: `P=/usr/lib/linux-tools/<版>/perf` (`/usr/bin/perf` はラッパーで kernel 版が合わず止まる。`perf_bin` が `scripts/lib/bench.sh` にある)。
   `sudo $P record -a -g -F 2000 -o <out> -- sleep 6` → `sudo $P report -i <out> --children --sort sym -g none | grep -E "<関数>"`。`--children` の割合がその関数の下の合計。比べる関数: `filemap_read` `page_cache_ra_order` `read_pages` `invalidate_inode_pages2` `__memcpy` `_copy_to_iter` `filemap_add_folio`。
3. **呼び出し回数と状態を bpftrace で**: `sudo timeout 20 bpftrace -e 'kprobe:<kmod の関数>{@a=count();} fentry:vmlinux:page_cache_sync_ra{@ra=lhist(args->ractl->ra->ra_pages,0,2048,256);} interval:s:4{exit();}'`。kmod の関数は kprobe (static はインライン化されて刺さらないことがある)、BTF のあるカーネル関数は fentry。**「1 回も呼ばれていない」の確認**にも使う (AC-76 は readahead が 0 回、`ra_pages` が [0,256) だけで原因に気づいた)。
4. **ベンチの仕組みが数字に入っていないか**: `ebpf-pcachedrop` の open ごとの invalidate は約 10%。
5. **変更の前後で同じ行を同じ条件で測り、割合も取り直す**。割合が動かなければ効いていない → 変更を戻し (`git checkout`)、**何を試して何が変わらなかったか**を backlog に残す (`journal-record`)。

## 落とし穴

- `perf c2c` は KVM ゲスト (6 vCPU) で `memory events not supported`。cacheline の取り合いはコードの変種を作って A/B で切り分ける。
- perf / bpftrace 実行中は他の重い処理を流さない。`/tmp` は再起動で消えるので出力は scratch に置く。
- 結論に推測を書かない。切り分けられなければ「未切り分け」と書く。
