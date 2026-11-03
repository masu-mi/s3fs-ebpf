# Go と bpf_arena の相性、Go から arena を使うときのコツ (2026-11-03)

「arena は mmap を直接触るので、GC 任せの Go とは相性が悪いのでは。確保してから malloc の再実装が要るのでは」という疑問に答えるメモ。前半が一般論、後半がこの PoC の daemon (`daemon/internal/arena/`, `daemon/internal/ebpf/arena_alloc.go`) で実際にやっていること。

- 前提の版: Linux 6.12 LTS、Go 1.26 (`daemon/go.mod`)、cilium/ebpf `v0.22.1-0.20261008141411-5aef01488cf5`。
- **この doc の Go ランタイムに関する記述 (GC、`SetPanicOnFault`、checkptr など) は言語仕様と標準ドキュメントからの一般論で、この PoC では実機で確かめていない。** 確かめたものには「(PoC で確認)」と付けた。
- cilium/ebpf 固有の課題 (load の順番、`addr_space_cast`、`__arena` グローバル変数) は [`cilium-ebpf-arena-and-struct-ops.md`](cilium-ebpf-arena-and-struct-ops.md) §1 が正本。ここでは繰り返さない。arena のアドレス変換は [`arena-address-translation.md`](arena-address-translation.md)、誰がどう読むかは [`arena-access-paths.md`](arena-access-paths.md)。

---

## 1. 結論

| 使い方 | 相性 | 理由 |
|---|---|---|
| Go は arena の「バイト列」を読み書きするだけ。構造は BPF / kmod 側が決める | **良い** | arena は Go のヒープの外。GC に関係するものが何もない |
| Go が固定長のレコードやスロットを arena に置き、BPF と共有する (offset で参照) | **良い** | pointer を持たない struct と offset だけで足りる。確保は Go 側の小さな allocator で済む |
| Go が arena の中にリスト、木、hash table を作り、BPF と pointer でたどり合う | **悪い** | libbpf の `__arena` グローバル変数、`bpf_arena_common.h`、selftests のリスト / hash table 実装が使えない。pointer は user_vm の番地で、Go の型システムに載らない |
| arena に Go の値 (string、slice、map、interface、関数) を置く | **不可** | GC は arena を見ない。中の Go pointer は参照と数えられず、先のオブジェクトが回収される |

「malloc の再実装」は、必要になるのは **可変長で確保と解放が混ざる場合だけ**。この PoC では最初の 2 行の使い方で、範囲を管理する小さな free list (§4) だけで済んでいる。

## 2. なぜ GC と衝突しないか (する場面はどこか)

- `unix.Mmap` が返す `[]byte` は、mmap した領域を指す slice header だけが Go のヒープにある。**領域そのものは GC のヒープではない。** GC は領域をスキャンしないし、回収も移動もしない。
- Go の GC は non-moving (ヒープのオブジェクトは動かない)。動くのは goroutine のスタックだけ。なので「base 番地を BPF に渡す」「arena の番地を覚えておく」こと自体は、番地が arena を指す限り安全。
- 衝突するのは次の 2 つだけ。
  1. **arena の中に Go の pointer を置く。** 置いた先の Go オブジェクトは、arena から参照されていても回収される。
  2. **arena の `[]byte` を持つ変数が先に回収される。** Go の slice が到達不能になっても mmap は解放されない (finalizer を付けない限り) ので leak するだけだが、`Munmap` 済みの slice を使うと SIGSEGV になる。
- 逆方向 (arena の中の番地を、BPF が Go のヒープに向ける) は、BPF からは Go のヒープが見えないので起きない。

## 3. Go から arena を使うときのコツ

### 3.1 型と配置

