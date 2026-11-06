---
name: tlc-design-verification
description: 設計や既存実装の調査で、状態遷移図を書き、TLA+ のモデルにして TLC (モデル検査器) で網羅検証し、反例を実機で確かめる手順。「状態遷移を検証して」「TLA+ で設計を検証して」「モデル検査して」「競合・順序の問題がないか調べて」「設計の抜けを見つけて」と頼まれたとき、または非同期・並行・持ち主が複数の状態 (kmod / daemon / S3 / page cache、flush・rename・sync など別ループ) を新しく設計・変更・調査するときに使う。
---

# 状態遷移図 → TLA+ → TLC → 実機で確認

入門: `docs/dev-notes/tlaplus-introduction.md`。方針: `docs/dev-notes/design-verification.md`。適用例: `docs/design/file-lifecycle-state-machine.md` と `specs/tla/FileLifecycle.tla`。この skill は**手順と、過去に実際に踏んだ落とし穴**を書く。

## 0. 使うかどうか

使う: 状態の持ち主が 2 つ以上で、更新が非同期 (別ループ、別 ticker、イベントキュー)、または更新の順序が決まっていない設計。例: flush と rename、sync と create、open_check の確認と更新通知。
使わない: 直列の処理、表を引くだけの変更。図だけを書き、使わない理由を PR に 1 行書く。

## 1. 状態と更新者を並べる

表にする: 状態 | 持ち主 | 更新するもの (経路が複数のものに印)。コードから読み取り、**関数名で裏付ける** (推測で書かない)。`CLAUDE.md` の手順どおり `scripts/fn.sh` で関数単位に読む。

## 2. 状態遷移図

mermaid `stateDiagram-v2` を `docs/design/<名前>.md` に書く。全部の状態から、作成 / 書き込み / flush / rename / 削除 / 外部の更新 / sync を引き、**矢印が出ない状態**と、**複数の更新者が競合する遷移**に印を付ける。ここで見つかる不具合も多い。

## 3. TLA+ のモデル

置き場: `specs/tla/<モジュール名>.tla`。モジュール名とファイル名は同じにする。

- **性質を先に**、ユーザーに観測できる形で書く (「見えるファイルは daemon が引く key で見つかる」)。内部の表の一致ではない。
- 関係のあるものだけを写す (内容は「どの inode のバイト列か」程度)。実装の変数を全部写さない。
- 修正案は**定数 (変種)** にする (`RecordKeys`, `Serialized` のように)。実装そのままが `base`、通る変種が設計の根拠。
- `TypeOK` を必ず入れる。
- キューなど**無限に伸びるものに上限の定数** (`MaxQ`) を入れる。入れないと、違反は見つかるが、通る変種が終わらない。
- 終わりのある振る舞いは `AllDone /\ UNCHANGED vars` で止める (deadlock と区別)。
- ファイル先頭のコメントに、変種の意味と、性質の意味を書く。**モデルの操作とコードの対応表**を設計文書に書く。

## 4. cfg

`specs/tla/<モジュール名>.<変種>.<性質>.cfg`。**1 行目は `\* EXPECT: ok` か `\* EXPECT: violated`**。失敗する変種は既知の反例の記録で、`make test-spec` が期待と食い違うと失敗にする。定数は小さく (名前 3 つ、inode 2 つ、`MaxQ` 3)。通ったら `MaxQ` を上げて (5 など) 結論が変わらないことも一度確かめる。

cfg の生成を zsh でやらない (**zsh は変数を単語分割しない**ので `set -- $v` が動かず、ファイル名が壊れる)。`bash -c '...'` で書く。値なしは `NONE = NONE` (モデル値)、文字列は `"a"`。

## 5. 実行

```bash
scripts/tlc.sh specs/tla/X.tla specs/tla/X.base.P.cfg    # 1 つ (反例のトレースが出る)
make test-spec                                           # 全部、EXPECT と照合
```

`scripts/tlc.sh` は使い捨てコンテナ (ネットワークなし、spec は read-only、sha256 固定の `tla2tools.jar` を `~/.cache/s3fs-ebpf/tlc` に取得)。ホストに Java は要らない。`specs/tla/*/*.cfg` (教材) も `--all` に入る。

