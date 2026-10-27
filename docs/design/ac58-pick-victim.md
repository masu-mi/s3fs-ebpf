# AC-58: `pick_victim` — victim の選択を BPF に渡す

状態: **実装済み (2026-10-17)**。SIEVE / LRU / S3-FIFO の `*_pick_ops` を実装し、結果は §6 に要約した。コードを変えたらこのメモも更新する。

## 1. 今の仕組みと問題

capacity 超過時の eviction は、kmod が主導する。

1. snapshot の ino ごとに `on_evict(ino)` を 1 回呼び、score (0 以上) を得る。
2. `cand[]` を (score 降順、ino hash) で整列し、`live_bytes <= capacity_bytes` になるまで先頭から evict する。

つまり BPF が決めるのは ino ごとの独立した score だけで、**「次に捨てるのはどれか」という順序・状態を持つアルゴリズム** (SIEVE の FIFO と hand、S3-FIFO の ghost list、LRU のリスト) を BPF 側で正しく書けない。`policy_sieve_ops` (旧版、今も残っている) は visited bit だけの近似で、FIFO も hand もない。

## 2. 変更

`struct s3fs_policy_ops` の**末尾**に 3 つ足す (既存の BPF プログラムは名前で bind するので、足さなくてもそのまま動く)。

```c
int (*on_insert)(u64 ino);       /* ino が候補の集合に新しく入った (冪等でよい) */
int (*on_remove)(u64 ino);       /* kmod が ino を evict した */
u64 (*pick_victim)(void);        /* 次に捨てる ino。0 = 決められない */
```

2026-10-20 (AC-66): さらに末尾に `int (*on_size)(u64 ino, u64 bytes);` を足した。Phase 1 で `on_insert` の直後に、kmod がその ino に課金するバイト数 (arena は entry の合計、pcache は `fd->cap`) を渡す。`capacity_bytes > 0` のときだけ呼ぶ。W-TinyLFU が window / main の予算をバイトで持つため (`docs/design/wtinylfu-pick-plan.md`)。

- `on_hit(ino)` は既存。これが access の通知。
- 非 sleepable。`on_insert` / `on_remove` / `pick_victim` は **kmod 側の mutex (`s3fs_policy_evict_mutex`) で直列化**して呼ぶ。BPF 側は状態の更新に lock を要らない (bpf_spin_lock の中では map helper を呼べず、リスト操作が書けないため)。`on_hit` だけは並行に来るので、visited の 1 ビットを立てるだけにする (落ちた更新は害がない)。issue #22 の `hit_accum=1` では `on_hit` も Phase 1 の mutex の下から呼ぶので直列になる (`hit-signal-percpu-accumulation.md`、計測は §18.44。既定は並行のまま)。

### kmod の呼び方 (`s3fs_ebpf_admit_replace` の capacity パス)

1. Phase 1 で ino を数えるときに `on_insert(ino)` を呼ぶ (round ごと。冪等)。
2. `live_bytes > capacity_bytes` で、vtable に `pick_victim` があるとき:
   - `pick_victim()` を呼び、返った ino が `cand[]` にあり、まだ試していなければ、既存の evict 処理 (pending なら shed、そうでなければ `admit_evict_ino`) を行う。
   - 返った ino が `cand[]` にないとき (別の経路で evict 済み): その ino に `on_remove` を呼んで BPF に忘れさせ、`pick_victim` を聞き直す (AC-64)。
   - 返った ino が 0 か試し済みのとき、または試行回数 (`ncand`) を超えたとき: **既定の score 順に戻す**。
3. score 順のパスでも、evict した ino には `on_remove(ino)` を呼ぶ。
4. pick した ino を BPF 側は自分で構造から外す (evict できなかった場合は次の round の `on_insert` で入り直す)。

score と同点処理 (`on_evict`、tiebreak) は、`pick_victim` を持たない policy のために残す。

## 3. SIEVE の BPF 実装 (`policy_sieve_pick_ops.bpf.c`)

- `slots[N]` (ARRAY、N=8192): `{ino, visited, valid}`。FIFO を seq 番号 (`head` 増加) の ring で持つ。
- `idx` (HASH): ino → seq。`on_hit` が O(1) で visited を立てるために引く。
- `st` (ARRAY[1]): `head` (次の seq)、`tail` (最古)、`hand`。
- `on_insert`: idx にあれば何もしない。なければ `slots[head % N]` に visited=0 で入れ、`head++`。満杯 (`head - tail >= N`) なら最古を捨てる。
- `on_remove`: idx から引いて `valid=0`。
- `pick_victim`: `hand` を tail 側から head 側に進める (bounded loop)。無効な slot は飛ばす。visited=1 なら 0 にして進む。visited=0 なら victim (slot を無効にして idx から消す、`hand = seq + 1`)。head に達したら tail に戻る。
- SIEVE の性質: 新しい item は head に入り、hand は動かず、promotion がない。

## 4. 測ること (計画時)

- `bench-hitrate` で `sieve_pick_ops` を、`lru_ops` / 現行 `sieve_ops` と比べる (so-fix のモデルでは SIEVE 46.8%、現行 33.3%)。
- `pick_victim` 1 回あたりの時間、mutex の保持時間、`on_hit` の read 経路へのコスト (bpftrace か perf)。read 経路は `on_hit` が map の lookup を 1 回増やす。
- 結果は `docs/journal/benchmark-results.md` に書き、ここに要約する。

