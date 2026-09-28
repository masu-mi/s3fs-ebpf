# eBPF × KFunc 活用型 次世代 S3FS 統合アーキテクチャ設計書 & Meetup 発表仕様

`bpf_arena` によるゼロコピー shared memory、Verifier 制限を回避する **Custom KFunc（カーネルモジュール）**、および **eBPF struct_ops による policy engine** を組み合わせた、超低レイテンシ S3 ファイルシステム（S3FS）の設計・実装および Meetup 登壇仕様の全貌です。

---

## 0. 現状 (2026-09-28) — 実装ステータス

> **この節は現実装との照合用**。以下の §1-§7 は Week 0 の initial
> design で、CEL 採用や eBPF struct_ops の追加など Week 4 以降で
> 進化した部分がある。Meetup 会場での実演は
> [`docs/demo-scenario.md`](./demo-scenario.md) の Demo 0-8 台本を
> 参照。実装の詳細は各セクションでリンクする専門 doc を見てほしい。

### 完成した Building Blocks

| 層 | 内容 | 詳細 |
|---|---|---|
| **Layer 0** | KFunc set 5 種 (`admit_count`, `softcap`, `evictions_total`, `time_ns`, `ino_size`) | `kmod/tree_traversal.c` |
| **Layer 1** | CEL DSL (Google, K8s admission と同 stack) — prewarm 判定と Layer 2 program 選択 | `daemon/internal/policy/`, `policies/default.cel` |
| **Layer 2** | built-in eBPF policy 9 種 (下記 hit-rate 表) | `bpf/policy_*_ops.bpf.c` |
| **Layer 3** | user eBPF plug (`--policy-bpf=file:<path>`, SHA256 allowlist gated) | `daemon/internal/policyprog/` |
| VFS + kmod | `s3fs_ebpf` FS type、`.getattr/.read_folio/.write/.release`、bpf_arena 連携 | `kmod/vfs_ops.c` |
| Observability | 18 counter/gauge + 2 histogram (read-miss / flush latency) + 19 Grafana panel | `daemon/internal/metrics/`, `deploy/grafana/` |
| Runtime API | POST `/policy/reload` (hot swap)、POST `/policy/eval` (bench trigger) | `daemon/cmd/s3fs-ebpf/main.go` |

### hit-rate 比較 (Zipf α=0.7, 30 files, 200 access, softcap=8)

同じ workload で **policy を変えるだけで hit rate が 6.5% → 88.5%**。
`docs/hit-rate-bench.md` に詳細と再現手順、caveats。

```
policy=observe         100.00%    0 evict   ← ceiling (no eviction)
policy=wtinylfu_ops     88.50%  161 evict   ← CMS + bpf_loop aging
policy=sieve_ops        66.00%  447 evict   ← 1-bit second-chance clock
policy=size_aware_ops   65.00%  475 evict   ← per-ino KFunc (ino_size)
policy=pressure_ops     54.00% 1704 evict   ← KFunc-driven adaptive
policy=s3fifo_ops       46.00%  991 evict   ← 3-tier {S, M, G} 論文準拠
policy=time_decay_ops   30.50% 6488 evict   ← KFunc-driven time decay
policy=lru_ops           7.50% 11278 evict  ← age-based FIFO
policy=freq2bit_ops      6.50% 11468 evict  ← 2-bit ref counter
```

さらに demo 専用の Layer 0 KFunc 消費 policy:

- **`policy_pressure_ops`** — `admit_count` / `softcap` で SIEVE
  strictness を動的に切替 (52-54%)
- **`policy_time_decay_ops`** — `time_ns` で `last_hit_ns > 5s`
  を evict (bench 対象外、demo 用)
- **`policy_size_aware_ops`** — `ino_size` で SMALL/BIG bias
  (44B bench で SIEVE 相当挙動)

### 現在の narrative の三本柱

1. **FUSE を経由しない → 10× stat latency** (`docs/benchmark-results.md`)
2. **struct_ops で policy を curl 一発 hot swap** — in-flight IO
   を中断せず algorithm を差替える shape (`docs/policy-engine-design.md §4.5`)
