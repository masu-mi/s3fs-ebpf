# struct_ops に関数ポインタ以外のメンバ (データ) を載せる

2026-11-03 時点のメモ。「struct_ops は単なる構造体として、関数ポインタ以外 (フラグ、名前、数値) も登録できるか」の調査。結論と、カーネル 6.12 から 7.3 までの変化、このリポジトリ (`kmod/policy_struct_ops.c` の `struct s3fs_policy_ops`) で使うときの手順をまとめる。関連: [`struct-ops-kmod-glue.md`](struct-ops-kmod-glue.md) (kmod が struct_ops を提供するときの glue)。

## 1. 結論

- **できる。** ただし**型ごとの `init_member` が、そのメンバを明示的に受け取ったときだけ**。`init_member` が 0 を返したメンバのうち、関数ポインタ以外は**すべて 0 でなければ登録が `-EINVAL` で失敗する**。
- 受け取るとは、`init_member` が `udata` (BPF 側が書いた値) を検査して `kdata` (カーネル側のコピー) に書き、`1` を返すこと。in-tree の例は `tcp_congestion_ops` の `flags` / `name[]`、`sched_ext_ops` の `flags` / `name[]` / `timeout_ms` など。
- **関数ポインタ以外のポインタ** (データへのポインタ) は渡せない (0 のみ)。値はスカラー、配列 (文字列)、入れ子の struct。ビットフィールドと無名メンバは拒否される。
- **値は load 時に固定される。** 実行時に変えたい状態は BPF map か kfunc に持たせる。
- **`BPF_LINK_UPDATE` で vtable ごと入れ替わるので、データメンバも関数ポインタと一緒に原子的に切り替わる。**
- このリポジトリでは `flags` と `name` を実装し、KUnit と実機で確かめた (§4)。
- この仕組み (メンバの処理順と規則) は **6.12 から 7.3 まで変わっていない** (`bpf_struct_ops.c` の diff で確認)。周辺の追加は §5。

## 2. カーネルの仕組み (`kernel/bpf/bpf_struct_ops.c`、`bpf_struct_ops_map_update_elem`)

struct_ops の map に vtable を書く (daemon が map を作って load するとき) と、型 (BTF の struct) の**メンバごと**に次の順で処理される。

| 順 | 処理 | 結果 |
|---|---|---|
| 1 | メンバが `struct module *` | 0 でなければ失敗。カーネル側に `THIS_MODULE` 相当 (`BPF_MODULE_OWNER`) を入れて次へ |
| 2 | `st_ops->init_member(t, member, kdata, udata)` | **`< 0`**: 登録失敗。**`> 0`**: その型が処理した (値は `kdata` に入っている) ので次のメンバへ。**`0`**: 3 へ |
| 3 | 関数ポインタ以外 (`ptype` が func proto でない) | **`udata` のそのメンバがすべて 0 でなければ `-EINVAL`** (`memchr_inv`)。0 なら次へ |
| 4 | 関数ポインタ | BPF プログラムの fd から trampoline を作り、`kdata` に入れる |

全メンバの処理後に `st_ops->validate(kdata)` が呼ばれ (組み合わせの検査に使える)、そのあと `reg` (link の作成) か `update` (`BPF_LINK_UPDATE`) が呼ばれる。`kdata` は struct_ops map の中の値で、trampoline の page は `arch_protect_bpf_trampoline` で読み取り専用になる。load 後にデータメンバを書き換える正規の手段はなく、変えるときは新しい vtable (map) を作って `BPF_LINK_UPDATE` で入れ替える。

`init_member` の引数のうち、`t` は値の struct の BTF 型、`member` は処理中のメンバ。メンバの位置は `moff = __btf_member_bit_offset(t, member) / 8` で求め、`offsetof(struct xxx_ops, field)` と `switch` で比べる。

### 2.1 in-tree の例 (6.12 のソースで確認)

`net/ipv4/bpf_tcp_ca.c` の `bpf_tcp_ca_init_member`:

```c
switch (moff) {
case offsetof(struct tcp_congestion_ops, flags):
	if (utcp_ca->flags & ~TCP_CONG_MASK)
		return -EINVAL;               /* 値の検査 */
	tcp_ca->flags = utcp_ca->flags;
	return 1;                             /* 処理した */
case offsetof(struct tcp_congestion_ops, name):
	if (bpf_obj_name_cpy(tcp_ca->name, utcp_ca->name, sizeof(tcp_ca->name)) <= 0)
		return -EINVAL;
	return 1;
}
return 0;                                      /* それ以外は既定の規則 */
```

`kernel/sched/ext.c` の `bpf_scx_init_member` (6.12) は `dispatch_max_batch`、`flags`、`name`、`timeout_ms`、`exit_dump_len`、`hotplug_seq` の 6 つを受け取る。値の範囲の検査 (`-E2BIG`、`-EINVAL`) と、未指定のときの既定値 (`exit_dump_len`) の補完もここで行う。

BPF 側は、初期化子に値を書くだけ:

```c
SEC(".struct_ops.link")
struct tcp_congestion_ops bbr = {
	.flags = TCP_CONG_NEEDS_ECN,
	.name  = "my_bbr",
	.cong_avoid = (void *)my_cong_avoid,
};
```

## 3. 制約

- 関数ポインタ以外のポインタは 0 のみ。ビットフィールドと無名メンバは `bpf_struct_ops_desc_init` (型の登録時) に拒否される。
- 値は load 時の定数。実行時の可変状態は BPF map や kfunc に置く (このリポジトリの `wtinylfu_config` などの配列 map がその形)。
- 型 (`struct s3fs_policy_ops`) は kmod の BTF にあり、BPF のソースは同じ形の struct を自前で宣言する (kmod の型は `vmlinux.h` にない)。libbpf はメンバを**名前**で合わせるので、BPF 側は先頭の一部だけを宣言してもよく、宣言しなかったメンバは 0 になる (このリポジトリの policy は 4 メンバだけを宣言している)。
- 関数ポインタの戻り値が 8 byte を超える型は 7.3 から `-EOPNOTSUPP` (§5)。

## 4. このリポジトリでの実装と使い方 (確定)

`struct s3fs_policy_ops` (`kmod/policy_struct_ops.h`) は、hook (関数ポインタ 8 個) の後ろに**データメンバ 2 個**を持つ。

| メンバ | 型 | 意味 | 受け取るときの検査 (`s3fs_policy_init_member`) |
|---|---|---|---|
| `flags` | `u32` | `S3FS_POLICY_F_*`。今は `S3FS_POLICY_F_NO_ON_HIT` (この policy は `on_hit` を持たない) の 1 bit | 未知の bit があれば `-EINVAL` |
| `name` | `char[16]` | policy の名前 (`[A-Za-z0-9_.]`、NUL 終端)。`""` は名前なし | 文字の検査 (`bpf_obj_name_cpy` と同じ規則。あれは module に export されていないので自前)、NUL がなければ `-EINVAL` |

- **どちらも省略できる。** 末尾に足したので、4 メンバや 8 メンバだけを宣言した既存の policy は、そのまま load できる (宣言されないメンバは 0 / 空)。libbpf はメンバを名前で合わせる。
- **`validate`** (`s3fs_policy_validate`) は、全メンバが入った後、`reg` / `update` の前に 1 回:
  - `pick_victim` があるなら `on_insert` と `on_remove` も必須 (AC-58、AC-64: `pick_victim` が返す ino の一覧が、kmod が入れた / 追い出したものを知らないまま古くなる)。
  - `S3FS_POLICY_F_NO_ON_HIT` と `on_hit` を同時に持てない (宣言と vtable を食い違わせない)。
