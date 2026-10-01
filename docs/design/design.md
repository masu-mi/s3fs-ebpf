# eBPF × KFunc 活用型 次世代 S3FS 統合設計書

## 1. コンセプトと位置付け

本プロジェクトは、Linux カーネルの `bpf_arena`、`bpf_obj_new`、およびカスタム **KFunc (カーネルモジュール)** を組み合わせ、FUSE のコンテキストスイッチオーバーヘッドを排除した超低レイテンシ S3 ファイルシステムです。

将来的に `struct_ops`（候補 B）を採用することで、**「eBPF による FUSE 置き換え汎用 VFS システム」** を確立し、その第一の実用例（S3FS）として位置付けます。

---

## 2. ライフサイクル & 運用フロー (Option 1)

```text
[ 1. insmod ]           s3fs_kfunc.ko をカーネルにロード (KFunc 登録)
     │
     ▼
[ 2. Daemon Launch ]    Go 管理プロセス起動
     │                   └─ bpf_arena Map を作成 (cilium/ebpf)
     │                   └─ Map ID を取得 (例: Map ID = 42)
     ▼
[ 3. VFS Mount ]        syscall.Mount("none", "/mnt/s3data", "s3fs_ebpf", 0, "map_id=42")
     │                   └─ カーネルモジュールが `bpf_map_get_curr_or_id(42)` で arena 取得
     ▼
[ 4. Steady State ]     Fast Path: eBPF + KFunc が bpf_arena 直参照で即時応答
                         Slow Path: Miss 時は bpf_ringbuf 経由で Go プロセスが S3 からフェッチ

```

---

## 3. 大容量ファイル（巨大データ）の将来設計（Tiered Storage）

`bpf_arena` の容量を超える巨大データは、カーネル内インデックスとローカルストレージを組み合わせた階層管理を行います。

* **スライディングウィンドウ・バッファリング:**
`bpf_arena` 内に固定長 chunk（例: 4MB × 1024 個 = 4GB）のバッファプールを確保。
* **レンジインデックス (`bpf_obj_new` + `bpf_rbtree`):**
eBPF 内で動的ノード `struct chunk_node { u64 start_offset; u64 end_offset; void *location; int storage_type; }` を保持し、データ領域が DRAM (`bpf_arena`) にあるか、ローカル NVMe ストレージにあるかを判別。
* **ローカル NVMe ステージング:**
`bpf_arena` から溢れたウォームデータは、Go デーモンがローカル NVMe ディスクに非同期フラッシュし、`copy_to_iter` または `splice` システムコールで VFS へ返却。

---

## 4. 利用イメージ (シェルスクリプト `s3fs-ebpf.sh`)

```bash
#!/usr/bin/env bash
set -euo pipefail

KMODULE="s3fs_kfunc.ko"
MOUNT_POINT="/mnt/s3data"
POLICY_FILE="./policies/ai_workload.lua"
BUCKET_NAME="my-target-bucket"

function mount_s3fs() {
    echo "[1/4] Loading KFunc Kernel Module..."
    if ! lsmod | grep -q "s3fs_kfunc"; then
        sudo insmod "./kmod/${KMODULE}"
    fi

    echo "[2/4] Creating Mount Point..."
    sudo mkdir -p "${MOUNT_POINT}"

    echo "[3/4] Launching Go Daemon & Mounting S3FS (Option 1: map_id integration)..."
    # Go デーモンが internal で bpf_arena 作成 -> Map ID 取得 -> mount を一括実行
    sudo ./daemon/s3fs-daemon \
        --bucket "${BUCKET_NAME}" \
        --mount-point "${MOUNT_POINT}" \
        --policy "${POLICY_FILE}" &
    
    echo "[4/4] Verifying Mount..."
    sleep 2
    df -h "${MOUNT_POINT}"
    echo "S3FS Successfully Mounted on ${MOUNT_POINT}"
}

function reload_policy() {
    echo "[+] Hot-reloading WASM/Lua policy..."
    sudo ./daemon/s3fs-daemon-cli policy-load \
        --mount-point "${MOUNT_POINT}" \
        --policy ./policies/log_streaming.lua
}

function unmount_s3fs() {
    echo "[1/2] Unmounting VFS..."
    sudo umount "${MOUNT_POINT}" || true

    echo "[2/2] Unloading Kernel Module..."
    if lsmod | grep -q "s3fs_kfunc"; then
        sudo rmmod s3fs_kfunc
    fi
    echo "S3FS Successfully Unmounted."
}

case "${1:-}" in
    mount)   mount_s3fs ;;
    unmount) unmount_s3fs ;;
    reload)  reload_policy ;;
    *)       echo "Usage: $0 {mount|unmount|reload}" ;;
esac

```

