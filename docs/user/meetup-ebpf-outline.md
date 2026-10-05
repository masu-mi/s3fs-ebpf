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
| C | KFunc実装 3ステップ | `__bpf_kfunc` / `BTF_KFUNCS_START` / `register_btf_kfunc_id_set`。prog typeごとに登録が必要な点も。Bと同じ walk を KFunc `s3fs_kfunc_arena_list_walk` で通す | 3min |
| D | struct_ops — ポリシーをホットスワップ | vtable `s3fs_policy_ops` を1回の `BPF_LINK_UPDATE` でアトミックに入れ替え。fmod_retとの比較表 | 3min |
| E | Layer 0 KFuncs — BPFからkernel状態を読む | `admit_count`/`softcap`/`evictions_total`/`time_ns`/`ino_size` を公開しpolicy BPFが利用 (`policy_pressure_ops`, `policy_time_decay_ops`, `policy_size_aware_ops`) | 3min |
| F | 性能数字 | open+read+close ~870×、close までの write latency 17× (durable 比較ではない)。hit率は出さない | 2min |
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
- verifier拒否の実際のメッセージ: `BPF program is too large. Processed 1000001 insn` / `processed 1000001 insns (limit 1000000) ... total_states 9664`
  - 原因: `n = n->next` の load 結果が `scalar()` (範囲不明) になり、verifier はループを抜ける条件を証明できない → 状態を展開し続けて 1M insn 上限 (`BPF_COMPLEXITY_LIMIT_INSNS`) に到達
  - 再現: `sudo ./bin/s3fs-ebpf dev verifier-demo` (6.12 / 6.18 で確認)
- → これを安全に越える手段がKFunc

### 3. KFunc実装 3ステップ (3min)

- `__bpf_kfunc` アノテーション (`__bpf_kfunc_start_defs()` で囲む)
- `BTF_KFUNCS_START` / `BTF_ID_FLAGS`（6.11+ は `BTF_SET8_START` だと `-EINVAL`。flag 例: `KF_SLEEPABLE`）
- `register_btf_kfunc_id_set`（prog typeごと: TRACING/SYSCALL/STRUCT_OPS）。fmod_ret 用の hook は別 API `register_btf_fmodret_id_set`
- コード例: **`s3fs_kfunc_arena_list_walk`** (`kmod/tree_traversal.c`) — §2 で拒否されたのと同じ walk
  - BPF 側 (`bpf/kfunc_walk.bpf.c`) は KFunc 呼出 1 回の直線コード → verifier は即通す
  - ループは kmod の C: 安全性は `copy_from_user` (壊れた `next` は -EFAULT)、停止性は `max_nodes` (循環は -ELOOP) + `fatal_signal_pending`/`cond_resched`
  - `copy_from_user` は sleep しうるので `KF_SLEEPABLE` + SYSCALL prog (`SEC("syscall")` は sleepable) に登録
  - `verifier-demo` の後半がこれを実行する (100k ノード、sum 一致を検証)
- 補足用: `s3fs_kfunc_echo` (2v+1) は最小のラウンドトリップ検証。`s3fs_kfunc_traverse_stub` は 0 を返すだけの stub なので「解決策」として見せない

### 4. struct_ops — ポリシーをvtableで差し替える (3min)

- `struct s3fs_policy_ops { on_admit, on_evict, on_pressure, on_hit }`
- BPF側: vtable を `SEC(".struct_ops.link")` で定義 (`BPF_F_LINK`)。各 method は `SEC("struct_ops/...")`。kmod が `rcu_read_lock()` 内から呼ぶので sleepable (`struct_ops.s/`) にはしない
- kmod側: RCUで1ポインタスワップ (`kmod/policy_struct_ops.c` の reg/unreg/update)
- hot swap: daemon は既存 link に `BPF_LINK_UPDATE` (new_map_fd + `BPF_F_REPLACE`) を 1 回 → kernel が `.update` を呼ぶ (`policyprog.SwapFrom`)。cilium/ebpf v0.22 の struct_ops link `Update` は未対応なので raw syscall で実装
- fmod_ret比較: tracing link は `BPF_LINK_UPDATE` 非対応 → 「新 N 本 attach → 旧 N 本 close」で、その間は新旧が同じ hook で両方走る / struct_ops は 1 syscall でアトミック

### 5. Layer 0 KFuncs — BPFからkmod状態を読む (3min)

- KFuncが公開するもの: `admit_count` / `softcap` / `evictions_total` / `time_ns` / `ino_size` (TRACING と STRUCT_OPS の両方に登録)
- `policy_pressure_ops`: admit_count/softcapでSIEVEのstrictnessを動的切替 (ratio ≥ 1.5 で grace を飛ばす)
- `policy_time_decay_ops`: time_nsでtime-based eviction (最後の on_hit から 5s 超で evict)
- `policy_size_aware_ops`: ino_size で小さいファイルを優遇
- 「BPFから外部状態をread-onlyで安全に参照できる窓口」— ただし `ino_size` は admit hash の spinlock を取るので、lock-free なのは他の 4 つ

