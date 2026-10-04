# ビルド・検証環境の依存箇所と解消方法

Ubuntu 24.04 (noble) + mainline 6.12 と Debian の 2 環境で `make all` / `make bench` を通すまでに踏んだトラブルを、**どこに依存しているか** (カーネルバージョン / ヘッダ / コンパイラ / ツール / テストツール) で整理し直したもの。各項目の ID (`K-1` など) は `make doctor` の出力と一致する。

```bash
make doctor            # 何が足りないか + 直し方を表示 (NG があれば exit 1)
make setup-toolchain   # 全ステップ (下記 §3)
make setup-kbuild      # カーネル入替後: headers + gcc/pahole/vmlinux だけ再実行
make toolchain         # Makefile が実際に使う CLANG / KMOD_CC / ... を表示
```

`make all` / `make kmod` / `make bpf` はビルド前に同じ検査を対象を絞って実行し (`doctor.sh --quiet --build` / `--only kmod|bpf`)、NG があればコンパイル前に直し方と `fix all: scripts/setup-toolchain.sh <steps>` を出して止まる。`PREFLIGHT=0` で無効化できる。さらに `make kmod` はビルド後に `scripts/check-kmod-btf.sh` で `.ko` の `.BTF` サイズを確認する — kbuild は vmlinux が無いと BTF 生成を **スキップしたまま exit 0** するため、以前は `make all` が成功して insmod で初めて落ちていた (H-2 / C-4)。外部モジュールの kbuild は vmlinux を `.ko` の依存に含めないため、**vmlinux / pahole を直す前にビルドした BTF 無し `.ko` は直した後も「最新」扱いのまま残る**。BTF 検証で失敗したら `.ko` を消して 1 回だけ再リンクするので、環境が直っていればそのまま通る。

関連: 初回踏破の記録 [`journal/week1-build-runtime-notes.md`](../journal/week1-build-runtime-notes.md)、bench 環境の修正記録 [`journal/bench-env-notes.md`](../journal/bench-env-notes.md)。

---

## 1. 依存箇所の一覧

### K: カーネルバージョン

| ID | 要件 | 満たさないと | 解消 | 由来 |
|---|---|---|---|---|
| K-1 | **6.12 以上** | `kmod/s3fs_ebpf_dev.c` の `#error` でビルド不可 (struct bpf_arena のレイアウト写し)。`BTF_KFUNCS_START` も 6.11+ | Ubuntu: `make setup-kernel` (mainline 6.12) → reboot。Debian: 6.12 系カーネル (trixie 標準、bookworm は backports 等) | week1 課題 5、`s3fs_ebpf_dev.c` |
| K-2 | 6.12 系列 (推奨) | 6.13+ でもビルドは通るが struct bpf_arena の写しが未検証。arena 読み出しが黙って壊れる可能性 | `make test-arena` で round-trip を確認してから使う | `s3fs_ebpf_dev.c` |
| K-3 | `CONFIG_DEBUG_INFO_BTF=y` (`/sys/kernel/btf/vmlinux`) | `bpf/vmlinux.h` が生成できない | BTF 付きカーネルで起動 (Ubuntu/Debian 標準カーネルは全て有効) | `gen-vmlinux.sh` |

> README に「`make all` は 6.8 でも通る」とあったが、K-1 の `#error` が入ってからは kmod が通らない。6.8 でのビルド検証は `make bpf daemon` に限られる。

### H: カーネルヘッダ (KDIR)

| ID | 要件 | 症状 | 原因 | 解消 |
|---|---|---|---|---|
| H-1 | `/lib/modules/$(uname -r)/build` | `make kmod` が即失敗 | mainline カーネルには apt の `linux-headers-<ver>` が無い | `setup-toolchain.sh headers`: apt に候補があれば apt、mainline なら kernel.ubuntu.com の **index を読んで** `_all.deb` + `-generic_<arch>.deb` を取得 (y/N 確認付き) |
| H-2 | `$KDIR/vmlinux` (ELF) | `Skipping BTF generation ... unavailability of vmlinux` → insmod 時に kfunc set 登録失敗 | distro のヘッダパッケージは vmlinux を含まない | `setup-toolchain.sh kbuild`: `/boot/vmlinuz-*` を `extract-vmlinux` で伸長して配置。**カーネルごとに再実行** |
| H-3 | `$KDIR/.config` 読める | gcc / pahole のバージョン突き合わせが推測になる | — | ヘッダを入れ直す (`/boot/config-*` も参照する) |

