// SPDX-License-Identifier: GPL-2.0
//
// policy_test_noonhit_hit_ops.bpf.c — TEST FIXTURE for the struct_ops data members and validate()
// (scripts/test-policy-data-members.sh). REFUSED: NO_ON_HIT together with an on_hit program (validate -> -EINVAL).
// (Every program in the object must be bound to a member, or the load fails
// before validate runs: hence no unused hook here.)
// Not a policy to run: it only exists to be loaded (and, for the rejected
// ones, refused). See docs/dev-notes/struct-ops-data-members.md.

#include "vmlinux.h"
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_tracing.h>

#define S3FS_POLICY_NAME_MAX    16
#define S3FS_POLICY_F_NO_ON_HIT (1u << 0)

struct s3fs_policy_ops {
	int (*on_admit)(__u64 count, __u64 arena_used_hint);
	int (*on_evict)(__u64 ino);
	int (*on_pressure)(__u64 count, __u64 threshold);
	int (*on_hit)(__u64 ino);
	int (*on_insert)(__u64 ino);
	int (*on_remove)(__u64 ino);
	__u64 (*pick_victim)(void);
	int (*on_size)(__u64 ino, __u64 bytes);
	__u32 flags;
	char name[S3FS_POLICY_NAME_MAX];
};

SEC("struct_ops/on_hit")
int BPF_PROG(t_hit, __u64 ino)
{
	return 0;
}

SEC(".struct_ops.link")
struct s3fs_policy_ops noonhit_hit_ops = {
	.on_hit = (void *)t_hit,
	.flags  = S3FS_POLICY_F_NO_ON_HIT,
	.name   = "noonhit_hit",
};

char _license[] SEC("license") = "GPL";
