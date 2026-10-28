# hit の信号を per-CPU で集計し、evict の前に policy へ反映する (issue #22)

状態: **実装済み (2026-10-27)。module param `hit_accum` で切り替え、**既定は 1** (2026-10-27 に既定化。0 で従来の方式)。`make test-regression` は 0 / 1 とも PASS、§5.2〜§5.5 を測った (§8、`benchmark-results.md` §18.44)。測定の途中で、bench の capacity のずれと、hit_accum と無関係な 2 つの不具合 (in-flight の miss の `on_insert`、S3-FIFO の ghost の下限) を見つけて直した。hit rate への上乗せは小さい。既定化の根拠は §6 と §9。**
コードの位置は `51e9e50` から実装後の `kmod/vfs_ops.c` に合わせている。issue の原文 (問題の整理、採らなかった案を含む) は GitHub issue #22。ここには実装した形と、実装で決めたことだけを書く。

## 1. 何を直すか

hit を policy に伝える経路は 2 本あった。`fentry/generic_file_read_iter` から KFunc `s3fs_kfunc_policy_hit` (S-03、(CPU, ino) ごとに jiffy に 1 回へ間引き、AC-47) と、kmod の `.read_folio` / arena 直読み (folio ごと)。問題は 4 つ。

1. 間引きで回数が落ちる。
2. 数える単位が揃っていない (read_iter の間引き後と folio)。大きいファイルで頻度が水増しされる (`wtinylfu-pick-plan.md` G5)。bench の `cat` は 1 回で `read_iter` を 2 回呼ぶが、同じ jiffy に入るので偶然 1 回になっている。
3. `on_hit` が evict の mutex の外から並行に来るので、policy は「1 ビットの store、取りこぼしは許す」形に制限される (`ac58-pick-victim.md` §2)。
4. hit を取るために汎用関数の fentry が要る (`meetup-qa.md` Q30)。

hit の信号は miss の経路では置き換えられない。evict は ino の page cache も捨てるので、page cache の hit が policy に届かないと、page cache で何度も読まれるファイルほど冷たく見える。

## 2. 実装した形

`hit_accum=1` のとき:

- **数える** (`s3fs_hit_count`、`s3fs_ebpf_read_iter` の先頭): 開いたファイル (`struct file`) ごとに 1 回。`file->private_data` の `struct s3fs_file_admit_cache` に `hit_tick` を足し、0 か `hit_rearm_jiffies` 以上経っていれば、inode の `s3fs_file_data` の per-CPU 値 (`struct s3fs_hit_acc`: `count`、`last_tick`) を進める。`hit_rearm_jiffies=0` は open ごとに 1 回。read のたびの仕事は `struct file` の値の比較だけで、共有の cacheline には書かない。
- **反映する** (`admit_replace` の Phase 1、`s3fs_policy_evict_lock` の下): Phase 1 のループの前に、全 ino について `s3fs_hit_collect` で全 CPU の `count` を足し、`hit_flushed` との差 d を得る。policy が既に知っている ino (`policy_seen`) の分は、ループの前に `last_tick` の昇順 (同じなら ino の順) に並べて `min(d, hit_replay_max)` 回ずつ `on_hit` を呼ぶ (`s3fs_hit_replay`)。この round で `on_insert` する新しい ino の分は、ループ (`on_insert`) のあとで同じ順に呼ぶ。miss が in-flight (`pending_exists`) の ino は、`on_insert` も collect も次の round に回す。既知の ino の hit は、この round で入る ino より古いので先に反映する (LRU の stamp の順を保つため)。**その後で** `on_evict` の票を取り、Phase 2 (`pick_victim` / score 順) に進む。票を後ろに回したのは、legacy の policy (`sieve_ops` など) が `on_evict` を判断点にしているため。
- **最初の look の基準**: populate (`s3fs_ebpf_populate_pages`、fd->lock 下) が、その時点の合計を `hit_base` に記録する。`policy_seen` が偽の ino は、最初の look で `hit_flushed = hit_base` にして `policy_seen` を立てる。S3 から取った最初の read は数えず、populate から最初の Phase 1 までの再アクセスは数える。populate がない (arena モード) ときは最初の look の合計を基準にし、その分は捨てる。`on_remove` の経路 (`s3fs_capacity_evict_one` の丸ごとの evict と、`cand[]` にない ino の聞き直し) で `s3fs_hit_forget` が `policy_seen` を落とす。
- **直接の経路を止める**: `s3fs_kfunc_policy_hit` (tree_traversal.c) と、`.read_folio` / arena 直読みの `on_hit` (`s3fs_hit_signal_direct`) は何もしない。kmod 側で止めるので BPF の作り直しは要らず、fentry のアクセスパターン判定 (R-01) は残る。
- Phase 1 で evict の mutex を取る条件は `pick_victim` を持つ policy に加えて `hit_accum` のとき (`hit_flushed` / `policy_seen` を直列に保つため)。vtable は変えていない。

