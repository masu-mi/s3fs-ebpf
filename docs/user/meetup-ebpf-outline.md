# eBPFミートアップ発表アウトライン (20min)

2026-10-02 (2026-10-11 実装照合レビュー反映)

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

`BPF_MAP_TYPE_ARENA` (Linux 6.9+) は、ユーザー空間が `mmap` し、BPF プログラムが `__arena` ポインタで直接読み書きする疎なメモリ領域 (最大 4 GiB)。**arena 内に置いたポインタ値がユーザー空間と BPF の両方でそのまま有効** (BPF 側は JIT が `addr_space_cast` で kern_vm_start + 下位 32 bit に変換)。通常の BPF map のように lookup/update syscall で値をコピーして出し入れする必要がない。

```
ユーザー (daemon mmap, user_vm)  ─┐
                                  ├─ 同じ物理ページ / 同じポインタ値
BPF prog (__arena ptr, kern_vm)  ─┘
```

#### 使い方の最小例

```c
// ユーザー空間 — BPF_F_MMAPABLE 必須 (無いと -EINVAL)。max_entries はページ数
LIBBPF_OPTS(bpf_map_create_opts, o, .map_flags = BPF_F_MMAPABLE,
            .map_extra = 1ULL << 44 /* user_vm_start (省略可) */);
int fd = bpf_map_create(BPF_MAP_TYPE_ARENA, "arena", 0, 0, 32768, &o);
struct node *head = mmap((void *)(1ULL << 44), 32768 * 4096,
                         PROT_READ | PROT_WRITE, MAP_SHARED | MAP_FIXED, fd, 0);
head->next = &head[1];   // ユーザー空間のポインタ値をそのまま格納
```

```c
// BPF プログラム側 — arena は lookup せず、ポインタを直接たどる
struct node __arena *n = (void __arena *)ctx->head;
sum += n->payload;       // clang 19+ (-mcpu=v3) が addr_space_cast を自動挿入
n = n->next;             // ユーザーが書いた next がそのまま使える
```

- S3FSでの使い方: daemon が S3 から取得したデータを arena の mmap に書き込む → `ADMIT_ARENA` ioctl で (ino, uaddr, size) を kmod に登録 → `.read_folio` が `pin_user_pages_remote` + `kmap_local_page` + `memcpy` で page cache の folio に充填 → 以降の read は page cache から返る (FUSE upcall なし)
- 「ゼロコピー」の範囲: daemon → kernel の受け渡しに write/ioctl のデータコピーや `/dev/fuse` バッファが無い、という意味。arena → page cache の memcpy は 1 回ある (page cache に載せる以上、FUSE でも必要なコピー)

#### 注意点・制限

- kmod (BPF でない普通のカーネルコード) は arena の kern_vm を素直には使えない: `bpf_arena_get_kern_vm_start()` も `struct bpf_arena` も非 export で、6.12 の kern_vm には汎用コンテキスト用の fault handler も無い → 本実装の read path は daemon の **user_vm を GUP** して読む (`kmod/vfs_ops.c` `s3fs_folio_fill_from_arena`)。kern_vm base の取得は `struct bpf_arena` のレイアウトを BTF から複製して `container_of` しており、カーネル更新で壊れうる (`kmod/s3fs_ebpf_dev.c`)
- BPF からの arena ロードは verifier が中身を追跡しない (`scalar()` 扱い)。不正アドレスは JIT の例外テーブルで 0 が読める扱いになり、oops にはならない — 安全性は「型チェック」ではなく「どこを指しても arena 内に閉じる」設計で担保
- ページは BPF 側 `bpf_arena_alloc_pages` / `bpf_arena_free_pages`、またはユーザー空間からのページフォルトで確保される (demand-paged なので RSS は実データ分のみ)
- 本実装の arena は 32768 pages = **128 MiB** (`bpf/s3fs_fastpath.bpf.c`)。arena 自体の上限は 4 GiB (32 bit オフセット)

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
- **必ず聞かれる: 「`can_loop` / `cond_break` なら書けるのでは？」→ 書ける**。`may_goto` 命令 (6.9+, arena と同じリリース) をループ条件に足すだけで verifier は通る (2026-10-11 に 6.18 で確認、下記「実装照合レビュー (2026-10-11)」参照)。ただし反復予算 (6.9〜6.14: 800 万回 / 6.15+ の x86-64 JIT: 250 ms) を使い切ると **エラーにならずループを抜ける** — 途中までの結果が返る
- → 予算に依存せず最後まで回す・sleep する・カーネルの既存 API (`copy_from_user`, page cache 操作) を使う処理は **KFunc** に逃がす

