# AC-66: W-TinyLFU を `pick_victim` で動かす — 変更の検討と計画

状態: **実装済み (2026-10-20)**。ライブの計測は 1 回だけ (§9.4)。計画は 2026-10-19、実装でどう決めたかと結果は §9。前提は pcache モード (`--arena-metadata-only`、`docs/design/pcache-mode.md`) と AC-58 の `pick_victim` (`docs/design/ac58-pick-victim.md`)。ロック順と不変条件は `docs/kmod-pcache-invariants.md` が正本。

## 1. 結論

- W-TinyLFU は **AC-58 の vtable (`on_insert` / `on_remove` / `pick_victim` / `on_hit`) のまま、BPF 側だけでほぼ書ける**。admission の「候補を入れない」は、populate の後に「候補を victim にする」として表せる (§3 G1)。
- kmod に要る変更は 1 つだけで、**ino ごとのバイト数を policy に渡す member (`on_size`) を vtable の末尾に足す** (§5)。window / main の予算を件数ではなくバイトで持つため。ロック順と不変条件は変わらない。
- 1 段目は kmod を変えずに件数ベースで作り (ベンチのファイルは等サイズ)、hit rate を確かめてから 2 段目でバイトにする (§7)。
- 既存の `wtinylfu_ops` / `wtinylfu_full_ops` は score 型の近似 (window を「round 数」で、main を「CMS が閾値未満なら evict 票」で表している) で、比較用に残す。新しい policy は `policy_wtinylfu_pick_ops.bpf.c`。

## 2. W-TinyLFU の要素 (Caffeine の形)

| 要素 | 役割 | 既定 |
|---|---|---|
| window (LRU) | 新しい item を最初に入れる。recency で守る | 容量の 1% |
| main = SLRU | probation (main の 20%) と protected (80%)。probation で hit したら protected へ | — |
| frequency sketch | 4-bit の Count-Min sketch。evict 済みの item の頻度も覚える (ghost の代わり) | 幅 = 容量の item 数の 2 冪 |
| doorkeeper | 1 回しか来ない item で sketch を汚さない Bloom filter | 任意 |
| reset (aging) | 加算が sample size (= 10 × 容量の item 数) に達したら全 counter を半分にし、doorkeeper を消す | — |
| admission | window から押し出された候補と main の victim (probation の LRU) の頻度を比べ、低い方を捨てる | 同点は候補を捨てる |

## 3. 今の仕組みに当てはめたときのずれ

