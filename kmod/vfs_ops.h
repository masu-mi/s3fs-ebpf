#ifndef _S3FS_EBPF_VFS_OPS_H
#define _S3FS_EBPF_VFS_OPS_H

#include <linux/fs.h>
#include <linux/fs_context.h>
#include <linux/mm_types.h>
#include <linux/types.h>

#include "s3fs_ebpf_uapi.h"

/* Root inode number — the mount point directory. G-01 uses this as
 * the "parent is root" sentinel value when daemon sends parent_ino=0
 * (or omits it in a pre-G-01 build). */
#define S3FS_ROOT_INO 1

int s3fs_ebpf_fill_super(struct super_block *sb, struct fs_context *fc);

/*
 * Admit table API — populated by the /dev/s3fs_ebpf ioctl handler,
 * consumed by the read path. `mm` is the daemon's mm_struct (with a
 * reference held by kmod); entries carry userspace addresses into
 * that mm which .read pins via pin_user_pages_remote. count==0 clears
 * the arena entries; a non-NULL mm still replaces the held mm (AC-43),
 * mm==NULL keeps it. Only admit_shutdown drops the mm ref.
 */
int  s3fs_ebpf_admit_replace(struct mm_struct *mm,
			     const struct s3fs_admit_entry *entries,
			     u32 count);
void s3fs_ebpf_admit_shutdown(void);

/* AB-04: populate the Linux page cache (via fd->buf) for `ino` with `size`
 * bytes of file data from user pointer `data_ptr` at file offset `offset`.
 * Inserts an S3FS_ADMIT_FLAG_PCACHE admit entry so read paths skip the
 * BPF arena / GUP and fall through to generic_file_read_iter. */
int  s3fs_ebpf_populate_pages(u64 ino, u64 offset, u64 size,
			      const void __user *data_ptr);

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
	unsigned long parent_ino; /* G-01: 0 == root. Compare inode->i_ino to filter iterate_shared. */
	__u64 size;
	umode_t mode;             /* S_IFDIR bit distinguishes subdirs from regular files */
	__u32 name_len;
	char name[S3FS_SYNC_NAME_MAX + 1]; /* basename only */
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
int s3fs_ebpf_signal_admit(u64 ino, u64 chunk_offset, int err, u32 flags);

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
int  s3fs_ebpf_flush_pop_wait(struct s3fs_flush_event *out);
long s3fs_ebpf_read_dirty(u64 ino, void __user *uaddr, u64 cap);
int  s3fs_ebpf_mark_dirty(u64 ino);
int  s3fs_ebpf_delete_pop(struct s3fs_delete_event *out);
int  s3fs_ebpf_rename_pop(struct s3fs_rename_event *out);

/*
 * Blocking fsync completion signal (E-01). Daemon calls this after
 * a PUT finishes (err=0 on success, negative errno on failure) to
 * wake the fsync() caller blocked in .fsync. -ENOENT (no waiter) is
 * an expected outcome for the .release fire-and-forget path.
 */
int s3fs_ebpf_signal_flush(u64 ino, int err);

/* Cumulative policy-driven evictions from the admit table. Exposed
 * via a new stats ioctl for daemon-side Prometheus export. */
#include <linux/atomic.h>
extern atomic_long_t s3fs_evictions_total;

/* Cumulative .read_folio calls that served from the arena (admit
 * hit). Divide by (this + read_miss_upcalls) for the true kmod-side
 * cache hit ratio. */
extern atomic_long_t s3fs_folio_hits;
extern atomic_long_t s3fs_admit_invalidations;

/* Running admit hash size. Read from s3fs_kfunc_admit_count() in
 * tree_traversal.c so BPF policy programs can make pressure-aware
 * decisions. Updated under s3fs_admits_lock write side (rwlock). */
extern atomic_t s3fs_admits_count;

/* Default for the `softcap` module parameter (entry count above which
 * eviction votes are applied, legacy mode). Runtime value lives in
 * s3fs_softcap so tree_traversal.c can expose it as a KFunc. */
#define S3FS_EVICT_SOFTCAP 8U
extern unsigned int s3fs_softcap;
/* `capacity_bytes` module parameter (S-02). 0 = legacy vote mode. */
extern unsigned long long s3fs_capacity_bytes;

/* AB-02: when non-zero, s3fs_ebpf_read_iter skips the arena fast path
 * and always falls through to generic_file_read_iter (page-cache path).
 * Allows bench-ab-arena-vs-pagecache.sh to compare arena vs page-cache
 * latency under identical conditions without remounting. */
extern bool s3fs_arena_bypass;

/* AB-01: cumulative GUP (pin_user_pages_remote) call count and total
 * latency in nanoseconds. Exported via IOC_STATS slots 3/4 so the
 * bench script can compute mean ns/call without bpftrace. */
extern atomic_long_t s3fs_gup_calls_total;
extern atomic_long_t s3fs_gup_ns_total;

/* T4: .read_folio calls that exhausted S3FS_UPCALL_MAX_RETRIES with the
 * daemon alive. Exported via IOC_STATS slot 5.  Non-zero means capacity
 * pressure caused -EIO (not data corruption). */
extern atomic_long_t s3fs_upcall_retry_exhausted;

/* U11: .read_folio upcall waits that timed out before daemon signal.
 * Exported via IOC_STATS slot 6. */
extern atomic_long_t s3fs_upcall_timeout;

/* D-diag: gen=0 fallback failure reason counters. Exported via slots 7/8/9. */
extern atomic_long_t s3fs_gen0_fail_buf_null;
extern atomic_long_t s3fs_gen0_fail_bitmap;
extern atomic_long_t s3fs_gen0_fail_other;

/* M1: fd->buf allocated bytes (gauge, slot 10) and deferred-free resets
 * (counter, slot 11). */
extern atomic_long_t s3fs_fd_buf_bytes;
extern atomic_long_t s3fs_reset_skipped;

/* M2: gauge cross-check (slots 12/13) — see vfs_ops.c. */
void s3fs_ebpf_fd_buf_audit(u64 *walk, u64 *pcache);

/* Per-ino size lookup. Returns 0 when the ino is not admitted.
 * Called by s3fs_kfunc_ino_size(). Takes s3fs_admits_lock read side. */
u64 s3fs_admit_size(u64 ino);

/* Drain the ring buffer of policy-evicted inos. `out` holds up to
 * `cap` slots; kmod writes the actual count into *filled and any
 * dropped entries (since last drain) into *overflow. */
void s3fs_ebpf_drain_evictions(u64 *out, u32 cap, u32 *filled, u32 *overflow);

/*
 * Policy hooks (Layer 2/3 fentry attach anchors). See
 * docs/design/policy-engine-design.md §4.5. Bodies are no-ops — BPF programs
 * supply the real behavior via fentry attach. Return values are
 * consumed by callers (kmod side) as advisory when the hook is
 * BPF-overridden.
 */
int s3fs_policy_on_admit(u64 count, u64 arena_used_hint);
int s3fs_policy_on_evict(u64 ino);
int s3fs_policy_on_pressure(u64 count, u64 threshold);
int s3fs_policy_on_hit(u64 ino);

#endif /* _S3FS_EBPF_VFS_OPS_H */
