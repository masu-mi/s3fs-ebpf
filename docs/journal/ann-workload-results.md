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

## 読み取り (この節の warm / cold の読みは下の「cold の調査」「warm の分析」で訂正した)

- warm の OLTP 点検索は ebpf-pcache が fuse-raw / fuse-cache の 4〜9 倍 (α=0: 218k vs 49k / 52k ops/s、p50 3.7 vs 23 µs)。page cache に載った後はカーネル内で返る (H5 と整合)。ただし fuse-cache の α=1.1 は p50 が 3.2 µs で ebpf と同等で、p99 と ops/s が劣るだけ。
- warm の vector は ebpf-pcache が fuse-raw の 3〜4 倍。fuse-cache とは α=0 で 0.6 倍 (24k vs 43k)、α=1.1 で同等 (19k vs 17k)。fuse-cache (kernel_cache) に対して ebpf が勝つとは言えない。
- cold の vector は 3 構成ともほぼ同じ (約 400〜600 ops/s)。MinIO への GET が支配的で、FS 層の差は出ない。
- **ebpf の cold OLTP (p50 4 µs) は本当の cold ではない可能性が高い**: 同じ `drop_caches=3` の後でも pcache のページが残っていて、fuse 側 (p50 23 µs 以上) と同じ条件の比較になっていない。α=1.1 の cold ebpf は逆に p99 が 5.9 ms と極端に悪い。原因は未調査 (admit / upcall の待ちの疑い)。cold の ebpf 対 fuse の優劣は、この計測では結論にしない。
- 1 回あたり 3 回しか測っておらず、cold の min–max が広い項目 (oltp α=1.1 など) は参考値。

# cold の挙動調査 (2026-10-30)

`scripts/bench-oltp-vector-cold.sh` (3 回、中央値)。計測の cold は 2 種類に分けた: `local` = `drop_caches=3` のみ (daemon の disk cache と fuse の use_cache は残る)、`s3` = さらに daemon / fuse を作り直して cache を消す (MinIO まで取りに行く)。open と最初のクエリを含めた時間で比べる。

## わかったこと

1. **`drop_caches` の値で ebpf の挙動が変わる**。`=1` (page cache のみ) は kmod の chunk (`fd_buf_bytes` 34.7 MB) を残し、upcall なしで p50 3.4µs / p99 20µs。`=2` / `=3` は inode を evict し、`s3fs_file_data_reset` が chunk を free する (`fd_buf_bytes` が 0 になる)。最初の計測は `=3` だったので chunk は消えている。
2. **最初のアクセス (マウント直後) は「pattern-aware full admit」で、ファイル全体 (32 MiB) が open の中で入る**。daemon ログ: `ov_kv.db seq_ratio=1.00 PROMOTE_SIGNAL=pattern-aware full admit (34734080 bytes)`。最初の 4 KiB 読み (SQLite のヘッダ) だけで seq_ratio=1.00 と判定される。upcall は 1 回で、タイマーの外 (`max(k)` が open 内) で 34 ms 払う。そのあとのループは全 hit (p50 3.9µs / p99 12µs) になる。→ 最初の計測の ebpf cold が「速い」のはこれ。
3. **evict 後 (2 回目以降の cold) は full admit にならず、4 MiB の chunk ごとに upcall する**。ログ: `miss ino=136 offset=... → pcache [..., +4 MiB)` が 9 回 (32 MiB / 4 MiB + 端数)。ランダムな点検索では、最初の 500 回で 8 chunk すべてが 1 回ずつ踏まれ、1 件あたり約 6〜7 ms (daemon の disk cache からの 4 MiB)。ループ全体では 1% を超えるので p99 が 6 ms になる。→ Zipf の cold の p99=5.9 ms の正体。
4. 以上により、最初の計測の cold には、(a) 最初のアクセスは open が肩代わり、(b) 2 回目以降は chunk の upcall、(c) daemon の disk cache は warm のまま、という不揃いがあった。

## open+ループの合計時間 (ms、中央値)

| workload | cold | ebpf-pcache | fuse-cache | fuse-raw |
|---|---|---|---|---|
| oltp (500 ops, a=1.1) | s3 (MinIO まで) | **37.3** | 46.2 | 46.7 |
| oltp | local (drop_caches=3) | 73.1 | 43.7 | 43.3 |
| vec (30 queries) | s3 | 60.4 | 79.2 | 51.4 |
| vec | local | 65.2 | 79.4 | 50.1 |

