# cilium/ebpf から bpf_arena と struct_ops を使う: 課題、使い方、upstream への要望 (2026-10-24)

この PoC の daemon は Go で、BPF の load と attach を cilium/ebpf で行っている。この doc は、(1) cilium/ebpf から bpf_arena を使うときに当たった課題と回避策、(2) struct_ops をこの PoC でどう使っているか (kmod / BPF / daemon の 3 か所) と一般的な使い方、(3) upstream (Linux カーネルと cilium/ebpf) への要望、を 1 か所にまとめる。

- 前提の版: Linux 6.12 LTS、clang 20、cilium/ebpf `v0.22.1-0.20261008141411-5aef01488cf5` (`daemon/go.mod`)。
- 関連 doc: 要望の 1 次素材と経緯は [`ebpf-ecosystem-gaps.md`](ebpf-ecosystem-gaps.md) (Week 4 時点)。この doc はその後の変化 (marshal 修正の取り込み、struct_ops link の update、pcache モード) を反映した最新版で、発表の付録スライド `appx-cilium-arena` / `appx-so-howto` の元になっている。
- 「Cilium」はここでは Go ライブラリ cilium/ebpf を指す。Cilium (CNI) 本体の話ではない。

---

## 1. cilium/ebpf から bpf_arena を使うときの課題

### 1.1 課題の一覧

| # | 課題 | 症状 | この PoC の回避策 | 今の状態 |
|---|---|---|---|---|
| C1 | `addr_space_cast` の Marshal バグ | `Instruction.Marshal` が plain な `Mov` の `Offset` を 0 に上書きし、arena のポインタ変換が `BPF_MOV uses reserved fields` で verifier に拒否される | Week 2 に cilium/ebpf を vendor してパッチ (`../misc/cilium-ebpf-addr-space-cast-marshal.patch`) | **この PoC の作者 (Masumi Kanai) が upstream に出して取り込まれた修正。** cilium/ebpf の main の commit `cc4947e` ("asm: conditionally set ins.Offset when marshaling Movs"、2026-09-24 作成、Timo Beckers が commit)。`go.mod` が指す commit (2026-10-08) はこれを含むので vendor は外した。タグ付きリリースにはまだ入っていない (2026-10-24 時点、`git tag --contains` が空) |
| C2 | load の順番 | arena を mmap して `user_vm_start` が決まる前に program を load すると、`addr_space_cast` が拒否される (`arena's user address must be set via map_extra or mmap()`)。`LoadAndAssign` は map の作成と program の load を一度にやるので、間に mmap を挟めない | arena の map だけ先に `ebpf.NewMap` で作り、mmap してから `CollectionOptions.MapReplacements` で渡す 2 段の load (`daemon/internal/ebpf/loader.go` の `Load` / `LoadRejectDemo`) | 回避策のまま |
| C3 | base 番地が取れない | `Map.Memory()` は Arena にも対応したが、mmap した先頭番地を返す API がない。BPF 側でポインタを組み立てるには user_vm の base が要る | `unix.Mmap` を自前で呼ぶ (`daemon/internal/arena/arena.go`)。base は syscall program の ctx (`arena_base_uaddr`) で BPF に渡す | 回避策のまま |
| C4 | `__arena` のグローバル変数が使えない | libbpf は `.addr_space.1` セクションの `__arena` グローバル変数を arena に置いて skeleton から見せるが、cilium/ebpf の ELF reader はこのセクションを扱わない (`addr_space` の記述がない) | arena 上のデータは「base + offset」で毎回組み立てる。リストの先頭も ctx で渡す | 回避策のまま |
| C5 | verifier のログが切れる | `VerifierError.Error()` は 1 行に切り詰める。arena や `addr_space_cast` の拒否理由が見えない | エラー経路だけ `%+v` で全文を出す (`loader.go`) | 回避策で足りる。`%+v` は以前からある正規の出し方で、main (2026-10-08) に `%+v` / `%-10v` / `%d` の使い分けの文書 (`docs/ebpf/concepts/verifier-errors.md`) が入った |
| C6 | clang の版を検出しない | clang 18 以下は `__BPF_FEATURE_ADDR_SPACE_CAST` を定義せず、`__arena` が普通のポインタに落ちたままビルドが通る。load して初めて `invalid mem access 'scalar'` になる | `make daemon-gen` が clang 19 未満を拒否、`TestEmbeddedObjectHasArenaCasts` がコミット済みの `.o` に cast があるかを見る (`daemon/internal/ebpf/arena_cast_test.go`) | 回避策のまま (bpf2go 側の問題) |
| C7 | arena の map を参照しないと関連付かない | program が arena の map を一度も参照しないと verifier が arena を関連付けず、`addr_space_cast` を拒否する | `asm volatile("" :: "r"(&s3fs_arena));` で参照を作る (`bpf/s3fs_fastpath.bpf.c`) | 回避策のまま (カーネル側の仕様) |
| C8 | `vmlinux.h` に `BPF_MAP_TYPE_ARENA` がない | pahole が作った `vmlinux.h` の enum が途中で切れていた | 自分で `#define BPF_MAP_TYPE_ARENA 33` | 回避策のまま (toolchain) |
| C9 | サイズは作成時に固定 | `max_entries` (ページ数) は map を作った後は伸ばせない。128 MiB に 100 MiB のファイルが入ると他が admit できなかった | 中身を arena に置くのをやめ、page cache に戻した (pcache モード) | 設計で回避 |

