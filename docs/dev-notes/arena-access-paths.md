# arena の 1 ページを誰がどう読むか: 6.12 と 7.3 の比較

2026-10-25 時点のメモ。このリポジトリのコード (`kmod/vfs_ops.c`, `kmod/tree_traversal.c`) と、v6.12 / v7.2 / v7.3 のタグのカーネルソースから整理した。**7.2 / 7.3 の挙動は実機では試していない。** 番地の計算そのもの (JIT の命令、ISA との対応) は [`arena-address-translation.md`](arena-address-translation.md) を参照。デッキの付録 `appx-arena-path-612` と `appx-arena-path-73` はこのメモを図にしたもの。

## 1. 1 枚の物理ページに 3 つの番地がある

```
  daemon の mm (このプロセスだけ)        カーネルのページテーブル (全プロセス共通)
 ┌──────────────────────────┐        ┌────────────────────────────────┐   ┌──────────────────┐
 │ user_vm                  │        │ kern_vm (4 GiB + ガードを予約)   │   │ direct map       │
 │ 0x7f7d_2620_1000         │        │ 0xffffc900_2806_a000            │   │ (page_address)   │
 └────────────┬─────────────┘        └───────────────┬────────────────┘   └────────┬─────────┘
              │ arena_vm_fault が張る                 │ alloc_pages / fault で張る          │ 常にある
              ▼                                      ▼                                    ▼
             ┌──────────────────────────────────────────────────────────────────────────┐
             │                        物理ページ (4 KiB)                                  │
             └──────────────────────────────────────────────────────────────────────────┘
       daemon が deref                BPF prog が load                kmod が kmap_local_page で読む
       KFunc が copy_from_user        (7.3) KFunc が __arena 引数で load   (x86-64 では direct map の番地)
```

- user_vm と kern_vm の対応は `kern = kern_vm_start + (u32)user`。上の例は `arena.c` 冒頭のコメントの値 (user_vm_start `7f7d26200000`、kern_vm_start `ffffc90001e69000`) で、2 ページ目 (pgoff=1) にあたる。
- user_vm に張られるのは、ユーザー側がそのページに触れて fault したときか GUP したとき。`bpf_arena_alloc_pages` は kern_vm にだけ張る。
- kmod は kern_vm を正規には使えない (`bpf_arena_get_kern_vm_start()` が未 export、kern_vm には BPF 以外向けの fault handler が無い)。なので user_vm 側から GUP でページを捕まえ、direct map の番地で読む。

## 2. 6.12: 経路ごとの手順 (1 ページあたり)

凡例: `[計算]` 番地の計算と load、`{待ち}` ロック・pin・fault、`<<コピー>>` データのコピー。

### 2.1 daemon (ユーザー空間)

```
[user_vm を deref]                                         コピーなし
```

### 2.2 BPF prog (どの文脈でも)

```
[r12 + (u32)ptr]  ─▶ [kern_vm を load]                       コピーなし
   └ verifier が PROBE_MEM32 に書き換え、JIT が mov rax, [rdi + r12 + off] を出す
   └ 未割り当てのページは例外テーブルが拾って 0 を返す
```

### 2.3 KFunc `s3fs_kfunc_arena_list_walk` (daemon の syscall 文脈だけ)

```
for each node:
  [current->mm の user_vm 番地] ─▶ {PTE が無ければ arena_vm_fault} ─▶ <<copy_from_user 16 B>>
                                                                      └ next も user_vm の番地
```

- arena の中に保存されている `next` はユーザー側の番地なので、`current->mm` が arena を mmap した daemon でないと読めない。そのため `BPF_PROG_TYPE_SYSCALL` に登録し、daemon からの `BPF_PROG_TEST_RUN` で呼ぶ前提にしている。
- 安全性は `copy_from_user` が担う (壊れた `next` は oops ではなく `-EFAULT`)。

### 2.4 kmod `.read_iter` (`s3fs_arena_copy_to_iter`、read したプロセスの文脈)

