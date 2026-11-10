---
name: regression-test
description: 実機 (insmod した kmod + daemon + MinIO) の回帰試験・検証試験 — make test-regression、個別の test-*、s3fs-fuse との差分テスト (test-parity)、lockdep カーネルでの再実行 (test-lockdep) — を回す手順と、新しい test-*.sh を足す手順。「回帰試験を回して」「検証試験」「test-parity」「test-lockdep」「回帰テストを足して」「修正前で FAIL するか確かめて」「PASS したか」と言われたとき、またはバグ修正・kmod / daemon の振る舞いの変更のあとで使う。
---

# 実機の回帰試験 / 検証試験

kmod と daemon を実際に載せ、MinIO に対して操作を流して、内容 (sha256) と振る舞いを確かめる層。`make test-regression` が全部を束ねる (約 25 分、30 件超、root)。単体テストは `kmod-unit-test` skill。

## 回す前に

1. `make compose-up` (再起動後はコンテナが止まっている)、`make all`、`make doctor`。MinIO の seed (`hello.txt` など) が無いときは memory `project-minio-bench-pitfalls` の即時修復コマンド。
2. 他の bench / daemon / mount が残っていない: `pgrep -a "fastpath|fio|hyperfine|s3fs-ebpf"`、`/mnt/s3demo` が mount されていない。**MinIO に `mc admin trace` が残っていると遅くなり、`test-policy-conformance` が落ちる** (`perf-bench-verify` §0)。
3. 長い run は背景で: `make test-regression > <scratch>/reg.log 2>&1; echo EXIT=$? >> <scratch>/reg.log`。`pkill -f` は自分のシェルも殺すので使わない (`pkill -x` か保存した PID)。

## 主な target

- 全部: `make test-regression`。個別: `make test-<name>` (`make help | grep test-` で一覧と目的)。
- **正しさ** (内容一致): `test-write-readback` (T1)、`test-large-file-read` (T2/Q3、300 MiB)、`test-evict-readback`、`test-pcache-multiread`、`test-write-existing` / `-nopm` / `-capacity` (AC-50)、`test-write-partial` (AC-51、multipart + server-side copy、412 fallback)、`test-fsync-new-file`、`test-fsync-large-new-file`。
- **競合・容量**: `test-pcache-evict-race` (既定 120 s、regression は `-short` の 30 s)、`test-pcache-capacity-*` (AC-41/45)、`test-pick-victim-stale`、`test-policy-reload`。
- **キャッシュ層・先読み**: `test-pcache-prewarm*`、`test-prefetch-next`、`test-staging` (AC-88: 書き込み / 削除 / rename のあとに古い staged copy を返さない)、`test-pcache-drop`、`test-sync-keeps-cache`、`test-splice-hits`。
- **シグナル**: `test-nocache-signal`、`test-cold-signal` (upcall 待ちの signal が -EIO にならない)。
- **s3fs-fuse との差分**: `make test-parity` — 同じ操作列を s3fs-fuse と s3fs-ebpf に流して結果か errno を比べる。既知の差は `scripts/lib/parity-known-diff.sh`。kmod の見え方 (lookup / readdir / rename / unlink / open) を変えたら、既知の差が増えたか減ったかを見る。性能比を出す前にも見る (`perf-bench-verify` §4)。
- **policy の忠実度**: `test-policy-conformance` (live の hit 数を simulator と比べる。タイミングに敏感でカナリアにも使える)。`policy-develop` skill。
- **ロック**: `make test-lockdep` は lockdep 付きカーネルが要る (`make setup-kernel-lockdep`、20〜40 分 + 再起動、`docs/dev-notes/lockdep-kernel.md`)。ロック順を触ったら必須。触っていなければ PR に「未実施」と明記する。
- **TLA+**: `make test-spec` (CI は別ジョブ、regression には含まれない)。`tlc-design-verification` skill。

## 結果の読み方

- 各 script は `PASS` / `FAIL` と終了コードを返す。`test-regression` の失敗は途中で止まる。失敗した個別の target を単独で再実行して切り分ける (再現しなければ環境: ディスク、残った daemon、`dmesg`)。
- 毎回 `sudo dmesg | grep -E "BUG|Oops|WARNING|lockdep"` が 0 件であること。1 件でも出たら先へ進まない (`kmod-change-verify` §4 に oops の復旧手順)。
- 回帰の後は MinIO の `s3fs-test` に 300 MiB の残骸などが残り、ab-pcache 系の bench が EIO になる (memory `project-minio-bench-pitfalls` の 6)。bench の前に掃除する。

## 新しい test-*.sh を足す

1. 既存を手本にする: `scripts/test-staging.sh` (設定、`trap cleanup EXIT`、専用 bucket、daemon 起動、`metric()` で `:9600/metrics` を読む、`bad()` で FAIL を溜めて最後に exit)、`scripts/test-write-readback.sh` (内容検証)。
2. `source scripts/lib/bench.sh` の helper を使う: `need_cmds` `need_built` `ebpf_mount` `ebpf_start_daemon <log> <flags>` `ebpf_stop` `ebpf_nocache` `ebpf_pcache_drop` `ebpf_open_check` `fd_buf_audit` `daemon_kill`。専用 bucket を作って `trap` で `mc rb --force` する。既存の bucket (`s3fs-test` …) を壊さない。
3. `Makefile` に `test-<name>: kmod daemon ## <AC 番号>: <何を確かめるか>` を足し、**`test-regression` の依存にも足す** (足さないと CI / 全体回帰で走らない)。`make cli-reference` で `docs/user/cli-reference.md` を再生成する。
4. **修正前のコードで FAIL することを確かめる**: `git worktree add --detach /tmp/<scratch>/base <修正前 commit>` でビルドし、同じ script を流す。終わったら `git worktree remove --force`。PASS するだけの試験は何も守らない。
5. `shellcheck` は `make lint-scripts`。`bash -n` では出力パスの間違いが見つからない。新しい script は本番の前に 1 回短く流す。
6. 速度ではなく**正しさ**を主に書く。daemon だけで足りるなら Go の単体テスト (`-race`) にする。
7. 起票: 直したバグは `docs/journal/backlog.md` の `## AC.` の表に `✅ 修正` と回帰テスト名を残す (`perf-bench-verify` §5)。
