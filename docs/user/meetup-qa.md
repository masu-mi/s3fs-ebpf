# 発表準備で出た質問と回答 (2026-10-13〜10-21)

meetup 発表 (`docs/user/meetup-ebpf-outline.md`) の準備中に出た質問と、その回答をまとめる。質疑応答の想定問答としても使う。回答のあとで分かったことがある項目には「その後」を付けた。根拠は benchmark-results の § 番号、backlog の ID、コードの位置で示す。

---

## 1. bpf_arena と KFunc

### Q1. KFunc で arena のデータをループすると GUP が起きて重いと聞いた。bpf_loop や iterator でも GUP は起きるか。bpf_loop はヘルパーで、カーネル側の関数ではないか

**起きない。GUP が要るかは「誰のコードが arena を読むか」で決まり、ループの書き方では決まらない。**

- bpf_loop の本体はカーネル関数だが、BPF のコールバックを N 回呼ぶだけで arena には触らない。arena を読むのはコールバック (BPF のコード) で、JIT が `__arena` ポインタを kern_vm の番地に変換して直接ロードする。
- open-coded iterator (`bpf_for` / `bpf_iter_num_*`) と `can_loop` / `cond_break` も、ループ本体が BPF のコードなので同じ。
- 重いのは KFunc が普通のカーネル C で書かれていて kern_vm を使えないから。`kmod/tree_traversal.c:97` の KFunc は `copy_from_user` で読んでいる。daemon の syscall 文脈なので厳密には GUP ではなく、ユーザーメモリのアクセスとページフォールトの処理。
- 本物の GUP (`pin_user_pages_remote`) が起きるのは、別プロセスの文脈から読む `.read_folio` や AA-03 の read 経路。

注: bpf_loop のインライン化など、カーネル内部の挙動は知識からの説明で、実測していない。

### Q2. KFunc が arena 内のデータ構造の iteration を提供し、要素アクセスと判定材料を返すハンドラを BPF 側で作って渡す構成なら、GUP を避けられるか

**要素を読むところは安くなるが、走査のコストは残る。** KFunc が次の要素へ進むには arena 内の `next` を読む必要があり、そこは普通のカーネル C から arena を読むことになる。

- daemon の syscall 文脈で呼ぶなら `copy_from_user` で済む。
- read 経路や struct_ops のフックのように他プロセスの文脈で呼ぶなら、`pin_user_pages_remote` (GUP) が要る。

GUP を避ける構成:
1. **リンクは kernel メモリ、payload だけを arena に置く。** KFunc は kmalloc したリンクだけをたどり、BPF のコールバックに arena のポインタを渡す。
2. **走査も BPF 側でやる** (`bpf_for` / `can_loop`)。KFunc は arena に触らない処理 (カーネル API、sleep) だけに使う。命令数の予算と完走の保証がない点は残る。

未確認: BPF のコールバックを引数に取る kfunc を外部モジュールが定義できるか。

スライド: 付録の arena-loop。

### Q3. kmod で kern_vm が使えない理由は。なぜ BPF では kern_vm に変換できるのか。BPF のフックに入る前に仮想メモリのマッピングが変わっているのか

**マッピングは変わっていない。arena は作成時から同じ物理ページを 2 か所にマップしていて、BPF は最初から kernel 側のアドレスを使っている。** 以下はカーネル v6.12 のソースで確認した。

- arena を作ると `get_vm_area(4 GiB + ガード)` で kern_vm を確保する (`kernel/bpf/arena.c:120`)。kernel のページテーブルに載るので、全プロセスで共通。
- user_vm (ユーザー空間の mmap) は daemon の mm にだけある。ページを割り当てるとき、同じページを `vm_area_map_pages` で kern_vm にも張る (arena.c:288、469)。
- 番地の対応は「kern_vm_start + ユーザーポインタの下位 32 bit」。
- JIT はプログラムの冒頭で kern_vm_start を r12 に入れ (`arch/x86/net/bpf_jit_comp.c:1430-1455`)、arena へのアクセスは `mov eax,eax; mov [rax+r12+off]` の 1 回のロードになる。「変換」はこの足し算のこと。
- 未割り当てのページに触れたときは、JIT が付けた例外テーブル (`ex_handler_bpf`) がロードを 0 にする。