module param (`/sys/module/s3fs_kfunc/parameters/`):

| param | 既定 | 意味 |
|---|---|---|
| `hit_accum` | 1 | 1 = この方式 (既定)、0 = 従来 |
| `hit_rearm_jiffies` | HZ/10 | 開いたままのファイルがもう 1 回数えるまでの jiffies。0 = open ごとに 1 回。既定は §5.3 の計測で決める (bench は毎回 open し直すので差は出ないはず) |
| `hit_replay_max` | 15 | 1 回の Phase 1 で 1 ino に呼ぶ `on_hit` の上限 (S3-FIFO / freq2bit は 3、W-TinyLFU の sketch は 15 で飽和) |

カウンタ `s3fs_ebpf_policy_hits_replayed_total` (`S3FS_IOC_STATS` slot 14) は、`hit_replay_max` で切る前の d の合計。

## 3. 実装で決めたこと・残っている穴

- ino から `s3fs_file_data` は `ilookup(s3fs_active_sb, ino)` で引く (`s3fs_ino_buf_bytes` と同じ)。inode が cache にない ino、`s3fs_file_data` を持たない inode は数えない。
- `hit_accum` の ON/OFF を実行中に切り替えても整合する (差分は `hit_flushed` で持っている)。ただし OFF の間に溜まった分は、次に ON にしたときに 1 回で反映される。
- Phase 1 用の配列の確保に失敗したときは、その回の hit の反映と `on_evict` の票を飛ばさず通常の順序に戻る (hit はその回だけ失われる)。
- `splice` / `sendfile` は `.read_iter` を通らないので、`.splice_read` を包んで同じように数える (`s3fs_ebpf_splice_read`)。mmap の読み (`filemap_fault`) は数えない (fentry でも同じ、R-06 の範囲)。
- **反映は `admit_replace` が呼ばれたときだけ**。capacity なしで miss も sync の変化もなければ呼ばれず、反映も起きない。evict の判断は常に `admit_replace` の中なので、判断の前には反映される。
- メモリは inode ごとに CPU 数 × 16 B。
- `time_decay_ops` は反映した時刻で記録される。アクセスの時刻に戻すには vtable に `on_hits(ino, count, last_tick)` を足す (2 段目。1 段目の計測で下がったら入れる)。

## 4. アルゴリズムごとの見込み (計測していない)

| 種類 | policy | 見込み |
|---|---|---|
| 訪問ビット | `sieve_pick_ops`、`sieve_ops`、`pressure_ops`、`size_aware_ops` | 本来の意味で動く。「前の判断から読まれたか」だけが要り、反映は判断の直前 |
| 回数 | `s3fifo_pick_ops`、`freq2bit_ops`、`wtinylfu_pick_ops` ほか | `hit_replay_max` までは正確 |
| 新しさ | `lru_pick_ops` | jiffy の粒度でアクセスの順を保つ |
| 時刻 | `time_decay_ops` | 1 段目では下がりうる (§3) |

この方式で直るのは page cache の hit の届き方で、`benchmark-results.md` §18.41 の simulator との 8〜11 pt の差 (evict の評価が 25 access ごとにしか回らないこと) は別の原因。切り分けは §5.3 の `INTERLEAVE`。

## 5. 再計測の手順

同じビルドで `hit_accum` を切り替えて A/B する。`scripts/lib/bench.sh` の `ebpf_mount` が insmod の後に `HIT_ACCUM` / `HIT_REARM_JIFFIES` / `HIT_REPLAY_MAX` を上の param に書く (未設定なら触らない)。hit rate は pcache モード (`--arena-metadata-only`) で測る。

