# s3fs-ebpf 採用 eBPF 機能ガイド — 用途・有用性・制限・落とし穴

> 作成: 2026-10-11  
> 目的: meetup 発表準備および将来の実装者向けに、本プロジェクトで採用した eBPF 機能の一覧とその価値・落とし穴を体系的に整理する。  
> 関連: `docs/dev-notes/ebpf-ecosystem-gaps.md`（upstream 改善提案の 1 次素材）、`docs/design/policy-attach-comparison.md`（fmod_ret vs struct_ops の実測比較）

---

## 1. 採用機能一覧

| カテゴリ | 機能 | 採用箇所 | 最低 kernel |
|---|---|---|---|
| Program 型 | `fentry` (BPF_PROG_TYPE_TRACING) | `fentry/vfs_getattr_nosec`、`fentry/generic_file_read_iter` | 5.5 (BTF 要) |
| Program 型 | `SEC("syscall")` (BPF_PROG_TEST_RUN) | arena プレアロケーション、verifier デモ | 5.5 |
| Program 型 | `struct_ops` (BPF_PROG_TYPE_STRUCT_OPS) | キャッシュポリシー vtable (LRU/SIEVE/S3-FIFO/W-TinyLFU/pressure/size-aware) | 5.6 (外部 kmod 登録は実質 6.4+) |
| Program 型 | `fmod_ret` (BPF_MODIFY_RETURN) | ポリシーフック戻り値オーバーライド（struct_ops の代替実装） | 5.7 |
| Map 型 | `BPF_MAP_TYPE_ARENA` | daemon↔BPF↔kmod 共有メモリ（ファイル内容） | **6.9** |
| Map 型 | `BPF_MAP_TYPE_RINGBUF` | スローパスミスイベント通知 | 5.8 |
| Map 型 | `BPF_MAP_TYPE_PERCPU_ARRAY` | ヒット/ミスカウンタ | 4.6 |
| Map 型 | `BPF_MAP_TYPE_LRU_HASH` | inode アクセスパターン記録 | 4.10 |
| Map 型 | `BPF_MAP_TYPE_HASH` | ino→arena オフセット索引、ポリシー状態 | 3.19 |
| 機能 | KFunc (`__ksym`) | unbounded 操作の kmod エスケープ、カーネル状態公開 | 5.18 (外部 kmod 登録) |
| 機能 | CO-RE (`bpf_core_read`) | kernel struct フィールド移植性 | 5.2 |
| 機能 | `bpf_loop()` | W-TinyLFU の CMS aging（verifier 複雑度制限の回避） | 5.17 |
| 機能 | `__arena` / `bpf_addr_space_cast` | arena ポインタデリファレンス | clang 19 + kernel 6.9 |
| Toolchain | bpf2go (cilium/ebpf) | Go バインディング自動生成 | — |

---

## 2. 各機能の詳細

### 2-1. `fentry` — VFS フック観測

**用途・有用性**

`SEC("fentry/vfs_getattr_nosec")` と `SEC("fentry/generic_file_read_iter")` でカーネルの stat/read パスを観測する。プログラムは BPF map を参照するだけなので、ヒット時はカーネルからユーザー空間へのコンテキストスイッチがゼロ。FUSE 最良ケースでも必須な 2 往復を完全に排除できる。

**FS・キャッシュでの価値**

- `fentry/vfs_getattr_nosec`: `s3fs_inode_index` map を引いてヒット/ミスを判定し、ヒットならカウンタ bump のみで完了。FUSE の `/dev/fuse` 往復が消える
- `fentry/generic_file_read_iter`: ページキャッシュヒット（`.read_folio` 非経由）でも on_hit を呼べる唯一のフック位置。アクセスパターン（`s3fs_read_pattern_map`）の更新もここで行う

**制限・落とし穴**

- フック名の調査コストが高い。`vfs_getattr` ではなく `vfs_getattr_nosec` が正解（v6.12 の `vfs_getattr` は `fs/stat.c` の中で `vfs_statx_path` に inline され、stat では関数として呼ばれない。`ftrace` での実地確認が必須。`docs/journal/week2-implementation-notes.md` §5）
- BTF が必要。ディストリビューション kernel に `/sys/kernel/btf/vmlinux` がない環境では動かない（古い RHEL 系等）
- 戻り値を変えられない。ヒット時に「内容を BPF から直接返す」は原理的に不可能で kmod 側が必要になる

> **TODO(確認)**: `vfs_statx()` → `vfs_getattr_nosec()` の呼び出しチェーンが kernel 更新後も成立しているか確認。`vfs_getattr()` が呼ばれるパス（`vfs_fstat` 等）との分岐点を把握しておくこと。  
> 確認箇所: https://elixir.bootlin.com/linux/v6.12/source/fs/stat.c

