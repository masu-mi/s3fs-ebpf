# eBPF フックポイントのギャップ分析

> 作成: 2026-10-01  
> 背景: キャッシュアルゴリズム判断を全 eBPF に移す価値の検討、および他の VFS/FUSE 実装と比べた観測点の不足を整理する。

---

## 1. 判断を全部 eBPF に移す価値はあるか

### 結論

**高価値な決定 (admit / evict / hit / pressure) はすでに eBPF に移っている。残りは「I/O 実行と不可分な判断」であり全移行のコストに見合わない。ただし `on_read_miss` pre-filter だけは具体的な改善余地がある。**

### 12 決定点の現状マップ

policy-engine-design.md §2 の 12 決定点を現状と照合する:

| # | 決定点 | 予算 | 現状 | 全 eBPF 化の効果 |
|---|---|---|---|---|
| 11 | on_hit | super-hot | ✅ struct_ops | 完了 |
| 5 | on_admit | warm | ✅ struct_ops | 完了 |
| 12 | on_evict | warm | ✅ struct_ops | 完了 |
| 6 | on_pressure | warm | ✅ struct_ops | 完了 |
| 3 | on_stat | hot | ✅ fentry/vfs_getattr_nosec | 完了 |
| 4 | on_read_miss | warm | ❌ daemon Go | **改善余地あり** |
| 7 | on_write | hot | ❌ daemon coalescer | 小 |
| 8 | on_release | warm | kmod→daemon ioctl | 小 |
| 9 | on_fsync | warm | kmod→daemon blocking | なし |
| 1 | on_startup | cold | daemon Go | ゼロ (S3 I/O 必須) |
| 2 | on_sync_tick | cold | daemon Go | ゼロ (S3 I/O 必須) |
| 10 | on_periodic | cold | daemon goroutine | ゼロ |

### 「全移行」が意味をなさない理由

eBPF がどれだけ賢くても S3 GET/PUT はカーネル内で実行できない。on_startup / on_sync_tick / on_read_miss の実行部分はこの I/O と不可分なので、判断を eBPF に移しても実行は daemon に残る。

現在の struct_ops 設計はこれを体現している: on_admit/on_evict は「投票 (vote)」を返し、S3 fetch は daemon が行う。**「判断を eBPF、実行を kmod/daemon」の分離はすでに適切に設計されている。**

### 唯一の具体的な改善余地: `on_read_miss` の pre-filter

現在は全 read miss が daemon upcall になる。eBPF で「このミスはキャッシュ拒否」を事前判定できれば daemon wake を省ける:

```c
// 改善案: s3fs_policy_ops に on_read_miss を追加
struct s3fs_policy_ops {
    // ... 既存 4 hook
    int (*on_read_miss)(u64 ino, u64 size, u64 access_count);
    // → 0: daemon upcall する、1: refuse (daemon を起こさない)
};
```

100 MB 超のファイルやランダムアクセスパターン検出 (`rand_count >> seq_count`) で「refuse」を返せば、S3 fetch コストをかけないまま kmod 内で完結できる。**この判断を下すには読み取りアクセスパターンの観測点 (§2) が先に必要。**

---

## 2. 他の VFS / FUSE と比べた観測点の不足

### 2.1 現在使っているフックポイントの全体像

```
fentry/vfs_getattr_nosec       ← stat 観測 (唯一の VFS フック)
fmod_ret/s3fs_policy_on_*      ← kmod noinline 関数への return 値 override
struct_ops/s3fs_policy_ops     ← vtable 差し替え
SEC("syscall")                 ← BPF_PROG_TEST_RUN プローブ用
```

stat 以外の VFS 操作は kmod が直接実装しており eBPF の観測点がない。

### 2.2 操作別の観測状況比較

| 操作 | FUSE daemon | s3fs-ebpf kmod | s3fs-ebpf eBPF |
|---|---|---|---|
| stat / getattr | ✅ 全量受信 | ✅ .getattr | ✅ fentry hook |
| read | ✅ 全量受信 | ✅ .read_folio | ❌ **観測点なし** |
| lookup | ✅ 全量受信 | ✅ .lookup | ❌ 観測点なし |
| open | ✅ 全量受信 | ✅ .open | ❌ 観測点なし |
| write | ✅ 全量受信 | ✅ .write_iter | ❌ 観測点なし |
| mmap fault | ❌ (mmap は FUSE 外) | △ read_folio 経由 | ❌ 観測点なし |
| cgroup 情報 | ❌ (daemon は cgroup 外) | ❌ | △ KFunc で取得可能 |
| cache policy hot swap | ❌ daemon restart 必要 | ❌ | ✅ struct_ops |

**FUSE が s3fs-ebpf より観測量が多い操作は read / lookup / open の 3 つ。** ただし FUSE は観測できる代わりに全部 daemon を経由するため、ゼロコンテキストスイッチの優位性は消える。