3. **hit-rate は policy 選択で 6% → 80% 動く** — 実測、workload 固定

### Initial design (§1-) からの主な変更点

- **Lua/WASM → CEL + eBPF**: hot path の call overhead が Lua で
  10μs 超だったので、cold-path 判定のみ CEL、実際の cache algorithm
  は eBPF program として出荷。`docs/policy-engine-design.md §5-§6`
- **Radix Tree/bpf_rbtree → hash table**: Verifier 突破の実装
  難易度を下げるため、初期実装は Linux hashtable + KFunc lookup
  helper (`s3fs_admit_size` etc.) の shape に変更
- **単一 kmod で完結 → 3-layer + KFunc**: 政策と実装の分離
  (Layer 1/2/3) を導入、user eBPF を allowlist で gate

### 参照 doc (現行 implementation)

- `docs/demo-scenario.md` — **Meetup デモ台本 (Demo 0-8, 35 min)**
- `docs/writing-policies.md` — **自作 policy を書く実装ガイド**
- `docs/policy-engine-design.md` — 3-layer architecture の詳細
- `docs/policy-attach-comparison.md` — fmod_ret vs struct_ops
- `docs/hit-rate-bench.md` — 上記の 80% 数値の出典と caveat
- `docs/trace-harness.md` — offline policy simulator の使い方
- `docs/lessons-learned.md` — 実装ハマりポイント (arena mm/gup /
  bpf_loop / struct_ops hot reload / iget_locked / etc.)
- `docs/backlog.md` — 完了/残タスク一元管理

---

> **§1 以下は 2026-05 の initial design**。読み進める前に §0 の
> "現状 (2026-09-28)" を必ず参照。実装は多くの決定を進化させて
> いる (CEL 採用、struct_ops policy engine、KFunc set 拡張、etc.)。

## 1. 背景と既存実装との構造的比較

公式の **Mountpoint for Amazon S3** や **s3fs-fuse** は、大容量データのシーケンシャル I/O に最適化されていますが、FUSE 由来のコンテキストスイッチコストと固定的なキャッシュ設計に限界があります。本提案アーキテクチャは FUSE IPC の排除とカーネル内メタデータ検索によりこれを解決します。

### 既存実装との比較

| 比較項目                    | s3fs-fuse          | Mountpoint for Amazon S3     | 提案構成 (eBPF + KFunc + WASM)         |
| ----------------------- | ------------------ | ---------------------------- | ---------------------------------- |
| **設計思想**                | POSIX 互換性の維持       | 高スループットな大容量 I/O              | **超低レイテンシ・動的ポリシードリブン**             |
| **FUSE/IPC コスト**        | **高**（毎回のユーザー空間往復） | **中**（Rust/CRT 最適化だが IPC あり） | **極小**（`bpf_arena` で Hit 時 IPC ゼロ） |
| **メタデータ (`stat`/`ls`)** | 非常に遅い              | 普通（固定キャッシュのみ）                | **超高速**（KFunc でカーネル内ポインタ直参照）       |
| **キャッシュ制御**             | 低（静的オプション）         | 低（ディスクキャッシュ中心）               | **極高**（WASM/Lua で即座にホットリロード）       |
| **Verifier 回避策**        | なし（カーネル非連携）        | なし（カーネル非連携）                  | **KFunc**（深層木走査の命令数制限を突破）          |

---

## 2. 3 レイヤー・アーキテクチャ概要

コントロールプレーンとデータプレーンを完全分離し、Verifier の制約を打破する 3 レイヤー構造を採用します。

```text
  [ System Call (stat, read, open) ]
                 │
                 ▼
┌────────────────────────────────────────────────────────┐
│ Fast Path (Data Plane / Kernel Space)                  │
│  - eBPF: システムコール捕捉, bpf_arena のルート渡す    │
│  - KFunc (Kernel Module): Verifier 制限を受けない      │
│    Radix Tree / bpf_rbtree の深層 C コード走査          │
└──────────────┬──────────────────────────┬──────────────┘
               │ Hit                      │ Miss
               ▼                          ▼
       [ ゼロコピー即時応答 ]      [ Slow Path (User Space) ]
                                  - S3 API (Get/Put/List)
                                  - WASM / Lua Policy Engine

```

