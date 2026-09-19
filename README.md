<!-- Generated from README.md.tmpl by `make readme`. Do not edit README.md directly. -->
# s3fs-ebpf

FUSE を経由せずに Linux VFS へ直接ぶら下がる、次世代 S3 ファイルシステムの PoC。
Linux 6.9+ の **`bpf_arena`**（生ポインタ・ゼロコピー共有メモリ）、カーネルモジュール上の **KFunc**（Verifier 制限突破）、**eBPF**（VFS フック）、**Go 製コントロールプレーン** の 4 段構成でメタデータ操作を IPC 0 回で完結させます。

> **Status: Week 0 skeleton.** ビルド系・ディレクトリ・観測基盤だけが揃っており、各コンポーネントは空実装（`insmod`/`rmmod`/`bpftool prog load`/`s3fs-ebpf --help` が動くだけ）です。ロードマップは末尾を参照。

---

## アーキテクチャ

```
  syscall (stat / read / open)
              │
              ▼
  ┌──────────────────────────────────────────────┐
  │ Fast Path (Data Plane / Kernel)              │
  │  eBPF (fentry) → KFunc (kmod)                │
  │  → bpf_arena 直参照で即応答                   │
  └──────────────┬──────────────────┬────────────┘
                 │ Hit              │ Miss
                 ▼                  ▼
        [ゼロコピー返却]     [Slow Path / User]
                              Go daemon → S3 (MinIO/AWS)
                              Lua/WASM ポリシーで動的制御
```

先行研究比較（ExtFUSE / RFUSE）と根拠は [`docs/related-works.md`](docs/related-works.md) にあります。

---

## 前提環境

| 要件 | バージョン |
|---|---|
| OS | Ubuntu 24.04 LTS |
| Kernel | **Linux 6.9+**（`bpf_arena` 必須） |
| Compiler | clang 17+ / LLVM 17+ |
| Go | 1.22+ |
| Tools | `bpftool`, `fio`, `bcc`, `docker compose` v2, `gettext-base` (envsubst) |

---

## 環境準備

```bash
# 1. mainline 6.9 カーネルを導入して再起動
make setup-kernel
sudo reboot
# 再起動後: uname -r  →  6.9.x-... であることを確認

# 2. ツールチェイン一式（clang/llvm/bpftool/go/fio/bcc/docker）
make setup-toolchain

# 3. eBPF 用の vmlinux.h を実行中カーネルの BTF から生成
make vmlinux
```

---

## ビルド

```bash
make            # help 表示（デフォルトターゲット）
make all        # kmod + bpf + daemon まとめて
make kmod       # カーネルモジュール単体
make bpf        # eBPF プログラム単体（vmlinux.h を自動生成）
make daemon     # Go デーモン単体（外部依存なしでビルド）
make daemon-gen # cilium/ebpf を fetch して bpf2go 実行 → build（Week 1+）
```

成果物:

| 成果物 | パス |
|---|---|
| カーネルモジュール | `kmod/s3fs_kfunc.ko` |
| eBPF オブジェクト | `bpf/s3fs_fastpath.bpf.o` |
| Go デーモン | `bin/s3fs-ebpf` |

> **カーネル 6.8 でのビルド互換**: スケルトンは 6.9+ 前提ですが、`make all` は 6.8 でも通ります（`insmod` 後の `mount` / `bpftool prog load` が実行時に 6.9+ の `bpf_arena` を要求します）。CI 用途で 6.8 コンテナ上でビルド検証だけ回す運用を想定。

---

## 起動

```bash
# 1. 観測スタック（MinIO + Prometheus + Grafana + node_exporter）
make compose-up

# 2. カーネルモジュールをロードして S3 バケットをマウント
./s3fs-ebpf.sh mount
#   ↑ 内部で insmod → daemon 起動 → syscall.Mount("...s3fs_ebpf...") を実行

# 3. 触ってみる（Week 0 は空 FS）
ls /mnt/s3data
```

`ENDPOINT` / `BUCKET_NAME` / `MOUNT_POINT` / `POLICY_FILE` を環境変数で上書き可能。

デーモンをフォアグラウンドで直叩きしたい場合:

```bash
sudo ./bin/s3fs-ebpf mount --bucket s3fs-test \
    --endpoint http://localhost:9000 \
    --mount-point /mnt/s3data
```

---

## CLI リファレンス

`s3fs-ebpf --help` の出力（`make readme` 実行時に自動再生成）:

```text
Usage: s3fs-ebpf <command> [options]

Commands:
  mount    Mount an S3 bucket via the s3fs_ebpf VFS
  unmount  Unmount a previously mounted target
  stats    Print fast/slow path counters
  policy   Hot-reload the cache policy blob

Run "s3fs-ebpf <command> --help" for command-specific flags.
```

サブコマンドの詳細フラグは `s3fs-ebpf <command> --help` で確認できます。例:

```text
Usage of mount:
  -bucket string
    	S3 bucket name
  -cache-dir string
    	Local NVMe tier cache dir (default "/var/cache/s3fs")
  -endpoint string
    	S3-compatible endpoint (default "http://localhost:9000")
  -mount-point string
    	Local mount point (default "/mnt/s3data")
  -policy string
    	Optional Lua/WASM policy path
```

---

## Make ターゲット一覧

`make help` の出力（`make readme` 実行時に自動再生成）:

```text
s3fs-ebpf — targets

Build (individual components):
  make kmod           Build kernel module   -> kmod/s3fs_kfunc.ko
  make bpf            Build eBPF program    -> bpf/s3fs_fastpath.bpf.o
  make daemon         Build Go user process -> bin/s3fs-ebpf

Build (combined / codegen):
  make all            kmod + bpf + daemon
  make build          alias of 'all'
  make daemon-gen     bpf2go codegen then daemon (needs network)

Housekeeping:
  make vmlinux        Regenerate bpf/vmlinux.h from running kernel BTF
  make vmlinux-clean  Drop bpf/vmlinux.h
  make kmod-clean     Clean kernel module build artefacts
  make bpf-clean      Clean eBPF build artefacts
  make daemon-clean   Clean Go build cache and bin/s3fs-ebpf
  make clean          Clean everything

Observability stack (docker compose):
  make compose-up     Start MinIO + Prometheus + Grafana + node_exporter
  make compose-down   Stop the stack
  make compose-logs   Tail compose logs

Benchmarks:
  make bench          Run fio + BCC jobs (requires mounted target)
  make bench-dry      Verify job files without running (dry-run)

Environment bootstrap (run once on a fresh host):
  make setup-kernel     Install mainline Linux 6.9 kernel (reboot after)
  make setup-toolchain  Install clang, bpftool, go, docker, fio, bcc

Documentation:
  make readme         Regenerate README.md from README.md.tmpl (envsubst)

Tip: 'make <target> -n' shows the recipe without executing it.
```

---

## ベンチマーク

```bash
make bench              # 実行（要マウント）
make bench-dry          # ジョブ定義の存在確認だけ
```

- `bench/fio/stat_latency.fio` — `filestat` エンジンで 10,000 件の `stat` レイテンシ
- `bench/fio/sequential_read.fio` — 大容量シーケンシャル read
- `bench/bcc/vfs_latency.py` — `vfs_read` / `vfs_getattr` の log2 ヒストグラム

結果は `bench/results/<timestamp>/` へ。

---

## 可視化

- Grafana: <http://localhost:3000> (admin / admin) → フォルダ **s3fs-ebpf** → **S3FS-eBPF Overview**
- Prometheus: <http://localhost:9090>
- MinIO Console: <http://localhost:9001> (minioadmin / minioadmin)

ダッシュボードは 4 パネル（Fast Path Hit 率 / VFS reqs/s / CS/s / MinIO S3 reqs/s）雛形入り。デーモンの `/metrics` は `:9600` で公開されます（Week 0 はゼロ値）。

---

## 片付け

```bash
./s3fs-ebpf.sh unmount
make compose-down
```

---

## ディレクトリ構成

```
s3fs-ebpf/
├── kmod/            カーネルモジュール（VFS + KFunc）
├── bpf/             eBPF プログラム
├── daemon/          Go コントロールプレーン
├── include/         C/Go/eBPF 共有型定義
├── policies/        Lua/WASM キャッシュポリシー
├── deploy/          docker-compose（MinIO + Prom + Grafana + node_exporter）
├── bench/           fio / BCC 性能試験ジョブ
├── scripts/         セットアップ・ヘルパスクリプト
├── docs/            設計 / 背景 / 先行研究
├── Makefile         トップレベル
├── README.md        自動生成
├── README.md.tmpl   README 生成用テンプレート（envsubst）
└── s3fs-ebpf.sh     mount / unmount / reload ラッパ
```

---

## ドキュメント再生成

README.md は `README.md.tmpl` から `envsubst` で組み立てられます。

```bash
make readme        # daemon をビルドしてから README.md を再生成
```

生成後の README.md は Git 管理対象です（レビュー時に差分を確認するため）。CLI や Make ターゲットを変更したら `make readme` を実行してからコミットしてください。

---

## ロードマップ

`docs/motivation.md §1` の 1 ヶ月スプリント計画に沿って進める:

| Week | ゴール | ステータス |
|---|---|---|
| **0** | ビルドテンプレート（本リポジトリ） | ✅ done |
| **1** | KFunc ↔ `bpf_arena` 疎通、ダミー Radix Tree 読み出し | pending |
| **2** | eBPF Fast Path、Verifier 拒否デモ、MinIO Slow Path 骨 | pending |
| **3** | Lua/WASM ポリシー、CLI `stats` / `policy reload` | pending |
| **4** | s3fs-fuse / Mountpoint との `stat` レイテンシ比較、Meetup 準備 | pending |

---

## 参考資料

**プロジェクト内**

- [`docs/deisign.md`](docs/deisign.md) — 統合設計書
- [`docs/motivation.md`](docs/motivation.md) — 背景、Meetup 発表構成、スプリント計画
- [`docs/related-works.md`](docs/related-works.md) — ExtFUSE / RFUSE との比較

**Linux カーネル / eBPF**

- Kernel Docs: [`Documentation/bpf/kfuncs.rst`](https://www.kernel.org/doc/html/latest/bpf/kfuncs.html)
- `bpf_arena` LWN 記事: <https://lwn.net/Articles/961941/>
- Linux VFS 概説: <https://www.kernel.org/doc/html/latest/filesystems/vfs.html>
- cilium/ebpf: <https://github.com/cilium/ebpf> / `bpf2go` doc

**先行研究論文**

- ExtFUSE (SOSP '19): <https://www.usenix.org/conference/atc19/presentation/bijlani>
- RFUSE (FAST '24): <https://www.usenix.org/conference/fast24/presentation/cho>

**既存 S3 ファイルシステム**

- Mountpoint for Amazon S3: <https://github.com/awslabs/mountpoint-s3>
- s3fs-fuse: <https://github.com/s3fs-fuse/s3fs-fuse>
