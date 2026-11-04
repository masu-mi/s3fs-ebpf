---
name: kmod-change-verify
description: kmod (kmod/vfs/*.inc.c, kmod/*.c) を変えて実機で試す前後の手順。ビルド、ロード、dmesg の確認、性能の測り直し、make test-regression / test-lockdep、invariants メモの更新、oops したときの復旧。「kmod を変えた」「read_folio / readahead / folio / page cache の変更」「実機で試して」「oops した」「rmmod できない」と言われたとき、または kmod の read / write / evict 経路を触るときに使う。
---

# kmod の変更 → 実機で検証

kmod の誤りはカーネルの oops になる。**oops すると、そのプロセスが kmod の参照を持ったまま死に、`rmmod` も `rmmod -f` も効かず、再起動以外に回復手段がない** (AC-76 で実際に起きた。再起動が効かずホストを止める羽目になった)。だから、実機に載せる前に落とし穴を確認し、載せたあとは段階を踏む。

## 1. 載せる前

- `docs/kmod-pcache-invariants.md` を読む (CLAUDE.md の指示)。ロック順は `fd->lock` → `s3fs_admits_lock`。不変条件を変えるなら同じ変更でこのメモを更新する。
- 関数は `scripts/fn.sh <func>` で引く。`vfs_ops.c` を全文 Read しない。
- folio / page cache API の落とし穴:
  - `readahead_count()` と `readahead_pos()` は、直前に取った folio の分だけ**遅れる**。覗き見に使わない。`while ((folio = readahead_folio(rac)))` で取り、位置は `folio_pos(folio)`。覗き見の版は NULL の folio を触って oops した。取った folio は成功でも失敗でも `folio_unlock` する (not-uptodate のまま unlock すれば、残りは core が unlock し、reader が `.read_folio` を呼ぶ)。
  - `kmap_local_folio(folio, 0)` は 1 ページしか map しない。large folio では `memcpy_to_folio` / `folio_zero_range` を使う。
  - `file->f_ra.ra_pages` は `->open` のあとの `file_ra_state_init` で上書きされる。readahead の窓は sb の bdi (`super_setup_bdi` → `sb->s_bdi->ra_pages`) で決める。専用の bdi が無い sb は `ra_pages == 0` で、readahead が一度も呼ばれない。
  - hit の計上 (`s3fs_folio_hits`、`s3fs_hit_signal_direct`) は folio 単位。large folio では 1 hit が複数ページ分になる。

## 2. 載せる

1. `make kmod` (`warning: the compiler differs` は無視してよい。`error` は見る)。
2. 再起動後は docker のコンテナが止まっている: `make compose-up`。
3. まず軽い run: `LAYERS="ebpf-pcache ebpf-pcachedrop" SIZES="4MiB 64MiB" TARGET_SEC=8 MINIO_METRICS_SETTLE=2 OUT=<scratch>/x timeout 420 scripts/bench-layer-latency.sh`。**リポジトリのルートから実行する** (bpf の相対パスが解決できず daemon が起動しない)。`timeout` を付ける (kmod が詰まるとベンチが無限に待つ)。
4. 毎回 `sudo dmesg | grep -E "BUG|Oops|WARNING|lockdep"` が 0 件。1 件でも出たらそれ以上走らせない。

## 3. 通す

- `make test-regression` (約 25 分、28 件が PASS)。背景で走らせ、`<scratch>/reg.log` の `EXIT=` と `status=PASS` の数を見る。終了通知は起動用シェルのもので、本体の終了ではない (`grep EXIT=` で確かめる)。
- `make test-lockdep` は lockdep 付きカーネル (`make setup-kernel-lockdep` で 20〜40 分の build、再起動) が要る。ロック順を触ったら必須。触っていなければ PR に「未実施」と明記する。`docs/dev-notes/lockdep-kernel.md`。
- 性能を言うなら `perf-bench-verify` の手順 (環境の衛生、対照項目、行ごとの counter)。

## 4. oops したら

1. `sudo dmesg | grep -n -E "BUG|Oops|RIP: 0010|Call Trace"` の最初の oops を見る。`RIP: 0010:<関数>+0x..` を `addr2line -e kmod/s3fs_kfunc.ko -f -i <関数の先頭+offset>` (先頭は `nm`) で行に落とす。
2. 後続のログ ("scheduling while atomic"、"Fixing recursive fault but reboot is needed") は連鎖。最初の 1 件だけ読む。
3. ベンチ、daemon、`sudo cat` を kill し、`umount -l`、`rmmod`。`Module is in use` / `Resource temporarily unavailable` なら**再起動する**。再起動できなければホストを止める。ここで時間を使わない。
4. 再起動後: `make compose-up`、`lsmod | grep s3fs` が空、`git status`。

## 5. 終わり

- 試して効かなかった変更は戻す (`git checkout <file>`) と、**何を試して何が変わらなかったか**を backlog の該当行に残す (「readahead だけでは速くならなかった」が次の判断の根拠になった)。
- 速そうな変更でも、`perf record -a -g` で割合を見てから効くか判断する。方法は `perf-bench-verify` の「遅い行の原因を切り分ける」。
- `docs/kmod-pcache-invariants.md` と `docs/journal/backlog.md` を更新してから PR にする。
