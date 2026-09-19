// SPDX-License-Identifier: (LGPL-2.1 OR BSD-2-Clause)
/*
 * s3fs-ebpf: fast-path + Week 1 shared-memory / KFunc round-trip.
 *
 * Programs:
 *   fentry/vfs_getattr        -- Week 0 no-op stub, kept for verifier smoke
 *   syscall/s3fs_test_shared  -- Week 1 harness invoked by BPF_PROG_TEST_RUN:
 *                                reads an mmap-shared array populated by the
 *                                daemon, then calls s3fs_kfunc_echo from the
 *                                kernel module.
 *
 * Note on arena vs. mmap-array: Week 1 tests true `BPF_MAP_TYPE_ARENA`
 * creation from the daemon side (see internal/arena), but the BPF
 * program itself uses `BPF_MAP_TYPE_ARRAY | BPF_F_MMAPABLE` because
 * cilium/ebpf v0.22 does not yet relocate `.addr_space.1` globals. Week 2
 * upgrades to direct arena dereference from the BPF program.
 */

#include "vmlinux.h"
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_tracing.h>

#include "arena_types.h"
#include "s3fs_shared_types.h"

char LICENSE[] SEC("license") = "Dual BSD/GPL";

#ifndef BPF_F_MMAPABLE
#define BPF_F_MMAPABLE (1U << 10)
#endif

#define S3FS_TEST_INODES 32

/*
 * Mmap-able array shared with the daemon. The daemon writes dummy
 * s3fs_inode_stub entries via Map.Memory(); the BPF program reads them
 * via bpf_map_lookup_elem.
 */
struct {
	__uint(type, BPF_MAP_TYPE_ARRAY);
	__uint(map_flags, BPF_F_MMAPABLE);
	__type(key, __u32);
	__type(value, struct s3fs_inode_stub);
	__uint(max_entries, S3FS_TEST_INODES);
} s3fs_shared SEC(".maps");

/*
 * KFunc provided by kmod/tree_traversal.c, registered against
 * BPF_PROG_TYPE_SYSCALL so this SEC("syscall") program may call it.
 */
extern __u32 s3fs_kfunc_echo(__u32 v) __ksym;

SEC("fentry/vfs_getattr")
int BPF_PROG(s3fs_fp_getattr, const struct path *path, struct kstat *stat,
	     __u32 request_mask, unsigned int query_flags)
{
	/* Week 2 replaces this with the real KFunc dispatch. */
	return 0;
}

SEC("syscall")
int s3fs_test_shared(struct s3fs_test_args *ctx)
{
	struct s3fs_inode_stub *inode;
	__u32 idx;

	if (!ctx)
		return -1;

	idx = ctx->idx;
	inode = bpf_map_lookup_elem(&s3fs_shared, &idx);
	if (!inode)
		return -2;

	ctx->out_ino   = inode->ino;
	ctx->out_kfunc = s3fs_kfunc_echo(ctx->kfunc_arg);
	return 0;
}
