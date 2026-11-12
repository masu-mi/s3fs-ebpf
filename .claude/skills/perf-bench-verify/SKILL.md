---
name: perf-bench-verify
description: 性能試験 (A〜H・層ごとのレイテンシ) の実行・記録・結果の検証。「ベンチを回して」「性能を測り直して」「計測結果を検証」で使う。性能に効く変更のあとにも。
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
7. **測定中に他の重い処理を流さない**: `go test ./...` (`internal/s3` は約 20 秒)、`make`、別の bench、`docker` の大きい操作。流した run の値は「環境が揺れた」と書くことになり、後から切り分けられない (§18.50 の seed 43)。
8. **新しい script は本番の前に短く 1 回流す**: ログや出力のパスの間違いは、値が 0 や空で返ってから気づくことになる (GET の数え直しで、ログのパスの誤りが状態を壊した)。`bash -n` では見つからない。
9. **終了を待つときは `pgrep -f` を使わない**: パターンが待っているシェル自身の command line に一致して終わらない。完了ファイル (`DONE`) か保存した PID で待つ。バックグラウンドの待機は 30 分で止まるので、長い run は完了ファイルを書く script にする。
10. **ディスクが止まっていない**: `scripts/disk-stall-probe.sh` (100 MiB の O_DIRECT 書き込みを 20 回、約 2 秒。0 で健全、1 で停止または遅い)。この KVM VM の disk (`QEMU HARDDISK`) は、MinIO も fs も介さない純粋な書き込みでも、数秒〜十数秒止まり、数分間ずっと遅いこともある (健全な中央値は 100 MiB で約 70 ms、遅いときは 270〜1200 ms。`br §18.64`)。書き込みを伴う項目 (B / C / D / G の seed / 層ごとのレイテンシの層 3) は、**測定の前後に probe を流し、1 が出たら間の run を捨てる**。fuse でも ebpf でも、基準でも新でも同じように遅くなるので、対照 (fuse-cache) が動いたら、まず disk を疑う。揺れを fs や MinIO の挙動と書かない。

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
- 見た目の確認: `isolated-browser` skill を使う (隔離した使い捨てコンテナの最新 Chrome。ホストに playwright / ブラウザを入れない)。`.claude/skills/isolated-browser/browse.sh -a admin:admin -o /tmp/<scratch>/shots http://localhost:3000/d/s3fs-ebpf-layer-latency`。コンテナランタイムが使えなければ撮らず、`POST /api/ds/query` で各パネルのクエリがデータを返すことだけ確かめる。
- daemon の histogram は `s3fs_ebpf_read_miss_latency_seconds` (50 µs〜10 s)。MinIO の TTFB histogram は最小 bucket が 50 ms でローカルの GET を分解できないので、図にしない。

## 3. 記録

1. 生データを `docs/journal/data/<名前>-<日付>/` に置く (`A.txt` `B.txt` … `*.json` `results.jsonl` `summary.txt` `environment.txt`、スクリーンショット)。`docs/INDEX.md` に 1 行足す。
2. `docs/journal/benchmark-results.md` の次の節 (`### 18.NN`) に、**条件** (日付、`uname -r`、commit、各構成の起動オプションと module param、n、cold の手順)、**値** (中央値 + 四分位範囲 + 最小・最大)、**比** (「s3fs-fuse の時間 ÷ s3fs-ebpf の時間」。1 超が ebpf の勝ち。IOPS や ops/s は逆数にする) を書く。表は構成ごとの絶対値と 3 組の比 (fuse-raw/ebpf-nocache = 構造の差、fuse-diskcache/ebpf-pcachedrop = 独自キャッシュの差、fuse-cache/ebpf-pcache = 全体)。
3. 以前の値と ±20% 以上違う項目は、**理由 + 根拠**か「未切り分け」と書く。推測を理由として書かない。
4. `docs/design/where-fs-layer-can-win.md` と `docs/user/meetup-qa.md` に新しい値を反映する (出典の節番号つきで)。

## 4. 検証 (不審な点の洗い出し)

数字を書く前に、次を全部見る。

