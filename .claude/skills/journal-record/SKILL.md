---
name: journal-record
description: 結果を docs/journal に記録: backlog 起票、benchmark-results の節、生データ、INDEX 追記。「backlog に起票」「結果を記録」「INDEX に足して」で使う。
---

# backlog / journal / INDEX への記録

CLAUDE.md の「不変条件を変えたらメモも更新」と同じ考え方で、**結果と未解決を repo に残す**。性能の値の検証は `perf-bench-verify` §4、ここは書き方。

## backlog (`docs/journal/backlog.md`)

- `## AC.` の表に 1 行足す。列は `| # | 内容 | 優先度 | 状態 |`、ID は末尾の次の番号 (`grep -n '^| AC-' docs/journal/backlog.md | tail -3`)。優先度は P0 (次スプリント) / P1 / P2。
- **内容**: 症状 (どの試験のどの値か)、再現手順、分かっていれば原因、根拠の節 (`br §18.NN`)。直したものは `✅ 修正` + 回帰テスト名 (`regression-test`)、未了は `⏳ 未着手` + 「何を確かめるか」。
- 試して効かなかった変更も、**何を試して何が変わらなかったか**を該当行に残す。原因不明は推測で埋めず「未切り分け」。
- 完了して時間が経った行は `backlog-archive.md` へ (ID と本文はそのまま)。新規 follow-up は週次 note → backlog の順。

## 性能結果 (`docs/journal/benchmark-results.md`)

1. 生データを `docs/journal/data/<名前>-<日付>/` に置く (`A.txt` `B.txt` … `*.json` `results.jsonl` `summary.txt` `environment.txt`、スクリーンショット)。
2. 末尾の次の節 (`### 18.NN`、`grep -n '^### 18\.' ... | tail -1`) に書く: **条件** (日付、`uname -r`、commit、各構成の起動オプションと module param、n、cold の手順)、**値** (中央値 + 四分位範囲 + 最小・最大)、**比** (s3fs-fuse の時間 ÷ s3fs-ebpf の時間。1 超が ebpf の勝ち。IOPS は逆数)。
3. 以前の値と ±20% 以上違う項目は、**理由 + 根拠**か「未切り分け」。
4. 反映先: `docs/design/where-fs-layer-can-win.md`、`docs/user/meetup-qa.md` (出典の節番号つき)。

## INDEX (`docs/INDEX.md`)

- 新しい doc / データ置き場は表に 1 行 (`| [path](path) | 内容 |`)。カテゴリは `user/` `design/` `journal/` `dev-notes/`。
- 設計 doc を足したら冒頭に概要と、状態遷移図 / TLA+ があればその場所を書く (`tlc-design-verification`)。
- CLI / `make help` を変えたら `make cli-reference` で `docs/user/cli-reference.md` を再生成する。

## 最後に

`git status` を見て、scratch の大きいファイルや 60 MB のバイナリ (`daemon/cmd/s3fs-ebpf` 内の `go build`) が混ざっていないか確かめる。
