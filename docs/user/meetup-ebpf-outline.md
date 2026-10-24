# eBPFミートアップ発表アウトライン (20min)

2026-10-02 作成 / 2026-10-11 実装照合レビュー反映 / **2026-10-13 AA-03 の失敗を受けて再構成** (HEAD `3b9bd79`)

## 発表テーマと方針

- **ターゲット**: eBPFミートアップ参加者（BPF全般に興味あり、S3FSの詳細は不要）
- **中心テーマ**: `bpf_arena` と `struct_ops` の**使い方**を実装例込みで紹介し、さらに「arena を誤った場所で使ってベンチで負けた」失敗談から**それぞれの適用範囲の境界**を持ち帰ってもらう
  - KFunc は「arena と verifier の間をつなぐ脇役」として 1 セクションに圧縮
- **ストーリー**: 使い方 (arena / KFunc / struct_ops) → 効いたところ (nocache 3×) → **効かなかったところ (AA-03)** → 反省と今後の対応 (検討中)
- **FSの話は**: 動機の1スライドのみ。各技術の説明はFS文脈を最小化して技術単体で理解できるように
- **枠**: 20min

---

## 2026-10-16 更新: 現行スライドの構成と、後から加わった知見

スライドの正本は claude.ai の Slides アーティファクト (本編 26 枚 + 付録 2 枚、2026-10-21 更新)。この節は、下のアウトライン (2026-10-02 時点) から変わった点だけをまとめる。

### 構成 (本編 26 枚 + 付録 2 枚)

| 区切り | スライド | 要点 |
|---|---|---|
| 導入 | cover / intro | arena の使いどころを間違えた話をする、と最初に言う |
| bpf_arena | arena-what / arena-who / arena-design | 変更なし (arena-map は付録へ) |
| KFunc | kfunc | 変更なし (arena-loop は付録へ) |
| struct_ops | so-concept / so-sieve / so-swap / so-pitfall / so-fix / **so-real** | so-real (新規): 案B (`pick_victim`) を実装。LRU / SIEVE / S3-FIFO を移し、3 seed で旧版より +2〜7 pt (S3-FIFO 36.7〜40.0% が最良)。read 経路のコストは 64 jobs で policy なし比 −4.6% (旧 SIEVE は −14.8%)、`pick_victim` 1 回 0.5〜0.9 µs (候補 8 件)。コード枠に AC-66 の `on_size` を追加し、W-TinyLFU も移した (実機 1 回で 38.55%、S3-FIFO 37.80%) |
| 勝てるところ | **perf** / **perf-lose** / **fs-usecases** | perf: pcache モードで測り直した数字 (open+read+close 3.3× / 172×、4 KiB × 10,000 個の cat 2 回目 93×、`ls -l` 初回 11×、新規の durable write 1.37×)。perf-lose (新規): 負けていた大きいファイルの 2 場面を直した前後 (既存 100 MiB の途中の書き換えは互角、cold read 300 MiB は毎回キャッシュを空にすると 1.26×、続けて読むと 0.80×)。fs-usecases: 小さいファイルを大量に開く、短命なプロセスが多い、fsync で確実に書く、キャッシュの判断を変えたい。向かないのはメモリの少ないホストで大きいファイルを次々に読む使い方 |
| 失敗談 | fail-title / fail-plan / fail-result / fail-why / **fail-found** / **hook-tips** | fail-found (新規): 64 jobs の律速は daemon の `fentry/generic_file_read_iter` だった。hook-tips (新規): hot path にフックを置くときの注意 6 項目 |
| まとめ | lessons / next / **usecases** / takeaway / end | usecases (新規): 判断は struct_ops、共有する状態は arena。takeaway に fentry の行を追加。next の 3 を「案B を実装」に (W-TinyLFU も追加)。end のノートは 172× (pcache モード) |
| 付録 (Q&A 用) | **arena-map** / **arena-loop** / **loop-why** | arena-map: 同じ物理ページを daemon の user_vm と kernel の kern_vm に二重にマップしている。BPF は JIT が `r12 = kern_vm_start` を使うので、フック時のマップ切り替えはない。arena-loop: コストを決めるのはループの書き方ではなく「arena を読むのが BPF か普通のカーネル C か」。loop-why (2026-10-21 追加): BPF のループに上限がある理由、KFunc の walk が長く回せる理由 (終わる保証を kmod が持ち、sleepable)、walk はデモ用で汎用ではなく、止まればよいだけなら bpf_loop / can_loop で足りる (meetup-qa Q14 / Q15) |

### 後から加わった知見 (根拠)

