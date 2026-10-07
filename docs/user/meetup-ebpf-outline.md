# eBPFミートアップ発表アウトライン (20min)

2026-10-02

## 発表テーマと方針

- **ターゲット**: eBPFミートアップ参加者（BPF全般に興味あり、S3FSの詳細は不要）
- **中心テーマ**: `bpf_arena` / `KFunc` / `struct_ops` の3技術を動機・実装例・実測込みで紹介し、「自分のユースケースでも使えそう」を持ち帰ってもらう
- **FSの話は**: 動機の1スライドのみ。各技術の説明はFS文脈を最小化して技術単体で理解できるように
- **性能評価**: nocache 3× を軸に「キャッシュの差ではなく構造の差」を示す
- **枠**: 20min

---

## ネタ採否

### 採用

| # | ネタ | 時間目安 |
|---|---|---|
| A | 動機とシステム概要 | 2min |
| B | `bpf_arena` — カーネル/ユーザーのゼロコピー shared memory | 3min |
| C | verifierの壁 | 2min |
| D | KFunc実装 3ステップ | 3min |
| E | `struct_ops` — vtableをBPFでホットスワップ | 3min |
| F | 性能数字 — nocache 3× で構造差を示す | 3min |
| G | takeaway: 3技術の使いどころ | 2min |

### 省く

| ネタ | 理由 |
|---|---|
| Layer 0 KFuncs の詳細 (5種API一覧) | struct_ops の例コード内に1行で組み込む。独立セクション不要 |
| CEL/Lua policy (Layer 1) | eBPFではない |
| VFS/kmodの実装詳細 | 趣旨からズレる |
| W-TinyLFU/S3-FIFOのアルゴリズム詳細 | eBPFの話でなくキャッシュアルゴリズムの話 |

---

## アウトライン

### 1. Intro (2min)

- 「S3をローカルFSとして使いたい」「FUSEは毎回ユーザー空間往復が起きる」
- 構成: kmod (VFS) + BPF fast path + Go daemon の3層
- **今日の話: このシステムで使ったeBPF技術3つ — `bpf_arena` / KFunc / `struct_ops`**
  - どれも「カーネルに踏み込む窓口」。S3FSに限らず使える

### 2. `bpf_arena` — ゼロコピー shared memory (3min)

#### 何者か

`BPF_MAP_TYPE_ARENA` (Linux 6.9+) は、カーネルとユーザー空間が **同じ仮想アドレスで** 読み書きできる連続メモリ領域。通常の BPF map は「カーネル側の値をユーザーが lookup でコピーして取り出す」が、arena はコピー不要。

```
カーネル (BPF/kmod) ←→ 同一 VA ←→ ユーザー (daemon mmap)
                    ↑ copy ゼロ
```

#### 使い方の最小例

```c
// ユーザー空間 (daemon)
int arena_fd = bpf_map_create(BPF_MAP_TYPE_ARENA, NULL, 0, 0, 4096, NULL);
void *base = mmap(NULL, 4096 * PAGE_SIZE, PROT_READ|PROT_WRITE,
                  MAP_SHARED, arena_fd, 0);
struct s3fs_admit_entry *e = base + offset;
e->size   = file_size;
e->uaddr  = (u64)file_data_ptr;   // daemon 側の mmap アドレス
```

```c
// BPF プログラム側 (arena pointer は map 経由で受け取る)
struct s3fs_admit_entry *e = bpf_map_lookup_elem(&admit_map, &ino);
// e->uaddr は daemon の mmap と同じ VA — copy_to_user せず pin_user_pages_remote でページ pin
```

- S3FSでの使い方: daemon が S3 からフェッチしたデータを arena に書き込む → kmod が `pin_user_pages_remote` でそのページを page cache に昇格 → read syscall が FUSE upcall なしに返る

#### 注意点・制限

