# Week 2 実装ノート — verifier とツールチェーンの地雷

Week 1 (`week1-build-runtime-notes.md`) が「ビルド環境の整備」の記録なのに対し、Week 2 は BPF プログラム側の細かい実装で踏んだ **verifier の挙動 / ライブラリのバグ / カーネル・コンパイラのバージョン差分** の記録。順序と一次情報 (`kernel/bpf/verifier.c` / `cilium/ebpf` ソース) を残す。

対応 commit: master ブランチの Week 2 サブトラック (a)〜(d)。

---

## 0. 対応関係の早見表

| 症状 | 原因 | 対策 | 参照 |
|---|---|---|---|
| `BPF_MOV uses reserved fields` (`bpf_addr_space_cast` load 時) | cilium/ebpf `asm/instruction.go` `Marshal` が Mov の `Offset=1` を常時 0 に上書き | `daemon/third_party/ebpf/` に vendor + 1 行パッチ、`go.mod` の `replace` で差し込み | §1 |
| `BPF_MOV uses reserved fields` (パッチ後も残る) | 別の要因: verifier が `prog->aux->arena` に arena map を関連付けていない | BPF プログラムから `asm volatile("" :: "r"(&s3fs_arena));` で LD_MAP_FD を強制発行 | §2 |
| `arena's user address must be set via map_extra or mmap()` | daemon が mmap する前に BPF プログラムを load している | `ebpf.NewMap(arenaSpec)` → `arena.Wrap()` (mmap 発火) → `LoadAndAssign(..., MapReplacements)` の 2 段順序に | §3 |
| `invalid zero-sized read` (`bpf_map_lookup_elem(&s3fs_arena, ...)`) | arena map の `key_size = 0` | arena に対する map lookup は使わない。参照だけ欲しいなら `asm volatile` を使う | §4 |
| `fentry/vfs_getattr` を貼っても発火しない | 現行 kernel の `stat` は `vfs_statx_path → vfs_getattr_nosec` を直接呼ぶ | ftrace で確認して `SEC("fentry/vfs_getattr_nosec")` に変更 | §5 |
| `BPF program is too large. Processed 1000001 insn` | 純 eBPF で `while (n) { n = n->next; }` の unbounded pointer chase | KFunc に外出しする (これが本プロジェクトの主張そのもの) | §6 |
| `use of undeclared identifier 'BPF_MAP_TYPE_ARENA'` | pahole が生成する vmlinux.h の enum が 32 (CGRP_STORAGE) で打ち切られている | BPF プログラム側で `#define BPF_MAP_TYPE_ARENA 33` を後付け | §7 |
| `s3fs_ebpf: failed to register kfunc set: -22` (Week 1) | 6.11+ の `register_btf_kfunc_id_set` は `BTF_SET8_KFUNCS` flag を要求 | `BTF_SET8_START` → `BTF_KFUNCS_START` に置換 | §8 |
| `use of unknown builtin '__builtin_bpf_addr_space_cast'` / `-mcpu=v3` が macro を発行しない | clang 18 は `__BPF_FEATURE_ADDR_SPACE_CAST` を持たない (LLVM 18 の BPF backend 不足) | clang-20 を使う (`Makefile` が自動検出) | §9 |
| cilium/ebpf の `VerifierError.Error()` が `(N line(s) omitted)` で truncate | 意図的な短縮 | `errors.As(err, &ve)` で unwrap → `%+v` で完全 log | §10 |

---

## 1. cilium/ebpf の `bpf_addr_space_cast` Marshal バグ

**症状:** clang 20 が正しい encoding (`opcode=0xbf, off=1, imm=1`) を出しているのに、kernel に到達した時点で `off=0` に化け、`BPF_MOV uses reserved fields` で verifier が拒否する。

**原因:** `daemon/third_party/ebpf/asm/instruction.go` の `Instruction.Marshal` が ALU 命令の `Offset` を `newOffset` (デフォルト 0) で **常時上書き** する。`SDiv` / `SMod` / `MovSX8/16/32` の場合だけ `newOffset` が非 0 になるが、`bpf_addr_space_cast` は **plain Mov として encode される** (opcode は Mov、意味は Offset=1 の marker) ので、この switch のどの case にも入らず `newOffset = 0` のまま `ins.Offset = 0` が書かれる。

`upstream` (`main` HEAD; 2026-09 時点) にも同じ実装が残っている。

**修正 (`daemon/third_party/ebpf/asm/instruction.go`):**

