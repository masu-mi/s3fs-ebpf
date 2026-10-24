# eBPF 開発環境の足りないもの — s3fs-ebpf を作って踏んだ地雷

Week 1〜4 (e) の PoC 実装で当たった「upstream / cilium/ebpf / kernel の未提供・未整備」を、**具体的な症状と修正提案** として整理する。`docs/dev-notes/lessons-learned.md` が「同じ地雷を次のプロジェクトで避ける」チェックリストなのに対し、この doc は **upstream に投げるならこの形** の 1 次素材。

範囲: BPF_ARENA / BPF_STRUCT_OPS / KF_SLEEPABLE の 3 機能を中心に、横断的な tooling gap も末尾で扱う。

> 2026-10-24: cilium/ebpf から arena と struct_ops を使うときの課題と要望は、その後の変化を反映して [`cilium-ebpf-arena-and-struct-ops.md`](cilium-ebpf-arena-and-struct-ops.md) にまとめ直した。

出典表記: `w4a` = `docs/journal/week4a-implementation-notes.md`、`ll` = `docs/dev-notes/lessons-learned.md`、`pc` = `docs/design/policy-attach-comparison.md`、`ci-patch` = `docs/misc/cilium-ebpf-addr-space-cast-marshal.patch`。関連する backlog 項目は `../journal/backlog.md §K` (upstream 貢献) と対応。

---

## 1. BPF_ARENA — 一番苦しんだ領域

### 1.1 問題点まとめ

| # | 事象 | 具体 | 現状の逃げ道 |
|---|---|---|---|
| A1 | **kern_vm range に generic fault handler がない** | kmod から `bpf_arena_get_kern_vm_start()` の返り値を deref → 全 EFAULT。JIT の `addr_space_cast` は独自経路で kern_vm を触るが、外部 kernel context (kmod 側 `.read_folio` 等) からの access には `arena_vm_fault` が反応しない | Path A′ を断念、B (GUP over user_vm) にピボット (`w4a §3-§4`) |
| A2 | **`struct bpf_arena` が非 export** | `kernel/bpf/arena.c` 内部型。`container_of(map, struct bpf_arena, map)` で戻したいが header が無い | BTF から取り出した `struct bpf_arena_layout_6_12` を kmod 内で redefine (`kmod/s3fs_ebpf_dev.c:45`)。6.12 の layout に固く縛られる、bump のたびに要検証 |
| A3 | **`bpf_arena_get_kern_vm_start()` が非 GPL export** | `EXPORT_SYMBOL_GPL` されていない | kmod からは直接呼べない → A2 の container_of hack が必須 |
| A4 | **arena → daemon の bytes 転送に GUP + kmap + copy_to_user の 3 段** | user_vm 側は fault handler が効くので pin できる | Path B 実装がまさにこれ。3 段の overhead が hot path に固定 (bench で fuse-cache 比 2.5×、A′ が動けばあと数百 ns 削れる見込み) |

### 1.2 改善案

**K-01 (backlog 済)**: `bpf_arena_get_kern_vm_start()` を `EXPORT_SYMBOL_GPL` 化。
- 変更範囲: 1 行 (`kernel/bpf/arena.c` に `EXPORT_SYMBOL_GPL(...)`)。
- 効果: A2 (BTF-derived redefine) の hack が丸ごと不要になる。out-of-tree kmod 全般に効く。
- リスク: なし。この関数はすでに BPF verifier 側から expose されており、追加露出は kmod 経路だけ。

**K-02 (backlog 済)**: arena kern_vm range 用の generic-context fault handler を追加。
- 変更範囲: `arena_vm_fault` の condition を JIT 経路の marker 依存から外し、kern_vm 側にも fault-in する path を作る。~50 LOC 規模の kernel patch。
- 効果: **Path A′ (kern_vm 直 deref) が復活**。B の GUP overhead が消える (数百 ns オーダー改善見込み)。
- リスク: **中**。arena の semantics (「BPF 実行時 context 専用の shadow mapping」という設計思想) に踏み込む変更なので、maintainers の同意を得るには A′ 側の use case を強く示す必要がある。