- arena は `bpf_arena_alloc_pages` / `bpf_arena_free_pages` で BPF 側管理。`kfree` 不可
- 6.9 時点では BPF verifier が arena ポインタに strict な型チェックを行うため、ポインタをキャストする場所に注意
- **128 MiB が現実的な上限** (demand-paged なので RSS は実データ分のみ)

### 3. verifierの壁 (2min)

- arena 上の linked-list walk を BPF 単体で書くと即死:

```c
// bpf/s3fs_fastpath.bpf.c — s3fs_deep_walk_reject (再現用コード)
struct node *n = root;
while (n) {          // n->next の load 結果が scalar() → verifier が
    sum += n->val;   // ループ終了を証明できず状態を 1M insn 展開して爆発
    n = n->next;
}
```

```
BPF program is too large. Processed 1000001 insn
(limit 1000000)  total_states 9664
```

- 再現: `sudo ./bin/s3fs-ebpf dev verifier-demo`
- 原因: `n->next` の型が `scalar()` (範囲不明ポインタ) になり、ループ終了条件が証明不能。1M insn 上限 (`BPF_COMPLEXITY_LIMIT_INSNS`) に到達
- → ループを **KFunc** に逃がす

### 4. KFunc — verifierを安全に越える窓口 (3min)

#### 3ステップで登録

```c
// kmod/tree_traversal.c

// Step 1: アノテーション
__bpf_kfunc_start_defs();

__bpf_kfunc long s3fs_kfunc_arena_list_walk(
        u64 head_uaddr, u32 max_nodes, u64 *sum_out)
{
    // 普通の C ループ — verifier の外なのでループ数制限なし
    struct node __user *n = (void __user *)head_uaddr;
    for (u32 i = 0; i < max_nodes && n; i++) {
        struct node tmp;
        if (copy_from_user(&tmp, n, sizeof(tmp)))  // 壊れた next は -EFAULT
            return -EFAULT;
        *sum_out += tmp.val;
        n = (void __user *)tmp.next;
    }
    return 0;
}
__bpf_kfunc_end_defs();

// Step 2: BTF に登録
BTF_KFUNCS_START(s3fs_kfunc_walk_ids)
BTF_ID_FLAGS(func, s3fs_kfunc_arena_list_walk, KF_SLEEPABLE)  // copy_from_user は sleep しうる
BTF_KFUNCS_END(s3fs_kfunc_walk_ids)
static const struct btf_kfunc_id_set s3fs_kfunc_walk_set = {
    .owner = THIS_MODULE,
    .set   = &s3fs_kfunc_walk_ids,
};

// Step 3: prog type ごとに登録 (6.11+: BTF_SET8_START でないと -EINVAL)
register_btf_kfunc_id_set(BPF_PROG_TYPE_SYSCALL, &s3fs_kfunc_walk_set);
```

```c
// BPF 側 (bpf/kfunc_walk.bpf.c) — KFunc 呼出1行で verifier が即通す
SEC("syscall")
int walk_arena(struct pt_regs *ctx) {
    u64 sum = 0;
    long r = s3fs_kfunc_arena_list_walk(head_uaddr, 100000, &sum);
    // ...
}
```

- `verifier-demo` の後半が 100k ノード walk → sum 一致を検証
- **`KF_SLEEPABLE`** を付けると `SEC("syscall")` (sleepable) prog にのみ登録が必要。TRACING / STRUCT_OPS に同じ KFunc を公開したい場合は `register_btf_kfunc_id_set` を prog type ごとに呼ぶ
- `fmod_ret` 用は別 API `register_btf_fmodret_id_set`

### 5. `struct_ops` — vtableをBPFでホットスワップ (3min)

#### 概念

カーネルモジュールが定義した `struct xxx_ops` の実装を、**BPF プログラムがそのまま提供できる**仕組み。`tcp_congestion_ops` の BPF 版が原型。ここでは `s3fs_policy_ops` を定義してキャッシュポリシーを差し替える。

```c
// kmod/policy_struct_ops.h
struct s3fs_policy_ops {
    int (*on_admit)   (u64 count, u64 arena_used_hint);
    int (*on_evict)   (u64 ino);
    int (*on_pressure)(u64 count, u64 threshold);
    int (*on_hit)     (u64 ino);
};
```

