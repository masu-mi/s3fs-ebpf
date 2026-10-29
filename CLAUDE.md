# CLAUDE.md

## セッション開始時
- kmod の read/pcache/eviction 周りに触れるときは、最初に `docs/kmod-pcache-invariants.md` だけを読む（ロック順、R1、pending のキー、chunk の pin、reset の条件）。コードから読み直さない。
- 不変条件を変えるコード変更をしたら、同じ変更でこのメモも更新する。

## kmod/vfs_ops.c の読み方
- `kmod/vfs_ops.c` は include と `#include "vfs/*.inc.c"` を並べるだけの薄い入口。実装は `kmod/vfs/*.inc.c` に関心ごと別に分かれている（全体で約 5800 行、1 つの翻訳単位。static な状態を共有するので並べる順序に意味がある）。
  - `entries` エントリ表 / `admit_table` admit 表・policy hook / `capacity_evict` 容量 evict・`admit_replace` / `read_miss` pending と upcall / `attr_override` / `fd_chunks` inode の chunk 表 / `hit_accum` / `fd_reset` / `flush_queue` / `delete_rename_queue` / `dirty_populate` / `file_ops` write・setattr / `aops_read` read_folio・open / `dir_super_ops`
- 全部を通して Read しない（offset/limit なしの全文 Read は禁止）。
- 関数単位で読む: `scripts/fn.sh <func> [<func>...]`（行番号付き、`-c` で直前のコメントも出す。`vfs_ops.c` と `vfs/*.inc.c` を全部探す）。
- 関数を探す: `scripts/fn.sh -l [PATTERN]` は `name start end file` を出力する。シンボル以外は `grep -n PATTERN kmod/vfs_ops.c kmod/vfs/*.inc.c` で探す。
- 関数外の範囲（struct 定義、マクロ、ファイル先頭のコメントなど）は `sed -n 'A,Bp' <file>` で必要な行だけ読む。
- 編集前には Edit の要件として Read が必要なので、その場合も `offset`/`limit` で対象範囲だけを Read する。