**追加案 (未提案)**: `include/linux/bpf.h` に `struct bpf_arena` の最小 opaque accessor を expose。
- 例: `unsigned long bpf_arena_kvm_start(struct bpf_map *)` / `unsigned long bpf_arena_size(struct bpf_map *)`。
- 効果: kmod が BTF hack なしで arena metadata を取れる。K-01 より広範な use case をカバー。
- K-01 (単一関数 export) が却下された場合の fallback 提案として温存。

---

## 2. BPF_STRUCT_OPS — 意味論は最高、実装は重い

### 2.1 問題点まとめ (`pc §1-§3`)

| # | 事象 | 具体 |
|---|---|---|
| S1 | **kmod 側 boilerplate が支配的** | `verifier_ops` / `cfi_stubs` / `reg / unreg / update / init_member` の 4-5 callback を書かないと `register_bpf_struct_ops` が通らない。fmod_ret 版 (~150 LOC) と struct_ops 版 (~350 LOC) で同じ algorithm を書いたら **LOC が 2.3× に膨張**、追加分の 9 割は glue コード |
| S2 | **既存 reference が大物すぎて minimal example にならない** | sched_ext / TCP CC / HID-BPF が現行の実例。どれも複雑で、"policy hook 系の最小 vtable" を書きたい人にとっての template として大きすぎる |
| S3 | **atomic swap の意味論は文句なし** | `rcu_assign_pointer + synchronize_rcu()` の 1 操作。fmod_ret の "3 link を順次 update" で発生する inconsistency window (on_admit 新 / on_evict 旧) が原理的にない — Meetup demo narrative では決定的な差 |
| S4 | **cilium/ebpf 側の docs が薄い** | `link.AttachStructOps` に相当する API は存在するが on-line docs にほぼ言及なし。attach path の型付けを sched_ext example 経由で逆算する必要がある |

### 2.2 改善案

**提案 1: kernel-side に "generic policy vtable" template マクロ**
- 例: `DEFINE_STRUCT_OPS_VTABLE(name, methods, ...)` で cfi_stubs / init_member / verifier_ops のデフォルトを自動生成。
- 効果: S1 の 200 LOC boilerplate を ~30 LOC に圧縮。
- 前例: `DEFINE_BPF_STRUCT_OPS` 的なマクロは既にあるが、cfi_stubs 生成までは自動化されていない。
- 難易度: 中 (Linux macro 芸)。

**提案 2: `selftests/bpf/` に "minimal external kmod struct_ops" reference**
- 例: `tools/testing/selftests/bpf/prog_tests/struct_ops_minimal_external.c` に、~200 LOC の外部 kmod + 1 vtable の完全例。
- 効果: S2 の学習コスト圧縮。sched_ext を読まずに済む。
- コスト: 単発の追加、maintenance は selftests 責任者。

**提案 3: cilium/ebpf 側 docs**
- `docs/attach.md` (仮) に AttachStructOps セクションを新設。SEC 命名 → attach type mapping、reg/unreg の callback fire タイミング、link.Update の semantics を明記。
- 効果: 我々自身が次に struct_ops program を書くときの摩擦がまず消える。

---

## 3. KF_SLEEPABLE — 使いたくても呼ぶ側の縛りが厳しい

### 3.1 問題点まとめ