```diff
-		ins.Offset = newOffset
+		if newOffset != 0 {
+			ins.Offset = newOffset
+		}
```

パッチファイルとして `docs/cilium-ebpf-addr-space-cast-marshal.patch` に置いてある。cilium/ebpf の tree から `patch -p1` あるいは `git am` で適用でき、v0.22.0 と `main` (2026-09-11 時点) の両方に clean で当たる。上流 PR / fork の commit にはこのファイルをそのまま使う想定。

**回避策:** cilium/ebpf を `daemon/third_party/ebpf/` に vendor し、`daemon/go.mod` に

```gomod
replace github.com/cilium/ebpf => ./third_party/ebpf
```

を追加。詳細と将来の fork への移行手順は `daemon/third_party/ebpf/README-s3fs-patch.md`。

**確認方法:** vendor パッチを外して `make test-arena` を回すと、verifier log の instruction 10 が `r2 = r3` (bare Mov) として表示され、`BPF_MOV uses reserved fields` で落ちる。パッチ後は `r2 = addr_space_cast(r3, 0x0, 0x1)` と正しく表示される。

---

## 2. verifier が arena map の関連付けを要求する

**症状:** cilium/ebpf のバグを直しても `BPF_MOV uses reserved fields` が消えない。verifier log を `%+v` で取ると命令 10 まで到達しているが、その直前 (§4 の途中) で落ちる。

**原因:** `kernel/bpf/verifier.c: check_alu_op` の addr_space_cast パスに `if (!env->prog->aux->arena) { verbose(...); return -EINVAL; }` チェックがある。`prog->aux->arena` は `resolve_pseudo_ldimm64` が LD_MAP_FD 命令から arena 型の map を発見した時にだけ set される (line 19115 前後、v6.12)。

つまり **BPF プログラムのどこかで arena map を参照する命令が出ていないと、addr_space_cast を含む一切のコードが verifier を通らない**。

**罠:**

- `SEC(".maps")` で map を宣言するだけでは足りない。**プログラムの命令として** LD_MAP_FD が emit される必要がある。
- 自然な参照方法は `bpf_map_lookup_elem(&arena_map, &key)`。ただし arena map は `key_size = 0` なので **`invalid zero-sized read` で別途拒否される** (§4)。
- `bpf_arena_alloc_pages(&arena_map, ...)` を実行時に呼べば LD_MAP_FD が出るが、page を消費する副作用がある。
- `volatile void *p = &arena_map;` は LLVM に dead-store elimination で消される。

**採用した回避策 (`bpf/s3fs_fastpath.bpf.c`):**

```c
static __always_inline void s3fs_arena_pin(void)
{
    asm volatile("" :: "r"(&s3fs_arena));
}
```

インラインアセンブリの "r" 制約は LLVM に「レジスタに値を用意しろ」と要求する。副作用なしで LD_MAP_FD 命令が生き残る唯一のイディオム。

**確認方法:** `llvm-readelf-20 --relocations daemon/internal/ebpf/s3fsfp_bpfel.o` で `.relsyscall` セクションに `s3fs_arena` の relocation が出ていることを確認。`asm volatile` を消すと消える。

---

## 3. verifier が `user_vm_start` の先設定を要求する

**症状:** §1 と §2 を対処すると次に `arena's user address must be set via map_extra or mmap()` が出る。

**原因:** kernel 側の `arena_map_alloc` / `arena_map_mmap` の順序ルール:

- `map_extra` を指定して map を作った場合: `arena->user_vm_start = attr->map_extra` が set される。以後 userspace は `mmap(user_vm_start, ..., MAP_FIXED)` でなければ `-EBUSY` を返す。cilium/ebpf の `Map.Memory()` は addr=0 で呼ぶので相性が悪い。
- `map_extra` を 0 にした場合: 最初に userspace が mmap した address が `arena->user_vm_start` として記録される。**その mmap 発火より前に BPF プログラムが load されると verifier が拒否する**。

**採用した順序 (`daemon/internal/ebpf/loader.go: Load`):**

```
loadS3fsfp() (spec のパース)
    ↓
ebpf.NewMap(arenaSpec)          -- arena map を単独で作成
    ↓
arena.Wrap(arenaMap)            -- ★ここで mmap 発火 → user_vm_start 決定
    ↓
spec.LoadAndAssign(&Objs, {MapReplacements: {s3fs_arena: arenaMap}})
                                -- BPF プログラム load。verifier チェック通過
```