**設計注記: stat ヒットをポリシーシグナル（`s3fs_kfunc_policy_hit`）に含めない理由**

`fentry/vfs_getattr_nosec` はヒット検出時にカウンタ bump のみ行い、`s3fs_kfunc_policy_hit` は呼ばない。`fentry/generic_file_read_iter` だけが on_hit をトリガーする。この非対称を解消して stat もシグナルに加えた場合のデメリットは以下の通り:

1. **stat はコンテンツアクセスではない** — `ls -la` や `find`、`git status` など metadata 参照が多いワークロードでは大量の stat が発生し、実際には read されていない inode の recency/frequency スコアが上がる。LRU/SIEVE/W-TinyLFU のスコアが汚染され、本当に hot なファイルが押し出されるリスクがある。

2. **stat ホットパスへの KFunc 追加コスト** — `vfs_getattr_nosec` は全 stat(2) syscall をフックする最高頻度のフックポイント。ここに KFunc 呼び出し（RCU read lock + vtable dispatch）を追加すると stat レイテンシが増加する。per-CPU カウンタを採用している理由と同じ「stat は呼び出し頻度が高い」という前提と相反する。

3. **frequency-counter ポリシーの混乱** — W-TinyLFU や freq2bit は on_hit で CMS/2-bit カウンタを bump する。stat による bump が重なると「stat は多いが read されないファイル」がキャッシュを占有し、on_evict の票が read されているファイルへ向かうリスクがある。

4. **on_hit セマンティクスの膨張** — 現在の on_hit は「arena キャッシュへの読み取りアクセスがあった」という一貫したセマンティクスを持つ。stat まで含めると「何らかの VFS 操作でヒットが確認された」という曖昧な意味になり、将来のポリシー設計者が挙動を予測しにくくなる。

---

### 2-2. `BPF_MAP_TYPE_ARENA` — 共有メモリ（最難所）

**用途・有用性**

daemon (Go)・eBPF プログラム・kmod の 3 者が **同一物理ページ** をそれぞれの仮想アドレスで参照できる。daemon は `mmap` 経由でファイル内容を書き込み、kmod は `pin_user_pages_remote` + `kmap_local_page` で同じページを page cache にマッピングする。`bpf_map_lookup_elem` すら不要で、ポインタのデリファレンス（`*ptr`）だけでデータを読める真のゼロコピー。

**FS・キャッシュでの価値**

S3 から取得したファイル内容を daemon が arena に書き込むと、kmod の `.read_folio` がコピーなしで page cache に繋げられる。FUSE の `fuse_copy_to_iter()` コピーが完全に消える。大きなオブジェクト（ML モデル、Parquet 等）で差が支配的になる（ベンチ結果: `docs/journal/benchmark-results.md`）。

**制限・落とし穴**

| ID | 事象 | 影響 | 対処 |
|---|---|---|---|
| A1 | **kern_vm に generic fault handler がない** | kmod から kern_vm ベースを直接 deref → 全 EFAULT | GUP over user_vm（Path B）に迂回。数百 ns のオーバーヘッドが残る。kernel patch（K-02）で解決可能 |
| A2 | **`struct bpf_arena` が非公開型** | kmod が `container_of` で kern_vm base を取れない | kmod 内で BTF 逆算による layout 再定義（`kmod/s3fs_ebpf_dev.c:45`）。kernel 6.12 に固く縛られる |
| A3 | **`bpf_arena_get_kern_vm_start()` が非 GPL export** | kmod から直接呼べない | A2 の container_of hack が必須 |
| A4 | **`BPF_MAP_TYPE_ARENA = 33` が vmlinux.h に出ない** | pahole が enum を 32 で打ち切る | BPF ソースに `#define BPF_MAP_TYPE_ARENA 33` をハードコード（`bpf/s3fs_fastpath.bpf.c` 冒頭） |
| A5 | **arena map の `key_size=0` で `bpf_map_lookup_elem` が reject** | 「invalid zero-sized read」エラー | `bpf_arena_alloc_pages` か inline-asm ピンで arena 型を wiring |
| A6 | **addr_space_cast が prog→aux→arena wiring なしに reject** | 「BPF program is too large」または即 reject | arena map を参照する LD_MAP_FD 命令を emit させる `s3fs_arena_pin()` inline-asm trick |
| A7 | **最小 kernel 6.9**（実質安定は 6.12 LTS） | 多くのディストリビューション kernel で使えない | Ubuntu 24.10 / Fedora 40+ / Arch 等の最新 distro のみ |

