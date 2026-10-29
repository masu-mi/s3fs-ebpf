# issue #22 の調査まとめ: hit_accum の効果と、途中で見つかった不具合 (2026-10-27)

issue #22 (hit の信号を per-CPU で集計し、evict の前に policy へ反映する) を実装し、測定した記録。設計は [`../design/hit-signal-percpu-accumulation.md`](../design/hit-signal-percpu-accumulation.md)、数値の表は [`benchmark-results.md`](benchmark-results.md) §18.44。ここには「結局どうだったか」と「何を間違えたか」をまとめる。

## 1. 結論

- **`hit_accum=1` の効き目は read 経路のコストと、数える単位が揃うこと**。hit rate への上乗せは小さい (§3)。
- 測定の途中で、`hit_accum` と無関係な **3 つのずれ** が見つかった。bench の capacity の 1 個ずれ、in-flight の miss の `on_insert`、S3-FIFO の ghost の下限。直したあとは live が simulator と ±3 pt で一致する。
- 最初に報告した「`hit_accum=1` で sieve_pick / wtinylfu_pick が +9〜11 pt」は、**大半が上の 3 つのずれで、`hit_accum` の効果ではなかった** (訂正済み)。
- **`hit_accum=1` を既定にした (2026-10-27)**。hit rate への上乗せは小さいが、read 経路のコスト、単位の統一、G5 の解消という利点がある。lockdep も通った (2026-10-28)。`hit_accum=0` で従来の信号に戻せる。

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
| 5 | **simulator との比較が kernel の ino と違うラベルで行われていた**。`trace.jsonl` は `100+idx`、kernel の ino は `105+idx`。W-TinyLFU の sketch は ino を hash するので、同じ access の列でも衝突の仕方が違う (sim では 1 回しか読まれていない ino の推定値が 6) | W-TinyLFU の per-access の食い違いが約 16%。sketch の中身を bpftool で dump して sim と突き合わせると、`ino+5` で全 4096 slot が一致 | bench が kernel の ino を記録し (`inodes-<policy>.txt`、`trace-ino-<policy>.jsonl`)、simulator はそれを replay する (`fix(scripts)`) |
| 6 | **`hit_accum` が、まだ cache にない ino への access を policy に伝えていなかった**。最初の look で、populate までの数を捨てていた | W-TinyLFU の sketch (admission の duel が比べる頻度) が、cache にない ino の access を数えない。simulator は miss でも数える | populate までの数 `d_pre` を `on_insert` の前に replay、populate 後の hit `d` を後に replay (`feat(kmod): hit_accum` の続きの修正) |
| 7 | **`s3fs_ebpf_entries_replace` が、listing が変わらなくても `shrink_dcache_sb` を呼んでいた**。bench は 3 秒ごと、既定は 30 秒ごとの sync で、使われていない dentry をすべて落とし、idle な inode が evict され、chunk が free される | resident な ino が `s3fs_ino_buf_bytes` で 0 B になる。capacity の計上が狂い、cache にあるはずの file が miss する。sieve と simulator の食い違いの大半 (337 件の最後の約 230 件) | listing が前回と同じなら shrink しない (`dyn_entries_equal`)。`SYNC_EVERY=3600s` で測ると sieve は simulator と **0 件の食い違い** |
| 8 | **S3-FIFO の ghost の上限が、その時点の S+M の数に依存していた**。miss が in-flight の間は 1 つ少ない | run ごとに hit 数が最大 +5 pt 動く | S+M の最大値 (`peak`) の 90% にした |

(1)(5) は測定の不具合、(2)(3)(8) は policy 側の不具合、(6) は `hit_accum` の設計の不足、(7) は `hit_accum` と無関係の kmod の不具合 (cache を定期的に空にしていた)。(1) があるので simulator との比較が全て 1 slot ずれていて、(2)(3) は同じ 7 slot の世界でだけ目立った。

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

## 7. 再発の防ぎ方 (回帰テストと、直した設計)

不具合は「同時に直すべき場所が散らばっていた」ことから起きていた。(1) bench の `FILE_BYTES` と seed の内容が別々、(2) policy への `on_insert` が 2 つのループにコピーされていて、pending の規則が片方にしかない、(3) policy の状態を忘れる処理が evict の経路ごとに散っていた (§4 の 4)、(4) ghost の長さが simulator と別の定数。次のように、規則を 1 箇所に集め、外れたら落ちる検査を足した。