| # | ずれ | どうするか |
|---|---|---|
| G1 | **admission が populate の後**。miss は先に chunk を populate し PCACHE entry を入れてから `admit_replace` が来る (AC-45 C2 の `enforceCapacityAfterPopulate`)。S3 からの取得はもう済んでいる | 「入れない」= 候補を `pick_victim` で返す。コスト (GET) は払った後だが、TinyLFU の目的 (main の中身を 1 回きりの item から守る) はこれで満たせる。取得の前に断る形 (§8) は範囲外 |
| G2 | **新しい ino は最初の capacity pass では必ず pending**。refresh は signal の前なので、候補を返しても kmod は丸ごと evict せず shed する (pin された chunk は残る)。AC-58 の約束では BPF は pick した ino を構造から外すので、次の round の `on_insert` で「新規」として window に入り直す | BPF 側に **limbo** (pick したが `on_remove` が来ていない ino と、その時の区画) を持つ。`on_insert` で limbo から戻ってきた ino は元の区画に戻す。admission で負けて戻ってきた ino は `rejected` 区画に入れ、次の `pick_victim` で最初に返す |
| G3 | **policy は ino のバイト数を知らない**。kmod の課金は pcache なら `fd->cap` (`s3fs_ino_buf_bytes`)、arena なら entry の合計。`s3fs_kfunc_ino_size` は「どれか 1 つの range のサイズ」で、課金と一致しない (`s3fs_admit_size` のコメント) | 2 段目で `on_size(ino, bytes)` を足す (§5)。1 段目は 1 ino = 1 単位、容量 = `capacity_bytes / 平均サイズ` を config map で daemon が渡す |
| G4 | **`on_hit` は並行に来て lock を持たない**。probation → protected の移動や LRU の先頭への移動をその場でできない (AC-58 §2 と同じ制約) | `on_hit` は sketch の加算と ino ごとの `hit` ビットだけ。区画の移動は `on_insert` / `pick_victim` (kmod の mutex の中) で遅延して行う。各区画の LRU は SIEVE / CLOCK の近似 (hit ビットで 1 周の猶予) にする。厳密な LRU ではない点を結果に書く |
| G5 | **頻度の信号の粒度**。`on_hit` は `fentry/generic_file_read_iter` から (CPU, ino) ごとに jiffy 1 回に間引かれる (AC-47)。1 回の `read(2)` の複数の read_iter はまとまるが、大きいファイルを順に読むと jiffy をまたぐたびに加算され、頻度が水増しされる。`.read_folio` の on_hit と二重にもなる | 1 段目はそのまま (bench は小さいファイル)。sketch の counter は 4-bit で飽和するので害は限られる。`bench-hitrate-mixed` で大きいファイルが不当に残るなら、2 段目で「pos == 0 の read だけ数える」fastpath の flag を足すか、`on_hit` に `pos` を渡すかを測って決める |
| G6 | **aging の契機**。既存の full 版は `on_admit` の回数 (round 数) で半減させるが、round の頻度は miss と eval で変わり、アクセス数と結びつかない | `on_hit` で加算回数の counter を atomic に増やし、sample size に達したら flag を立てるだけにする。半減 (`bpf_loop`) は次の `on_insert` / `pick_victim` の中で行う (read 経路で sketch 全体を回さない) |
| G7 | **evict 済み ino の扱い**。capacity 以外の経路 (sync の prune、truncate、別 round) で消えた ino は AC-64 の聞き直しで `on_remove` が来る | `on_remove` は区画と limbo から外すが、sketch は消さない (再来時の admission に使う) |
| G8 | **eviction は round 単位**で、アクセスごとではない。1 round の新規 ino は Phase 1 の配列の順 (arena の entry → pcache の ino) で `on_insert` される | 他の `*_pick_ops` と同じ近似。pcache モードでは miss ごとに refresh するので、1 round の新規はたいてい 1 つ |
| G9 | **capacity より大きい ino**。全体 admit にはならず shed される (`fullAdmitExceedsCapacity`) | 2 段目: `on_size` のバイトが window の予算を超える候補は window を飛ばして probation の候補にし、main の予算を超えるなら即座に候補を返す (Caffeine の weigher と同じ扱い) |

## 4. BPF 側の設計 (`policy_wtinylfu_pick_ops.bpf.c`)

### 4.1 状態

- `node[N]` (ARRAY、N=4096): `{ino, seg, hit, size, seq}`。`seg` は WINDOW / PROBATION / PROTECTED / LIMBO / REJECTED。
- `idx` (HASH): ino → node の位置。
- 区画ごとの ring (S3-FIFO の `*_pick_ops` と同じ作り) と、区画ごとの合計 (件数、2 段目はバイト)。
- `sketch` (ARRAY、u8 × 4 行 × 幅): 幅は config で決め、daemon が load 前に `bpf_map__set_max_entries` で合わせる。既定は 1024 × 4。
- `dk` (doorkeeper、1 段目は既存 full 版の 4096 bit を流用)。
- `cfg` (ARRAY): `window_pct` (既定 1、ただし最低 1 件)、`protected_pct` (80)、`capacity_units` (1 段目)、`sample_factor` (10)。
- `st`: 加算回数、aging の flag、統計 (admission の勝ち負け、limbo からの復帰数)。

### 4.2 各 member