> **TODO(確認 A1)**: `arena_vm_fault` が user_vm VMA のみを対象とし、kern_vm range には fault handler が登録されていないことを確認。  
> 確認箇所: https://elixir.bootlin.com/linux/v6.12/source/kernel/bpf/arena.c (`arena_vm_ops` / `arena_vm_fault` 付近)

> **TODO(確認 A2/A3)**: `struct bpf_arena` が `kernel/bpf/arena.c` 内部にのみ定義され公開ヘッダに存在しないこと、および `bpf_arena_get_kern_vm_start()` の export 有無（`EXPORT_SYMBOL` / `EXPORT_SYMBOL_GPL` / 未 export）を確認。  
> 確認箇所: https://elixir.bootlin.com/linux/v6.12/source/kernel/bpf/arena.c

> **TODO(確認 A4)**: `include/uapi/linux/bpf.h` の `enum bpf_map_type` で `BPF_MAP_TYPE_ARENA` の値（33）と位置を確認。kernel バージョンアップ時に値が変わっていないかも合わせて確認すること。  
> 確認箇所: https://elixir.bootlin.com/linux/v6.12/source/include/uapi/linux/bpf.h (`BPF_MAP_TYPE_ARENA` で検索)

> **TODO(確認 A5)**: verifier の「invalid zero-sized read」が key_size=0 の arena map への `bpf_map_lookup_elem` に対して発生することを確認。  
> 確認箇所: https://elixir.bootlin.com/linux/v6.12/source/kernel/bpf/verifier.c (`invalid zero-sized read` で検索)

> **TODO(確認 A6)**: `resolve_pseudo_ldimm64` で LD_MAP_FD 命令が arena 型 map を参照した際に `prog->aux->arena` がセットされること、および `check_alu_op` で `prog->aux->arena` が未設定なら addr_space_cast を reject するチェックを確認。  
> 確認箇所: https://elixir.bootlin.com/linux/v6.12/source/kernel/bpf/verifier.c (`resolve_pseudo_ldimm64` / `prog->aux->arena` で検索)

---

### 2-3. `BPF_MAP_TYPE_RINGBUF` — イベント通知

**用途・有用性**

eBPF プログラムがミスイベント（`s3fs_miss_event`）を `bpf_ringbuf_reserve → bpf_ringbuf_submit` で積み、Go daemon が非同期に消費する。ロックフリー・順序保証あり・可変長レコード対応。`perf_event_array` 比で CPU オーバーヘッドが低い。

**FS・キャッシュでの価値**

`fentry/vfs_getattr_nosec` と `fentry/generic_file_read_iter` の両方から miss イベントを 1 本のバッファに流す。daemon はリングバッファをポーリングして S3 フェッチをトリガーする非同期プリウォーム経路が完結する。

**制限・落とし穴**

- 容量はべき乗サイズ必須。小さすぎると `bpf_ringbuf_reserve` が NULL を返してイベントがドロップされる（`S3FS_STAT_SLOW_MISSES` カウンタで観測可能）
- kernel 5.8+。それ以前は `perf_event_array` で代替するが API が変わる

> **TODO(確認)**: `BPF_MAP_TYPE_RINGBUF` が 5.8 で追加されたこと、および容量のべき乗制約が実装レベルで課されていることを確認。  
> 確認箇所: https://elixir.bootlin.com/linux/v6.12/source/kernel/bpf/ringbuf.c (`ringbuf_map_alloc` 付近)

---

### 2-4. `BPF_MAP_TYPE_PERCPU_ARRAY` — ロックフリーカウンタ

**用途・有用性**

`s3fs_counters` マップで hit/miss/unknown_fs を CPU ごとに独立カウント。`__sync_fetch_and_add` でアトミックに bump してもキャッシュラインの競合がない。

**FS・キャッシュでの価値**

`fentry/vfs_getattr_nosec` は全 stat(2) システムコールにフックするので呼び出し頻度が高い。per-CPU 設計がないとカウンタがボトルネックになる。

**落とし穴**

Go daemon 側で読む際は全 CPU の値を合算する必要がある（cilium/ebpf の `LookupAll` が各 CPU スロットを返す）。

---

### 2-5. `BPF_MAP_TYPE_LRU_HASH` — アクセスパターン記録

**用途・有用性**

`s3fs_read_pattern_map` で inode ごとのシーケンシャル/ランダム読み取り比を追跡。`max_entries` 超過時にカーネルが自動で LRU 退場させるため GC 不要。

**FS・キャッシュでの価値**

シーケンシャルアクセスが多い inode をプリフェッチポリシーに伝える信号源。ページキャッシュヒット（`.read_folio` 非経由）の場合でも fentry で更新できる。

**落とし穴**

LRU 退場は per-bucket approximate LRU（bucket 内最古を近似的に選ぶ）。全 bucket 横断の厳密な LRU ではない。