1. **arena に置く struct は pointer を持たない。** 整数、固定長配列、`[N]byte` だけにする。参照は arena 先頭からの byte offset (`uint64`) で持つ。string は「長さ + offset」。
2. **レイアウトは C 側と 1 対 1 に固定する。** Go の struct は C と同じ順序・同じ padding になる保証が仕様にはないので、明示的な `_ [N]byte` と、テストでの `unsafe.Sizeof` / `unsafe.Offsetof` の確認 (C 側は `_Static_assert`) を対にする。または、struct にせず `binary.LittleEndian.Uint64(mem[off:])` で読む。後者は alignment と型の別名の問題が出ない。
3. **BPF が arena に書く pointer は user_vm の番地** (`user_vm_start + offset`)。Go で offset に直すには `ptr - base`。`base` は自分で mmap した先頭番地 (PoC の `Arena.BaseAddr()`)。BPF に渡すときだけ `uint64` で持ち、**Go の pointer に戻すときは `uintptr` 経由にせず**、`unsafe.Add(unsafe.Pointer(&mem[0]), off)` か `unsafe.Slice(&mem[off], n)` を使う。`uintptr` を保存して後で pointer に戻す形は `go vet` と checkptr に引っかかる。
4. **mmap の領域は 1 つの `[]byte` として持ち、型付きのビューは都度作る。** 型付きの `*T` を長く持つと、`Munmap` 後に使う事故が型で防げない。

### 3.2 並行性

5. **BPF / kmod と同時に触る場所は atomic にする。** Go 側は `sync/atomic` を使う。arena の 8 byte 境界の番地は `(*atomic.Uint64)(unsafe.Pointer(&mem[off]))` で扱える。BPF 側の `__sync_*` / `__atomic_*` と同じ番地を指せば、同じ cache line 上で互いに見える。
6. **race detector は arena を見ない。** Go 側の mutex は Go 同士の排他にしかならない。BPF との排他は、atomic、seqlock (版数の偶奇)、または「書く側を 1 つに決める」で作る。
7. **Go の goroutine は非同期に preempt される。** 「この 2 回の store の間は BPF に見えない」と期待しない。途中の状態を見せたくないなら、先に中身を書いて、最後に flag / offset を release store する。

### 3.3 ライフサイクルと fault

8. **`Munmap` は明示的な Close でやる。** finalizer に頼らない (回収のタイミングが不定で、順序も保証されない)。Close の後に `mem = nil` にして、以降の `ReadAt` / `WriteAt` が error を返す形にする (PoC の `arena.go`)。Close と読み書きが並行するなら RWMutex で守る。
9. **最初に触ったページは fault で確保される** (`WriteAt` のコメント、PoC で確認)。サイズは作成時に固定で、あとから伸ばせない (C9)。範囲外や確保失敗の fault は SIGBUS / SIGSEGV で、Go のランタイムは、`runtime/debug.SetPanicOnFault(true)` を goroutine ごとに呼んでいない限り、プロセスを fatal にする。fault を recover したいなら使えるが、**この PoC では試していない**。普通は fault を起こさないよう、offset と長さを `Size()` と照らして先に検査する (PoC の `ReadAt` / `WriteAt` がそう)。
10. **`copy` で大きく動かす。** arena は普通の `[]byte` なので `copy(mem[off:], p)` が memmove になる。1 バイトずつ atomic にしない。BPF が読む前に書き終わったことは、最後の release store (上の 7) で伝える。

### 3.4 BPF 側との分担

11. **page の確保は BPF 側 (`bpf_arena_alloc_pages`) に寄せるか、Go 側が全部持つかを最初に決める。** 混ぜると、同じページを両方が free する、片方が確保したページに他方が無断で触る、といった事故が出る。この PoC は「Go 側が byte 範囲を管理し、BPF は読むだけ」(§4)。
12. **BPF 側に長い pointer の鎖 (リスト、木) を作らせると、Go で読むのが辛い。** Go 側は offset から 1 回で引ける、平らな表 (配列 + index) にしておく方が読みやすく、検証もしやすい。

## 4. この PoC の実際: Go 側の allocator はこの大きさ

`daemon/internal/ebpf/arena_alloc.go` (約 200 行) が、arena のバイト範囲を管理している。やっていることは次のとおりで、**汎用 malloc ではない。**

