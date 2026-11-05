# CLAUDE.md

## セッション開始時
- kmod の read/pcache/eviction 周りに触れるときは、最初に `docs/kmod-pcache-invariants.md` だけを読む（ロック順、R1、pending のキー、chunk の pin、reset の条件）。コードから読み直さない。
- 不変条件を変えるコード変更をしたら、同じ変更でこのメモも更新する。

## 設計・既存実装の調査
- 設計するとき、既存の実装を調べるときは、状態を持つもの (kmod / daemon / S3 / page cache) と更新者を並べ、**状態遷移図 (mermaid、`docs/design/`) を書いて、全部の状態からの遷移を検証する**。非同期・並行・持ち主が複数の遷移は、TLA+ のモデル (`specs/tla/`) にして `make test-spec` で網羅する。反例は `scripts/test-parity.sh` で実機に当てる。手順と理由: `docs/dev-notes/design-verification.md`、適用例: `docs/design/file-lifecycle-state-machine.md`。
- 速度比を書く前に、S3 への HEAD / GET と upcall の数を双方で数える (`.claude/skills/perf-bench-verify/SKILL.md` の 4)。

## kmod/vfs_ops.c の読み方
- `kmod/vfs_ops.c` は include と `#include "vfs/*.inc.c"` を並べるだけの薄い入口。実装は `kmod/vfs/*.inc.c` に関心ごと別に分かれている（全体で約 5800 行、1 つの翻訳単位。static な状態を共有するので並べる順序に意味がある）。
  - `entries` エントリ表 / `admit_table` admit 表・policy hook / `capacity_evict` 容量 evict・`admit_replace` / `read_miss` pending と upcall / `attr_override` / `fd_chunks` inode の chunk 表 / `hit_accum` / `fd_reset` / `flush_queue` / `delete_rename_queue` / `dirty_populate` / `file_ops` write・setattr / `aops_read` read_folio・open / `dir_super_ops`
- 全部を通して Read しない（offset/limit なしの全文 Read は禁止）。
- 関数単位で読む: `scripts/fn.sh <func> [<func>...]`（行番号付き、`-c` で直前のコメントも出す。`vfs_ops.c` と `vfs/*.inc.c` を全部探す）。
- 関数を探す: `scripts/fn.sh -l [PATTERN]` は `name start end file` を出力する。シンボル以外は `grep -n PATTERN kmod/vfs_ops.c kmod/vfs/*.inc.c` で探す。
- 関数外の範囲（struct 定義、マクロ、ファイル先頭のコメントなど）は `sed -n 'A,Bp' <file>` で必要な行だけ読む。
- 編集前には Edit の要件として Read が必要なので、その場合も `offset`/`limit` で対象範囲だけを Read する。