#### BPF 側の実装例 — SIEVE ポリシー

```c
// bpf/policy_sieve_ops.bpf.c
SEC("struct_ops/on_admit")       // kmod が rcu_read_lock() 内から呼ぶので sleepable にしない
int BPF_PROG(sieve_on_admit, u64 count, u64 arena_used_hint) {
    u64 cap = s3fs_kfunc_softcap();   // Layer 0 KFunc でkmod状態を参照
    return (count < cap) ? 0 : -ENOSPC;
}

SEC("struct_ops/on_hit")
int BPF_PROG(sieve_on_hit, u64 ino) {
    bpf_map_update_elem(&visited, &ino, &(u8){1}, BPF_ANY);
    return 0;
}

SEC(".struct_ops.link")          // BPF_F_LINK — link として管理
struct s3fs_policy_ops sieve_policy = {
    .on_admit = (void *)sieve_on_admit,
    .on_hit   = (void *)sieve_on_hit,
};
```

#### アトミックなホットスワップ

```go
// daemon/internal/policyprog/loader.go — SwapFrom
// cilium/ebpf v0.22 は struct_ops link の Update 未対応 → raw syscall
syscall.Syscall(SYS_BPF, BPF_LINK_UPDATE, uintptr(unsafe.Pointer(&attr)), ...)
// attr.link_id    = 既存 link の fd
// attr.new_map_fd = 新 struct_ops map の fd
// attr.flags      = BPF_F_REPLACE
```

- kmod 側: `rcu_dereference(policy_ops)` → vtable 呼び出し → 1 syscall でポインタがアトミックにスワップ
- **fmod_ret との違い**: tracing link は `BPF_LINK_UPDATE` 非対応 → 「新 N 本 attach → 旧 N 本 close」でその間は新旧が同時走る。`struct_ops` は 1 syscall でアトミック
- デモ: `curl -X POST localhost:8080/policy/reload -d builtin:policy_sieve_ops` → dmesg に `s3fs_ebpf: policy struct_ops updated (hot swap)`

### 6. 性能 — キャッシュを揃えても消えない構造差 (3min)

#### 公平な比較のために: ebpf-nocache

「ebpf が速いのはキャッシュが効いているからでは？」という反論に答えるため、**両方キャッシュ無効**の条件を用意した。

- **fuse-raw**: `-o use_cache` 無し、毎回 S3 GET
- **ebpf-nocache**: `kmod nocache=1` + daemon `--cache-dir` 無し。open ごとに page cache と admit を破棄、毎回 S3 GET を強制

同じ条件で測ると：

| 指標 | fuse-raw | ebpf-nocache | 差 |
|---|---:|---:|---:|
| open+read+close p50 (S-09修正後) | 1.69 ms | **558 µs** | **3× 速い** |

差の正体は `/dev/fuse` queue + open ごとの getattr upcall がないこと。**キャッシュではなくトランスポート構造の差**なので fuse-raw がどう最適化しても消えない。

#### キャッシュ有り同士: ebpf-A+C vs fuse-cache

fuse を `-o use_cache + kernel_cache` で最大限最適化した状態でも差が残る:

| 指標 | fuse-cache | ebpf-A+C | 差 |
|---|---:|---:|---:|
| warm cat (1 MiB) | 1.5 ms | **0.7 ms** | 2.1× |
| open+read+close p50 | 526 µs | **3.0 µs** | **175×** |
| CPU cycles/op | 431k | **331k** | 1.3× 少ない |
| IPC | 0.79 | **1.03** | パイプライン stall 少ない |

open+read+close の 175× は **open ごとの FUSE getattr upcall**（fuse-cache でも消えない）が原因。`ls` や小ファイル大量アクセスのワークロードで直撃する。

#### write (durable, S-08修正後)