### 1.2 cilium/ebpf の外にある、より大きい課題

cilium/ebpf の問題を全部直しても、この PoC の性能の問題は解けない。arena の「ゼロコピー」は BPF とユーザー空間の間の話で、kmod (普通のカーネル C) から見た arena は daemon のユーザーメモリになる。

| # | 課題 | 回避策 |
|---|---|---|
| A1 | kern_vm に BPF 以外のコンテキスト向けの fault handler がない | kmod は GUP (`pin_user_pages_remote` + `kmap_local_page`) で user_vm を読む。1 回約 1.7 µs |
| A2 / A3 | `struct bpf_arena` も `bpf_arena_get_kern_vm_start()` も export されていない | BTF から 6.12 のレイアウトを複製 (`kmod/s3fs_ebpf_dev.c`)。使うのは起動時の確認 (`PROBE_KVBASE`) だけ |

詳細は [`ebpf-ecosystem-gaps.md §1`](ebpf-ecosystem-gaps.md) と `../user/meetup-qa.md` Q10 / Q55。

### 1.3 使うときのチェックリスト (cilium/ebpf + arena)

1. clang 19 以上、`-mcpu=v3` 以上でビルドし、`.o` に `addr_space_cast` (ALU64 MOV|X、Offset=1) があるかテストで確かめる。
2. cilium/ebpf は `addr_space_cast` の Marshal 修正が入った版を使う (この PoC は 2026-10-08 の commit)。
3. arena の map を先に作って mmap し、`MapReplacements` で渡してから load する。
4. base 番地は自前の mmap で持ち、BPF には ctx か map で渡す。
5. 拒否されたら `VerifierError` を `%+v` で出す。
6. 読むのが BPF とユーザー空間だけか確かめる。kmod や KFunc が別プロセスの文脈から読むなら GUP になる (スライド `arena-who` / `arena-loop`)。

---

## 2. struct_ops の使い方

### 2.1 一般的な形

struct_ops は、カーネル (またはカーネルモジュール) が定義した関数ポインタの表 (vtable) を、BPF の program で実装して差し込む仕組み。

- **向く場面:** カーネルが「次にどれを選ぶか」を問い合わせる点があり、その単位がアルゴリズムの判断の単位と合うとき。例は sched_ext (`sched_ext_ops`、次に走らせるタスク)、TCP 輻輳制御 (`tcp_congestion_ops`、cwnd)、HID-BPF。
- **良いところ:** vtable 全体を 1 回の `BPF_LINK_UPDATE` で差し替えられる (中で RCU のポインタを 1 回付け替えるだけ)。fentry / fmod_ret を個別に付け替えると、新旧の hook が混ざる時間ができる。
- **制約:** 呼ばれる側は通常 RCU の下で、sleep できない (sleepable な struct_ops は 6.9 以降で、vtable の定義側の対応が要る)。状態は BPF の map に持つ。

### 2.2 この PoC での使い方 (3 か所)

| 場所 | やること | コード |
|---|---|---|
| kmod | `struct s3fs_policy_ops` (on_admit / on_evict / on_pressure / on_hit / on_insert / on_remove / pick_victim / on_size) を定義し、`struct bpf_struct_ops` に `verifier_ops` (`get_func_proto` / `is_valid_access`)、`init`、`init_member`、`reg` / `unreg` / `update`、`cfi_stubs` (member ごとの stub) を揃えて `register_bpf_struct_ops()` で登録する。呼び出し側は `rcu_read_lock()` + `rcu_dereference()` で今の vtable を取って呼ぶ | `kmod/policy_struct_ops.{c,h}` |
| BPF | vtable と同じ形の `struct s3fs_policy_ops` を BPF 側でも定義する (モジュールの型は `vmlinux.h` にない)。member ごとに `SEC("struct_ops/<member>")` + `BPF_PROG` で書き、`SEC(".struct_ops.link")` の変数に並べる。状態 (FIFO、hand、3 つの queue など) は HASH / ARRAY map に置く | `bpf/policy_*_pick_ops.bpf.c` |
| daemon | `ebpf.LoadCollection` で読み、`StructOpsMap` 型の map を `link.AttachStructOps` で attach する。hot swap は既存の link に `BPF_LINK_UPDATE` (`BPF_F_REPLACE`、新旧の map fd) を raw syscall で発行する。失敗したら「新しい方を attach してから古い方を閉じる」に戻る | `daemon/internal/policyprog/loader.go` の `attachStructOps` / `SwapFrom` / `updateStructOpsLink` |

