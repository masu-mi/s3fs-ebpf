# kCFI の基本動作と、BPF / struct_ops との関係

2026-10-26 時点のメモ。根拠は v6.12 のソース (`arch/x86/include/asm/cfi.h`、`arch/x86/kernel/alternative.c`、`kernel/cfi.c`、`arch/x86/net/bpf_jit_comp.c`、`kernel/bpf/bpf_struct_ops.c`)。**この PoC の実機で kCFI が有効かどうかは確かめていない。** デッキの付録 `appx-kcfi` はこのメモを 1 枚にしたもの。struct_ops の glue 全体は [`struct-ops-kmod-glue.md`](struct-ops-kmod-glue.md)。

## 1. 何をするものか

kCFI (kernel Control-Flow Integrity) は、Clang でビルドしたカーネル (`CONFIG_CFI_CLANG`) で使える、**関数ポインタ経由の呼び出し (間接呼び出し) の検査**。

- コンパイラが関数ごとに、その関数の型 (引数と戻り値) から 32 bit のハッシュを作り、関数の入口の直前に置く。
- 関数ポインタで呼ぶ側には、「呼び先の入口の直前にあるハッシュが、この呼び出しの型のハッシュと一致するか」を確かめるコードを入れる。
- 一致しなければ trap する。

目的は、攻撃者がメモリ上の関数ポインタを書き換えて、型の違う関数や関数の途中 (ROP / JOP のガジェット) に飛ばすのを防ぐこと。直接呼び出し (`call foo`) は飛び先が命令に埋まっているので検査しない。

## 2. x86-64 での命令列

`arch/x86/include/asm/cfi.h` 冒頭のコメントから。

### 2.1 呼ばれる側

```
__cfi_foo:
  movl $0x12345678, %eax      # 型のハッシュ (movl の即値として埋める。実行はされない)
                              # CONFIG_CALL_PADDING なら nop × 11
foo:
  endbr64                     # IBT が有効なとき
  ... 本体 ...
  ret
```

ハッシュは `movl` 命令の即値として置かれるだけで、この命令が実行されるわけではない。入口 `foo` から見て決まったオフセット (CALL_PADDING なしなら 4 バイト前、ありなら 15 バイト前) にハッシュがある。

### 2.2 関数ポインタで呼ぶ側

```
  lea foo(%rip), %r11         # 呼び先 (関数ポインタの値)
  ...
  movl $(-0x12345678), %r10d  # 期待するハッシュの符号を反転した値
  addl -15(%r11), %r10d       # 呼び先の入口の前にあるハッシュを足す (CALL_PADDING なしは -4)
  jz   1f                     # 一致すれば和が 0
  ud2                         # 不一致 → 無効命令で trap
1:call *%r11
```

`ud2` の trap は `handle_cfi_failure()` から `report_cfi_failure()` (`kernel/cfi.c`) に渡り、`CFI failure at ... (target: ...; expected type: 0x...)` を出して BUG にする。`CONFIG_CFI_PERMISSIVE` なら警告だけで続行する。

### 2.3 FineIBT

IBT (CPU の Indirect Branch Tracking) のある x86 では、起動時に命令列が FineIBT という形に書き換えられる (`cfi_mode = CFI_FINEIBT`)。呼ぶ側は期待値を `%r10d` に入れて入口の 16 バイト前に飛び、呼ばれる側の `__cfi_foo` で `subl $hash, %r10d; jz foo; ud2` と比べる。検査する場所が呼ばれる側に移るだけで、型のハッシュで確かめる考え方は同じ。

### 2.4 ハッシュの取り出し

カーネルは `cfi_get_func_hash(func)` (`arch/x86/kernel/alternative.c`) で、既存の関数の入口の前からハッシュを読める。kCFI なら `func - cfi_get_offset() + 1` (movl のオペコードの次) を読む。

## 3. BPF との関係

### 3.1 BPF prog 自体

カーネルは BPF prog を `prog->bpf_func` という関数ポインタで呼ぶ (`bpf_prog_run()` など)。JIT が作った prog にはコンパイラがハッシュを付けないので、x86 の JIT はプロローグの前に `emit_cfi(cfi_bpf_hash)` (サブプログラムは `cfi_bpf_subprog_hash`) を出す。`cfi_bpf_hash` は「BPF prog を呼ぶ関数ポインタの型」のハッシュ。

### 3.2 struct_ops の trampoline と `cfi_stubs`

```
kmod: ops->on_hit(ino)            ← 関数ポインタ。型 int (*)(u64) のハッシュで検査される
   │
   ▼
struct_ops の trampoline           ← 実行時に JIT が作る。コンパイラのハッシュが無い
   │   (直接 call)                    → 作るときに stub からハッシュを写す
   ▼
BPF prog (on_hit の実装)
```

- kmod は vtable の member を関数ポインタで呼ぶので、kCFI の検査を受ける。
- 呼び先は、`bpf_struct_ops_prepare_trampoline()` が実行時に作る trampoline。コンパイラが作った関数ではないので、入口の前にハッシュが無い。
- そこで x86 の JIT は `BPF_TRAMP_F_INDIRECT` (struct_ops 用) のとき、渡された stub の関数ポインタから `cfi_get_func_hash(stub)` でハッシュを読み、trampoline の入口の前に `emit_cfi()` で書き込む (`bpf_jit_comp.c` 2985 行付近)。
- stub はコンパイラが作った本物の関数で、member と同じ型なので、正しいハッシュを持っている。`cfi_stubs` はこの「型の見本」の表で、実際には呼ばれない。
- trampoline から BPF prog へは直接 call なので、そこでは検査しない。

### 3.3 kCFI が無効なカーネルでも stub が要る

`bpf_struct_ops_desc_init()` は kCFI の有無にかかわらず `cfi_stubs` が NULL なら `-EINVAL` を返す。また stub が NULL の member は `bpf_struct_ops_supported()` で非対応になる。kCFI が無効なら `cfi_get_func_hash()` は 0 を返すだけだが、登録には stub の表が必須。

## 4. まとめ

- kCFI は「関数ポインタの呼び先が、その型の関数の入口か」を、入口の前に置いた型のハッシュで確かめる仕組み。
- 実行時に作るコード (BPF prog、trampoline) はハッシュを持たないので、JIT が自分で付ける。BPF prog は共通の `cfi_bpf_hash`、struct_ops の trampoline は member ごとに型が違うので stub から写す。
- struct_ops を kmod から提供するときに member ごとの空関数 (`cfi_stubs`) を書かされるのは、このため。