| Size | fuse-raw | ebpf-fsync |
|---|---:|---:|
| 1 MiB (fsync まで) | 19.5 ms | min ≈ 19.7 ms |

S-08 (100ms ポーリングを `blocking ioctl` に修正) 後、1 MiB durable write は **fuse-raw と同オーダー**に収束。

### 7. takeaway (2min)

| 技術 | 一言 | どこで使える？ |
|---|---|---|
| `bpf_arena` | カーネル/ユーザーのゼロコピー shared memory | hot path のデータをコピーなしで渡したい、ポリシーの判定結果をカーネルに即時反映したい |
| KFunc | verifier の壁を安全に越える逃がし先 | unbounded loop、ポインタ walk、sleepable ops が必要な処理 |
| `struct_ops` | vtableをBPFで実装 + アトミックスワップ | 差し替え可能なアルゴリズム (スケジューラ、輻輳制御、ポリシー)、再起動なしの切り替え |

- **「3つとも カーネルに踏み込む窓口」** — FS に限らず、ネットワーク・スケジューラ・セキュリティポリシーにも同じパターンが使える
- KFunc の壁: prog type ごとに `register_btf_kfunc_id_set` が必要。`BPF_PROG_TYPE_TRACING` と `STRUCT_OPS` は別エントリ
- `struct_ops` の壁: cilium/ebpf などのライブラリが `BPF_LINK_UPDATE` 未対応の場合がある → raw syscall が必要なケースあり
- コード: `kmod/tree_traversal.c` (KFunc)、`kmod/policy_struct_ops.c` + `bpf/policy_sieve_ops.bpf.c` (struct_ops)

---

## スライド補足メモ

- **最大のフック「2→3」の流れ**: verifier に弾かれる walk を先に見せ、KFunc で解決する展開。`verifier-demo` 1 コマンドで「拒否 → 同じ walk を KFunc で 100k ノード走査・sum 一致」まで示せる
- **性能スライドのポイント**: 大きい数字 (175×、554×) ではなく **nocache 3×** を先に出す。「キャッシュを揃えても 3× 差が残る = 構造の差」を確立してから、キャッシュ有りの倍率に進む。そうしないと「キャッシュの比較では？」という質問で詰まる
- **struct_ops demo**: `curl -X POST localhost:8080/policy/reload -d builtin:policy_sieve_ops` でライブスワップ。`/metrics` の `s3fs_ebpf_read_miss_upcalls_total` / `s3fs_ebpf_folio_hits_total` / `s3fs_ebpf_policy_evictions_total` の変化を見せる
- **bpf_arena の「FS 依存に見える」問題の対処**: §2 ではコード例の `s3fs_admit_entry` を `struct cached_item` に変名してもよい。本質は「同じ VA をカーネルとユーザーが共有する map」で、S3FS でなくても使える
- **コード引用元**:
  - `bpf/s3fs_fastpath.bpf.c` (`s3fs_deep_walk_reject`)
  - `kmod/tree_traversal.c` (`BTF_KFUNCS_START`, `s3fs_kfunc_arena_list_walk`)
  - `bpf/kfunc_walk.bpf.c` (KFunc 呼出側)
  - `kmod/policy_struct_ops.c` (reg/unreg/update)
  - `bpf/policy_sieve_ops.bpf.c` (struct_ops 実装例)
  - `daemon/internal/policyprog/loader.go` (`SwapFrom` / `BPF_LINK_UPDATE`)

---

## 実装照合レビュー (2026-10-05)

アウトラインの主張を実装・ベンチ記録と突き合わせた結果。

### 実装漏れ (対応済)

