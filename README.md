<!-- Generated from README.md.tmpl by `make readme`. Do not edit README.md directly. -->
# s3fs-ebpf

FUSE を経由せずに Linux VFS へ直接ぶら下がる、次世代 S3 ファイルシステムの PoC。
Linux 6.12 LTS の **`bpf_arena`**（生ポインタ・ゼロコピー共有メモリ、6.9 導入・6.12 で安定）、カーネルモジュール上の **KFunc**（Verifier 制限突破 + Layer 0 primitives）、**eBPF struct_ops による policy engine**（Layer 2/3、hot swap 可能）、**Go 製コントロールプレーン** の 4 段構成でメタデータ操作を IPC 0 回で完結させます。

> **Status (2026-09-28)**: Week 0-4 完了。read + write path、動的な
> policy engine (CEL + built-in eBPF + user eBPF + KFunc の 4 層)、
> hit-rate bench で **W-TinyLFU 88.5% / SIEVE 66% / LRU 7.5% /
> freq2bit 6.5%** を実測、Grafana に 21 panel。次段階は Meetup
> 登壇、および Week 5+ の分散 / multi-tenant / 大ファイル対応。
>
> **すぐに使ってみたい場合**:
> - 概要と数値 → [`docs/user/motivation.md §0`](docs/user/motivation.md)
> - Meetup demo (35 分台本) → [`docs/user/demo-scenario.md`](docs/user/demo-scenario.md)
> - **自作 policy を書く** → [`docs/user/writing-policies.md`](docs/user/writing-policies.md)
> - **FS 拡張 (POSIX 準拠 + 実用性)** → [`docs/user/posix-and-practical-notes.md`](docs/user/posix-and-practical-notes.md)
> - **GCS backend で動かす** → [`docs/user/gcs-backend.md`](docs/user/gcs-backend.md)
> - **フックポイントとオブジェクト図** → [`docs/design/hook-timings-diagrams.md`](docs/design/hook-timings-diagrams.md)
> - **Partial admit の内部構造とシーケンス** → [`docs/design/partial-admit-internals.md`](docs/design/partial-admit-internals.md)
> - policy engine の詳細 → [`docs/design/policy-engine-design.md`](docs/design/policy-engine-design.md)
> - hit-rate bench 数値と caveats → [`docs/journal/hit-rate-bench.md`](docs/journal/hit-rate-bench.md)
> - 実装ハマりポイント → [`docs/dev-notes/lessons-learned.md`](docs/dev-notes/lessons-learned.md)
> - 残 backlog → [`docs/journal/backlog.md`](docs/journal/backlog.md)
> - **トピック別索引** → [`docs/INDEX.md`](docs/INDEX.md)

---

## アーキテクチャ

```
  syscall (stat / read / write / open)
              │
              ▼
  ┌──────────────────────────────────────────────┐
  │ Fast Path (Data Plane / Kernel)              │
  │  eBPF (fentry + struct_ops policy vtable)    │
  │  ↔ KFunc (kmod): Layer 0 primitives          │
  │  → bpf_arena 直参照で即応答                   │
  └──────────────┬──────────────────┬────────────┘
                 │ Hit              │ Miss
                 ▼                  ▼
        [ゼロコピー返却]     [Slow Path / User]
                              Go daemon → S3 (MinIO/AWS)
                              CEL DSL + eBPF policy hot swap
```

### Policy engine の 4 layer

| 層 | 内容 | 実装 |
|---|---|---|
| **Layer 1** | CEL (Google) DSL — prewarm 判定 / Layer 2 program 選択 | `policies/default.cel` |
| **Layer 2** | built-in eBPF policy 9 種 (observe / LRU / SIEVE / S3-FIFO / **W-TinyLFU** / pressure / time_decay / size_aware / freq2bit) | `bpf/policy_*_ops.bpf.c` |
| **Layer 3** | user eBPF plug (SHA256 allowlist で gate) | `--policy-bpf=file:<path>` |
| **Layer 0** | KFunc primitives (`admit_count` / `softcap` / `evictions_total` / `time_ns` / `ino_size`) | `kmod/tree_traversal.c` |

