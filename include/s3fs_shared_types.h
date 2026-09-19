/* SPDX-License-Identifier: (LGPL-2.1 OR BSD-2-Clause) */
/*
 * s3fs-ebpf: type definitions shared across kernel module, eBPF program,
 * and Go daemon.
 *
 * The layout MUST stay stable — Go mirrors these via cgo or hand-rolled
 * binary.Read. Bump S3FS_SHARED_ABI_VERSION on any layout change.
 */

#ifndef _S3FS_EBPF_SHARED_TYPES_H
#define _S3FS_EBPF_SHARED_TYPES_H

#define S3FS_SHARED_ABI_VERSION 1

#define S3FS_MAX_PATH 4096
#define S3FS_MAX_NAME 255

#if defined(__KERNEL__)
#include <linux/types.h>
#elif defined(__bpf__) || defined(__BPF__)
/* Types come from vmlinux.h in the BPF translation unit. */
#else
#include <stdint.h>
typedef uint64_t __u64;
typedef uint32_t __u32;
typedef uint16_t __u16;
#endif

/* Mirror of struct stat we cache in arena. */
struct s3fs_stat {
	__u64 ino;
	__u64 size;
	__u64 blocks;
	__u64 atime_ns;
	__u64 mtime_ns;
	__u64 ctime_ns;
	__u32 mode;
	__u32 nlink;
	__u32 uid;
	__u32 gid;
};

/* Slow-path event pushed via BPF_MAP_TYPE_RINGBUF to the Go daemon. */
struct s3fs_miss_event {
	__u64 ino;
	__u64 offset;
	__u32 length;
	__u32 op;               /* 0=stat 1=read 2=lookup */
	char  path[S3FS_MAX_PATH];
};

/*
 * Context passed via BPF_PROG_TEST_RUN to the SEC("syscall") test harness
 * added in Week 1. The daemon fills `idx` and `kfunc_arg`; the BPF program
 * writes `out_ino` (arena[idx].ino) and `out_kfunc` (s3fs_kfunc_echo result).
 */
struct s3fs_test_args {
	__u32 idx;
	__u32 kfunc_arg;
	__u64 out_ino;
	__u32 out_kfunc;
	__u32 _pad;
};

#endif /* _S3FS_EBPF_SHARED_TYPES_H */
