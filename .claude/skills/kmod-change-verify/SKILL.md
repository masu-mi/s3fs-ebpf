---
name: kmod-change-verify
description: kmod (kmod/vfs/*.inc.c, kmod/*.c) を変えて実機で試す前後の手順。ビルド、ロード、dmesg の確認、性能の測り直し、make test-regression / test-lockdep、invariants メモの更新、oops したときの復旧。「kmod を変えた」「read_folio / readahead / folio / page cache の変更」「実機で試して」「oops した」「rmmod できない」と言われたとき、または kmod の read / write / evict 経路を触るときに使う。
---

# kmod の変更 → 実機で検証

kmod の誤りはカーネルの oops になる。**oops すると、そのプロセスが kmod の参照を持ったまま死に、`rmmod` も `rmmod -f` も効かず、再起動以外に回復手段がない** (AC-76 で実際に起きた。再起動が効かずホストを止める羽目になった)。だから、実機に載せる前に落とし穴を確認し、載せたあとは段階を踏む。

## 1. 載せる前

- `docs/kmod-pcache-invariants.md` を読む (CLAUDE.md の指示)。ロック順は `fd->lock` → `s3fs_admits_lock`。不変条件を変えるなら同じ変更でこのメモを更新する。
- flush / rename / sync のように**非同期・複数の持ち主**の状態を触るなら、コードの前に `tlc-design-verification` (状態遷移図 → TLA+)。実装後に実機で見つけるより安い (AC-84 / AC-85)。
- 関数は `scripts/fn.sh <func>` で引く。`vfs_ops.c` を全文 Read しない。
- folio / readahead / page cache を触るなら `docs/dev-notes/ac75-ac76-lessons.md` の「AC-76」の知見と、`docs/kmod-pcache-invariants.md` の「読み取りの readahead と large folio」を読む (`readahead_*` の遅れ、`kmap_local_folio` の範囲、`ra_pages` は bdi で決めること、hit の計上単位)。

## 2. 載せる

1. `make kmod` (`warning: the compiler differs` は無視してよい。`error` は見る)。
2. 再起動後は docker のコンテナが止まっている: `make compose-up`。
3. まず軽い run: `LAYERS="ebpf-pcache ebpf-pcachedrop" SIZES="4MiB 64MiB" TARGET_SEC=8 MINIO_METRICS_SETTLE=2 OUT=<scratch>/x timeout 420 scripts/bench-layer-latency.sh`。**リポジトリのルートから実行する** (bpf の相対パスが解決できず daemon が起動しない)。`timeout` を付ける (kmod が詰まるとベンチが無限に待つ)。
4. 毎回 `sudo dmesg | grep -E "BUG|Oops|WARNING|lockdep"` が 0 件。1 件でも出たらそれ以上走らせない。

## 3. 通す

- `make test-regression` (約 25 分、30 件が PASS。`test-parity` と `test-open-check` を含む。`test-spec` は含まない: ネットワークとコンテナが要るので、手元では `make test-spec`、CI は別ジョブ)。背景で走らせ、`<scratch>/reg.log` の `EXIT=` と `status=PASS` の数を見る。終了通知は起動用シェルのもので、本体の終了ではない (`grep EXIT=` で確かめる)。
- **KUnit** (`make test-kunit KSRC=<linux-6.12 の tree>`、UML、lockdep + KASAN 付き、約 80 秒): kmod の**新しいデータ構造・分岐にはテストを足す**。実機の回帰は粗いので、tombstone の消去条件のような境界は KUnit で書く。足したら**変異で確かめる** (条件を 1 つ外して、そのテストだけが落ちること。確認後は戻す)。tree は `curl -O https://cdn.kernel.org/pub/linux/kernel/v6.x/linux-6.12.tar.xz` で取り、**sha256 を `.../v6.x/sha256sums.asc` の行と照合**してから専用ディレクトリに解く (`docs/dev-notes/kunit.md`)。KUnit の lockdep は、実機の `test-lockdep` の代わりにならない (UML は 1 CPU、実カーネルの経路ではない)。
- **ABI を変える** (`include/s3fs_ebpf_uapi.h` の構造体にフィールドを足す) と、ioctl 番号と `sizeof` が変わる。`daemon/internal/kmodio` の構造体、`kmodio_test.go` の期待値 (`sizeof` と ioctl 番号。C の `sizeof` は手計算で確かめる)、kmod と daemon の同時 build。古い daemon と新しい kmod の組み合わせは動かない。
- `make test-parity` は s3fs-fuse との差分テスト。kmod の見え方 (lookup / readdir / rename / unlink) を変えたら、`scripts/lib/parity-known-diff.sh` の既知の差が減ったか増えたかを見る。修正前のコードで新しい操作が FAIL することも確かめる (`git worktree add --detach <scratch>/base <commit>` で修正前をビルド)。
- `make test-lockdep` は lockdep 付きカーネル (`make setup-kernel-lockdep` で 20〜40 分の build、再起動) が要る。ロック順を触ったら必須。触っていなければ PR に「未実施」と明記する。`docs/dev-notes/lockdep-kernel.md`。
- 性能を言うなら `perf-bench-verify` の手順 (環境の衛生、対照項目、行ごとの counter)。

## 4. oops したら

1. 最初の oops の `RIP: 0010:<関数>+0x..` を `addr2line` で行に落とす (`docs/dev-notes/ac75-ac76-lessons.md` の「環境の落とし穴」)。後続のログは連鎖なので読まない。
2. ベンチ、daemon、`sudo cat` を kill し、`umount -l`、`rmmod`。`Module is in use` / `Resource temporarily unavailable` なら**再起動する**。再起動できなければホストを止める。ここで時間を使わない。
3. 再起動後: `make compose-up`、`lsmod | grep s3fs` が空、`git status`。

## 5. 終わり

- 試して効かなかった変更は戻す (`git checkout <file>`) と、**何を試して何が変わらなかったか**を backlog の該当行に残す (「readahead だけでは速くならなかった」が次の判断の根拠になった)。
- 速そうな変更でも、`perf record -a -g` で割合を見てから効くか判断する。方法は `perf-bench-verify` の「遅い行の原因を切り分ける」。
- `docs/kmod-pcache-invariants.md` と `docs/journal/backlog.md` を更新してから PR にする。
