---
name: perf-visualize
description: 性能の計測結果と実行中の daemon / kmod の状態を Grafana・Prometheus で可視化する手順 (compose-up、layer-latency ダッシュボードの取り込み、新しいダッシュボード / パネルの足し方、metrics の確認、スクリーンショット)。「Grafana に出して」「グラフにして」「ダッシュボードを作って」「metrics を見て」「Prometheus で確認」「計測結果を図にして」と言われたとき、または bench の結果を人に見せるとき、実行中の upcall / hit / latency を観察するときに使う。測定そのものは perf-bench-verify。
---

# 性能の可視化 (Grafana / Prometheus)

測る手順と検証は `perf-bench-verify`。この skill は**見せる側**: スタックの起動、既存ダッシュボード、新しい図の足し方。図を作る前に `dataviz` skill (配色・形式の選び方) を読む。

## スタック

- `make compose-up` で MinIO + Prometheus (`:9090`) + Grafana (`http://localhost:3000`、admin:admin) + node_exporter。`make compose-down` / `compose-logs`。再起動後はコンテナが止まっている。
- Prometheus は daemon の `--metrics-addr=:9600` を scrape する (`deploy/prometheus/prometheus.yml`)。bench は `--metrics-addr=:9600` を付けて daemon を起動する。**daemon が動いていないと時系列は空**。
- 既定ダッシュボード: `deploy/grafana/dashboards/s3fs-ebpf-overview.json` (provisioning は `deploy/grafana/provisioning/`)。
- **Grafana の bind mount はメインのチェックアウトの `deploy/` を指す。worktree で JSON を置いても読み込まれない** → API (`POST /api/dashboards/db`) で取り込む。

## 層ごとのレイテンシ (棒グラフ + 表 + 時系列)

```bash
OUT=/tmp/r/layer TARGET_SEC=12 scripts/bench-layer-latency.sh        # layer-latency.csv と annotation (タグ layer-latency) を出す
scripts/grafana-layer-latency.sh /tmp/r/layer/layer-latency.csv       # "S3FS-eBPF Layer latency" を取り込む
# -> http://localhost:3000/d/s3fs-ebpf-layer-latency
```

- 生成は `scripts/lib/grafana-layer-latency.py` (CSV 埋め込みの TestData datasource + Prometheus の時系列)。datasource が無ければ script が作る。`GRAFANA_URL` `GRAFANA_AUTH` で変える。
- 使える主な series: daemon の `s3fs_ebpf_read_miss_latency_seconds` (histogram、50 µs〜10 s)、`s3fs_ebpf_read_miss_upcalls_total`、MinIO の `minio_s3_requests_total{api="getobject"|"headobject"}`。**MinIO の TTFB histogram は最小 bucket が 50 ms でローカルの GET を分解できない。図にしない。**
- kmod の folio hit は `--sample-every` (既定 60 s) ごとにしか反映されない。層の判定には upcall 数と GET 数を使う。

## 新しい図を足す

1. まず値の出どころを確かめる: `curl -s localhost:9600/metrics | grep s3fs_ebpf_` と `curl -s localhost:9090/api/v1/query --data-urlencode 'query=<式>'`。
2. 既存の `grafana-layer-latency.sh` / `.py` を手本に、bench の CSV / JSON から JSON ダッシュボードを生成する script にする (手で JSON を書かない。再現できなくなる)。bench が各 run を annotation で投稿しておくと、時系列を run に重ねられる。
3. 比は「s3fs-fuse の時間 ÷ s3fs-ebpf の時間」(1 超が ebpf の勝ち) に揃える。図に載せる比は `perf-bench-verify` §4 の「同じ仕事を比べているか」を通してから。
4. 結果の生データ・スクリーンショットは `docs/journal/data/<名前>-<日付>/` に置き、`docs/INDEX.md` に足す (`perf-bench-verify` §3)。

## 見た目の確認

`isolated-browser` skill (使い捨てコンテナの Chrome。ホストに playwright / ブラウザを入れない):

```bash
.claude/skills/isolated-browser/browse.sh -a admin:admin -o /tmp/<scratch>/shots http://localhost:3000/d/s3fs-ebpf-layer-latency
```

コンテナランタイムが無ければ撮らず、`POST /api/ds/query` で各パネルのクエリが値を返すことを確かめる。撮ったら scratch とコンテナを片付ける。