| 防ぎ方 | 内容 |
|---|---|
| bench の seed (集約) | `hitrate_content()` / `small_content()` が内容の唯一の定義。`FILE_BYTES` / `SMALL_FILE_BYTES` はそこから測る。seed 後に `mc stat` で保存されたサイズを確かめ、違えば止まる |
| kmod の policy 通知 (集約) | `on_insert` / `on_size` / `on_remove` は `s3fs_policy_track` / `s3fs_policy_untrack` だけが呼ぶ。pending の規則は `s3fs_policy_trackable` の 1 箇所。2 つの Phase 1 ループの重複していた処理も同じ関数になった。hit の状態を忘れるのは `untrack` と `admit_evict_ino` だけ |
| 静的検査 `make check-invariants` | `scripts/check-kmod-invariants.sh`。上の呼び出し制限と、bench の file size が書き込みでないことを grep で見る。root も kernel も要らない。`make test-regression` の先頭に入れた。わざと生の呼び出しを足す、`FILE_BYTES=44` に戻す、のどちらでも落ちることを確かめた |
| 回帰テスト `make test-policy-conformance` | `scripts/test-policy-conformance.sh`。4 つの pick_victim policy を live で動かし (`hit_accum=1`、capacity = 8 ファイル)、同じ trace を simulator に通して per-access で比べる。条件は hit 数の差 ≤ 3.0 pt と、食い違う access の割合が policy ごとの上限以下 (lru 4%、sieve 2%、s3fifo 9%、wtinylfu 9%)。`SYNC_EVERY=3600s` で回し (sync の tick が入ると hit 数が動く)、落ちた policy だけ 1 回やり直す。simulator には kernel の ino を replay させる。`make test-regression` に入れた |

回帰テストが過去の不具合を捕まえるか、元に戻して確かめた (seed 42、s3fifo_pick_ops):

| 戻したもの | hit 数の差 | 結果 |
|---|---|---|
| なし (現在) | 1.8〜2.2 pt | PASS |
| ghost の下限 16 | 4.2 pt | FAIL |
| pending の ino を `on_insert` | 5.3 pt | FAIL |
| (3 つとも直す前) | 約 6 pt | (FAIL する) |

bench の capacity のずれは、1 slot 足りないと simulator との差が全 policy に出る。今は `FILE_BYTES` を測るので起きない。起きたら静的検査と seed 後の `mc stat` が止める。

テストの限界:
- `hit_accum=0` (旧の信号) では、旧信号が一部の hit を落とす (jiffy に 1 回の間引き) ので、hit 数の差が 3.3 pt まで出てテストが揺れた。テストは既定で `hit_accum=1` で測る (`CONF_HIT_ACCUM=0` で旧信号を測れる)。
- s3fifo と wtinylfu は、hit 率が合っていても 6〜7% の access で simulator と食い違う。simulator がモデル化していない部分 (kmod の round、pending の shed、wtinylfu の limbo / rejected) による。上限はその値のすぐ上に置いてあり、これを超える悪化は捕まえる。sieve は 0%、lru は 1.8%。
- kmod 側の pending の規則を単体で試す KUnit は書いていない (Phase 1 は mount と daemon を要する)。live のテストで捕まえる。

追加した回帰テスト (2026-10-28):

| テスト | 防ぐもの |
|---|---|
| `make test-sync-keeps-cache` (`scripts/test-sync-keeps-cache.sh`) | listing が変わらない sync が cache を空にすること (上の 7)。`--sync-every=2s` で file を読み、sync の tick を 3 回待って読み直しても upcall が増えない。object を足すと次の tick で見える。`changed` を常に真に戻すと `cache-dropped-by-sync(upcalls 1->2)` で FAIL する。`make test-regression` に入れた |
| KUnit `s3k_dyn_entries_equal` | 「同じ listing」の判定 (size、名前、親 ino、key の増減) |
| KUnit `s3k_hit_delta_once` / `s3k_hit_replay_order` | populate までの数 `d_pre` と、populate 後の `d` の replay の順序 |

## 7.1 後から足した回帰テスト (2026-10-29)