`POST /policy/reload` で **走行中に curl 一発で policy 差替え**、in-flight IO は古い vtable で完走。

先行研究比較（ExtFUSE / RFUSE）と根拠は [`docs/user/related-works.md`](docs/user/related-works.md) にあります。

---

## 前提環境

| 要件 | バージョン |
|---|---|
| OS | Ubuntu 24.04 LTS |
| Kernel | **Linux 6.12 LTS**（`bpf_arena` 必須。6.9 で導入、6.12 で LTS 化） |
| Compiler | **clang 19+ (推奨 20)** — `__BPF_FEATURE_ADDR_SPACE_CAST` (`-mcpu=v3`) が必要。Ubuntu 24.04 標準の clang-18 では arena deref パスがビルドできません |
| Go | 1.22+ |
| Tools | `bpftool`, `fio`, `bcc`, `docker compose` v2, `gettext-base` (envsubst) |

---

## 環境準備

```bash
# 1. mainline 6.12 LTS カーネルを導入して再起動
make setup-kernel
sudo reboot
# 再起動後: uname -r  →  6.12.x-... であることを確認

# 2. ツールチェイン一式（clang/llvm/bpftool/go/fio/bcc/docker）
make setup-toolchain

# 3. eBPF 用の vmlinux.h を実行中カーネルの BTF から生成
make vmlinux
```

> **mainline カーネル特有の落とし穴 (Ubuntu 24.04)** — `bpf_arena` の要件で
> mainline カーネル (`kernel.ubuntu.com/mainline`) を使う前提のため、Ubuntu
> archive にある stock 向けパッケージ (`linux-tools-$(uname -r)` /
> `linux-headers-$(uname -r)`) は解決できません。`setup-toolchain` は以下で
> 回避します:
>
> - **bpftool**: Ubuntu の `bpftool` は virtual package で、実体は
>   `linux-tools-<kernel>` 側。`linux-tools-generic` (stock 6.8 系) を入れ、
>   `/usr/lib/linux-tools/*/bpftool` を `/usr/local/bin/bpftool` に symlink して
>   `/usr/bin/bpftool` wrapper (uname -r で dispatch して失敗) を上書きします。
>   bpftool は BTF/map/prog 操作については kernel version に非依存です。
> - **kernel headers**: `kernel.ubuntu.com/mainline/v<ver>/amd64/` から
>   `linux-headers-<ver>_<build>_all.deb` と
>   `linux-headers-<ver>-generic_<build>_amd64.deb` を取得して `dpkg -i` します。
>   セキュリティ上、実行するコマンドを事前に echo して y/N プロンプトで
>   確認します。TTY が無い環境で自動化したい場合は
>   `S3FS_INSTALL_MAINLINE_HEADERS=1` を付けて `make setup-toolchain` を叩いて
>   ください。URL が 404 の場合は build timestamp が別リビジョンの可能性が
>   あるので、ブラウザで一覧を確認してください。
> - **perf**: mainline 用は archive に無いのでスキップします。必要な場合は
>   mainline カーネルソースから手動 build してください。

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

> **カーネル 6.8 でのビルド互換**: 実行時要件は 6.12 LTS ですが、`make all` は 6.8 でも通ります（`insmod` 後の `mount` / `bpftool prog load` が実行時に `bpf_arena` を要求します）。CI 用途で 6.8 コンテナ上でビルド検証だけ回す運用を想定。

---

## 起動