- `on_hit(ino)`: doorkeeper を通ったら sketch を加算、加算回数を増やし、sample size に達したら aging の flag。`idx` にあれば `hit = 1`。map の lookup と store だけで、区画は動かさない。
- `on_insert(ino)`: aging の flag があれば先に半減。`idx` にあり LIMBO なら元の区画へ戻す (G2)。rejected の印があれば REJECTED 区画へ入れる (同じ round の中では LIMBO のままなので、2 回返さない)。なければ WINDOW の先頭に入れる。window が予算を超えたら、window の CLOCK で押し出した ino を probation の「候補」にする (この時点では捨てない)。
- `pick_victim()`:
  1. REJECTED があれば、その ino を LIMBO にして返す。
  2. 候補 (window から押し出された ino) があれば、probation の CLOCK が選ぶ victim (probation が空なら protected) と sketch の推定値を比べる。候補が高ければ victim を返し、候補は probation に入る。そうでなければ候補を返し、rejected の印を付けて LIMBO に入れる。
  3. 候補がなければ、probation の CLOCK (hit = 1 なら protected へ移して hit = 0、protected が予算を超えたら protected の CLOCK で押し出した ino を probation に戻す) で victim を返す。probation も空なら window から。
  4. 返す ino は LIMBO に移す (AC-58 の約束の「構造から外す」を、区画を覚えたまま行う)。何もなければ 0 (kmod は score 順に戻る)。
- `on_remove(ino)`: 区画 / LIMBO から外して `idx` から消す。sketch は残す。
- `on_evict(ino)`: score 順に戻ったとき用に、sketch の推定値が低いほど高い score を返す (pick と順位を揃える)。

kmod は 1 round で最大 `ncand` 回 `pick_victim` を呼び、試し済みの ino が返ると score 順に移る。LIMBO の ino を同じ round で 2 回返さないので、この条件には当たらない。

## 5. kmod の変更 (2 段目)

- `struct s3fs_policy_ops` の**末尾**に `int (*on_size)(u64 ino, u64 bytes);` を足す (既存の BPF プログラムは名前で bind するので、そのまま load できる)。`kmod/policy_struct_ops.h` / `.c` に dispatcher と既定の stub を足す。
- 呼ぶ場所は `s3fs_ebpf_admit_replace` の Phase 1、`s3fs_policy_ops_insert(ino)` の直後 (vtable が `pick_victim` を持ち `cap_bytes > 0` のときだけ)。バイト数は live_bytes の計算で使った値と同じもの (arena は entry の合計、pcache は `s3fs_ino_buf_bytes`)。今は live_bytes のループで ino ごとの値を捨てているので、`eval_inos[]` と並べて持つ。
- `s3fs_policy_evict_lock` の中で呼ぶので、BPF 側は lock なしで合計を更新できる。新しいロックはなく、ロック順 (policy mutex → fd->lock → admits_lock) も変わらない。`s3fs_ino_buf_bytes` は Phase 1 の前 (mutex の外) で計算済みの値を使い、mutex の中で fd->lock を新しく取らない。
- shed で round の途中にバイトが減っても、policy の合計は次の round の `on_size` で直る。round の中の停止条件は今どおり kmod の `live_bytes` が決める。
- `docs/kmod-pcache-invariants.md` の「policy の pick_victim (AC-58)」に `on_size` の呼び方を 1 行足す (同じ変更で)。`docs/design/ac58-pick-victim.md` §2 にも追記する。

## 6. daemon と周辺

- `daemon/internal/policyprog/loader.go`: `builtin:policy_wtinylfu_pick_ops` を登録し、`wtinylfu_pick_config` を `SetConfigU64` で書く (`--cache-capacity` から 1 段目の `capacity_units` を出す)。sketch の幅は load 前に設定する。
- `bpf/Makefile` に追加。`scripts/bench-hitrate.sh` の `POLICIES` と `bench-policy-cost.sh` の `ARMS` に入れる。
- `dev trace-replay` (`daemon/internal/trace/sim.go`) に W-TinyLFU と S3-FIFO の参照実装を足す (`docs/design/trace-harness.md` の残り)。BPF 版との差 (round 単位、CLOCK 近似、limbo) を見るため、`meetup/vtable_model.py` 型の round モデルにも同じ policy を足す。

## 7. 手順

