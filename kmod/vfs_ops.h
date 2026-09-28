#ifndef _S3FS_EBPF_VFS_OPS_H
#define _S3FS_EBPF_VFS_OPS_H

#include <linux/fs.h>
#include <linux/fs_context.h>
#include <linux/mm_types.h>
#include <linux/types.h>

#include "s3fs_ebpf_uapi.h"

int s3fs_ebpf_fill_super(struct super_block *sb, struct fs_context *fc);

/*
 * Admit table API — populated by the /dev/s3fs_ebpf ioctl handler,
 * consumed by the read path. `mm` is the daemon's mm_struct (with a
 * reference held by kmod); entries carry userspace addresses into
 * that mm which .read pins via pin_user_pages_remote. Passing
 * count==0 (with mm==NULL) clears the table and drops the mm ref.
 */
int  s3fs_ebpf_admit_replace(struct mm_struct *mm,
			     const struct s3fs_admit_entry *entries,
			     u32 count);
void s3fs_ebpf_admit_shutdown(void);

/*
 * Entry table replace — daemon publishes the current bucket snapshot
 * here; kmod atomically swaps in the new table and flushes the dcache
 * so stale positive/negative dentries clear. Ownership of `entries`
 * transfers to the module on a non-zero count; count==0 (or entries
 * NULL) restores the compile-time bootstrap. Old dynamic array is
 * freed inside; caller must not touch it after this returns.
 */
struct s3fs_entry_dyn {
	unsigned long ino;
	__u64 size;
	umode_t mode;
	__u32 name_len;
	char name[S3FS_SYNC_NAME_MAX + 1];
};
int s3fs_ebpf_entries_replace(struct s3fs_entry_dyn *entries, u32 count);
void s3fs_ebpf_entries_shutdown(void);

/*
 * Read-miss upcall API — daemon-facing side.
 * s3fs_read_miss_dequeue removes the head event (blocking or
 * non-blocking) and copies it out. s3fs_read_miss_poll bridges the
 * miscdevice .poll to the internal waitqueue. signal_admit wakes a
 * pending .read waiter.
 */
struct file;
struct poll_table_struct;
ssize_t s3fs_read_miss_dequeue(struct s3fs_read_miss_event *out,
			       bool nonblocking);
__poll_t s3fs_read_miss_poll(struct file *filp, struct poll_table_struct *pt);
int s3fs_ebpf_signal_admit(u64 ino, int err);

/*
 * Write-back queue: kmod → daemon via S3FS_IOC_POP_FLUSH.
 *
 * s3fs_ebpf_flush_pop: dequeue one event; returns 0 on success (out
 * populated), -EAGAIN if empty.
 * s3fs_ebpf_read_dirty: copy up to `cap` bytes of ino's scratch
 * buffer into user-space `uaddr`. Returns bytes copied on success,
 * negative errno on failure.
 */
int  s3fs_ebpf_flush_pop(struct s3fs_flush_event *out);
long s3fs_ebpf_read_dirty(u64 ino, void __user *uaddr, u64 cap);
int  s3fs_ebpf_mark_dirty(u64 ino);

/* Cumulative policy-driven evictions from the admit table. Exposed
 * via a new stats ioctl for daemon-side Prometheus export. */
#include <linux/atomic.h>
extern atomic_long_t s3fs_evictions_total;

/* Running admit hash size. Read from s3fs_kfunc_admit_count() in
 * tree_traversal.c so BPF policy programs can make pressure-aware
 * decisions. Updated under s3fs_admits_lock. */
extern atomic_t s3fs_admits_count;

/* Soft cap that gates eviction voting — must equal the local
 * `softcap` in s3fs_ebpf_admit_replace(). Duplicated as an extern
 * so tree_traversal.c can expose it as a KFunc. */
#define S3FS_EVICT_SOFTCAP 8U

/* Drain the ring buffer of policy-evicted inos. `out` holds up to
 * `cap` slots; kmod writes the actual count into *filled and any
 * dropped entries (since last drain) into *overflow. */
void s3fs_ebpf_drain_evictions(u64 *out, u32 cap, u32 *filled, u32 *overflow);

/*
 * Policy hooks (Layer 2/3 fentry attach anchors). See
 * docs/policy-engine-design.md §4.5. Bodies are no-ops — BPF programs
 * supply the real behavior via fentry attach. Return values are
 * consumed by callers (kmod side) as advisory when the hook is
 * BPF-overridden.
 */
int s3fs_policy_on_admit(u64 count, u64 arena_used_hint);
int s3fs_policy_on_evict(u64 ino);
int s3fs_policy_on_pressure(u64 count, u64 threshold);
int s3fs_policy_on_hit(u64 ino);

#endif /* _S3FS_EBPF_VFS_OPS_H */