> **TODO(確認)**: `BPF_MAP_TYPE_LRU_HASH` の退場ロジックが per-bucket approximate LRU であることを `hashtab.c` の実装で確認。`htab_lru_map_update_batch` または `__htab_lru_map_update_elem` 付近を参照。  
> 確認箇所: https://elixir.bootlin.com/linux/v6.12/source/kernel/bpf/hashtab.c

---

### 2-6. `struct_ops` — ポリシー vtable のホットスワップ

**用途・有用性**

`struct s3fs_policy_ops { on_admit / on_evict / on_pressure / on_hit / on_insert / on_remove / pick_victim }` を BPF プログラムとして実装し、kmod が RCU 経由でディスパッチする。`BPF_LINK_UPDATE` 1 回で vtable を **原子的に** 差し替えられる（`rcu_assign_pointer + synchronize_rcu()`）。

実装済みポリシー:

| ファイル | アルゴリズム | 特徴 |
|---|---|---|
| `policy_lru_ops.bpf.c` | Approximate LRU (epoch-based) | on_hit で seed リフレッシュ |
| `policy_sieve_ops.bpf.c` | SIEVE の近似 (visited-bit のみ、FIFO と hand なし) | 旧版。本来の SIEVE は `policy_sieve_pick_ops` |
| `policy_s3fifo_ops.bpf.c` | S3-FIFO (3-tier FIFO) | small/main/ghost の 3 キュー |
| `policy_freq2bit_ops.bpf.c` | 2-bit 頻度カウンタ | fmod_ret 版と共通 algorithm |
| `policy_wtinylfu_ops.bpf.c` | W-TinyLFU (CMS + aging) | 4×256 CMS sketch、bpf_loop で aging |
| `policy_pressure_ops.bpf.c` | SIEVE + pressure-aware | KFunc で admit_count/softcap を読む |
| `policy_size_aware_ops.bpf.c` | size-aware eviction | KFunc で ino_size を読む |
| `policy_time_decay_ops.bpf.c` | 時刻ベース decay LRU | KFunc `s3fs_kfunc_time_ns` で最終ヒット時刻管理。KFunc 呼び出しデモが主目的 |
| `policy_wtinylfu_full_ops.bpf.c` | W-TinyLFU 完全版（Window LRU + Doorkeeper + CMS） | 論文忠実実装。ヒット率はスコープ版（`policy_wtinylfu_ops`）より劣る（`hit-rate-bench.md` 参照） |
| `policy_sieve_pick_ops.bpf.c` | SIEVE (FIFO + hand) | AC-58 の `pick_victim` で victim を BPF が選ぶ |
| `policy_lru_pick_ops.bpf.c` | 厳密な LRU (stamp の最小) | 同上。旧 `policy_lru_ops` は epoch 近似 |
| `policy_s3fifo_pick_ops.bpf.c` | S3-FIFO (S / M / G の 3 queue) | 同上。capacity mode の hit rate で最も高い (`benchmark-results.md` §18.42) |
| `policy_wtinylfu_pick_ops.bpf.c` | W-TinyLFU (window + SLRU + Count-Min sketch + admission) | 同上。AC-66 で足した `on_size` で予算をバイトで持つ (`docs/design/wtinylfu-pick-plan.md`) |

`on_insert` / `on_remove` / `pick_victim` は AC-58 で vtable の末尾に足した (`docs/design/ac58-pick-victim.md`)。持たない policy は従来どおり `on_evict` の score 順で evict される。

**FS・キャッシュでの価値**

"キャッシュポリシー = vtable" という自然な意味論。LRU から SIEVE に daemon 再起動なしで切り替えられる。fmod_ret 版と比較してポリシー切替中の inconsistency ウィンドウが原理的にゼロ（→ `docs/design/policy-attach-comparison.md`）。

**制限・落とし穴**

| # | 事象 | 影響 |
|---|---|---|
| S1 | **kmod 側 boilerplate が約 200 LOC** | `verifier_ops` / `cfi_stubs` / `init` / `init_member` / `reg` / `unreg` / `update` の 7 callback が必須。fmod_ret 版（~150 LOC）の 2.3 倍に膨張 |
| S2 | **atomic swap（`BPF_LINK_UPDATE`）は kernel 6.4+** | それ以前は link close + 新規 attach で inconsistency window が生じる |
| S3 | **sleepable struct_ops は kernel 6.9+** | `.struct_ops.link.s` SEC は 6.9 未満で reject |
| S4 | **`BTF_KFUNCS_START` macro は kernel 6.11+** | 6.11 未満では `BTF_SET8_START` へのフォールバックが必要（`kmod/tree_traversal.c` に `#ifndef BTF_KFUNCS_START` ガード済） |
| S5 | **cilium/ebpf の struct_ops ドキュメントがほぼない** | `AttachStructOps` の使い方を sched_ext サンプルから逆算する必要がある |
| S6 | **既存の external kmod struct_ops 実例が大物すぎる** | sched_ext / TCP CC / HID-BPF は複雑で minimal template として使えない |

