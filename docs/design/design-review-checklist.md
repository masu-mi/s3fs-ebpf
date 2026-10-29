# 設計・実装・測定の検討観点 (issue #22 で見つけた不具合から)

状態: 2026-10-29。issue #22 (`hit_accum`) の調査で見つけた不具合 (`docs/journal/issue22-hit-accum-investigation.md` §4) を、**同じ種類の不具合を設計の段階で止める**ための検討観点にまとめた。不具合の経緯は調査まとめ、数値は `docs/journal/benchmark-results.md` §18.44〜18.46、ロックの不変条件は `docs/kmod-pcache-invariants.md` を見る。ここには、次に似た機能を設計するときに先に問う項目だけを書く。

使い方: 設計メモを書くとき (§1〜§6)、レビューのとき (末尾のチェックリスト)、測定を計画するとき (§5) に読む。各項目は「なぜ起きたか」「問うこと」「このリポジトリでの仕組み」の順。

## 1. 「同時に直す箇所」を作らない

**起きたこと**: bench のファイル内容と `FILE_BYTES=44` が別々に書かれ、実ファイルは 46 B だった (capacity が 1 slot 少ない)。policy への `on_insert` が arena と pcache の 2 つのループにコピーされ、pending の規則が片方にしかなかった。「policy を忘れさせる」処理が evict の経路ごとに散っていた。

**問うこと**
- ある規則 (「in-flight の ino は入れない」「evict したら状態を忘れる」) を、**何箇所で**書いているか。2 箇所以上なら、1 つの関数に寄せられないか。
- 定数が、別の場所の値から**導けるもの**ではないか (ファイルのサイズ、capacity、slot 数)。導けるなら書かずに測る。
- 「ここを変えたらあそこも変える」と書かないと保てない構造になっていないか。

**仕組み**: `s3fs_policy_track` / `untrack` / `trackable` と `admit_evict_ino` への集約 (kmod)。`hitrate_content()` から `FILE_BYTES` を測り、seed 後に `mc stat` で確かめる (bench)。`make check-invariants` が、集約した関数の外からの呼び出しと、サイズの直書きを grep で止める。

## 2. 周期処理と「全部やり直す」操作の副作用

**起きたこと**: `s3fs_ebpf_entries_replace` が、bucket の listing が変わらなくても毎回 `shrink_dcache_sb` を呼んでいた。使われていない dentry がすべて落ち、idle な inode が evict され、chunk が free される。sync (既定 30 秒) のたびに cache の内容、capacity の計上、policy の見え方が壊れた。

**問うこと**
- 周期的に走る処理が、**何も変わっていないとき**に副作用を起こしていないか。冪等な更新は no-op でなければならない。
- 大きな操作 (全 dentry の破棄、全体の再構築) を呼ぶ前に、**差分があるか**を見ているか。
- その操作が、別の場所の状態 (容量の計上、policy の状態、「entry があれば chunk がある」という R1) を巻き込まないか。
- 周期を極端に短く / 長くして、定常状態が変わらないか。

**仕組み**: `dyn_entries_equal` で listing が同じなら shrink しない。`make test-sync-keeps-cache` が、sync を 3 回挟んでも upcall が増えないことを見る。

## 3. 2 つの持ち主がいる状態: 提案と確定を分ける

**起きたこと**: policy (BPF) と kmod が、それぞれ「どの ino が resident か」を持つ。kmod は miss の最中の ino を evict できない (shed になる) のに、policy はそれを victim に選び、ghost / hand を先に動かしていた。S3-FIFO では新しい ino が ghost に入ったまま cache に残り、次の `on_insert` で M に直行した。

**問うこと**
- 状態を 2 か所が持つとき、**片方が実行できない操作**を、もう片方が先に確定していないか。
- 順序は「選ぶ → 実行する → 確定する (policy に伝える)」になっているか。実行に失敗する可能性があるものを、確定の前に取り除けるか (候補にしない)。
- 一方が状態を失ったとき (evict、inode の解放) に、もう一方が**忘れる経路**が全部そろっているか。出口は 1 つに集約できるか (§1)。
- 「in-flight」「pending」「limbo」のような中間状態を、policy が知らなくてよい設計か。

**仕組み**: pending の ino は `on_insert` / `on_size` / hit の collect の対象にしない (`s3fs_policy_trackable`)。evict の唯一の出口 `admit_evict_ino` で hit 状態を忘れる。KUnit `s3k_policy_trackable`、`s3k_vfs_forget_on_evict`。

## 4. 「数える」ことの意味を決める