### 2.3 不足フックポイントの詳細 (影響度順)

#### ① `fentry/generic_file_read_iter` — **最大の穴**

```
read(2)
  └─ vfs_read
       └─ generic_file_read_iter   ← ここに eBPF がいない
            └─ filemap_read / read_folio
                 └─ kmod の .read_folio
```

現在の on_hit は「どの ino がヒットしたか」しかわからない。`generic_file_read_iter` を hook すれば:

- `kiocb.ki_pos` と `iov_iter.count` からシーケンシャル / ランダムを判定
- 「次のチャンクを prefetch すべきか」を on_read_miss 前に eBPF で決定
- ML モデルの大ファイル読み出し (大 ino × 連続 offset) を検出して prewarm 優先度を上げる

on_pressure だけでは「何を evict するか」しかわからず「何を prewarm すべきか」の信号が eBPF に届かない。

**→ このフックを追加する実装が本ドキュメントに対応する変更 (§3 参照)**

#### ② `fentry/d_lookup` — lookup の観測欠如

ExtFUSE の核心は lookup のショートサーキット。s3fs-ebpf は `.lookup` を kmod で実装しているが eBPF から観測できない。ネガティブキャッシュの dentry 頻度が見えないため evict 候補の精度が落ちる。

#### ③ LSM フック (`lsm/inode_permission`) — per-cgroup アクセス制御の欠如

```c
SEC("lsm/inode_permission")
int BPF_PROG(s3fs_perm, struct inode *inode, int mask) {
    u64 cgid = bpf_get_current_cgroup_id();
    // cgroup 別 policy テーブルを引いてアクセス制御
}
```

LSM フックなら VFS に入る前に cgroup コンテキストで遮断でき、kmod まで処理が到達しない分コストが低い。ebpf-fuse-advantages-roadmap.md §3-1 の per-cgroup policy と組み合わせると効果が高い。

#### ④ `BPF_PROG_TYPE_ITER` (iter/bpf_map_elem) — arena GC の欠如

現在は daemon goroutine が admit table の GC を担当している。BPF iterator を使えば daemon 停止中も arena が解放される。ebpf-fuse-advantages-roadmap.md §3-4 参照。

#### ⑤ `fentry/filemap_fault` — mmap アクセスの不可視

S3 上の大ファイルを mmap して読む場合 (ML 推論の典型)、`vfs_getattr_nosec` は最初の stat でしか発火しない。page fault ごとのアクセス位置が eBPF に見えないため、どのオフセット範囲が hot かを policy が知る方法がない。

#### ⑥ `fentry/vfs_create` — ファイル作成時の proactive warm 欠如

FUSE daemon は `FUSE_CREATE` を受け取った瞬間にキャッシュ戦略を組める。s3fs-ebpf では kmod の `.create` が動くが eBPF 観測点がなく「作ったばかりのファイルは直後に読まれる可能性が高い」という信号を policy に渡せない。

### 2.4 ExtFUSE との比較

ExtFUSE (OSDI 2019) の核心は FUSE カーネルドライバに eBPF フックを追加し、lookup/getattr をデーモン upcall なしでショートサーキットすること。

| 機能 | ExtFUSE | s3fs-ebpf |
|---|---|---|
| lookup ショートサーキット | ✅ eBPF maps でインライン応答 | ❌ kmod 固定、eBPF 観測なし |
| getattr ショートサーキット | ✅ eBPF maps でインライン応答 | ✅ fentry + inode_index |
| 大容量コンテンツキャッシュ | ❌ maps サイズ制限 (~数 MB) | ✅ bpf_arena (最大 4 GiB) |
| アルゴリズム extensibility | ❌ maps 固定、選択肢なし | ✅ struct_ops hot swap |
| unbounded traversal | ❌ verifier 制限 | ✅ KFunc escape hatch |
| 読み取りパターン観測 | ✅ read hook あり | ❌ (§3 で追加予定) |

---

## 3. 実装: `fentry/generic_file_read_iter` の追加 (R-01)

最も効果が高い `fentry/generic_file_read_iter` を実装する。これ 1 本で:

- アクセスパターン (sequential / random) の信号が policy に届く
- W-TinyLFU の frequency estimate が「stat 頻度」から「実 read 頻度」ベースに変わる
- `on_read_miss` pre-filter の実装基盤が揃う

### 3.1 追加する共有型 (`include/s3fs_shared_types.h`)