| 部品 | 内容 |
|---|---|
| bump allocator | `cacheNextOff` を進めるだけ。arena の終端を超えたら `ErrCacheFull` |
| free list | `arenaChunk{offset,size}` の slice。**サイズ昇順に保ち**、`sort.Search` で best-fit (最小で収まる chunk) を O(log n) で引く |
| split | 空き chunk が大きければ、前半を渡して残りを free list に戻す |
| coalesce | 解放のたびに隣り合う chunk を合わせる。末尾の chunk は bump の位置まで戻す |
| 排他 | `cacheMu` の mutex 1 つ。Go の goroutine 同士の排他だけ |
| ページの populate | `PreallocArenaPages` が `SEC("syscall")` の BPF program 経由で `bpf_arena_alloc_pages` を呼び、user_vm と kern_vm の PTE を先に張る。byte 範囲の管理 (上の 4 行) とは別で、kmod が EFAULT を見ないための前処理 |

- 可変長の確保と解放が混ざる場合に必要な分 (best-fit、split、coalesce) だけを実装している。size class や thread-local cache など、本物の malloc にある部品は入れていない。**Go の mutex の下で 1 本化できるから**で、並列度を上げたくなったら size class の free list を並べる、が次の手になる。
- 読み書きは `Arena.ReadAt` / `WriteAt` (開始 offset の範囲検査つきの `copy`、終端を超える分は short read / write の error) だけで、型付きの view は作っていない。
- この allocator が使われるのは arena モード (既定) の admit で、ファイルの中身を arena に置く。pcache モード (`--arena-metadata-only`) は中身を kmod の chunk と page cache に置き、daemon は arena を作って mmap するが read 経路では使わない ([`../design/pcache-mode.md`](../design/pcache-mode.md) §1)。移った理由は、サイズ固定 (C9) と、kmod から読むと GUP が要ること ([`arena-access-paths.md`](arena-access-paths.md)) で、**Go 固有の問題ではない。**

## 5. 他の言語との比べ方 (一般論、PoC では確かめていない)

| | C / libbpf | Rust (aya など) | Go (cilium/ebpf) |
|---|---|---|---|
| `__arena` グローバル変数を skeleton から見る | できる (libbpf が `.addr_space.1` を処理) | ライブラリ次第 | **できない** (C4) |
| load の前に arena を mmap する | libbpf が作成時に mmap する形 (知識からの説明) | ライブラリ次第 | 自分で 2 段 load (C2) |
| arena 内の pointer を型で扱う | C の pointer がそのまま使える | `unsafe` が要る | `unsafe` が要る。GC 非対応なので型に pointer を持たせられない |
| arena 用のリスト / hash table | selftests に実装がある (`bpf_arena_list.h` など) | 自前 | 自前 |

つまり、Go が不利なのは「GC があるから」より、**周辺の道具 (libbpf の arena 対応、共通ヘッダ、selftests の資産) が C 向けに揃っていること**の方が大きい。arena を BPF の中だけで使い、Go が offset で外から読み書きする、という分担にすれば、その差はほとんど効かない。

## 6. 判断の目安

1. arena を **誰が確保し、誰が解放するか** を 1 か所に決める。決まらないなら arena をやめて BPF map にする。
2. arena に置く構造を **平らな配列 + offset** で書けるか。書けるなら Go で十分。リストや木が必須なら、その部分は BPF 側 (C) に閉じ、Go は読み出し用の関数だけにする。
3. arena のデータを **kmod / KFunc が別プロセスの文脈から読む** なら、Go の問題ではなく GUP の問題 ([`arena-access-paths.md`](arena-access-paths.md))。
4. サイズが **作成時に固定で足りるか** (C9)。足りなくなるなら、中身を arena に置く設計をやめる選択を早めに考える。

## 7. 未確認の点

- `SetPanicOnFault` が arena の範囲外 fault を実際に recover できるか。
- checkptr (`-race` / `-gcflags=-d=checkptr`) が、`unsafe.Add` で作った arena 内の pointer に何か出すか。
- libbpf が arena を作成時に mmap すること、`__arena` グローバル変数に対応していることは、知識からの説明。
- arena を大きくしたときの、Go のプロセスの RSS / memcg への見え方。
