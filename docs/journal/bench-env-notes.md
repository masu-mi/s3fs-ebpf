# 性能検証環境 — 修正記録と残課題

`make compose-up; make all; make bench` が失敗する報告を受けて Grafana ダッシュボード + bench 起動フローを直したセッションの記録。次に環境を触る人が同じ落とし穴を踏まないよう、**壊れていた原因** と **残っている使いにくさ** の両方をメモとして残す。

対応 commit: `e8d3967 chore(observability): wire runtime metrics + fix Grafana provisioning`、`237acf6 fix(bench): make bench works end-to-end from a clean host`。

---

## 1. 壊れていた 3 レイヤ

### 1.1 Grafana ダッシュボードが空 (`e8d3967`)

3 個の壊れが重なっていた:

- **`metrics.Serve()` がどこからも呼ばれていない** — `daemon/internal/metrics/exporter.go` に HTTP endpoint 実装はあったが `main.go` から起動する行が抜けていて、`:9600` が listen していなかった。Prometheus scrape は最初から失敗。
- **`metrics.Global.FastPathHits/SlowPathMisses` の `Add()` 呼出が 0 件** — 型定義は Week 0 で先に作ったが実際に counter を bump する場所が付いていなかった。endpoint が上がっても値は常に 0 になる。
- **Grafana datasource UID が pin されていない** — `deploy/grafana/provisioning/datasources/prometheus.yaml` が `uid` を指定していなかったので Grafana が起動ごとに `PBFA97CFB590B2093` のような auto UID を割り当てる。dashboard JSON の `"datasource": { "uid": "prometheus" }` と mismatch し、全 panel が "Datasource not found" になる。

### 1.2 `make bench` が動かない (`237acf6`)

- **`make bench` が Week 0/1 skeleton (`bench/runner.sh`) を呼んでいた** — `/mnt/s3data` (実際の mount 先は `/mnt/s3demo`) 前提 + `nrfiles=10000` の事前配置 + bcc install 必須。現行 PoC のどの前提とも噛み合わない。
- **`scripts/bench-latency.sh` は s3fuse を pre-mount 前提** — 手順が Makefile / README のどこにも書かれておらず `"$MP_FUSE not mounted"` で即 exit。
- **手動 lifecycle target 不在** — `make mount` / `daemon-run` が無く、operator が insmod + mount + daemon を全部手で叩く暗黙依存。README 側も手順を書いていない。

### 1.3 datasource UID 固定時の provisioning collision

`prometheus.yaml` に `uid: prometheus` を追記して再起動したら Grafana が起動失敗 (`Datasource provisioning error: data source not found`)。既存の persisted DB に auto-UID の同名 datasource が残っていて collision するのが原因。`deleteDatasources` ブロックで先に消してから create する 2 段 provisioning に。

---

## 2. 修正後の想定フロー

```bash
# 1. 観測スタックだけ立ち上げ (MinIO / Prometheus / Grafana / node_exporter)
make compose-up

# 2. コード build (kmod + bpf + daemon 一括)
make all

# 3. ベンチ実行 — kmod insmod / mount / daemon 起動 / fuse mount / cleanup
#    は script 内 trap で完結する。Grafana は http://localhost:3000 で開いておく。
make bench            # hyperfine + microbench + fio (vs s3fs-fuse)
make bench-write      # dd 1M / 10M / 100M overwrite (vs fuse-raw/cache)
make bench-scale      # fio filestat + psync numjobs=1..64
make bench-3way       # ebpf vs fuse-raw vs fuse-cache 3-way

# 手動でマウントだけしておきたい場合 (別 shell で daemon を回すなど)
make mount            # insmod + mount /mnt/s3demo
make daemon-run       # foreground daemon (Ctrl+C で停止、--metrics-addr=:9600)
make unmount          # daemon-stop + umount + rmmod
```

**Grafana**: `http://localhost:3000` (admin/admin) → **s3fs-ebpf** フォルダ → **S3FS-eBPF Overview**。12 panel (fast path hit ratio / admits / sync entries / arena usage / req rate / write-back throughput / Path C upcalls / flush events / sync rounds / host CS / CPU 分解 / MinIO ingress)。

**Prometheus**: `http://localhost:9090`。`/api/v1/targets` で `s3fs_ebpf_daemon` `up=1` を先に確認するのが debug の入口。

---

## 3. 現在の metric surface

`daemon/internal/metrics/exporter.go` が公開している 14 metric:

| metric | type | 意味 |
|---|---|---|
| `s3fs_ebpf_fast_path_hits_total` | counter | BPF fentry admit-hit の getattr 数 |
| `s3fs_ebpf_slow_path_misses_total` | counter | BPF fentry admit-miss の getattr 数 |
| `s3fs_ebpf_unknown_fs_total` | counter | 他 FS の getattr (noise 目安) |
| `s3fs_ebpf_read_miss_upcalls_total` | counter | Path C upcall 到達数 |
| `s3fs_ebpf_read_miss_errors_total` | counter | upcall 中 S3 Get / admit 失敗数 |
| `s3fs_ebpf_sync_rounds_total` | counter | sync loop 成功回数 |
| `s3fs_ebpf_sync_errors_total` | counter | sync loop 失敗回数 |
| `s3fs_ebpf_sync_entries_current` | gauge | 現 dyn entries 数 |
| `s3fs_ebpf_flush_events_total` | counter | POP_FLUSH で drain した event 数 |
| `s3fs_ebpf_flush_bytes_total` | counter | S3 に PUT したバイト数 |
| `s3fs_ebpf_flush_errors_total` | counter | PUT 失敗数 |
| `s3fs_ebpf_admits_current` | gauge | 現 admit snapshot entry 数 |
| `s3fs_ebpf_arena_capacity_bytes` | gauge | arena size (定数) |
| `s3fs_ebpf_arena_used_bytes` | gauge | AdmitContent の high-water offset |

---

## 4. 残っている課題 (次に踏むであろう地雷)

環境改修を「動く」ところまでは戻したが、実際に使い込むと以下で困る:

### 4.1 bench 側

- **microbench 100k iters は s3fs-fuse で ~8 分** — fuse read が 5 ms/iter、100k で 500 秒。開発サイクルに乗せるには `MICRO_ITERS=5000 make bench` 相当を明示するか、bench-latency.sh に "quick" mode を切る
- **bench 実行中に SIGINT すると microbench が panic** — 私がテスト中に発生させた `open /mnt/s3fuse/hello.txt: no such file or directory`。 trap の umount 順序と実行中の子プロセス扱いが噛み合っていない。microbench 側で open 失敗を error return に変える (今 log.Fatal → panic path)
- **`fuse-cache` を bench-3way でしか mount していない** — bench-latency.sh は fuse-raw のみ。cached vs uncached を latency bench でも横に並べたいことがあるが今は分離
- **arena_used_bytes は AdmitContent の high-water で reset されない** — 長時間 daemon を回すと monotonic に増える。gc 未実装 (lessons §5.4)。dashboard の gauge が「99%」で貼り付いて再起動しないと戻らない可能性
- **`bench/runner.sh` は Week 0 skeleton で実質不要** — `make bench-dry` の存在確認だけに使っている。将来的には削除して bench/ ディレクトリ自体を消せる (fio job も legacy)

### 4.2 環境ライフサイクル

- **daemon が SIGINT で終わっても kmod / mount は残る** — `make unmount` を別途叩く必要がある。UX 的には bench script の trap を daemon-only ではなく kmod/mount まで拾う統一形にしてもよいが、bench 内から `make unmount` を呼ぶのは循環しがちなので現状維持
- **fuse mount が busy で umount 失敗** — bench 中に SIGINT すると子 process が fuse fd を握りっぱなしになり `umount /mnt/s3fuse` が `target is busy`。`umount -l /mnt/s3fuse` で lazy unmount を使う必要がある。将来 script で自動 fallback
- **`s3fs` (fuse client) の passwd_file 依存** — `-o passwd_file=/etc/passwd-s3fs` を script が hardcode。他 host に環境を持って行くと `/etc/passwd-s3fs` の作成が別手順。README にセットアップ手順を書くか、環境変数化する
- **compose を上げていない状態で `make bench` を叩くと** `docker inspect s3fs-ebpf-minio` が失敗 → 分かりやすい error message は出るが、`make bench` に `compose-up` の暗黙 dependency を張るかは要検討 (副作用が大きくなる)

### 4.3 metric / Grafana 側