`MapReplacements` は cilium/ebpf 側で `arenaMap.Clone()` (fd の dup(2)) が呼ばれる。dup 先も同じ kernel arena struct を指すので `user_vm_start` は共有される。

---

## 4. arena map に対する `bpf_map_lookup_elem` は使えない

**症状:** §2 で使えるかと期待した `bpf_map_lookup_elem(&s3fs_arena, &key)` が `invalid zero-sized read` で verifier に拒否される。

**原因:** arena map は key/value を持たない (`key_size = 0`, `value_size = 0`) 特殊 map。`bpf_map_lookup_elem` の第 2 引数は `void *key` で、verifier は `key_size` バイト分の読み取り可能領域を要求する。`key_size = 0` のとき `access_size == 0` が偽なら kernel が `verbose(env, "invalid zero-sized read\n")` を出す (v6.12 verifier.c line 7290)。

**含意:** arena map の参照は `bpf_arena_alloc_pages` (副作用あり) か §2 の `asm volatile` トリック (副作用なし) のどちらかしかない。

---

## 5. `fentry/vfs_getattr` は現行 kernel で発火しない

**症状:** `SEC("fentry/vfs_getattr")` として BPF プログラムを attach しても、`stat` 系 syscall を叩いてもカウンタが動かない。attach 自体は成功している (`bpftool link list` で確認可)。

**原因:** 現行 kernel (v6.12) の `stat` / `newfstatat` syscall は `vfs_statx()` → `vfs_getattr_nosec()` を **直接** 呼ぶ。`vfs_getattr()` は `vfs_fstat()` など一部の legacy パスからしか呼ばれない。

**確認方法 (ftrace):**

```bash
sudo bash -c '
  echo "vfs_getattr*" > /sys/kernel/tracing/set_ftrace_filter
  echo function > /sys/kernel/tracing/current_tracer
  echo 1 > /sys/kernel/tracing/tracing_on'
stat /mnt/target
sudo bash -c 'echo 0 > /sys/kernel/tracing/tracing_on'
sudo head /sys/kernel/tracing/trace
```

出力に `vfs_getattr_nosec <- vfs_statx_path` は並ぶが `vfs_getattr` は 1 件も出ない。

**修正 (`bpf/s3fs_fastpath.bpf.c`):**

```diff
-SEC("fentry/vfs_getattr")
+SEC("fentry/vfs_getattr_nosec")
```

引数の型は同じ (`const struct path *, struct kstat *, u32, unsigned int`) なので BPF_PROG マクロは変更不要。

---

## 6. Meetup 主張の証拠となる verifier 拒否ログ

**目的:** 「純 eBPF で深いツリー走査を書くと verifier に落ちる」ことを 1 コマンドで再現する (`s3fs-ebpf verifier-demo`)。

**BPF プログラム (`bpf/s3fs_fastpath.bpf.c: s3fs_deep_walk_reject`):**

```c
n = (struct s3fs_list_node __arena *)(unsigned long)ctx->arena_base_uaddr;
while (n) {
    sum += n->payload;
    n = n->next;
}
```

**verifier の出方 (実際の出力を抜粋):**

```text
7: (bf) r2 = addr_space_cast(r2, 0, 1)  ; R2_w=arena
8: (79) r4 = *(u64 *)(r2 +8)            ; R2_w=arena R4_w=scalar()
9: (0f) r4 += r3                        ; R3_w=scalar(id=111110)
10: (79) r2 = *(u64 *)(r2 +0)           ; R2_w=scalar()
11: (bf) r3 = r4                        ; R3_w=scalar(id=111110)
12: (55) if r2 != 0x0 goto pc-6         ; back-edge → 7 に戻る
...
BPF program is too large. Processed 1000001 insn
processed 1000001 insns (limit 1000000) max_states_per_insn 4 total_states 9664 peak_states 9664 mark_read 1
```

`n = n->next` が **`R2_w=scalar()`** (unknown) を返すため、verifier は「次イテレーションが arena 内かどうか」を絞り込めず、状態爆発で 1M 命令上限 (`BPF_COMPLEXITY_LIMIT_INSNS`) を突破する。

**bpf2go / cilium/ebpf 上の注意:** 生成された `s3fsfpObjects` はこの reject プログラムを含む。`LoadAndAssign(&Objs)` はそれを load しようとして落ちる。回避策として `daemon/internal/ebpf/loader.go` に **healthy program だけを列挙した `runtimeObjs` mirror struct** を用意し、runtime パスはそれを使う。demo は `rejectDemoObjs` (reject プログラム + arena map のみ) を別途 `LoadAndAssign` する。

