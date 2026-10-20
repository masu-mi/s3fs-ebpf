# kmod の KUnit テスト

daemon も root も使わずに kmod (`kmod/*.c`) を検査する単体テスト。`make test-regression` 系 (実カーネルに insmod して daemon と S3 を動かす) より前に、データ構造と VFS の振る舞いを数秒で確かめる。

## 実行

```bash
# Linux >= 6.12 のソースツリーが要る (distro の headers では足りない)。
# Ubuntu 24.04 なら deb-src を有効にして:
apt-get source --download-only linux-hwe-6.14 && tar xzf linux-hwe-6.14_6.14.0.orig.tar.gz

# カーネルの build に flex / bison / bc が要る
sudo apt install build-essential flex bison bc
# pahole は無くてよい (壊れた pahole は kunit.sh が無視する)

make test-kunit KSRC=$PWD/linux-6.14                  # UML (既定)
make test-kunit KSRC=... KUNIT_ARGS="'s3fs_ebpf_vfs*'"  # suite を絞る
make test-kunit KSRC=... KUNIT_ARGS="--arch=x86_64 --qemu_args='-smp 4'"  # qemu (SMP)
```

- `scripts/kunit.sh` が `kmod/*.[ch]` を `$KSRC/fs/s3fs_ebpf_kunit/` に symlink し、`-DS3FS_KUNIT` 付きで kernel に組み込んで (`obj-y`) `tools/testing/kunit/kunit.py run` を呼ぶ。UML には root fs がなく insmod できないので組み込みにしている。`$KSRC/fs/Makefile` に 1 行足す以外は tree を変えない。出力は `$KSRC/.kunit-s3fs-<arch>/`。
- `kmod/.kunitconfig` で lockdep (`PROVE_LOCKING`)、`DEBUG_ATOMIC_SLEEP`、`DEBUG_LIST` を、`kmod/kunit-kasan.config` で KASAN を有効にしている。assert していない lock 順の逆転や use-after-free もテストの失敗として出る。UML 用の追加は `kmod/kunit-uml.config`。
- UML は 1 CPU なので、並行テストはプリエンプションでの交互実行にとどまる。本当の並列は `--arch=x86_64 --qemu_args='-smp 4'` (qemu-system-x86 が要る。KVM がないと遅い)。
- 実カーネルで回すなら `make -C kmod KDIR=... KUNIT=1` で suite 入りの module を作り、`CONFIG_KUNIT` のある kernel に insmod する (結果は dmesg)。suite は global な表 (admit / pending / queue) を触るので、mount 中の環境では使わない。

## カバレッジ (Codecov)

```bash
sudo apt install clang lld llvm libclang-rt-18-dev
make test-kunit-cov KSRC=...   # -> coverage/kmod.lcov (kmod/*.c、テストファイルは除く)
make test-daemon-cov           # -> coverage/daemon.out
```

- kmod は clang の source-based coverage (`-fprofile-instr-generate`) で取る。gcc の gcov は UML に link できない (gcc 13 以降の libgcov が `mangle_path` を定義していて `fs/seq_file.c` と衝突する)。
- profile の計装は dynamic link の UML が要り、UML の KASAN は `STATIC_LINK` が要るので、カバレッジ用の build (`COVERAGE=1 scripts/kunit.sh`、出力は `$KSRC/.kunit-s3fs-um-cov/`) は KASAN なし。KASAN 付きの検査は通常の `make test-kunit` で行う。
- `.github/workflows/test.yml` が push / PR ごとに両方を走らせ、リポジトリが public のときだけ Codecov に送る (`CODECOV_TOKEN` secret を設定する)。設定は `codecov.yml` (flag `daemon` / `kmod`、PR を落とさない informational)。

## 構成

テストは対象の `.c` の末尾で `#include` する (`#ifdef S3FS_KUNIT`)。static 関数をそのまま呼べるようにするためで、本番 build には入らない。

| suite | ファイル | 対象 |
|---|---|---|
| `s3fs_ebpf_chunks` | `kmod/vfs_ops_kunit.c` | chunk 表 (AC-45: 確保サイズ、倍々、hole の 0、truncate、M1 の `fd->cap` と `s3fs_fd_buf_bytes`、pin)、populate の `populated` 判定 (T5)、modify op (AC-50/51: `s3fs_mop_*`、mpin の対称性、remote) |
| `s3fs_ebpf_tables` | 同上 | dyn entry index (AC-55)、admit 表 (P1/P3/S4、per-file cache)、pending (U13/P1/H-06、C2 の verify)、miss / flush / evict queue (F1)、pending の並行 get/put/signal |
| `s3fs_ebpf_vfs` | 同上 | `kern_mount` した fs を VFS 経由で操作: bootstrap の read、write 後の page cache、truncate、close の flush event と W1 pin、fsync と古い PUT (W3)、再 mount、2 GiB を超える read、write / read / truncate の並行実行 |
| `s3fs_ebpf_policy_ops` | `kmod/policy_struct_ops_kunit.c` | struct_ops vtable の dispatch、欠けた member の既定値、reg / unreg / update の hot reload 規則 |