ヘッダ取得で踏んだ細部 (いずれも `scripts/lib/mainline.sh` / `setup-toolchain.sh` に吸収済):

- `apt-cache show linux-tools-<mainline>` は他パッケージの Depends に名前が出るだけで 0 を返す → `apt-cache policy` の `Candidate:` で判定 (`apt_has_candidate`)。
- mainline の URL は `6.12.0-061200` → `v6.12`、`6.12.5-061205` → `v6.12.5` (末尾 `.0` を落とす)。ファイル名のビルドタイムスタンプは推測せず index から拾う。
- 旧 `setup-kernel-6.12.sh` は `*generic*` だけを取っていたため `linux-headers-<ver>_..._all.deb` が漏れ、generic ヘッダの dpkg が依存未解決で中途半端に残る (week1 課題 1 の壊れた dpkg 状態と同じ形)。`_all` も取るように修正。

### C: コンパイラ

| ID | 要件 | 症状 | 原因 | 解消 |
|---|---|---|---|---|
| C-1 | **clang 19+** (BPF) | clang-18 だと **エラーにならず** `arena_types.h` が `__arena` を素のポインタにフォールバックしたオブジェクトができる | `-mcpu=v3` の `__BPF_FEATURE_ADDR_SPACE_CAST` は clang 19+。noble 標準は 18 | 自動検出 (`clang-30`..`clang-19` → `clang`)。noble は `clang-20` (noble-updates)、bookworm は apt.llvm.org。`bpf/Makefile` に `check-clang` ガードを追加。**コミット済みの bpf2go 生成物 (`daemon/internal/ebpf/s3fsfp_bpf{el,eb}.o`) も同じ罠を踏んでいた** (cast 0 個 → `make test-arena` で `R2 invalid mem access 'scalar'`)。`make daemon-gen` にも preflight を付け、`arena_cast_test.go` で埋め込み object に `bpf_addr_space_cast` があることを `go test` で検査 |
| C-2 | `llvm-strip-<N>` | `make daemon-gen` (bpf2go) のみ失敗 | bpf2go は `-cc clang-20` から `llvm-strip-20` を導出する。フルパスを渡すと `llvm-strip` になる | `llvm-<N>` を一緒に入れる。CLANG はコマンド名 (`clang-20`) で渡す |
| C-3 | kmod 用 gcc = カーネルをビルドした gcc の major (最低 14)。major が一致していれば kbuild の `warning: the compiler differs from the one used to build the kernel` (パッケージ版の文字列差) は無害。gcc-15 カーネルを gcc-14 でビルドするとヘッダの `__counted_by` で `attribute directive ignored` が大量に出る (境界チェック用の注釈でレイアウトは不変) ため、その組合せのときだけ `KCFLAGS=-Wno-attributes` を自動付与 (`KMOD_KCFLAGS=` で上書き可) | `gcc-15: not found` / gcc-13 で `-fmin-function-alignment` 不明 | ヘッダの Makefile がビルド時のコンパイラ名を hard-code。mainline 6.12 は gcc-15、noble は gcc-14 まで | `CONFIG_GCC_VERSION` から major を読み、`gcc-<major>` → 同 major の `gcc` → `gcc-14` 以上の最新、の順に選んで `CC=`/`HOSTCC=` で渡す |
| C-4 | pahole ≥ `CONFIG_PAHOLE_VERSION` | module BTF が **黙って** 空 (410 B) → insmod で kfunc 登録失敗 | noble の dwarves 1.25 は 1.30 でビルドされたカーネルの distilled base BTF を扱えない | distro pahole で足りればそれ、足りなければ `$HOME/.local` にソースビルド。`LD_LIBRARY_PATH` は **ローカル pahole を使う時だけ** 付く |

### T: ツール

