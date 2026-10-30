# ANN (IVF / HNSW) policy simulator 結果 (P0〜P2)

2026-10-30。`docs/design/ann-workload-measurement-plan.md` の P0〜P2。`scripts/bench-ann-sim.sh` (`dev ann-sim`) で再現できる。kmod / MinIO は使っていない (simulator のみ)。実機 (P4) と OLTP (E4) は未実施。

条件: N=1000 万、chunk 4 MiB、各点 5 seed、warmup 20% は集計外、cold start。容量はデータ全体 (index+raw) に対する比。OPT は等サイズ近似の上界、noevict は無限容量。全表は `/tmp/bench-ann-sim/*.md` (再生成可)。数値は seed 平均 (seed 間の幅は表に出る)。

## 結果の要点 (req hit % / GET per query)

| workload | cap | lru | s3fifo | wtinylfu | opt |
|---|---|---|---|---|---|
| IVF d128 一様 | 5% | 9.7 / 17.9 | 9.7 / 17.9 | 10.1 / 17.8 | 31.7 / 13.4 |
| IVF d128 Zipf1.1 | 5% | 24.2 / 14.9 | 30.5 / 13.5 | 30.7 / 13.5 | 46.7 / 10.4 |
| IVF d128 Zipf1.1 | 25% | 48.6 / 10.0 | 51.3 / 9.5 | 52.5 / 9.2 | 75.0 / 4.9 |
| IVF d768 Zipf1.1 | 5% | 31.6 / 15.0 | 36.2 / 13.4 | 35.0 / 13.8 | 51.7 / 10.2 |
| HNSW bfs Zipf1.1 | 25% | 84.2 / 239 | 84.3 / 238 | 83.1 / 257 | 91.9 / 122 |
| HNSW insertion Zipf1.1 | 25% | 24.7 / 1144 | 25.0 / 1138 | 27.0 / 1108 | 61.0 / 591 |

- 人気が一様なら policy 間の差はほぼない (差は 1 ポイント未満)。差が出るのは偏り (Zipf1.1) のとき。そこでは s3fifo / wtinylfu が lru を 2〜6 ポイント上回る (容量 5%)。
- OPT との差は大きい (IVF Zipf1.1・25% で 20 ポイント以上)。どの policy も上界には遠い。特に rerank (小さなランダム読み) は OPT 30〜70% に対し実 policy は 4〜37%。
- HNSW はレイアウトが支配的: bfs は insertion の 5 倍ほどヒットし、GET/query は 1/5。policy の差よりレイアウトの差が桁で大きい。直列 GET の深さ (serial/q) は約 52 でどの policy でも変わらない (依存連鎖は policy で縮まない)。
- mixed (hot IVF + 周期的な一度きり scan): scan の chunk は全 policy でほぼ 0〜2% しか残らない (意図どおり)。hot の centroid は 99.8〜100% 維持。lru のみ centroid 99.8% と僅かに落ちる。cluster の順位は単独 IVF と同じ。
- readahead 1 chunk (d768): GET/query が増え (15.0→19.0 lru)、ヒット率は改善せず (31.6→30.1)。精度は 3〜13%。クラスタが chunk に収まるため、この workload では先読みは負け。
- chunk 径: 256 KiB / 1 MiB / 4 MiB で同容量比のヒット率は小さい chunk ほど高い (lru 5%: 34.4 / 30.2 / 24.2)。取得バイト/query は 4.2 MB / 14.7 MB / 59.6 MB で大差。ただし GET 数は 16.9 / 14.7 / 14.9 でほぼ同じ (レイテンシが GET 数で決まるなら小 chunk の利点は帯域のみ)。

## 注意 (妥当性の脅威)

- 人気分布 (Zipf α)、HNSW のグラフ局所性 (近傍の 80% が ±4096、20% が長距離) はモデルの仮定。HNSW insertion 配置は近傍を完全に散らす悲観的モデル。実データでの確認 (P3) は未実施。
- 先読み時の精度は、先読みが `Access` を呼ぶため既存 entry のスコアを触る点で、実 kmod とずれる。
- byte hit は centroid 表 (毎クエリ数 MB) に引きずられ高めに出る。req hit と tag 別の値を併せて読む。
- OPT は chunk sweep では外した (eviction ごとに O(cap))。
