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

/* Magic identifying an s3fs_ebpf super_block. Set by the kmod's fill_super. */
#define S3FS_EBPF_MAGIC 0x53334642UL  /* "S3FB" */

/*
 * Context passed via BPF_PROG_TEST_RUN to the SEC("syscall") test harness.
 * Week 1 laid this out for an mmap-array; Week 2 promotes it to a real
 * bpf_arena: the daemon mmaps the arena, writes stubs at byte offsets, and
 * passes `arena_base_uaddr` = mmap base so the BPF program can reconstruct
 * arena pointers (`__arena *`) via a scalar-to-address_space cast.
 *
 * Layout is padded to 8-byte boundaries; `_pad` keeps sizeof stable at 32.
 */
struct s3fs_test_args {
	__u64 arena_base_uaddr;
	__u32 idx;
	__u32 kfunc_arg;
	__u64 out_ino;
	__u32 out_kfunc;
	__u32 _pad;
};

#endif /* _S3FS_EBPF_SHARED_TYPES_H */