| ID | 要件 | 症状 | 原因 | 解消 |
|---|---|---|---|---|
| T-1 | 実行できる bpftool | `bpftool: not found` / `WARNING: bpftool not found for kernel ...` | Ubuntu の `/usr/sbin/bpftool` は `uname -r` で `linux-tools-<ver>` に dispatch する wrapper (mainline に該当パッケージ無し)。Debian は `/usr/sbin` で一般ユーザーの PATH 外 | `detect_bpftool` が PATH → `/usr/sbin` → `/usr/lib/linux-tools/*/bpftool` の順に「実際に動く」ものを探す。BTF dump はカーネル版非依存 |
| T-2 | Go ≥ `daemon/go.mod` の `go` 行 (現在 1.25.0) | 古い Go だと GOTOOLCHAIN の自動 DL 頼み (オフラインで失敗) | 旧 `setup-toolchain.sh` は 1.22.5 を hard-code していた | `setup-toolchain.sh go` が go.mod の版を `/usr/local/go` に入れる。ただし apt の golang・`~/go`・mise/asdf などの古い `go` が PATH で先に見つかると、入れても `go` は古いまま。そこで Makefile は `detect_go` (PATH → `/usr/local/go` → `~/sdk/go*` → `/usr/lib/go-1.*` から要件を満たすもの) を `GO` として使い、doctor は PATH 側の古い go を T-3 で指摘する |
| T-4 | 環境変数 `GOROOT` を残さない | `compile: version "go1.24.5" does not match go tool version "go1.25.0"` が全パッケージで出る | 古い go を手で入れた時の `export GOROOT=...` が残っていると、新しい go が古い GOROOT のコンパイラを使う | Makefile は選んだ go 自身の GOROOT (`go_root`) を `GOROOT=` で渡す。対話シェル用には rc から `export GOROOT` を消す (現行の go は GOROOT 不要) |

### B: テストツール / ベンチ実行環境

| ID | 要件 | 症状 | 解消 |
|---|---|---|---|
| B-1 | hyperfine / fio / s3fs / python3 / curl | `missing ...` | `make setup-bench-env` (= `setup-toolchain.sh bench`) |
| B-2 | `docker compose` (v2 plugin) | `compose-up` 失敗 | Ubuntu: `docker-compose-v2`、Debian trixie: `docker-compose`、Docker 公式 repo: `docker-compose-plugin` の順に apt 候補を選ぶ。docker-ce 導入済みなら `docker.io` は入れない (競合) |
| B-3 | `/etc/passwd-s3fs` | s3fs-fuse の A/B mount 失敗 | `setup-toolchain.sh bench` が MinIO の開発用 creds で作成 (0600) |
| B-4 | MinIO コンテナ + seed 済オブジェクト | `... missing in MinIO` | `make compose-up` (`deploy/minio/init.sh` が seed) |

ベンチスクリプト側で踏んだもの (`scripts/lib/bench.sh` に集約):

- **MinIO の存在確認**: コンテナ内 `mc` は creds 無しだと匿名アクセスになり、`mc ls` は存在しないオブジェクトでも exit 0。alias を設定して `mc stat` で確認する (`minio_require`)。`bench-parallel.sh` だけ `mc ls` のまま残っていたのも統一。
- **pkill の自己マッチ**: `sudo pkill -f 's3fs-ebpf fastpath'` は sudo/pkill 自身の argv にもマッチし、SIGKILL で自爆 → bash の `Killed` 通知 (`\r` 付き) が出力を壊す。`fast[p]ath` パターンに統一 (`stress-shutdown.sh` は旧パターンのままだった)。
- **停止順序**: SIGINT 後に固定 `sleep 1` で umount していた。daemon の flush drain が終わるまで待ってから umount/rmmod (`ebpf_stop`)。busy 時は lazy umount (`umount_hard`)。
- **`LD_LIBRARY_PATH=$HOME/.local/lib sudo -E daemon`**: sudo は `LD_*` を常に除去するので無意味だった (daemon は pure Go)。削除。
- **daemon 起動待ち**: 40〜80 回ループの寄せ集めだったのを `DAEMON_READY_TIMEOUT` (秒) に統一し、タイムアウト時は警告を出す。

---

## 2. 構成: 判定ロジックを 1 箇所に

```
scripts/lib/common.sh      log / run (DRY_RUN) / confirm (ASSUME_YES) / apt_has_candidate / ver_ge
scripts/lib/toolchain.sh   要件定数 + detect_clang / detect_kmod_cc / detect_pahole / detect_bpftool / kconfig
scripts/lib/mainline.sh    kernel.ubuntu.com/mainline の index 解析と取得
scripts/lib/bench.sh       ベンチ共通: ebpf_mount / ebpf_start_daemon / ebpf_stop / fuse_mount* / minio_require
scripts/lib/umount.sh      umount_hard (既存)

scripts/detect-toolchain.sh  --make → Makefile が VAR?=value として取り込む
scripts/doctor.sh            K/H/C/T/B を検査して fix を表示
scripts/setup-toolchain.sh   doctor が NG にする項目をステップ単位で解消
```