kmod が使えない理由:
1. **正規の取得手段がない。** `bpf_arena_get_kern_vm_start()` は export されておらず、`struct bpf_arena` も arena.c の中だけで定義されている。この PoC は BTF から構造体のレイアウトを複製して番地を取っている (`kmod/s3fs_ebpf_dev.c`) が、カーネルの更新で壊れる。
2. **未割り当てのページで oops になる。** 普通のカーネル C には BPF のような例外テーブルがない。
3. **解放と競合する。** `bpf_arena_free_pages` は kern_vm のマップを外してからページを解放する (arena.c:525-535)。

read 経路が GUP を使う決め手は 2 と 3。スライド: 付録の arena-map、next の 4 (upstream への要望)。

### Q4. struct_ops と arena の適切な用例は

**判断は struct_ops に、共有する状態は arena に。**

- **struct_ops:** カーネルが判断を問い合わせる点と、アルゴリズムの判断単位が一致するときに向く。例は sched_ext (次に走らせるタスクを選ぶ)、TCP 輻輳制御 (`tcp_congestion_ops`)、HID-BPF。s3fs なら「どれを追い出すか」を聞く vtable (後の `pick_victim`)。
- **bpf_arena:** BPF とユーザー空間が、ポインタを含むデータ構造を syscall なしで共有したいときに向く。例は sched_ext のタスクごとの状態、BPF 側のリスト・木・ハッシュ。s3fs なら admit のメタデータとポリシーの状態だけを arena に置き、ページの実体は page cache に任せる。

スライド: usecases。

---

## 2. struct_ops とキャッシュポリシー

### Q5. BPF 版の SIEVE / LRU の hit rate が simulator より低いのはなぜか。改善できるか

**vtable の形が原因。** 2 点ある (`kmod/vfs_ops.c` の capacity モード)。

1. **順序の情報が捨てられる。** kmod は `on_evict` の戻り値 (score) の大きい順に evict し、同点は `hash_32(ino)` 順。LRU も SIEVE も 0/1 しか返さないので、同点の中はほぼランダムになる。
2. **問い合わせのたびに状態が変わる。** `on_evict` は ADMIT のたびに全 ino に呼ばれ、SIEVE はそのたびに全員の visited を落とす。

改善案は 2 つ: A は score を段階的に返す (BPF だけで直る)、B は `pick_victim()` で BPF が victim を選ぶ (kmod の変更が要る)。trace モデルでは A の LRU が 39.0%、B の SIEVE が 46.8% (現行 33.3%)。

その後: B を AC-58 として実装した (Q8)。

### Q6. SIEVE が負けるのは、アルゴリズムに必要な素性を kmod が提供できていないからか。統計値を提供するか、コア並列で集計できるフックポイントを提供すれば解決するのではないか

**半分正しい。足りないのは素性 (統計値) ではなく、BPF が選んだ順番を kmod に伝える口。統計値を足しても SIEVE は直らない。**

- SIEVE に要るのは visited ビット、挿入順 (FIFO)、hand の位置。visited は `on_hit` で BPF の map に持てている。FIFO と hand も BPF 側で持てる。
- 問題は `on_evict(ino)` が 1 つの ino に 0/1 を返すだけで、kmod が並べ替えと同点処理を持っていること (`kmod/policy_struct_ops.h`)。
- 統計値やコア並列の集計フックは、頻度を使うポリシー (LFU、TinyLFU) には効く。共有 map への書き込みのコストも下がる (fentry の件と同じ理屈)。

### Q7. victim を明確に指定できないと、サポートできるアルゴリズムは狭い。任意のアルゴリズムを差し込める汎用フックは、将来のアルゴリズムも踏まえた境界定義が要るので困難で、今見えている部分の境界を定めるのが限界ということか