**起きたこと**: hit の信号が、fentry (jiffy に 1 回に間引き)、`.read_folio` (folio ごと)、arena 読み (read ごと) と、経路ごとに単位が違った。大きいファイルは 1 access が `on_hit` 約 82 回になった。後の版では、cache にない ino の access を捨てたため、頻度の sketch が simulator とずれた。

**問うこと**
- **何を 1 回と数えるか**を 1 行で言えるか (ここでは「開いたファイル 1 つにつき、一定時間に 1 回」)。経路が複数あっても、同じ単位になっているか。`sendfile` や `splice` のような裏口は。
- その policy が**何の頻度を比べるか**。W-TinyLFU の admission は、候補 (まだ cache にない) の頻度と victim の頻度を比べる。cache にない ino の access を捨てると、機能の前提が壊れる。
- 最初の観測、観測の中断 (evict の間)、再開の扱いは。捨てるのか、数えるのか、いつ反映するのか。
- 反映する順序は、アルゴリズムの前提 (新しさ、頻度、挿入順) を壊さないか。既知の ino の hit は、この round で入る ino より古い、など。
- 数えるコスト (hot path) と、反映するコスト (mutex の下) を、それぞれ見積もったか。

**仕組み**: `.read_iter` と `.splice_read` で open ごとに数える。反映は `admit_replace` の Phase 1 で、既知の ino と新しい ino の populate 前の分を `on_insert` の前に、populate 後の分を後に。`make test-splice-hits`、KUnit `s3k_vfs_hit_count`、`s3k_hit_replay_order`。

## 5. アルゴリズムの実装は、参照と 1 手ずつ比べる

**起きたこと**: SIEVE の hand が newest 端にあるとき、次の挿入で新しい item の上に乗り、newest を evict した。S3-FIFO の ghost の下限 16 は、8 slot の cache で参照の 2 倍以上を覚えさせた。W-TinyLFU は、simulator が `100+idx`、kernel が `105+idx` の ino を hash して、衝突が違った。

**問うこと**
- 実装を、**参照 (simulator、論文の疑似コード) と同じ入力で 1 手ずつ**比べる仕組みがあるか。hit 率の平均が近いだけでは、これらは見つからない。
- 参照のほうが正しいか。入力は同じか (ラベル、hash に入る値、capacity、ファイルサイズ)。参照と実装の**前提のずれ**が、差の原因になりうる。
- **小さい cache、境界、端**で動かしたか。floor、丸め (window 1% が 0 個)、ポインタが先頭・末尾にあるとき、空のとき。大きい cache でしか効かない定数は、小さい cache で壊れる。
- 状態が、「そのときの瞬間の値」(S+M の数) に依存して揺れないか。必要なら最大値や不変な量に束ねる。
- 参照がモデル化していない部分 (round の遅れ、pending、limbo) は、何を見落とすかを書いたか。

**仕組み**: `dev trace-replay --per-access`、`PER_ACCESS_LOG`、`WORKLOAD_FILE` で live と simulator の hit / miss を access ごとに突き合わせ、最初に食い違う所を bpftrace の event と並べて読む。`make test-policy-conformance` が、4 つの policy で hit 数の差と食い違う割合を見る。kernel の ino を replay する。

## 6. 測定は、A/B だけでなく基準と対照を置く

**起きたこと**: 最初に報告した「`hit_accum` で hit rate が +9〜11 pt」は、大半が §1〜§5 の不具合によるもので、`hit_accum` の効果ではなかった。「simulator との差の原因は評価の間隔」という説明 (§18.41) も、確かめずに受け入れていて、誤りだった。

**問うこと**
- A/B の**両方が同じ誤りの下にある**可能性を考えたか。絶対値を、独立した基準 (simulator、上限の `opt`) と比べる。
- 信号を使わない**対照** (`observe`) を置いて、ノイズの大きさを知っているか。差がそれより小さいなら、効果とは言わない。
- 差を見つけたら、**原因を 1 つに絞る前に**、条件の違い (capacity、入力、周期処理、時刻) を洗ったか。
- 指標が**意味する通りの値か**。例: pcache での `hit_rate = 1 − miss/N` は、大きいファイルで miss が N を超えて負になる。
- 時刻に依存する要素 (周期の sync、drain、stats の tick、sleep) が結果を動かしていないか。止められるものは止め、止められないものは、再実行して分布で見る。
- 「測っていない」ことを、測った結果と区別して書いているか。

**仕組み**: `observe` を含む 7 policy の中央値 (3 seed)。`SYNC_EVERY=3600s` で決定的にする。結果は `benchmark-results.md` に条件つきで残す。

## 7. テストは、不具合を戻すと落ちるまで信用しない

