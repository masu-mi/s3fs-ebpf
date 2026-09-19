// SPDX-License-Identifier: (LGPL-2.1 OR BSD-2-Clause)
/*
 * s3fs-ebpf: fast-path fentry stub.
 *
 * Week 0: attaches to vfs_getattr and returns 0. The bpf_arena map is
 * declared here so bpf2go generates a Go binding for it, but nothing
 * writes to it yet. Week 2 replaces the stub with a KFunc dispatch.
 */

#include "vmlinux.h"
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_tracing.h>

#include "arena_types.h"

char LICENSE[] SEC("license") = "Dual BSD/GPL";

/*
 * BPF_MAP_TYPE_ARENA was introduced in Linux 6.9. Older kernels' BTF
 * does not know the enum value, so we fall back to the upstream numeric
 * constant purely so the source compiles on 6.8 for CI checks. At load
 * time the arena requires a real 6.9+ kernel.
 */
#ifndef BPF_MAP_TYPE_ARENA
#define BPF_MAP_TYPE_ARENA 33
#endif

/* 4 GiB sparse arena, mmap-able from userspace. */
#define S3FS_ARENA_MAX_PAGES (1UL << 20)

struct {
	__uint(type, BPF_MAP_TYPE_ARENA);
	__uint(map_flags, BPF_F_MMAPABLE);
	__uint(max_entries, S3FS_ARENA_MAX_PAGES);
} s3fs_arena SEC(".maps");

SEC("fentry/vfs_getattr")
int BPF_PROG(s3fs_fp_getattr, const struct path *path, struct kstat *stat,
	     __u32 request_mask, unsigned int query_flags)
{
	/* Skeleton: no-op. Real fast-path lands in Week 2. */
	return 0;
}
