// SPDX-License-Identifier: GPL-2.0
//
// kfunc_walk — the "after" half of `s3fs-ebpf dev verifier-demo`.
//
// s3fs_fastpath.bpf.c:s3fs_deep_walk_reject writes an arena
// linked-list walk as a plain eBPF loop and the verifier refuses it
// (it cannot bound `n = n->next`). This program hands the identical
// walk to the kmod KFunc s3fs_kfunc_arena_list_walk
// (kmod/tree_traversal.c), so the BPF side is straight-line code the
// verifier accepts immediately, and the unbounded part runs as
// ordinary kernel C with its own safety (copy_from_user) and
// termination (max_nodes, signal check) guarantees.
//
// Kept out of s3fs_fastpath.bpf.c so it does not depend on the
// embedded bpf2go object; the daemon loads bpf/kfunc_walk.bpf.o from
// disk. Needs the kmod loaded (the __ksym resolves against its BTF).

#include "vmlinux.h"
#include <bpf/bpf_helpers.h>

/* Mirrors daemon/cmd/s3fs-ebpf/main.go walkArgs. */
struct s3fs_walk_args {
	__u64 head_uaddr;  /* in: first node (arena user_vm address) */
	__u64 out_sum;     /* out: sum of payloads */
	__s64 out_ret;     /* out: nodes visited or -errno */
	__u32 max_nodes;   /* in: 0 = kmod default cap */
	__u32 _pad;
};

extern __s64 s3fs_kfunc_arena_list_walk(__u64 head_uaddr, __u32 max_nodes,
					__u64 *sum) __ksym;

SEC("syscall")
int s3fs_deep_walk_kfunc(struct s3fs_walk_args *ctx)
{
	__u64 sum = 0;

	ctx->out_ret = s3fs_kfunc_arena_list_walk(ctx->head_uaddr,
						  ctx->max_nodes, &sum);
	ctx->out_sum = sum;
	return 0;
}

char _license[] SEC("license") = "GPL";