> **TODO(確認 S1)**: `bpf_struct_ops` の登録に必要な callback 一覧（`verifier_ops`・`cfi_stubs`・`reg`・`unreg`・`update`・`init_member`）が現行でも全て必須かを確認。省略可能な callback が追加されていないか確認。  
> 確認箇所: https://elixir.bootlin.com/linux/v6.12/source/include/linux/bpf.h (`struct bpf_struct_ops` 定義)

> **TODO(確認 S2)**: struct_ops link の `update` callback（`BPF_LINK_UPDATE` による atomic vtable swap）が追加された kernel バージョンを git log で確認。6.4 で正しいか。  
> 確認箇所: https://elixir.bootlin.com/linux/v6.12/source/kernel/bpf/bpf_struct_ops.c (`bpf_struct_ops_link_update` 付近)

> **TODO(確認 S3)**: sleepable struct_ops（`.struct_ops.link.s` SEC）のサポートが追加されたカーネルバージョンを確認。`bpf_struct_ops_map_sys_lookup_elem` または `bpf_struct_ops.sleepable` フィールドの初出を参照。  
> 確認箇所: https://elixir.bootlin.com/linux/v6.12/source/kernel/bpf/bpf_struct_ops.c

> **TODO(確認 S4)**: `BTF_KFUNCS_START` マクロが `include/linux/btf_ids.h` に追加されたバージョンを確認。6.11 で正しいか。`BTF_SET8_KFUNCS` フラグとの関係も合わせて確認。  
> 確認箇所: https://elixir.bootlin.com/linux/v6.12/source/include/linux/btf_ids.h (`BTF_KFUNCS_START` で検索)

---

### 2-7. KFunc — verifier エスケープハッチ

**用途・有用性**

BPF プログラムが unbounded なポインタ chase やスリープ可能な操作を実行したい場合に、通常の kernel C 関数として実装して `__ksym` で呼ぶ。termination 保証はシグナルチェック + `max_nodes` cap、安全性は `copy_from_user` の EFAULT。verifier の 1M 命令制限・ループ境界チェックを受けない。

登録した主な KFunc:

| KFunc | 用途 | prog type | flags |
|---|---|---|---|
| `s3fs_kfunc_arena_list_walk` | arena linked-list の unbounded walk（verifier デモ） | syscall | KF_SLEEPABLE |
| `s3fs_kfunc_admit_count` | 現在の admit 数を policy プログラムに公開（Layer 0 primitive） | tracing / struct_ops | — |
| `s3fs_kfunc_softcap` | softcap 値を policy プログラムに公開（Layer 0 primitive） | tracing / struct_ops | — |
| `s3fs_kfunc_capacity_bytes` | admit セットのバイト容量を公開 | tracing / struct_ops | — |
| `s3fs_kfunc_evictions_total` | 累積 eviction 数を公開 | tracing / struct_ops | — |
| `s3fs_kfunc_time_ns` | 時刻ベース decay ポリシー用の単調時刻 | tracing / struct_ops | — |
| `s3fs_kfunc_ino_size` | inode のファイルサイズを公開（size-aware ポリシー用）| tracing / struct_ops | — |
| `s3fs_kfunc_policy_hit` | fentry から on_hit を呼ぶブリッジ | tracing | — |

**FS・キャッシュでの価値**

eBPF verifier が "証明できない" 操作（arena linked-list 走査、カーネル側 admit 統計の参照）を kmod に移譲しつつ、eBPF 側は直線的な呼び出しのみにする。`s3fs_deep_walk_reject`（verifier reject）vs `s3fs_kfunc_arena_list_walk`（kmod 委譲）の対比は "なぜ KFunc エスケープハッチが必要か" を 1 スライドで説明できるデモになる（`bpf/s3fs_fastpath.bpf.c`・`bpf/kfunc_walk.bpf.c` 参照）。

**制限・落とし穴**