| # | 事象 | 具体 |
|---|---|---|
| K1 | **呼び出し側 BPF program も sleepable 型でないと reject** | KF_SLEEPABLE 付き kfunc は tp_btf / lsm / uprobe / `SEC(".struct_ops.link.s")` などの sleepable program 型からしか呼べない。fentry / fmod_ret / (通常の) struct_ops からは verifier reject |
| K2 | **sleepable struct_ops はまだ拡張中** | 6.9+ で `bpf_struct_ops.sleepable = true` の flag が入ったが、既存の vtable を sleepable 化するには **vtable 定義側と kmod 側両方の変更** が要る。in-flight event の semantics 詳細調査も未 |
| K3 | **迂回が事実上必須** | 我々のケース (S3 access は blocking I/O 必須) では BPF 側で完結させる道は無い。**upcall 3 段 pattern** (kmod fop → miscdev queue → daemon PUT/GET → ioctl signal) で全部逃げた (`ll §2.3`) |
| K4 | **verifier error message が薄い** | sleepable/non-sleepable 不整合で reject されると "kfunc calls are not allowed in non-sleepable programs" と出るが、**どの kfunc がどの環境で呼べるかの正典が selftests/bpf しかない** |

### 3.2 改善案

**提案 4: kernel doc に KF_SLEEPABLE の実行環境 matrix**
- 場所: `Documentation/bpf/kfuncs.rst` に "Sleepable KFunc invocation matrix" セクション。
- 内容: KF_SLEEPABLE 付き kfunc がどの program type から呼べるかの表 (tp_btf / lsm / uprobe / struct_ops.s / ...)。
- 効果: K4 の学習コスト圧縮。
- 難易度: 低 (doc-only)。

**提案 5: verifier error message の improvement**
- 現状: "kfunc calls are not allowed" (どの制約が引っかかったのか不明)。
- 提案: "kfunc `foo` requires sleepable context (KF_SLEEPABLE), but this program is non-sleepable. Use SEC(\".struct_ops.link.s\") or attach as tp_btf.s" のような具体メッセージ。
- 効果: 初見での debug 時間が数十分オーダーで削れる。
- 難易度: 中 (verifier.c の該当 branch を特定して message を差し替える)。

**提案 6: cilium/ebpf 側の SEC → sleepable flag mapping documentation**
- 現状: `asm/instruction.go` を読まないと分からない。
- 提案: `docs/prog-types.md` (仮) に SEC 命名一覧 + それぞれの sleepable 可否 + 呼べる KF_SLEEPABLE kfunc の例。
- 効果: K1 の設計時判断が cilium/ebpf ユーザで完結する。

---

## 4. 横断的な ecosystem の穴

### 4.1 cilium/ebpf の marshal bug (patch 済み、upstream PR 未提出)

`docs/misc/cilium-ebpf-addr-space-cast-marshal.patch` 参照。

- **症状**: plain `Mov` instruction の `Offset` が `Instruction.Marshal` 段階で unconditionally 0 に上書きされ、`bpf_addr_space_cast` (opcode `0xbf|ALU64|X` + `Offset=1` + `imm=1`) が verifier reject される (`BPF_MOV uses reserved fields`)
- **影響**: **addr_space_cast を使う人は誰でも踏む** (LLVM 19+ + `-mattr=+addr_space_cast` の組合せ)
- **patch**: 10 LOC。cilium/ebpf `asm/instruction.go:192` 付近に条件付き分岐
- **次アクション**: upstream PR 化。手元の patch はテスト済 (`make test-arena` で verified)、maintainers への説明 template も同 patch ヘッダに準備済
- **追記 (2026-10-24)**: 今 `daemon/go.mod` が指す cilium/ebpf の commit (`5aef01488cf5`、2026-10-08) の `asm/instruction.go` には同等の修正が入っており、vendor は外している。タグ付きリリースに入ったかは未確認。最新の状態は [`cilium-ebpf-arena-and-struct-ops.md`](cilium-ebpf-arena-and-struct-ops.md) の C1

### 4.2 VerifierError の truncation

- **現状**: cilium/ebpf の `VerifierError.Error()` は full kernel verifier log を truncate する。`%+v` フォーマットを使わないと reject の実際の原因が見えない (`daemon/internal/ebpf/loader.go:152` の workaround)
- **影響**: verifier reject を初見で追う人にとって最大の摩擦点。1 行の error message からは何も分からず、実装者は必ずこの workaround に辿り着くまで数時間浪費する
- **改善案**: cilium/ebpf 側で `VerifierError.Error()` に full log を含めるオプション (`log-full` env var 相当) を追加、あるいは `Error()` の default を full log に切り替え