**前半はその通り。後半は少し違う。** 境界を「判断材料」ではなく「役割」で切れば汎用にできる。

- **kernel 側 (仕組み):** イベントを通知する (insert / access / remove)。evict を実行し、BPF が返した victim を確かめる。おかしければ既定のポリシーに戻す。
- **BPF 側 (ポリシー):** 順番や状態のデータ構造をすべて持ち、聞かれたら victim を返す。

この形なら LRU、MRU、SIEVE、LFU、S3-FIFO は同じ口で書ける。sched_ext がスケジューラで同じ考え方をとっている (page cache では cache_ext という研究が近いはずだが、詳細は未確認)。

それでも残る限界:
1. kernel が観測できない情報 (アプリのヒント、将来のアクセス) は渡せない。
2. コスト。victim の選択は lock の下で呼ばれ、verifier のループ上限もある。hot path のフックが重いと fentry と同じことが起きる。
3. 安全性。BPF が返した victim を kernel 側で必ず確かめる必要がある。

### Q8. pick_victim 案を採用してチューニングで軽量化を目指す。アプリ側のヒントは eBPF maps 経由で渡して参照すれば、ある程度対応できるか

**できる。** pick_victim の形 (BPF がポリシーの状態を持つ) と相性がよい。

1. **キー:** BPF から見えるのは ino なので `fstat` の `st_ino`。ジョブ単位なら cgroup id をキーにし、`on_hit` で `bpf_get_current_cgroup_id()` と突き合わせる。
2. **書き込み権限:** map の更新には CAP_BPF か pin した map の権限が要る。daemon が持ち、アプリは API か xattr で渡す方が安全。cgroup ごとの上限も要る。
3. **アプリを変えない手段:** `fadvise` / `madvise` に fentry を付けて既存のヒントを拾う。呼ばれる頻度が低いので hot path の問題になりにくい。
4. **限界:** ヒントは粗く、アクセスの後に届いたものは効かない。

その後 (AC-58、§18.41-18.42、`docs/design/ac58-pick-victim.md`):
- vtable の末尾に `on_insert` / `on_remove` / `pick_victim` を追加。LRU / SIEVE / S3-FIFO を移した。
- hit rate は 3 seed で旧版より 2〜7 ポイント高い。S3-FIFO 36.7〜40.0%、LRU 34.9〜37.5%、SIEVE 35.0〜37.1%。
- read 経路のコストは、1 file を 1 byte ずつ読む最悪ケースの 64 jobs で、policy なし比 −4.6% (旧 SIEVE は on_hit の map update で −14.8%)。`pick_victim` 1 回は候補 8 件で 0.5〜0.9 µs。
- アプリのヒントは未着手。

---

## 3. 性能と FUSE との比較

### Q9. FUSE に勝つためには、どういう検証と変更が残っているか (2026-10-16 時点)

当時の回答: 負けていたのは page cache に載った後の読み込み (64 jobs の IOPS 0.66×、100 MiB の warm read 1.20×)。仮説として「kmod が large folio を有効にしていないので、folio の参照カウントの取り合いが起きている」を挙げ、folio の order の確認と `perf c2c` を提案した。

その後:
- **large folio の仮説は外れた。** 計測カーネル 6.12 では FUSE 側も order 0 だった。
- **本当の律速は daemon が付けた `fentry/generic_file_read_iter` だった** (`docs/design/read-iter-hook-overhead.md`、§18.16 / 18.23 / 18.24)。フックなしでは fuse-raw の 1.00〜1.02×。BPF_CORE_READ の probe_read、同じ ino の値への atomic などが内訳。直接参照と per-CPU 化で 0.96× まで戻した。
- 100 MiB の warm read は fuse-cache より速い (9.5 / 10.5 ms)。§18.2 の 1.20× は再現しなかった。
- 現在のビルドでは AB-07 の gate (a)(b)(c) がすべて PASS (§18.43)。

スライド: fail-found、hook-tips。

### Q10. ベンチ結果では FUSE に負けているが、勝てるポイントや用例を再整理したい