* **Fast Path (Data Plane / Kernel Space): eBPF ＋ KFunc (Kernel Module)**
* **eBPF:** VFS / FUSE フックをキャッチし、`bpf_arena` のルートポインタを KFunc へ引き渡す軽量プロキシ。
* **KFunc (カーネルモジュール):** Verifier の総命令数制限（最大 100 万命令 / `BPF_COMPLEXITY_LIMIT_EXCEEDED`）を受けずに、`bpf_arena` 上の Radix Tree や `bpf_obj_new` で作成した `bpf_rbtree` を非制限 C コードで深層までトラバース。

* **Slow Path & Control Plane (User Space): ユーザー空間 S3 デーモン（言語未確定）**
* S3 API 通信、非同期 I/O、リトライ、`bpf_arena` へのツリー構造書き込みを担当。
* ※コントロールプレーン言語は、採用する S3 SDK や外部統合 API の要件（Go / Rust / C++）に応じて選定。

* **Dynamic Policy Engine (Control Plane Plugin): WASM / Lua Runtime**
* S3FS デーモンを再起動することなく、拡張子や S3 メタデータタグに応じたキャッシュ TTL・プリフェッチポリシーを外付けスクリプトから動的に注入・更新。

---

## 3. コンポーネント役割分担と Verifier 回避スキーム

| 層                  | 役割                                   | 採用技術・要素                                | Verifier 回避             |
| ------------------ | ------------------------------------ | -------------------------------------- | ----------------------- |
| **カーネル Fast Path** | 任意の深さの探索ループ、ポインタトラバース、`stat` 即時応答    | **KFunc** (自作 Linux カーネルモジュール)         | **対象外**（通常の C 言語ループを実行） |
| **カーネル フック**       | イベント検知、KFunc 呼び出し、`bpf_arena` アドレス保持 | **eBPF** (`bpf_arena` / `bpf_obj_new`) | **対象**（短小・軽量なプロキシコード）   |
| **ユーザー デーモン**      | S3 通信、`bpf_arena` への構造体注入、非同期 I/O    | ユーザープロセス (Language TBD)                | ―                       |
| **ポリシー エンジン**      | パス／タグ別の動的キャッシュルール評価                  | WASM (Wasmtime) / Lua / Sobek          | ―                       |

### Verifier 突破の仕組み

`bpf_arena` 内で深さが不定なツリー構造を `while(node)` で辿ろうとすると、eBPF 単体では Verifier に拒否されます。KFunc に `KF_TRUSTED_ARGS` フラグを付与して呼び出すことで、eBPF から渡された `bpf_arena` 空間のポインタをモジュール内で安全にキャストし、ループ数制限なしで探索を実行できます。

---
## 4. WASM / Lua による動的キャッシュポリシー

ユーザー空間に軽量スクリプトランタイムを組み込み、評価結果を `bpf_arena` の共有メモリ空間に即時フラグ書き込みします。

```lua
-- Lua で記述する動的キャッシュポリシーの例 (policies/custom_policy.lua)
function on_cache_check(path, s3_metadata)
    -- ログファイルは 5 秒だけ Write-Back キャッシュ
    if path:match("%.log$") then
        return { action = "WRITE_BACK", ttl = 5, sync_delay = 10 }
    end
    
    -- AI モデルの重みファイルは永久キャッシュ ＋ 事前全件ロード
    if s3_metadata["x-amz-meta-type"] == "model-weights" then
        return { action = "CACHE_PERMANENT", prefetch = true }
    end

    return { action = "DEFAULT" }
end

```

---

## 5. CLI コマンド体系 (`s3fs-ebpf`)