1. **正しさ**: `make test-regression`、`make test-daemon`、`make test-kunit KSRC=...`、`HIT_ACCUM=1 make test-regression`。KUnit は `s3k_hit_delta_once` (差分が 1 回だけ渡る、最初の look は捨てる) と `s3k_hit_replay_order` (`last_tick` の昇順、`hit_replay_max` で切り、カウンタは切る前の値)。
2. **数える単位**: `HIT_ACCUM=1 SEED=42 POLICIES="sieve_pick_ops" DAEMON_EXTRA_FLAGS="--arena-metadata-only" OUT=/tmp/hitsig/unit ./scripts/bench-hitrate.sh` で、`s3fs_ebpf_policy_hits_replayed_total` の増分が N_ACCESS − (upcall の増分) に近ければ 1 回の `cat` が 1 回。2 倍に近ければ `read_iter` の呼び出しを数えている。
3. **hit rate**: `HIT_ACCUM` {0,1} × `INTERLEAVE` {25,1}、seed は §18.41 / §18.42 に合わせて `SEEDS="42 7 123"`、`bench-hitrate-multi.sh`、policy は `observe sieve_ops lru_pick_ops sieve_pick_ops s3fifo_pick_ops wtinylfu_pick_ops time_decay_ops`。`observe` は `HIT_ACCUM` で変わらないこと、pick 版が 0 より下がらないこと、`time_decay_ops` の下がり幅、`INTERLEAVE=1` で simulator との差がどれだけ縮むか。
4. **大きいファイル (G5)**: `bench-hitrate-mixed.sh` を `HIT_ACCUM` {0,1} で。
5. **read 経路と Phase 1**: `bench-policy-cost.sh` (`ARMS="none sieve_pick_ops s3fifo_pick_ops" JOBS_LIST="1 16 64"`) と `HIT_ACCUM=1 ./scripts/bench-ab-pcache.sh`。ino 100 / 1,000 (`N_FILES`) で `s3fs_ebpf_admit_replace` の時間を bpftrace で。`on_hit` 0.3〜0.4 µs × 1,000 ino × 15 回で mutex の下に最大約 5 ms。長ければ `on_hits` を先に入れる。
6. 記録先: `docs/journal/benchmark-results.md` に新しい節 (`hit_accum` × `INTERLEAVE` の表と read 経路の表)、このメモに結果の節、`ac58-pick-victim.md` §6、`read-iter-hook-overhead.md` §6、`wtinylfu-pick-plan.md` G5、`meetup-qa.md` Q30。

## 6. 採否の基準

次をすべて満たしたら `hit_accum=1` を既定にする。**既定にした (2026-10-27)**。lockdep を除いて、下の基準は §8 の測定で満たした。

- 5.1 がすべて通る。
- 5.2 で反映した回数が hit の数に近い (2 倍にならない)。
- 5.3 でどの pick 版も 0 より 2 pt を超えて下がらない (seed 間のばらつきは約 2 pt)。
- 5.5 で read 経路のコストが 0 より悪くならず、`bench-ab-pcache.sh` の gate がすべて PASS。
- Phase 1 の増分が、1,000 ino で 1 回の `admit_replace` の時間の大半を占めない。

## 7. 採らなかった案

- fentry の中で per-CPU に数え、BPF の syscall プログラムで集める: 他 CPU の slot を読む必要があり、反映を evict の直前に揃えるには daemon が毎回走らせる必要がある。slot の衝突と fentry の固定費も残る。
- 間引きの幅だけを変える: 単位の不揃いと並行の更新が残る。

## 8. 結果 (2026-10-27、まだ 1 点だけ)

環境: KVM ゲスト、6.12.0-061200、HZ=1000 (`hit_rearm_jiffies` の既定 = 100 jiffies)、ローカル MinIO、pcache モード。

- **正しさ (§5.1 の一部)**: `make test-daemon` PASS。`make test-regression` は `hit_accum=0` と `HIT_ACCUM=1` の両方で全項目 PASS (evict-race の sha256_fail=0、read_err=0)。`make test-kunit` は KSRC がなく未実行 (KUnit 込みのコンパイルは通る)。
- **数える単位 (§5.2)**: `bench-hitrate.sh` (`sieve_pick_ops`、SEED=42、N_ACCESS=2000、`INTERLEAVE` は既定) で `HIT_ACCUM=1` のとき `s3fs_ebpf_policy_hits_replayed_total` = 859。N_ACCESS − upcall = 2000 − 1114 = 886 に近く (2 倍ではない)、1 回の `cat` が 1 回と数えられている。不足の 27 は §3 の「新しく入った ino の取りこぼし」と考えられる(未確認)。`bench-hitrate.sh` に `DETAIL ... hits_replayed=` の行を足した。
- **hit rate (§5.3)**: 最初の測定 (`hit_accum=1` で sieve_pick / wtinylfu_pick が +9〜11 pt) は、調査の結果、大半が `hit_accum` の効果ではなかった。bench の capacity のずれ (7 個分、`FILE_BYTES=44`)、in-flight の miss を policy が victim に選ぶ問題、S3-FIFO の ghost の下限 16 の 3 つを直したあとの 3 seed 中央値 (`INTERLEAVE=25`):