| # | 事象 | 影響 |
|---|---|---|
| K1 | **`KF_SLEEPABLE` はスリープ可能 prog type からのみ呼べる** | fentry / fmod_ret からは不可。sleepable struct_ops か tp_btf.s / lsm.s / uprobe.s から呼ぶ必要がある |
| K2 | **verifier エラーメッセージが薄い** | 「kfunc calls are not allowed in non-sleepable programs」だけで、どの kfunc がどの環境で呼べるかが不明。selftests/bpf が唯一の正典 |
| K3 | **prog type ごとに個別の `BTF_KFUNCS_START` セットが必要** | tracing / syscall / struct_ops の 3 セットを `register_btf_kfunc_id_set` で別登録しないと reject |
| K4 | **kmod が先にロードされている必要がある** | eBPF ロード時に `__ksym` をカーネル BTF に解決するため。kmod 未ロードで `LoadAndAssign` を呼ぶと即 EINVAL |
| K5 | **スリープ可能な処理は BPF 内で完結させられない** | S3 アクセス等の blocking I/O は upcall 3 段（kmod fop → miscdev queue → daemon → ioctl signal）のパターンに逃がすのが現実的な唯一手段（`docs/dev-notes/lessons-learned.md §2.3`）|

> **TODO(確認 K1)**: verifier が non-sleepable prog type から `KF_SLEEPABLE` kfunc の呼び出しを reject するチェックの実装箇所を確認。エラーメッセージの正確な文言（K2 と合わせて）も確認。  
> 確認箇所: https://elixir.bootlin.com/linux/v6.12/source/kernel/bpf/verifier.c (`KF_SLEEPABLE` / `kfunc calls are not allowed` で検索)

> **TODO(確認 K3)**: `register_btf_kfunc_id_set` が prog type ごとに独立したセットを要求する仕組みを確認。同一 kfunc を複数の prog type から呼べるようにするために複数回登録が必要かを確認。  
> 確認箇所: https://elixir.bootlin.com/linux/v6.12/source/kernel/bpf/btf.c (`register_btf_kfunc_id_set` 実装)

---

### 2-8. `fmod_ret` — フック戻り値オーバーライド

**用途・有用性**

`noinline int s3fs_policy_on_evict(u64 ino)` 等に `SEC("fmod_ret/s3fs_policy_on_*")` を attach し、BPF プログラムの戻り値でオリジナルの戻り値を置き換える。struct_ops の代替実装として `bpf/policy_freq2bit.bpf.c` に実装済み。

`bpf/policy_observe.bpf.c` も fmod_ret を使用するが、eviction 決定を行わない**観測専用ベースライン**。`on_admit` と `on_hit` のみを実装（`on_evict` / `on_pressure` なし）し、`policy_stats` マップに呼び出し回数と最終引数を記録する。KFunc 配線・policy hot-swap・daemon の stats 読み取りパスの動作確認用として使う。

**制限・落とし穴**

- tracing link は `BPF_LINK_UPDATE` **非対応**。ホットスワップは「新 3 link attach → 旧 3 link close」の 2 フェーズになり、その間 on_admit は新ポリシー・on_evict は旧ポリシーという inconsistency window が生じる
- struct_ops の atomic swap（1 RCU pointer swap）に比べると atomicity 保証が弱い
- フック関数に `noinline` が必要（コンパイラにインライン化させない）
- struct_ops との使い分け基準は `docs/design/policy-attach-comparison.md §3` 参照

> **TODO(確認)**: tracing link (`fentry` / `fmod_ret`) が `BPF_LINK_UPDATE` を `ENOTSUPP` で reject する実装箇所を確認。`tracing_link_update` または `bpf_tracing_link_update` の有無を参照。  
> 確認箇所: https://elixir.bootlin.com/linux/v6.12/source/kernel/bpf/tracing.c (`link_update` / `ENOTSUPP` で検索)

---

### 2-9. CO-RE (`bpf_core_read`) — カーネル移植性

**用途・有用性**

`BPF_CORE_READ(path, dentry)` 等で kernel バージョン間の struct レイアウト差を BTF リロケーションで吸収する。一度コンパイルしたオブジェクトを異なる kernel バージョンで動かせる。

**FS での価値**

`dentry → super_block → s_magic` や `kiocb → ki_filp → f_inode → i_ino` のような多段チェーンが移植性を保ちながら安全に書ける。

**落とし穴**

- BTF が `/sys/kernel/btf/vmlinux` にない環境では CO-RE が機能しない（distro kernel の設定依存）
- `vmlinux.h` は `bpftool btf dump file /sys/kernel/btf/vmlinux format c` で生成するが、pahole のバージョンによって enum が途中で打ち切られることがある（`BPF_MAP_TYPE_ARENA = 33` が出力されない問題の根本原因）

---

### 2-10. `bpf_loop()` — 境界なしループの verifier 回避

**用途・有用性**

W-TinyLFU ポリシーの CMS スケッチ aging（1024 スロット全体のカウンタを半減）に使用。`bpf_loop(CMS_W * CMS_D, age_slot_cb, NULL, 0)` で境界をコールバック経由に移譲し、verifier の静的ループ展開上限を回避する（`bpf/policy_wtinylfu_ops.bpf.c` 参照）。