**起きたこと**: conformance テストのリトライは、`bench-hitrate.sh` がログに追記するため、2 回目も 1 回目の行を読んで同じ結果を返していた (一度も再実行していなかった)。lockdep の判定は、`/proc/lockdep_stats` が root 専用 (`0400`) で、一般ユーザーの `[[ -r ]]` が「無い」と誤判定した。KUnit は、初回から全部 PASS した。

**問うこと**
- 不具合を**わざと戻して**、そのテストが落ちることを確かめたか (負の対照)。初回で通ったテストは、何も確かめていないかもしれない。
- 再試行は、**本当に別の走行**か (状態、ログ、出力先が前回と共有されていないか)。
- テストを走らせる**ユーザーの権限**で動くか (root 専用のファイル)。
- 時刻や並行性で揺れるなら、決定的にする (周期処理を止める)、または統計的に扱う (別の走行で再試行、許容幅)。揺れを許容する根拠を書く。
- 環境が違うと意味を失う検査は、**理由つきで SKIP** にする (lockdep カーネルでは遅くて hit 数がずれる)。FAIL にしない。
- 環境に依存するスクリプト (grub) は、fixture と `DRY_RUN` で、root なしに試せるか。

**仕組み**: 各テストの追加時に、戻して FAIL することを確認し、その結果を調査まとめの §7 に残した。`make test-boot-kernel` は fixture で動く。`CONFORMANCE_ON_LOCKDEP=1` で SKIP を外せる。

## 8. 並行性とロック

**問うこと**
- 各フィールドに、**「どのロックの下でだけ触る」**を書いたか (`hit_flushed` は evict の mutex の下、`hit_base` は fd->lock の下で書き、lock なしで読む)。
- 機能のフラグを既定で on にすると、**他の経路のロック取得が変わる**ことはないか。今回は、`pick_victim` を持たない policy でも `admit_replace` が evict の mutex を取るようになった。
- ロックの中で呼ぶ関数が、別のロックを取らないか (`ilookup` / `iput` が `evict_inode` まで走る経路)。
- 実際に lockdep の下で回したか。

**仕組み**: `docs/kmod-pcache-invariants.md` のロック順と、`make setup-kernel-lockdep` / `make test-lockdep`。

## 9. 機能の出し方

**問うこと**
- 既定を変える前に、**off で出して測り、証拠で on にする**手順を踏んだか。今回は、read 経路のコスト、単位の統一、G5 の解消を根拠にして既定を on にした。hit rate の上乗せは小さかった。
- **戻す手段**があるか (`hit_accum=0`)。
- 既定を変える判断の**根拠と、まだ測っていないこと**を残したか。

## 10. 環境と運用

- 自前ビルドのカーネルの headers には `resolve_btfids` が入らない。module の BTF が、ビルドの最後で壊れる。
- カーネルを入れると、grub の既定 (`GRUB_DEFAULT=0`) は**最新のカーネル**になる。性能を測るカーネルを取り違えない。`GRUB_DEFAULT=saved` にして明示的に選ぶ (`scripts/boot-kernel.sh`)。
- ディスク: debug info 付きの全 module のビルドは 40 GB 以上。必要な容量を、ビルドの前に確かめて止める。
- lockdep カーネルは遅い。性能の数字は取らない。

## 11. 記録と報告

- 誤りが分かったら、**元の記述を訂正**する (§18.41 の注記)。書き足すだけにしない。
- 数値には、条件 (kernel、policy、seed、sync の間隔、capacity) を添える。
- 「測っていない」「推測」を、事実と区別して書く。推測を確かめたら、結果で置き換える。

## 12. レビュー用チェックリスト

新しい機能や policy の変更を見るとき、次を順に問う。

1. 同じ規則が 2 箇所以上に書かれていないか。定数は導けるものではないか (§1)
2. 周期処理が、何も変わらないときに副作用を起こさないか (§2)
3. 2 つの持ち主の状態で、片方が実行できないものを先に確定していないか。忘れる経路は全部そろっているか (§3)
4. 「1 回」の定義が、すべての経路で同じか。頻度を使う policy が要る信号を捨てていないか (§4)
5. 参照と 1 手ずつ比べたか。小さい cache、端、境界で動かしたか (§5)
6. 測定に、独立した基準と対照があるか。原因を 1 つに絞る前に、条件を洗ったか (§6)
7. 各テストは、不具合を戻すと落ちるか。再試行は本当に再実行するか。権限は (§7)
8. 各フィールドの持ち主のロックが書いてあるか。既定を変えると他の経路のロックが変わらないか (§8)
9. 戻す手段と、既定を変える根拠が残っているか (§9)