### 4. KFunc — verifierを安全に越える窓口 (3min)

#### 3ステップで登録

```c
// kmod/tree_traversal.c

// Step 1: アノテーション
__bpf_kfunc_start_defs();

__bpf_kfunc s64 s3fs_kfunc_arena_list_walk(u64 head_uaddr, u32 max_nodes,
                                           u64 *sum)
{
    // 普通の C ループ — verifier の外。停止性は自分で保証する
    struct { u64 next; u64 payload; } node;
    u64 cur = head_uaddr, acc = 0;
    u32 n = 0;

    while (cur) {
        if (n >= max_nodes)
            return -ELOOP;                      // 循環リスト
        if (copy_from_user(&node, u64_to_user_ptr(cur), sizeof(node)))
            return -EFAULT;                     // 壊れた next
        acc += node.payload;
        cur = node.next;
        if (!(++n & 1023)) {
            if (fatal_signal_pending(current))
                return -EINTR;
            cond_resched();
        }
    }
    *sum = acc;
    return n;                                   // 訪問ノード数
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

// Step 3: prog type ごとに登録
// (6.9+: BTF_KFUNCS_START で作ったセットでないと -EINVAL。BTF_SET8_START は不可)
register_btf_kfunc_id_set(BPF_PROG_TYPE_SYSCALL, &s3fs_kfunc_walk_set);
```

```c
// BPF 側 (bpf/kfunc_walk.bpf.c) — KFunc 呼出1行で verifier が即通す
extern __s64 s3fs_kfunc_arena_list_walk(__u64 head_uaddr, __u32 max_nodes,
                                        __u64 *sum) __ksym;

SEC("syscall")
int s3fs_deep_walk_kfunc(struct s3fs_walk_args *ctx) {
    __u64 sum = 0;
    ctx->out_ret = s3fs_kfunc_arena_list_walk(ctx->head_uaddr,
                                              ctx->max_nodes, &sum);
    ctx->out_sum = sum;
    return 0;
}
```

- `verifier-demo` の後半が 100k ノード walk → sum 一致を検証
- **`KF_SLEEPABLE`** の KFunc は sleepable な prog からしか呼べない。`SEC("syscall")` は libbpf / cilium/ebpf とも sleepable でロードされるのでそのまま呼べる (on_hit などの非 sleepable な struct_ops からは呼べない)。TRACING / STRUCT_OPS に KFunc を公開したい場合は `register_btf_kfunc_id_set` を prog type ごとに呼ぶ
- arena を `copy_from_user` で読めるのは、arena 内のポインタが daemon の user_vm アドレスだから。`current->mm` が arena を mmap したプロセスである syscall コンテキスト (BPF_PROG_TEST_RUN) 専用
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
// kmod が rcu_read_lock() 内から呼ぶので sleepable (struct_ops.s/) にしない
SEC("struct_ops/on_evict")       // 戻り値 = eviction 投票 (1 = evict)
int BPF_PROG(sieve_evict, u64 ino) {
    u8 *v = bpf_map_lookup_elem(&sieve_visited, &ino);
    if (!v)  { bpf_map_update_elem(&sieve_visited, &ino, &(u8){1}, BPF_ANY); return 0; }
    if (*v)  { bpf_map_update_elem(&sieve_visited, &ino, &(u8){0}, BPF_ANY); return 0; } // second chance
    bpf_map_delete_elem(&sieve_visited, &ino);
    return 1;
}

SEC("struct_ops/on_hit")         // per-folio の hot path
int BPF_PROG(sieve_hit, u64 ino) {
    bpf_map_update_elem(&sieve_visited, &ino, &(u8){1}, BPF_ANY);
    return 0;
}

