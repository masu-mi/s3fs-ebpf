# docs/ INDEX

トピック別に docs/ を分類したエントリポイント。個々の doc は先頭に概要を持つ想定だが、初訪の読者は「どこから読み始めるか」の答えをここで得る。相互リンクの真の source of truth は各 doc の本文 (`docs/<file>.md §N` 表記) だが、この索引が壊れたら本文の参照も直したいので、追加/削除は同時に。

## 起点

| doc | 対象読者 | いつ読むか |
|---|---|---|
| [`motivation.md`](motivation.md) | 全員 | プロジェクトの前提と Meetup narrative |
| [`design.md`](design.md) | 設計に興味 | 統合設計書 (initial pitch) |
| [`../README.md`](../README.md) | 新入 dev | Quick start / demo path |
| [`demo-scenario.md`](demo-scenario.md) | Meetup 発表担当 | 35 min の発表 script |
| [`related-works.md`](related-works.md) | 比較資料が要る人 | ExtFUSE / RFUSE / mount-s3 との位置付け |

## Read path (Fast Path B/C)

| doc | 内容 |
|---|---|
| [`read-path-design.md`](read-path-design.md) | Path A/B/C の design 比較、A′ 断念の記録 |
| [`week4a-implementation-notes.md`](week4a-implementation-notes.md) | Path B (GUP prewarm) の実装、arena kern_vm 断念の 1 次情報 |
| [`week4a-c-session-notes.md`](week4a-c-session-notes.md) | Path C (read-miss upcall) の実装記録、追加課題 |
| [`partial-admit-internals.md`](partial-admit-internals.md) | D-01 partial admit / D-02 arena free-list の内部構造 |
| [`hook-timings-diagrams.md`](hook-timings-diagrams.md) | 8 タイミング × 3 path のオブジェクト図 |

## Write path

| doc | 内容 |
|---|---|
| [`week4c-implementation-notes.md`](week4c-implementation-notes.md) | E-01/02/03 (fsync blocking, unlink/rename, coalescer) の実装記録 |
| [`posix-and-practical-notes.md`](posix-and-practical-notes.md) | POSIX 準拠と "実用的" 妥協点 |
| [`g01-subdir-plan.md`](g01-subdir-plan.md) | G-01/F-02 subdir 対応 + basename 衝突の実装計画 (実装済) |

## Policy engine (Layer 1-4)

| doc | 内容 |
|---|---|
| [`policy-engine-design.md`](policy-engine-design.md) | 3 (+ KFunc) layer architecture の設計 |
| [`writing-policies.md`](writing-policies.md) | CEL / eBPF 両サイドの書き方入門 |
| [`policy-attach-comparison.md`](policy-attach-comparison.md) | fmod_ret vs struct_ops の attach 方式比較 |
| [`hit-rate-bench.md`](hit-rate-bench.md) | Zipf workload での policy hit-rate 実測 (SIEVE/S3-FIFO/W-TinyLFU/LRU) |
| [`trace-harness.md`](trace-harness.md) | P-04 ms-scale offline simulator |

## 環境 / bench / metric

| doc | 内容 |
|---|---|
| [`week1-build-runtime-notes.md`](week1-build-runtime-notes.md) | kernel 6.12 + clang-20 + kmod の build 手順 |
| [`week2-implementation-notes.md`](week2-implementation-notes.md) | Week 2 MinIO + s3-list/stat 実装 |
| [`week3-implementation-notes.md`](week3-implementation-notes.md) | Week 3 arena/GUP プロトタイプ |
| [`week4b-implementation-notes.md`](week4b-implementation-notes.md) | Week 4 (b) dynamic bucket sync |
| [`bench-env-notes.md`](bench-env-notes.md) | bench 環境 caveats、Grafana metric surface |
| [`benchmark-results.md`](benchmark-results.md) | Meetup 用の headline 数値 + fair-bench 説明 |
| [`gcs-backend.md`](gcs-backend.md) | GCS 対応の使い方と backend abstraction |
| [`lessons-learned.md`](lessons-learned.md) | 実装で刺さった罠の 1 次情報 |
| [`cilium-ebpf-addr-space-cast-marshal.patch`](cilium-ebpf-addr-space-cast-marshal.patch) | cilium/ebpf に投げる予定の marshal patch |

## 運用 (backlog)

| doc | 内容 |
|---|---|
| [`backlog.md`](backlog.md) | 全 follow-up の一元リスト。優先度 × カテゴリ (P0/P1/P2 × A-L) |

## 更新運用

- doc を追加したら **このファイルにも 1 行反映**。無ければ検索経由でしか辿れなくなる。
- doc をリネームしたら `grep -rn '<old-name>' docs/ ../README.md` で参照サイトを全部書き換える。
- 相互リンクは `docs/<file>.md §N` の形で書き、他の docs から辿れるようにする — anchor に依存しない (heading 追加/削除で anchor は変わる)。
- L-03 (このタスク) の cleanup で `deisign.md` → `design.md` の typo rename と README の索引追記を実施済。