## 5. LRU と S3-FIFO (`policy_lru_pick_ops.bpf.c` / `policy_s3fifo_pick_ops.bpf.c`)

- LRU: slot ごとに `on_hit` で単調増加の stamp を付け、`pick_victim` は使用中の slot の最高位置までを走査して最小の stamp を返す (候補数に比例、N=4096 まで)。
- S3-FIFO: S (新規、約 10%) / M (main) / G (ghost) の 3 queue を ring で持つ。`on_hit` は freq を 3 まで上げるだけ。`pick_victim` は S の最古を見て freq > 1 なら M に移し、そうでなければ victim にして G に入れる。M では freq を減らしながら一周させる。G にある ino が `on_insert` で戻ると M に入る。`on_remove` は G に入れる。G の長さは (S+M) の 90% まで (下限 1)。以前は max(|M|, 16) で、8 slot の bench では simulator の 2 倍の ino を覚えて M に直行させていた (issue #22 の調査で修正、`benchmark-results.md` §18.44)。
- kmod は、miss が in-flight (`pending_exists`) の ino を `on_insert` しない (次の round に回す)。pending は kmod の evict を止めるが、policy が victim に選ぶと ghost / hand が動いてしまうため。
- score 型の policy (`freq2bit_ops` / `time_decay_ops` / `size_aware_ops` / `pressure_ops`) は kmod の score 順で同じ結果になるので移していない。W-TinyLFU は admission window と SLRU と sketch があり、別の設計が要るので見送った。

## 6. 結果 (`docs/journal/benchmark-results.md` §18.41 / §18.42 / §18.43)

- hit rate (`bench-hitrate`、capacity mode、30 files / 8 slot、3 seed): S3-FIFO 36.7〜40.0%、LRU 34.9〜37.5%、SIEVE 35.0〜37.1%。どれも旧版 (score 型の `lru_ops` / `sieve_ops` / `s3fifo_ops`、29〜34%) より 2〜7 ポイント高く、順位は S3-FIFO > LRU ≈ SIEVE。trace-replay の値 (SIEVE 44〜48%) には届かない。ライブは eval を 25 access ごとにしか回さないため。
- read 経路 (`bench-policy-cost.sh`、1 file の `bs=1` read、どの read も `on_hit` を通る最悪ケース): 64 jobs で policy なしより −4.6% (旧 `sieve_ops` は −14.8%)。`on_hit` を map update から lookup + 1 ビットの store にしたため。
- BPF プログラム 1 回 (`bpf_stats_enabled`、8 候補): `pick_victim` 0.5〜0.9 µs、`on_insert` 約 0.4 µs、`on_hit` 0.3〜0.4 µs。1 round の合計は数 µs で、`admit_evict_ino` より桁で小さい。
- 回帰テストに `test-pick-victim-stale` を追加 (AC-64: 削除した ino が policy に残らない)。
- 残り: 候補が数千のときの `pick_victim` の時間 (LRU は候補を全部見る)、§7 のアプリのヒント。W-TinyLFU は AC-66 で `policy_wtinylfu_pick_ops` にした (`docs/design/wtinylfu-pick-plan.md`)。

## 7. アプリのヒント (未着手)

map で渡す。キーは ino (`st_ino`)。更新権限は daemon が持ち、アプリは daemon の API か xattr で渡す。アプリを変えない手段は `fadvise` / `madvise` の fentry。この変更の範囲外。

## 8. Future work: `admit_replace` の全件の問い合わせ (AC-69)

2026-10-26 にコードを読んで気づいた点。**計測はしていない。**

- `s3fs_ebpf_admit_replace` (`kmod/vfs_ops.c`、Phase 1 は 1398 行付近) は、admit 表を入れ替えるたびに、表にある ino (arena の entry と pcache の entry) を重複なしで集め、**全件** に `on_evict(ino)` を呼ぶ。`pick_victim` を持つ vtable では、全件に `on_insert(ino)` も呼ぶ (1420 / 1461 行付近)。容量を超えていない回も同じで、その回の結果は使われない。
- 走るのは admit 表の入れ替えのときだけ (arena モードの admit、容量が有効な pcache モードの populate の後 (`enforceCapacityAfterPopulate`)、定期の sync、`/policy/eval`)。page cache の hit の read では走らない。
- BPF 側のコストは §6 の値 (`on_hit` 0.3〜0.4 µs、`on_insert` 約 0.4 µs) から、1 ino あたり 1 µs 弱と見積もれる。1,000 ino なら 1 回の入れ替えで 1 ms 近い。
- kmod 側の重複除去は配列の線形探索 (`eval_inos`、`cand`、`pcache_inos`、1370 / 1405 / 1446 / 1485 / 1497 / 1527 / 1539 行付近) で、全体は O(N²)。数千件ではこちらが効くはず。
- 改善案:
  1. `pick_victim` を持つ vtable では、`on_evict` を容量を超えたとき (score 順に戻るとき) だけ呼ぶ。
  2. `on_insert` は新しく入った ino にだけ呼ぶ (前回の表との差分を kmod が持つ)。
  3. 重複除去をハッシュにして O(N) にする。
- 計測: ino 数 (100 / 1,000 / 10,000) を振って、`admit_replace` 1 回の時間 (bpftrace か ftrace の function_graph)、`bpf_stats_enabled` で `on_evict` / `on_insert` の合計、miss の latency への影響を見る。改善の前後で同じ計測を取り、`docs/journal/benchmark-results.md` に書く。
