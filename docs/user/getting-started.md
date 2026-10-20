# 使い始める: 環境構築・ビルド・起動・計測

[`README.md`](../../README.md) の Quick start の詳細版。前提環境の確認から、ビルド、マウント、テスト、ベンチマーク、片付けまでを並べる。CLI のフラグと Make ターゲットの全量は [`cli-reference.md`](cli-reference.md)、初期 (Week 1〜3) の個別の動作確認手順は [`../journal/early-walkthroughs.md`](../journal/early-walkthroughs.md) にある。

## 前提環境

| 要件 | バージョン |
|---|---|
| OS | Ubuntu 24.04 LTS / Debian (trixie で 6.12 標準) |
| Kernel | **Linux 6.12 LTS 以上**（`bpf_arena` 必須。`kmod/s3fs_ebpf_dev.c` が 6.12 未満を `#error`、arena レイアウトは 6.12 系で検証） |
| Kernel headers | `/lib/modules/$(uname -r)/build` + その直下の `vmlinux`（module BTF 生成に必要） |
| Compiler | **clang 19+ (推奨 20)** — `__BPF_FEATURE_ADDR_SPACE_CAST` (`-mcpu=v3`)。clang-18 だと arena パスが黙って別実装になるので `bpf/Makefile` が拒否します。kmod 用 gcc はカーネルをビルドした版（最低 14）、pahole はカーネルの `CONFIG_PAHOLE_VERSION` 以上 |
| Go | `daemon/go.mod` の `go` 行 (1.25+) |
| Tools | `bpftool`, `docker compose` v2, `gettext-base` (envsubst)。bench は `hyperfine`, `fio`, `s3fs` |

依存箇所（カーネル版 / ヘッダ / コンパイラ / ツール / テストツール）ごとの症状と解消方法は
[`docs/dev-notes/build-env-dependencies.md`](../dev-notes/build-env-dependencies.md) にまとめています。

---

## 環境準備

```bash
# 0. 何が足りないかを確認 (NG ごとに直し方を表示)
make doctor

# 1. (Ubuntu) mainline 6.12 LTS カーネルを導入して再起動
make setup-kernel
sudo reboot
# 再起動後: uname -r  →  6.12.x-... であることを確認

# 2. ツールチェイン一式 (clang/bpftool/headers/gcc・pahole 整合/go/bench ツール/docker)
make setup-toolchain        # 個別: scripts/setup-toolchain.sh clang bpftool ...
                            # 実行内容の確認だけ: DRY_RUN=1 scripts/setup-toolchain.sh

# 3. 確認 (NG 0 ならビルド可能)
make doctor
```

カーネルを入れ替えたら `make setup-kbuild`（ヘッダ・gcc/pahole・`$KDIR/vmlinux` の再整備）を
再実行してください。Makefile が使う `CLANG` / `KMOD_CC` / `KMOD_PAHOLE` / `BPFTOOL` / `KDIR` は
`scripts/lib/toolchain.sh` が自動検出し（`make toolchain` で表示）、`make CLANG=clang-21` のように上書きできます。

> **mainline カーネル特有の落とし穴 (Ubuntu 24.04)** — `linux-headers-$(uname -r)` /
> `linux-tools-$(uname -r)` が archive に無い、ヘッダの Makefile が `gcc-15` を hard-code、
> distro の pahole 1.25 では module BTF が空になる、ヘッダに `vmlinux` が無い、
> `/usr/sbin/bpftool` wrapper が失敗する — の 5 点を `setup-toolchain` が順に解消します
> (詳細は上記 doc の H/C/T 節)。mainline ヘッダの取得は y/N 確認付きで、TTY が無い環境では
> `ASSUME_YES=1 make setup-toolchain` を使ってください。

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

> **6.12 未満でのビルド**: kmod は `kmod/s3fs_ebpf_dev.c` の `#error` で止まります。6.8 などのコンテナでビルド検証だけ回す場合は `make bpf daemon` に限定してください (`make doctor-build` で事前確認できます)。

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

# 2. カーネルモジュールをロードしてマウント (insmod + mount -t s3fs_ebpf、既定は /mnt/s3demo)
make mount

# 3. デーモンをフォアグラウンドで起動 (s3fs-ebpf fastpath --s3、Ctrl+C で停止)
make daemon-run

# 4. 別の端末で触ってみる
ls /mnt/s3demo

# 後片付け (デーモン停止 + umount + rmmod)
make unmount
```

`MOUNT_POINT` / `METRICS_ADDR` は make の変数で、バケットや endpoint は `S3FS_BUCKET` / `S3FS_ENDPOINT` /
`S3FS_ACCESS_KEY` / `S3FS_SECRET_KEY` などの環境変数で上書きできる (`sudo -E` で渡る)。デーモンのオプション
(`--arena-metadata-only` / `--cache-capacity` / `--policy-bpf` など) は
`sudo -E ./bin/s3fs-ebpf fastpath --help` を見る。

> `./s3fs-ebpf.sh mount` と `s3fs-ebpf mount` / `unmount` サブコマンドは初期の骨組みのままで、
> マウントもデーモンの起動もしない。

---

## 単体テスト (`test-kunit` / `test-daemon`)

daemon と root なしで回せるテスト。kmod は KUnit (UML、lockdep と KASAN 付き)、daemon は `go test`。

```bash
make test-daemon
make test-kunit KSRC=/path/to/linux-6.12+   # カーネルのソースツリーが要る
make test-daemon-cov test-kunit-cov KSRC=...  # カバレッジ (coverage/)。CI は public のとき Codecov に送る
```

KUnit の準備、suite の中身、見つかったバグは [`docs/dev-notes/kunit.md`](../dev-notes/kunit.md)。

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
```

計測 script は `scripts/bench-*.sh`。`docs/journal/benchmark-results.md §7-§10` に結果解釈。hit-rate bench (`make bench-hitrate*`) は [`../journal/hit-rate-bench.md`](../journal/hit-rate-bench.md)。

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

ダッシュボードの定義は `deploy/grafana/dashboards/s3fs-ebpf-overview.json`。デーモンの `/metrics` は `:9600` で公開されます。

---

## 片付け

```bash
make unmount        # daemon-stop + umount + rmmod
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
├── policies/        CEL ポリシー (Layer 1)
├── deploy/          docker-compose（MinIO + Prom + Grafana + node_exporter）
├── scripts/         セットアップ・ヘルパスクリプト + bench (bench-latency/write/scale/3way)
├── docs/            設計 / 背景 / 先行研究 (索引は docs/INDEX.md)
├── Makefile         トップレベル
├── README.md        概要
└── s3fs-ebpf.sh     mount / unmount / reload ラッパ
```