> **Object storage は Pigsty Silo (MinIO fork) を使用** — MinIO Inc は 2026-10
> 時点で quay.io / Docker Hub 両方で community イメージの anonymous pull を
> 制限しているため、`deploy/docker-compose.yml` では **`docker.io/pgsty/silo:latest`**
> (MinIO の AGPL-3.0 fork, <https://silo.pgsty.com/>) に切替えています。
> CLI (`silo server /data --console-address :9001`) / 環境変数 (`MINIO_ROOT_USER`
> 等) / health endpoint (`/minio/health/ready`) / port は MinIO と完全互換なので、
> daemon・bench スクリプト側は無変更です。`mc` も同一イメージ内に同梱されており、
> `minio-init` も同じ silo イメージで動かしています。

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
  mount        Mount an S3 bucket via the s3fs_ebpf VFS
  unmount      Unmount a previously mounted target
  stats        Print fast/slow path counters
  policy       Hot-reload the cache policy blob
  test-arena   Week 1 probe: bpf_arena + KFunc round-trip (requires 6.9+ kernel
               and the s3fs_kfunc module loaded)
  fastpath     Week 2 probe: attach fentry/vfs_getattr, stream ringbuf miss
               events and hit/miss counters until interrupted
  verifier-demo Week 2 probe: try to load an intentionally-rejected pure eBPF
               arena walker and print the verifier's rejection log
  s3-list      List a bucket. Backend selected by --backend s3|gcs
               (S3-compatible endpoint or Google Cloud Storage).
  s3-stat      Stat a single object. Backend selected by --backend s3|gcs.
  probe-kvbase Week 4 (a) spike: create an arena, ioctl(/dev/s3fs_ebpf,
               S3FS_IOC_PROBE_KVBASE) to read its kernel-VM base, print
               it alongside the user-VM base for comparison
  microbench   Week 4 (e): tight-loop stat/read latency on a target path,
               reports min/p50/p95/p99/max (no fork/exec overhead)
  hash-policy  Print SHA256 for a user policy .bpf.o (for allowlist entries).
               See --policy-allowlist and policies/allowlist.example.txt.
  trace-replay Offline policy simulator (P-04). Feeds a workload trace or
               a synthetic Zipf sequence through Go-side LRU/SIEVE sims
               and reports per-policy hit rates in milliseconds.

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

Runtime lifecycle (kmod + mount + daemon):
  make mount          insmod kmod + mount -t s3fs_ebpf on /mnt/s3demo
  make unmount        umount + rmmod (also stops any running daemon)
  make daemon-run     Foreground: run bin/s3fs-ebpf fastpath (needs mount + compose-up)
  make daemon-stop    SIGINT any running daemon

Benchmarks (all auto-start compose-up requirement + kmod/mount/daemon in-script):
  make bench           Latency bench (hyperfine + microbench + fio) vs s3fs-fuse
  make bench-write     Write-back throughput bench (dd 1M/10M/100M, ebpf vs fuse)
  make bench-scale     Parallel scaling (numjobs=1..64) stat + read
  make bench-3way      ebpf vs fuse-raw vs fuse-cache 3-way comparison
  make setup-bench-env install some pkgs

Week 1 verification (requires 6.12+ kernel):
  make test-arena     insmod + run bpf_arena/KFunc round-trip probe + rmmod

Environment bootstrap (run once on a fresh host):
  make setup-kernel     Install mainline Linux 6.12 LTS kernel (reboot after)
  make setup-toolchain  Install clang, bpftool, go, docker, fio, bcc

Documentation:
  make readme         Regenerate README.md from README.md.tmpl (envsubst)

Tip: 'make <target> -n' shows the recipe without executing it.
```

---

## 動作確認 (`test-arena`)

Linux 6.12+ ホスト上で以下を叩くと 3 つのサブテストが順に走ります:

```bash
make test-arena     # sudo insmod → daemon test-arena → sudo rmmod
```

サブテスト内容:

| ID | 内容 | 検証する仮説 |
|---|---|---|
| A | `BPF_MAP_TYPE_ARENA` を daemon 側で作成、mmap して往復 read/write | bpf_arena (6.9 導入 / 6.12 LTS) の standalone create/mmap が動く |
| B | BPF プログラムが宣言した `BPF_MAP_TYPE_ARENA` を daemon が mmap で populate → SEC(`"syscall"`) の BPF プログラムが `__arena` deref 経由で読み出す | daemon ↔ BPF が同一 arena 上でゼロコピー共有できる |
| C | 上記 BPF プログラムが kmod 内の `s3fs_kfunc_echo(u32) → 2v+1` を呼ぶ | KFunc の登録・ロード・呼び出しパスが Verifier を通過 |

期待出力 (抜粋):

```text
== subtest A: bpf_arena create + mmap ==
[A] arena map id=42 size=4096 bytes
[A] roundtrip OK: wrote/read "hello arena"
== subtest B+C: bpf_arena populate + KFunc call ==
[B] arena mmap base=0x7ab...000 size=4096 bytes
[B] wrote 32 dummy inodes into arena
[B/C] prog ret=0 out_ino=0x1003 out_kfunc=43
PASS
```

---

## Fast Path 動作確認 (`fastpath`)

`fentry/vfs_getattr_nosec` をアタッチし、`stat` 系システムコールごとに Hit/Miss を分別してリングバッファ+per-CPU カウンタに落とします。

```bash
# 1. kmod をロード (未ロードなら)
sudo insmod kmod/s3fs_kfunc.ko

# 2. s3fs_ebpf をマウント (getattr が s_magic フィルタを通るのに必要)
sudo mkdir -p /mnt/s3demo
sudo mount -t s3fs_ebpf none /mnt/s3demo

# 3. 別 shell で fastpath を起動 (Ctrl+C で終了)
sudo ./bin/s3fs-ebpf fastpath --sample-every=1s

# 4. さらに別 shell で stat を叩く
stat /mnt/s3demo      # 初回: [miss] ino=1 op=getattr が飛ぶ
stat /tmp             # 非 s3fs → unknown_fs カウンタが伸びる

# 5. Fast Path Hit を模倣: ino=1 → arena offset=0 を index に注入
BPFTOOL=/usr/lib/linux-tools/$(uname -r | awk -F- '{print $1"-"$2}')/bpftool
INDEX_ID=$(sudo $BPFTOOL map list | awk '/s3fs_inode_index/{print $1}' | tr -d ':')
sudo $BPFTOOL map update id "$INDEX_ID" \
    key 0x01 0 0 0 0 0 0 0 \
    value 0 0 0 0
stat /mnt/s3demo      # 以降: fast_hits がインクリメントされ ringbuf は静か
```

期待出力例 (`fastpath` 側):

```text
[fastpath] attached fentry/vfs_getattr_nosec; arena base=0x7ab...000 size=4096
[fastpath] stat any path to trigger events. Ctrl+C to stop.
[counters/sample] fast_hits=0 slow_misses=0 unknown_fs=25788
[miss] ino=1 op=getattr
[miss] ino=1 op=getattr
[counters/sample] fast_hits=0 slow_misses=2 unknown_fs=56068
[counters/sample] fast_hits=3 slow_misses=11 unknown_fs=130815   ← ino=1 を index に注入後
```

**設計上の注意** — fentry フックは kstat を「返さない」（fentry は observe 専用）。実際の getattr レスポンスは kernel module の `inode_operations.getattr` から arena を読んで埋める設計で、その配線は Week 3 (kmod 側の inode 生成) 以降。今の fastpath は Hit/Miss メトリクスと Slow Path キック (ringbuf → daemon → S3 fetch) の 2 役に絞っている。

### Week 3: 実 VFS + fastpath prewarm フルスタック

kmod が 3 つのデモオブジェクト (hello.txt / world.txt / models_weights.bin) を lookup / readdir / read / write できるようになったので、`stat`・`cat`・`echo >` が通常のファイルとして通ります。daemon の fastpath は起動時に entry table の全 ino を prewarm するため、**1 発目の `stat` から Fast Path Hit** としてカウントされます。

```bash
# 1. kmod と mount
sudo insmod kmod/s3fs_kfunc.ko
sudo mkdir -p /mnt/s3demo && sudo mount -t s3fs_ebpf none /mnt/s3demo

# 2. VFS 基本動作
ls -la /mnt/s3demo                          # 3 files
cat /mnt/s3demo/hello.txt                   # "hello, s3fs-ebpf"
sudo bash -c 'echo "new content" > /mnt/s3demo/world.txt'
cat /mnt/s3demo/world.txt                   # "new content"

# 3. Fast Path 完全形 (--s3 で real S3 metadata を反映)
make compose-up
sudo ./bin/s3fs-ebpf fastpath --s3 --sample-every=1s
#   [prewarm] ino=100 name=hello.txt size=16 src=s3://hello.txt → offset=0
#   [prewarm] ino=102 name=models_weights.bin size=22 src=s3://models/weights.bin → offset=32
#   [prewarm] ino=101 name=world.txt size=12 src=s3://world.txt → offset=64

# 4. 別 shell で連続 stat → 全部 fast_hits に計上される
for f in hello.txt world.txt models_weights.bin; do stat /mnt/s3demo/$f > /dev/null; done
#   [counters/sample] fast_hits=9 slow_misses=0 unknown_fs=...
```

**現状の制約と Week 4 に持ち越すこと**:

- kmod のファイル内容は per-inode `kmalloc` プール。Week 4 で arena バッキング (KFunc 経由で arena kernel-VM base を取得) に切り替え、BPF Fast Path が「実データを直接返す」形へ
- kmod の entry table (`s3fs_entries[]`) が compile-time 静的。dynamic bucket sync (ioctl / netlink) は未実装。今の運用は「S3 バケットに `hello.txt` / `world.txt` / `models/weights.bin` を入れておく」規約で成立している
- `write()` は成功するが S3 write-back は無し (memory-only)。Week 4 の dirty page 追跡 + async upload で対応

### 統合デモ: miss → S3 fetch → arena populate → 次回 hit

`--s3` を付けると fastpath は miss ringbuf event を消費して S3 metadata を取得し、arena + `s3fs_inode_index` を warmup します。次の `stat` は Fast Path Hit として counter が動きます。

```bash
# 1. MinIO を立てて bucket にオブジェクトを入れる (前節参照)
make compose-up
docker run --rm --network=deploy_default --entrypoint /bin/sh docker.io/pgsty/silo:latest -c '
  mc alias set local http://minio:9000 minioadmin minioadmin
  echo hello | mc pipe local/s3fs-test/hello.txt
  echo world | mc pipe local/s3fs-test/world.txt'

# 2. kmod と mount
sudo insmod kmod/s3fs_kfunc.ko
sudo mkdir -p /mnt/s3demo && sudo mount -t s3fs_ebpf none /mnt/s3demo

# 3. S3 統合版 fastpath (別 shell)
sudo ./bin/s3fs-ebpf fastpath --s3 --sample-every=1s

# 4. さらに別 shell で連続 stat
for i in 1 2 3 4 5; do stat /mnt/s3demo > /dev/null; done
```

期待出力 (`fastpath` 側):

```text
[fastpath] attached fentry/vfs_getattr_nosec; arena base=0x7a...000 size=4096
[fastpath] s3 backend: bucket=s3fs-test (will warm arena on miss)
[fastpath] stat any path to trigger events. Ctrl+C to stop.
[counters/sample] fast_hits=0 slow_misses=0 unknown_fs=2005
[miss] ino=1 op=getattr
[warm] ino=1 ← s3://s3fs-test (3 objects) → arena offset=0
[miss] ino=1 op=getattr        ← ringbuf にキューされた重複 miss、IsWarmed() で S3 は再叩きしない
[miss] ino=1 op=getattr
[counters/sample] fast_hits=4 slow_misses=4 unknown_fs=8236  ← ここで cache warm 完了
[counters/sample] fast_hits=8 slow_misses=4 unknown_fs=14051 ← 以降は全て hit
```

**制限事項**:

- 現状 `ino=1` (マウントポイントの root inode) だけを S3 の bucket listing に紐付けている。他 ino が来ると `[warm] ino=N skipped (no S3 key mapping yet — Week 3)` を出す。理由は kmod がまだ per-object inode を作らないため。Week 3 で kmod の `inode_operations` を拡張したら任意 ino → S3 key の解決を daemon 側で書く
- arena は 1 ページ (128 slot) しか確保していない。溢れると `WarmCache` が `ErrCacheFull` を返す。デモ規模では問題にならない
- ringbuf は BPF 側で 256KB。ドレインが遅れて溢れるとイベントロスするが、`slow_misses` counter は fentry 側で直接インクリメントするので数値は正しい

---

## Verifier 拒否デモ (`verifier-demo`)

「なぜ KFunc が必要か」を verifier のログで示すためのデモ。純 eBPF で arena 上の linked-list を `while (n) { n = n->next; }` で辿ろうとして、verifier の 1M 命令上限 (`BPF_COMPLEXITY_LIMIT_INSNS`) に落ちる様子を見せます。

```bash
sudo ./bin/s3fs-ebpf verifier-demo
```

期待出力 (末尾抜粋):

```text
== verifier-demo ==============================================
Attempting to load s3fs_deep_walk_reject (pure eBPF arena walk
with an unbounded pointer chase). ...
=== verifier log ===
...
	7: (bf) r2 = addr_space_cast(r2, 0, 1)   ; R2_w=arena
	8: (79) r4 = *(u64 *)(r2 +8)             ; R2_w=arena R4_w=scalar()
	...
	12: (55) if r2 != 0x0 goto pc-6          ; back-edge
	...
	BPF program is too large. Processed 1000001 insn
	processed 1000001 insns (limit 1000000) max_states_per_insn 4 total_states 9664 peak_states 9664 mark_read 1

== outcome ====================================================
The verifier refused this program. In s3fs-ebpf the same walk
is expressed as a KFunc call (see kmod/tree_traversal.c and
the `make test-arena` subtest C), which runs unbounded C and
bypasses the eBPF verifier entirely.
```

対比:

| 実装 | Verifier | 効果 |
|---|---|---|
| pure eBPF `while (n) { n = n->next; }` | ❌ `BPF program is too large. Processed 1000001 insn` | 深さ不定のツリー走査は静的解析で 1M 命令の状態爆発を起こす |
| KFunc (`s3fs_kfunc_traverse_stub` / kmod C コード) | ✅ 通過 (`make test-arena` subtest C) | KFunc の境界で verifier が「信頼」する → 中身は普通の C なのでループ制限なし |

デモは exit 0 で「rejected as expected」を返します。将来のカーネル改良で万一ロードが通ってしまった場合は exit 2 (「デモは obsolete」) を返します。

---

## MinIO Slow Path 疎通 (`s3-list` / `s3-stat`)

daemon が本物の S3-compatible endpoint と会話できることを確認するスケルトンです。`fentry` から見た miss → S3 fetch → arena populate → hit の完全ループは kmod の inode ops (Week 3) を待つため今週はまだ組みませんが、S3 API 側の骨格はここまで動きます。

```bash
# 1. 観測スタック (MinIO 含む) を起動
make compose-up
#   MinIO:  http://localhost:9000  (minioadmin / minioadmin)
#   Console: http://localhost:9001
#   compose 内で bucket "s3fs-test" が自動作成される

# 2. 何かオブジェクトを put (mc / aws / SDK いずれでも可)
docker run --rm --network=deploy_default --entrypoint /bin/sh docker.io/pgsty/silo:latest -c '
  mc alias set local http://minio:9000 minioadmin minioadmin
  echo "hello, s3fs-ebpf" | mc pipe local/s3fs-test/hello.txt
  echo "AI weights"       | mc pipe local/s3fs-test/models/weights.bin'

# 3. daemon から list / stat
./bin/s3fs-ebpf s3-list
./bin/s3fs-ebpf s3-list --prefix=models/
./bin/s3fs-ebpf s3-stat hello.txt
```

期待出力:

```text
$ ./bin/s3fs-ebpf s3-list
bucket=s3fs-test prefix="" count=3
  hello.txt            17  787e25ff430270dcf04ba82bba3032c1-1  2026-09-21T01:00:05Z
  hostname             13  68d9ef9c4bdd75fab935fc116df6111b     2026-09-21T01:00:05Z
  models/weights.bin   23  139f878b2b374df6ca2a2cc2199056a1-1  2026-09-21T01:00:05Z

$ ./bin/s3fs-ebpf s3-stat hello.txt
bucket=s3fs-test key=hello.txt
  size          17
  etag          787e25ff430270dcf04ba82bba3032c1-1
  content-type  text/plain
  last-modified 2026-09-21T01:00:05Z
```

共通フラグ:

| フラグ | デフォルト | 説明 |
|---|---|---|
| `--endpoint` | `localhost:9000` | S3-compatible endpoint (`host:port`, scheme なし) |
| `--bucket` | `s3fs-test` | Bucket 名 |
| `--access-key` | `minioadmin` (`$S3FS_ACCESS_KEY`) | アクセスキー |
| `--secret-key` | `minioadmin` (`$S3FS_SECRET_KEY`) | シークレットキー |
| `--ssl` | `false` | HTTPS を使う |

AWS 本番 endpoint (`s3.us-west-2.amazonaws.com` 等) と IAM キーを指定すれば MinIO を経由せずに直接叩けます。

---

> **Week 2 の学習事項** — subtest B/C を「本物の arena deref」に上げるために踏んだ地雷:
>
> - `bpf_addr_space_cast` は `-mcpu=v3` を要求 (`__BPF_FEATURE_ADDR_SPACE_CAST`)。Ubuntu 24.04 標準の clang-18 では未サポート。Makefile が `clang-20` を優先検出します
> - verifier は arena map が `prog->aux->arena` に関連付いていないと `addr_space_cast` を拒否。単に `.maps` セクションに置くだけでは足りず、プログラム側で `LD_MAP_FD` を発行させる必要がある (`asm volatile("" :: "r"(&s3fs_arena));` で強制)
> - verifier は arena の `user_vm_start` が未設定だとロードを拒む。daemon が `Map.Memory()` で mmap してから BPF プログラムをロードする 2 段順序が必要 (`daemon/internal/ebpf/loader.go`)
> - cilium/ebpf v0.22.0 (main HEAD 含む) の `asm/instruction.go` `Marshal` が `bpf_addr_space_cast` のエンコーディングを壊すバグを踏んだ。`Instruction.Offset=1` (BPF_ADDR_SPACE_CAST マーカー) が plain Mov に対して常に 0 で上書きされてしまう。`daemon/third_party/ebpf/` にパッチ済みのソースを vendor して `go.mod` の `replace` で差し込んでいる。詳細は `daemon/third_party/ebpf/README-s3fs-patch.md`
> - `SEC("fentry/vfs_getattr")` は fires しない — 現行 kernel の `newfstatat` は `vfs_statx_path` → `vfs_getattr_nosec` を直接呼ぶため。ftrace で `set_ftrace_filter=vfs_getattr*` して確認したのち `SEC("fentry/vfs_getattr_nosec")` へ変更

---

## ベンチマーク

`make bench*` 系は kmod insmod / mount / daemon 起動 / s3fs-fuse 対抗 mount / cleanup までを script 内 trap で完結させます。前提は `make compose-up` (MinIO/Prometheus/Grafana up) と `make all` (kmod + bpf + daemon 済) のみ:

```bash
make compose-up
make all
make bench              # latency bench (hyperfine + microbench + fio)     → /tmp/bench/
make bench-write        # write-back throughput 1M/10M/100M                → /tmp/bench-write/
make bench-scale        # 並列度スケール numjobs=1..64                     → /tmp/bench-scale/
make bench-3way         # ebpf vs fuse-raw vs fuse-cache 3-way             → /tmp/bench-3way/
make bench-dry          # 旧 runner.sh の存在確認だけ (Week 0/1 skeleton)
```

計測 script は `scripts/bench-*.sh`。`docs/journal/benchmark-results.md §7-§10` に結果解釈。

**手動ライフサイクル**:

```bash
make mount              # insmod + mount /mnt/s3demo
make daemon-run         # daemon foreground; Ctrl+C で停止
make unmount            # daemon-stop + umount + rmmod
```

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
├── scripts/         セットアップ・ヘルパスクリプト + bench (bench-latency/write/scale/3way)
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

`docs/user/motivation.md §1` の 1 ヶ月スプリント計画に沿って進める:

| Week | ゴール | ステータス |
|---|---|---|
| **0** | ビルドテンプレート（本リポジトリ） | ✅ done |
| **1** | `bpf_arena` 疎通、daemon↔BPF 共有メモリ、KFunc 呼び出し (`make test-arena`) | ✅ done |
| **2 (a)** | arena `__arena` deref パス (`bpf_addr_space_cast`) の疎通 | ✅ done (cilium/ebpf の Marshal バグを vendor patch で回避) |
| **2 (b)** | eBPF Fast Path (`fentry/vfs_getattr_nosec` → hit/miss counter + ringbuf) | ✅ done (`s3fs-ebpf fastpath`) |
| **2 (c)** | Verifier 拒否デモ | ✅ done (`s3fs-ebpf verifier-demo`) |
| **2 (d)** | MinIO Slow Path 骨 (minio-go / bucket list & stat) | ✅ done (`s3fs-ebpf s3-list` / `s3-stat`) |
| **3 (a)** | kmod が entry table のオブジェクトを VFS に materialise (lookup / readdir / read / write) | ✅ done (`ls` / `cat` / `echo >` が動作) |
| **3 (b)** | daemon の fastpath 起動時 prewarm (entry table の全 ino を arena に admit) | ✅ done (`--s3` で real S3 metadata を反映) |
| **3 (c)** | file 内容の arena バッキング (kmalloc → arena) | ✅ done (Path B GUP prewarm + Path C read-miss upcall、`docs/design/read-path-design.md` §3) |
| **3 (d)** / **4 (d)** | policy engine + hot reload (CEL + built-in eBPF + user eBPF + KFunc の 4 層) | ✅ done (`docs/design/policy-engine-design.md`) |
| **4 (b)** | dynamic bucket sync (basename 衝突解消、subdir 対応) | ✅ done (`docs/journal/week4b-implementation-notes.md`、`docs/design/g01-subdir-plan.md`) |
| **4 (c)** | write-back path (fsync blocking / unlink / rename / coalescer) | ✅ done (`docs/journal/week4c-implementation-notes.md`) |
| **4 (e)** | s3fs-fuse / Mountpoint との latency / throughput bench、Meetup 準備 | ✅ done (`docs/journal/benchmark-results.md`、`docs/journal/hit-rate-bench.md`) |
| **5+** | 分散 / multi-tenant / 大ファイル対応、残 backlog | pending (`docs/journal/backlog.md`) |

---

## 参考資料

**プロジェクト内**

- [`docs/INDEX.md`](docs/INDEX.md) — トピック別索引 (user / design / journal / dev-notes / misc)
- [`docs/design/design.md`](docs/design/design.md) — 統合設計書
- [`docs/user/motivation.md`](docs/user/motivation.md) — 背景、Meetup 発表構成、スプリント計画
- [`docs/user/related-works.md`](docs/user/related-works.md) — ExtFUSE / RFUSE との比較

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
