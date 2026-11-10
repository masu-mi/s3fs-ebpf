---
name: kmod-unit-test
description: root・実機なしの単体テスト。KUnit (UML/qemu)・カバレッジ・go test・check-invariants を回し、KUnit を足す。「KUnit」「単体テスト」「カバレッジ」で使う。
---

# 単体テスト (kmod KUnit / daemon go test / 静的検査)

実機の回帰試験 (`regression-test` skill、約 25 分、root、MinIO が要る) より先に、数秒〜数分で境界を確かめる層。詳細は `docs/dev-notes/kunit.md` (suite の一覧、見つけたバグ、カバレッジの仕組み)。

## 回す

| 目的 | コマンド | 前提 |
|---|---|---|
| kmod の KUnit (UML、lockdep + KASAN 付き、約 80 秒) | `make test-kunit KSRC=<Linux 6.12 以上のソースツリー>` | `flex bison bc`。distro の headers では足りない。tarball は https://cdn.kernel.org/pub/linux/kernel/v6.x/ (sha256 を照合) |
| suite を絞る | `... KUNIT_ARGS="'s3fs_ebpf_vfs*'"` | suite: `s3fs_ebpf_chunks` / `_tables` / `_vfs` / `_policy_ops` |
| 本当の並列 (SMP) | `... KUNIT_ARGS="--arch=x86_64 --qemu_args='-smp 4'"` | qemu-system-x86。KVM がないと遅い |
| kmod のカバレッジ | `make test-kunit-cov KSRC=...` → `coverage/kmod.lcov` | clang / lld / llvm / libclang-rt。KASAN なし (UML の制約) |
| daemon の単体 | `make test-daemon` (`cd daemon && go test -race ./...`) | `internal/s3` は約 20 秒。**測定中は流さない** (`perf-bench-verify` §0) |
| daemon のカバレッジ | `make test-daemon-cov` → `coverage/daemon.out` | |
| kmod の静的検査 (root 不要) | `make check-invariants` | `vfs_ops.c` + `vfs/*.inc.c` と bench の seed を grep ベースで検査 |
| v2 spec モデル (python3 のみ) | `make spec-check` | `docs/design/v2-spec` |
| TLA+ モデル | `make test-spec` → `tlc-design-verification` skill | 使い捨てコンテナ、ネットワーク |
| WTinyLFU の pick を verifier + Go model で | `make test-wtinylfu-harness` | root。kmod / mount は不要 |

実カーネルに suite 入りの module を載せる場合 (`make -C kmod KDIR=... KUNIT=1`) は、mount 中の環境では使わない (global な表を触る)。

## KUnit のテストを足す

1. **足すべき場面**: kmod の新しいデータ構造、分岐、境界 (tombstone の消去条件、chunk 表の hole、pending の key、ロック順)。実機の回帰は粗いので境界は KUnit に書く。
2. テストは対象 `.c` の末尾で `#include` する (`#ifdef S3FS_KUNIT`)。static 関数をそのまま呼べる。ファイル: `kmod/vfs_ops_kunit.c` (chunks / tables / vfs)、`kmod/policy_struct_ops_kunit.c` (policy_ops)。`vfs_ops.c` 自体は全文 Read しない (`scripts/fn.sh <func>`、CLAUDE.md)。
3. user memory が要る呼び出し (`.write`、`POPULATE_PAGES`、`READ_DIRTY`) は `kunit_vm_mmap` の領域を使い、daemon の代わりに ioctl の中身の関数を直接呼ぶ。
4. **変異で確かめる**: 足したテストは、修正前 / 条件を壊したコードで FAIL することを確かめる (通るだけのテストは何も守らない)。
5. 並行テストは UML (1 CPU、プリエンプションの交互実行) では弱い。ロックや race を触るなら SMP の qemu でも回す。
6. 不変条件を変えたら `docs/kmod-pcache-invariants.md` も同じ変更で更新する (CLAUDE.md)。

## 詰まったとき

- 新しい ioctl / ABI (`include/s3fs_ebpf_uapi.h`) を変えたら `daemon/internal/kmodio` と `kmodio_test.go` の `sizeof` / ioctl 番号の期待値も更新する (`kmod-change-verify` §3)。
- `pahole` が壊れていても `kunit.sh` が無視する。build の環境が分からなければ `dev-env-setup` skill。
- KUnit が通っても実機の oops は防げない。kmod を変えたら `kmod-change-verify` の手順 (dmesg、実機の回帰) へ進む。