- **64 jobs の律速は read の fentry** (`docs/design/read-iter-hook-overhead.md`、`docs/journal/benchmark-results.md` §18.16 / §18.23 / §18.24)
  - フックなしの ebpf-pcache は fuse-raw の 1.00〜1.02 倍。フックありは 0.81 倍だった。
  - 内訳は BPF_CORE_READ の probe_read 約 7%、同じ ino の値への atomic 約 8%、lookup と policy 約 3%、trampoline ほか約 2%。
  - 直接参照 (0.87)、per-CPU 化 (0.93)、slot 配列と間引き (0.94〜0.96) で戻した。フックの固定費だけで上限 0.974。
  - 以前の計測は 1 スレッドの遅延 (1 µs 未満) しか見ておらず、共有値の取り合いが見えなかった。
  - 「FUSE は large folio を使うので速い」という仮説は、計測カーネル 6.12 で両方とも order 0 だったので外れた。
- **100 MiB の warm read は fuse-cache より速い** (9.5 ms / 10.5 ms、§18.16 T6)。§18.2 の 1.20 倍は再現しない。
- **kmod が kern_vm を使えない理由** (v6.12 `kernel/bpf/arena.c`): `bpf_arena_get_kern_vm_start` は未 export。この PoC は `struct bpf_arena` のレイアウトを BTF から複製して番地を取っている (`kmod/s3fs_ebpf_dev.c`) が、普通のカーネル C には BPF のような例外テーブルがないので、未割り当てのページや `bpf_arena_free_pages` と競合したときに oops になる。read 経路が GUP を使う決め手はこちら。
- **勝てる・負けるところの数字** (§18.27〜18.41、2026-10-17、ローカル MinIO)
  - open+read+close と durable write は pcache モードで測り直した (§18.27 / 18.28)。arena モードと同じ水準。
  - 既存の大きいオブジェクトの途中の書き換えは、AC-50 の読み込みで一度 2.46× 遅くなった (§18.29)。AC-51 (`--partial-modify`、既定では無効) で、触れる chunk だけを読み、残りを `UploadPartCopy` で運ぶようにして、100 MiB で fuse-raw と互角 (§18.36)。16 MiB 未満は全体を読み書きするので、10 MiB はまだ 0.61×。
  - cold read は AC-52 (ディスクキャッシュを非同期に、4 並列の range GET と chunk ごとの populate) で直した (§18.34〜18.37)。bench で続けて読むと遅いのはホストのメモリ回収待ちで (§18.39)、毎回 `drop_caches` すると 300 MiB で fuse-raw の 1.26×。pcache の容量の既定値が無制限でホストのメモリを見ないのは設計の課題 (§18.39.1)。
  - AC-51 のレビュー指摘 (remote の区間が短いと flush が永久に失敗する、古い ETag pin、`partial_modify` が 0 に戻らない ほか) は AC-59〜65 で修正済み。
  - 実際の S3 での計測はまだ。
- **pick_victim** (`docs/design/ac58-pick-victim.md`、§18.41): vtable の末尾に `on_insert` / `on_remove` / `pick_victim` を足した。SIEVE の FIFO と hand は BPF の map に持ち、kmod は返った ino を確かめて evict する (使えなければ score 順に戻る)。LRU / SIEVE / S3-FIFO を移し、3 seed で旧版より +2〜7 ポイント (§18.42)。read 経路のコストは、1 file を 1 byte ずつ読む最悪ケースの 64 jobs で policy なし比 −4.6% (旧 SIEVE は on_hit の map update で −14.8%)。候補が数千のときの `pick_victim` の時間は未計測。
- **arena を BPF でループする場合**: `can_loop` / `bpf_for` / `bpf_loop` のコールバックは、どれも BPF のコードとして kern_vm を直接読むので GUP はない。KFunc に走査させるなら、リンクは kernel メモリに置き、arena には payload だけを置く。BPF のコールバックを取る kfunc を外部モジュールが定義できるかは未確認。

---

## ネタ採否

### 採用

| # | ネタ | 時間目安 |
|---|---|---|
| A | 動機とシステム概要 | 1.5min |
| B | `bpf_arena` の使い方 — 「誰から見ると何に見えるか」 | 3.5min |
| C | verifierの壁 → KFunc (圧縮版) | 2.5min |
| D | `struct_ops` の使い方 — vtable をBPFで実装 + ホットスワップ | 3.5min |
| E | 性能: 効いたところ — nocache 3× | 2min |
| F | **失敗談: AA-03 「arena から直接読めばもっと速い」は外れた** | 4min |
| G | 反省・知見と今後の対応 (検討中) + takeaway | 3min |

### 省く

| ネタ | 理由 |
|---|---|
| Layer 0 KFuncs の詳細 (5種API一覧) | struct_ops の例コード内に1行で組み込む。独立セクション不要 |
| CEL/Lua policy (Layer 1) | eBPFではない |
| VFS/kmodの実装詳細 | 失敗談で必要な `.read_iter` / `.read_folio` の違いだけ図 1 枚で出す |
| W-TinyLFU/S3-FIFOのアルゴリズム詳細 | eBPFの話でなくキャッシュアルゴリズムの話 |
| write / mount-s3 との比較の詳細 | 2026-10-11 版の数字は「付録」に退避。Q&A 用 |
| AA-04 (seq prefetch) / AA-05 (disk cache watermark) / Z-03 | daemon (Go) 側の改善でeBPFの話ではない |