| 主張 | 実装の実態 | 対応 |
|---|---|---|
| 2→3「verifier に弾かれる walk を KFunc で解決」 | KFunc 側の walk が存在しなかった。`s3fs_kfunc_traverse_stub` は `return 0`。`verifier-demo` の「same walk is expressed as a KFunc call (… `make test-arena` subtest C)」も誤りで、subtest C は `s3fs_kfunc_echo` (2v+1) だけだった | `s3fs_kfunc_arena_list_walk` (kmod) + `bpf/kfunc_walk.bpf.c` を追加し、`verifier-demo` の後半で実行・検証する |
| struct_ops を「1 回の link.Update でアトミックに入れ替え」 | reload は「新 link attach → 旧 link close」で、kmod の `.update` callback は一度も呼ばれていなかった。cilium/ebpf の struct_ops link `Update` は `ErrNotSupported` | `policyprog.SwapFrom` で `BPF_LINK_UPDATE` (map 版) を raw syscall 実行。非対応時は従来経路へ fallback |
| fmod_ret は「N 回 link.Update」 | tracing link は `BPF_LINK_UPDATE` 非対応 | 比較の記述を「N attach + N close (重複期間あり)」に訂正 (outline / `policy-attach-comparison.md` / loader コメント) |

### バグ (対応済)

- **policy struct_ops が sleepable だった**: 全 `*_ops.bpf.c` が `SEC("struct_ops.s/...")` (`BPF_F_SLEEPABLE`)。kmod は `rcu_read_lock()` 内から呼ぶため、sleepable trampoline の `might_fault()` が RCU read-side で走る (`CONFIG_DEBUG_ATOMIC_SLEEP` で splat)。on_hit は per-folio の hot path。sleepable kfunc は使っていない → `SEC("struct_ops/...")` に変更
- **hot reload 後に policy metrics が止まる**: `samplePolicyStats` が起動時の `*Program` を保持し続け、reload で close された map を読んでいた。`defer polProg.Close()` も起動時の値を捕捉しており、reload 後の program は close されなかった。reload handler は同時実行の排他も無かった → mutex で保護し、都度現在の program を参照する
- **evict した inode の page cache が残る**: `.read_iter = generic_file_read_iter` なので evict 後も page cache から読め、`.read_folio` (= miss 計上 / on_hit) に到達しない → policy eviction 時に `invalidate_mapping_pages` (`s3fs_evict_page_cache`)
- trace simulator の SIEVE: 唯一のノードを evict したとき hand が unlink 済みノードに残る → 修正

### ベンチの根拠・参照試験の漏れ

- **hit-rate に参照上限が無かった** → `trace-replay` に `opt` (Belady) と `noevict` を追加。`bench-hitrate.sh` は実際に使った trace を `$OUT/trace.jsonl` に保存し、同じ trace で上限を計算する。この上限を当てると、W-TinyLFU 88.5% / SIEVE 65〜66% は 8 slot の最適値 (61〜65%) 以上になり、容量で正規化された比較ではない。各 policy の admit-set サイズ (`adm_mean` / `adm_max`) も出すようにした
- **sim と bench が同じ trace という主張が誤り**: Go `math/rand` と Python `random` は seed 42 でも別系列 → bench の trace を `--trace` で渡す方式に変更
- **ベンチのデフォルトが記録と不一致**: `bench-hitrate.sh` は α=0.9 / 400 回、記録と `trace-replay` は α=0.7 / 200 回 → 記録側に戻した
- **write 比較の durability 非対称**: ebpf は close で PUT を待たない → `ebpf-fsync` 行を追加 (要再計測)
- **キャッシュ無し同士の対照群が無かった**: fuse-raw の比較相手は常にキャッシュ有りの ebpf だった → kmod に `nocache` module param (open ごとに page cache + admit を破棄) を追加し、`bench-3way.sh` に `ebpf-nocache` を追加
- **read 875× の出典が 2 run 混在**: 同一 run の比は ~865×。§3.3 の「886×」とも不一致 → 「~870×」に統一
- ✅ hit-rate 再計測済 (2026-10-05): 3 seed × 10 policy。S-01+S-02 修正後の正当な比較。`docs/journal/hit-rate-bench.md` §"Result (2026-10-05)" 参照。
- 再計測が必要: write の `ebpf-fsync`、3-way の `ebpf-nocache`。いずれも単一 run・n 小なので、複数 run の中央値とばらつきも併記したい
