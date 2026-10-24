# arena のポインタはどこで kern_vm の番地になるか (6.12 / 7.2 / 7.3)

2026-10-24 時点のメモ。根拠は v6.12 / v7.2 / v7.3 のタグのソース (`kernel/bpf/arena.c`, `kernel/bpf/verifier.c`, `kernel/bpf/fixups.c`, `kernel/bpf/btf.c`, `arch/x86/net/bpf_jit_comp.c`, `arch/arm64/net/bpf_jit_comp.c`)。**実機では試していない。** 行番号は v7.3 のもの。

## 1. 前提: 番地の対応

arena を作ると、カーネルの仮想アドレスに 4 GiB (+ 前後のガード) の領域 kern_vm を予約する。ユーザー空間の mmap 先 (user_vm) と同じ物理ページを両方に張る。

```
kern_addr = kern_vm_start + (u32)user_ptr
```

`(u32)user_ptr` は「user_vm の先頭からのオフセット」ではなく、ユーザー側の番地の **下位 32 bit そのもの** である点に注意。`arena.c` 冒頭のコメントの例:

| | 値 |
|---|---|
| user_vm_start (mmap が選ぶ) | `7f7d26200000` |
| kern_vm_start (get_vm_area + GUARD_SZ/2) | `ffffc90001e69000` |
| pgoff=0 のページの kern 側番地 | `ffffc90001e69000 + 26200000 = ffffc90028069000` |

ページ番号は `compute_pgoff()` で `(u32)(uaddr - (u32)user_vm_start) >> PAGE_SHIFT` と別に計算する。4 GiB の窓の中で下位 32 bit が一周しても、窓の中の位置は一意に決まる。

## 2. BPF の ISA と x86 JIT のレジスタ

BPF には 64 bit のレジスタ r0〜r10 がある。x86 の JIT は固定で割り当てる (`reg2hex[]`, 183 行付近)。

| BPF | x86 | 用途 |
|---|---|---|
| r0 | rax | 戻り値 |
| r1〜r5 | rdi, rsi, rdx, rcx, r8 | 引数。x86-64 の呼び出し規約の第 1〜5 引数と一致 |
| r6〜r9 | rbx, r13, r14, r15 | callee-saved |
| r10 | rbp | フレームポインタ |
| (なし) | r12 | arena を使う prog だけ、プロローグで kern_vm_start を入れる (1845 行付近) |

r12 は BPF のレジスタに対応しない JIT 専用の場所なので、r12 を使う命令は BPF のバイトコードには現れない。

ISA の性質で効いてくるのは「32 bit の ALU 命令 (`BPF_ALU` クラス) は結果の上位 32 bit を 0 にする」こと。x86 でも 32 bit レジスタへの書き込みは上位 32 bit を 0 にするので、BPF の `w1 = w1` は x86 の `mov edi, edi` 1 命令になる。

## 3. BPF 自身が arena を読み書きする場合 (6.9 から)

1. ソースの `cast_kern` (`addr_space_cast(rN, 0, 1)`、as1 → as0) は、`BPF_ALU64 | BPF_MOV | BPF_X`、`off = BPF_ADDR_SPACE_CAST`、`imm = 1` の命令になる。
2. verifier がこれを 32 bit の mov (`wN = wN`) に書き換える (`fixups.c` の `is_addr_space_cast32()` と 1569 行付近)。やるのは下位 32 bit に切ることだけで、kern_vm_start は足さない。
3. arena を指すレジスタからの load / store は、verifier が `BPF_PROBE_MEM32` モードに書き換える (`fixups.c` 958 行付近)。
4. JIT は PROBE_MEM32 を `mov rax, [rdi + r12 + off]` のように出す (`emit_ldx_r12()`, 2334 行付近)。kern_vm_start の足し算はアドレッシングモードに畳み込まれ、追加の命令はない。

未割り当てのページで fault したときは、例外テーブル (`EX_TYPE_BPF`) が拾って load の結果を 0 にする。

## 4. kfunc に arena のポインタを渡す場合

### 6.12〜7.2: 変換はない

kfunc は普通のカーネル C で、JIT を通らない。BPF 側が渡したレジスタの値 (user_vm の番地か、cast_kern 後の下位 32 bit) がそのまま届く。kern_vm の番地にするには kfunc 側で kern_vm_start を足す必要があるが、`bpf_arena_get_kern_vm_start()` はモジュールに export されていない。