---

## アウトライン

### 1. Intro (1.5min)

- 「S3をローカルFSとして使いたい」「FUSEは毎回ユーザー空間往復が起きる」
- 構成: kmod (VFS) + BPF (fast path / policy) + Go daemon の3層
- **今日の話**: このシステムで使った `bpf_arena` / `struct_ops` (+ KFunc) の使い方と、**arena の使いどころを間違えた話**

### 2. `bpf_arena` の使い方 (3.5min)

#### 何者か

`BPF_MAP_TYPE_ARENA` (Linux 6.9+) は、ユーザー空間が `mmap` し、BPF プログラムが `__arena` ポインタで直接読み書きする疎なメモリ領域 (最大 4 GiB)。**arena 内に置いたポインタ値がユーザー空間と BPF の両方でそのまま有効** (BPF 側は JIT が `addr_space_cast` で kern_vm_start + 下位 32 bit に変換)。通常の BPF map のように lookup/update syscall で値をコピーして出し入れする必要がない。

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

#### 「誰から見ると何に見えるか」 — 失敗談の伏線になるスライド

| 主体 | arena へのアクセス | コスト |
|---|---|---|
| ユーザー空間 (daemon) | `mmap` した user_vm を普通に deref | 通常のメモリアクセス (初回は page fault で demand-paging) |
| BPF prog | `__arena` ポインタを deref。JIT が kern_vm に変換 | 通常のメモリアクセス。不正アドレスは例外テーブルで 0 |
| **kmod (BPF でない普通のカーネルコード)** | kern_vm は使えない → **daemon の user_vm を GUP** (`pin_user_pages_remote` + `kmap_local_page`) | **アクセスのたびに mmap_lock + ページテーブル walk + pin/unpin** |

- kmod が kern_vm を使えない理由: `bpf_arena_get_kern_vm_start()` も `struct bpf_arena` も非 export、6.12 の kern_vm には BPF 以外のコンテキスト用の fault handler も無い (`docs/dev-notes/ebpf-ecosystem-gaps.md §1`)。kern_vm base の取得は `struct bpf_arena` のレイアウトを BTF から複製して `container_of` しており、カーネル更新で壊れうる (`kmod/s3fs_ebpf_dev.c`)
- **arena の「ゼロコピー」は BPF ↔ ユーザー空間の間の話**。kmod から見た arena は「よそのプロセスのユーザーメモリ」にすぎない ← §6 で効いてくる

#### S3FS での使い方 (元の設計 = うまくいった方)

- daemon が S3 から取得したデータを arena の mmap に書き込む → `ADMIT_ARENA` ioctl で (ino, uaddr, size) を kmod に登録
- `.read_folio` (page cache miss 時のみ) が GUP + `kmap_local_page` + `memcpy` で folio に充填 → **以降の read は page cache から返る** (FUSE upcall なし、GUP もなし)
- 「ゼロコピー」の範囲: daemon → kernel の受け渡しに write/ioctl のデータコピーや `/dev/fuse` バッファが無い、という意味。arena → page cache の memcpy は 1 回ある (page cache に載せる以上、FUSE でも必要なコピー)

#### 注意点・制限

- BPF からの arena ロードは verifier が中身を追跡しない (`scalar()` 扱い)。安全性は「型チェック」ではなく「どこを指しても arena 内に閉じる」設計で担保
- ページは BPF 側 `bpf_arena_alloc_pages` / `bpf_arena_free_pages`、またはユーザー空間からのページフォルトで確保 (demand-paged なので RSS は実データ分のみ)
- 本実装の arena は 32768 pages = **128 MiB** (`bpf/s3fs_fastpath.bpf.c`)。arena 自体の上限は 4 GiB (32 bit オフセット)

### 3. verifierの壁 → KFunc (2.5min, 圧縮版)

- arena 上の linked-list walk を素の `while (n)` で書くと verifier が `BPF program is too large. Processed 1000001 insn` で拒否 (`n->next` が `scalar()` でループ終了が証明不能)。再現: `sudo ./bin/s3fs-ebpf dev verifier-demo`
- **先に自分で言う: 「`can_loop` を足せば通る」** (may_goto, 6.9+)。ただし予算切れ (6.9〜6.14: 約 840 万回 / 6.15+ x86-64: 250 ms) で**エラーなく途中で抜ける**、sleep できない、`copy_from_user` などカーネル API は呼べない
- → 完走保証・sleep・カーネル API が要る処理は KFunc に逃がす:

```c
// kmod/tree_traversal.c — 3 ステップ: __bpf_kfunc 定義 → BTF_KFUNCS_START で ID set → register
__bpf_kfunc s64 s3fs_kfunc_arena_list_walk(u64 head_uaddr, u32 max_nodes, u64 *sum);
BTF_KFUNCS_START(s3fs_kfunc_walk_ids)
BTF_ID_FLAGS(func, s3fs_kfunc_arena_list_walk, KF_SLEEPABLE)  // copy_from_user は sleep しうる
BTF_KFUNCS_END(s3fs_kfunc_walk_ids)
register_btf_kfunc_id_set(BPF_PROG_TYPE_SYSCALL, &s3fs_kfunc_walk_set);
// (6.9+: BTF_KFUNCS_START で作ったセットでないと -EINVAL)
```

- BPF 側は `extern ... __ksym;` を `SEC("syscall")` から 1 行呼ぶだけ。`verifier-demo` 後半で 100k ノード walk → sum 一致
- `KF_SLEEPABLE` は**呼び出し元 prog が sleepable** である必要 (`SEC("syscall")` は OK、非 sleepable な struct_ops からは呼べない)
- この KFunc も arena を **user_vm 経由 (`copy_from_user`) で読んでいる** — kmod 側から arena を触ると必ずこの形になる (§2 の表、§6 の伏線)

### 4. `struct_ops` の使い方 — vtable をBPFで実装 + ホットスワップ (3.5min)

#### 概念

カーネルモジュールが定義した `struct xxx_ops` の実装を、**BPF プログラムがそのまま提供できる**仕組み。`tcp_congestion_ops` / sched_ext が先例。ここでは `s3fs_policy_ops` を定義してキャッシュポリシーを差し替える。

```c
// kmod/policy_struct_ops.h — kmod 側は vtable と「いつ呼ぶか」を決める
struct s3fs_policy_ops {
    int (*on_admit)   (u64 count, u64 arena_used_hint);  // 戻り値は統計のみ
    int (*on_evict)   (u64 ino);                         // 1 = evict に投票
    int (*on_pressure)(u64 count, u64 threshold);
    int (*on_hit)     (u64 ino);                         // hot path
};
```

kmod 側の必須 glue: `verifier_ops` / `cfi_stubs` / `reg` / `unreg` / `update` / `init_member` → `register_bpf_struct_ops`。fmod_ret 版の 2.3× の LOC になり、増分の 9 割は glue (`docs/dev-notes/ebpf-ecosystem-gaps.md §2`)。

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

SEC("struct_ops/on_hit")
int BPF_PROG(sieve_hit, u64 ino) {
    bpf_map_update_elem(&sieve_visited, &ino, &(u8){1}, BPF_ANY);
    return 0;
}

