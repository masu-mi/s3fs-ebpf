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

# 実機 (kmod + MinIO) での OLTP / vectordb 風の読み取り計測

2026-10-30。`scripts/bench-oltp-vector.sh` を 3 回実行 (各回 mount し直し、中央値と min–max)。ebpf-pcache (`--arena-metadata-only`) と s3fs-fuse の raw / cache を MinIO に対して比べる。KVM ゲスト (6 vCPU)、読み取り専用のみ (書き込みは E-01 / AC-51 の限界のとおり対象外)。

- OLTP: SQLite 32 MiB の読み取り専用 DB (`immutable=1`、SQLite のキャッシュ 512 KiB)、PK の点検索 5000 回。α=0 は一様、α=1.1 は Zipf。
- vectordb: 32 MiB の mmap ファイルに対する IVF 風クエリ (128 KiB のクラスタ 8 個の全ページ + rerank 10 回)、300 クエリ。
- cold = `vm.drop_caches=3` 直後の最初の 1/10 の操作、warm = 全操作 (キャッシュ温まった後)。

## 結果 (中央値。ops_s は 1 秒あたり操作数 / クエリ数、p50・p99 は µs)

```
workload alpha phase backend n  ops_s(min-max)  p50_us  p99_us (medians)
oltp 0 cold ebpf-pcache 3 162576(159078-173221) 4.2 21.5
oltp 0 cold fuse-cache 3 37321(34489-37867) 24.4 55.5
oltp 0 cold fuse-raw 3 38242(31888-39659) 23.4 59.3
oltp 0 warm ebpf-pcache 3 218251(216139-227301) 3.7 9.1
oltp 0 warm fuse-cache 3 51515(49926-53462) 23.3 41.7
oltp 0 warm fuse-raw 3 48972(48206-49912) 23.2 43.1
oltp 1.1 cold ebpf-pcache 3 10613(10544-10938) 3.9 5876.8
oltp 1.1 cold fuse-cache 3 15486(13786-15837) 89.9 367.1
oltp 1.1 cold fuse-raw 3 53970(53695-54698) 22.9 68.4
oltp 1.1 warm ebpf-pcache 3 276379(267558-280185) 2.9 7.5
oltp 1.1 warm fuse-cache 3 29886(28893-33426) 3.2 172.8
oltp 1.1 warm fuse-raw 3 92192(91464-94685) 3.1 35.6
vec 0 cold ebpf-pcache 3 428(428-431) 439.6 45435.1
vec 0 cold fuse-cache 3 462(448-466) 402.1 42425.2
vec 0 cold fuse-raw 3 449(445-465) 410.8 43688.4
vec 0 warm ebpf-pcache 3 24322(23770-24782) 13.0 317.6
vec 0 warm fuse-cache 3 42703(42227-43282) 13.0 180.2
vec 0 warm fuse-raw 3 5867(5389-6100) 13.2 3284.7
vec 1.1 cold ebpf-pcache 3 433(420-448) 182.5 55293.5
vec 1.1 cold fuse-cache 3 375(373-410) 1319.0 10105.2
vec 1.1 cold fuse-raw 3 620(614-663) 343.0 31834.5
vec 1.1 warm ebpf-pcache 3 18953(18131-19280) 25.5 245.6
vec 1.1 warm fuse-cache 3 17364(16407-17800) 14.8 880.6
vec 1.1 warm fuse-raw 3 6321(6011-6600) 14.8 1553.4
```

## 読み取り

- warm の OLTP 点検索は ebpf-pcache が fuse-raw / fuse-cache の 4〜9 倍 (α=0: 218k vs 49k / 52k ops/s、p50 3.7 vs 23 µs)。page cache に載った後はカーネル内で返る (H5 と整合)。ただし fuse-cache の α=1.1 は p50 が 3.2 µs で ebpf と同等で、p99 と ops/s が劣るだけ。
- warm の vector は ebpf-pcache が fuse-raw の 3〜4 倍。fuse-cache とは α=0 で 0.6 倍 (24k vs 43k)、α=1.1 で同等 (19k vs 17k)。fuse-cache (kernel_cache) に対して ebpf が勝つとは言えない。
- cold の vector は 3 構成ともほぼ同じ (約 400〜600 ops/s)。MinIO への GET が支配的で、FS 層の差は出ない。
- **ebpf の cold OLTP (p50 4 µs) は本当の cold ではない可能性が高い**: 同じ `drop_caches=3` の後でも pcache のページが残っていて、fuse 側 (p50 23 µs 以上) と同じ条件の比較になっていない。α=1.1 の cold ebpf は逆に p99 が 5.9 ms と極端に悪い。原因は未調査 (admit / upcall の待ちの疑い)。cold の ebpf 対 fuse の優劣は、この計測では結論にしない。
- 1 回あたり 3 回しか測っておらず、cold の min–max が広い項目 (oltp α=1.1 など) は参考値。