### 2.3 実装で効いた決まりごと

1. **vtable の粒度は判断の単位に合わせる。** エントリごとに 0/1 を聞く形では SIEVE の hand や LRU の古さが伝わらず、simulator より 9〜13 pt 低かった。「どれを追い出すか」を聞く `pick_victim` を足して直した (`../design/ac58-pick-victim.md`)。
2. **member は末尾に足す。** BPF 側は member 名で結び付くので、古い 4 member の program もそのまま load できる。
3. **`reg` / `unreg` は重なりを前提に書く。** hot swap の fallback では新しい vtable の `reg` が先に走るので、`unreg` は「今のポインタが自分のときだけ」消す。
4. **hot path の member で map を update しない。** `on_hit` を「lookup + 1 ビットの store」に変えて、64 jobs の read のコストが −14.8% → −4.6%。
5. **BPF が返した値は検証する。** kmod は `pick_victim` が返した ino が候補にあるか確かめ、なければ `on_remove` で忘れさせて聞き直す。
6. **直列化は kmod 側で。** `on_insert` / `on_remove` / `pick_victim` は `s3fs_policy_evict_lock` の下で呼ぶので、BPF 側はそれらだけが触る状態にロックが要らない。
7. **sleep が要る処理は入れない。** S3 への問い合わせなどは miscdev 経由の upcall で daemon に返す (`../user/meetup-qa.md` Q25)。

### 2.4 cilium/ebpf で struct_ops を使うときの課題

| # | 課題 | 回避策 |
|---|---|---|
| S1 | `structOpsLink.Update` が常に `ErrNotSupported` (`*ebpf.Program` しか受けず、map を差し替える形がない) | `BPF_LINK_UPDATE` を raw syscall で呼ぶ |
| S2 | struct_ops の docs がほぼない。SEC の名前と attach type の対応、link の update の意味は sched_ext の例やソースから逆算した | この doc と `../design/policy-attach-comparison.md` |
| S3 | kmod 側の glue が多い。fmod_ret 版の約 2.3 倍の行数 (`ebpf-ecosystem-gaps.md §2`) | なし (カーネル側の課題) |

---

## 3. upstream への要望

| 先 | 要望 | 根拠 | 状態 |
|---|---|---|---|
| Linux | K-01: `bpf_arena_get_kern_vm_start()` を `EXPORT_SYMBOL_GPL` にする | A2 / A3 | 未提出 (`../journal/backlog.md` §K) |
| Linux | K-02: kern_vm に BPF 以外のコンテキスト向けの fault handler を足す | A1。入れば kmod から GUP なしで読める | 未提出 |
| Linux | `struct bpf_arena` の最小の accessor (kern_vm の start、size) | K-01 が通らなかったときの代案 | 未提出 |
| Linux | struct_ops の glue (`cfi_stubs` / `init_member` / `verifier_ops`) を生成するマクロ、外部モジュールの最小例を selftests に | S3 | 未提出 |
| cilium/ebpf | `addr_space_cast` の Marshal 修正 | C1 | **提出済み・main に取り込み済み** (この PoC の作者による `cc4947e`)。リリース待ち。回帰テストの追加は未 |
| cilium/ebpf | arena の map を作成時に mmap する (または load の前に mmap する手段を用意する) | C2 | 未提出 |
| cilium/ebpf | `Memory` から mmap 先の番地を取れるようにする | C3 | 未提出 |
| cilium/ebpf | `.addr_space.1` (`__arena` グローバル変数) に対応する | C4 | 未提出 |
| cilium/ebpf | struct_ops link の map の差し替え (`Update` の map 版) | S1 | 未提出 |
| cilium/ebpf | struct_ops と arena の docs | S2 | 未提出 |
| cilium/ebpf | `VerifierError` の全文を出す手段を分かりやすくする | C5 | 不要になった (main に文書が入った、2026-10-24 確認) |

libbpf が arena の map を作成時に mmap すること、`__arena` グローバル変数に対応していることは知識からの説明で、この PoC では確かめていない。