**制限・落とし穴**

- kernel 5.17+
- コールバック呼び出しのオーバーヘッドが per-iteration に乗る（インライン展開されない）

> **TODO(確認)**: `bpf_loop()` が kernel 5.17 で追加されたことを確認。コールバックがインライン展開されない（verifier がコールバックを独立したプログラムとして扱う）ことも合わせて確認。  
> 確認箇所: https://elixir.bootlin.com/linux/v6.12/source/kernel/bpf/helpers.c (`bpf_loop` 実装)

---

### 2-11. `__arena` / `bpf_addr_space_cast` — arena ポインタ型

**用途・有用性**

BPF プログラム内で arena ポインタを `__arena` アドレス空間でアノテートすると、clang が `bpf_addr_space_cast` 命令を自動 emit し、verifier が PTR_TO_ARENA として追跡する。`arena_types.h` に各 toolchain 向けのフォールバック定義あり。

**落とし穴（最も手が込む）**

| # | 事象 |
|---|---|
| C1 | **clang 19+ + `-mattr=+addr_space_cast`** が必須。clang 18 以前は `__BPF_FEATURE_ADDR_SPACE_CAST` が未定義でフォールバックパスになる |
| C2 | **cilium/ebpf の marshal バグ**: `Instruction.Marshal` が plain `Mov` の `Offset` フィールドを無条件で 0 上書き → `bpf_addr_space_cast`（opcode `0xbf｜ALU64｜X`、Offset=1、imm=1）が「BPF_MOV uses reserved fields」で verifier reject される。**ローカルパッチ適用済み、upstream PR 未提出**（`docs/misc/cilium-ebpf-addr-space-cast-marshal.patch`）|
| C3 | **prog→aux→arena wiring が必要**: verifier は arena を参照する `LD_MAP_FD` 命令を見た時だけ `prog->aux->arena` をセットする。`bpf_map_lookup_elem`（key_size=0 で reject）も `bpf_arena_alloc_pages`（実ページ確保）も使えないため inline-asm で LD_MAP_FD を強制 emit する `s3fs_arena_pin()` trick が必要（`bpf/s3fs_fastpath.bpf.c` 参照）|

> **TODO(確認 C1)**: clang の `__BPF_FEATURE_ADDR_SPACE_CAST` が 19 で追加されたことを確認。clang 18 でのフォールバック挙動（`__arena` が無効化される）を実際にビルドして確認しておくこと。  
> 確認箇所: https://github.com/llvm/llvm-project (`__BPF_FEATURE_ADDR_SPACE_CAST` で検索、clang/lib/Basic/Targets/BPF.cpp 付近)

> **TODO(確認 C3)**: `resolve_pseudo_ldimm64` で LD_MAP_FD が arena 型 map を参照した際に `env->prog->aux->arena` がセットされること、および `check_alu_op` の addr_space_cast パスで `prog->aux->arena` が未設定の場合に reject するチェックを確認。  
> 確認箇所: https://elixir.bootlin.com/linux/v6.12/source/kernel/bpf/verifier.c (`prog->aux->arena` / `resolve_pseudo_ldimm64` で検索)

---

## 3. カーネル・toolchain バージョン別制約マトリクス

| 機能 | 最低 kernel | 最低 clang | 備考 |
|---|---|---|---|
| fentry / CO-RE 基本セット | 5.5 | 10 | BTF あり distro kernel 前提 |
| BPF_MAP_TYPE_RINGBUF | 5.8 | — | |
| bpf_loop() | 5.17 | — | |
| KFunc 外部 kmod 登録 | 5.18 | — | |
| fmod_ret (BPF_MODIFY_RETURN) | 5.7 | — | |
| struct_ops 外部 kmod 登録 | 5.6 (basic) / **6.4** (BPF_LINK_UPDATE atomic swap) | — | |
| BPF_MAP_TYPE_ARENA | **6.9** | **19** + `-mattr=+addr_space_cast` | 実質安定は 6.12 LTS |
| sleepable struct_ops | 6.9 | — | |
| BTF_KFUNCS_START macro | 6.11 | — | 6.11 未満は BTF_SET8_START |
| `kvrealloc` (kmod 側) | 6.11 | — | 6.11 未満は kvfree+kvmalloc |
| `mnt_idmap` inode_ops 引数 | 6.3 | — | 6.3 未満は別 signature |
| `pin_user_pages_remote` の mmap_read_lock 必須 | 6.12 固有 WARN | — | ドキュメントと実装が乖離 |
| `no_llseek` 削除 → `noop_llseek` | 6.12 | — | file_operations の `.llseek` 要更新 |

