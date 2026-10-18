# eBPF-FS の FUSE に対する優位性と実装ロードマップ

> 作成: 2026-10-01  
> 背景: daemon 実装を厚くした場合 (xk6/k6 モデルのような plugin 対応) でも FUSE の再実装にならない理由を整理し、未実装の優位性それぞれについて実装計画を記す。

---

## 1. 優位性の整理

「daemon がすべてを担う」構成にしても、以下 3 点は構造上 FUSE 側に持っていけない。

### 1-1. キャッシュヒット時のゼロコンテキストスイッチ

`fentry/vfs_getattr_nosec` が BPF マップのデータを直接返すとき、daemon は一切関与しない。FUSE 最良ケースでも `read(2)` → `/dev/fuse` → daemon → reply の 2 回コンテキストスイッチが必須。`fast_hits` カウンタが増えるパスがこれに相当する。

### 1-2. `bpf_arena` + `pin_user_pages_remote` によるゼロコピー read

`s3fs_ebpf_admit_replace` が daemon の arena mmap 領域を `pin_user_pages_remote` でカーネルが pin し、page cache のフォリオとして直接マッピングする。FUSE は `.read()` ハンドラで必ず `fuse_copy_to_iter()` のコピーが発生する。大きなオブジェクト (ML モデル、Parquet ファイル等) では差が支配的になる。

### 1-3. `struct_ops` によるカーネルグラニュラリティのポリシーホットスワップ

```c
struct s3fs_policy_ops {
    int (*on_admit)(u64 count, u64 arena_used_hint);
    int (*on_evict)(u64 ino);
    int (*on_pressure)(u64 count, u64 threshold);
    int (*on_hit)(u64 ino);
    /* AC-58 (2026-10-17) で末尾に追加 */
    int (*on_insert)(u64 ino);
    int (*on_remove)(u64 ino);
    u64 (*pick_victim)(void);
};
```

FUSE daemon で LRU を SIEVE に切り替えるには daemon 再起動が必要。`link.Update()` 一発で検証済み BPF コードをホットスワップでき、カーネルクラッシュのリスクがない。

---

## 2. 現在の実装状況

| 優位性 | 状態 | コード位置 |
|---|---|---|
| ゼロコンテキストスイッチ (Path A) | ✅ 実装済み | `daemon/internal/ebpf/` fentry attach、BPF `inode_index` map lookup |
| ゼロコピー read (Path B) | ✅ 実装済み | `kmod/vfs_ops.c` `s3fs_ebpf_admit_replace` + `pin_user_pages_remote` |
| struct_ops ホットスワップ | ✅ 実装済み | `kmod/policy_struct_ops.c`、`bpf/policy_*.bpf.c`、POST /policy/reload |
| fmod_ret per-cgroup ポリシー | ❌ 未実装 | — |
| bpf_timer 自律 eviction | ❌ 未実装 | — |
| KFunc + radix tree readdir | ❌ 未実装 | — |
| BPF Iterator arena GC | ❌ 未実装 | — |

以下 §3 で未実装の 4 項目それぞれについて実装計画を述べる。

---

## 3. 未実装の発展可能性と実装計画

### 3-1. `fmod_ret` + `cgroup_id()` per-cgroup ポリシー

#### 目的

コンテナ A は SIEVE、コンテナ B は LRU という cgroup 単位のキャッシュポリシー分離。FUSE では daemon が cgroup 情報を取得して判断する迂回路が必要だが、eBPF は `bpf_get_current_cgroup_id()` で呼び出し元の cgroup を直接取得できる。

#### 実装ステップ

**Step 1 — kmod: per-cgroup vtable map の追加** (1〜2 日)

`kmod/policy_struct_ops.c` に `BPF_MAP_TYPE_HASH` ベースの `cgroup_id → s3fs_policy_ops *` テーブルを追加。`s3fs_policy_ops_eval_admit` 等の eval 関数がまず cgroup hash を引き、ヒットすれば cgroup vtable を使い、miss ならグローバル vtable にフォールバックする。

```c
// kmod/policy_cgroup.h (新規)
int  s3fs_policy_cgroup_register(u64 cgroup_id, struct s3fs_policy_ops *ops);
void s3fs_policy_cgroup_unregister(u64 cgroup_id);
```

**Step 2 — BPF: cgroup-aware policy プログラム** (1 日)

`bpf/policy_cgroup_sieve.bpf.c` を新規作成。`on_admit` 内で `bpf_get_current_cgroup_id()` を呼び、cgroup ごとの per-entry state map を分けて管理する。マップは `BPF_MAP_TYPE_HASH_OF_MAPS` か cgroup id キー付きの単一 hash で実装。

**Step 3 — daemon: cgroup 登録 API の追加** (1〜2 日)

`/dev/s3fs_ebpf` に新 ioctl `S3FS_IOC_REGISTER_CGROUP_POLICY` を追加。daemon が `POST /policy/cgroup` で `{ cgroup_id, policy_bpf_path }` を受け取り、cgroup 紐付け load + attach を行う。`--cgroup-policy` フラグでも指定可能にする。