- runtime は podman → docker → nerdctl のうち**そのイメージを持っているもの**、なければ最初に見つかったもの (`TLC_RUNTIME=docker` で固定)。イメージが無ければ run が pull する。
- コンテナが書くファイルをホストが消せるように、docker には `--user <uid>:<gid>` を付ける。**rootless podman には付けない** (sub-uid に割り当てられて、呼び出し元のディレクトリに書けない。コンテナ内の root が既に呼び出し元)。
- `make test-spec` は**ネットワークとコンテナ runtime が要る**ので、`test-regression` には入れない。CI は別ジョブ (`.github/workflows/test.yml` の `spec`)。

## 6. 反例を読む

- 反例は**幅優先で最短**。`State N: <Action>` の並びを操作名だけ抜き出して読む: `grep -E "^State [0-9]+:" | sed -E 's/ line .* of module .*//'`。
- 状態を読んで、**どの変数が食い違ったか**を言葉にする (「mirror が flush を学ばない」)。それが設計の漏れ。
- 全部の変種 × 性質を流して、表にする (`docs/design/` に貼る)。

## 7. 実機で確かめる

反例を再現する操作を `scripts/test-parity.sh` (s3fs-fuse との差分テスト) に足す。再現したら、既知の差に **backlog の ID とモデルの反例の番号**を書く。再現しなければ: モデルが実装と違う、または別のバグが隠している。両方を設計文書に書く (`FileLifecycle` の反例 3 は、別のバグ AC-85 に隠れていた)。

## 8. 直したら

`base` を実装に合わせて更新し、通る変種を新しい `base` にする。`parity-known-diff.sh` の行を消し、`make test-parity` と `make test-spec` を通す。不変条件が変わるなら `docs/kmod-pcache-invariants.md` も同じ変更で更新する。

## 9. 落とし穴 (実際に踏んだ)

- `pgrep -f` / `ps | grep <script 名>` で止める・待つと、**自分のシェルに一致して自分を kill する**。コンテナは ID で (`docker ps -q --filter ancestor=<image> | xargs -r docker kill`)、script は完了ファイルで待つ。
- 違反のない変種が終わらない → キューの上限 (`MaxQ`) がない。
- 全部の変種が違反する → 性質が強すぎるか、モデルの初期状態が実装と違う。反例の最初の 3 手を実装と見比べる。
- 通った → 範囲 (名前の数、`MaxQ`) の中だけ。モデルに入れていないもの (fsync の WAIT 経路、外部クライアント、タイミング) を設計文書に書く。
- TLC は `/spec` に書けない (read-only)。作業ディレクトリは書き込み可の `/work` (`--workdir`、使い捨て) で、反例のときの `SpecTE` もそこに出る (v1.7.4 の TLC に `-noGenerateSpecTE` はない)。tla2tools.jar は **pre-release (v1.8.0 / rolling) に固定しない**: 配布物が差し替わり sha256 が合わなくなって CI の spec ジョブが落ちた (2026-11-06)。コンテナを root で動かすと、ホストが消せない一時ファイルができる (`--user`)。
- 実行時間は小さいモデルで秒単位。数分かかるなら定数を下げる。長く走らせるときは完了ファイルで待つ。
- **CI で初めて動かす script は、新品の環境で一度流してから push する**: キャッシュ (`TLC_CACHE=$(mktemp -d)`) もイメージ (`docker rmi`) も無い状態、docker と podman の両方。手元はイメージもジャーもあるので、「無いとき」の経路は一度も通っていない。`spec` ジョブは、これを怠って 2 回落ちた。
- **`set -e` と `$(for ...; done)`**: ループの最後のコマンドが失敗 (`image inspect` で「イメージ無し」) すると、コマンド置換が失敗して代入で script が**無言で**終わる (出力なし、終了コード 1)。選択ループには `|| true` を付ける。CI のログに何も出ていないときは、まず代入の失敗を疑う。
- 通らなかった設計案も変種として残す (`always` / `absent` / `applied-listed` のように)。棄却の記録になり、`EXPECT: violated` で「この設計は壊れる」ことを回帰として持てる。最初の案はたいてい 1 つか 2 つ穴がある (AC-85 では 3 つ: sync で全部消す / listing に無ければ消す / 昇格が即時)。反例の最後の状態を読んで、足りない規則 (取り消し、「適用済み」の目印) を足す。
- 修正で隠れていた症状が出る (AC-86 を直すと旧名の open が見えた、AC-85)。モデルの反例のうち、実機で再現しなかったものは「別のバグが隠している」と書いて起票する。
