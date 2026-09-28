/* SPDX-License-Identifier: GPL-2.0 */
/*
 * s3fs-ebpf UAPI: shared ioctl definitions between kmod, daemon, and BPF.
 *
 * All commands are issued against /dev/s3fs_ebpf, exposed by the module as
 * a miscdevice once loaded.
 */
#ifndef _S3FS_EBPF_UAPI_H
#define _S3FS_EBPF_UAPI_H

#ifdef __KERNEL__
#include <linux/ioctl.h>
#include <linux/types.h>
#else
#include <sys/ioctl.h>
#include <stdint.h>
typedef uint32_t __u32;
typedef uint64_t __u64;
#endif

#define S3FS_EBPF_DEV_NAME "s3fs_ebpf"
#define S3FS_IOC_MAGIC     0xE3

/*
 * S3FS_IOC_PROBE_KVBASE — Week 4 (a) spike.
 *
 * Given a BPF arena map fd, return the kernel-VM base address that the
 * arena's pages are mapped at (arena->kern_vm->addr). Meant only for
 * verifying that the container_of-based extraction works on the running
 * kernel; production paths will admit ino→backing tuples via a separate
 * ioctl once the theory is validated.
 */
struct s3fs_probe_kvbase {
	__u32 arena_fd;   /* in:  fd of BPF_MAP_TYPE_ARENA held by daemon */
	__u32 _pad;
	__u64 out_kvbase; /* out: kernel-VM start of the arena */
	__u64 out_size;   /* out: mapped size in bytes (pages * PAGE_SIZE) */
};

#define S3FS_IOC_PROBE_KVBASE \
	_IOWR(S3FS_IOC_MAGIC, 0x01, struct s3fs_probe_kvbase)

/*
 * S3FS_IOC_ADMIT_ARENA — register per-ino userspace ranges that back
 * file content.
 *
 * The daemon writes each object's bytes into its arena mmap, then
 * submits this ioctl to tell kmod: "ino N's content lives at user_vm
 * address `uaddr` for `size` bytes". kmod stores (ino → mm, uaddr,
 * size) in a hash keyed by ino; .read then uses
 * pin_user_pages_remote + kmap_local_page to reach the same physical
 * pages the arena mmap holds and copy_to_user out of them.
 *
 * We use the daemon's user_vm view (not the arena's kernel-VM base)
 * because 6.12's arena kern_vm range has no generic-context fault
 * handler; only BPF programs can service the fault path. GUP over
 * user_vm sidesteps that limitation by going through the daemon's
 * process mm, which does have arena_vm_fault registered.
 *
 * Passing count=0 clears kmod's admission table (used on daemon exit
 * to drop the mm reference cleanly).
 */
struct s3fs_admit_entry {
	__u64 ino;
	__u64 uaddr; /* daemon-side user_vm address of the object bytes */
	__u64 size;
};

struct s3fs_admit_arena {
	__u32 count;   /* in: number of entries; 0 clears the table */
	__u32 _pad;
	__u64 entries; /* in: user pointer to array of s3fs_admit_entry */
};

#define S3FS_IOC_ADMIT_ARENA \
	_IOW(S3FS_IOC_MAGIC, 0x02, struct s3fs_admit_arena)

/*
 * S3FS_IOC_SIGNAL_ADMIT — wake a pending .read waiter.
 *
 * The daemon calls this after fetching an unadmitted object from S3
 * and issuing an admit-arena. kmod's blocked read finds its pending
 * entry keyed by ino, records the err code, and completes the
 * waiter. err==0 signals success (the caller should retry the admit
 * lookup and serve out of the arena page); err<0 propagates the
 * error to the read syscall.
 */
struct s3fs_signal_admit {
	__u64 ino;
	__s32 err;
	__u32 _pad;
};

#define S3FS_IOC_SIGNAL_ADMIT \
	_IOW(S3FS_IOC_MAGIC, 0x03, struct s3fs_signal_admit)

/*
 * Read-miss event stream.
 *
 * kmod's .read on an unadmitted ino appends an event of this shape to
 * an internal FIFO and wakes the char-dev read waitqueue. The daemon
 * reads /dev/s3fs_ebpf like a stream, one struct per event (short
 * reads return the current head atomically). O_NONBLOCK is honored;
 * poll(2) returns POLLIN when at least one event is queued.
 */
struct s3fs_read_miss_event {
	__u64 ino;
	__s64 offset; /* where the miss was */
	__u32 count;  /* bytes requested */
	__u32 _pad;
};

