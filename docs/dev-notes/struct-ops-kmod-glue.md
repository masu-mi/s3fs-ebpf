# struct_ops を kmod から提供するときの glue: 何が、なぜ要るか

2026-10-26 時点のメモ。対象は `kmod/policy_struct_ops.c` / `policy_struct_ops.h` (`struct s3fs_policy_ops`、8 member)。「なぜ要るか」は v6.12 の `kernel/bpf/bpf_struct_ops.c` と `include/linux/bpf.h` で確認した。行数の比較は [`../design/policy-attach-comparison.md`](../design/policy-attach-comparison.md) §1.1。デッキの付録 `appx-so-glue` はこのメモを 1 枚にしたもの。

## 1. なぜ fmod_ret より glue が多いのか

| 方式 | kmod 側の基盤 | 何を kmod が持つか |
|---|---:|---|
| fmod_ret | 約 15 行 (BTF の ID セット + `register_btf_fmodret_id_set`) | 何もしない関数を置くだけ。attach、trampoline、detach は tracing の仕組みがやる |
| struct_ops | 約 220 行 (`policy_struct_ops.{c,h}`、比較時点) | vtable の型、その型を BPF に見せる設定、vtable を受け取って公開・差し替えする処理、呼び出し側 |

fmod_ret は「既存の関数の戻り値を上書きする」ので、呼び先はもともと kmod の関数で、BPF は既存の tracing の仕組みで横に付くだけ。struct_ops は「kmod が呼ぶ関数ポインタの表そのもの」を BPF から受け取るので、表の型、表が来たとき・消えたとき・差し替わったときの処理、表を読む呼び出し側を、すべて kmod が書く。差し替えがアトミックになる (`BPF_LINK_UPDATE` 1 回でポインタを 1 回付け替える) のは、この処理を kmod が持っているからでもある。

## 2. 部品ごとの説明

### 2.1 vtable の型と `register_bpf_struct_ops(&bpf_s3fs_policy_ops, s3fs_policy_ops)`

- やること: `struct s3fs_policy_ops` と、マクロが作る `struct bpf_struct_ops_s3fs_policy_ops` (共通ヘッダ + vtable) を BTF に出して登録する。
- なぜ: `bpf_struct_ops_desc_init()` は、型名 `s3fs_policy_ops` と値の型名 `bpf_struct_ops_s3fs_policy_ops` をモジュールの BTF から引く。見つからなければ `-EINVAL`。BPF 側の `SEC(".struct_ops.link")` の変数も、この BTF の型名と member 名で照合される。なのでモジュールの BTF (`CONFIG_DEBUG_INFO_BTF_MODULES`、pahole) が要る。
- BPF 側への波及: モジュールの型は `vmlinux.h` に無いので、BPF のソースに同じ形の struct を書き直している。

### 2.2 `cfi_stubs` (member ごとの空関数 8 個 + それを並べた表)

- やること: member と同じ型の、何もしない関数を用意して表にする。実際には呼ばれない。
- なぜ:
  - `cfi_stubs` が NULL だと `bpf_struct_ops_desc_init()` が `-EINVAL` を返す (必須)。
  - 各 member の trampoline は、対応する stub の関数ポインタを渡して作られる (`bpf_struct_ops_prepare_trampoline()`)。kCFI が有効なカーネルでは、間接呼び出しの型チェックに使う型の情報をここから取る。
  - stub が NULL の member は `bpf_struct_ops_supported()` で非対応 (`-ENOTSUPP`) になる。member を足すときは stub も足す必要がある。
  - kCFI の仕組みと trampoline へのハッシュの写し方は [`kcfi-basics.md`](kcfi-basics.md)。

### 2.3 `verifier_ops` (`get_func_proto` と `is_valid_access`)

- やること: struct_ops の BPF prog が呼べる helper と、引数 (ctx) の読み方を決める。ここでは `bpf_base_func_proto()` と `bpf_tracing_btf_ctx_access()` を返すだけ。
- なぜ: struct_ops の prog を検査するとき、verifier は `env->ops = st_ops->verifier_ops` として、その vtable の `verifier_ops` をそのまま使う。モジュールが用意しないと検査できない。

### 2.4 `init` / `init_member`

- やること: `init` は登録時に 1 回、`init_member` は daemon が vtable を map に書いたとき member ごとに呼ばれる。どちらも 0 を返すだけ。
- なぜ: フレームワークが NULL チェックなしで呼ぶので、中身が無くても書く必要がある。関数ポインタ以外の member (設定値など) を持つ vtable なら、`init_member` でユーザー側の値をカーネル側にコピーする。

### 2.5 `reg` / `unreg`

- やること: link の作成時に `s3fs_policy_active` (RCU のポインタ) に vtable を置き、破棄時に外す。
- なぜ: フレームワークが渡すのは「BPF の関数が入った vtable の実体 (`kdata`)」だけで、それを kmod のどの変数に置き、呼び出し側がどう読むかは kmod にしか分からない。呼び出し側は並行に走っているので、`rcu_assign_pointer` で公開し、外すときは `synchronize_rcu()` で読み手がいなくなるのを待つ。
- 落とし穴: hot swap の fallback (新しい vtable を attach してから古い link を close) では、新しい `reg` が先に走る。古い `unreg` が無条件にポインタを NULL にすると新しい vtable まで外れるので、今のポインタが自分のときだけ消す。

### 2.6 `update`

- やること: `BPF_LINK_UPDATE` で、ポインタを新しい vtable に 1 回で付け替える。
- なぜ: 無いと `BPF_LINK_UPDATE` が `-EOPNOTSUPP` になる (任意の部品)。fmod_ret には link の update が無く、差し替えは「新しいのを N 個 attach → 古いのを N 個 detach」になり、その間は新旧が混ざる。struct_ops を選んだ理由そのもの。
- daemon 側: cilium/ebpf の `Link.Update` は struct_ops に対応していない (`ErrNotSupported`) ので、`BPF_LINK_UPDATE` を raw syscall で呼んでいる。

### 2.7 呼び出し側の wrapper (`s3fs_policy_ops_eval_*` など)

- やること: `rcu_read_lock()` → `rcu_dereference(s3fs_policy_active)` → NULL なら既定値、あれば member を呼ぶ。member ごとに 1 関数。
- なぜ: vtable は attach 前や detach 後には無い。fmod_ret なら kmod の関数を普通に呼べば済むが、struct_ops では毎回「今の vtable」を RCU で読んで NULL を確かめる必要がある。member は RCU の読み側で呼ばれるので、BPF 側で sleep はできない。

## 3. まとめ

- struct_ops の glue は、kmod が「呼び先の型」と「付け替え」を持つことの代償。そのおかげで vtable ごとのアトミックな差し替えができる。
- 差し替えが要らないなら、fmod_ret や kfunc のほうが kmod の負担はずっと小さい。デッキの採用基準 (`adopt-so`) ではこれを「避ける」に入れている。
- vtable の単位 (kmod が何を問い合わせるか) は、自分で kmod を書くなら使うと決めた後の設計の問題で、`pick_victim` のように直せる。既存の struct_ops (例: `tcp_congestion_ops`) を使う場合は変えられないので、採用の判断に入る。