### 6. 性能 (2min)

| 指標 | s3fs-ebpf | s3fs-fuse (cache 無効) | 差 |
|---|---|---|---|
| open+read+close, 16 B, warm (microbench p50) | 5.20 μs | ~4.5 ms | **~870×** |
| 100 MiB overwrite, `dd` 完了まで (close まで) | 88 ms ± 32 | 1516 ms ± 315 | **17×** ※ |

- ※ s3fs-fuse は close() 内で S3 PUT まで同期実行、ebpf の close は flush を enqueue して即 return (PUT は非同期)。durable 同士の比較ではない → スライドに注記するか、`ebpf-fsync` 行 (`scripts/bench-write.sh` に追加済、未計測) の数字に差し替える
- 前の版の「875×」は ebpf の今回 run と fuse の前回 run (4.552 ms) の比。同一 run の p50 同士だと ~865×
- ~870× は「arena + page cache 有り ebpf」対「キャッシュ無し fuse」の比で、caching の差が大半を占める。構造の差は対照群 `ebpf-nocache` (kmod `nocache=1` + daemon `--cache-dir` 無し = 毎 open で upcall + S3 GET。FUSE kernel_cache 無しと同じ形) との比較で示す → `scripts/bench-3way.sh`、要計測。スライドに出すなら「fuse-raw vs ebpf-nocache」(構造差) と「fuse-cache vs ebpf-A+C」(ベストケース同士) の 2 行が公平
- 前の版にあった「cache hit率 W-TinyLFU 88.5% vs LRU 7.5% (12×)」は削除:
  1. 両方 s3fs-ebpf の policy 同士で、s3fs-fuse との比較ではない (表の列と矛盾)
  2. capacity 8 の Belady 上限は同じ trace で 61% (最良の warm start でも 65%)。88.5% は 8 slot の hit 率として成立しない。softcap は eviction の trigger で容量ではない。順位はほぼ eviction 回数の逆順 (`docs/journal/hit-rate-bench.md` Validity review)
  3. 最新 run では LRU は 13.0% (7.5% は古い run)
  4. 計測後に kmod の bug (evict 後も page cache から hit 扱い) を修正したので再計測が必要

### 7. まとめ (2min)

- KFunc = verifier安全性を保ちながらkernelロジックをBPFに公開する窓口
- struct_ops = 複数hookを1vtableで管理 + アトミックスワップ
- Layer 0 KFunc = BPF policyがkmod状態を参照する境界面
- 「eBPFは書けないこともKFuncに逃がせば書ける」

---

## スライド補足メモ

- **最大のインパクトは「2→3」の流れ**: verifierに弾かれるコードを先に見せ、KFuncで解決する展開が聴衆のフックになる。`verifier-demo` 1 コマンドで「拒否 → 同じ walk を KFunc で 100k ノード走査・sum 一致」まで見せられる
- **セクション4/5のdemo候補**: curl ワンライナー (`POST /policy/reload -d builtin:policy_sieve_ops`) でポリシーをライブスワップ。レスポンスと dmesg (`policy struct_ops updated (hot swap)`) で `BPF_LINK_UPDATE` 経由を示せる。`/metrics` には hit 率そのものは無いので、`s3fs_ebpf_read_miss_upcalls_total` / `s3fs_ebpf_folio_hits_total` / `s3fs_ebpf_policy_evictions_total` / `s3fs_ebpf_admits_current` の変化を見せる
- **性能表の注意事項**: ~870× は s3fs-fuse のキャッシュ無効構成 (毎回 MinIO へ HTTP GET) との比較。fuse `use_cache + kernel_cache` 有効時は、cat (wall clock) で 2.05〜2.45×、open+read+close の microbench で 245× (差の大半は open ごとの FUSE upcall)。質問が来たら指標を区別して答える
- **コード引用元**:
  - `bpf/s3fs_fastpath.bpf.c` (`s3fs_deep_walk_reject`)
  - `kmod/tree_traversal.c` (`BTF_KFUNCS_START`, `s3fs_kfunc_arena_list_walk`)
  - `bpf/kfunc_walk.bpf.c` (KFunc 呼出側)
  - `daemon/internal/policyprog/loader.go` (`SwapFrom` / `BPF_LINK_UPDATE`)
  - `kmod/policy_struct_ops.c` (reg/unreg/update)
  - `bpf/policy_pressure_ops.bpf.c` (Layer 0 KFunc利用例)

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
- 再計測が必要: hit-rate 全行 (page cache 修正後)、write の `ebpf-fsync`。いずれも単一 run・n 小なので、複数 run の中央値とばらつきも併記したい