| テスト | 防ぐもの | 戻すと |
|---|---|---|
| `make test-splice-hits` (`scripts/test-splice-hits.sh`) | `sendfile` / `splice` の読みが hit として数えられること。pcache モード、容量あり、pick_victim の policy。file A を cat で miss させ、`sendfile` を 6 回(毎回 open し直す)、file B の miss で round を走らせて、`policy_hits_replayed_total` の増分が 6 以上 | `.splice_read` を `filemap_splice_read` に戻すと +1 で FAIL(数えると +7) |
| `make test-boot-kernel` (`scripts/test-boot-kernel.sh`) | `scripts/boot-kernel.sh` が正しい grub entry を選ぶこと。fixture (`scripts/testdata/grub-submenu.cfg`) と `DRY_RUN=1` で、root も grub への書き込みも不要。一覧、`generic`、`lockdep`、`--once` (`grub-reboot`)、部分一致、未知の kernel の拒否、`GRUB_DEFAULT=saved` でないときの拒否 | submenu の id に先頭の引用符が残る不具合、title の先頭 1 文字が欠ける不具合(どちらも最初の版にあった)を戻すと FAIL |
| KUnit `s3k_vfs_hit_count` | `.read_iter` が open ごとに 1 回数える(`hit_rearm_jiffies=0`)こと、`hit_accum=0` では数えないこと | accum の判定を外すと FAIL |
| KUnit `s3k_vfs_forget_on_evict` | `admit_evict_ino` が `policy_seen` を落とすこと(丸ごとの evict の唯一の出口)、policy が知っている ino の populate は base を動かさないこと | `s3fs_hit_forget` を外すと FAIL |
| KUnit `s3k_policy_trackable` | miss が in-flight の ino は policy に入れない(`on_insert` / `on_size` / collect しない)こと、完了後は入れること | pending の判定を外すと FAIL |

KUnit は 45 件 PASS。3 つとも、戻すと 3 件 FAIL(42 件 PASS)になることを確かめた。

**`test-policy-conformance` のリトライが、実は効いていなかった**: `bench-hitrate.sh` は per-access のログに追記するので、2 回目の判定が 1 回目の最初の 2000 行をもう一度読み、毎回同じ結果を返していた。リトライの前にログを消すようにして、上限を 3 回にした。ごく弱い許容(0.05 pt)で 3 回とも別の走行(934、934、944 hit)になることを確認した。s3fifo は run ごとに +0.1〜4.7 pt動く(daemon の 2 秒ごとの eviction drain と stats の tick が access の合間に入るため)ので、1 回目で通らなくても、別の走行でやり直す必要がある。

## 8. 残っていること

- per-access の食い違いの残り (`SYNC_EVERY=3600s`、決定的な条件): lru 1.8%、sieve **0%**、s3fifo 約 6〜7%、wtinylfu 約 6%。hit 数は simulator と ±0.5 pt (外れ値で ±2 pt)。s3fifo と wtinylfu の残りは simulator がモデル化していない部分: kmod の round (miss した ino は 1 round 遅れて policy に入る)、pending の shed、wtinylfu の limbo / rejected キュー。
- 時刻に依存する round が access の合間に入るので、hit 数が run ごとに動く (s3fifo で最大 +4.7 pt、wtinylfu で +2.5 pt)。sync の tick は `SYNC_EVERY=3600s` で止めているが、daemon の 2 秒ごとの eviction drain (`drainEvictionsLoop`、間隔が固定) と stats の tick は止められない。`test-policy-conformance` は落ちた policy を別の走行で最大 3 回やり直す。
- 既定を 1 にした (2026-10-28)。残りの課題は、上の simulator のモデル化と、長時間の soak。

調査の続きで確認したこと (`benchmark-results.md` §18.45):
- **lockdep (AC-42)**: lockdep 付きカーネル (6.12.0-s3fs-lockdep) で `make test-lockdep` が PASS (2026-10-28)。`hit_accum` 1 / 0 の `test-regression` と 120 秒の evict-race で lockdep / sleeping-in-atomic / RCU の報告が 0 件。evict の mutex の中の `ilookup` / `iput` を含む。手順と踏んだこと: `docs/dev-notes/lockdep-kernel.md`。
- `INTERLEAVE=1` にしても hit rate は上がらない。評価の間隔は simulator との差の原因ではなかった。
- gate (b) は 10 MiB の cat が 16 ms 前後の同点で、ベースライン (`51e9e50`) は PASS、現在は `hit_accum` 0 / 1 とも FAIL だが差は 0.7% でノイズ内。`hit_accum` の影響ではない。
- G5: large の 1 access が `on_hit` 約 82 回 (folio ごと) だったのが、`hit_accum=1` で高々 1 回になった。
- `make test-kunit` (UML) PASS (最後は 42 件)。

## 9. 関連

- [`../design/hit-signal-percpu-accumulation.md`](../design/hit-signal-percpu-accumulation.md) — 設計、測定の手順、採否の基準
- [`benchmark-results.md`](benchmark-results.md) §18.41 (訂正済み)、§18.44
- [`../kmod-pcache-invariants.md`](../kmod-pcache-invariants.md) — 「hit の集計」の節 (pending の ino を `on_insert` しない)
- [`../design/ac58-pick-victim.md`](../design/ac58-pick-victim.md) — S3-FIFO の ghost の長さ
