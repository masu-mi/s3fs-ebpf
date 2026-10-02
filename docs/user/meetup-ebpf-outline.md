# eBPFミートアップ発表アウトライン (20min)

2026-10-02

## 発表テーマと方針

- **ターゲット**: eBPFミートアップ参加者（BPF全般に興味あり、S3FSの詳細は不要）
- **中心テーマ**: KFuncとstruct_opsを使ってBPFの制約を越える実装パターン
- **FSの話は**: 「KFuncが必要になった動機」として1スライドで紹介に留める
- **性能評価**: インパクトがあるのでさらっと1スライド紹介（3行表）
- **枠**: 20min

---

## ネタ候補と採否

### 採用するネタ

| # | ネタ | 説明 | 時間目安 |
|---|---|---|---|
| A | 動機とシステム概要 | 「S3をFSとして使いたい」 + kmod/BPF/daemonの3層を1スライド | 2min |
| B | BPFの限界 — verifierの壁 | arena linked-list walk (`s3fs_deep_walk_reject`) が拒否される実例で「eBPFの限界」を示す | 3min |
| C | KFunc実装 3ステップ | `__bpf_kfunc` / `BTF_KFUNCS_START` / `register_btf_kfunc_id_set`。prog typeごとに登録が必要な点も | 3min |
| D | struct_ops — ポリシーをホットスワップ | vtable `s3fs_policy_ops` を1回のlink.Updateでアトミックに入れ替え。fmod_retとの比較表 | 3min |
| E | Layer 0 KFuncs — BPFからkernel状態を読む | `admit_count`/`softcap`/`time_ns`/`ino_size` を公開しpolicy BPFが利用 (`policy_pressure_ops`, `policy_time_decay_ops`) | 3min |
| F | 性能数字 | read latency 875×、write 17×、cache hit率 88.5% vs 7.5%の3行表 | 2min |
| G | まとめ/takeaway | "KFuncはBPFのverifier制限を安全に抜ける窓口" 1枚 | 2min |

### 省くネタ

| ネタ | 理由 |
|---|---|
| arenaの詳細 (BPF_MAP_TYPE_ARENA, GUP) | FSの読み込みパスの話になりFS寄りすぎる |
| CEL/Lua policy (Layer 1) | eBPFではない |
| VFS/kmodの実装詳細 | 趣旨からズレる |
| fmod_retの詳細実装 | struct_opsとの比較表1枚で十分 |
| W-TinyLFU/S3-FIFOのアルゴリズム詳細 | eBPFの話でなくキャッシュアルゴリズムの話 |

---

## アウトライン

### 1. Intro (2min)

- 「S3をローカルFSとして使いたい」
- 構成: kmod (VFS) + BPF fast path + Go daemon
- 今日の話: このシステムで使ったeBPF機能3つ

### 2. BPFの限界 — verifierの壁 (3min)

- arena linked-list walkのコードを見せる (`bpf/s3fs_fastpath.bpf.c` の `s3fs_deep_walk_reject`)
- verifier拒否のエラーメッセージ ("unbounded pointer chase / insn budget 1M")
- → これを安全に越える手段がKFunc

### 3. KFunc実装 3ステップ (3min)

- `__bpf_kfunc` アノテーション
- `BTF_KFUNCS_START` / `BTF_ID_FLAGS`
- `register_btf_kfunc_id_set`（prog typeごと: TRACING/SYSCALL/STRUCT_OPS）
- コード例: `s3fs_kfunc_echo`（シンプルなラウンドトリップ検証）

### 4. struct_ops — ポリシーをvtableで差し替える (3min)

- `struct s3fs_policy_ops { on_admit, on_evict, on_pressure, on_hit }`
- BPF側: `SEC(".struct_ops.link")` で実装
- kmod側: RCUで1ポインタスワップ (`kmod/policy_struct_ops.c` の reg/unreg/update)
- fmod_ret比較: N回link.Update vs 1回でアトミック

### 5. Layer 0 KFuncs — BPFからkmod状態を読む (3min)

- KFuncが公開するもの: `admit_count` / `softcap` / `time_ns` / `ino_size`
- `policy_pressure_ops`: admit_count/softcapでSIEVEのstrictnessを動的切替
- `policy_time_decay_ops`: time_nsでtime-based eviction
- 「BPFから外部状態をread-onlyで安全に参照できる窓口」

### 6. 性能 (2min)

| 指標 | s3fs-ebpf | s3fs-fuse | 差 |
|---|---|---|---|
| read latency (p50) | 5.20 μs | 4.55 ms | **875×** |
| write throughput (100 MiB) | 88 ms | 1516 ms | **17×** |
| cache hit率 (W-TinyLFU vs LRU) | 88.5% | 7.5% | **12×** |

### 7. まとめ (2min)

- KFunc = verifier安全性を保ちながらkernelロジックをBPFに公開する窓口
- struct_ops = 複数hookを1vtableで管理 + アトミックスワップ
- Layer 0 KFunc = BPF policyがkmod状態を参照する境界面
- 「eBPFは書けないこともKFuncに逃がせば書ける」

---

## スライド補足メモ

- **最大のインパクトは「2→3」の流れ**: verifierに弾かれるコードを先に見せ、KFuncで解決する展開が聴衆のヒックになる
- **セクション5のdemo候補**: curl ワンライナー (`POST /policy/reload`) でポリシーをライブスワップするデモが可能。hit率変化を直後に `/metrics` で確認できる
- **性能表の注意事項**: read 875× は s3fs-fuse のキャッシュ無効構成との比較。fuse+kernel_cache 有効でも ebpf は依然 2〜2.5× 速い（質問が来た時用）
- **コード引用元**:
  - `bpf/s3fs_fastpath.bpf.c` (`s3fs_deep_walk_reject`)
  - `kmod/tree_traversal.c` (`BTF_KFUNCS_START`)
  - `kmod/policy_struct_ops.c` (reg/unreg/update)
  - `bpf/policy_pressure_ops.bpf.c` (Layer 0 KFunc利用例)