```c
// 既存 enum に追加
enum s3fs_stat_slot {
    S3FS_STAT_FAST_HITS       = 0,
    S3FS_STAT_SLOW_MISSES     = 1,
    S3FS_STAT_UNKNOWN_FS      = 2,
    S3FS_STAT_READ_ITER_HITS  = 3,  // NEW: read_folio cache hit
    S3FS_STAT_READ_ITER_MISSES = 4, // NEW: read miss (ino 未登録)
    S3FS_STAT_SLOT_MAX,             // 3 → 5
};

// NEW: per-ino アクセスパターン (BPF map の value 型)
struct s3fs_read_pattern {
    __u64 last_end_pos;  // 前回の read 終端 offset
    __u32 seq_count;     // シーケンシャル read 回数
    __u32 rand_count;    // ランダム read 回数
    __u64 total_bytes;   // 累計 read バイト数
};
```

### 3.2 BPF プログラム (`bpf/s3fs_fastpath.bpf.c`)

```c
// 追加マップ: ino → read pattern
struct {
    __uint(type, BPF_MAP_TYPE_LRU_HASH);
    __type(key, __u64);
    __type(value, struct s3fs_read_pattern);
    __uint(max_entries, 4096);
} s3fs_read_pattern_map SEC(".maps");

// 追加プログラム
SEC("fentry/generic_file_read_iter")
int BPF_PROG(s3fs_fp_read_iter,
             struct kiocb *iocb, struct iov_iter *to)
{
    // s_magic フィルタ → inode_index 参照 → pattern 更新 → miss emit
}
```

シーケンシャル判定: `pos <= last_end_pos + SEQUENTIAL_SLACK (= 4096)` なら seq、それ以外は rand。

### 3.3 daemon 側の変更

- `loader.go`: `S3fsFpReadIter` プログラム + `S3fsReadPatternMap` を runtimeObjs に追加、`AttachFentryReadIter()` メソッド追加
- `main.go`: `runFastpath` で `AttachFentryReadIter()` を呼ぶ、`printCounters` に read iter カウンタを追加
- Prometheus metrics に `s3fs_ebpf_read_iter_hits_total` / `_misses_total` を追加

### 3.4 pattern-aware admit (`--pattern-aware-admit`)

R-01 フックのデータを即座にキャッシュ判断に活用する最適化 (実装済み)。

**動作原理**: `fentry/generic_file_read_iter` は kmod の `.read_folio` が block する**前**に発火するため、`handleReadMiss` goroutine が起動した時点で `seq_count ≥ 1` が保証される。`SeqRatio() = 1.0 > 0.70` → 最初の miss で `s3c.Get()` (全体) を発行し、以降の chunk miss を消去。

**効果の理論値**: 10 MiB ファイルの初回 cold read は D-01 chunk では 3 × S3 GetRange、R-01 pattern-aware では 1 × S3 Get。MinIO ローカルで ~30 ms/RPC なら **90 ms → 30 ms** (3×) の cold-read tail 削減。

**評価スクリプト**:
- `scripts/bench-read-iter.sh` — fentry hook の read latency オーバーヘッド測定 (with / without `--no-read-iter-hook`)
  - 1 スレッドの遅延しか測らないので、並列 read での共有 cacheline の取り合いは見えない。64 jobs で約 19% の負担があった件と、フックの注意点・採用基準は [`read-iter-hook-overhead.md`](read-iter-hook-overhead.md)
- `scripts/bench-read-pattern-policy.sh` — pattern-aware admit シナリオ (upcall count と初回 cold-read 時間の比較)

**将来の拡張 (`on_read_miss` pre-filter)**: `rand_count / (seq_count + rand_count) > 0.8` のような条件で `struct_ops.on_read_miss` が refuse を返すことで daemon wake そのものを削減できる (R-02)。

---

## 4. 残課題 (backlog)

| ID | 内容 | 依存 |
|---|---|---|
| R-01 | `fentry/generic_file_read_iter` 追加 + `--pattern-aware-admit` sequential 大ファイル最適化 | **実装済 + bench スクリプト追加** |
| R-02 | `struct_ops` に `on_read_miss(ino, size, rand_ratio)` hook 追加 | R-01 データ蓄積後 |
| R-03 | `fentry/d_lookup` で lookup 観測 + ネガティブキャッシュ frequency | — |
| R-04 | `lsm/inode_permission` per-cgroup policy (→ Q-04 との合流) | — |
| R-05 | `iter/bpf_map_elem` arena GC (→ Q-02 との合流) | — |
| R-06 | `fentry/filemap_fault` mmap アクセス観測 | — |

---

## 5. 参照

- `docs/design/policy-engine-design.md §2` — 12 決定点の taxonomy
- `docs/design/ebpf-fuse-advantages-roadmap.md §3` — per-cgroup / bpf_timer / BPF iterator の実装計画
- `bpf/s3fs_fastpath.bpf.c` — fentry/vfs_getattr_nosec の現実装 (R-01 の参考)
- `include/s3fs_shared_types.h` — 共有型定義
- `daemon/internal/ebpf/loader.go` — BPF オブジェクト管理
- ExtFUSE: Bijlani & Ramachandran, OSDI 2019