- s3 cold の OLTP は ebpf が最良 (full admit が 1 回の 4 並列 range GET で済む)。ただし 32 MiB の小さいファイルでの 1 点で、大きいファイルや多くのファイルでは未確認。
- local cold (page cache だけが冷えた状態) では ebpf が最悪 (OLTP で fuse の約 1.7 倍)。原因は、ランダムなアクセスなのに 4 MiB の chunk 単位で fill すること (9 upcall、1 件約 6〜7 ms)。fuse は 4 KiB 単位 (+ kernel_cache) で必要なページだけ。
- vec は 3 構成が 50〜80 ms で、差は小さい。

## 未調査

- chunk 1 件 6〜7 ms の内訳 (daemon の disk 読み、populate ioctl の memcpy、signal、スケジューリング)。
- full admit の判定が seq_ratio=1.00 (ヘッダの 1 回読み) だけで決まる点。ランダムアクセスのファイルでも最初は full admit になる。

# warm の分析 (2026-10-30)

`scripts/bench-oltp-vector-warm.sh`: 1 回 prime したあと各点 5 回 (OLTP 20000 ops、vec 3000 queries)、中央値。rusage は負荷プロセス自身のもの。

| wl | α | backend | ops/s | p50µs | p99µs | CPU/op µs | stime ms |
|---|---|---|---|---|---|---|---|
| oltp | 0 | ebpf-pcache | 275k | 3.0 | 9.2 | 3.6 | 17 |
| oltp | 0 | ebpf (hook なし) | 277k | 3.0 | 9.1 | 3.6 | 13 |
| oltp | 0 | fuse-cache | 272k | 3.0 | 8.9 | 3.6 | 12 |
| oltp | 0 | fuse-raw | 99k | 3.4 | 37.1 | 6.3 | 47 |
| oltp | 1.1 | ebpf-pcache | 301k | 2.8 | 6.2 | 3.4 | 10 |
| oltp | 1.1 | fuse-cache | 307k | 2.6 | 5.7 | 3.1 | 7 |
| vec | 0 | ebpf-pcache | 76k | 12.0 | 29.5 | 13.0 | 1 |
| vec | 0 | fuse-cache | 73k | 12.4 | 33.1 | 13.3 | 1 |
| vec | 1.1 | ebpf-pcache | 89k | 10.3 | 27.1 | 11.3 | 0 |
| vec | 1.1 | fuse-cache | 87k | 10.5 | 29.3 | 11.3 | 1 |

## 結論: 「ebpf が fuse-cache に勝てない」は、warm の定常状態では事実で、理由は構造的

1. **定常状態では ebpf-pcache と fuse-cache は同じ (差 ±4%、seed / 回の幅の内)**。どちらも読みは kernel の page cache のヒットで終わり、FS のコードは通らない。ここでは勝負にならない。read_iter の hook を外した版とも差がない (hook のコストは、この負荷では測定できない)。
2. **前の節の warm の差 (vec α=0 で fuse-cache の 0.6 倍、OLTP α=0 で ebpf が 4 倍など) は計測の誤り**。前の warm は「cold の 1/10 のあとの全 n 回」で、prime していなかった。構成ごとに warm になるまでの残りの fill 量が違い (ebpf は chunk の upcall、fuse-cache は cache ファイルからの fill)、その差が出ていた。prime したあとは消える。
3. **この負荷は FS の差が出にくい**: 負荷プロセスの CPU のほとんどが utime (SQLite / Python / 全ページに触る mmap の読み)。stime は全体の 5〜25% 以下で、FS の経路が違っても 1 回 3〜13µs の中の数 % しか動かせない。minflt/op も全構成で同じ (OLTP 0.01〜0.02、vec 0.18)。単スレッドの Python では 2 割の上限の中に収まる。
4. **fuse-raw に対する優位 (2.5〜3 倍) は、FUSE の往復を払うかどうか**。page cache を持たない構成 (raw) との比較なので、これは「kernel の page cache を使えるか」の差で、ebpf 固有の優位ではない。
5. ebpf は kmod にも chunk の複本を持つ (`fd_buf_bytes` 68 MB = 64 MiB のファイル 2 本)。page cache と二重なので、メモリ制約下では fuse-cache より不利になる可能性がある (未計測)。

## 次にやること (候補)

- ebpf が勝てる条件の探索: 並列度 (多スレッド / 多プロセス、fio)、複数プロセスでの共有 (H2)、cgroup のメモリ制約下の二重保持、fuse-cache の cache ファイルを超える大きさ。
- cold 側の改善案の検証: ランダムアクセス判定後の chunk fill の粒度 (`S3FS_CHUNK_SIZE` の再ビルド)、full admit の判定 (seq_ratio のしきい値)、chunk upcall 1 件の 6〜7 ms の内訳。