**Step 4 — テスト** (1 日)

`scripts/test-cgroup-policy.sh` を追加。`cgcreate -g memory:/testA` + `/testB` で 2 cgroup を作り、cgroup A に SIEVE、B に LRU を割り当て、`cgexec` でそれぞれ cat を実行して `/metrics` の `policy_hook_invocations` カウントが正しく分かれることを確認する。

**推定作業量**: 4〜5 日

---

### 3-2. `bpf_timer` による自律 eviction

#### 目的

現在 `drainEvictionsLoop` は daemon goroutine でタイマー駆動 (2s interval)。`bpf_timer` を使えば kmod→daemon→kmod のフィードバックループなしに、BPF プログラム自身が圧力を感知して evict 候補をリングバッファに積める。eviction のレイテンシが daemon のポーリング間隔に左右されなくなる。

#### 実装ステップ

**Step 1 — BPF: timer 起動 + pressure 検知** (2 日)

`bpf/policy_timer_evict.bpf.c` を新規作成。グローバルな `struct bpf_timer` を持ち、`on_admit` hook の中で初回のみ `bpf_timer_init` + `bpf_timer_start(5_000_000_000 /* 5s */)` を発行。タイマーコールバック内で `s3fs_kfunc_admit_count()` と `s3fs_kfunc_softcap()` を比較し、圧力比 ≥ 1.0 なら `bpf_ringbuf_output` で evict 候補を emit してから自分を再アーム (`bpf_timer_start`)。

```c
// bpf/policy_timer_evict.bpf.c (新規)
static int timer_cb(void *map, int *key, struct bpf_timer *timer) {
    u64 count = s3fs_kfunc_admit_count();
    u64 cap   = s3fs_kfunc_softcap();
    if (count > cap)
        emit_evict_candidates();
    bpf_timer_start(timer, 5ULL * 1000000000, 0);
    return 0;
}
```

**Step 2 — kmod: タイマー用リングバッファの追加** (1 日)

`kmod/vfs_ops.c` に `bpf_timer_evict_ringbuf` を追加し、タイマー経路からの evict 候補を daemon に通知する専用チャネルとして分離する。既存の `S3FS_IOC_DRAIN_EVICTIONS` ioctl が両バッファを drain できるよう `s3fs_ebpf_drain_evictions` を拡張する。

**Step 3 — daemon: timer path のドレイン調整** (0.5 日)

`drainEvictionsLoop` のポーリング間隔を 2s から 500ms に短縮するのではなく、ringbuf reader を epoll 経由で通知ベースに変更する。タイマーが emit したタイミングで即 wake する。

**Step 4 — テスト** (1 日)

`scripts/bench-timer-evict.sh`: arena を softcap 直上まで warm してから daemon の polling loop を止め、timer が自律で evict を発行することを dmesg で確認する。drainEvictionsLoop を停止した状態でも `s3fs_ebpf_policy_evictions_total` が増加し続けることを smoke で確認する。

**推定作業量**: 3〜4 日

---

### 3-3. KFunc + in-kernel radix tree による `readdir` 高速化

#### 目的

現在 `iterate_shared` は `s3fs_entry_dyn` の線形スキャン。エントリ数が増えると O(n) になる。S3 メタデータを kmod 内の radix tree で管理して KFunc で走査することで `readdir` のオーバーヘッドを O(log n) に下げ、かつ daemon 停止中でもディレクトリ一覧が機能するようにする。eBPF ベリファイアが証明できない unbounded loop を KFunc に移譲するパターン (`kmod/tree_traversal.c` の思想) の直接的な応用例。

#### 実装ステップ

**Step 1 — kmod: radix tree の構築** (2 日)

`kmod/vfs_ops.c` の `s3fs_ebpf_entries_replace` が呼ばれたとき、既存の線形配列と並行して `radix_tree_root` にも挿入する。key は `(parent_ino << 32) | hash(name)` でユニーク化し、`radix_tree_lookup` / `radix_tree_insert` / `radix_tree_delete` を使う。`iterate_shared` をリニアスキャンから `radix_tree_for_each_slot` に置き換える。

**Step 2 — KFunc: ディレクトリ走査の expose** (1 日)

`kmod/tree_traversal.c` に以下の KFunc を追加する。これにより BPF プログラムが検索・一覧取得を行えるようになる。

```c
// kmod/tree_traversal.c (追加)
__bpf_kfunc int s3fs_kfunc_dir_walk(u64 parent_ino,
                                     struct s3fs_dir_entry *out,
                                     u32 max_entries);
__bpf_kfunc int s3fs_kfunc_lookup_entry(u64 parent_ino, const char *name,
                                         u32 name_len,
                                         struct s3fs_dir_entry *out);
```

`s3fs_dir_entry` は `ino`, `name[S3FS_SYNC_NAME_MAX+1]`, `mode`, `size` のみのスリムな struct として BTF 登録する。

**Step 3 — BPF: KFunc 呼び出し側の実装** (1 日)