- **使われ方**: `reg` と `update` が dmesg に `name=… flags=0x…` を出す (`policy struct_ops registered (ops=… name=observe flags=0x0)`、`policy struct_ops updated (hot swap) name=named_ok flags=0x1`)。`BPF_LINK_UPDATE` で vtable ごと入れ替わるので、名前と flags も hook と同時に切り替わる。
- **`flags` は今は宣言と検査だけ**で、read の経路の最適化 (`on_hit` の呼び出しや hit の集計を省く) には使っていない。hit の集計 (issue #22 の `hit_accum`) の不変条件を変えるので、別の課題にした (backlog AC-77)。

### 4.1 コード (実装そのもの)

```c
/* kmod/policy_struct_ops.c */

/* name: [A-Za-z0-9_.]*, NUL 終端。"" は許す (name がなかった頃の policy は空のまま) */
static int s3fs_policy_copy_name(char *dst, const char *src)
{
	size_t n = strnlen(src, S3FS_POLICY_NAME_MAX);
	size_t i;

	if (n == S3FS_POLICY_NAME_MAX)	/* NUL がない */
		return -EINVAL;
	for (i = 0; i < n; i++)
		if (!isalnum(src[i]) && src[i] != '_' && src[i] != '.')
			return -EINVAL;
	memset(dst, 0, S3FS_POLICY_NAME_MAX);
	memcpy(dst, src, n);
	return 0;
}

static int s3fs_policy_init_member(const struct btf_type *t, const struct btf_member *member,
				   void *kdata, const void *udata)
{
	const struct s3fs_policy_ops *uops = udata;
	struct s3fs_policy_ops *ops = kdata;
	u32 moff = __btf_member_bit_offset(t, member) / 8;
	int err;

	switch (moff) {
	case offsetof(struct s3fs_policy_ops, flags):
		if (uops->flags & ~S3FS_POLICY_F_ALL)
			return -EINVAL;
		ops->flags = uops->flags;
		return 1;			/* 受け取った */
	case offsetof(struct s3fs_policy_ops, name):
		err = s3fs_policy_copy_name(ops->name, uops->name);
		return err ? err : 1;
	}
	return 0;				/* 関数ポインタなどはフレームワークに任せる */
}

static int s3fs_policy_validate(void *kdata)
{
	const struct s3fs_policy_ops *ops = kdata;

	if (ops->pick_victim && (!ops->on_insert || !ops->on_remove))
		return -EINVAL;
	if ((ops->flags & S3FS_POLICY_F_NO_ON_HIT) && ops->on_hit)
		return -EINVAL;
	return 0;
}

/* struct bpf_struct_ops bpf_s3fs_policy_ops = { ..., .init_member = s3fs_policy_init_member,
 *                                               .validate = s3fs_policy_validate, ... }; */
```

BPF 側 (`bpf/policy_observe_ops.bpf.c`): データメンバを設定するには、その前のメンバまで**同じ並びで宣言**する (libbpf は名前で合わせるが、宣言したメンバの途中は飛ばせない)。値は初期化子の定数。

```c
#define S3FS_POLICY_NAME_MAX 16
struct s3fs_policy_ops {
	int (*on_admit)(__u64 count, __u64 arena_used_hint);
	int (*on_evict)(__u64 ino);
	int (*on_pressure)(__u64 count, __u64 threshold);
	int (*on_hit)(__u64 ino);
	int (*on_insert)(__u64 ino);
	int (*on_remove)(__u64 ino);
	__u64 (*pick_victim)(void);
	int (*on_size)(__u64 ino, __u64 bytes);
	__u32 flags;
	char name[S3FS_POLICY_NAME_MAX];
};

SEC(".struct_ops.link")
struct s3fs_policy_ops observe_ops = {
	.on_admit = (void *)observe_admit,
	/* … */
	.name     = "observe",              /* データメンバ: 定数 */
};
```

### 4.2 試験

| 試験 | 何を確かめるか |
|---|---|
| KUnit `s3k_pol_data_members_taken` / `_refused` (`kmod/policy_struct_ops_kunit.c`) | `init_member` に fake の `btf_type` / `btf_member` を渡し、`flags` と `name` を受け取る (1 を返し `kdata` に入る)、未知の bit・不正な文字・NUL なし・無名を拒否する (`-EINVAL`)、関数ポインタのオフセットには 0 を返す |
| KUnit `s3k_pol_validate` | `pick_victim` の組み合わせ、`NO_ON_HIT` と `on_hit`、空の vtable、既存の vtable が通ること |
| KUnit `s3k_pol_data_swap_with_vtable` | `reg` / `update` で、名前と flags が vtable と一緒に入れ替わる |
| 実機 `make test-policy-data-members` (`scripts/test-policy-data-members.sh`) | 実カーネルで BPF の初期化子 → libbpf → `bpf()` → `init_member` → `validate` → `reg` / `update` の全経路。`policy_observe_ops` の起動、`policy_test_named_ops` (name + flag) への reload が 200 で dmesg に名前と flags が出ること、4 つの拒否用 fixture (`policy_test_badflags_ops`、`_badname_`、`_noonhit_hit_`、`_pickonly_`) が `update: invalid argument` で拒否され、hot swap が起きず、link が生きていて read が通ること |

- **検査を外すと試験が落ちることを確認した**: kmod の `init_member` と `validate` の 4 つの検査を外してビルドすると、KUnit は 3 件失敗し、実機の試験は 4 つの拒否用 fixture がすべて受理されて失敗した。戻すと PASS。
- 実機の試験は拒否の**理由**も見る (`invalid argument`)。最初は `noonhit_hit` が、vtable に結びつかない未使用のプログラムのせいで別の理由 (`load program: operation not supported`) で拒否されていて、`validate` に届いていなかった。struct_ops のオブジェクトは、中のプログラムがすべて member に結びついていないと load 自体が失敗する。fixture の冒頭にこの注意を書いた。

### 4.3 使い道の案 (未実装)

| 案 | 内容 |
|---|---|
| `flags` による hot path の省略 | `NO_ON_HIT` の policy では `hit_accum` の集計と `on_hit` の呼び出しを省く (backlog AC-77。`hit_accum` の不変条件を変える) |
| `abi_version` | vtable の意味が変わったときに `validate` で不一致を拒否する |
| 容量や閾値の既定値 | policy ごとの `softcap` の既定など。ただし**実行時に変えたい値は map** (daemon が書ける) |

## 5. 新しいカーネルでの変化 (6.12 → 7.3)

`kernel/bpf/bpf_struct_ops.c`、`include/linux/bpf.h`、`include/uapi/linux/bpf.h`、`kernel/sched/ext.c` を各タグ (v6.12、v6.13、…、v7.3) から取得して差分を読んだ。**`struct bpf_struct_ops` の定義 (16 メンバ: `verifier_ops`、`init`、`check_member`、`init_member`、`reg`、`unreg`、`update`、`validate`、`cfi_stubs`、`owner`、`name`、`func_models` ほか) は 6.12 から 7.3 まで一行も変わっていない。メンバの処理順と規則 (§2) も同じ。**

| 版 | 変更 | このリポジトリへの意味 |
|---|---|---|
| 6.13 | trampoline ごとに ksym (`bpf__<struct>_<メンバ>`) を登録 | perf や stack trace に policy の関数名が出る。コードの変更は不要 |
| 6.14 | `struct module` を持つ型で、BTF に `module` がない (`CONFIG_MODULES` なし) と `-EOPNOTSUPP` | 影響なし (kmod は module) |
| 6.15 | **stub 関数の見つけ方が変わった**: 名前の規約 (`foo_ops__bar`) をやめ、`cfi_stubs` の関数のアドレスを kallsyms で引き、その名前を BTF から探す。**見つからないと `-ENOENT` で登録失敗** (6.14 までは黙ってスキップ)。stub が NULL のメンバは `bpf_struct_ops_supported()` で型の登録時にスキップされる (この関数は 6.12 からあるが、使う場所が増えた) | **static の stub でも BTF に入っていれば動く**。手元の 6.12 ビルドで、モジュールの BTF に `__s3fs_stub_*` の 8 本が `linkage=static` で入っていることを確認した (`bpftool btf dump file /sys/kernel/btf/s3fs_kfunc -B /sys/kernel/btf/vmlinux`)。pahole が stub を落とす構成では、6.15 以降は登録が失敗し、kmod の読み込みも失敗する。オプションの hook は stub を NULL にして「未対応」と宣言できる |
| 6.15 | stub の引数名に `__ref` (参照カウント付き引数) を追加 (`__nullable` に続く) | 影響なし |
| 6.18 | `bpf_struct_ops_id(kdata)` を export (struct_ops map の id) | `reg` / `update` で「どの vtable か」を id で識別できる |
| 6.19 | `bpf_struct_ops_get()` / `bpf_struct_ops_put()` を export | kmod が vtable (`kdata`) の参照を保持できる (RCU のポインタに頼らない設計も可能) |
| 7.0 | **`BPF_PROG_ASSOC_STRUCT_OPS`** (syscall) と `bpf_prog_get_assoc_struct_ops(aux)`: BPF プログラムを struct_ops map に関連付け、kfunc から**呼び出し元プログラムに関連付けた struct_ops の `kdata`** を引ける。struct_ops 以外のプログラム型 (tracing など) も関連付けられる。状態 (初期化済みか、attach 済みか) の追跡は実装者の責任 | データメンバ (`flags`、`name` など) を、kfunc から呼び出し元の policy ごとに読める。複数の policy を同時に attach する設計の土台になりうる (推測。このリポジトリは今 policy を 1 つだけ attach する) |
| 7.1 | 関連付けの時点を link の確保後に移動 (7.0 の修正) | 影響なし |
| 7.2 | **`bpf_struct_ops_for_each_prog(kdata, cb, data)`** を export。`reg()` の中で member の BPF プログラムを列挙し、`prog->aux->used_maps` から参照している map を見つけられる。trampoline の内部表現 (`tlinks` → `tnodes`) の変更 | kmod が policy の BPF map (`policy_stats` など) を、vtable から直接たどれる |
| 7.3 | stub の引数に **`__arena` / `__arena__nullable`** を追加 (arena のポインタを callback の引数にできる)。**戻り値が 8 byte を超える関数ポインタは `-EOPNOTSUPP`** | hook の引数に arena のポインタを渡す設計が可能になる (このリポジトリは今、arena を kmod から `copy_from_user` で読んでいる、`docs/dev-notes/arena-access-paths.md`)。戻り値は今の vtable (`int` / `u64`) で問題ない |

6.16 / 6.17 の変更は内部 (trampoline の大きさの計算に stub を渡す、`bpf_link_init` に attach type を渡す) で、struct_ops を提供する側のコードには影響しない。

### 5.1 このリポジトリの最低要件との関係

kmod の最低要件は 6.12 のまま (`kmod/s3fs_ebpf_dev.c` が 6.12 未満を `#error`)。上の表のうち **6.15 の stub の BTF 検索だけが、より新しいカーネルで動かすときの確認項目** (kmod が登録に失敗すると読み込みも失敗する)。ほかは、使えるカーネルなら使える追加機能。6.13 以降では `MODULE_IMPORT_NS(BPF_INTERNAL)` が文字列リテラルを取る (`docs/dev-notes/kunit.md` の build の問題 7)。

## 6. 調べた範囲と、未検証のこと

- **ソースで確認**: 上記の版ごとの `bpf_struct_ops.c` の差分 (v6.12〜v7.3)、`struct bpf_struct_ops` の定義、`bpf_tcp_ca_init_member`、`bpf_scx_init_member` (6.12 のみ。7.3 では `ext.c` の場所が変わっていて取得できなかった)。手元の 6.12 の tree と取得した v6.12 のファイルは一致。
- **手元の 6.12 の実機で確認**: §4.2 のとおり、データメンバの登録、拒否 (init_member と validate)、hot swap で名前と flags が切り替わること。モジュールの BTF に stub 8 本があること (§5)。KUnit は UML と qemu (`-smp 4`) で通る。
- **未検証**:
  - cilium/ebpf から struct_ops map の初期値 (データメンバ) を load 時に書き換える方法 (libbpf の skeleton では load 前に `skel->struct_ops.<名前>-><メンバ>` を書き換えられるが、daemon の Go 側で同じことができるかは調べていない。今は BPF のソースの定数で設定している)。
  - 6.15 以降の実機での kmod の読み込み (今回の環境は 6.12)。
  - 7.0 以降の `BPF_PROG_ASSOC_STRUCT_OPS` の実際の使い方 (syscall の引数と、kfunc 側の実装例)。
