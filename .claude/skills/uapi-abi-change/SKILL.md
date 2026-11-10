---
name: uapi-abi-change
description: uapi.h の構造体・ioctl を変えるときのチェックリスト (kmod と daemon の同期、sizeof 期待値)。「uapi を変えた」「ioctl を足す」で使う。
---

# uapi / ABI の変更

kmod と daemon は `include/s3fs_ebpf_uapi.h` の構造体と ioctl 番号で話す。**片方だけ変えると、daemon が kmod の知らない ioctl を送る / 構造体の大きさが食い違って黙って壊れる。古い daemon と新しい kmod の組み合わせは動かない。**

## チェックリスト

1. **header を変える** (`include/s3fs_ebpf_uapi.h`): 構造体にフィールドを足す、または `_IOW/_IOR/_IOWR(S3FS_IOC_MAGIC, <番号>, struct ...)` を足す。番号は既存と重ねない。サイズが変わる変更はコメントに ABI note を残す (G-01 の例が header 内にある)。構造体は 8 byte 境界、padding を明示。
2. **kmod 側**: ioctl のハンドラ (`kmod/s3fs_ebpf_dev.c`) と、使う `kmod/vfs/*.inc.c` を更新する。関数は `scripts/fn.sh <func>`。
3. **daemon 側**: `daemon/internal/kmodio` の Go 構造体と ioctl 定数を同じレイアウトで更新する。
4. **期待値を更新**: `daemon/internal/kmodio/kmodio_test.go` の `TestStructSizesMatchUAPI` と ioctl 番号の表。**値は header を gcc でコンパイルして `sizeof` と `S3FS_IOC_*` を出して確かめる** (手計算で済ませない。`make test-daemon`)。
5. **同時に build して試す**: `make all` (kmod + bpf + daemon)、古い module を `rmmod` してから載せる。`make test-daemon`、新しい ioctl には KUnit の呼び出しテスト (`kmod-unit-test`: ioctl の中身の関数を直接呼べる) と、実機の回帰 (`regression-test`、境界値は `test-populate-boundary` が手本)。
6. **kmod の不変条件に触れるなら** `docs/kmod-pcache-invariants.md` も同じ変更で更新する (CLAUDE.md)。
7. 実機で試す前後の手順 (dmesg、oops の復旧) は `kmod-change-verify`。記録は `journal-record`。

## 境界値で確かめるもの

size 0、オーバーフロー、上限超え (`test-populate-boundary` の size=0 / overflow / >64 MiB)、未知の ioctl 番号、`copy_from_user` の失敗。