/*
 * S3FS_IOC_SYNC_ENTRIES — replace the kmod's dentry/inode entry table
 * with a fresh snapshot.
 *
 * The daemon lists the S3 bucket, assigns an ino to each key
 * (first-seen order, in-memory only), and pushes the full set here.
 * kmod atomically swaps in the new table under a rwlock and flushes
 * stale dentries so `ls` and lookup reflect the new state. Names must
 * be NUL-safe and <= NAME_MAX; `mode` should include S_IFREG for
 * regular files (dir support will land later).
 *
 * This is orthogonal to S3FS_IOC_ADMIT_ARENA: SYNC_ENTRIES declares
 * "these files exist"; ADMIT_ARENA says "here is the content backing
 * for these inos". Typical order per sync round is
 * SYNC_ENTRIES → per-object prewarm → ADMIT_ARENA.
 *
 * count == 0 restores the compile-time bootstrap table (used at
 * shutdown so the mount is still browsable if the daemon exits).
 */
#define S3FS_SYNC_NAME_MAX 255
struct s3fs_sync_entry {
	__u64 ino;
	__u64 size;      /* authoritative size for i_size, from S3 stat */
	__u32 mode;      /* S_IFREG | 0644 etc. */
	__u32 name_len;  /* strlen(name), 0 < name_len <= S3FS_SYNC_NAME_MAX */
	char  name[S3FS_SYNC_NAME_MAX + 1]; /* NUL-terminated for safety */
};

struct s3fs_sync_entries {
	__u32 count;
	__u32 _pad;
	__u64 entries; /* user pointer to array of s3fs_sync_entry */
};

#define S3FS_IOC_SYNC_ENTRIES \
	_IOW(S3FS_IOC_MAGIC, 0x04, struct s3fs_sync_entries)

/*
 * Write-back queue.
 *
 * kmod's .release / .fsync on a dirty regular file enqueues a
 * flush event; the daemon periodically drains this queue via
 * S3FS_IOC_POP_FLUSH and PUTs the current buffer bytes back to S3.
 * A .create fop enqueues an empty flush (size=0) so `touch` results
 * in a zero-byte PUT.
 *
 * The event carries the file's name (basename in the mount) so the
 * daemon can resolve it back to an S3 key even for freshly-created
 * inos that haven't hit an ino allocator yet.
 *
 * POP_FLUSH returns 0 with `event` populated on success, or -EAGAIN
 * when the queue is empty (non-blocking; daemon polls on a ticker).
 */
struct s3fs_flush_event {
	__u64 ino;
	__u64 size;      /* current buffer size at enqueue time */
	__u32 name_len;
	__u32 _pad;
	char  name[S3FS_SYNC_NAME_MAX + 1];
};

#define S3FS_IOC_POP_FLUSH \
	_IOR(S3FS_IOC_MAGIC, 0x05, struct s3fs_flush_event)

/*
 * S3FS_IOC_READ_DIRTY — pull `size` bytes from kmod's per-inode
 * scratch buffer into the daemon's `uaddr` for PUT to S3.
 *
 * `size` is capped by the kmod's current `fd->size`; a shorter file
 * returns min(cap, request). The kmod validates `uaddr` via
 * copy_to_user; caller must have the buffer allocated in its own mm.
 * Returns 0 on success and sets `out_bytes` to the actual copy size.
 */
struct s3fs_read_dirty {
	__u64 ino;
	__u64 uaddr;      /* in:  daemon-side buffer address */
	__u64 size;       /* in:  buffer capacity */
	__u64 out_bytes;  /* out: number of bytes actually copied */
};

#define S3FS_IOC_READ_DIRTY \
	_IOWR(S3FS_IOC_MAGIC, 0x06, struct s3fs_read_dirty)

/*
 * S3FS_IOC_MARK_DIRTY — daemon → kmod: re-mark the ino's fd as dirty.
 *
 * Called by the daemon flush loop when a S3 PUT fails. Without this,
 * .release cleared the dirty flag before enqueue, so a failed PUT
 * would silently lose the write until the next user-driven write().
 * Marking dirty again lets the next .release / .fsync re-enqueue.
 *
 * No completion signalling — the mark is fire-and-forget. If the
 * inode has been evicted between .release and the PUT, kmod
 * silently ignores the request (ino not in inode_hashtable).
 */
struct s3fs_mark_dirty {
	__u64 ino;
};

#define S3FS_IOC_MARK_DIRTY \
	_IOW(S3FS_IOC_MAGIC, 0x07, struct s3fs_mark_dirty)

#endif /* _S3FS_EBPF_UAPI_H */
