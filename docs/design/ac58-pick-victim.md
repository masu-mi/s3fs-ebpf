# AC-58: `pick_victim` — victim の選択を BPF に渡す

状態: 提案 → 実装中 (2026-10-17)。コードを変えたらこのメモも更新する。

## 1. 今の仕組みと問題

capacity 超過時の eviction は、kmod が主導する。

1. snapshot の ino ごとに `on_evict(ino)` を 1 回呼び、score (0 以上) を得る。
2. `cand[]` を (score 降順、ino hash) で整列し、`live_bytes <= capacity_bytes` になるまで先頭から evict する。

つまり BPF が決めるのは ino ごとの独立した score だけで、**「次に捨てるのはどれか」という順序・状態を持つアルゴリズム** (SIEVE の FIFO と hand、S3-FIFO の ghost list、LRU のリスト) を BPF 側で正しく書けない。現行の `policy_sieve_ops` は visited bit だけの近似で、FIFO も hand もない。

## 2. 変更

`struct s3fs_policy_ops` の**末尾**に 3 つ足す (既存の BPF プログラムは名前で bind するので、足さなくてもそのまま動く)。

```c
int (*on_insert)(u64 ino);       /* ino が候補の集合に新しく入った (冪等でよい) */
int (*on_remove)(u64 ino);       /* kmod が ino を evict した */
u64 (*pick_victim)(void);        /* 次に捨てる ino。0 = 決められない */
```

- `on_hit(ino)` は既存。これが access の通知。
- 非 sleepable。`on_insert` / `on_remove` / `pick_victim` は **kmod 側の mutex (`s3fs_policy_evict_mutex`) で直列化**して呼ぶ。BPF 側は状態の更新に lock を要らない (bpf_spin_lock の中では map helper を呼べず、リスト操作が書けないため)。`on_hit` だけは並行に来るので、visited の 1 ビットを立てるだけにする (落ちた更新は害がない)。

### kmod の呼び方 (`s3fs_ebpf_admit_replace` の capacity パス)

1. Phase 1 で ino を数えるときに `on_insert(ino)` を呼ぶ (round ごと。冪等)。
2. `live_bytes > capacity_bytes` で、vtable に `pick_victim` があるとき:
   - `pick_victim()` を呼び、返った ino が `cand[]` にあり、まだ試していなければ、既存の evict 処理 (pending なら shed、そうでなければ `admit_evict_ino`) を行う。
   - 返った ino が 0、`cand[]` にない、試し済みのとき、または試行回数 (`ncand`) を超えたとき: **既定の score 順に戻す**。
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

## 4. 測ること

- `bench-hitrate` で `sieve_pick_ops` を、`lru_ops` / 現行 `sieve_ops` と比べる (so-fix のモデルでは SIEVE 46.8%、現行 33.3%)。
- `pick_victim` 1 回あたりの時間、mutex の保持時間、`on_hit` の read 経路へのコスト (bpftrace か perf)。read 経路は `on_hit` が map の lookup を 1 回増やす。
- 結果は `docs/journal/benchmark-results.md` に書き、ここに要約する。

## 5. アプリのヒント (未着手)

map で渡す。キーは ino (`st_ino`)。更新権限は daemon が持ち、アプリは daemon の API か xattr で渡す。アプリを変えない手段は `fadvise` / `madvise` の fentry。この変更の範囲外。
