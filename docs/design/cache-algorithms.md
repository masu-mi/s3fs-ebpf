# キャッシュアルゴリズムの早見 (LRU / SIEVE / S3-FIFO / W-TinyLFU)

この PoC が struct_ops の policy として持つ 4 つのアルゴリズムの、動きの説明。どれも「キャッシュが一杯のとき、どれを追い出すか」を決める。実装は `bpf/policy_*_pick_ops.bpf.c` (`pick_victim` を持つ AC-58 の vtable、[`ac58-pick-victim.md`](ac58-pick-victim.md))、W-TinyLFU の設計は [`wtinylfu-pick-plan.md`](wtinylfu-pick-plan.md)。

kmod から見た呼び出しは共通で、次の 4 つ。

| member | いつ呼ばれるか | policy がすること |
|---|---|---|
| `on_insert(ino)` | ino がキャッシュに入った | 自分の queue に足す |
| `on_hit(ino)` | read のたび (hot path、並行に走る)。`hit_accum=1` では `admit_replace` の中で evict の mutex の下に、数えた回数ぶんまとめて呼ばれる (#22、(§18.44 で計測。この kfunc を外した効果は read のコストが policy なしと同等まで下がった)) | 印を付けるだけ (1 ビットや小さいカウンタ)。`hit_accum=1` なら直列になるので制限は緩む |
| `pick_victim()` | 容量を超えた | 追い出す ino を 1 つ返す |
| `on_remove(ino)` | kmod が別の経路で追い出した | 忘れる |

`on_hit` は(既定では) read の hot path でロックなしに並行して走るので、どの実装も「値を 1 つ書くだけ」にし、queue の並べ替えは kmod が直列化する `on_insert` / `pick_victim` の中だけで行う (AC-58、hot path で map を update すると 64 jobs で 14.8% 落ちた)。

---

## 1. LRU (Least Recently Used)

LRU には「この論文が初出」と言える 1 本がない。比較の基準によく使う最適解 (Belady の OPT、未来を知っている場合の上限。スライド so-pitfall の 64.15%) は L. A. Belady, "A Study of Replacement Algorithms for a Virtual-Storage Computer", IBM Systems Journal 5(2), pp. 78–101, 1966

いちばん長く使われていないものを追い出す。

```
 新しい ←──────────────────────────→ 古い
  [E] [B] [D] [A] [C]                 ← C を追い出す
   ↑
  B が hit すると先頭へ移る:  [B] [E] [D] [A] [C]
```

- 長所: 単純で、直近に使ったものが残る。
- 短所: 1 回しか読まれないファイルの走査 (scan) が来ると、よく使うものまで押し出される。hit のたびに並べ替えが要る。
- この PoC (`policy_lru_pick_ops`): 並べ替えの代わりに、ino ごとに最後に使った時刻 (`++clock`) を記録し、`pick_victim` が一番小さい時刻を探す。`on_hit` は時刻を 1 つ書くだけ。

## 2. SIEVE (NSDI 2024)

論文: Yazhuo Zhang, Juncheng Yang, Yao Yue, Ymir Vigfusson, K. V. Rashmi, "SIEVE is Simpler than LRU: an Efficient Turn-Key Eviction Algorithm for Web Caches", NSDI '24 ([USENIX](https://www.usenix.org/conference/nsdi24/presentation/zhang-yazhuo)、[PDF](https://junchengyang.com/publication/nsdi24-SIEVE.pdf))

FIFO に「訪問済み」の 1 ビットを足し、hand (針) が古い側から見て回る。

```
 新しい ←──────────────────────────→ 古い
  [E:0] [D:1] [C:0] [B:1] [A:1]
                              ↑ hand
 hand が A を見る: 1 なので 0 に戻して見逃す → B も同じ → C は 0 なので追い出す
  [E:0] [D:1]  ×C   [B:0] [A:0]
                ↑ hand はここで止まり、次はここから
```

- hit しても位置は動かさず、ビットを 1 にするだけ。新しく入ったものは先頭に入り、hand は動かない。
- hand は古い側から新しい側へ進み、端に来たら古い側に戻る。
- 長所: LRU より hit 時の仕事が少なく (1 ビットの store)、1 回だけのアクセスを早く追い出せる。
- この PoC (`policy_sieve_pick_ops`): FIFO はリング (`slots`)、hand は seq で持つ。旧版 (`policy_sieve_ops`) は「この ino を追い出してよいか」を 0/1 で答えるだけで hand を持てず、hit rate が simulator より 9〜13 pt 低かった (スライド so-pitfall)。

## 3. S3-FIFO (SOSP 2023)

論文: Juncheng Yang, Yazhuo Zhang, Ziyue Qiu, Yao Yue, K. V. Rashmi, "FIFO Queues are All You Need for Cache Eviction", SOSP '23 ([DOI 10.1145/3600006.3613147](https://doi.org/10.1145/3600006.3613147)、[PDF](https://junchengyang.com/publication/sosp23-s3fifo.pdf)、[著者のブログ](https://blog.junchengyang.com/system/cache/2023/08/01/s3fifo))

FIFO を 3 本使う。新しいものはまず小さい queue に入り、そこで 2 回以上使われたものだけが本体に上がる。

```
             ┌──────────── S (small, 約 10%) ──────────┐
 新規 ─────→ │ [F:0] [E:0] [D:2]                       │ ── 古い側から見る
             └─────────────────────────────────────────┘
                     D: freq > 1 → M へ        E: freq ≤ 1 → 追い出して G へ
             ┌──────────── M (main) ───────────────────┐
             │ [D:0] [C:1] [B:0] [A:2]                 │ ── 古い側から見る
             └─────────────────────────────────────────┘
                     freq > 0 → freq を 1 減らして M の末尾に戻す
                     freq = 0 → 追い出して G へ
             ┌──────────── G (ghost) ──────────────────┐
             │ 名前だけ (中身なし)。|G| ≤ |M|           │
             └─────────────────────────────────────────┘
 G にある ino がまた入ってきたら、S を飛ばして M に直接入る
```

- hit では freq を 1 増やすだけ (上限 3)。
- `pick_victim` は、S が S+M の 10% 以上なら S の一番古いものを、そうでなければ M の一番古いものを見る。
- 長所: 1 回だけのアクセス (one-hit wonder) は S で早く追い出され、M を汚さない。ghost が「最近追い出したが、また来た」ものを見分ける。
- この PoC: `policy_s3fifo_pick_ops`。実機の bench-hitrate では 3 seed とも 4 つの中で一番高かった (36.7〜40.0%、スライド so-real)。

## 4. W-TinyLFU (TOS 2017、Caffeine の形)

論文: Gil Einziger, Roy Friedman, Ben Manes, "TinyLFU: A Highly Efficient Cache Admission Policy", ACM Transactions on Storage 13(4), Article 35, 2017 ([arXiv:1512.00727](https://arxiv.org/abs/1512.00727))。W-TinyLFU はこの論文で提案された形で、Java のキャッシュライブラリ [Caffeine](https://github.com/ben-manes/caffeine) の既定の方式

小さな LRU の窓と、頻度で入場を決める本体を組み合わせる。頻度は Count-Min sketch で近似する。

```
 新規 ──→ ┌ W (window, 1%) ┐     あふれた ino が「候補」になる
          │ [H] [G]        │ ──────────────┐
          └────────────────┘               ↓
          ┌──────────── main (99%) ───────────────────────────┐
          │  P (probation)               T (protected, 80%)    │
          │  [D] [C] [B] ── 古い側 ─→ 対戦相手                  │
          │  P で再び hit したものは T へ                        │
          └────────────────────────────────────────────────────┘
                    候補 vs P の一番古いもの
                    sketch の推定頻度が低いほうを追い出す (同点なら候補を断る)

 sketch: 4 行 × 1024 列の 4 bit カウンタ。read のたびに足し、
         一定回数ごとに全部を半分にする (古い人気を忘れる)
```

- 窓 W があるので、急に人気が出たものもすぐに入れる。本体には「候補が今いるものより頻繁に使われている」ときだけ入れる。
- 長所: scan に強く、頻度と新しさの両方を見る。
- この PoC (`policy_wtinylfu_pick_ops`): 各区画は厳密な LRU ではなく、hit ビット付きの FIFO (CLOCK)。`on_hit` は sketch を足してビットを立てるだけ。容量はバイトで数えるので、kmod から `on_size(ino, bytes)` を受け取る (AC-66)。sketch の半減は `bpf_loop` で回す。実機は 1 回の計測で 38.55% (S3-FIFO 37.80%、差は 15 miss で有意ではない)。

---

## 5. 並べて比べる

| | 入る場所 | hit で何をするか | 追い出すもの | 苦手 |
|---|---|---|---|---|
| LRU | 先頭 | 先頭へ移す (この PoC は時刻を書く) | 一番古く使われたもの | scan、hit 時のコスト |
| SIEVE | FIFO の先頭 | ビットを立てる | hand が最初に見つけたビット 0 | 頻度の差は見ない |
| S3-FIFO | S | freq++ (上限 3) | S か M の古い側で freq が低いもの | 状態が 3 本の queue |
| W-TinyLFU | W | sketch++ とビット | 候補と P の古いもののうち頻度が低いほう | 実装が一番重い |

数字は `docs/journal/benchmark-results.md` §18.41〜18.42、比較の simulator は `daemon/internal/trace/`。

## 6. 参考文献

- SIEVE: [USENIX NSDI '24](https://www.usenix.org/conference/nsdi24/presentation/zhang-yazhuo)
- S3-FIFO: [ACM SOSP '23 (DOI 10.1145/3600006.3613147)](https://doi.org/10.1145/3600006.3613147)
- W-TinyLFU: [arXiv:1512.00727](https://arxiv.org/abs/1512.00727) (ACM TOS 13(4), 2017)
- OPT: Belady, IBM Systems Journal 5(2), 1966
