// SPDX-License-Identifier: GPL-2.0
//
// AC-66: runs policy_wtinylfu_pick_ops's operations as a SEC("syscall")
// program, so the verifier and the logic can be checked without the kmod
// (whose struct_ops type the policy's own programs need).  Driven by
// daemon/internal/trace/bpf_harness_test.go via
// scripts/test-wtinylfu-harness.sh.  Not part of bpf/Makefile's OBJS.

#include "../policy_wtinylfu_pick_ops.bpf.c"

enum { WT_OP_HIT = 1, WT_OP_INSERT = 2, WT_OP_REMOVE = 3, WT_OP_PICK = 4, WT_OP_SIZE = 5 };

struct drive_args {
	__u32 op;
	__u32 pad;
	__u64 ino;
	__u64 arg;
	__u64 ret;
};

SEC("syscall")
int drive(struct drive_args *a)
{
	switch (a->op) {
	case WT_OP_HIT:    do_hit(a->ino); break;
	case WT_OP_INSERT: do_insert(a->ino); break;
	case WT_OP_REMOVE: do_remove(a->ino); break;
	case WT_OP_PICK:   a->ret = do_pick(); break;
	case WT_OP_SIZE:   do_size(a->ino, a->arg); break;
	}
	return 0;
}
