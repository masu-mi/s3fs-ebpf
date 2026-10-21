# Writing cache policies for s3fs-ebpf

s3fs-ebpf のキャッシュポリシーを書く人向けの実装ガイド。**Meetup
発表資料ではなく、実際に手を動かして policy を試す人**が対象。
概念的な architecture は `docs/design/policy-engine-design.md`、Meetup
narrative は `docs/user/motivation.md §0` を見てほしい。

## 目次

- [Which layer should I write?](#which-layer-should-i-write)
- [Layer 1 — CEL DSL でルールを書く](#layer-1--cel-dsl-でルールを書く)
- [Layer 2 — built-in eBPF policy を tuning する](#layer-2--built-in-ebpf-policy-を-tuning-する)
- [Layer 3 — 独自 eBPF policy を書く](#layer-3--独自-ebpf-policy-を書く)
- [Layer 0 — KFunc primitives 参照](#layer-0--kfunc-primitives-参照)
- [Hot swap ワークフロー](#hot-swap-ワークフロー)
- [Allowlist で user policy を承認する](#allowlist-で-user-policy-を承認する)
- [Trace-replay で offline に試す](#trace-replay-で-offline-に試す)
- [Debugging / trouble shooting](#debugging--trouble-shooting)

## Which layer should I write?

まず「どのレイヤーが要件に合うか」を決める。判断表:

| やりたいこと | Layer | 難易度 | 例 |
|---|---|---|---|
| ファイル名や size で prewarm/skip を切替 | **1 (CEL)** | 低 | `entry.size <= 32*1024*1024 && !entry.name.endsWith(".log")` |
| 動作時に使う built-in policy を差替え | **1 (CEL)** | 低 | `policy: "builtin:policy_sieve_ops"` |
| policy 判定入力を Prometheus に出力 | **1 (CEL)** | 低 | `metric("last_size", entry.size)` |
| built-in policy を選ぶ + tuning したい | **2 (rebuild)** | 中 | `SMALL_THRESHOLD` を大きくして SIEVE のグレースを長く |
| 完全新規の cache algorithm を作りたい | **3 (user eBPF)** | 高 | LIRS 実装、tenant-specific tuning |
| policy から kmod-side state を読みたい | **Layer 0** | 中 | pressure_ops のように admit_count を読む |

- **Layer 1** は CEL 式を書くだけで再ビルド不要。runtime に反映
- **Layer 2** は既存の `.bpf.c` を編集 → `make bpf` → `POST /policy/reload`。built-in なので allowlist 不要
- **Layer 3** は独自 `.bpf.o` を書いて `--policy-bpf=file:<path>` で load。allowlist で SHA256 承認が必要
- **Layer 0** は KFunc set が Layer 2/3 の primitives (`s3fs_kfunc_*()`)。書き足すには kmod を編集

## Layer 1 — CEL DSL でルールを書く

CEL (Google の Common Expression Language、K8s admission controller
と同じ stack) で書く。cold path のみで評価されるので、hot read
path には出ない。

### ファイル形式

`policies/*.cel` — `hook: <式>` の並び。行頭の `#` はコメント。空白 (space / tab) で始まる行は前の行の式の続きとして連結するので、長い式は複数行に分けて書ける。式の途中 (行末) に `#` のコメントは書けない (CEL の構文エラーになる)。`policies/*.cel` はすべて `go test ./internal/policy/` で読み込めることを確かめている。

```cel
# policies/mypolicy.cel

# prewarm: bool — daemon 起動時に entry を prewarm するか
# デフォルト (未定義) = true
prewarm: entry.size <= 33554432 && !entry.name.endsWith(".log")

# policy: string — 起動時に load する Layer 2 program
# --policy-bpf flag より優先
policy: "builtin:policy_wtinylfu_ops"

# prefetch_next: int — read miss のあと、同じディレクトリの名前順で
# 次の何個を先読みするか (AC-68、pcache モードだけ。デフォルト 0、上限 64)
prefetch_next: entry.s3_key.startsWith("datasets/") ? 2 : 0
```

`prefetch_next` は、ファイルの最初の chunk の miss を処理したあとに、そのファイルを `entry` として評価する。daemon はマウント経由で兄弟を読み、page cache に載せる (`docs/design/pcache-mode.md` §4.1)。学習データの shard や連番のログのように、名前順に読まれるファイルに向く。名前順に読まれないディレクトリでは外れた先読みが容量を使うので 0 にする。

### 使える変数と関数

| 変数 / 関数 | 型 | 意味 |
|---|---|---|
| `entry.ino` | uint64 | daemon が振った ino (run 内 stable) |
| `entry.name` | string | dentry basename |
| `entry.s3_key` | string | 完全な S3 key |
| `entry.size` | uint64 | S3 stat サイズ (byte) |
| `entry.mode` | uint64 | Unix mode bits |
| `metric(name, value)` | (string, dyn) → bool | Prometheus gauge を発火 (side-effect) |

`metric()` は必ず true を返すので `&&` chain に組み込める:

```cel
# entry の size を metric に出す + prewarm 判定
prewarm: metric("size_bytes", entry.size)
      && metric("size_mib", double(entry.size) / 1048576.0)
      && entry.size <= 33554432
```

CEL 標準の string / int / bool 演算子は全て使える (`entry.name.matches("^model_.*")`
等)。CEL リファレンス: https://cel.dev/

### metric() の制約

- 同時に持てる metric 名は最大 32 個 (`CustomGaugeCap`)。溢れは silent drop
- gauge のみ (counter/histogram は他の hand-rolled 経路を使う)
- Prometheus 名は `s3fs_ebpf_policy_metric{name="<name>"}` に自動で bind

### 使い方

```bash
# daemon 起動時に load
sudo bin/s3fs-ebpf fastpath --s3 \
    --policy=policies/mypolicy.cel \
    --metrics-addr=:9600
```

CEL policy を書き換えても daemon は再起動が要る (CEL は startup 1 回読み)。

### よくある prewarm レシピ

下の例はすべてコピペして `policies/*.cel` に置いてそのまま使える。
`metric()` を付けると Prometheus で warm/skip の判定理由が可視化できる。

#### レシピ 1: 全ファイルを warm する (最小構成)

arena サイズが十分大きく、まずは全部 preload したい場合の出発点。

```cel
# policies/prewarm-all.cel
prewarm: true
policy: "builtin:policy_wtinylfu_ops"
```

```bash
# kmod ロード + マウント
sudo insmod kmod/s3fs_kfunc.ko
sudo mount -t s3fs_ebpf none /mnt/s3demo

# daemon 起動 — 全ファイルを prewarm
sudo bin/s3fs-ebpf fastpath --s3 \
    --policy=policies/prewarm-all.cel \
    --metrics-addr=:9600

# 確認: prewarm ログに全 ino が "[prewarm] ino=... body=..." で出る
# 確認: stat 後に fast_hits が上がる
curl -s http://localhost:9600/metrics | grep s3fs_ebpf_fast_path_hits_total
```

> **注**: `prewarm: true` (宣言なし) がデフォルト動作なので、
> 最終的に「全 warm」にしたいなら policy ファイル自体を省略できる。
> ただし `policy:` 行だけを書きたいとき (Layer 2 program を固定したい
> が prewarm は全部) は `prewarm: true` を明示する。

---

#### レシピ 2: S3 key prefix で warm 対象を絞る

`models/` 以下のモデルウェイトだけを startup 時に preload し、
ログ・設定・その他は on-demand (Path C) にする ML serving 向け例。

```cel
# policies/prewarm-models.cel
prewarm: metric("last_model_size_bytes", entry.size)
      && entry.s3_key.startsWith("models/")
      && (entry.name.endsWith(".pt")
          || entry.name.endsWith(".bin")
          || entry.name.endsWith(".pkl")
          || entry.name.endsWith(".onnx"))
      && entry.size <= 2147483648

policy: "builtin:policy_wtinylfu_ops"
```

`entry.s3_key` はバケット root からのフルパスなので、`"hot-data/"` や
`"datasets/train/"` のような深い prefix にも使える。

`policies/prewarm-models.cel` として既に用意してある。

```bash
# daemon 起動
sudo bin/s3fs-ebpf fastpath --s3 \
    --policy=policies/prewarm-models.cel \
    --metrics-addr=:9600

# 確認1: models/ 配下のみ "[prewarm] ino=... body=..." が出る
#        models/ 以外は "[prewarm] ino=... content-skip (policy=false ...)" になる

# 確認2: prewarm 判定サイズを Prometheus で観察
curl -s http://localhost:9600/metrics \
    | grep 'policy_metric{name="last_model_size_bytes"}'

# 確認3: models/weights.bin を cat → fast_hits が増える (arena hit)
#        other/config.json  を cat → slow_misses → upcall → arena admit
stat /mnt/s3demo/models/weights.bin   # Fast Path Hit
stat /mnt/s3demo/other/config.json    # Slow Path Miss (first access)
curl -s http://localhost:9600/metrics \
    | grep -E 'fast_path_hits|slow_path_misses'
```

---

#### レシピ 3: ファイル拡張子 + サイズで 3 段階に分ける

小ファイルは全数 warm、分析フォーマット (parquet / arrow 等) は中サイズまで
warm、大ファイル・ログは on-demand にするバランス型。

```cel
# policies/prewarm-tiered.cel
# Tier 1: 64 KiB 以下は全部
# Tier 2: ~128 MiB かつ分析フォーマット
prewarm: (entry.size <= 65536)
      || (entry.size <= 134217728
          && (entry.name.endsWith(".parquet")
              || entry.name.endsWith(".arrow")
              || entry.name.endsWith(".csv")
              || entry.name.endsWith(".bin")
              || entry.name.endsWith(".pt"))
          && !entry.name.endsWith(".log")
          && !entry.name.endsWith(".tmp"))
# Tier 3: 128 MiB 超 or .log は prewarm しない
# → --pattern-aware-admit で sequential と判定された時だけ full admit

policy: "builtin:policy_wtinylfu_ops"
```

`policies/prewarm-tiered.cel` として既に用意してある。

```bash
# daemon 起動
sudo bin/s3fs-ebpf fastpath --s3 \
    --policy=policies/prewarm-tiered.cel \
    --metrics-addr=:9600

# 確認: admits_current がどのファイルを warm したか
#   Tier 1 (≤64 KiB)  → prewarm ログに body= あり
#   Tier 2 (.parquet 等) → 同上
#   Tier 3 (.log / 大) → "content-skip (policy=false ...)" ログ
grep "\[prewarm\]" /tmp/bench/fastpath.log | grep -E "body=|content-skip"

# 確認: arena 使用量がどれだけ増えたか
curl -s http://localhost:9600/metrics \
    | grep -E 'arena_used_bytes|arena_capacity_bytes|admits_current'
```

---

#### レシピ 4: pattern-aware admit と組み合わせる

CEL `prewarm: false` で content は warm しないが、
`--pattern-aware-admit` を有効にすると初回の sequential read-miss で
自動的に full admit に昇格できる。「起動時はメタデータだけ warm、
実際に読まれたら全体を arena に入れる」という二段構え。

```cel
# policies/prewarm-meta-only.cel
# stat は warm (metadata always warm、docs/design/read-path-design.md §F-03)
# content は読まれるまで admit しない
prewarm: false
```

```bash
# daemon 起動 — content は on-demand、sequential 判定で full admit に昇格
sudo bin/s3fs-ebpf fastpath --s3 \
    --policy=policies/prewarm-meta-only.cel \
    --pattern-aware-admit \
    --metrics-addr=:9600

# 確認1: 起動直後は content-skip が全 ino 分出る
grep "content-skip" /tmp/bench/fastpath.log

# 確認2: 大ファイルを cat → upcall + "pattern-aware full admit" ログ
cat /mnt/s3demo/big.bin > /dev/null
grep "pattern-aware full admit" /tmp/bench/fastpath.log

# 確認3: stat は最初から Fast Path (metadata warm 済み)
stat /mnt/s3demo/big.bin
curl -s http://localhost:9600/metrics | grep fast_path_hits_total
```

> **仕組み**: `prewarm=false` でも `prewarmEntry` の step 1 (metadata warm)
> は必ず走り、BPF `inode_index` に登録される。stat は Path A のまま高速。
> content は first read-miss → `handleReadMiss` → `SeqRatio() ≥ 0.70` で
> full-file GET という流れ。詳細は `docs/design/ebpf-hook-gaps.md §3.4`。

---

#### レシピ 5: 除外 + 複数の条件を組み合わせる (学習ジョブ向け)

学習データ、モデル、ログ、作業中のファイルが混在するバケットで、ジョブが起動直後に必ず読むものだけを prewarm する。正規表現 (`matches`、RE2)、リスト定数と `in`、マクロ `exists`、複数行の式を使う、少し複雑な例。

```cel
# policies/prewarm-dataset.cel (抜粋)
prewarm: metric("prewarm_input_bytes", entry.size)
    && !entry.name.startsWith(".")
    && ![".log", ".tmp", ".part"].exists(ext, entry.name.endsWith(ext))
    && !entry.s3_key.matches("(^|/)(_tmp|_staging|archive)/")
    && (entry.name in ["_SUCCESS", "manifest.json", "schema.json", "index.json"]
        || (entry.s3_key.matches("^datasets/[^/]+/(train|val)/")
            && entry.size <= 8388608u
            && [".parquet", ".arrow", ".tfrecord"].exists(ext, entry.name.endsWith(ext)))
        || (entry.s3_key.matches("^models/[^/]+/latest/")
            && entry.size <= 2147483648u)
        || entry.size <= 4096u)

policy: "builtin:policy_s3fifo_pick_ops"
```

| S3 key | size | 判定 | 理由 |
|---|---|---|---|
| `datasets/imagenet/train/shard-0001.parquet` | 6 MiB | warm | train 配下の shard、8 MiB 以下 |
| `datasets/imagenet/train/shard-0002.parquet` | 64 MiB | skip | 8 MiB 超 |
| `datasets/imagenet/test/shard-0001.parquet` | 1 MiB | skip | train / val 以外 |
| `datasets/imagenet/manifest.json` | 200 KiB | warm | 目印のファイル |
| `models/resnet/latest/weights.pt` | 1.5 GiB | warm | latest 配下、2 GiB 以下 |
| `models/resnet/v1/weights.pt` | 1.5 GiB | skip | latest 以外 |
| `configs/train.yaml` | 900 B | warm | 4 KiB 以下 |
| `logs/run1/stdout.log` | 100 B | skip | `.log` は除外が先に効く |
| `datasets/_staging/train/shard-0001.parquet` | 1 MiB | skip | `_staging/` 配下 |

この表は `daemon/internal/policy/policy_test.go` の `TestPrewarmDatasetPolicy` で確かめている (評価だけで、マウントしての計測はしていない)。

注意:
- サイズの定数に `u` を付けると `entry.size` (uint) と型がそろう。付けなくても比較はできる。
- prewarm は daemon 起動時に 1 回だけ評価する。実行中に増えたファイルには効かない。
- pcache モード (`--arena-metadata-only`) は既定では中身の prewarm をしない (AC-44)。`--pcache-prewarm` を付けると、`prewarm` が true のファイルを daemon が起動後にマウント経由で 1 回読む (AC-67、`docs/design/pcache-mode.md` §4.1)。容量の既定は無制限なので `--cache-capacity` も付ける。

## Layer 2 — built-in eBPF policy を tuning する

10 種の built-in が `bpf/policy_*_ops.bpf.c` にある。それぞれの
`#define` を編集して `make bpf` → hot reload できる。

### 出荷している built-in 一覧

| policy | ファイル | 特徴 | bench 実測 (Zipf α=0.7) |
|---|---|---|---|
| `observe` | `policy_observe.bpf.c` | 何も evict しない baseline | 100 % (=ceiling) |
| `wtinylfu_ops` | `policy_wtinylfu_ops.bpf.c` | CMS + bpf_loop aging (default) | **88.5 %** |
| `size_aware_ops` | `policy_size_aware_ops.bpf.c` | SMALL/BIG bias (KFunc: ino_size) | 66 % |
| `sieve_ops` | `policy_sieve_ops.bpf.c` | 1-bit second-chance clock (Zhang 2024) | 65 % |
| `s3fifo_ops` | `policy_s3fifo_ops.bpf.c` | 3-tier {S/M/G} (Yang 2023) | 64 % |
| `pressure_ops` | `policy_pressure_ops.bpf.c` | adaptive (KFunc: admit_count) | 52 % |
| `wtinylfu_full_ops` | `policy_wtinylfu_full_ops.bpf.c` | paper-complete W-TinyLFU | 49.5 % ※ |
| `time_decay_ops` | `policy_time_decay_ops.bpf.c` | time-based (KFunc: time_ns) | 35 % |
| `lru_ops` | `policy_lru_ops.bpf.c` | age-based FIFO (近似 LRU) | 13 % |
| `freq2bit_ops` | `policy_freq2bit_ops.bpf.c` | 2-bit reference counter | 10.5 % |

※ Full W-TinyLFU が scoped 版より下なのは workload sensitivity — 詳細は
`docs/journal/hit-rate-bench.md` の "The W-TinyLFU paradox" 参照。

### tuning できるパラメタの例

- `policy_wtinylfu_ops`:
  - `CMS_W`, `CMS_D` — sketch 幅×深さ (default 256×4 = 1 KiB)
  - `EVICT_THRESHOLD` — CMS 見積 < N で evict (default 2)
  - `AGE_INTERVAL` — halve every N admit rounds (default 4)
- `policy_wtinylfu_full_ops` (paper-complete):
  - `WINDOW_MAX_ROUNDS` — Window tier に留まる round 数 (default 8)
  - `PROMOTE_THRESHOLD` — W → M 昇格の CMS 閾値 (default 1)
  - `DK_RESET_INTERVAL` — doorkeeper reset 頻度 (default 16)
- `policy_sieve_ops`:
  - `SIEVE_VISITED_MAX` — visited bit の上限 (default 1、1-bit clock)
- `policy_size_aware_ops`:
  - `SMALL_THRESHOLD` — SMALL / BIG 境界 byte 数 (default 4096)
  - `BIG_MISS_CAP` — BIG file のグレース round 数 (default 2)

### tuning ワークフロー

```bash
# 1. パラメタを編集
$EDITOR bpf/policy_sieve_ops.bpf.c
# e.g. #define SIEVE_VISITED_MAX 3 に上げて 3-bit clock 化

# 2. 再ビルド
make bpf

# 3. hot reload (daemon 再起動なし)
curl -X POST http://localhost:9600/policy/reload \
     -d 'builtin:policy_sieve_ops'

# 4. bench で挙動確認
POLICIES=sieve_ops ./scripts/bench-hitrate.sh
```

## Layer 3 — 独自 eBPF policy を書く

paper-complete impl や tenant-specific tuning など、built-in で
表現できない algorithm を書きたい場合。

### 最小 template

built-in の最も単純な `policy_sieve_ops.bpf.c` を copy して
改造するのが早い。骨子:

```c
// SPDX-License-Identifier: GPL-2.0
//
// policy_mypolicy_ops — 自作 policy の template。
// Load: bin/s3fs-ebpf fastpath --policy-bpf=file:/path/to/policy_mypolicy_ops.bpf.o

#include "vmlinux.h"
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_tracing.h>

// vtable 定義 — kmod/policy_struct_ops.h と一致させる
struct s3fs_policy_ops {
    int (*on_admit)(__u64 count, __u64 arena_used_hint);
    int (*on_evict)(__u64 ino);
    int (*on_pressure)(__u64 count, __u64 threshold);
    int (*on_hit)(__u64 ino);
};

// 好きな state map (BPF hash / array / bloom filter / ringbuf 等)
struct {
    __uint(type, BPF_MAP_TYPE_HASH);
    __type(key, __u64);
    __type(value, __u8);
    __uint(max_entries, 4096);
} mypolicy_state SEC(".maps");

// stats (optional) — daemon が /metrics に mirror してくれる
enum { PSTAT_INVOCATIONS = 0, PSTAT_LAST_COUNT = 1, PSTAT_LAST_HINT = 2, PSTAT_MAX };
struct {
    __uint(type, BPF_MAP_TYPE_ARRAY);
    __type(key, __u32);
    __type(value, __u64);
    __uint(max_entries, PSTAT_MAX);
} policy_stats SEC(".maps");

SEC("struct_ops/on_admit")
int BPF_PROG(mypolicy_admit, __u64 count, __u64 hint) {
    // 1 admit_replace batch ごと呼ばれる。cold path なので好きな処理
    // OK (max_entries 内)。return は無視される (advisory hook)
    return 0;
}

SEC("struct_ops/on_hit")
int BPF_PROG(mypolicy_hit, __u64 ino) {
    // .read_folio hit branch から per-folio 呼ばれる — HOT PATH。
    // 短く。O(1) 操作のみ。
    __u8 one = 1;
    bpf_map_update_elem(&mypolicy_state, &ino, &one, BPF_ANY);
    return 0;
}

SEC("struct_ops/on_evict")
int BPF_PROG(mypolicy_evict, __u64 ino) {
    // admit_replace ごとに一覧の全 ino について呼ばれる。
    // 戻り値はスコア: 0 = keep、>0 = evict 候補 (大きいほど優先)。
    // legacy mode では >0 は全部 evict、capacity mode では収まるまで
    // スコア順に evict (下の「eviction のモード」参照)。
    __u8 *v = bpf_map_lookup_elem(&mypolicy_state, &ino);
    if (!v) return 0;  // first sight — grace
    return (*v == 0) ? 1 : 0;
}

SEC("struct_ops/on_pressure")
int BPF_PROG(mypolicy_pressure, __u64 count, __u64 threshold) {
    return 0;  // advisory only
}

// vtable の登録 — 名前は kmod/policy_struct_ops.h の struct 定義と一致
SEC(".struct_ops.link")
struct s3fs_policy_ops mypolicy_ops = {
    .on_admit    = (void *)mypolicy_admit,
    .on_evict    = (void *)mypolicy_evict,
    .on_pressure = (void *)mypolicy_pressure,
    .on_hit      = (void *)mypolicy_hit,
};

char _license[] SEC("license") = "GPL";
```

### ビルド

repo 内なら Makefile に足すのが早い:

```makefile
# bpf/Makefile OBJS に追加
OBJS := ... policy_mypolicy_ops.bpf.o
```

外部から作る場合は同じ clang command で:

```bash
clang -O2 -g -Wall -target bpf -mcpu=v3 \
    -D__TARGET_ARCH_x86 \
    -I<repo>/bpf -I<repo>/include \
    -c policy_mypolicy_ops.bpf.c -o policy_mypolicy_ops.bpf.o
```

`vmlinux.h` は `<repo>/bpf/vmlinux.h`。必要なら `make vmlinux` で
再生成。

### hook 別の制約 (重要)

- **`on_hit(ino)`** — HOT PATH: `.read_folio` から per-folio 呼ばれる。
  100 MiB read で 25600 回。O(1) map 更新のみ推奨。bpf_loop() は
  絶対に呼ばない
- **`on_evict(ino)`** — admit_replace ごとに一覧の全 ino について呼ばれる。
  戻り値はスコア (0 = keep、>0 = evict 候補、大きいほど先に追い出す)。cold path
- **`on_admit(count, hint)`** と `on_evict` は同じ全置換ラウンドで続けて呼ばれる。
  入ったばかりの ino にも on_evict が来るので、grace (初見は 0) を持つこと
- **`on_admit(count, hint)`** — admit_replace batch ごと 1 回。
  cold path、bpf_loop() 使える (aging 等)
- **`on_pressure(count, threshold)`** — softcap 越えた瞬間 1 回。
  advisory (return 無視)

hook 全部は optional — 不要なら空 (`return 0;`) にする。

### eviction のモード (S-02)

kmod の module parameter (daemon の `--softcap` / `--cache-capacity` で設定) で切り替わる:

| モード | 条件 | 動き |
|---|---|---|
| legacy | `capacity_bytes=0` (既定) | 送られてきた一覧が `softcap` (既定 8) 件を超えたら、スコア >0 の ino を**全部** evict。残る件数は policy 次第 |
| capacity | `capacity_bytes>0` | admit 済みバイト数が上限を超えている間だけ、スコアの高い順 → スコア 0 は古い順に evict。上限に収まった時点で止まる。softcap は無視 |

どちらのモードでも、miss upcall で reader が待っている ino は evict しない。
capacity mode では policy は「誰を先に追い出すか」を決めるだけで、上限は kmod が守る。
詳細は `docs/design/admit-and-softcap.md`。

### verifier で reject されたら

- **loop unroll 失敗**: `#pragma unroll` で bounded にする、または
  `bpf_loop()` helper を使う (kernel 5.17+)
- **map lookup NULL 未 check**: `if (!p) return 0;` を必ず入れる
- **1M insn 超過**: 命令数を減らす。map lookup を減らす、helper を
  少なくする、`#pragma unroll` を外して bpf_loop 化

## Layer 0 — KFunc primitives 参照

kmod-side state を BPF から読むための primitive 集。`kmod/tree_traversal.c`
で定義、tracing + struct_ops の両 prog type に register。

### 使える KFuncs

| KFunc | 戻り値 | 意味 |
|---|---|---|
| `s3fs_kfunc_admit_count()` | u32 | 現 admit hash size |
| `s3fs_kfunc_softcap()` | u32 | `softcap` module param (既定 8、legacy mode の発動条件) |
| `s3fs_kfunc_capacity_bytes()` | u64 | `capacity_bytes` module param (0 = legacy mode) |
| `s3fs_kfunc_evictions_total()` | u32 | 累計 eviction 数 |
| `s3fs_kfunc_time_ns()` | u64 | monotonic ns since boot (suspend 込) |
| `s3fs_kfunc_ino_size(ino)` | u64 | admit hash 内の ino のサイズ (無ければ 0) |

### policy 側の宣言

```c
extern __u32 s3fs_kfunc_admit_count(void)     __ksym;
extern __u32 s3fs_kfunc_softcap(void)         __ksym;
extern __u32 s3fs_kfunc_evictions_total(void) __ksym;
extern __u64 s3fs_kfunc_time_ns(void)         __ksym;
extern __u64 s3fs_kfunc_ino_size(__u64)       __ksym;
```

### 使用例

```c
SEC("struct_ops/on_evict")
int BPF_PROG(pressure_evict, __u64 ino) {
    __u32 admits = s3fs_kfunc_admit_count();
    __u32 cap    = s3fs_kfunc_softcap();

    // 1.5x 超過で aggressive
    if (admits * 2 >= cap * 3) {
        // ... evict aggressively
    }
    // ... else SIEVE
}
```

より詳しい consumer は `bpf/policy_pressure_ops.bpf.c`,
`policy_time_decay_ops.bpf.c`, `policy_size_aware_ops.bpf.c`。

### hot path 制約

- KFunc call は 1 回 ~10-20 ns 程度。per-folio on_hit で毎回呼ぶと
  100 MiB read で 25 μs のオーバーヘッド (許容範囲)
- 現在 `ino_size` だけが spinlock (`s3fs_admits_lock`) を短時間
  取る。他は lock-free atomic_read / const 返却

## Hot swap ワークフロー

daemon 再起動なし、in-flight IO 継続で policy を切替える。

```bash
# built-in を hot swap
curl -X POST http://localhost:9600/policy/reload \
     -d 'builtin:policy_sieve_ops'

# user eBPF を hot swap (allowlist 有効なら SHA256 check)
curl -X POST http://localhost:9600/policy/reload \
     -d 'file:/tmp/policy_myalgo_ops.bpf.o'

# eviction round を manually 発火 (bench 用 knob)
curl -X POST http://localhost:9600/policy/eval
```

struct_ops → struct_ops の reload は既存 link に対する 1 回の
`BPF_LINK_UPDATE` (new_map_fd = 新 vtable、`BPF_F_REPLACE` で旧 map を
期待値に指定)。kernel が kmod の `.update` callback を呼び、kmod-side は
RCU で vtable を 1 ポインタ swap — 既存 vtable で走行中の call は完走、
次の call から新 vtable を使う。dmesg で観測:

```
s3fs_ebpf: policy struct_ops updated (hot swap)
```

fmod_ret (tracing link は `BPF_LINK_UPDATE` 非対応) との相互切替や、
link update を拒否する kernel (< 6.4) では「新を attach → 旧を close」に
fallback し、dmesg は次のようになる:

```
s3fs_ebpf: policy struct_ops registered (ops=<new_addr>)
s3fs_ebpf: policy struct_ops unregistered (superseded, keeping <new_addr>)
```

> **sleepable にしない**: kmod は `rcu_read_lock()` の中から vtable を
> 呼ぶ (`kmod/policy_struct_ops.c`)。`SEC("struct_ops.s/...")` にすると
> sleepable trampoline (`might_fault()`) が RCU read-side で走り、
> `CONFIG_DEBUG_ATOMIC_SLEEP` kernel で splat する。`SEC("struct_ops/...")`
> を使うこと。

## Allowlist で user policy を承認する

`file:<path>` load は default で全通し (dev mode)。production では
`--policy-allowlist=<path>` で SHA256 gate を有効化する:

```bash
# 1. 承認する .bpf.o の hash を計算
./bin/s3fs-ebpf hash-policy /path/to/user.bpf.o \
    >> policies/allowlist.txt
# 出力: 6c9d3d6cf14...  /path/to/user.bpf.o
# → 末尾に手動で description を書き足す (optional)

# 2. daemon 起動時に allowlist を指定
sudo bin/s3fs-ebpf fastpath --s3 \
    --policy-bpf=builtin:policy_wtinylfu_ops \
    --policy-allowlist=policies/allowlist.txt \
    --metrics-addr=:9600

# 3. 承認済 hash の user policy → 通る
curl -X POST http://localhost:9600/policy/reload \
     -d 'file:/path/to/user.bpf.o'
# → 200 "reloaded to ..."
# → stderr: [allowlist] approved: ... matched "<description>"

# 4. 未承認 hash → 403
curl -X POST http://localhost:9600/policy/reload \
     -d 'file:/path/to/unknown.bpf.o'
# → 403 "allowlist: ... (sha256=...) not in ..."
```

- allowlist は startup 1 回読み。追加後は daemon 再起動が要る
- `builtin:` は engine 出荷なので bypass、常に load 可能
- template: `policies/allowlist.example.txt`

## Trace-replay で offline に試す

kmod / mount 起動なしに、workload trace を Go 側 policy simulator
で回して hit rate を出す。algorithm 直感を掴む用途。

```bash
# Zipf(0.7) 200 accesses を LRU と SIEVE で比較 (default)
./bin/s3fs-ebpf dev trace-replay --policies=lru,sieve

# 10000 accesses × Zipf α=0.9 を強い skew で
./bin/s3fs-ebpf dev trace-replay --zipf=0.9,30,10000 --policies=lru,sieve

# 記録済 JSONL trace を replay
./bin/s3fs-ebpf dev trace-replay --trace=/tmp/prod.jsonl --policies=lru,sieve
```

**制約**: simulator は Go reimpl なので **BPF policy と挙動が一致しない**
(strict LRU vs 近似 LRU 等)。数値は algorithm 直感、実 bench 数値は
`scripts/bench-hitrate.sh` で。詳細 `docs/design/trace-harness.md`。

## Debugging / trouble shooting

### policy が load できない

```bash
sudo dmesg -T | grep -E "s3fs_ebpf|verifier|struct_ops" | tail
```

- BTF relocation エラー → `struct s3fs_policy_ops` の layout が
  `kmod/policy_struct_ops.h` と一致しているか確認
- verifier reject → `bpftool prog dump xlated` で命令数、`bpftool
  prog show` で複雑度限界。1M 超えなら bpf_loop 化 or 命令数削減

### policy が呼ばれていない

```bash
# invocation counter を確認
curl -s http://localhost:9600/metrics | grep policy_hook_invocations
# 0 のままなら on_admit が発火していない — admit_replace path を確認
```

### hit rate が期待と違う

```bash
# 実 bench (30s/policy)
./scripts/bench-hitrate.sh
# より速い algorithm 比較 (ms scale)
./bin/s3fs-ebpf dev trace-replay --policies=lru,sieve --zipf=<α>,<N>,<accesses>
```

- `docs/journal/hit-rate-bench.md` の caveats 節で "workload sensitivity"、
  "s3fifo variance" 等の既知の挙動を確認

### 実装ハマりポイント

過去の実装で踏んだ地雷は `docs/dev-notes/lessons-learned.md` に:
- arena mm/gup の 3 段 upcall
- iget_locked vs new_inode
- struct_ops hot reload の guard
- bpf_loop() vs verifier insn budget
- fmod_ret 呼出の GCC 定数畳込対策
- ...

## 参照

- 設計: `docs/design/policy-engine-design.md` (4 layer 詳細)
- attach 方式比較: `docs/design/policy-attach-comparison.md` (fmod_ret vs struct_ops)
- bench 数値: `docs/journal/hit-rate-bench.md`
- Meetup demo: `docs/user/demo-scenario.md`
- trace harness: `docs/design/trace-harness.md`
- 教訓集: `docs/dev-notes/lessons-learned.md`