| 段 | 内容 | 終わりの条件 |
|---|---|---|
| 0 | trace-replay と round モデルに W-TinyLFU を足し、30 files / 8 slot の Zipf と、Zipf に一度きりの scan を混ぜた trace で S3-FIFO / LRU / SIEVE と比べる | 参照実装で W-TinyLFU ≥ S3-FIFO (Zipf)、scan 混在で明確に上。round モデルで BPF の近似の損失が見える |
| 1 | `policy_wtinylfu_pick_ops` を件数ベースで実装 (kmod 変更なし)。daemon の登録と bench への追加 | `bench-hitrate` 3 seed で S3-FIFO の 36.7〜40.0% と並べる。`bench-policy-cost` の 64 jobs が S3-FIFO 並み (on_hit は lookup 2〜3 回)。`bpf_stats` で `pick_victim` / `on_insert` の時間。`test-pick-victim-stale` を新 policy でも通す |
| 2 | kmod に `on_size` を足し、予算をバイトにする。G9 の扱いを入れる | `bench-hitrate-mixed` で、頻度の高い大きいファイルを残し 1 回きりの大きいファイルを入れない。`make test-regression` が通る。invariants メモと AC-58 の doc を更新 |
| 3 | (必要なら) G5 の頻度の粒度と、window の割合の調整 (hill climbing は入れず config の固定値) | 結果を `docs/journal/benchmark-results.md` に書き、ここに要約する |

## 8. 範囲外とリスク

- **取得の前に断る admission** (daemon が sketch を読んで「この miss は cache しない」と決め、populate せずに読み出しだけ返す) は、pcache モードでは read が chunk を経由するので経路が要る。G1 の形で効果が足りないと分かってから考える。
- **小さい容量**: bench の 8 slot では window の 1% は 1 件未満なので最低 1 件にする。ここでの数字は大きい容量の挙動を代表しない (trace-replay で容量を変えて確かめる)。
- **live の eval 間隔**: AC-58 §6 のとおり、ライブは 25 access ごとにしか eval を回さず、trace-replay の値には届かない。W-TinyLFU も同じ。
- **区画の LRU が近似**であること (G4) と、limbo の ino が多いとき `node[]` を圧迫すること。limbo は `on_remove` か次の `on_insert` で必ず抜けるが、あふれたら最古の limbo を捨てる。

## 9. 実装 (2026-10-20)

段 1 と段 2 を一度に入れた (kmod に `on_size` を足したので、件数ベースの段を別に出す意味がなくなった)。ライブの `bench-hitrate` と `bench-policy-cost` はまだ回していない (この変更はカーネルのない環境で作った)。

### 9.1 入ったもの

- `bpf/policy_wtinylfu_pick_ops.bpf.c`: §4 の設計どおり。W / P / T / X (rejected) / L (limbo) の 5 つの ring、sketch (1024 × 4、u8 の 4-bit counter)、候補は `st.cand` (slot と seq) で持つ。各 member の中身は `do_*` の inline 関数で、struct_ops の program はそれを呼ぶだけ (harness から同じ関数を呼ぶため)。
- kmod: `struct s3fs_policy_ops` の末尾に `on_size(ino, bytes)`。`s3fs_ebpf_admit_replace` の Phase 1 で、`pick_victim` を持つ policy かつ `capacity_bytes > 0` のとき、`on_insert` の直後に呼ぶ。arena はその ino の entry の合計、pcache は `s3fs_ino_buf_bytes` (live_bytes と同じ数え方)。`s3fs_ino_buf_bytes` は `ilookup` と `READ_ONCE(fd->cap)` だけで fd->lock を取らないので、policy の mutex の中で呼んでもロック順は変わらない。
- `dev trace-replay` に `s3fifo` / `wtinylfu` (厳密な LRU の参照実装) / `wtinylfu-clock` (BPF 版を手順どおりに写したもの) を追加 (`daemon/internal/trace/sim_wtinylfu.go`)。
- `make test-wtinylfu-harness` (`scripts/test-wtinylfu-harness.sh`): policy の操作を `SEC("syscall")` の program として load し (kmod も mount も要らない、root だけ要る)、Go から trace を流す。verifier を通ること、BPF 版の hit 数が `wtinylfu-clock` と一致すること (件数 / 4 KiB の重み、容量 2 / 8 / 32)、shed で残った ino が limbo から戻っても常に resident な ino を返すことを確かめる。

### 9.2 計画から変えたこと

