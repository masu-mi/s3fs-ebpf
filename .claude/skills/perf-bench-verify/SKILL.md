---
name: perf-bench-verify
description: s3fs-ebpf の性能試験 (A〜H と層ごとのレイテンシ) を実行し、Grafana で可視化し、benchmark-results.md に記録したうえで、結果に不審な点がないか検証し、バグや調査課題を backlog に起票する手順。「性能を測り直して」「ベンチを回して」「Grafana に出して」「計測結果を検証して」「backlog に起票して」と頼まれたとき、または性能に影響する変更 (kmod・daemon の read / write / flush・policy) のあとで使う。
---

# 性能試験 → 可視化 → 記録 → 検証 → backlog

各試験の中身と、何の側面を測っているかは `docs/dev-notes/bench-suite-overview.md`。まずそれを読む (A〜H と層ごとのレイテンシの表がある)。この skill は**手順と、過去に実際に踏んだ落とし穴**を書く。

## 0. 始める前に (環境の衛生、毎回)

測定の前に全部確かめる。1 つでも外れたら測らない。外れた状態で取った値は、後から原因を切り分けられず、全部やり直しになった (issue #35)。

1. **MinIO に `mc admin trace` が残っていない**: `docker top s3fs-ebpf-minio -o pid,args | grep "mc admin trace"`。`docker exec` のクライアントを kill してもコンテナ内の `mc` は止まらない。残っていると MinIO が全 request を trace 購読者へ流し、S3 を伴う項目が 25〜75% 遅くなり、`test-policy-conformance` が落ちる。止め方は `sudo kill <host の pid>` (`pkill -f` は自分のシェルを殺すので使わない)。`scripts/lib/bench.sh` の `minio_require` が検出して止まる。
2. **ディスクの空きが 20% 以上**: `df -h /`。MinIO は空きが少ないと `Storage backend has reached its minimum free drive threshold` で書き込みを拒否する (D の 300 MiB の seed で出た)。B / C / D のあとは `mc rb --force local/<bucket>` で消す。
3. 動いている bench / daemon / fio / hyperfine がない (`pgrep -a "fastpath|fio|hyperfine"`)、`/mnt/s3demo` と `/mnt/s3fuse-bench` が mount されていない、`loadavg` が低い。
4. kmod の module param が既定: `nocache=0`、`pcache_drop=0` (rmmod で戻る)。`partial_modify` は daemon が起動のたびに書く。
5. build が今の作業ツリー: `make all` (clang-20、`scripts/doctor.sh` が全 OK)。`git rev-parse HEAD` と `git status` を記録する (未 commit なら「HEAD + 作業ツリー」と書く)。
6. **測定中に `scripts/` と `scripts/lib/` を編集しない**: bash はスクリプトを読みながら実行する。編集するなら `.new` に書いて `mv` し、`bash -n` を通す。測定が使う `bin/s3fs-ebpf` も作り直さない。

## 1. 実行

```bash
# A〜E (6 構成): bench-pcache-remeasure.sh。STEPS / BACKENDS / RUNS_W / RUNS_COLD / E_REPS で絞る
OUT=/tmp/r/main STEPS="A B C D" scripts/bench-pcache-remeasure.sh
OUT=/tmp/r/main-E STEPS=E E_REPS=5 scripts/bench-pcache-remeasure.sh
OUT=/tmp/r/main-F scripts/bench-read-64jobs.sh                       # F (hello.txt が s3fs-test に要る)
OUT=/tmp/r/main-Gc REPS=5 scripts/bench-oltp-vector-cold.sh          # G cold
OUT=/tmp/r/main-Gw REPS=5 scripts/bench-oltp-vector-warm.sh          # G warm (ov_kv.db / ov_vec.bin は cold が seed する)
OUT=/tmp/r/layer TARGET_SEC=12 scripts/bench-layer-latency.sh        # 層ごとのレイテンシ + Grafana の annotation
# H: 5 seed × 2 モード。bench-hitrate.sh は s3fs-test を作り直す (F / G の object が消える。H は最後)
N_FILES=30 N_ACCESS=2000 ZIPF_ALPHA=0.7 SOFTCAP=8 SEED=42 PCACHE_DROP=1 DAEMON_EXTRA_FLAGS=--arena-metadata-only \
  POLICIES="observe_ops lru_pick_ops sieve_pick_ops s3fifo_pick_ops wtinylfu_pick_ops" OUT=/tmp/r/H scripts/bench-hitrate.sh
```

- 全体は 2〜3 時間 (E が約 20 分、H が約 40 分)。`nohup` の連続実行スクリプトにして、各段の前後で 0. の 1 と 2 を確かめる (`/tmp/s35/chain2.sh` が例。git には入れていない)。
- 長い run は `run_in_background` で起動し、終了通知を待つ。起動用シェルの終了通知は本体の終了ではない (`pgrep` と出力ファイルで確かめる)。
- 構成の起動は `scripts/lib/bench.sh` の `bench_up <backend> <log> [flags]` / `bench_down`。6 構成は `BENCH_BACKENDS_ALL`。層 3 用に `ebpf-nocache-diskcache` もある。

## 2. 可視化 (Grafana)

- `scripts/grafana-layer-latency.sh <layer-latency.csv>`: ダッシュボード "S3FS-eBPF Layer latency" (`http://localhost:3000/d/s3fs-ebpf-layer-latency`、admin:admin) を取り込む。棒グラフと表は CSV を埋め込み、時系列は Prometheus (daemon の `:9600` を 5 秒間隔で scrape)。bench が各 run を annotation (`layer-latency` タグ) で投稿する。
- Grafana の bind mount は**メインのチェックアウト**の `deploy/` を指す。worktree で `deploy/grafana/dashboards/` に置いても読み込まれないので API で取り込む。
- 見た目の確認: ブラウザがなければ `npm i playwright && npx playwright install chromium` を**スクラッチのディレクトリ**で行い (`/tmp/<scratch>/pw`)、`Authorization: Basic` ヘッダ付きでスクリーンショットを撮る。ブラウザがなくても、`POST /api/ds/query` で各パネルのクエリがデータを返すことは確かめられる。
- daemon の histogram は `s3fs_ebpf_read_miss_latency_seconds` (50 µs〜10 s)。MinIO の TTFB histogram は最小 bucket が 50 ms でローカルの GET を分解できないので、図にしない。

## 3. 記録

1. 生データを `docs/journal/data/<名前>-<日付>/` に置く (`A.txt` `B.txt` … `*.json` `results.jsonl` `summary.txt` `environment.txt`、スクリーンショット)。`docs/INDEX.md` に 1 行足す。
2. `docs/journal/benchmark-results.md` の次の節 (`### 18.NN`) に、**条件** (日付、`uname -r`、commit、各構成の起動オプションと module param、n、cold の手順)、**値** (中央値 + 四分位範囲 + 最小・最大)、**比** (「s3fs-fuse の時間 ÷ s3fs-ebpf の時間」。1 超が ebpf の勝ち。IOPS や ops/s は逆数にする) を書く。表は構成ごとの絶対値と 3 組の比 (fuse-raw/ebpf-nocache = 構造の差、fuse-diskcache/ebpf-pcachedrop = 独自キャッシュの差、fuse-cache/ebpf-pcache = 全体)。
3. 以前の値と ±20% 以上違う項目は、**理由 + 根拠**か「未切り分け」と書く。推測を理由として書かない。
4. 発表の資料 (`docs/user/meetup-ebpf-outline.md`) と `docs/design/where-fs-layer-can-win.md` に新しい値を反映する (出典の節番号つきで)。

## 4. 検証 (不審な点の洗い出し)

数字を書く前に、次を全部見る。

- **正しさの検査が全部 PASS か**: D の sha256 (全 object)、B の verify (全 object)、E の cat の sha256、`dd` の rc、`WARN ... never completed`。B は fsync の直後に HEAD で `Content-Length` を見る (`wait_object`)。fsync が PUT の完了前に返ると「早く返った回数」が出る。0 でなければバグ。
- **対照項目が前回と合うか**: page cache だけで終わる項目 (A の ebpf-pcache ≈ 3 µs、F、G の warm) は前回と ±5% で合うはず。**S3 を伴う項目だけが 25% 以上ずれ、対照が動かないときは、コードではなくホスト / MinIO の異常を疑う** (0. の 1 と 2 を再確認)。fuse 側と ebpf 側が同じ向きに動いているなら環境、片側だけなら変更のせい。
- **MinIO の GET 数はキャッシュされる**: cluster metrics は数秒キャッシュされるので、run の直後に取った増分は次の行にずれる (`bench-layer-latency.sh` は snapshot の前後に `MINIO_METRICS_SETTLE`=12 秒待つ)。待たない値は「桁が合うか」の確認にだけ使う。
- **層の判定が counter と合うか**: upcall 数 (`s3fs_ebpf_read_miss_upcalls_total`)、kmod の folio hit、MinIO の `minio_s3_requests_total{api="getobject"}` の増分が、その構成が狙った層と合う (ebpf-pcachedrop は upcall 0、disk cache の層は GET が約 1 回、fuse-diskcache は GET 0)。合わないなら構成の作り方が違う。
- **行ごとに counter を見る (集計だけで済ませない)**: 層ごとのレイテンシで、層 3 (disk cache) の 64 MiB の行だけ S3 GET が 288 upcall に対して 1136 回あり、daemon の disk cache が 4 MiB を超える object に効かないことが分かった (backlog AC-75)。「64 MiB で disk cache と S3 が同じ時間」という数字だけを見て、取り込みが支配的と誤読していた。数字が不自然に揃っているときは、その行の counter を疑う。
- **疑わしい run の結論は書かずに取り直す**: 環境が異常な run (残った trace など) から出た結論 (「page cache があると policy の順位が崩れる」) は、取り直すと逆だった。環境の異常を見つけたら、その間の値と結論をすべて無効として測り直す。
- **一時的な揺れは再測定で確かめる**: C の ebpf の一部のセルだけが 3〜4 倍遅かった (他の構成と run 内で比べると外れ) が、同じ試験をもう 1〜3 回流すと戻った。1 run の外れ値に原因を作らず、再測定の値を並べて「一時的」と書く (原因が分からなければ未切り分け)。
- **kmod の folio hit は sample 間隔遅れ**: daemon の `--sample-every` (既定 60s) ごとにしか反映されない。層の判定には upcall 数と S3 GET 数を使う。
- **ばらつき**: 四分位範囲と最小・最大を見る。最大だけ飛んでいるのは外れ値、四分位範囲が広いのは不安定 (D の 300 MiB は既知)。n が小さい項目は結論を強く書かない。
- **カナリア**: `scripts/test-policy-conformance.sh` はタイミングに敏感で、ホストが遅いと live hits が simulator から 3〜12 pt ずれる。測定の前後で通ることを確かめると、環境が変わっていないことの確認になる。
- **回帰試験**: `make test-regression` が全 PASS (所要 約 25 分)。KUnit は `make test-kunit KSRC=<linux-6.12 の tree>` (UML) と `KUNIT_ARGS="--arch=x86_64 --qemu_args='-smp 4'"` (SMP)。
- **ログ**: `dmesg` に新しい warning / BUG が出ていないか、daemon のログに `put: ` `read dirty:` `mark dirty:` などのエラーがないか。

## 5. バグ・調査課題の起票

不審な点が実際のバグか調査課題だったら、`docs/journal/backlog.md` の `## AC.` の表に 1 行足す (列は `| # | 内容 | 優先度 | 状態 |`、ID は末尾の次の番号)。

- **内容**: 症状 (どの試験のどの値か)、再現手順、原因が分かっていれば原因、根拠の節 (`br §18.NN`)。直したものは `✅ 修正` と回帰テストの名前、直していないものは `⏳ 未着手` と「何を確かめるか」。
- **回帰テストを足す**: 修正前のコードで FAIL することを確かめる。`git worktree add --detach /tmp/<scratch>/base <commit>` で修正前をビルドし、同じスクリプトを走らせる (終わったら `git worktree remove --force`)。
- 実機のテストは `scripts/test-*.sh` + `Makefile` の `test-*` と `test-regression` の依存。daemon だけなら Go の単体テスト (`-race`)。kmod の不変条件を変えたら `docs/kmod-pcache-invariants.md` も同じ変更で更新する (`CLAUDE.md`)。
- 原因が不明な差は、推測で埋めずに「未切り分け」と書いて起票する。

## 6. 後片付け

- MinIO の scratch bucket (`s35-*` など自分が作ったもの) と、`/tmp` の大きい cache dir / payload を消す。既存の bucket (`s3fs-test` `s3fs-ab-exact` …) は消さない。
- スクラッチに入れた道具 (playwright など) と、`docker exec` で起動したものが残っていないか確かめる。
- `go build` は repo のルートか `daemon/` で行う (`daemon/cmd/s3fs-ebpf` の中で行うと 60 MB のバイナリが出て commit に混ざる)。