- **MinIO metric 名の version 依存** — `minio_s3_traffic_received_bytes` は MinIO のあるバージョンから rename されている (`minio_s3_traffic_recv_bytes_total` 等)。現行 dashboard のクエリが空を返す場合は `http://localhost:9000/minio/v2/metrics/cluster` を curl して実際の metric 名を確認、dashboard 更新
- **`arena_used_bytes / arena_capacity_bytes` gauge が長時間観測で 100% 貼り付き** — §4.1 参照。dashboard の閾値 (0.7 / 0.9) は「初回 bench で警告色になったら fair」相当。gc 実装後に見直し
- **Prometheus scrape interval が 5s** — dashboard の `rate(...[30s])` はサンプル数少なく (~6 個) 荒れる。持続 load でしか安定した曲線が見えない。より短い window で `rate(...[1m])` に統一するか、interval を 1s に落とす検討
- **Latency histogram が無い** — 現状 counter/gauge のみ。read/write latency の p50/p95/p99 を Grafana で見たい場合、`prometheus/client_golang` に置き換えて `HistogramVec` を追加する必要がある。dep-free の hand-rolled では histogram は書きたくない
- **daemon 側の goroutine 数 / GC pause が観測できない** — `runtime/metrics` を expose すれば Go runtime 側の負荷が見えるが、hand-rolled exporter には手を入れていない。client_golang 移行のタイミングで追加

### 4.4 dashboard 側の細かい抜け

- **panel 8 (Flush events / errors)** — errors line が 0 の状態が normal。可視化としては hit-rate の逆で「>0 になったら赤くする」だと即座に気付ける。stat panel に切って threshold を付ける改造の余地
- **CPU% panel が iowait 分離のみ** — nice / softirq / irq / steal も並べたい。特に BPF fentry hook が softirq に載っているかどうかは observability にとって重要
- **write-back throughput は byte のみで request/s が無い** — flush_events rate はあるが「1 req あたり N bytes」の分解ができない。panel を追加するか、既存 panel を legend に組み込む
- **node_context_switches の分母がない** — 「fuse-bench 中は増える、ebpf-bench 中は減る」ことを伝えたいが、比較対象が同時に存在しない (bench は逐次実行)。1 グラフに 2 系列で並べたいが実現には bench harness の並列化が要る

### 4.5 fix そのものの残り物

- **`bench/runner.sh` の削除タイミング** — legacy header は入れたが、`make bench-dry` の依存で残っている。README も既に scripts/bench-* を主にしているので、次のクリーンアップで削除でよい (bench/fio/*.fio と bench/bcc/vfs_latency.py も同じ)
- **README.md 再生成** — `README.md.tmpl` の bench 節を更新したが、`make readme` を回して `README.md` を再生成していない (envsubst 経由なので生成手順あり)。次に README を触る人が忘れないよう note

---

## 5. debug 手順の目安

Grafana panel が空 / エラーのとき、以下の順で切り分ける:

```bash
# 1. daemon の metric endpoint が上がっているか
curl -s http://localhost:9600/metrics | head -5

# 2. Prometheus が daemon を UP と認識しているか
curl -s http://localhost:9090/api/v1/targets \
    | python3 -c "import json,sys; [print(t['labels']['job'], t['health']) \
                  for t in json.load(sys.stdin)['data']['activeTargets']]"

# 3. Prometheus に特定 metric が積まれているか
curl -s --data-urlencode 'query=s3fs_ebpf_fast_path_hits_total' \
    -G http://localhost:9090/api/v1/query

# 4. Grafana proxy 経由で query できるか (UID mismatch チェック)
curl -s -u admin:admin --data-urlencode 'query=s3fs_ebpf_admits_current' \
    -G http://localhost:3000/api/datasources/proxy/uid/prometheus/api/v1/query

# 5. Grafana logs で provisioning error を探す
docker logs s3fs-ebpf-grafana 2>&1 | grep -iE 'error|provision'
```

`4` が失敗する場合は datasource UID mismatch — provisioning yaml と dashboard JSON の `uid` を照合。

---

## 6. 参照

- `docs/dev-notes/lessons-learned.md` §5.4 — arena gc の未対応
- `docs/journal/benchmark-results.md` — bench の出力例と数字の解釈
- `scripts/bench-latency.sh` / `bench-write.sh` / `bench-scale.sh` / `bench-3way.sh` — 各 bench の実装
- `daemon/internal/metrics/exporter.go` — metric 定義の 1 次資料
- `deploy/grafana/dashboards/s3fs-ebpf-overview.json` — dashboard 本体
- `deploy/grafana/provisioning/datasources/prometheus.yaml` — datasource UID pin の理由コメント