- **config map は作らなかった**。window 1 %、protected 80 %、sample = 10 × resident (最低 16)、sketch の幅 1024 は定数。調整が要ると分かってから足す。
- **doorkeeper は入れなかった**。Caffeine の既定にもなく、小さい容量では 1 回目の hit を捨てる害の方が大きい。
- **window の予算はバイトで、最低 1 件の床がない**。容量 8 件 (4 KiB ずつ) なら 1 % は 327 B で、新しい ino はすぐ probation の候補になる (Caffeine の weigher と同じ)。件数モードでは 1 件の床がある。
- **G9 (容量を超える ino の特別扱い) は入れなかった**。daemon が capacity を超えるファイルを全体 admit にしないので (`fullAdmitExceedsCapacity`)、policy から見える重みは容量を超えない。
- **G5 (頻度の粒度) は触っていない**。`bench-hitrate-mixed` を回してから決める。
- duel に勝った候補は probation に残り、hit すれば protected に上がる。probation の最古が候補自身のときは duel をせず、そのまま victim にする。

### 9.3 trace-replay の結果

`dev trace-replay`、容量は件数 (`--softcap`)。scan 混在は Zipf(0.9, 200 files) の 2,000 read ごとに一度きりの ino を 4 × 容量個読む trace (`zipfWithScans`、テストにある)。

| trace | 容量 | LRU | SIEVE | S3-FIFO | W-TinyLFU (参照) | W-TinyLFU (BPF 版) | OPT |
|---|---|---|---|---|---|---|---|
| Zipf(0.7) 30 files / 2,000 | 8 | 40.9 | 48.0 | 49.0 | 49.9 | 50.0 | 64.7 |
| Zipf(0.9) 2,000 files / 50,000 | 16 | 15.3 | 27.8 | 30.0 | 28.5 | 29.3 | 37.0 |
| 同上 | 256 | 54.6 | 62.0 | 62.2 | 60.5 | 60.5 | 74.2 |
| scan 混在 / 40,000 | 8 | 20.0 | 31.6 | 34.5 | 33.3 | 34.2 | 43.4 |
| 同上 | 32 | 45.0 | 54.2 | 53.7 | 53.7 | 55.3 | 66.3 |
| 同上 | 128 | 65.8 | 70.1 | 69.9 | 66.5 | 66.3 | 75.4 |

- §7 の段 0 の条件 (W-TinyLFU ≥ S3-FIFO) は、bench と同じ形 (30 files / 容量 8) と scan 混在の小さい容量では満たすが、大きい容量では S3-FIFO と SIEVE に 1〜4 ポイント負ける。静的な Zipf では SIEVE / S3-FIFO も頻度の高いものを残せるので、W-TinyLFU の利点は小さい (S3-FIFO の論文の結果と同じ傾向)。window の割合を固定 1 % にしていることと、sketch の reset が 10 × 容量のアクセスごとと短いことが効いている。Caffeine は window の大きさを hill climbing で動かす。
- BPF 版 (CLOCK、件数でなくバイトの予算) は参照実装と ±1.6 ポイントで、近似による損は見えない。

### 9.4 実機 (2026-10-20、ma さんの環境)

- `make kmod` は成功 (gcc のバージョン差の既知の警告だけ)。`make test-regression` は全部 PASS (`test-pick-victim-stale` / `test-pattern-aware-admit` を含む)。`on_size` に起因する問題は見つかっていない。
- `bench-hitrate` (30 files、Zipf α=0.7、2,000 access、softcap=8、1 回):

| policy | hit_rate | miss | err | ms/access |
|---|---|---|---|---|
| `s3fifo_pick_ops` | 37.80% | 1244 | 0 | 6.56 |
| `wtinylfu_pick_ops` | 38.55% | 1229 | 0 | 6.70 |

- W-TinyLFU が 0.75 ポイント (15 miss) 上で、trace-replay と同じ向き。1 回なので有意とは言えない。ライブが trace-replay より低いのは AC-58 §6 と同じ (eval が 25 access ごと、prewarm 済み)。

### 9.5 残り

- `bench-hitrate` を 3 seed、`bench-hitrate-mixed` (大きいファイルの扱い、G5)、`bench-policy-cost` (on_hit の read 経路のコスト) を回す。
- 必要なら window の割合と sample の倍数を config map にする。
