# issue #22 の調査まとめ: hit_accum の効果と、途中で見つかった不具合 (2026-10-27)

issue #22 (hit の信号を per-CPU で集計し、evict の前に policy へ反映する) を実装し、測定した記録。設計は [`../design/hit-signal-percpu-accumulation.md`](../design/hit-signal-percpu-accumulation.md)、数値の表は [`benchmark-results.md`](benchmark-results.md) §18.44。ここには「結局どうだったか」と「何を間違えたか」をまとめる。

## 1. 結論

- **`hit_accum=1` の効き目は read 経路のコストと、数える単位が揃うこと**。hit rate への上乗せは小さい (§3)。
- 測定の途中で、`hit_accum` と無関係な **3 つのずれ** が見つかった。bench の capacity の 1 個ずれ、in-flight の miss の `on_insert`、S3-FIFO の ghost の下限。直したあとは live が simulator と ±3 pt で一致する。
- 最初に報告した「`hit_accum=1` で sieve_pick / wtinylfu_pick が +9〜11 pt」は、**大半が上の 3 つのずれで、`hit_accum` の効果ではなかった** (訂正済み)。
- 既定は off のまま。理由は、hit rate への上乗せが小さいことと、gate (b) の取り直しと lockdep が残っていること。

## 2. 何を実装したか

`hit_accum=1` のとき、`.read_iter` (と `.splice_read`) が開いたファイルごとに 1 回、inode の per-CPU の値を進める。`admit_replace` の Phase 1 が、evict の mutex の下で、数えた回数を `on_hit` として policy に渡す (oldest `last_tick` 順、`on_evict` の票と `pick_victim` の前)。fentry の KFunc、`.read_folio`、arena 直読みの `on_hit` は何もしなくなる。module param は `hit_accum` (既定 0) / `hit_rearm_jiffies` (HZ/10) / `hit_replay_max` (15)。反映した回数は `/metrics` の `s3fs_ebpf_policy_hits_replayed_total`。詳細は設計メモ §2。

## 3. hit_accum の効果

KVM ゲスト (6 vCPU、HZ=1000)、ローカル MinIO、pcache モード。

| 項目 | 結果 |
|---|---|
| read 経路のコスト (fio psync bs=1、64 jobs) | policy なし比 −5.9% (sieve_pick) / −7.3% (s3fifo_pick) から **+0.8% / +0.6%**。16 jobs も同じ |
| `bench-ab-pcache.sh` | gate (a)(c) PASS、`hook_ratio` 0.939→0.966。gate (b) は `hit_accum` 0 / 1 とも FAIL (既存の揺れ) |
| Phase 1 の時間 (全 ino が admit に残る) | 100 ino で 50→58 µs、1,000 ino で 315→335 µs (+6%)。見積もりの最大 5 ms は起きない |
| create (10,000 個) | 平均 170 対 181 µs。ばらつき (112〜236 µs) のほうが大きく、`alloc_percpu` の負担は検出できない |
| 数える単位 | 反映した回数 ≒ N_ACCESS − upcall (165 対 166 など)。`cat` 1 回 = 1 回で、2 倍にならない |
| `hit_rearm_jiffies` | 1 つの fd で 3 秒読み続けて 0 → 0 回、100 jiffies → 30 回、10 jiffies → 300 回の反映。仕様どおり |
| hit rate (3 seed の中央値、後述の修正後) | wtinylfu_pick +3.0 pt、sieve_pick +0.6、s3fifo_pick −0.6、lru_pick −0.2 (ノイズ ±3 pt) |
| `time_decay_ops` | 下がらなかった。`on_hits` (vtable の 2 段目) は要らない |
| 正しさ | `make test-regression` は 0 / 1 とも 19 件 PASS、`make test-daemon` PASS |

つまり、policy を付けても read が遅くならない (これは確実に効いた) ことと、頻度や新しさの単位が揃うことが利点。hit rate は、不具合を直したあとでは `hit_accum` の有無でほぼ変わらない。

## 4. 見つけた不具合

どれも「`hit_accum` の効果」に見えた差を作っていた。(1)〜(3) は `hit_accum` と無関係で、別コミットにしてある。