SEC(".struct_ops.link")          // BPF_F_LINK — link として管理
struct s3fs_policy_ops sieve_ops = {
    .on_admit = (void *)sieve_admit,
    .on_evict = (void *)sieve_evict,
    .on_pressure = (void *)sieve_pressure,
    .on_hit   = (void *)sieve_hit,
};
```

- KFunc でカーネル状態を参照する例は `bpf/policy_pressure_ops.bpf.c` (`s3fs_kfunc_admit_count()` / `s3fs_kfunc_softcap()`)
- ポリシーの時定数 (W-TinyLFU aging, time_decay の DECAY_NS など) は BPF ARRAY map + daemon フラグで実行時変更 (Z-01/Z-02/Z-05)、map の `max_entries` はロード前に `CollectionSpec` を書き換えて調整 (Z-03, `policyprog.LoadOpts`)

#### アトミックなホットスワップ

```go
// daemon/internal/policyprog/loader.go — SwapFrom
// cilium/ebpf は struct_ops link の Update 未対応 (upstream main でも ErrNotSupported) → raw syscall
syscall.Syscall(SYS_BPF, BPF_LINK_UPDATE, uintptr(unsafe.Pointer(&attr)), ...)
// attr.link_id = 既存 link の fd / attr.new_map_fd = 新 struct_ops map の fd / attr.flags = BPF_F_REPLACE
```

- kmod 側: `rcu_dereference(policy_ops)` → vtable 呼び出し → 1 syscall でポインタがアトミックにスワップ
- **fmod_ret との違い**: tracing link は `BPF_LINK_UPDATE` 非対応 → 「新 N 本 attach → 旧 N 本 close」でその間は新旧が同時に走る。`struct_ops` は 1 syscall でアトミック
- ポリシーの状態 (map) は引き継がない — 新ポリシーは空の `sieve_visited` から始まる
- デモ: `curl -X POST localhost:9600/policy/reload -d builtin:policy_sieve_ops` → dmesg に `s3fs_ebpf: policy struct_ops updated (hot swap)`。前提: daemon を `--policy-bpf=builtin:<struct_ops 版>` で起動していること。`--metrics-token` 設定時は `-H "Authorization: Bearer $S3FS_METRICS_TOKEN"`

#### struct_ops の落とし穴 (2026-10-13 の hit-rate 計測から)

`bench-hitrate` (N_FILES=30, Zipf α=0.7, 2000 access, softcap=8, `br §12.3`):

| | hit rate |
|---|---:|
| kernel 実装 best (`size_aware_ops`) | 35.2% |
| kernel 実装 `sieve_ops` / `lru_ops` | 30.6% |
| 同じ trace の **simulator** SIEVE / LRU | **43.8% / 39.2%** |
| Belady (opt) | 64.15% |

- **同じアルゴリズムなのに BPF 版が simulator 版より 9〜13 pt 低い**。全ポリシーが sim LRU を下回る
- 仮説 (未検証): vtable の形がアルゴリズムを表現しきれていない。kmod は eviction 候補の**各 ino に対して on_evict を 1 回ずつ呼んで投票を集め**、victim の順序は kmod 側 (`s3fs_victim_cmp`) が決める (`kmod/vfs_ops.c` Phase 1/2)。SIEVE の「hand が 1 つずつ進む」意味論とは違い、1 回の eviction 評価で全候補の visited bit が落ちうる
- 教訓: **「差し替え可能」と「そのアルゴリズムを書ける」は別**。struct_ops の vtable 粒度 (誰が victim を選ぶか、hook がどの粒度で呼ばれるか) の設計が本体。※ §6 の AA-03 で on_hit の呼ばれ方も per-folio → per-`read_iter` 呼び出しに変わっている

### 5. 性能: 効いたところ — nocache 3× (2min)

「ebpf が速いのはキャッシュが効いているからでは？」に答えるため、**両方キャッシュ無効**で比較:

| 指標 (2026-10-11、同一 run) | fuse-raw | ebpf-nocache | 差 |
|---|---:|---:|---:|
| open+read+close p50 | 1.691 ms | **526 µs** | **3.2×** |

- bpftrace で fuse-raw は 1 op あたり `/dev/fuse` **4.01 往復** (read_iter 2 + flush 1 + release 1)、S3 GET は 0 → 1.69 ms はほぼ純粋な IPC コスト
- ebpf は miss 時の upcall が共有メモリ経由で、open/flush/release の往復が無い → **構造差**
- キャッシュ有り同士 (2026-10-13, `br §12.1`): warm cat は ebpf 586 µs / fuse-cache 1.4 ms / mount-s3 769 µs、open+read+close p50 は ebpf 3.2 µs / fuse-cache 513 µs / mount-s3 113 µs
- **ただし「open 済み fd の read」は速くない** → ここから §6 へ

### 6. 失敗談: AA-03 「arena から直接読めばもっと速い」は外れた (4min)

#### 出発点: 並列 read で負けていた

2026-10-11 の `fio psync` (shared small file, `drop_caches=1`, `br §10.2`):

| jobs | ebpf | fuse-raw | ebpf/fuse-raw |
|---:|---:|---:|---:|
| 64 | 4638k | 5320k | 87% |

#### 立てた仮説と打った手 (3b9bd79)

- 仮説: 「`.read_folio` の admit hash の spinlock と、page cache を経由する分のオーバーヘッドが重い」
- **AA-01**: admit hash の `spinlock` → `rwlock` (reader 同士は並行できるはず)
- **AA-03**: `.read_iter` を自前実装。admit hit なら page cache を通さず **arena から `copy_to_iter` で直接ユーザーバッファへ**

```c
// kmod/vfs_ops.c — s3fs_arena_copy_to_iter (AA-03, 抜粋)
while (remaining > 0) {
    mmap_read_lock(mm);                                  // daemon の mm
    pin_user_pages_remote(mm, abs & PAGE_MASK, 1, 0, &ap, &locked);
    mmap_read_unlock(mm);
    akva = kmap_local_page(ap);
    copied = copy_to_iter(akva + pgoff, chunk, to);
    kunmap_local(akva);
    unpin_user_pages(&ap, 1);
    ...
}
```

- 受け入れ条件 (先に書いておいた): **64 jobs で ebpf ≥ fuse-raw**、**microbench p50 ≤ 2 µs**

#### 結果 (2026-10-13, `br §12`)

| 指標 | 目標 | 前 (10-11) | 後 (10-13) |
|---|---|---:|---:|
| microbench open+read+close p50 | ≤ 2 µs | 3.075 µs | **3.216 µs** (改善なし) |
| fio read 64 jobs: ebpf / fuse-raw | ≥ 100% | 4638k / 5320k = 87% | **4045k / 5289k = 76%** (悪化) |
| fio read 1 job: ebpf / fuse-raw | — | 1495k / 1616k = 92.5% | 1460k / 1636k = 89% (ほぼ同じ) |

- ebpf は **8 jobs で ~4300k に飽和**。fuse-raw / fuse-cache / mount-s3 は 64 jobs で ~5300k まで伸びる
- 絶対値は run 間で動くので比で見る。**どれも目標に届かず、並列は悪化**
- 訂正 (2026-10-22): 1 job の「前」を以前は 1199k / 1165k = 103% (「逆転負け」) としていたが、これは U-05 (計測ごとの `drop_caches=1`) の前の run の数字だった。同じ条件の 10-11 の run (`br §10.2`) では 92.5% で、1 job は AA-03 の前から負けていた

#### なぜ外れたか (コードから読める構造。perf/lockstat での裏取りは未)

1. **page cache を捨てたら、毎 read が GUP になった**
   - 旧: GUP は page cache miss 時 (`.read_folio`) だけ。2 回目以降は page cache (xarray の RCU lookup) から返る
   - 新: **hit のたびに** daemon の `mmap_read_lock` + ページテーブル walk + pin/unpin。GUP は「一度 pin して長く使う」ための API で、read ごとに呼ぶものではない
2. **全 reader が同じ共有状態に書き込む**
   - 64 スレッドが 1 つの daemon mm の `mmap_lock` (rwsem の reader カウント) と、同じ arena ページの refcount (pin は `GUP_PIN_COUNTING_BIAS` を加算) を叩く → cacheline bouncing。8 jobs で飽和するのと整合的
   - AA-01 の `rwlock` も read 側でロック語に書き込む。**rwlock は reader 同士をスケールさせない**。backlog AA-01 の本来の案 (a) は RCU だった
3. **§2 の表の 3 行目を忘れていた**
   - 「arena はゼロコピー」を kmod にも当てはめてしまった。kmod から見た arena は**他プロセスのユーザーメモリ**で、アクセスごとに GUP 税がかかる。arena は BPF ↔ user のデータ共有には最適だが、**kmod のデータプレーン (毎 read で触る場所) には向かない**
   - 元の設計 (arena → folio に 1 回だけ充填、以降は page cache) のほうが筋が良かった

#### 敗北要因の整理 (2026-10-22、スライド fail-factors)

小さいファイルの並列 read、fuse-raw 比。上の「なぜ外れたか」はコードからの推測で、その後の計測で次のように切り分けた。

| 要因 | いつ効くか | 効き方 | 確度 |
|---|---|---|---|
| `fentry/generic_file_read_iter` | page cache を通る read のたび (AA-03 の前と pcache モード) | 外すと 0.94 → 1.00。フックの固定費だけで上限 0.974 | 64 jobs の A/B で確定 (`br §18.16`、`§18.23`) |
| policy の `on_hit` | hit の read のたび | 旧 SIEVE −13〜15%、pick_victim 版 −5〜6% | A/B で確定 (`br §18.42`) |
| GUP と rwlock | AA-03 の arena 経路 | 外しても 64 jobs は 0.63〜0.64 のまま。大きいファイルの latency には効く | 並列の主因ではない (`br §14`、`§15`) |
| 1 job の「103%」 | 比べ方 | U-05 の前の run。同条件では 92.5% → 89% | 計測の誤り |

- 1 job の差 (7〜11%、read 1 回あたり約 50 ns) は固定費。AA-03 の前は fentry と on_hit、後は arena hit で `generic_file_read_iter` を通らないので fentry は動かず、GUP と rwlock と見ている。1 job で A/B を取ったことはなく、推定。
- `br §13.1` の GUP 1.7 µs は別の負荷で 106 回だけ測った値で、1 job の read 1 回 (約 685 ns) と合わない。ここでは使わない。

### 7. 反省・知見と今後の対応 (3min)

#### 反省・知見

| 知見 | 内容 |
|---|---|
| arena の「ゼロコピー」は**主体を限定して**言う | BPF prog とユーザー空間の間は真。非 BPF のカーネルコードから見ると GUP 経由のユーザーメモリ |
| page cache は**倒す相手ではなく使う相手** | カーネルの read 最速路はすでに lockless (RCU)。bypass すると、その最適化を自前で作り直す羽目になる |
| スケールの敵はコピーより**共有書き込み** | memcpy を 1 回減らしても、mmap_lock / refcount / rwlock カウンタを全 reader が叩けば負ける |
| 受け入れ条件を先に書く | 「≤ 2 µs」「≥ fuse-raw」を事前に決めていたから「失敗」と言い切れた |
| 仮説 → 実装の前にプロファイル | 「lock が重い」は計測ではなく推測だった。先に `perf c2c` / `lockstat` を取るべきだった |
| struct_ops は vtable 設計が本体 | 差し替えの仕組みは完璧でも、hook の粒度がアルゴリズムに合わないと sim の性能が出ない (§4) |

#### 今後の対応 (検討中 — 未実装・未計測)

| # | 対応 | 狙い | 状態 |
|---|---|---|---|
| 1 | AA-03 の直読みを既定 off に戻し (module param で opt-in)、`.read_folio` + page cache 経路を既定に | 回帰を戻す | 案 |
| 2 | `perf c2c` / `lockstat` / `perf lock` で 64 jobs のボトルネックを計測 | §6 の「なぜ」を推測から事実に | 案 (最優先) |
| 3 | admit hash を RCU 化 (`hlist_for_each_entry_rcu` + `kfree_rcu`) | reader が共有語に書かない (AA-01 本来の案 a) | 案 |
| 4 | `ADMIT_ARENA` 時に 1 回だけ pin して `struct page *` 配列を admit entry に保持 | per-read の GUP / `mmap_lock` を消す。evict / arena free 時に unpin が要る | 案 |
| 5 | upstream: K-01 `bpf_arena_get_kern_vm_start` の export、K-02 kern_vm の汎用 fault handler | kmod が kern_vm を直接 deref → GUP 自体が不要に。BTF hack (`struct bpf_arena` の複製) も消える | backlog K-01/K-02 |
| 6 | policy vtable に「victim を BPF が選ぶ」hook (例: `pick_victim`) を追加 / sim と kernel の差を分解 | §4 の hit-rate 差の解消 | 案 |

- スライドでは 1・2・5 を中心に。「arena を kmod から素直に使える API が upstream に無い」は**コミュニティへの問題提起**として締めに使える

#### takeaway

| 技術 | 一言 | 向いている | 向いていない (今回わかった) |
|---|---|---|---|
| `bpf_arena` | BPF ↔ ユーザー空間のポインタごと共有できるメモリ | BPF とユーザー空間でデータ構造を共有、ユーザー空間からの受け渡し | **非 BPF のカーネルコードの hot path** (毎回 GUP) |
| KFunc | verifier の壁を安全に越える逃がし先 | 完走保証が要る walk、sleep、カーネル API | 非 sleepable な prog から sleepable 処理 |
| `struct_ops` | vtable を BPF で実装 + 1 syscall のアトミックスワップ | 差し替え可能なアルゴリズム、無停止の切り替え | vtable 粒度がアルゴリズムに合っていない場合 (設計が本体) |

- コード: `kmod/vfs_ops.c` (`s3fs_arena_copy_to_iter` / `s3fs_ebpf_read_folio`)、`kmod/tree_traversal.c` (KFunc)、`kmod/policy_struct_ops.c` + `bpf/policy_sieve_ops.bpf.c` (struct_ops)

---

## スライド補足メモ

- **全体の山場は §6**。§2 の「誰から見ると何に見えるか」表の 3 行目を伏線として置き、§6 で回収する。§3 の KFunc も arena を `copy_from_user` で読んでいることを一言添えておくと伏線が 2 本になる
- **失敗談の見せ方**: 「仮説 → 受け入れ条件 → 結果の表 → 原因 3 つ」の順。結果表は「目標」列を必ず残す (失敗の定義を明示するため)
- **原因は「推測」と明言する**: perf/lockstat の裏取りは未。Q&A で突かれたら今後の対応 #2 に繋げる。発表までに #2 が取れれば差し替える
- **性能スライドのポイント**: §5 は nocache 3× だけに絞る。大きい倍率 (169× など) は付録。「効いたところ」は短く、「効かなかったところ」に時間を使う
- **struct_ops demo**: `curl -X POST localhost:9600/policy/reload -d builtin:policy_sieve_ops` でライブスワップ。`/metrics` の `s3fs_ebpf_folio_hits_total` / `s3fs_ebpf_policy_evictions_total` の変化を見せる
- **コード引用元**:
  - `bpf/s3fs_fastpath.bpf.c` (arena 定義, `s3fs_deep_walk_reject`)
  - `kmod/tree_traversal.c` (`BTF_KFUNCS_START`, `s3fs_kfunc_arena_list_walk`)
  - `bpf/kfunc_walk.bpf.c` (KFunc 呼出側)
  - `kmod/policy_struct_ops.c` (reg/unreg/update)
  - `bpf/policy_sieve_ops.bpf.c` (struct_ops 実装例)
  - `daemon/internal/policyprog/loader.go` (`SwapFrom` / `BPF_LINK_UPDATE` / `LoadOpts`)
  - `kmod/vfs_ops.c` (`s3fs_arena_copy_to_iter`, `s3fs_ebpf_read_iter`, `admit_snapshot`)

---

## 付録 (Q&A 用): 2026-10-11 時点の性能数字

本編から外した数字。詳細は `docs/journal/benchmark-results.md` §8, §11。

### キャッシュ有り同士: ebpf-A+C vs fuse-cache (2026-10-11 同一 run)

| 指標 | fuse-cache | ebpf-A+C | 差 |
|---|---:|---:|---:|
| warm cat (1 MiB) | 1.6 ms | **730 µs** | 2.1× |
| open+read+close p50 | 519 µs | **3.075 µs** | **169×** |

- 169× は **open ごとの FUSE upcall** が主因。cycles/IPC は `cat` プロセス全体 (fork/exec 込み) の値なので使わない

### write (durable, S-08 修正後, mean ± σ)

| Size | fuse-raw | ebpf-fsync | 差 |
|---|---:|---:|---:|
| 1 MiB | 18.2 ± 1.5 ms | **14.2 ± 0.8 ms** | 1.28× |
| 10 MiB | 74.5 ± 3.7 ms | **43.9 ± 4.2 ms** | 1.70× |
| 100 MiB | 359.4 ± 10.7 ms | **208.8 ± 25.3 ms** | 1.72× |

### mount-s3 (v1.24.0, --cache)

- 2026-10-11: p50 110 µs (fuse-cache 比 4.7× 速い、ebpf-A+C 比 36× 遅い)
- 2026-10-13: warm cat 769 µs (ebpf 586 µs)、100m.bin cat は fuse-cache 11.1 ms < ebpf 13.0 ms < mount-s3 44.9 ms — **大ファイルでは disk cache の fuse-cache が arena の ebpf を上回る**
- 並列 read 64 jobs: mount-s3 5326k / ebpf 4045k (AA-06 で追加)

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

発表前の再検証。アウトラインの各主張を実装 (HEAD `2309665`)・ベンチ記録・カーネルソースと突き合わせ、本文は修正済み。

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
- **write**: ✅ 2026-10-11 に bench-write.sh を RUNS=10 で再実行。ebpf-fsync の mean ± σ と fuse-raw の mean ± σ が揃い、全サイズで ebpf-fsync 優位と確定
- **hot read (open 済み fd)** は 3 者同等 (§10.2)。「open を含むアクセス」が速い、と限定して言う

### 発表前に追加で測ると良いもの (優先順)

1. ✅ `bench-3way.sh` 同一 run — 2026-10-11 に実施。3.2× と 169× が同一 run 由来で確定
2. ✅ 3× の内訳 — 2026-10-11 bpftrace 計測で確定。fuse-raw: 4.01 upcall/op × 423µs/upcall = 1.69ms。S3 GET delta=0 (page cache warm)。差の主因は `/dev/fuse` IPC 往復回数 (4 vs ≈1)。詳細 `benchmark-results.md §11.1/§11.2`
3. ✅ `bench-write.sh` S-08 後・3 サイズ再計測 — 2026-10-11 実施。ebpf-fsync が全サイズで fuse-raw より速いと確定
4. ✅ I-01 で追加した mount-s3 の実測 — 2026-10-11 実施。p50=110µs (--cache)。fuse-cache 比 4.7× 速い。ebpf-A+C 比 36× 遅い。詳細 `benchmark-results.md §11.3`

---

## 再構成メモ (2026-10-13)

HEAD `3b9bd79` (AA-01〜AA-06 / Z-03) の再計測 (`benchmark-results.md §12`) で、AA-01/AA-03 が受け入れ条件 (64 jobs で ebpf ≥ fuse-raw、microbench p50 ≤ 2 µs) を満たさず、64 jobs は 87% → 76% に悪化したことが判明。これを受けて本文を再構成した。

| 変更 | 内容 |
|---|---|
| 中心テーマ | 「3 技術の紹介」→「arena / struct_ops の使い方 + arena の使いどころを誤った失敗談」 |
| §2 arena | 「誰から見ると何に見えるか」表を追加 (BPF / user / kmod でコストが違う)。§6 の伏線 |
| §3 KFunc | 圧縮。arena を `copy_from_user` (user_vm) で読んでいる点を伏線として追加 |
| §4 struct_ops | 2026-10-13 hit-rate (kernel 実装 < 同 trace の simulator) から「vtable 粒度が本体」を追加 |
| §5 性能 | nocache 3× のみに絞り、169× / write / mount-s3 は付録へ |
| §6 (新) | AA-03 の仮説・受け入れ条件・結果・原因 (推測) |
| §7 (新) | 反省・知見、今後の対応 (検討中)、takeaway に「向いていない」列 |

- §6 の原因 (mmap_lock / page refcount / rwlock の cacheline bouncing) は**コードから読める構造に基づく推測**で、perf c2c / lockstat による裏取りは未実施
- §4 の hit-rate 差の原因 (on_evict の投票方式と SIEVE の hand 意味論の不一致) も仮説
- 今後の対応 #1〜#6 はすべて未実装。#5 のみ backlog (K-01/K-02) に既存。残りは backlog 未登録