**「read が速い FS」ではなく「open と fsync が速い FS」。** FUSE は 1 回の open+read+close で /dev/fuse を 4 往復するが、s3fs-ebpf は miss のときだけ upcall する。

pcache モードで測り直した数字 (§18.27-18.32、ローカル MinIO):

| 場面 | 結果 |
|---|---|
| open+read+close (キャッシュなし同士) | 3.3× |
| open+read+close (キャッシュあり同士) | 172× |
| 4 KiB × 10,000 個の cat (2 回目) | 93× (fuse-cache 比 32×) |
| 4 KiB × 10,000 個の `ls -l` (初回) | 11× |
| 新規ファイルの durable write 1〜100 MiB | 1.37× |

向く使い方: 小さいファイルを大量に開く (学習データ、ログの走査)、短命なプロセスが多い (バッチ、CI)、新しいファイルに書いて fsync する (チェックポイント)、キャッシュの判断をワークロードごとに変えたい (機能の差)。

負けていた場面と、その後の修正 (§18.34-18.39):
- 既存の大きいオブジェクトの途中の書き換え: 100 MiB で 0.41× → AC-51 (`--partial-modify`、server-side copy) で fuse-raw と互角。16 MiB 未満は 0.61× のまま。
- 大きいファイルの cold read: 300 MiB で 0.34× → AC-52 で、毎回キャッシュを空にすれば 1.26×。続けて読むとホストのメモリ回収待ちで 0.80×。

スライド: perf、perf-lose、fs-usecases。

### Q11. hit rate など実測値の変化はどうか (2026-10-18 時点)

| policy | 旧 (score 型) | pick_victim 版 | trace 再生 |
|---|---|---|---|
| LRU | 31.7〜33.8% | 34.9〜37.5% | 39.2〜41.7% |
| SIEVE | 29.3〜32.0% | 35.0〜37.1% | 43.8〜48.0% |
| S3-FIFO | 31.8〜33.6% | 36.7〜40.0% | — |

- trace 再生との差 (4〜11 ポイント) は、実機の eval が 25 access ごとにしか回らないため。LRU にも同じ差が出る。
- **pcache モードの hit rate が 52〜54% (§18.1) から 29〜35% に下がって見えるのは退行ではない。** AC-44 で prewarm がメタデータだけになり、計測条件が変わった (§18.43)。発表で並べないこと。
- AB-07 の gate は §18.2 の 3 つとも FAIL から、現在のビルドで全部 PASS (64 jobs 0.983×、100 MiB cat 14.7 / 17.1 ms、microbench 1.07×)。

どれも n は 1〜10 と少なく、S3 はローカルの MinIO。

### Q12. pcache モードと arena モードの違いは

**ファイルの中身 (ページの実体) をどこに置くか。**

| | arena モード (元の方式) | pcache モード (`--arena-metadata-only`) |
|---|---|---|
| 中身の置き場所 | bpf_arena (daemon と BPF の共有メモリ、128 MiB) | fd ごとの 4 MiB chunk の表と page cache |
| arena に置くもの | 中身とメタデータ | admit したファイル・範囲と policy の状態だけ |
| read の経路 | kmod が arena からコピー。kmod から見た arena は daemon のメモリなので毎回 GUP (1 回約 1.7 µs) | 普通の `read_iter` / page cache。2 回目以降は kmod の中で完結 |
| 容量 | arena の大きさ | `capacity_bytes` (既定は無制限) で chunk を数える |
| 弱み | 毎回の GUP。並列 read が伸びなかった (AA-03) | 読んだ page を残すので、大きいファイルを続けて読むとメモリ回収を待つ (§18.39)。pin などの不変条件が増えて複雑 (`docs/kmod-pcache-invariants.md`) |

切り替えた理由は、失敗談の「誰から見ると何に見えるか」の 3 行目。arena は BPF とユーザー空間の共有には向くが、kmod の hot path から毎回読む場所には向かない。スライド: next の 1 (page cache に戻す)。

---

## 4. 振り返り

