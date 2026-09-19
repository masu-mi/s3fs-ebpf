/* SPDX-License-Identifier: (LGPL-2.1 OR BSD-2-Clause) */
/*
 * s3fs-ebpf: types shared inside the bpf_arena mapping.
 *
 * All three sides (kernel module KFunc, eBPF program, Go daemon)
 * dereference the arena as raw C pointers, so this header MUST use
 * fixed-width types and stay ABI-compatible.
 */

#ifndef _S3FS_EBPF_ARENA_TYPES_H
#define _S3FS_EBPF_ARENA_TYPES_H

/*
 * When built for BPF (-target bpf), types come from vmlinux.h which the
 * translation unit already includes. In kernel builds they come from
 * <linux/types.h>. Only pure userspace / test builds need stdint.
 */
#if !defined(__KERNEL__) && !defined(__bpf__)
#include <stdint.h>
typedef uint64_t __u64;
typedef uint32_t __u32;
#endif

#define S3FS_ARENA_ABI_VERSION 1

/* Placeholder inode entry stored in the arena. */
struct s3fs_inode_stub {
	__u64 ino;
	__u64 size;
	__u64 mtime_ns;
	__u32 mode;
	__u32 _pad;
} __attribute__((packed));

/* Arena root: single fixed header the daemon writes at offset 0. */
struct s3fs_arena_root {
	__u32 abi_version;
	__u32 entry_count;
	__u64 tree_root_off;   /* byte offset into the arena */
} __attribute__((packed));

#endif /* _S3FS_EBPF_ARENA_TYPES_H */