```bash
# S3 バケットをマウント（kmod ロード、eBPF ロード、WASM ポリシー適用）
$ s3fs-ebpf mount \
    --bucket my-data-bucket \
    --endpoint https://s3.us-west-2.amazonaws.com \
    --mount-point /mnt/s3data \
    --cache-dir /var/cache/s3fs \
    --policy-plugin ./policies/ai_workload.wasm

# 実行中の S3FS に対して動的ポリシーをホットリロード
$ s3fs-ebpf policy load \
    --mount-point /mnt/s3data \
    --plugin ./policies/log_streaming.lua

# キャッシュ Hit 率や eBPF/KFunc Fast Path のパフォーマンス監視
$ s3fs-ebpf stats /mnt/s3data
Fast Path (KFunc/eBPF) Hits: 94.2% (128,401 reqs)
Slow Path (S3 API) Misses : 5.8%  (7,910 reqs)
bpf_arena Memory Usage     : 42.1 MB / 4.0 GB
Active Policy Engine       : log_streaming.lua (Hot-reloaded)

# アンマウント（eBPF デタッチ、kmod アンロード、デーモン停止）
$ s3fs-ebpf unmount /mnt/s3data

```

---

## 6. ディレクトリ構成案

言語依存部（Control Plane）を分離し、カーネルモジュール・eBPF・WASM ポリシーの境界を整理したディレクトリレイアウトです。

```text
s3fs-ebpf/
├── README.md
├── Makefile
├── kmod/                       # カーネルモジュール (KFunc 側)
│   ├── Makefile
│   ├── s3fs_kfunc_main.c       # KFunc 登録と bpf_arena ポインタ受渡
│   └── tree_traversal.c        # Verifier 制限を受けない深層 Radix Tree 走査
│
├── bpf/                        # eBPF フックコード (C言語)
│   ├── s3fs_fastpath.bpf.c     # システムコールフック ＋ KFunc 呼び出し
│   └── arena_types.h           # bpf_arena 共有ヘッダー
│
├── daemon/                     # コントロールプレーン (言語未確定: Go/Rust/C++)
│   ├── main/                   # CLI エントリポイント (mount, unmount, stats)
│   ├── ebpf_loader/            # kmod/eBPF ロード、bpf_arena 初期化
│   ├── s3_client/              # S3 SDK 抽象化レイヤー (非同期 Get/Put/List)
│   ├── metadata_sync/          # S3 メタデータを bpf_arena への構造体書き込み
│   └── runtime/                # WASM / Lua / Sobek ポリシー実行エンジン
│
├── policies/                   # ユーザー定義キャッシュポリシー (WASM / Lua / Rust)
│   ├── default_policy.lua
│   └── ai_workload.rs          # WASM コンパイル対象の Rust ソース例
│
└── include/
    └── s3fs_shared_types.h     # カーネル／モジュール／ユーザー空間共有構造体

```

---

## 7. Meetup 発表ストーリー & ライブデモ構成

### 発表のアジェンダ案（20分構成）

1. **背景:** なぜ S3FS は遅いのか？（FUSE IPC ＋ 階層パス解決のボトルネック）
2. **eBPF の進化と夢:** `bpf_arena` ゼロコピー共有メモリへの期待
3. **直面した絶望:** 探索ループが長大化し `BPF_COMPLEXITY_LIMIT_EXCEEDED` で Verifier に蹴られる問題
4. **ブレイクスルー:** Custom KFunc パターンによる「複雑探索のカーネルモジュール切り出し」
5. **アーキテクチャ統合:** eBPF (Hook) × KFunc (Fast Loop) × WASM (Hot Policy)
6. **ライブデモ & ベンチマーク**

### デモの見せ場 (Live Demo)

* **デモ 1 (Verifier エラーの再現):** 純粋な eBPF 内で `bpf_arena` を `while` で辿るコードを `bpftool load` し、コンパイル/ロード拒否画面を見せる。
* **デモ 2 (KFunc 導入による超高速 Fast Path):** KFunc 経由に差し替えて即時ロード成功。`ls -R` や `find` を叩き、IPC ゼロで `bpf_arena` から爆速で応答が返るログを披露。
* **デモ 3 (WASM ポリシーのホットリロード):** S3FS を起動したまま外部から WASM ポリシーを差し替え、キャッシュ挙動と Hit 率が瞬時に変わる様子を展示。