### Q13. bpf_arena を使う意義はあったか (2026-10-21 時点)

**性能の面ではなかった。学びと発表の材料としてはあった。** この PoC で arena が担った「ファイルの中身の置き場所」は arena に向かない役割で、速さの源は arena ではなく「hit のときに upcall しない」構造の方だった。arena に向く役割 (BPF と daemon が共有するポリシーの状態) には、結局まだ使っていない。

**性能の面で意義がなかった根拠**

- **初期の大きな倍率は arena の手柄ではなかった。** arena モードで出した open+read+close の約 169 倍 (fuse-cache 比、§7.5) は、中身を arena に置かない pcache モードでも 172 倍 (§18.27) で同じ水準になった。小さいファイル 10,000 個の warm cat 93 倍 (§18.31) も open で upcall しないことが効いている。キャッシュなし同士の 3.3 倍も pcache モードの数字。
- **kmod から見た arena は「daemon のユーザーメモリ」だった。** arena の「ゼロコピー」は BPF とユーザー空間の間の話で、kmod は kern_vm を正規には使えない (Q3、`dev-notes/ebpf-ecosystem-gaps.md` A1〜A3)。read のたびに GUP が要り、1 回約 1.7 µs、1 MiB のファイルでは 1 回の `read()` で約 442 µs になった (§13.1)。1 MiB の cat は page cache 経路の方が速かった (§13.2)。
- **容量が固定だった。** arena は 128 MiB (BPF 側でコンパイル時に固定) で、100 MiB のオブジェクトが 1 つ入ると他が admit できなかった (§18.2)。大きいファイルのために chunk と range の admit を足し、それが AC-48 / AC-49 のバグを生んだ。
- **pcache モードに移っても性能は落ちなかった。** 64 jobs の read IOPS、100 MiB の cat、microbench、write はどれも arena モードと同じか少し良い (§14、§16、§17)。hit rate も下がらなかった (§16.2)。
- 注意: 64 jobs の read が fuse-raw に負けた主因は arena ではなかった。pcache モードでも 0.64× のままで (§14.1)、本当の律速は `fentry/generic_file_read_iter` だった (Q9)。arena を外しても、それだけで勝てたわけではない。

**今の arena の使われ方 (コードで確認)**

- 既定の arena モードでは、daemon がファイルの中身を arena に書き、kmod が GUP で読む (`daemon/internal/ebpf/loader.go`、`kmod/vfs_ops.c` の `s3fs_arena_copy_to_iter`)。
- pcache モード (`--arena-metadata-only`) では、中身は `POPULATE_PAGES` で kmod の chunk に渡り、arena には何も書かない (`daemon/cmd/s3fs-ebpf/admit_arena.go` の AB-04 の分岐、AC-44 で prewarm もメタデータだけ)。admit の表は kmod の hash にあり、arena ではない。名前は「metadata-only」だが、実際には arena を使っていない。
- hot path の BPF プログラム (`fentry/vfs_getattr_nosec`、`fentry/generic_file_read_iter`) は arena を読まない。ポリシーの状態は HASH / ARRAY map に置いている (`bpf/policy_*_ops.bpf.c`)。
- arena を `__arena` ポインタで読むのは、verifier の拒否デモ (`s3fs_deep_walk_reject`)、prealloc、`make test-arena` などの syscall プログラムだけ (`bpf/s3fs_fastpath.bpf.c`)。

**意義があったところ**

