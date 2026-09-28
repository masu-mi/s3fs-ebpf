/* SPDX-License-Identifier: GPL-2.0 */
#ifndef _S3FS_POLICY_STRUCT_OPS_H
#define _S3FS_POLICY_STRUCT_OPS_H

#include <linux/types.h>

/*
 * s3fs_policy_ops — the vtable BPF struct_ops programs implement.
 *
 * BPF side (see bpf/policy_s3fifo_ops.bpf.c):
 *
 *   SEC(".struct_ops.link")
 *   struct s3fs_policy_ops s3fifo_ops = {
 *       .on_admit    = (void *)my_on_admit,
 *       .on_evict    = (void *)my_on_evict,
 *       .on_pressure = (void *)my_on_pressure,
 *   };
 *
 * kmod side dispatches via rcu_dereference'd pointer; the whole
 * vtable is swapped in a single link.Update.
 */
struct s3fs_policy_ops {
	int (*on_admit)(u64 count, u64 arena_used_hint);
	int (*on_evict)(u64 ino);
	int (*on_pressure)(u64 count, u64 threshold);
	int (*on_hit)(u64 ino);
};

/* Kmod entry points: call these from hot paths in vfs_ops.c. Each
 * returns 0 when no policy is loaded (safe default). */
int s3fs_policy_ops_eval_admit(u64 count, u64 arena_used_hint);
int s3fs_policy_ops_eval_evict(u64 ino);
int s3fs_policy_ops_eval_pressure(u64 count, u64 threshold);
int s3fs_policy_ops_eval_hit(u64 ino);

int  s3fs_policy_struct_ops_register(void);
void s3fs_policy_struct_ops_unregister(void);

#endif /* _S3FS_POLICY_STRUCT_OPS_H */