既存の `fentry/vfs_getattr_nosec` アタッチと並行して `fentry/s3fs_ebpf_iterate_shared` をアタッチし、KFunc 経由の高速 readdir パスを試す BPF プログラム `bpf/fast_readdir.bpf.c` を追加する。

**Step 4 — ベンチマーク** (1 日)

`scripts/bench-readdir.sh`: エントリ数を 10 / 100 / 1000 / 10000 と変えて `ls -la /mnt/s3demo/ | wc -l` の実行時間を計測し、線形スキャンと radix tree の latency カーブを比較する。

**推定作業量**: 4〜5 日

---

### 3-4. BPF Iterator による arena GC

#### 目的

現在 arena の解放は daemon goroutine (`arenaFree` スライス + `coalesceArenaFree`) が管理している。daemon 停止中は arena が解放されず、kmod が古い pin 済みページを保持したままになる。`BPF_PROG_TYPE_ITER` を使えば「arena のエントリを一定数ずつスキャンして refcount ゼロのチャンクを回収する」GC を BPF 側で動かすことができる。

#### 実装ステップ

**Step 1 — kmod: arena chunk の refcount 追加** (1.5 日)

`kmod/vfs_ops.c` の admit table entry に `atomic_t refcount` を追加する。`admit_replace` で refcount = 1、eviction で atomic_dec_and_test、ゼロになったチャンクを GC キューに投入する。

**Step 2 — BPF: iterator プログラムの実装** (2 日)

`bpf/arena_gc.bpf.c` を `SEC("iter/bpf_map_elem")` で作成する。admit hash の各エントリをイテレートし、refcount == 0 のものを `bpf_map_delete_elem` で削除するとともに解放サイズを統計 map に加算する。`bpf_iter_create` → `read(iter_fd)` のパターンで daemon から起動できるようにする。

```c
// bpf/arena_gc.bpf.c (新規)
SEC("iter/bpf_map_elem")
int arena_gc_iter(struct bpf_iter__bpf_map_elem *ctx) {
    // refcount == 0 のエントリを scan して解放
}
```

**Step 3 — daemon: GC トリガーの追加** (1 日)

`daemon/cmd/s3fs-ebpf/flush_loop.go` に `runArenaGCLoop` を追加し、30s ごとに `bpf_iter_create` 経由で GC プログラムを起動する。結果 (`freed_bytes`, `freed_chunks`) を `/metrics` に追加する。

**Step 4 — テスト** (1 日)

`daemon/internal/ebpf/loader_test.go` に iterator round-trip テストを追加する。10 エントリ admit → 5 evict → arena GC 起動 → freed_chunks == 5 を確認する単体テスト。

**推定作業量**: 4〜5 日

---

## 4. 実装順序と依存関係

```
現在実装済み
  ├── Path A (fentry ゼロ csw)        ✅
  ├── Path B (GUP ゼロコピー)          ✅
  └── struct_ops ホットスワップ        ✅

未実装 (推奨実装順)
  Step 1: bpf_timer 自律 eviction [§3-2]   ← daemon 依存を下げる基盤
    依存: なし (KFunc は既存)
    所要: 3〜4 日

  Step 2: BPF Iterator arena GC [§3-4]     ← GC を daemon 外に逃がす
    依存: Step 1 (refcount 導入を共有)
    所要: 4〜5 日

  Step 3: KFunc + radix tree readdir [§3-3] ← 大規模バケット対応
    依存: なし (kmod 独立)
    所要: 4〜5 日

  Step 4: fmod_ret + cgroup policy [§3-1]  ← 実用化の最終形
    依存: Step 1-3 が安定後に着手推奨
    所要: 4〜5 日
```

**合計推定**: Step 1〜4 で 15〜19 日 (並走可能部分あり)

---

## 5. 各 Step と backlog ID の対応

未実装の 4 項目は `docs/journal/backlog.md` の既存 ID 体系に従い以下 ID で追記する。

| backlog ID | 対応 Step | タイトル |
|---|---|---|
| Q-01 | §3-2 | bpf_timer 自律 eviction |
| Q-02 | §3-4 | BPF Iterator arena GC |
| Q-03 | §3-3 | KFunc + radix tree readdir |
| Q-04 | §3-1 | fmod_ret + cgroup_id() per-cgroup ポリシー |

---

## 6. 参照

- `docs/design/policy-engine-design.md` — Layer 1-3 policy architecture
- `docs/design/policy-attach-comparison.md` — fmod_ret vs struct_ops の選択基準
- `docs/dev-notes/ebpf-ecosystem-gaps.md` — arena kern_vm / KF_SLEEPABLE の既知制限
- `docs/design/read-path-design.md` — Path A/B/C の設計比較
- `kmod/policy_struct_ops.h` — vtable 定義
- `kmod/tree_traversal.c` — KFunc expose の現行実装 (§3-3 の参考)
- `docs/journal/backlog.md` — 未完了 follow-up の一元リスト (完了した古い項目は `docs/journal/backlog-archive.md`)