SEC(".struct_ops.link")          // BPF_F_LINK — link として管理
struct s3fs_policy_ops sieve_ops = {
    .on_admit = (void *)sieve_admit,     // 統計のみ (kmod は戻り値を見ない)
    .on_evict = (void *)sieve_evict,
    .on_pressure = (void *)sieve_pressure,
    .on_hit   = (void *)sieve_hit,
};
```

- KFunc でカーネル状態を参照する例は `bpf/policy_pressure_ops.bpf.c` (`s3fs_kfunc_admit_count()` / `s3fs_kfunc_softcap()` を on_evict の判定に使う)

#### アトミックなホットスワップ

```go
// daemon/internal/policyprog/loader.go — SwapFrom
// cilium/ebpf は struct_ops link の Update 未対応 (v0.22 / 2026-10-08 の main でも
// ErrNotSupported) → raw syscall
syscall.Syscall(SYS_BPF, BPF_LINK_UPDATE, uintptr(unsafe.Pointer(&attr)), ...)
// attr.link_id    = 既存 link の fd
// attr.new_map_fd = 新 struct_ops map の fd
// attr.flags      = BPF_F_REPLACE
```

- kmod 側: `rcu_dereference(policy_ops)` → vtable 呼び出し → 1 syscall でポインタがアトミックにスワップ
- **fmod_ret との違い**: tracing link は `BPF_LINK_UPDATE` 非対応 → 「新 N 本 attach → 旧 N 本 close」でその間は新旧が同時走る。`struct_ops` は 1 syscall でアトミック
- vtable の切り替えはアトミックだが、ポリシーの状態 (map) は引き継がない — 新ポリシーは空の `sieve_visited` から始まる
- デモ: `curl -X POST localhost:9600/policy/reload -d builtin:policy_sieve_ops` → dmesg に `s3fs_ebpf: policy struct_ops updated (hot swap)`。前提: daemon を `--policy-bpf=builtin:<struct_ops 版>` で起動していること (fmod_ret 版から/への切り替えや `--policy-bpf` 無しでは attach+detach か 503 `policy engine not active`)。`--metrics-token` を設定した場合は `-H "Authorization: Bearer $S3FS_METRICS_TOKEN"` が要る

### 6. 性能 — キャッシュを揃えても消えない構造差 (3min)

#### 公平な比較のために: ebpf-nocache

「ebpf が速いのはキャッシュが効いているからでは？」という反論に答えるため、**両方キャッシュ無効**の条件を用意した。

- **fuse-raw**: `-o use_cache` 無し、毎回 S3 GET
- **ebpf-nocache**: `kmod nocache=1` + daemon `--cache-dir` 無し。open ごとに page cache と admit を破棄、毎回 S3 GET を強制

同じ条件で測ると：

| 指標 | fuse-raw | ebpf-nocache | 差 |
|---|---:|---:|---:|
| open+read+close p50 (2026-10-11、同一 run) | 1.691 ms | **526 µs** | **3.2× 速い** |

差の候補は `/dev/fuse` queue + open ごとの getattr upcall がないこと。**キャッシュ条件を揃えても残る差**であることは言えるが、内訳は未測定 — ebpf-nocache も miss ごとに daemon へ upcall + S3 GET しており、S3 クライアント実装 (Go daemon vs s3fs-fuse/libcurl、open 時の HEAD の有無) の差も含む。perf stat の CPU 時間差は 8% (10-11: 187 vs 203 µs)、1.2 ms の差の大半は待ち時間側。「fuse-raw がどう最適化しても消えない」とは言わない。
- 注: 1.691 ms と 526 µs は **2026-10-11 の同一 run** (`bench-3way.sh` microbench §7.3 と §7.7.2)。「約 3×」は 3.22× が実測値

#### キャッシュ有り同士: ebpf-A+C vs fuse-cache

fuse を `-o use_cache + kernel_cache` で最大限最適化した状態でも差が残る:

| 指標 | fuse-cache | ebpf-A+C | 差 |
|---|---:|---:|---:|
| warm cat (1 MiB) | 1.6 ms | **730 µs** | 2.1× |
| open+read+close p50 (同一 run) | 519 µs | **3.075 µs** | **169×** |
| CPU cycles/op | 441k | **328k** | 1.3× 少ない |
| IPC | 0.78 | **1.05** | (補足のみ。下記注) |

open+read+close の 169× は **open ごとの FUSE upcall**（fuse-cache でも消えない）が主因と見ている (推定。opcode 別の upcall 数は未計測)。`ls` や小ファイル大量アクセスのワークロードで直撃する。
- cycles/IPC は `cat` 200 回を `perf stat` で包んだプロセス全体の値で、fork/exec が支配的。「パイプライン stall が少ない」の根拠には弱いので、スライドでは CPU 時間差 (118 vs 163 µs) までに留める
- 逆に **open 済み fd の hot read は差が無い** (§10.2 fio psync: 1 job で ebpf 1199k / fuse-raw 1165k / fuse-cache 1143k IOPS)。「read が速い」ではなく「open を含むアクセスが速い」と言う

#### write (durable, S-08修正後)

| Size | fuse-raw | ebpf-fsync |
|---|---:|---:|
| 1 MiB (fsync まで) | 19.5 ± 2.2 ms (mean) | min ≈ 19.7 ms |

S-08 (100ms ポーリングを `blocking ioctl` に修正) 後、1 MiB durable write は **fuse-raw と同オーダー**に収束。ただし ebpf-fsync は min、fuse-raw は mean で、統計量が揃っていない。10 MiB / 100 MiB は S-08 後未計測。発表前に `bench-write.sh` を回して mean ± σ で揃えるか、「同オーダー」以上の主張はしない

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
- **struct_ops demo**: `curl -X POST localhost:9600/policy/reload -d builtin:policy_sieve_ops` でライブスワップ。`/metrics` の `s3fs_ebpf_read_miss_upcalls_total` / `s3fs_ebpf_folio_hits_total` / `s3fs_ebpf_policy_evictions_total` の変化を見せる
- **bpf_arena の「FS 依存に見える」問題の対処**: §2 のコード例は FS 非依存の `struct node` (linked list) にしてあり、§3 の verifier-demo とそのまま繋がる。本質は「ポインタ値をユーザー空間と BPF で共有できる map」で、S3FS でなくても使える
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

---

## 実装照合レビュー (2026-10-11)

発表前の再検証。アウトラインの各主張を実装 (HEAD `9337132`)・ベンチ記録・カーネルソースと突き合わせ、本文は修正済み。

### 修正した事実誤り

| 箇所 | 旧記述 | 実態 | 根拠 |
|---|---|---|---|
| §2 最小例 | `bpf_map_create(BPF_MAP_TYPE_ARENA, NULL, 0, 0, 4096, NULL)` | `map_flags = BPF_F_MMAPABLE` が無いと -EINVAL | `bpf/s3fs_fastpath.bpf.c` の arena 定義 |
| §2 BPF 側 | `bpf_map_lookup_elem(&admit_map, &ino)` で arena ポインタを取る | arena は lookup しない (`__arena` ポインタを直接たどる)。`admit_map` は存在せず、admit 表は kmod 内の hash (`ADMIT_ARENA` ioctl で登録) | `kmod/vfs_ops.c` `struct s3fs_admit` |
| §2 | 「kmod が `pin_user_pages_remote` でそのページを page cache に昇格」「カーネルとユーザーが同一 VA」 | kmod は daemon の user_vm を GUP → `kmap_local_page` → `memcpy` で folio に充填 (1 回コピー)。同一ポインタ値が通じるのは BPF prog とユーザー空間の間 | `kmod/vfs_ops.c` `s3fs_folio_fill_from_arena` |
| §2 | 「verifier が arena ポインタに strict な型チェック」 | 逆で、arena ロードは `scalar()` 扱いで追跡されない。不正アドレスは例外テーブルで 0 | verifier ログ `R4_w=scalar()` |
| §2 | 「128 MiB が現実的な上限」 | 128 MiB は本実装の設定値 (32768 pages)。arena の上限は 4 GiB | `kernel/bpf/arena.c` (v6.9) `vm_range > SZ_4G → -E2BIG` |
| §4 Step 3 | 「6.11+: BTF_SET8_START でないと -EINVAL」 | **6.9+** で **BTF_KFUNCS_START** (BTF_SET8_KFUNCS フラグ) でないと -EINVAL。BTF_SET8_START は弾かれる側 | `kernel/bpf/btf.c`: v6.8 にチェック無し、v6.9 の `register_btf_kfunc_id_set` に有り |
| §4 コード | `long` を返し `return 0`、BPF 側 `struct pt_regs *ctx` | `s64` で訪問ノード数を返す (-ELOOP/-EFAULT/-EINTR)。BPF 側 ctx は `struct s3fs_walk_args *` | `kmod/tree_traversal.c`, `bpf/kfunc_walk.bpf.c` |
| §4 | 「`KF_SLEEPABLE` を付けると syscall prog にのみ登録が必要」 | 登録先の制約ではなく、呼び出し元 prog が sleepable である必要がある、という制約 | — |
| §5 SIEVE 例 | on_admit で softcap と比較し `-ENOSPC` を返す | kmod は on_admit の戻り値を捨てる (`(void)s3fs_policy_ops_eval_admit`)。SIEVE の本体は on_evict (戻り値 1 = evict 投票)。softcap KFunc を使うのは `policy_pressure_ops` | `kmod/vfs_ops.c:739`, `bpf/policy_sieve_ops.bpf.c` |
| §5 / メモ デモ | `curl localhost:8080/...` | 既定は `--metrics-addr=:9600`。`--metrics-token` 設定時は Bearer ヘッダ必須 | `daemon/cmd/s3fs-ebpf/main.go`, `docs/user/demo-scenario.md` |
| §5 | 「cilium/ebpf v0.22 は未対応」 | 現在 upstream main (`v0.22.1-0.20261008141411-5aef01488cf5`) を参照。こちらも `structOpsLink.Update` は `ErrNotSupported` のまま → 結論は変わらず、表記のみ更新 | module cache の `link/struct_ops.go` |

### 追加調査: `can_loop` で arena walk は BPF 単体で書けるか

§3 の「BPF 単体で書くと即死 → KFunc」は、Q&A で「`can_loop` / `cond_break` (may_goto, 6.9+) を使えば？」と突かれる。selftests の `bpf_arena_list.h` もこの方式で arena list を走査している。手元 (kernel 6.18, clang 19, cilium/ebpf main) で検証した:

```c
while (n && can_loop) {          // can_loop = may_goto を 1 命令足すだけ
    sum += n->payload;
    n = n->next;
}
ctx->ret = n ? -1 : cnt;         // n != NULL なら予算切れで抜けた
```

| 版 | 結果 |
|---|---|
| `while (n)` (verifier-demo と同形) | 拒否: `BPF program is too large. Processed 1000001 insn` — リポジトリの記録と同じ |
| `while (n && can_loop)`, 100k ノード | **verifier 通過**、sum=5000050000 一致 (≈0.7 ms) |
| 同, 10M ノード | 通過・完走 (≈73 ms) |
| 同, 60M ノード | **≈251 ms で may_goto が打ち切り、エラーなしで途中までの sum を返した** |

- may_goto の予算: 6.9〜6.14 は反復回数 (`BPF_MAX_LOOPS` = 8M)、6.15+ で JIT が対応していれば時間 (`NSEC_PER_SEC / 4` = 250 ms、`bpf_check_timed_may_goto`)。6.18 x86-64 での 251 ms はこれと一致。開発環境 6.12 は回数予算のほう
- **発表への影響**: 「BPF では書けない」は誤り (6.9+ では)。正確な主張は「verifier を通すための書き方はあるが、(1) 予算切れで黙って打ち切られる — 完全な走査の保証が要るなら自前で検出が必要、(2) sleep できない、(3) `copy_from_user` やページキャッシュ操作などカーネル API が要る処理は BPF からは書けない → KFunc」。§3 にこの 1 行を追加済み。素の `while(n)` 拒否のデモはそのまま使えるが、「`can_loop` を足すと通る」を先に自分で言ってしまうほうが強い
- 実際 `s3fs_kfunc_arena_list_walk` が KFunc である理由も (1)(3) — `max_nodes` 超過は `-ELOOP` として明示的に返し、`copy_from_user` で読む

### 数字の扱い (本文に注記済み)

- **nocache 3×**: ✅ **2026-10-11 の同一 run で確定** — fuse-raw 1.691 ms / ebpf-nocache 526 µs = 3.22×。内訳 (FUSE upcall / S3 クライアント差 / HEAD の有無) は未分解
- **ebpf-A+C vs fuse-cache ~169×**: ✅ **同一 run** — fuse-cache 519 µs / ebpf-A+C 3.075 µs = 168.8×
- **IPC / cycles**: `cat` プロセス全体 (fork/exec 込み) の値。パイプラインの議論には使わない
- **write**: ebpf-fsync の min と fuse-raw の mean を並べていた。統計量を揃える (bench-write.sh 再実行で解消予定)
- **hot read (open 済み fd)** は 3 者同等 (§10.2)。「open を含むアクセス」が速い、と限定して言う

### 発表前に追加で測ると良いもの (優先順)

1. ✅ `bench-3way.sh` 同一 run — 2026-10-11 に実施。3.2× と 169× が同一 run 由来で確定
2. 3× の内訳: microbench 中の MinIO リクエスト数 / op (GET と HEAD を分けて) と、FUSE opcode 別 upcall 数 (`bpftrace` で `fuse_simple_request` などを数える)。HEAD の有無で差の大半が説明できるなら「FUSE 構造の差」とは言えない
3. `bench-write.sh` を S-08 後のコードで 3 サイズ再計測 (mean ± σ)
4. (任意) I-01 で追加した mount-s3 arm の実測 — 「他の FUSE 実装なら？」への回答材料
