# read の fentry フックの負担: 調査記録と、フックを使うときの注意点 (2026-10-16)

AB-07 の性能 gate (a)「64 jobs の psync read で、ebpf-pcache の IOPS が fuse-raw 以上」が落ち続けていた原因を調べた記録。被疑箇所に気づいた経緯、行った試験と結果、そこから分かったフックポイントを使うときの注意点と、フックを採用するかどうかの判断基準をまとめる。数値の詳細は `docs/journal/benchmark-results.md` §18.16、残りの課題は `docs/journal/backlog.md` の AC-47。

## 1. 結論

- gate (a) の差は、daemon が付けている `fentry/generic_file_read_iter` (R-01、`bpf/s3fs_fastpath.bpf.c` の `s3fs_fp_read_iter`) から来ていた。このフックを外すと、ebpf-pcache の IOPS は fuse-raw と同じになる。kmod の read の経路 (`fc_pcache_check` → `generic_file_read_iter`) は原因ではなかった。
- フックの負担は、64 jobs の fio で CPU 時間の約 19%。内訳は次のとおり。
  - `BPF_CORE_READ` が使う `bpf_probe_read_kernel`: 約 7%
  - pattern map の更新 (同じ ino の値に atomic 3 回と書き込み 1 回): 約 8%
  - inode_index の lookup と policy の hit 処理: 約 3%
  - trampoline / rcu / migrate: 約 2%
- 1 つ目は、ポインタを直接たどる形に変えて解消した (a4e7b0f)。gate (a) は fuse-raw 比で 0.81 から 0.87 になった。挙動は変わらない。
- 2 つ目と 3 つ目は残っている (AC-47)。
- gate (b) (100 MiB の warm read) は、今のコードでは PASS している。「FUSE は large folio を使っているので速い」という仮説は、計測カーネル (6.12) では成り立たなかった。

## 2. 被疑箇所に気づいた経緯

1. **出発点**: §18.2 の gate は (a) 0.66 倍、(b) 1.20 倍で FAIL だった。最初の仮説は「FUSE は large folio を使い、kmod は 4 KiB の folio だけなので、folio の参照カウントの atomic を取り合っている」というもので、(a) と (b) を同じ原因で説明しようとしていた。
2. **前提を確かめる**: 計画を立てる前に、仮説が置いている前提をコードと環境で確かめた。
   - 計測カーネルは 6.18 ではなく 6.12 だった (`uname -r`)。
   - gate (a) の fio は `--size=1 --bs=1` で、18 B のファイルを読む。page cache の folio は、どの FS でも 4 KiB が 1 枚だけになる。folio のサイズを変えても参照カウントを操作する回数は変わらないので、large folio では (a) を説明できない。
3. **read の経路を最後まで追う**: kmod 側の read (`s3fs_ebpf_read_iter`) は、pcache で hit したときは fd ごとのキャッシュを見て `atomic_read` を 1 回するだけで、ロックも共有データへの書き込みもない。一方で、同じ read の途中で動く BPF の fentry (`s3fs_fp_read_iter`) を読むと、read のたびに次の処理をしていた。
   - `s3fs_read_pattern_map` の **同じ ino の値に、`__sync_fetch_and_add` を 3 回と、普通の書き込みを 1 回** 行う。64 jobs が同じファイルを読むと、全員が同じ cacheline に書き込む。
   - `s3fs_inode_index` の lookup をして、kfunc で policy の on_hit を呼ぶ。
   - FUSE の read では s_magic の判定ですぐに return するので、FUSE 側にはこの負担がない。
4. **以前の計測が見逃していた理由**: このフックの負担は `scripts/bench-read-iter.sh` で測ってあったが、これは 1 スレッドでの 1 回あたりの遅延 (「1 µs 未満」) を測るものだった。同じ値への書き込みの取り合いは並列度を上げたときにだけ出るので、1 スレッドの計測には現れない。過去の記録にある「8 jobs で頭打ち (~4300k)」も、共有された値の取り合いのパターンに合っていた。

この段階では、まだ仮説だった。そこで、§3 の試験で順に確かめた。

## 3. 試験項目と結果

計測環境: KVM ゲスト、6 vCPU、カーネル 6.12。fio は gate と同じ `--ioengine=psync --rw=read --numjobs=64 --size=1 --bs=1 --thread --runtime=5`、毎回 `drop_caches=1` してから測る。ebpf は pcache モード (`--arena-metadata-only --policy-bpf=builtin:policy_observe --softcap=8 --cache-capacity=0`)。各条件 3 回。

