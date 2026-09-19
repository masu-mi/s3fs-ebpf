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

/*
 * Address-space tag for arena pointers. Mirrors the pattern used in
 * tools/testing/selftests/bpf/bpf_arena_common.h. Compilers that support
 * __BPF_FEATURE_ADDR_SPACE_CAST emit `bpf_addr_space_cast` transparently
 * on dereference and pointer assignment; older toolchains fall back to
 * plain pointers (which the JIT still validates via the verifier).
 */
#if defined(__BPF__) && defined(__BPF_FEATURE_ADDR_SPACE_CAST)
# define __arena          __attribute__((address_space(1)))
# define __arena_global   __attribute__((address_space(1)))
# define cast_kern(ptr)  /* LLVM nop */
# define cast_user(ptr)  /* LLVM nop */
#elif defined(__BPF__)
# define __arena
# define __arena_global   SEC(".addr_space.1")
# define cast_kern(ptr) ((void)(ptr = bpf_addr_space_cast(ptr, 0, 1)))
# define cast_user(ptr) ((void)(ptr = bpf_addr_space_cast(ptr, 1, 0)))
#else
# define __arena
# define __arena_global
# define cast_kern(ptr) ((void)(ptr))
# define cast_user(ptr) ((void)(ptr))
#endif

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