| policy | simulator | acc=0 | acc=1 |
|---|---|---|---|
| wtinylfu_pick_ops | 48.4 | 49.1 | 52.1 |
| sieve_pick_ops | 46.8 | 48.2 | 48.8 |
| s3fifo_pick_ops | 47.2 | 49.0 | 48.4 |
| lru_pick_ops | 39.2 | 38.5 | 38.3 |
| sieve_ops (旧) | — | 33.2 | 32.4 |
| time_decay_ops | — | 30.8 | 31.8 |
| observe | — | 30.9 | 34.4 |

  (`observe` が 31〜34 で動くのでノイズは約 ±3 pt。全体の経緯と修正前の表は `journal/benchmark-results.md` §18.44。)
  - live が simulator と ±3 pt で一致するようになった。pick 版は acc=1 で下がらない。
  - **`hit_accum=1` の hit rate への上乗せは小さい** (wtinylfu +3.0、sieve_pick +0.6、s3fifo −0.6、lru −0.2)。この方式の利点は read 経路のコスト (下) と数える単位が揃うことで、hit rate を大きく上げることではない。
  - `time_decay_ops` は下がらなかった。`on_hits` (2 段目) は要らない。
  - 上の表の `INTERLEAVE=1` は修正前の測定だけ。simulator との差は縮まらなかった。
- **見つけた不具合 (`hit_accum` と無関係に効く)**: (1) bench の capacity が 1 個少なかった、(2) kmod が in-flight の miss を `on_insert` していた (pending は evict を止めるが、policy が victim に選ぶと ghost / hand が動く。pending の ino は次の round に回す)、(3) `policy_s3fifo_pick_ops` の ghost の長さの下限が 16 で、8 slot の bench では simulator の 2 倍以上の ino を覚えていた ((S+M) の 90%、下限 1 に変更)。simulator との per-access の突き合わせ (`PER_ACCESS_LOG` と `dev trace-replay --per-access`) で見つけた。
- **read 経路のコスト (§5.5)**: 64 jobs で policy なし比が −5.9% (sieve_pick) / −7.3% (s3fifo_pick) から +0.8% / +0.6% になった (16 jobs も同様)。`bench-ab-pcache.sh` は (a)(c) PASS、`hook_ratio` は 0.939 から 0.966。(b) は `hit_accum` に関係なく FAIL (pcache 16.4〜17.2 ms 対 fuse-cache 15.4〜16.6 ms)。取り直しが要る既存の揺れ。
- **Phase 1 の時間**: 全 ino が admit に残る条件で、`admit_replace` の平均は 100 ino で 50→58 µs、1000 ino で 315→335 µs (+6%)。見積もりの最大約 5 ms は、d が小さいので起きない。**Phase 1 を触られた ino のリストに絞る案と、`on_hits` は今は要らない。**
- **create のコスト**: 10,000 個の小さいファイルの create は平均 170 µs 対 181 µs で、ばらつき (112〜236 µs) のほうが大きい。`alloc_percpu` の負担は検出できない。
- **`hit_rearm_jiffies`**: 1 つの fd で 3 秒間読み続けると、0 → 0 回、100 jiffies → 30 回、10 jiffies → 300 回の反映。仕様どおり。既定は HZ/10 のままにする (fd を開いたままの read も、100 ms ごとに 1 回は数える)。
- **大きいファイル (§5.4)**: `bench-hitrate-mixed.sh` の hit_rate は pcache では意味を持たない (chunk ごとの upcall で miss が N_ACCESS を超える)。miss 数は acc=0 / 1 でほぼ同じ (541/579 対 546/590)。G5 が消えたかは ino ごとの反映回数を見ないと言えず、未測定。
## 9. 残っていること

- sieve (約 9〜12%)、wtinylfu (約 16%)、s3fifo (約 8%) の per-access の食い違い。hit 率は ±2 pt で合っている。SIEVE は hand の端のバグを 1 つ直した (§18.45)。wtinylfu は未調査。run ごとにばらつくので、タイミング由来の揺れもある。
- `fentry` の KFunc 呼び出しを BPF 側で飛ばす案: read のコストは policy なしと同等まで下がっているので、効果が測れるまで入れない。
- 

確認済み (2026-10-28): lockdep 付きカーネルで `make test-lockdep` PASS (`hit_accum` 1 / 0 の `test-regression` と、120 秒の evict-race で報告 0 件、`dev-notes/lockdep-kernel.md`)。

確認済み (2026-10-27): `make test-kunit` (UML、41 件、`s3k_hit_delta_once` / `s3k_hit_replay_order` を含む) PASS、`INTERLEAVE=1` の取り直し (評価の間隔は効かない)、gate (b) (`hit_accum` と無関係の同点)、G5 (large の `on_hit` が 1 access あたり約 82 回から高々 1 回)。数値は `journal/benchmark-results.md` §18.45。