```
for each page:
  {mmap_read_lock(daemon の mm)} ─▶ {pin_user_pages_remote: ページテーブルを引いて pin}
     ─▶ [kmap_local_page] ─▶ <<copy_to_iter: arena ページ → read のバッファ>> ─▶ {unpin_user_pages}
                                                                              コピー 1 回
```

- 読むのは daemon 以外のプロセスなので、`copy_from_user` は使えず、daemon の mm を名指しで引く `pin_user_pages_remote` になる。PTE が無ければ GUP が `arena_vm_fault` を呼んで張る。
- ページごとに daemon の `mmap_lock` を取るので、並列の read はここで競合する。

### 2.5 kmod `.read_folio` (`s3fs_folio_fill_from_arena`、page cache を埋める)

```
for each page:
  {mmap_read_lock} ─▶ {pin_user_pages_remote} ─▶ [kmap_local_page]
     ─▶ <<memcpy: arena ページ → page cache の folio>> ─▶ {unpin}
その後:
  <<filemap の read: folio → read のバッファ>>
                                                                              コピー 2 回
```

### 2.6 比較: pcache モード (`--arena-metadata-only`、arena を使わない)

```
[page cache の folio] ─▶ <<copy_to_iter: folio → read のバッファ>>              コピー 1 回、GUP なし
```

### 2.7 まとめ (6.12)

| 経路 | 入口の番地 | ロック・pin | コピー | 制約 |
|---|---|---|---|---|
| daemon | user_vm | なし | なし | daemon だけ |
| BPF prog | kern_vm (JIT が r12 を足す) | なし | なし | なし |
| KFunc (list walk) | user_vm (current) | fault のみ | 16 B / ノード | daemon の syscall 文脈だけ |
| kmod `.read_iter` | user_vm (daemon の mm) → direct map | mmap_lock + pin / ページ | 1 回 | なし |
| kmod `.read_folio` | 同上 | 同上 | 2 回 | なし |
| pcache モード | page cache | なし | 1 回 | arena を使わない |

## 3. 7.2〜7.3 で変わること

```
                       6.12                                  7.3
BPF prog        [r12 + (u32)ptr] → load              変わらない

KFunc の        値がそのまま届く                       JIT: mov edi, edi ; add rdi, r12
__arena 引数    (user_vm 番地 or 下位 32 bit)          → kfunc は kern_vm の番地を受け取り、普通に load
                → copy_from_user で読む                  コピーなし、pin なし

KFunc が触れた  (kern_vm を触る手段がない)              7.2: fault → scratch page を張る、BPF stream に報告
未割り当てページ                                         → oops しない

KFunc が辿る    user_vm の番地                          変わらない。kern_vm_start を足す必要があるが、
next                                                  bpf_arena_map_kern_vm_start() はモジュールに未 export

kmod の         mmap_lock + GUP + kmap + コピー         変わらない (BPF の外なので、どちらの変更も効かない)
read 経路
```

- 7.3 の付け替えは **BPF から kfunc に渡す引数のレジスタだけ** が対象。kfunc の中でたどるポインタ (arena の中に保存された `next`) は user_vm の番地のまま。
- 7.2 の fault の救済は、`bpf_prog_find_from_stack()` で BPF prog が stack にいるときだけ働く。
- この PoC の list walk を 7.3 向けに書き直すと、引数で受けた先頭ノードは直接読めるが、`next` をたどる部分は `copy_from_user` のまま (試していない)。in-tree の kfunc なら `bpf_arena_map_kern_vm_start(bpf_prog_arena(prog))` で足せるが、モジュールからは呼べない。

## 4. 結論

- arena の「ゼロコピー」が成り立つのは、6.12 では **daemon と BPF prog の間だけ**。7.3 でここに **BPF から呼ばれた kfunc の引数** が加わる。
- kmod の read 経路は、6.12 でも 7.3 でも「よそのプロセスのユーザーメモリ」として GUP で読むしかなく、ページごとのロック・pin と 1〜2 回のコピーが残る。pcache モードが arena モードより軽いのはこのため。
- kmod から GUP を外すには、[`ebpf-ecosystem-gaps.md`](ebpf-ecosystem-gaps.md) の K-01 (kern_vm_start の export) と K-02 (BPF 以外の文脈向けの fault handler) が要る。
