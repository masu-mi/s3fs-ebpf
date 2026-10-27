# CLAUDE.md

## セッション開始時
- kmod の read/pcache/eviction 周りに触れるときは、最初に `docs/kmod-pcache-invariants.md` だけを読む（ロック順、R1、pending のキー、chunk の pin、reset の条件）。コードから読み直さない。
- 不変条件を変えるコード変更をしたら、同じ変更でこのメモも更新する。

## kmod/vfs_ops.c の読み方
- `kmod/vfs_ops.c`（約 4000 行）は **Read しない**（offset/limit なしの全文 Read は禁止）。
- 関数単位で読む: `scripts/fn.sh <func> [<func>...]`（行番号付き、`-c` で直前のコメントも出す）。
- 関数を探す: `scripts/fn.sh -l [PATTERN]` は `name start end` を出力する。シンボル以外は `grep -n PATTERN kmod/vfs_ops.c` で探す。
- 関数外の範囲（struct 定義、マクロ、ファイル先頭のコメントなど）は `sed -n 'A,Bp' kmod/vfs_ops.c` で必要な行だけ読む。
- 編集前には Edit の要件として Read が必要なので、その場合も `offset`/`limit` で対象範囲だけを Read する。
