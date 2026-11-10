---
name: dev-env-setup
description: 環境構築と build・実行の切り分け。「make が通らない」「doctor」「MinIO が空」「mount が残った」「rmmod できない」「カーネル入れ替え」「新マシン」で使う。
---

# 環境構築と build / 実行の切り分け

手順の本体は `docs/user/getting-started.md`、依存の理由は `docs/dev-notes/build-env-dependencies.md`、既知の落とし穴は `docs/dev-notes/lessons-learned.md`。

## 最初の 3 コマンド

```bash
make doctor          # kernel / headers / compilers / tools を点検し、直し方を出す (build だけなら make doctor-build)
make toolchain       # 解決された CLANG / KMOD_CC / KMOD_PAHOLE / BPFTOOL / KDIR
make all             # kmod + bpf + daemon
```

## 足りないとき

- 全部入れる: `make setup-toolchain` (`ASSUME_YES=1` で非対話、`DRY_RUN=1 scripts/setup-toolchain.sh` で実行内容だけ確認、`scripts/setup-toolchain.sh clang bpftool ...` で個別)。clang-20、bpftool wrapper、gcc/pahole、go、bench の依存。
- カーネル: **kmod は 6.12 以上が要る** (`kmod/s3fs_ebpf_dev.c` の `#error`)。`make setup-kernel` (mainline 6.12 LTS、Ubuntu、再起動が要る)。入れ替えたら `make setup-kbuild` (headers、gcc/pahole、`$KDIR/vmlinux`)。6.12 未満のコンテナでは `make bpf daemon` だけ。`vmlinux.h` は `make vmlinux` で実行中のカーネルの BTF から作り直す。
- lockdep カーネル: `make setup-kernel-lockdep` (`docs/dev-notes/lockdep-kernel.md`)。ホストが落ちると復旧に時間がかかるので、試す前に `scripts/boot-kernel.sh` の grub 選択を確かめる (`make test-boot-kernel`)。
- bench の依存 (hyperfine、fio、s3fs、`/etc/passwd-s3fs`): `make setup-bench-env`。
- **go build は repo のルートか `daemon/` で行う** (`daemon/cmd/s3fs-ebpf` の中だと 60 MB のバイナリが出て commit に混ざる)。cobra は `@v1.10.2` (memory `project-go-deps-cobra-pitfall`)。
- ビルド警告 `the compiler differs` は無視してよい。`error` は見る。

## 実行環境 (MinIO / mount)

- `make compose-up` (MinIO + Prometheus + Grafana)。**再起動後は止まっている**。バケットが空 / `mc` の alias に認証がない → `docker exec s3fs-ebpf-minio mc alias set local http://localhost:9000 minioadmin minioadmin` と、memory `project-minio-bench-pitfalls` の seed コマンド。
- `make mount` (insmod + mount `/mnt/s3demo`)、`make daemon-run` (別ターミナルで前面実行)、`make daemon-stop` (SIGINT)、`make unmount` (umount + rmmod)。
- **後始末**: 異常終了で mount / kmod が残ったら `make unmount`。`umount -l` → `scripts/kmod-unload.sh s3fs_kfunc`。`Module is in use` / `Resource temporarily unavailable` が出るのは oops 後で、**再起動以外に回復手段がない** (`kmod-change-verify` §4)。ここで時間を使わない。
- 古い kmod が載ったままだと、前セッションの admit 状態で stat が通ってしまう。結果が不自然なら `lsmod | grep s3fs` を見る。
- `pkill -f` は自分のシェルを殺す (`pkill -x` か保存した PID)。daemon の起動は **リポジトリのルートから** (bpf の相対パス)。
- ディスクの空きは 20% 以上 (MinIO が書き込みを拒否する)。

## 環境が整ったあと

- 単体テスト → `kmod-unit-test`、実機の回帰 → `regression-test`、kmod を変えた → `kmod-change-verify`、性能 → `perf-bench-verify`。