> **TODO(確認・表全体)**: 各行のバージョンを kernel の git log / Elixir で 1 件ずつ確認してから発表資料に転記すること。特に下記は誤りやすい:
>
> - `KFunc 外部 kmod 登録 5.18`: https://elixir.bootlin.com/linux/v5.18/source/kernel/bpf/btf.c (`register_btf_kfunc_id_set` の初出を確認)
> - `struct_ops BPF_LINK_UPDATE 6.4`: https://elixir.bootlin.com/linux/v6.4/source/kernel/bpf/bpf_struct_ops.c (`bpf_struct_ops_link_update` の有無を確認)
> - `mnt_idmap 引数 6.3`: https://elixir.bootlin.com/linux/v6.3/source/include/linux/fs.h (`inode_operations.getattr` のシグネチャ変更を確認)
> - `pin_user_pages_remote mmap_read_lock 必須`: https://elixir.bootlin.com/linux/v6.12/source/mm/gup.c (`pin_user_pages_remote` の lockdep assert を確認)

---

## 4. ディストリビューション別の動作可否

| ディストリビューション | kernel | BPF_ARENA | struct_ops atomic | 備考 |
|---|---|---|---|---|
| Ubuntu 24.10 (Oracular) | 6.11 | ✅ | ✅ | 検証環境はこれ |
| Ubuntu 24.04 LTS (Noble) | 6.8 (HWE: 6.11) | ✅ (HWE のみ) | ✅ (HWE のみ) | generic kernel (6.8) は BPF_ARENA 不可 |
| Fedora 40 | 6.9 | ✅ | ✅ | |
| Fedora 39 | 6.6 | ❌ | ✅ | |
| Debian 12 (Bookworm) | 6.1 LTS | ❌ | ❌ | backports-kernel でも 6.6 が上限 |
| RHEL 9.x | 5.14 (RHEL 特化) | ❌ | ❌ | BPF 機能が大幅制限される |
| Arch Linux (rolling) | 最新 | ✅ | ✅ | |

---

## 5. 発表で伝えるべき "本質的な制限 3 つ"

### 5-1. BPF_ARENA の kern_vm 問題は kernel patch 前提

6.12 時点では arena の kern_vm 側に汎用 fault handler がなく、kmod から直接触れない。GUP over user_vm の迂回を強いられ、数百 ns のオーバーヘッドが残る。`EXPORT_SYMBOL_GPL` 1 行（K-01）+ fault handler patch ~50 LOC（K-02）で解決可能だが upstream 到達は月単位。現実的な短期対策は Path B（GUP over user_vm）で妥協すること（`docs/design/read-path-design.md` 参照）。

### 5-2. struct_ops は意味論が美しいが kmod boilerplate が高い

vtable atomic swap・hot reload・型安全性は群を抜く。ただし外部 kmod からの登録に verifier_ops / cfi_stubs / reg/unreg/update の全 boilerplate が必要で、最小実装でも ~200 LOC。sched_ext 以外の minimal external example が selftests にない。「policy = vtable」という設計思想の価値とトレードオフを明示的に語ることで、このコストが妥当だと納得させられる（`docs/design/policy-attach-comparison.md §3` 参照）。

### 5-3. KF_SLEEPABLE の呼び出し側制約は verifier エラーメッセージから見えない

「どの prog type からどの kfunc が呼べるか」の正典が selftests/bpf しかなく、非合法な呼び出しを試みると「kfunc calls are not allowed」という 1 行だけが返る。実際には非スリープ可能 program から KF_SLEEPABLE kfunc を呼ぼうとしていることが多いが、エラーメッセージにそのヒントがない。`docs/dev-notes/ebpf-ecosystem-gaps.md §3` に改善提案あり。

---

## 6. 参照

- `docs/design/policy-attach-comparison.md` — fmod_ret vs struct_ops の実測比較
- `docs/design/ebpf-fuse-advantages-roadmap.md` — FUSE に対する構造的優位性と未実装の発展計画
- `docs/dev-notes/ebpf-ecosystem-gaps.md` — upstream 改善提案の 1 次素材（BPF_ARENA / STRUCT_OPS / KF_SLEEPABLE）
- `docs/dev-notes/lessons-learned.md` — 横断的な実装教訓チェックリスト
- `bpf/s3fs_fastpath.bpf.c` — fentry・ringbuf・arena・percpu_array の主要実装
- `bpf/kfunc_walk.bpf.c` — KFunc 委譲の "after" デモ
- `kmod/tree_traversal.c` — KFunc 登録（BTF_KFUNCS_START セット）
- `kmod/policy_struct_ops.c` — struct_ops vtable kmod 側実装
- `bpf/arena_types.h` — __arena / addr_space_cast のフォールバック定義
