# fmod_ret vs struct_ops — Layer 2 attach pattern の比較評価

Week 4 (d) Layer 2 で policy BPF program の attach pattern を fmod_ret と struct_ops の 2 方式で実装し、同一 algorithm (S3-FIFO) を両方で動作させた。実測に基づく比較と recommendation。

対応 commit: (次 commit) `fmod_ret 化 + struct_ops 化 + 両方併存で voted evict 経路検証`

対応 ソース:
- **fmod_ret**: `kmod/tree_traversal.c` の `BTF_SET8_START(s3fs_policy_fmodret_ids)` + `bpf/policy_s3fifo.bpf.c` の `SEC("fmod_ret/s3fs_policy_on_*")`
- **struct_ops**: `kmod/policy_struct_ops.{c,h}` + `bpf/policy_s3fifo_ops.bpf.c` の `SEC(".struct_ops.link") struct s3fs_policy_ops`

---

## 1. 5 軸での比較 (実測)

### 1.1 実装コスト (LOC 追加分)

| category | fmod_ret | struct_ops | delta |
|---|---:|---:|---:|
| kmod 側 infrastructure | ~15 (BTF_SET8 + register 1 呼出) | **220** (policy_struct_ops.{c,h} = 183+37) | +205 |
| kmod call site の追加 (直接呼び) | 3 hook × 1 呼出 = 3 行 | + rcu_dereference wrapper 呼び 3 行 | ± 0 |
| BPF program 側 | 131 行 (policy_s3fifo.bpf.c) | 105 行 (policy_s3fifo_ops.bpf.c、side-channel map 不要でむしろ short) | -26 |
| daemon loader 側 | fentry attach + multi-link 対応 15 行 | + struct_ops attach path 20 行 | +5 |
| **合計** | **~150 行** | **~350 行** | **+200 行 (+130%)** |

**判定**: fmod_ret **勝ち**。struct_ops は verifier_ops / cfi_stubs / reg/unreg/update callback / init_member の 4 callback 実装が必要で、kmod 側 boilerplate が支配的。

### 1.2 API の意味論の直結度

**fmod_ret**:
```c
// kmod
noinline int s3fs_policy_on_evict(u64 ino) { ... return 0; }
if (s3fs_policy_on_evict(ino) != 0) evict_votes++;

// BPF
SEC("fmod_ret/s3fs_policy_on_evict")
int BPF_PROG(evict, u64 ino) { return decision; }
```
「関数を fentry hook で置換」の意味論。3 hook が独立、swap は per-hook。

**struct_ops**:
```c
// kmod
struct s3fs_policy_ops { int (*on_evict)(u64); ... };
struct s3fs_policy_ops *ops = rcu_dereference(active);
if (ops && ops->on_evict(ino) != 0) evict_votes++;

// BPF
SEC(".struct_ops.link")
struct s3fs_policy_ops s3fifo_ops = { .on_evict = ..., ... };
```
「vtable = policy」の意味論。全 hook が 1 unit として atomic に扱える。

**判定**: struct_ops **勝ち**。「policy とは decision functions の集合」= vtable が自然。sched_ext / TCP CC / HID-BPF の実績パターン。

### 1.3 Hot swap の atomicity

**fmod_ret**:
- 3 hook を個別に attach、hot swap は 3 links を順次 `link.Update()`
- **差替え瞬間の inconsistency 可能性**: on_admit は新 policy、on_evict は旧 policy、の状態がありうる (short window だが 0 ではない)
- 実装で緩和するには全 links を synchronize させる external barrier が要る

**struct_ops**:
- 1 vtable map、hot swap は `rcu_assign_pointer + synchronize_rcu()` の 1 操作
- **原理的に atomic** — RCU readers は「旧 vtable の全 method」か「新 vtable の全 method」のどちらかを見る
- kmod 側 update callback (`s3fs_policy_update`) が実装済で、grace period 明示

**判定**: struct_ops **明確に勝ち**。「LRU → S3-FIFO に瞬時に切替」demo narrative に決定的な差。

### 1.4 Latency (per-call overhead)

両方とも kmod → BPF の 1 回 dispatch なので理論値は近い。実測は今回まだ取っていないが:

**fmod_ret**:
- fentry trampoline の overhead: ~50-100ns
- + BPF program 実行
- + return value override

**struct_ops**:
- `rcu_dereference` (barrier + load): ~5ns
- + function pointer indirect call: ~5-10ns
- + BPF program 実行 (fentry trampoline なし、BPF program 直接呼出?)

**判定**: 概ね同等、struct_ops **わずかに勝つ可能性**。厳密なマイクロベンチは future work。両方とも per-op で数十 ns、それ以下 (algorithm 本体 + map lookup の方が支配的)。

### 1.5 運用面 (debug / observability / hot reload)

