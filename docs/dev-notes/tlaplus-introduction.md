# TLA+ と TLC による設計の記述と検証: 入門

対象: TLA+ を読み書きしたことがない、このリポジトリの開発者。目標は、**状態が複数の持ち主に分かれ、更新の順序が決まっていない設計**を小さなモデルにして、TLC に全部の順序を試させ、壊れる順序を見つけられるようになること。手順だけを知りたいときは `docs/dev-notes/design-verification.md` と `.claude/skills/tlc-design-verification/SKILL.md`。

## 1. なにができるのか

テストは「書いた操作列」しか試さない。並行・非同期の設計の不具合は、操作が**ある順序で重なったとき**だけ出る。TLC (TLA+ のモデル検査器) は、モデルが取りうる状態を幅優先で全部たどり、性質が破れる最初の状態を、そこへ至る**最短の操作列**として返す。

このリポジトリでの実例 (`docs/design/file-lifecycle-state-machine.md`): flush と rename が別々のループで動く設計を コメントを含めて 140 行弱のモデルにしたら、「作成直後に rename すると S3 の key が旧名になる」など 3 件の反例が数秒で出た。1 件は実機で再現した。実装と測定のあとで偶然気づくより、設計の段階で見つかる。

## 2. 三つの考え方

- **状態**: 変数の値の組。`counter = 0, pc = [p1 |-> "read", ...]` のような、ある瞬間の全体。
- **遷移**: 「今の状態」と「次の状態」の関係。プログラムの 1 ステップではなく、**あり得る 1 手**を表す。次の状態の変数には `'` を付ける (`counter'`)。
- **振る舞い**: 初期状態から遷移をたどった状態の列。モデルは「あり得る振る舞い全部」。並行性は、各プロセスの遷移の**どれを次に選んでもよい** (非決定性) ことで表す。

検証したい性質は 2 種類ある。この記事と `specs/tla/` が使うのは前者だけ。

- **安全性** (悪いことは起きない): どの状態でも成り立つ式 (**不変条件**)。TLC は、破れる状態を見つける。
- **活性** (いつかよいことが起きる): 公平性の仮定が要る。扱いが難しいので、まず安全性で足りるかを考える。

## 3. 最初のモデル: 更新の消失

2 つのプロセスが共有カウンタを +1 する。「読む」「書く」が別のステップだと、片方の更新が消える。`specs/tla/tutorial/LostUpdate.tla` の要点を読む。

```tla
VARIABLES counter, pc, tmp       \* 共有カウンタ、各プロセスの位置、読んだ値のコピー

Init ==                           \* 初期状態
    /\ counter = 0
    /\ pc  = [p \in Procs |-> "read"]
    /\ tmp = [p \in Procs |-> 0]

Read(p) ==                        \* p が counter を読む
    /\ pc[p] = "read"                          \* 起きる条件 (ガード)
    /\ tmp' = [tmp EXCEPT ![p] = counter]      \* 次の状態: tmp[p] だけが counter になる
    /\ pc'  = [pc EXCEPT ![p] = "write"]
    /\ UNCHANGED counter                       \* counter は変わらない

Write(p) ==                       \* p が (読んだ値 + 1) を書く
    /\ pc[p] = "write"
    /\ counter' = tmp[p] + 1
    /\ pc'      = [pc EXCEPT ![p] = "done"]
    /\ UNCHANGED tmp

Next == \E p \in Procs : Read(p) \/ Write(p)   \* どれかのプロセスが、どれかの手を打つ
Spec == Init /\ [][Next]_vars                  \* 初期状態から Next を繰り返す

Correct == AllDone => counter = Cardinality(Procs)   \* 守りたい性質
```

読み方の要点:

| 書き方 | 意味 |
|---|---|
| `/\` , `\/` | かつ、または (行頭に縦に並べて書く) |
| `x' = e` | 次の状態で `x` は `e` になる |
| `UNCHANGED x` | 次の状態でも `x` は同じ。**書き忘れると、その変数は何にでもなる**と解釈され、状態が爆発する |
| `[f EXCEPT ![k] = v]` | 関数 (表) `f` の `k` の値だけを `v` にしたもの |
| `\E p \in S : P(p)` | `S` のどれかの `p` で `P` が成り立つ。**ここが非決定性** |
| `[][Next]_vars` | 毎回 `Next` か、`vars` が変わらないか (スタッタリング) |
| `ガード` | 遷移の先頭の条件。偽のとき、その遷移は選べない |

## 4. 設定ファイル (.cfg) と実行

モデルの定数と、検査する性質は `.cfg` に書く (`LostUpdate.racy.cfg`)。

```
\* EXPECT: violated
SPECIFICATION Spec
CONSTANTS
  Procs = {"p1", "p2"}
  Atomic = FALSE
INVARIANT Correct
```

- `CONSTANTS`: モデルに入れる具体的な値。集合は小さくする (名前 3 つ、プロセス 2 つ)。**小さいモデルで見つかる不具合は多い** (小スコープ仮説)。
- `NONE = NONE` のように右辺を引用符なしで書くと、**モデル値** (他のどれとも等しくない名前だけの値) になる。「値なし」に使う。
- このリポジトリの約束: 1 行目に `\* EXPECT: ok` か `\* EXPECT: violated` を書く。`make test-spec` が期待と食い違うと失敗にする。