---

## 7. `BPF_MAP_TYPE_ARENA` が vmlinux.h に無い

**症状:**

```text
error: use of undeclared identifier 'BPF_MAP_TYPE_ARENA'; did you mean 'BPF_MAP_TYPE_ARRAY'?
   43 |         __uint(type, BPF_MAP_TYPE_ARENA);
```

**原因:** `scripts/gen-vmlinux.sh` が生成する `bpf/vmlinux.h` の `enum bpf_map_type` が `BPF_MAP_TYPE_CGRP_STORAGE = 32` で打ち切られている。pahole の抽出が enum の最後を "stable" と判断して切っている挙動。kernel v6.12 では 33 に `BPF_MAP_TYPE_ARENA` が定義されているが export されない。

**対策 (`bpf/s3fs_fastpath.bpf.c`):**

```c
#ifndef BPF_MAP_TYPE_ARENA
#define BPF_MAP_TYPE_ARENA 33
#endif
```

**将来:** vmlinux.h を kernel 側 tools/lib で正式生成するか、bpftool の `btf dump` を使う手もある。Week 3 以降で `bpf_arena_alloc_pages` kfunc を宣言する時に同じ問題が再発する見込み。

---

## 8. カーネルモジュール側のバージョン依存アノテーション

Week 1 の記録 (`week1-build-runtime-notes.md` 課題 #5) を要約 + 補足:

### 8.1 `BTF_SET8_START` vs `BTF_KFUNCS_START`

kernel 6.11 で `register_btf_kfunc_id_set` が `set->flags & BTF_SET8_KFUNCS` チェックを追加した。この flag を立てるのは `BTF_KFUNCS_START` マクロ (`BTF_SET8_START` は 0 のまま)。既存コード内のコメントで「両者はエイリアス」と書かれていたが誤り。

**現行 (`kmod/tree_traversal.c`):**

```c
#ifndef BTF_KFUNCS_START
#define BTF_KFUNCS_START(name) BTF_SET8_START(name)
#define BTF_KFUNCS_END(name)   BTF_SET8_END(name)
#endif

BTF_KFUNCS_START(s3fs_kfunc_ids_tracing)
BTF_ID_FLAGS(func, s3fs_kfunc_traverse_stub, KF_TRUSTED_ARGS)
BTF_KFUNCS_END(s3fs_kfunc_ids_tracing)
```

**症状 (対策前):** `insmod` 時に `s3fs_ebpf: failed to register kfunc set: -22`。

### 8.2 `SEC(...)` と kernel BTF の一致

kmod と BPF プログラムで `S3FS_EBPF_MAGIC` (`0x53334642`) を共有する必要がある。以前は kmod 側のみに `#define` があり BPF 側では filter できなかった。

**現行:** `include/s3fs_shared_types.h` に集約し、両者が include する:

```c
#define S3FS_EBPF_MAGIC 0x53334642UL  /* "S3FB" */
```

`kmod/vfs_ops.c` は `#include "s3fs_shared_types.h"` (Kbuild の `ccflags-y += -I$(src)/../include` に依拠)。BPF 側は `-I../include` 経由で同ヘッダを取り込む。

### 8.3 kbuild と vmlinux ELF

Ubuntu の `linux-headers` パッケージは vmlinux ELF を含まないため、kmod ビルドで BTF/`resolve_btfids` パイプラインが `Skipping BTF generation for s3fs_kfunc.ko due to unavailability of vmlinux` で無音でスキップされる。結果として上記 `BTF_KFUNCS_START` の flag が BTF に載らず -22 に至る。

**対策:** `scripts/extract-vmlinux` で `/boot/vmlinuz-*` を伸長して KDIR 配下に配置 (Week 1 の課題 #4)。

### 8.4 pahole バージョン

noble 標準の pahole 1.25 は kernel 1.30 でビルドされた vmlinux BTF (distilled base BTF 付き) に対して silent no-op を出す。対策として `~/.local` に pahole 1.30 をビルド。Week 1 参照。

### 8.5 gcc バージョン

kernel 6.12 は gcc-15 で build されているが、noble にない。gcc-14 で override 可 (`-fmin-function-alignment=16` が gcc-14 で追加されたフラグ。gcc-13 では失敗)。

---

## 9. clang バージョン (`__BPF_FEATURE_ADDR_SPACE_CAST`)

**症状:** `-target bpf -mcpu=v3` を指定しても `__BPF_FEATURE_ADDR_SPACE_CAST` が定義されず、`__arena` ポインタ deref に `bpf_addr_space_cast` が emit されない。

**原因:** clang 18 (Ubuntu 24.04 標準 = `1:18.1.3`) は BPF backend で v3/v4 CPU を未サポート、`__BPF_FEATURE_ADDR_SPACE_CAST` macro も未定義。`clang -target bpf --print-supported-cpus` が `v1, v2` しか返さない。

**対策 (`Makefile`):** clang-20 を優先検出:

```make
CLANG ?= $(shell command -v clang-20 2>/dev/null || command -v clang-19 2>/dev/null || echo clang)
```

`bpf/Makefile` と `daemon/generate.go` の bpf2go 引数に `-mcpu=v3` を追加。

**確認方法:**

```bash
$ clang-20 -target bpf -mcpu=v3 -dM -E -x c /dev/null | grep FEATURE_ADDR
#define __BPF_FEATURE_ADDR_SPACE_CAST 1
```

**将来:** LLVM 21 以降での `-mcpu=v4` 挙動や CO-RE relocation の再確認。

---

## 10. cilium/ebpf の verifier log を取得する

**症状:** `LoadAndAssign` が返す `error` の `.Error()` は最初の 1 行 + `(N line(s) omitted)` で truncate される。arena / addr_space_cast 関係の verifier 拒否理由が分からない。

**対策 (`daemon/internal/ebpf/loader.go`):**

```go
var ve *ebpf.VerifierError
if errors.As(err, &ve) {
    return nil, fmt.Errorf("load bpf objects: %w\n=== verifier log ===\n%+v", err, ve)
}
```

`%+v` フォーマット動詞で `VerifierError.Format` がフル log を出す。cilium/ebpf は load 失敗時に自動でリトライして log を膨らませるので、`ProgramOptions.LogLevel` を明示 set しなくても良い (成功時は無駄なコストを払わない)。

**verifier-demo の設計:** `LoadRejectDemo` が `*ebpf.VerifierError` をそのまま返す。呼び出し側 (CLI) が `%+v` で表示。

---

## 11. cilium/ebpf の他の見つけた挙動 (未パッチ、回避で済ませたもの)

### 11.1 `MapReplacements` は map を Clone する

`CollectionOptions.MapReplacements` に渡した `*ebpf.Map` は cilium/ebpf 内で `Clone()` (= `dup(2)`) される。呼び出し側は元の fd を保持し続けるので、`Programs.Close()` は「Collection.Close() (dup した fd を close) + arena.Close() (元の fd と munmap を close)」の 2 系統。片方を怠ると fd リークする。

### 11.2 `LoadAndAssign` は target struct に無い program を load しない (実質)

`s3fsfpObjects` は bpf2go が生成する全プログラム/マップを含むが、`LoadAndAssign(&subsetObjs)` に別 struct を渡せば subset だけを load できる。ただし **struct のフィールド名は `ebpf:"..."` タグで一致必須**。runtime パスは `runtimeObjs`、reject demo は `rejectDemoObjs` を使う。

---

## 12. 参考にした一次情報

- `kernel/bpf/verifier.c` v6.12
  - `check_alu_op` の addr_space_cast パス (line 14338 前後、"BPF_MOV uses reserved fields" と "addr_space_cast insn can only be used ...")
  - `resolve_pseudo_ldimm64` の arena 関連付け (line 19100 前後、"Only one arena per program" / "arena's user address must be set")
  - `check_stack_range_initialized` (line 7290、"invalid zero-sized read")
  - `bpf_check` (line 22400 で `resolve_pseudo_ldimm64`、22418 で `do_check_main`)
- `include/uapi/linux/bpf.h` v6.12
  - `enum bpf_addr_space_cast { BPF_ADDR_SPACE_CAST = 1 }` (line 1348)
- `kernel/bpf/arena.c` v6.12
  - `arena_map_mmap`: `if (arena->user_vm_start && arena->user_vm_start != vma->vm_start) return -EBUSY;`
- `daemon/third_party/ebpf/asm/instruction.go` (v0.22.0 相当)
  - `Instruction.Unmarshal` (line 63〜114) と `Instruction.Marshal` (line 147〜214)
- Linux `tools/testing/selftests/bpf/bpf_arena_common.h`
  - `__arena` / `__arena_global` マクロの正式定義 (`__attribute__((address_space(1)))`)
- Linux `tools/testing/selftests/bpf/progs/verifier_arena.c`
  - `bpf_arena_alloc_pages` 経由の arena ポインタ取得パターン