- 以前は「どの clang/gcc/pahole を使うか」が Makefile (`command -v clang-20 || ...`)、`setup-week1-runtime.sh`、`gen-vmlinux.sh`、README の手順に別々に書かれていた。現在は `toolchain.sh` だけが規則を持ち、Makefile / doctor / setup が同じ関数を呼ぶ。
- 全て `?=` / 環境変数で上書き可能: `make CLANG=clang-21 KMOD_CC=gcc-14 KDIR=/path`。
- 付随して見つかった Makefile の不具合: トップレベルで検出した `CLANG` が `bpf/` の sub-make に渡っておらず、`make bpf` は常に `/usr/bin/clang` (noble では 18) を使っていた。C-1 のサイレントフォールバックと組み合わさりエラーにならなかった。

---

## 3. setup-toolchain.sh のステップ

| step | 内容 | doctor ID | 再実行タイミング |
|---|---|---|---|
| `packages` | build-essential, llvm, libbpf-dev, libelf-dev, docker + compose | B-2 | 初回 |
| `clang` | clang ≥ 19 + `llvm-<N>` | C-1, C-2 | 初回 |
| `bpftool` | 動く bpftool を用意 | T-1 | 初回 / Ubuntu でカーネル入替時 |
| `headers` | KDIR を用意 | H-1 | **カーネル入替時** |
| `kbuild` | 壊れた dpkg の修復、gcc / pahole の整合、`$KDIR/vmlinux` | C-3, C-4, H-2 | **カーネル入替時** |
| `go` | go.mod の版 | T-2 | go.mod 更新時 |
| `bench` | hyperfine fio s3fs + `/etc/passwd-s3fs` | B-1, B-3 | 初回 |
| `docker-group` | `$USER` を docker group へ | — | 初回 |

`DRY_RUN=1` で実行内容だけ表示、`ASSUME_YES=1` で y/N を自動承認 (TTY 無しでは既定で「no」扱いになり、システムを黙って変更しない)。旧 `S3FS_INSTALL_MAINLINE_HEADERS=1` も `ASSUME_YES=1` として受け付ける。`scripts/setup-week1-runtime.sh` は `setup-toolchain.sh kbuild` へのラッパーとして残した。

---

## 4. 環境別メモ

| 環境 | 主に効く項目 | 手順 |
|---|---|---|
| **Ubuntu 24.04 + mainline 6.12** | K-1 (要入替), H-1, H-2, C-1, C-3 (gcc-15→14), C-4 (pahole 1.25→ソース), T-1 | `make setup-kernel` → reboot → `make setup-toolchain` → `make doctor` |
| **Ubuntu 24.04 標準 6.8** | K-1 で kmod 不可 | ビルド検証のみ `make bpf daemon` |
| **Debian trixie (6.12)** | H-2, B-2 (`docker-compose`), T-1 (`/usr/sbin`) — 標準パッケージの版が新しいので C-1/C-3/C-4 は概ね apt で揃う | `make setup-toolchain` → `make doctor` |
| **Debian bookworm** | K-1 (標準 6.1)、C-1 (clang は 16 まで → apt.llvm.org)、C-4、B-2 (標準 `docker-compose` は v1) | 6.12+ カーネルを用意してから `make setup-toolchain`、残りは doctor の指示に従う |

どの環境でも最後は `make doctor` が NG 0 になることを確認してから `make all` する。

---

## 5. 検証記録

| 環境 | `make doctor` | `make all` | `make test-arena` | `make bench` |
|---|---|---|---|---|
| dev1: Ubuntu 24.04 + mainline `6.12.0-061200` (gcc-14 ビルド) | NG 0 | ✅ | ✅ A/B/C PASS | ✅ (cat big.bin 6.7× / 10m.bin 7.7× / 100m.bin 11.3× vs s3fs-fuse) |
| Ubuntu 24.04 + mainline `6.12.90-061290` (gcc-15 ビルド, kmod は gcc-14) | NG 0 (C-3 WARN) | ✅ | — | — |

この検証で新たに見つかって直したもの (§1 の各行にも反映):

- 環境を直す前にビルドした BTF 無し `.ko` が「最新」扱いで残る → BTF 検証失敗時に 1 回だけ再リンク
- `export GOROOT` の残骸で新しい go が古いコンパイラを呼ぶ (T-4)
- `s3fs-ebpf dev` 配下へ移った `test-arena` / `microbench` を旧名で呼んでいた (Makefile / bench)
- コミット済み bpf2go object が clang < 19 生成で `addr_space_cast` 無し → verifier 拒否 (C-1、`arena_cast_test.go`)
- `rmmod` が BPF prog の module 参照解放 (非同期) と競合して "in use" → `scripts/kmod-unload.sh` でリトライ