| # | 不具合 | 症状 | 修正 (コミット) |
|---|---|---|---|
| 1 | **bench の capacity が 1 個少ない**。`bench-hitrate.sh` が `FILE_BYTES=44` から capacity を計算するが、実ファイルは 46 B。352 B は 7 個分で、simulator は 8 個 | LRU が simulator より約 4 pt 低い。§18.41 の「simulator との差 8〜11 pt」の主因。softcap=7 の simulator は live と一致 | `FILE_BYTES=46`、mixed の `SMALL_FILE_BYTES=60` (`9ba7d2c`) |
| 2 | **kmod が in-flight の miss の ino を `on_insert` していた**。pending は kmod の丸ごとの evict を止める (shed になる) が、policy がその ino を victim に選ぶと ghost / hand が動く | S3-FIFO で新しい ino が ghost に入り、次の `on_insert` で M に直行して居座る。simulator との最初の食い違いは 39 番目の access | pending の ino は `on_insert` / `on_size` / collect を次の round に回す (`e710994`、`efd7a65`) |
| 3 | **S3-FIFO の ghost の下限 16**。|G| ≤ max(|M|, 16) | 8 slot の bench で simulator (|G| ≤ 7) の 2 倍以上の ino を覚え、M に直行させる | |G| ≤ (S+M) の 90% (下限 1) (`71a76fd`) |
| 4 | **legacy の evict 経路 (softcap の vote) が `policy_seen` を落とさない** (`hit_accum` の実装の穴) | 再 admit 後、evict 中の miss read まで hit として数える (過大計上) | `s3fs_hit_forget` を両方の `admit_evict_ino` に追加 (`efd7a65`) |

(1) は測定の不具合、(2)(3) は policy 側の不具合。(1) があるので simulator との比較が全て 1 slot ずれていて、(2)(3) は同じ 7 slot の世界でだけ目立った。

## 5. どう見つけたか (再現できる手順)

1. A/B の表で `lru_pick_ops` と `s3fifo_pick_ops` だけ simulator との差が大きいことに気づいた。
2. simulator を `--softcap=7` にして比べると LRU が一致 → bench の capacity を疑い、`mc stat` でファイルが 46 B と確認した。
3. 同じ trace の per-access の hit/miss を live と simulator で出して、最初に食い違う access を探した (`dev trace-replay --per-access`、`PER_ACCESS_LOG`)。S3-FIFO は 39 番目で、live が simulator より ino を長く残していた。
4. bpftrace で `s3fs_policy_ops_insert` / `eval_hit` / `pick_victim` / `remove` を追うと、新しい ino に対する `V` (victim) が出てから `R` が出ない (= shed) ケースがあった (2)。policy のコードを読んで ghost の下限を見つけた (3)。
5. 直すたびに per-access の食い違いが 352 → 293 → 159 件と減った (seed 42、S3-FIFO)。

使った道具: `bench-hitrate.sh` の `PER_ACCESS_LOG` / `WORKLOAD_FILE` / `ACCESS_SLEEP`、`s3fs-ebpf dev trace-replay --per-access=<file>`。

## 6. 間違えたこと

- **「+9〜11 pt」を `hit_accum` の効果として報告した**。同じ capacity で A/B にはなっていたが、(2)(3) が `hit_accum=0` の経路でだけ強く効いていたので、差が `hit_accum` のものに見えた。simulator と比べて初めて、絶対値がおかしいことに気づいた。A/B だけでなく、基準 (simulator) との絶対値も見る。
- **「simulator との差の原因は eval の間隔 (25 access ごと)」という説明 (§18.41) を、確かめずに受け入れていた**。`INTERLEAVE=1` で差が縮まらなかったときに疑うべきだった。§18.41 に訂正を入れた。
- 反映の遅れを「末尾の取りこぼし」と推測して、確認しないまま書いた (27〜34 件の不足)。今は populate 時点を基準にしたので窓は閉じているが、不足が 0 になったかは測っていない。
- `sleep` を入れて access を遅くした実験は、daemon の周期 sync で inode が落ちて miss が増え、別の現象を測っていた (cache の寿命の話で、policy とは無関係)。タイミングで相関を取る実験は、周期処理を止めてから。

## 7. 残っていること

- S3-FIFO の per-access の食い違いがまだ 159 件 (hit 907 対 simulator 936)。両方向に散っていて、細かい実装差と思われる。sieve と wtinylfu でも同じ突き合わせをする。
- `INTERLEAVE=1` を修正後の kmod で取り直す。
- G5 (大きいファイルの頻度の水増し): ino ごとの反映回数を見る。mixed bench は capacity を直したので取り直す。
- `bench-ab-pcache.sh` gate (b) の取り直し。lockdep (AC-42)、`make test-kunit` (KSRC が要る)。
- 既定を 1 にするかの判断 (read 経路のコストと単位の統一だけが理由になる)。

## 8. 関連

- [`../design/hit-signal-percpu-accumulation.md`](../design/hit-signal-percpu-accumulation.md) — 設計、測定の手順、採否の基準
- [`benchmark-results.md`](benchmark-results.md) §18.41 (訂正済み)、§18.44
- [`../kmod-pcache-invariants.md`](../kmod-pcache-invariants.md) — 「hit の集計」の節 (pending の ino を `on_insert` しない)
- [`../design/ac58-pick-victim.md`](../design/ac58-pick-victim.md) — S3-FIFO の ghost の長さ