---

## 5. ディレクトリ構成

```text
s3fs-ebpf/
├── README.md
├── s3fs-ebpf.sh                # 運用・マウント用スクリプト
├── Makefile
│
├── kmod/                       # KFunc & VFS ドライバ (カーネルモジュール)
│   ├── Makefile
│   ├── s3fs_kfunc_main.c       # モジュール初期化、file_system_type 登録
│   ├── vfs_ops.c               # .read_iter, .open, .getattr の VFS フック
│   └── tree_traversal.c        # bpf_arena を走査する C 言語 KFunc (Verifier 回避)
│
├── bpf/                        # eBPF フックコード (C言語)
│   ├── s3fs_fastpath.bpf.c     # fentry 補獲 & KFunc 呼び出し
│   └── arena_types.h           # bpf_arena メタデータ構造体定義
│
├── daemon/                     # コントロールプレーン (Go言語 / cilium/ebpf)
│   ├── main.go                 # デーモン起動・CLI (mount, unmount)
│   ├── ebpf_bridge.go          # cilium/ebpf による bpf_arena 生成・Mmap
│   ├── mounter.go              # Map ID 付与 syscall.Mount 呼び出し
│   ├── s3_fetcher.go           # MinIO / AWS S3 API 非同期通信
│   ├── tiered_cache.go         # NVMe ↔ bpf_arena 巨大データ階層管理
│   └── policy_engine.go        # GopherLua / Wasmtime 実行環境
│
├── policies/                   # ホットリロード可能スクリプト
│   ├── default_policy.lua
│   └── ai_workload.lua
│
└── include/
    └── s3fs_shared_types.h     # C/Go/eBPF 全域で共有する Inode/Stat 構造体

```


## 巨大データ取り扱い

### 従来の FUSE 版 S3FS における巨大データの扱い

* **`s3fs-fuse` (ローカルディスク・ステージング):** 巨大なファイルやマルチパートアップロード時、指定したローカルストレージ（`/tmp` や NVMe キャッシュ）に一時ファイルを作成し、ディスク上で I/O バッファリングを行ってから S3 に送信／返却します。
* **`Mountpoint for Amazon S3` (ストリーミング & ページキャッシュ):** 大容量のシーケンシャル Read に対しては、ローカルディスクに溜め込まずメモリ上のリングバッファで 8MB 単位などの「ストリーミング chunk」としてパイプライン処理します（一部ローカルディスクキャッシュも併用可能）。

### 本設計における将来的な巨大データ対応方針（Tiered Hybrid 方式）

本設計では、`bpf_arena`（DRAM / Shared Memory）のサイズ上限（例: 4GB）を超える巨大ファイル（数GB〜数TB）に対して、以下の **階層化ストレージ（Tiered Strategy）** を採用します。

1. **ホットデータ (DRAM / `bpf_arena`):** ディレクトリツリー、`stat` メタデータ、現在読み書き中のアクティブな chunk（スライディングウィンドウ）。
2. **ウォームデータ (ローカル NVMe / SSD):** Go デーモンが裏側で `io_uring` や Direct I/O を使ってローカル NVMe にステージングキャッシュを保持。
3. **インデックス管理 (`bpf_obj_new` + `bpf_rbtree`):** eBPF/KFunc 側で `bpf_rbtree` を構築し、ファイルオフセット範囲（例: `0-5MB` は `bpf_arena` 内、`5MB-100MB` は NVMe 上のブロック ID）を $O(\log N)$ で超高速マッピング。