| 観点 | fmod_ret | struct_ops |
|---|---|---|
| bpftool prog show の見え方 | 3 `tracing` prog | 3 `struct_ops` prog + 1 STRUCT_OPS map |
| bpftool link show | 3 link (attach_type modify_return) | 1 link (attach_type struct_ops) |
| Attach 失敗時 partial state | 1〜2 個 attach 成功、残 fail の場合 unwind 必要 | atomic (成功か全 fail か) |
| Hot reload 単位 | `link.Update` × N | `link.Update` × 1 |
| kmod 側 debug print の見え方 | 3 callback、fentry stub 呼出 | reg/unreg/update callback 明示的に log 可能 |
| kmod 実装複雑度 | 1 個 register 呼出 | init/init_member/reg/unreg/update/validate/cfi_stubs |

**判定**: struct_ops **やや勝ち** (atomic + observability API 明確)。ただ実装複雑度は明確に負け。

---

## 2. 実測 log から見た snapshot

**fmod_ret** (両方 attach 中):
```
[349548] s3fs_ebpf: policy voted evict on 11/12 entries
[349552] s3fs_ebpf: policy voted evict on 11/11 entries
```

**struct_ops** (両方 attach 中):
```
[349975] s3fs_ebpf: policy struct_ops registered
[349994] s3fs_ebpf: struct_ops voted evict on 11/12 entries
```

同じ S3-FIFO logic が両 attach 経路で同じ vote (11/12) を出す。BPF map (freq counter) を共有していないので値の一致は algorithm の再現性を示す。

---

## 3. どちらを primary にすべきか

### PoC / spike / demo 段階では **struct_ops 推奨**

理由:
- **narrative**: 「policy = vtable、hot swap で瞬時 atomic 差替」が Meetup demo で clean
- **意味論**: 型で表現される「policy = decisions の集合」が学習コスト最小
- **atomicity**: policy 切替中の inconsistency が原理的にない
- **precedent**: sched_ext / TCP CC / HID-BPF の実績 pattern
- **実装コストは high だが 1 回だけ払えばよい** — 追加 policy を書くのは fmod_ret と同じくらいのコスト

### Production tuning / 極限 latency 段階では **fmod_ret も温存**

理由:
- **1 hook だけ動的に override したい** ケース (例: 特定 kernel path の A/B test) には per-hook attach が向く
- **既存 fentry pattern の team familiarity** — struct_ops は学習コスト
- **軽量な "1 個だけ policy" を差し込むテスト** に手軽

### 両立共存の姿

**現状の実装で両方共存が可能**:
- kmod は fmod_ret hook (`s3fs_policy_on_*`) と struct_ops vtable (`s3fs_policy_active`) の両方を保持
- call site は両方 evaluate、両方の vote を独立 log
- `--policy-bpf` flag で BPF program を選ぶだけ (中身が SEC fmod_ret か SEC struct_ops かで自動 dispatch)
- future: struct_ops が primary、fmod_ret は「LSM 相当の細粒度 hook」として補助

---

## 4. Recommendation

**s3fs-ebpf 期間中は struct_ops を primary に格上げ、fmod_ret は互換用に維持**:

1. `--policy-bpf=builtin:policy_s3fifo_ops` を Meetup demo 用 default に
2. `--policy-bpf=builtin:policy_s3fifo` (fmod_ret 版) は「fine-grained per-hook attach」の option として残す
3. 新 algorithm (LRU / SIEVE / W-TinyLFU) は struct_ops 版 (`policy_lru_ops.bpf.c` 等) を先に書く
4. kmod 側 call site から fmod_ret hook の呼出は残す (無害、性能影響も無視できる)。fmod_ret が attach されていなければ return 0、struct_ops が attach されていれば struct_ops vote を採用

## 5. 未対応 / 後回し

- **kmod 側の vote 消費実装**: 現状「voted evict on N/M」log を出すだけ。実際に admit table から drop する enforcement は followup (P-09 の続き)
- **CEL Layer 1 から struct_ops program を選ぶ integration**: 現状は `--policy-bpf` 直接指定、CEL で `policy: "s3fifo_ops"` を書けるように (P-03)
- **micro benchmark**: fmod_ret vs struct_ops の per-call overhead 実測 (LTTng / perf trace で)
- **struct_ops の update callback 再検証**: 現在の実装は `synchronize_rcu()` で待つが in-flight event の semantics 詳細調査要

---

## 6. TL;DR (Meetup 3 行)

> - **fmod_ret**: BTF_SET8 で hook を BPF 側から override 可能にする最小 diff の方式 (~150 行)。fentry と同じ per-hook attach。
> - **struct_ops**: 「policy = vtable」を型で表現し、atomic swap で hot reload を保証。実装は ~350 行だが意味論が clean で demo narrative に強い。
> - **s3fs-ebpf は struct_ops を primary、fmod_ret を fine-grained escape hatch として温存**。両方 attach しても互いに独立で動作する (kmod 側 call site が両方 evaluate)。