----
1ヶ月という短期間で発表を成功させる鍵は、「フルスペックのS3FSの完成」を目指すのではなく、「技術的説得力のあるPoC（概念検証）とプレゼンストーリー」に実装スコープを絞ることです。実際のS3通信はローカルの擬似S3（MinIO等）で代用し、カーネル階層の Fast Path に集中したロードマップと試験項目をまとめました。

---

## 1. 1ヶ月の実装ロードマップ（スプリント計画）

**Week 1: KFunc と `bpf_arena` の疎通（コア技術の検証）**

* カーネルモジュールを作成し、カスタム KFunc をカーネルに登録。
* `bpf_arena` を確保し、ユーザー空間からダミーの Radix Tree（数件の `stat` 構造体）を書き込んで、KFunc がそれを直参照で読み出せるか検証。

**Week 2: eBPF Fast Path と「Verifier 拒否」の仕込み**

* VFS / FUSE フック用の eBPF プログラムを実装し、KFunc を呼び出すルートを構築。
* 意図的に eBPF 単体で `while` ループを回す比較用コードを作成（Verifier に拒否させるデモ用）。
* ユーザー空間デーモン側は、MinIO からデータを引く Slow Path の最小限の枠組みを作成。

**Week 3: 動的ポリシー（Lua/WASM）とデモシナリオの実装**

* ユーザー空間プロセスに Lua または Wasmtime の組み込みランタイムを接続。
* スクリプトの評価結果に応じて `bpf_arena` 内の `ttl` や `flags` を更新するロジックを接続。
* CLI (`mount` / `policy load` / `stats`) の簡易実装。

**Week 4: ベンチマーク測定 & スライド・ライブデモ準備**

* 既存ツール（`s3fs-fuse` や `Mountpoint for Amazon S3`）との `stat` レイテンシ比較データを取得。
* デモの動作確認（失敗パターン ➔ 成功パターン ➔ ホットリロード）。

---

## 2. デモ・性能評価向け試験項目一覧

| 分類     | 試験項目                  | 実装・測定方法                                        | 期待される結果 / 見せ場                                                 |
| ------ | --------------------- | ---------------------------------------------- | ------------------------------------------------------------- |
| **機能** | **Verifier 拒否再現**     | eBPF 内で `bpf_arena` のポインタを `while` ループで深層検索    | `bpftool load` 実行時に `BPF_COMPLEXITY_LIMIT_EXCEEDED` でエラー落下のログ |
| **機能** | **KFunc 突破**          | ループ処理を KFunc へ外出しして eBPF から呼び出し                | Verifier を通過し正常にプログラムがロード・実行される                               |
| **機能** | **`bpf_arena` ゼロコピー** | ユーザー空間で更新した Inode 情報を KFunc 経由で即時参照            | IPC や BPF Map Lookup 呼出なしでカーネル空間側からデータ更新を確認                   |
| **機能** | **ポリシーホットリロード**       | デーモン停止なしで外部 Lua/WASM ファイルを切り替え                 | S3FS の再起動なしで `stats` の Fast Path Hit 率やログが瞬時に変化               |
| **性能** | **`stat` 応答レイテンシ**    | `time stat` または `fio` を用いて 10,000 回のメタデータ参照を実施 | 既存の `s3fs-fuse` に対し、Fast Path Hit 時で **10〜100倍以上の高速化**        |
| **性能** | **CPU / CS コスト**      | `pidstat -w` または `perf` でコンテキストスイッチ（CS）数を計測    | FUSE 経由のアクセスに比べ、コンテキストスイッチ発生回数が **大幅減少**                      |

---

## 3. 短期開発を乗り切る「割り切り」ポイント

* **S3 API 通信:** ローカルに立たせた MinIO や mock サーバーを使用し、ネットワーク揺らぎによるベンチマークブレを排除します。
* **データ構造の限定:** ディレクトリツリーは 2〜3 階層程度の静的・固定データ構造で PoC を作成し、走査アルゴリズムが動くことの証明に注力します。
* **スクリプトエンジン:** WASM の組み込みに時間がかかる場合は、より軽量な Lua (GopherLua / LuaJIT) に割り切ると 1〜2 日で組み込めます。