1. **適用範囲の境界が実測で分かった。** 「判断は struct_ops、共有する状態は arena」(Q4) と、「arena は BPF ↔ ユーザー空間の共有には向くが、kmod の hot path から毎回読む場所には向かない」(Q12) は、arena を誤った場所で使って計測したから言える。発表の中心 (失敗談 AA-03) になっている。
2. **arena の仕組みを v6.12 のソースで確かめた。** 同じ物理ページを user_vm と kern_vm に二重にマップし、JIT が r12 = kern_vm_start で変換する、未割り当てのページは例外テーブルで 0 になる、という説明 (Q3) は arena を使ったから得られた。
3. **verifier と KFunc のデモの土台になった。** arena 上の linked list を `while (n)` でたどると verifier が拒否し、KFunc に渡すと通る、というデモ (`daemon/cmd/s3fs-ebpf/verifier_demo_kfunc.go`) は arena がないと成り立たない。
4. **upstream への具体的な要望が出た。** `bpf_arena_get_kern_vm_start` の export (K-01)、kern_vm の generic context 用 fault handler (K-02)、cilium/ebpf の `addr_space_cast` の Marshal 修正 (`docs/misc/`)。K-02 が入れば kmod から GUP なしで読めるので、arena に中身を置く設計の評価は変わりうる。

**やり直すなら**

- 中身は最初から page cache に置く (pcache モードの形)。
- arena は、BPF のポリシーと daemon が共有する状態に使う。例: `pick_victim` の FIFO / ghost queue のような、ポインタを含むデータ構造 (いまは ARRAY map の添字で表している)、アプリのヒントの表 (Q8)、ヒストグラムなどの統計。daemon が syscall なしで読み書きでき、arena の利点がそのまま出る。ただし、このやり方での arena の効果はまだ測っていない。
- arena モードを既定から外すか、arena モードそのものを消すかは決めていない (`docs/design/pcache-mode.md` §9)。

スライド: arena-who、失敗談 (AA-03)、usecases。

### Q14. CEL で複雑なキャッシュ / prewarm のポリシーを試したことはあるか (2026-10-21 時点)

**ない。** 実際にマウントして使った CEL ポリシーは 1 行の単純なものだけで、複数行の「複雑な」サンプル 2 本はそもそも読み込めない状態だった。

これまでに使ったもの:
- `prewarm: false` (`scripts/test-pcache-prewarm-none.sh`、`test-arena-partial-readback.sh`、`policies/prewarm-meta-only.cel`)。
- `prewarm: entry.size <= 4194304` (`policies/bench-no-large-prewarm.cel`、§18.17 の pattern-aware の計測)。
- `policies/default.cel` (`metric()` + サイズ + 拡張子の 1 行と、`policy:` で SIEVE を選ぶ)。P-01 / P-03 の spike で動作を確認。

見つかった問題 (この PR で修正):
- ローダーは 1 行 1 hook で、続きの行を前の式に連結しなかった。`policies/prewarm-models.cel` と `prewarm-tiered.cel` は 2 行目で `syntax: expected 'hook: expr'` になり、daemon が起動できなかった。`docs/user/writing-policies.md` のレシピ 2 / 3 も同じで、行末の `#` コメントも CEL では構文エラーになる。
- CEL の hook には `daemon/internal/policy` のテストがなかった。

修正: 空白で始まる行を前の式の続きとして連結するようにし、`policies/*.cel` を全部読み込むテストと、少し複雑な例 `policies/prewarm-dataset.cel` (除外の正規表現、リストと `exists`、複数行) の判定表のテストを足した。マウントしての計測はまだしていない。

CEL でできることの範囲 (誤解しやすいので):
- hook は 2 つだけ。`prewarm` (daemon 起動時に 1 回、ファイルごとに bool) と `policy` (起動時に 1 回、Layer 2 の BPF プログラム名)。**どれを追い出すか (キャッシュの eviction) は CEL では書けず、BPF の struct_ops (Layer 2 / 3) の役目。** CEL は「どの BPF ポリシーを使うか」を選ぶだけ。
- 入力は `entry` の 5 項目 (ino / name / s3_key / size / mode) だけで、アクセス頻度や時刻、cgroup は見えない。
- **pcache モードでは中身の prewarm をしない (AC-44) ので、`prewarm` の CEL は arena モードでしか効かない。** 計測の主軸を pcache モードに移してからは、CEL の prewarm は実質的に使われていない。
- ポリシーを書き換えたら daemon の再起動が要る (BPF の `policy` は `POST /policy/reload` で差し替えられる)。

例と判定表: `docs/user/writing-policies.md` のレシピ 5。
