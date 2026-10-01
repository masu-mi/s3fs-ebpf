# Week 1 動作確認で踏んだ環境課題と対策

Ubuntu 24.04 (noble) + Ubuntu mainline カーネル `6.12.90-061290-generic` の組合せで `make test-arena` を通すまでに 5 つの環境課題を踏んだ。再現できるよう課題・原因・対策・副作用を並べる。

対応スクリプト: [`scripts/setup-week1-runtime.sh`](../../scripts/setup-week1-runtime.sh)

---

## 課題一覧

| # | 症状 | 原因 | 対策 | 副作用 |
|---|---|---|---|---|
| 1 | `sudo apt install *` が `E: The package linux-image-unsigned-6.19.14-061914-generic needs to be reinstalled` で全面停止 | 過去のカーネル入替で 3 パッケージが Half-inst / halF-conf / Unpacked の壊れた状態のまま残置 (`iHR`/`iF`/`iU`) | `sudo dpkg --purge --force-remove-reinstreq` で該当 3 パッケージ (`linux-image-unsigned-6.19.14-061914-generic`, `linux-headers-6.19.14-061914-generic`, `linux-headers-6.9.0-060900-generic`) を強制削除 | dpkg status からエントリが消えるが、apt archive にも存在しないため復元は不可。現行 6.12 カーネルには一切影響しない |
| 2 | `make kmod` で `/bin/sh: 1: gcc-15: not found` | kernel header の Makefile が `CC = gcc-15` を hard-code (kernel は gcc-15 でビルドされた)。gcc-15 は noble 標準リポジトリに存在しない | noble 標準の `gcc-14` を install し `make CC=gcc-14 HOSTCC=gcc-14` で override (`-fmin-function-alignment=16` は gcc-14 で追加されたフラグ。gcc-13 では失敗) | `gcc-14` (+ `cpp-14`, `libgcc-14-dev` 依存) が入る |
| 3 | pahole が module BTF を silently 空 (410 B stub) で出力 | noble 標準 `dwarves 1.25` は distilled base BTF を扱えず、kernel 1.30 でビルドされた vmlinux BTF に対して silent no-op になる | `github.com/acmel/dwarves` v1.30 を `libelf-dev` `libdw-dev` `libbpf-dev` `libzstd-dev` `cmake` から `$HOME/.local` にビルド。`PAHOLE=$HOME/.local/bin/pahole LD_LIBRARY_PATH=$HOME/.local/lib` で使う | `$HOME/.local/{bin,lib,share/dwarves}` にファイルが追加される (システム側 `dwarves` 1.25 はそのまま) |
| 4 | kbuild が `Skipping BTF generation for s3fs_kfunc.ko due to unavailability of vmlinux` を出して BTF/`resolve_btfids` パイプラインをスキップ | Ubuntu の linux-headers パッケージには vmlinux ELF が含まれない。kbuild は `[ -f vmlinux ]` で存在確認してから pahole/resolve_btfids を実行する | booted `/boot/vmlinuz-6.12.90-061290-generic` を KDIR 同梱の `scripts/extract-vmlinux` で伸長し、`/usr/src/linux-headers-6.12.90-061290-generic/vmlinux` (66 MB) に配置 | KDIR 直下に `vmlinux` (66 MB) が残る。カーネル入替時は再配置が必要 |
| 5 | ロード時に `s3fs_ebpf: failed to register kfunc set: -22` | 6.11 で `register_btf_kfunc_id_set` に `kset->set->flags & BTF_SET8_KFUNCS` チェックが追加。`BTF_SET8_START` は `flags=0`、`BTF_KFUNCS_START` は `flags=BTF_SET8_KFUNCS` を発行する **別マクロ**。既存コメント (「エイリアス」) は誤り | `kmod/tree_traversal.c` の `BTF_SET8_START/END` → `BTF_KFUNCS_START/END` に変更。旧カーネル向け fallback `#define` を追加 | ソース変更 (repo 内)。副作用なし |

---

## ビルドコマンド

課題 2〜4 の対策込みで:

```bash
export LD_LIBRARY_PATH=$HOME/.local/lib
make CC=gcc-14 HOSTCC=gcc-14 PAHOLE=$HOME/.local/bin/pahole test-arena
```

Makefile へ検出ロジックを組込む・README で案内する等は follow-up。

---

## カーネル選択別の代替案

同じ実装を別カーネルで動かす場合の選び方。

| 環境 | 課題の縮小度 | 手順 |
|---|---|---|
| **Ubuntu mainline 6.12 (今回)** | 5 課題全て | 上記スクリプト |
| **Ubuntu distro kernel (`linux-image-generic` 6.8/6.11)** | 課題 2・3・4 が消える | distro カーネルは noble 同梱の gcc/pahole で整合。`ppa:canonical-kernel-team/*` の HWE を待って 6.11 ベースの `linux-image-generic-hwe-24.04` が入るのが理想。ただし arena は 6.9 で追加、`bpf_addr_space_cast` の JIT 挙動が安定するのは 6.10 以降。**6.11 HWE 版が出るまでは runtime 検証ができない** |
| **Ubuntu 25.04 (plucky) 標準カーネル 6.14** | 課題 2・3・4 が消える | plucky は gcc-14/pahole 1.27 同梱。ただし本レポジトリは noble 前提 (`daemon/go.mod` の Go 版・docker-compose の CE 版とか) なのでフル移行はコスト高 |
| **kernel.org からソースビルド** | 課題 4 が消える (vmlinux ELF が build tree に残るため) | 20 GB ディスク・数十分。CONFIG_DEBUG_INFO=y で BTF 有 vmlinux が生成される |
| **Fedora/CentOS Stream (`kernel-devel` に vmlinux 同梱)** | 課題 3・4 が消える | 6.12 なら Fedora 41、6.14 なら Fedora 42。distro ごとに pahole/gcc の追従が早い |

**判断基準**: この repo で継続開発するなら「Ubuntu 25.04 upgrade」か「distro HWE 待ち」が最も工数対効果が高い。今回のように mainline に飛ぶ場合はこのドキュメントの手順を毎カーネルで踏み直す必要がある。

---

## 副作用の永続性まとめ

再現スクリプト実行後に **システムに残る変更**:

- `/var/lib/dpkg/status` — 破損 3 パッケージ (`linux-*-6.19.14`, `linux-headers-6.9.0-060900`) のエントリが消える
- APT installed set — `gcc-14`, `cpp-14`, `libgcc-14-dev`, `gcc-14-x86-64-linux-gnu`, `libdw-dev` 追加
- `$HOME/.local/{bin,lib,share/dwarves}` — pahole 1.30 と libdwarves.so 群
- `/usr/src/linux-headers-6.12.90-061290-generic/vmlinux` — 66 MB の vmlinux ELF
- `kmod/tree_traversal.c` — `BTF_KFUNCS_START` への差替 (これは repo 変更)

**カーネル入替時に再実行が必要**:
- 課題 2 (gcc バージョン確認 — kernel が新しい gcc でビルドされる可能性)
- 課題 3 (pahole バージョン確認 — kernel が新しい pahole でビルドされる可能性)
- 課題 4 (vmlinux は KDIR 名に紐付くので新カーネルごとに再配置)