実行 (ホストに Java は不要。使い捨てコンテナ、ネットワークなし):

```bash
scripts/tlc.sh specs/tla/tutorial/LostUpdate.tla specs/tla/tutorial/LostUpdate.racy.cfg   # 1 つ
make test-spec                                                                            # 全部
```

## 5. 出力の読み方

`racy` の出力 (抜粋、状態の一部を省略):

```
Error: Invariant Correct is violated.
State 1: <Initial predicate>        counter = 0, pc = [p1 |-> "read",  p2 |-> "read"]
State 2: <Next ...>                 counter = 0, pc = [p1 |-> "read",  p2 |-> "write"]    ← p2 が読んだ
State 3: <Next ...>                 counter = 0, pc = [p1 |-> "write", p2 |-> "write"]    ← p1 も読んだ (まだ 0)
State 4: <Next ...>                 counter = 1, pc = [p1 |-> "done",  p2 |-> "write"]    ← p1 が 0+1 を書いた
State 5: <Next ...>                 counter = 1, pc = [p1 |-> "done",  p2 |-> "done"]     ← p2 も 0+1 を書いた → 1 (期待は 2)
17 states generated, 13 distinct states found
```

- **反例は最短**: 幅優先なので、これより短い再現はない。まず「どの手の並びか」(`Next` の行) を読む。
- `states generated` は遷移の数、`distinct states found` は異なる状態の数。通った (`No error`) とき、この数が「見た範囲」。0 に近すぎるなら、ガードが厳しすぎてモデルが何もしていない。
- `atomic` 版 (読んで書くまでを 1 手) は `4 distinct states`、`No error`。**修正案を定数 (変種) にして、同じ性質で比べる**。通る変種が、設計の根拠になる。

## 6. このリポジトリの実例

`specs/tla/FileLifecycle.tla` は上と同じ作りで、次を表す。

| モデルの変数 | 実装 |
|---|---|
| `kd` | kmod の entry table (ユーザーに見える名前) |
| `s3` | S3 の key → 内容 |
| `mirror` / `local` | daemon の entry mirror / `localKeys` |
| `evQ` | kmod → daemon の flush / rename event |

`Next` は `Create`, `Rename` (kmod 側)、`DaemonFlush`, `DaemonRename`, `Sync` (daemon 側) の選択。flush と rename は別ループなので、`evQ` のどの種類の先頭からでも取れる (`Oldest(j, t)`)。定数 `RecordKeys`, `Serialized` で修正案を切り替える。性質 `QuiescentConsistent` は「daemon の仕事が尽きたら、S3 の key はユーザーの付けた名前と一致する」。

結果 (12 組): 実装そのまま (`base`) は 3 つの性質が全部破れる。通るのは両方の修正を入れた `keys-serialized` だけ。詳しくは `docs/design/file-lifecycle-state-machine.md`。

## 7. モデルの書き方のコツ

1. **性質を先に、観測できる形で書く**。「内部の表が更新される」ではなく「ユーザーに見えるファイルは、daemon が引く key で見つかる」。
2. **関係のあるものだけを写す**。内容は「どの inode のバイト列か」だけ、時間は「順序」だけ。実装の変数を全部写すと、状態が爆発して読めなくなる。
3. **無限に伸びるものに上限を付ける**。キューが伸びる遷移を無制限にすると、通る変種が終わらない (違反は見つかるので気づきにくい)。`MaxQ` のような定数で止める。
4. **終わりのある振る舞いには停止の遷移を足す** (`AllDone /\ UNCHANGED vars`)。足さないと、最後の状態が deadlock として報告される。
5. **型の不変条件 (`TypeOK`) を必ず入れる**。変数が意図しない値になる書き間違いを、最初に拾える。
6. **実装との対応表を残す** (どの操作がどのコードか)。モデルとコードがずれたとき、どちらが古いか分かる。

## 8. 限界

- **通ったことは、そのモデルの範囲で破れないことだけ**を意味する (名前 3 つ、inode 2 つ、キュー長 ≤ 5)。実装が正しいとは言えない。含めていないものは文書に書く。
- 反例は**実機で確かめる**。再現しなければ、モデルが実装と違うか、別のバグが隠している (`FileLifecycle` の反例 3 は後者)。
- 状態数は変数と定数の大きさで急に増える。まず定数を小さくする。
- 活性 (「いつか終わる」) は、公平性の宣言と時相式が要る。必要になってから学べばよい。

## 9. 手を動かす

1. `scripts/tlc.sh specs/tla/tutorial/LostUpdate.tla specs/tla/tutorial/LostUpdate.racy.cfg` を実行し、反例を 5 手で読む。
2. `Procs` を 3 つにして、`Correct` の反例が何手になるか見る。
3. `Inc` と `Atomic` を消し、「書く前に読み直す」(compare-and-swap) 版を自分で書いて、`ok` になるか確かめる。
4. `specs/tla/FileLifecycle.keys.QuiescentConsistent.cfg` を実行し、反例の `DaemonRename` と `DaemonFlush` の順序を `Oldest` と見比べる。

参考文献: Leslie Lamport, *Specifying Systems*。Hillel Wayne, *Practical TLA+* と Learn TLA+ のサイト。