user memory が要る呼び出し (`.write`、`POPULATE_PAGES`、`READ_DIRTY`) は `kunit_vm_mmap` の領域を使う。daemon の代わりに、テストが ioctl の中身の関数 (`s3fs_ebpf_populate_pages`、`s3fs_ebpf_flush_pop`、`s3fs_ebpf_signal_flush` など) を直接呼ぶ。

## このテストで見つけて直したもの

修正前のコードでは、下の 1〜5 に対応するテストが UML で 7 件、4 CPU の qemu では並行テストを加えて 8 件失敗する (6 は daemon 側の修正で、kmod が cap 0 の `READ_DIRTY` を受け付けることをテストしている)。

1. **2 GiB を超える read が EOF になる。** `fill_super` が `s_maxbytes` を設定せず、`alloc_super` の既定 (`MAX_NON_LFS` = 2 GiB - 1) のままだった。pcache モードの read (`generic_file_read_iter`) は 2 GiB で 0 を返し、`lseek` も -EINVAL になる。`MAX_LFS_FILESIZE` にした (chunk 表の上限 1 TiB を超える位置は -EFBIG / -EIO で失敗する)。
2. **chunk の途中から始まる populate が chunk 全体を populated にする。** 最後の chunk に限り、範囲の開始位置を見ずに `populated` を立てていたので、取得していない前半を 0 として返した (T5)。daemon は chunk 境界から populate するので通常の経路では起きないが、ioctl としては受け付けていた。
3. **fsync が、自分のデータを含まない PUT の完了で返る。** close(2) の PUT が read_dirty した後に write + fsync すると、その PUT の `SIGNAL_FLUSH` が fsync の waiter を 0 で起こしていた (W1 の seq 判定は pin にしか使われていなかった)。W3 として waiter にも同じ判定を使う。
4. **rmmod しない再 mount で fsync queue が閉じたまま。** `put_super` が立てる `s3fs_flush_wait_shutdown` を戻す箇所がなく、次の mount で `POP_FLUSH_WAIT` が即 -ENODEV を返し、daemon の fsync loop が終了していた。
5. **truncate と競合した read が -EIO になる。** read が i_size を見た後に truncate が縮めると、`.read_folio` は新しい終端より後ろの folio を「データなし」として失敗させ、read(2) が -EIO を返した (4 CPU の qemu で、write / read / truncate を並行させるテストで発生)。終端より後ろの folio は 0 で埋めて成功させる (`.read_folio` の先頭で i_size を確認し、dirty な fd では `fd->size` より後ろも 0)。
6. **空のファイルの flush が完了しない (daemon)。** `handleFlush` は `ev.Size == 0` のとき `READ_DIRTY` を呼ばなかったので、kmod から見るとその PUT は flush を覆わず、fd は flushing と W1 の pin を持ったままだった (`: > f` の後の read は populate を -EBUSY で断られる)。3 の修正後は、空のファイルの fsync が 30 秒待って -EIO になる。daemon は常に `READ_DIRTY` を呼ぶ (cap 0 で可)。

build の問題 (テストなし):

7. 6.13 以降で build できない。`MODULE_IMPORT_NS(BPF_INTERNAL)` は 6.13 から文字列リテラルを取る。
8. `vfs_ops.c` が `copy_to_iter` / `iov_iter_count` の `<linux/uio.h>` を直接 include していなかった (UML の build で露見)。

## 直していない気になる点

- W3 の残る窓: fsync の WAIT event の read_dirty の後に古い PUT の signal が届くと、seq が一致して fsync を起こす。`SIGNAL_FLUSH` に seq を持たせれば閉じられる (uapi と daemon の変更)。
- `S3FS_IOC_STATS` で `count == 13` のとき、slot 12 は 0 のまま `filled = 13` を返す (12 と 13 は 1 回の走査で両方埋めるため)。
- write が `s3fs_fd_store` の途中で失敗 (-EFAULT / -EFBIG) すると、`s3fs_fd_mark_remote_locked` がつけた remote と途中まで書いた chunk が clean な fd に残る。-EFBIG は書く前に判定できる。