| # | 試験 | 目的 | 結果 |
|---|---|---|---|
| T1 | fentry あり / なしの A/B (`--no-read-iter-hook --pattern-aware-admit=false`) | フックが原因かを確かめる | あり 4900k〜4965k、なし 6071k〜6145k、fuse-raw 6126k〜6145k。フックを外すと差は消える |
| T2 | `--pattern-aware-admit=false` だけ (fentry は残す) | フラグの影響か、フック本体の影響かを分ける | 4959k〜4992k。フック本体が原因 |
| T3 | fio 実行中の `perf record -a -g` | フックの中の負担を内訳に分ける | §1 の内訳 (probe_read 6.8%、プログラム本体 5.6%、hash lookup 4.1%、trampoline ほか 2.4%) |
| T4 | BPF の変種で A/B (B0 = 今のソースを作り直したもの、V1 = 直接参照、V2 = V1 + pattern の更新なし、V3 = V1 + inode_index / policy の処理なし) | 処理ごとの効き方を測る (`perf c2c` の代わり) | B0 4822k〜4927k、V1 5272k〜5284k (+8%)、V2 5656k〜5683k (+16%)、V3 5352k〜5488k (+11%) |
| T5 | `tracepoint:filemap:mm_filemap_add_to_page_cache` で、100 MiB を読んだときの folio の order を数える | large folio 仮説を確かめる | fuse-cache も ebpf も order 0 が 25,600 件ずつ。仮説は成り立たない |
| T6 | 100 MiB の warm cat を `hyperfine` で比べる (`-N` と `sudo cat` の両方、15 回) | gate (b) が再現するか確かめる | ebpf 9.5 ms / fuse-cache 10.5 ms (`-N`)、約 14 ms / 約 15 ms (`sudo cat`)。再現しない |
| T7 | 本物の `scripts/bench-ab-pcache.sh` を B0 と V1 で 1 回ずつ | gate の判定を出す | B0: a 0.81 / b PASS / c PASS、V1: a 0.87 / b PASS / c PASS |
| T8 | `make test-regression` (8 本) と `go test ./...` | V1 で正しさが崩れていないか確かめる | すべて PASS / ok |

補足:

- `perf c2c` は KVM ゲストでは memory events が使えず、動かなかった (`memory events not supported`)。そのため、どの cacheline を取り合っているかは、BPF の変種を作って A/B で比べる方法 (T4) で代わりに確かめた。
- `/usr/bin/perf` はラッパーで、6.12 用の linux-tools がないと止まる。`/usr/lib/linux-tools-6.8.0-146/perf` を直接使えば、record と report は動く。
- bpf2go の `.o` は、ソースを変えなくても、手元の toolchain で作り直すとバイナリが少し変わる。そのため、比べる基準には、コミット済みの `.o` ではなく、今のソースを作り直した B0 を使った。
- V2 と V3 は、処理ごとの効き方を測るためだけの版で、採用していない。

## 4. フックポイントを使うときの注意点

### 4.1 汎用の関数に付けたフックは、すべての FS の負担になる

`generic_file_read_iter` のような汎用の関数に付けた fentry は、この FS 以外 (FUSE や ext4 など) の read でも毎回動く。s_magic で自分の FS かどうかを判定する処理は、**フックの中で最初に、できるだけ少ない処理で** 行う。判定より前に lookup や atomic を置かない。

### 4.2 型の付いた引数は、ポインタを直接たどる

fentry / fexit / tp_btf の引数には BTF の型が付いているので、`iocb->ki_filp->f_inode` のように直接たどれる。直接たどると、fault しても安全な load 命令になり、CO-RE の relocation も付く。

`BPF_CORE_READ` は、フィールドを 1 つ読むたびに `bpf_probe_read_kernel` (関数呼び出しと、アクセスしてよいアドレスかのチェック) を通る。read のたびに 7 回読んでいたので、CPU の約 7% になっていた。`BPF_CORE_READ` が必要になるのは、kprobe や raw tracepoint のように型の付いていないポインタを扱うときだけである。

### 4.3 呼ばれるたびに共有された値へ書かない

同じキー (例えば同じ ino) の map の値に、呼ばれるたびに atomic で書くと、並列に読む jobs の間で cacheline を取り合う。次のような対策がある。