### 4.3 `bpftool` の情報密度

- **現状**: `bpftool prog show` / `link show` は attach_type や name しか出さない
- **struct_ops の場合**: vtable の中身は `bpftool struct_ops dump` 別コマンド。1 shot で "全 loaded state" を見る tool がない
- **改善案**: `bpftool inspect [subsystem]` (仮) で prog + link + map + struct_ops を bundle 表示する higher-level view

### 4.4 kernel / clang version pin の複雑さ

現代の BPF 開発では機能ごとに要求 version が異なる:

| 機能 | 最低要件 |
|---|---|
| BPF_ARENA | kernel 6.9+ |
| addr_space_cast (`bpf_addr_space_cast`) | clang 19+ + `-mattr=+addr_space_cast` |
| sleepable struct_ops | kernel 6.9+ + `bpf_struct_ops.sleepable = true` |
| KF_SLEEPABLE + tp_btf.s | kernel 6.4+ |
| bpf_loop() | kernel 5.17+ |

- **現状**: 組合せ matrix を出す standard tool がない。実装者が毎回 selftests/bpf の README や git log で確認する
- **改善案**: `bpftool features` を拡張して kernel / clang / libbpf の 3 段組合せから「有効な feature」を出力

### 4.5 out-of-tree kmod ワークアラウンドの正当化コスト

上の K-01/K-02 のような **5 行〜数十行の kernel 改善** でも upstream 到達に月単位。実装者の合理的判断は「out-of-tree kmod で workaround」に流れる (我々もそう判断した)。

- **副作用**: 生態系が分岐する。BPF_ARENA のようなまだ新しい機能は「本当の使い方」が selftests と各プロジェクトの hack で分裂したまま
- **改善方向**: これは upstream カルチャの問題で 1 プロジェクトで解けないが、**PoC 側は必ず「upstream patch にすべき変更 vs out-of-tree で許容する hack」を明示的に区別する doc を持つ**。この doc + `docs/journal/backlog.md §K` がその役割

---

## 5. 開発者への 1 行アドバイス

s3fs-ebpf の 4 週間で確定した現場則。

1. **完璧設計より spike-first**: BPF_ARENA A′ に固執せず 1-2 日で GUP にピボットしたのが 4 週 PoC を成立させた最大要因 (`ll §2.1`)
2. **kmod 側で BTF hack をするなら kernel version guard を必ず立てる** — `#if LINUX_VERSION_CODE < KERNEL_VERSION(6,12,0)` (`kmod/s3fs_ebpf_dev.c:41`)
3. **struct_ops vs fmod_ret は "atomicity が要るか" で決める** — narrative demo → struct_ops、per-hook A/B test → fmod_ret (`pc §3`)
4. **sleepable が必要な処理は BPF 内で頑張らず upcall pattern に** — 3 段 upcall は書ける、`refcount_t + wait_for_completion_interruptible_timeout` の template で (`ll §2.3`)
5. **`VerifierError` は必ず `%+v` で出力** — さもなくば reject 原因は闇に消える

---

## 6. TL;DR (3 行)

> - **BPF_ARENA**: kern_vm range が JIT 経路専用で外部 kmod から見えない。GUP over user_vm に迂回するのが現行の唯一手段。EXPORT_SYMBOL_GPL 1 行と fault handler patch で解決可能
> - **BPF_STRUCT_OPS**: 意味論と atomicity は素晴らしいが kmod-side boilerplate が 200 LOC 規模。generic vtable template マクロと minimal external kmod reference が欲しい
> - **KF_SLEEPABLE**: 呼ぶ側の program 型制約が verifier error message から見えず、迂回に upcall 3 段 pattern が事実上必須。実行環境 matrix の kernel doc 化と error message 改善で摩擦の半分は消える