7.2 で入った「bpf/arena: Direct kernel-side access」は、kfunc が kern_vm の **未割り当て** のページに触れて fault したときの救済。`bpf_arena_handle_page_fault()` が、BPF prog が stack にいることと番地が arena の範囲内であることを確かめてから、scratch page を張って命令をやり直させ、BPF stream に違反を記録する。番地の付け替えはしない。

### 7.3: JIT が呼び出しの直前に付け替える

kfunc の引数名に `__arena` (または `__arena__nullable`) を付ける。

- **verifier**: `is_kfunc_arg_arena()` (11477 行付近) で検査する。条件は、JIT が対応していること (`bpf_jit_supports_arena_args()`)、prog に arena があること、レジスタ渡しの引数であること (stack 渡しは拒否)。
- **BTF**: `__get_arg_fmodel_flags()` (`btf.c` 7604 行付近) が、関数モデルの引数に `BTF_FMODEL_ARENA_ARG` (nullable なら `BTF_FMODEL_NULLABLE_ARG` も) を立てる。
- **JIT**: `BPF_PSEUDO_KFUNC_CALL` の call を出す直前に `emit_kfunc_arena_args()` (1692 行付近) を呼び、印の付いた引数ごとに次を出す。

```
; 例: 第 1 引数 (BPF r1 = rdi) が __arena
mov  edi, edi        ; 上位 32 bit を 0 にする (BPF の w1 = w1 と同じ)
add  rdi, r12        ; r12 = kern_vm_start
call kfunc           ; kfunc は rdi を普通のポインタとして受け取る
```

`__arena__nullable` のときは、1 行目と 2 行目の間に `test edi, edi; je +3` が入る。下位 32 bit が 0 なら add を飛ばし、NULL のまま渡す。

値で追うと次のとおり。

| 時点 | rdi |
|---|---|
| 呼び出し前 (user_vm の番地) | `0x7f7d_2620_1040` |
| `mov edi, edi` の後 | `0x0000_0000_2620_1040` |
| `add rdi, r12` の後 | `kern_vm_start + 0x2620_1040` (§1 の例なら pgoff=0 のページの +0x1040) |

足し算が §3 と同じなのは、どちらも同じ式 `kern_vm_start + (u32)ptr` だから。load ではアドレッシングモードに畳み込めるが、kfunc には値そのものを渡す必要があるので add を明示的に出す。コストは引数 1 つあたり 2 命令 (nullable は 4 命令)。

arm64 の JIT にも同名の `emit_kfunc_arena_args()` があり、`bpf_jit_supports_arena_args()` は true を返す。arm64 では `add xN, x<base>, wN, uxtw` (32 bit を 0 拡張しながら足す) の 1 命令で、切ると足すが一度に済む。nullable のときだけ `mov wN, wN` と `cbz` を前に出す。

struct_ops のコールバックの引数に `__arena` が付いている場合は向きが逆になる。trampoline の `save_args()` が `emit_arena_arg_conv()` で kern_vm の番地から `(u32)(src - kern_vm_start)` を作り、arena のポインタの形にして BPF の ctx に入れる。

付け替えは JIT が出すネイティブ命令だけにあり、BPF のバイトコードにはないので、`bpftool prog dump xlated` には出ない。`dump jited` では見えるはず (未確認)。

## 5. この PoC への影響

7.2 の fault の救済も 7.3 の付け替えも、**BPF prog が stack にいて kfunc を呼ぶ場合** だけに効く。kmod の `.read_folio` のように BPF を経由しない経路では、今も `pin_user_pages_remote` (GUP) で user_vm 経由で読むしかない ([`ebpf-ecosystem-gaps.md`](ebpf-ecosystem-gaps.md) の A1 / K-02)。

## 参考

- ISA: RFC 9669 "BPF Instruction Set Architecture" (32 bit の ALU は上位 32 bit を 0 にする)
- 7.2 の議論: https://ratatoskr.run/lkml/2026/04/3531050/t, https://ratatoskr.run/bpf/2026/05/17020464/t
- `__arena` 引数の議論: https://ratatoskr.run/bpf/2026/07/17249083/t, https://ratatoskr.run/bpf/2026/08/17385603/t
