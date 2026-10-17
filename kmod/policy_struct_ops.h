/* SPDX-License-Identifier: GPL-2.0 */
#ifndef _S3FS_POLICY_STRUCT_OPS_H
#define _S3FS_POLICY_STRUCT_OPS_H

#include <linux/types.h>

/*
 * s3fs_policy_ops — the vtable BPF struct_ops programs implement.
 *
 * BPF side (see bpf/policy_sieve_ops.bpf.c for the simplest example):
 *
 *   SEC(".struct_ops.link")
 *   struct s3fs_policy_ops <name>_ops = {
 *       .on_admit    = (void *)my_on_admit,
 *       .on_evict    = (void *)my_on_evict,
 *       .on_pressure = (void *)my_on_pressure,
 *       .on_hit      = (void *)my_on_hit,
 *   };
 *
 * Programs must be non-sleepable (SEC("struct_ops/..."), not
 * "struct_ops.s/..."): the eval wrappers call them inside
 * rcu_read_lock(), and on_hit fires per folio on the read path.
 *
 * kmod side dispatches via rcu_dereference'd pointer; the whole
 * vtable is swapped by one BPF_LINK_UPDATE on the struct_ops link
 * (-> s3fs_policy_update, a single rcu_assign_pointer).
 */
struct s3fs_policy_ops {
	int (*on_admit)(u64 count, u64 arena_used_hint);
	int (*on_evict)(u64 ino);
	int (*on_pressure)(u64 count, u64 threshold);
	int (*on_hit)(u64 ino);
	/* AC-58 (docs/design/ac58-pick-victim.md).  Appended at the end so
	 * programs built against the 4-member vtable keep loading (libbpf
	 * binds by member name).  on_insert / on_remove / pick_victim are
	 * serialized by the kmod (s3fs_policy_evict_lock), so the BPF side
	 * needs no lock for state they alone touch. */
	int (*on_insert)(u64 ino);   /* ino entered the candidate set (idempotent) */
	int (*on_remove)(u64 ino);   /* the kmod evicted ino */
	u64 (*pick_victim)(void);    /* next ino to evict; 0 = no opinion */
};

/* Kmod entry points: call these from hot paths in vfs_ops.c. Each
 * returns 0 when no policy is loaded (safe default). */
int s3fs_policy_ops_eval_admit(u64 count, u64 arena_used_hint);
int s3fs_policy_ops_eval_evict(u64 ino);
int s3fs_policy_ops_eval_pressure(u64 count, u64 threshold);
int s3fs_policy_ops_eval_hit(u64 ino);

/* AC-58.  Take s3fs_policy_evict_lock around a whole eviction round:
 * the three calls below must not run concurrently with each other. */
void s3fs_policy_evict_lock(void);
void s3fs_policy_evict_unlock(void);
bool s3fs_policy_has_pick_victim(void);
void s3fs_policy_ops_insert(u64 ino);
void s3fs_policy_ops_remove(u64 ino);
u64  s3fs_policy_ops_pick_victim(void);

int  s3fs_policy_struct_ops_register(void);
void s3fs_policy_struct_ops_unregister(void);

#endif /* _S3FS_POLICY_STRUCT_OPS_H */