- **同じ仕事を比べているか (比を書く前に必ず)**: 速度比 (「構造差 3.2×」など) は、双方が同じ仕事をしたときだけ意味がある。silo の `minio_s3_requests_total{api="getobject"}` と `{api="headobject"}`、daemon の `s3fs_ebpf_read_miss_upcalls_total` を、**構成ごとに同じ N 回の操作で数えて表にする** (cluster metrics は 12 秒の settle が要る)。片方が 0 でもう片方が 1 往復/操作なら、速い理由は設計ではなく省いた仕事かもしれない。経験: s3fs-fuse は open ごとに HEAD する (1000 回で HEAD 1002、キャッシュありでも 1001) が、s3fs-ebpf は entry table だけで open を完了していた。3.2× の約半分はこの HEAD の分で、HEAD を払うと 1.72× になった (`br §18.51`)。GET だけ数えていたので見えなかった。
- **振る舞いが同じか**: 速度比を出す構成の組は、`make test-parity` (s3fs-fuse と s3fs-ebpf に同じ操作列を流し、結果か errno を比べる) の既知の差 (`scripts/lib/parity-known-diff.sh`) を確認する。**既知の差が速度に効く操作 (open の存在確認、rename、外部の更新の検知) は、その差を書かずに比を headline にしない**。新しい比較の操作を足したら、`test-parity.sh` にも同じ操作を足す。
- **不自然に大きい比は設計漏れを疑う**: 2× を超える「構造差」は、まず「相手がしていて自分がしていない仕事」を探す (HEAD、flush、fsync、権限・存在・更新の確認)。
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

## 4b. 遅い行の原因を切り分ける (perf / bpftrace)

(独立した手順は `perf-root-cause` skill。記録は `journal-record`。)

層ごとのレイテンシなどで「ある層が想定より遅い」とき、**変更を入れる前に**、時間の内訳を取る。推測で入れた変更 (AC-76 の readahead) は、`.read_folio` の呼び出しを 0 にしても時間が変わらなかった。

1. 対象の行を長めに走らせる (`LAYERS=<層> SIZES=<サイズ> TARGET_SEC=40`)。リポジトリのルートから、`timeout` を付けて実行する。
2. 走行中に `perf record -a -g` で採り、`perf report --children --sort sym -g none` で自分のコードと core の割合を並べる。`bpftrace` では、その関数が呼ばれた回数と状態を確かめる (「1 回も呼ばれていない」の確認にも使う)。perf のパスなどの環境の癖は `docs/dev-notes/ac75-ac76-lessons.md` の「環境の落とし穴」。
3. ベンチの仕組みが数字に入っていないか確かめる (`ebpf-pcachedrop` の open ごとの invalidate は約 10%)。
4. 変更の前後で同じ行を同じ条件で測り、割合も取り直す。割合が動かなければ効いていない。

AC-75 / AC-76 で分かったこと: `docs/dev-notes/ac75-ac76-lessons.md`。

## 5. バグ・調査課題の起票

不審な点が実際のバグか調査課題だったら、`docs/journal/backlog.md` の `## AC.` の表に 1 行足す (列は `| # | 内容 | 優先度 | 状態 |`、ID は末尾の次の番号)。

- **内容**: 症状 (どの試験のどの値か)、再現手順、原因が分かっていれば原因、根拠の節 (`br §18.NN`)。直したものは `✅ 修正` と回帰テストの名前、直していないものは `⏳ 未着手` と「何を確かめるか」。
- **回帰テストを足す**: 修正前のコードで FAIL することを確かめる。`git worktree add --detach /tmp/<scratch>/base <commit>` で修正前をビルドし、同じスクリプトを走らせる (終わったら `git worktree remove --force`)。
- 実機のテストは `scripts/test-*.sh` + `Makefile` の `test-*` と `test-regression` の依存。daemon だけなら Go の単体テスト (`-race`)。kmod の不変条件を変えたら `docs/kmod-pcache-invariants.md` も同じ変更で更新する (`CLAUDE.md`)。
- 原因が不明な差は、推測で埋めずに「未切り分け」と書いて起票する。

## 6. 後片付け

- MinIO の scratch bucket (`s35-*` など自分が作ったもの) と、`/tmp` の大きい cache dir / payload を消す。既存の bucket (`s3fs-test` `s3fs-ab-exact` …) は消さない。
- `isolated-browser` の「後片付けの確認」を実行する。`docker exec` で起動したものが残っていないかも確かめる。
- `go build` は repo のルートか `daemon/` で行う (`daemon/cmd/s3fs-ebpf` の中で行うと 60 MB のバイナリが出て commit に混ざる)。