- `PERCPU_HASH` / `LRU_PERCPU_HASH` などの per-CPU の map を使い、読む側 (daemon) で CPU ごとの値を合算する。
- 値が変わるときだけ書く。
- 数え上げを間引く。
- 判断に必要な数に達したら、それ以上数えない。

カウンタの `s3fs_counters` は `PERCPU_ARRAY` なので、この問題はない。

### 4.4 kfunc の負担は、使う policy によって変わる

フックの中から kfunc で kmod の policy を呼ぶと、その負担は選んでいる policy によって変わる。今回は observe policy で測ったので、policy の処理はプロファイルの上位に出なかった。他の policy では、read のたびに policy の状態を書き換える可能性がある。フックの負担を測るときは、policy も変えて確かめる。

### 4.5 1 スレッドの計測では、並列のときの負担が見えない

1 回あたりの遅延が 1 µs 未満でも、全員が同じ値に書き込むと、並列度に応じて負担が増える。フックの負担は、**numjobs を 1 から 64 まで変えた fio の IOPS を、フックなしの場合と比べて** 測る。そのために、フックを止めるフラグ (`--no-read-iter-hook` のようなもの) は必ず残しておく。

### 4.6 page cache で hit する read は、フックの負担がいちばん目立つ

page cache で hit する read は、kmod の処理がほとんどなく、とても軽い。そのため、フックの負担がそのまま全体の速さの差として見える。フックの目的が miss のときの判断 (例: pattern-aware admit) なら、すべての read で集めるのではなく、miss の経路 (`.read_folio` や upcall) で集める形にできないかを先に考える。

## 5. フックを採用するかどうかの判断基準

page cache で hit する read の経路に入るフックや変更は、次の基準で採否を決める。

1. **負担を測っていること**: 64 jobs の psync read (gate (a) と同じ条件) で、フックなしと比べた IOPS を 3 回以上測る。1 スレッドの遅延 (`bench-read-iter.sh`) も合わせて記録する。
2. **効果を測っていること**: フックで得たいもの (例: cold read の S3 往復の削減) を数字で示す。効果が miss のときにしか出ないなら、hit のときの負担と見合っているかを説明する。
3. **挙動を変えない最適化** (今回の V1 のようなもの) は、次の 3 つを満たせば採用する。
   - 回帰テスト 8 本と go test が通る。
   - 3 回の計測の範囲が、変更前と重ならない程度に良くなっている。
   - gate のほかの項目が悪くなっていない。
4. **挙動を変える最適化** (例: pattern map を per-CPU にする) は、3 の条件に加えて、daemon の判断が変わらないことを確かめる。pattern map の場合は、`SeqRatio` による full admit の判定と、`LastEndPos` を使った先読みの判定が変わらないことを、unit test か、upcall の回数の比較で確かめる。
5. **計測のためだけの変種** (V2 / V3 のようなもの) は採用しない。結果は記録に残し、次の設計の材料にする。
6. **gate の基準**: AB-07 gate (a) は「fuse-raw 以上」なので、フックが残る限り、フックなしの場合よりわずかに遅いことは避けられない。それでも、フックなしとの差が計測のばらつき (約 2%) に収まるところまで減らすことを目標にする。そこまで減らせない場合は、gate をフックなしの構成で判定するか、フックの目的を miss の経路で実現する方法に切り替えるかを決める。

## 6. 残りの課題 (AC-47)

| 項目 | 負担 | 案 |
|---|---|---|
| pattern map の更新 | 約 8% | `LRU_PERCPU_HASH` にして、daemon 側で CPU ごとの値を合算する (`LastEndPos` は最大値を使う)。または、判断に必要な数に達したら数えるのをやめる |
| inode_index の lookup + policy の hit 処理 | 約 3% | policy に伝える hit を間引く、または page cache で hit したものは伝えない |
| trampoline などフック自体の固定の負担 | 約 2% | フックを使う限り残る。§5 の 6 の判断が必要 |

## 関連

- `docs/design/ebpf-hook-gaps.md` §3 — R-01 (`fentry/generic_file_read_iter`) の設計
- `docs/journal/benchmark-results.md` §18.2 / §18.16 — gate の計測値
- `scripts/bench-ab-pcache.sh` — AB-07 gate
- `scripts/bench-read-iter.sh` — 1 スレッドでのフックの負担
