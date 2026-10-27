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
/*
 * `offset` (D-01, added 2026-09): file offset where this range
 * starts. offset==0 & size==file_size means whole-file admit
 * (backward-compat default). Multiple entries per ino are allowed
 * — daemon splits large files into ranges so a 200 MiB blob can
 * be partially admitted into the 128 MiB arena. kmod's admit
 * lookup walks entries at the same hash bucket and picks the range
 * covering the folio pos.
 */
struct s3fs_admit_entry {
	__u64 ino;
	__u64 uaddr;  /* daemon-side user_vm address of the range bytes */
	__u64 size;   /* range length in bytes */
	__u64 offset; /* file offset where this range starts */
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
 * entry keyed by (ino, chunk_offset), records the err code, and
 * completes the waiter. err==0 signals success (the caller should
 * retry the admit lookup and serve from arena/pcache); err<0
 * propagates the error to the read syscall.
 * chunk_offset must be aligned to S3FS_PCACHE_CHUNK_SIZE (4 MiB).
 * Full-file admits (small files) use chunk_offset=0.
 */
struct s3fs_signal_admit {
	__u64 ino;
	__u64 chunk_offset;   /* chunk boundary; 0 for full-file admits */
	__s32 err;
	__u32 flags;          /* S3FS_SIGNAL_* */
};

/*
 * AC-45: with err == 0, only wake the waiter if the 4 MiB chunk at
 * chunk_offset is actually cached (allocated and populated) in the ino's
 * fd; otherwise leave the waiter asleep and return -EAGAIN so the daemon
 * can populate again.  Without this a handler whose data was evicted
 * between its populate and its signal wakes the reader with nothing to
 * read, and under frequent eviction the reader burns its retries on such
 * stale wakeups (pcache mode only: arena admits keep no chunk).
 */
#define S3FS_SIGNAL_VERIFY_POPULATED	0x1u

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
	__u32 flags;  /* S3FS_MISS_FLAG_* */
};

/*
 * AC-50: the miss is not a read.  kmod is about to modify (write, truncate)
 * a file whose content it does not hold: open(2) loads nothing, and a
 * flush PUTs the whole buffer, so the unmodified part must be fetched
 * first.  The daemon loads the 4 MiB chunk containing `offset` into the
 * ino's fd with POPULATE_PAGES — in arena mode too — and signals the
 * waiter; it does not admit anything into the arena.
 */
#define S3FS_MISS_FLAG_FOR_WRITE	0x1u
/* AC-51: the fd's content depends on the object's pinned ETag (it is dirty or
 * flushing, or holds remote chunks that a flush copies from the object).  A
 * changed object is then a conflict.  Without this flag the fd depends on
 * nothing, so the daemon may drop a stale pin and re-Stat. */
#define S3FS_MISS_FLAG_PINNED		0x2u
/* AC-51: also sent for a read of a remote chunk of a dirty file: load the
 * whole chunk at `offset` into the fd (kmod accepts it because the chunk is
 * remote; the daemon needs nothing else). */

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
/* G-01: struct s3fs_sync_entry v2 adds parent_ino for subdir support.
 *
 * ABI note: this is a size change vs the pre-G-01 layout. kmod and
 * daemon must be rebuilt together (there is no version negotiation
 * yet — see docs/design/g01-subdir-plan.md §4). parent_ino=0 is treated
 * equivalently to root (S3FS_ROOT_INO=1) so any hand-crafted caller
 * that forgets to set it still lands under root.
 */
struct s3fs_sync_entry {
	__u64 ino;
	__u64 parent_ino; /* G-01: 0 == root == ino 1. Directory entries must live at their intended parent. */
	__u64 size;      /* authoritative size for i_size, from S3 stat */
	__u32 mode;      /* S_IFREG | 0644 or S_IFDIR | 0755 */
	__u32 name_len;  /* strlen(name), 0 < name_len <= S3FS_SYNC_NAME_MAX. basename only, no `/`. */
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
/*
 * flush flags (E-03):
 *   FLUSH_FLAG_WAIT — fsync() caller is blocked on signal_flush.
 *                     daemon must PUT immediately (no coalescing).
 */
#define S3FS_FLUSH_FLAG_WAIT 0x1U

struct s3fs_flush_event {
	__u64 ino;
	__u64 parent_ino; /* G-01: dir the file lives under. 0 == root. */
	__u64 size;      /* current buffer size at enqueue time */
	__u32 name_len;
	__u32 flags;     /* S3FS_FLUSH_FLAG_* (E-03) */
	char  name[S3FS_SYNC_NAME_MAX + 1]; /* basename only */
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

/*
 * S3FS_IOC_STATS — read misc kmod-side counters that don't fit into
 * the existing per-event streams. Extensible via `count` (in) /
 * `filled` (out); daemon reads the first `filled` slots.
 * Slot layout (additive-only, do not renumber):
 *   [0] s3fs_evictions_total — policy-driven evictions from admits
 *   [1] s3fs_folio_hits      — .read_folio arena hits (kmod-side
 *                              cache hits, divide by (this + read_miss
 *                              _upcalls) for true hit ratio)
 *   [2] s3fs_admit_invalidations — page-cache invalidations by
 *                              admit_replace for new/moved ranges
 *   [14] s3fs_hits_replayed_total — hit_accum: accesses counted in
 *                              .read_iter and replayed into the policy
 *                              (before the hit_replay_max cut)
 */
#define S3FS_STATS_MAX 16
struct s3fs_stats {
	__u32 count;             /* in: number of slots caller allocated */
	__u32 filled;            /* out: number of slots kmod wrote */
	__u64 values[S3FS_STATS_MAX];
};

#define S3FS_IOC_STATS \
	_IOWR(S3FS_IOC_MAGIC, 0x08, struct s3fs_stats)

/*
 * S3FS_IOC_DRAIN_EVICTIONS — pull the list of inos the policy has
 * evicted from the admit hash since the last drain. Daemon polls
 * this periodically to keep its snapshot consistent with kmod's
 * actual hash; without it the daemon re-sends evicted inos on the
 * next admit_replace and kmod evicts them again in a loop.
 *
 * Caller allocates `inos` (up to `cap` slots); kmod writes up to
 * `filled` entries and drops them from its internal queue. Missing
 * entries (queue overflow) surface as `overflow` count so the daemon
 * can degrade gracefully (e.g. do a full-snapshot reconcile).
 */
#define S3FS_EVICT_LOG_MAX 256
struct s3fs_drain_evictions {
	__u32 cap;                              /* in: caller buffer capacity */
	__u32 filled;                           /* out: entries written to inos[] */
	__u32 overflow;                         /* out: kmod-side dropped due to queue full since last drain */
	__u32 _pad;
	__u64 inos[S3FS_EVICT_LOG_MAX];         /* out: evicted ino list */
};

#define S3FS_IOC_DRAIN_EVICTIONS \
	_IOWR(S3FS_IOC_MAGIC, 0x09, struct s3fs_drain_evictions)

/*
 * S3FS_IOC_SIGNAL_FLUSH — wake a pending .fsync waiter (E-01).
 *
 * Blocking .fsync registers a completion under (ino), then dispatches
 * a flush event to the daemon. After the daemon's S3 PUT resolves
 * (success or failure) it calls this ioctl with the PUT result. kmod
 * finds the pending entry and signals the fsync waiter to return.
 *
 * When no waiter exists (eager .release path — the default for
 * close(2)) the daemon still calls this after PUT so the same code
 * path handles both flush completion signals. Missing waiter returns
 * -ENOENT which the daemon ignores.
 *
 * err semantics match signal_admit: err==0 = PUT succeeded, negative
 * errno propagates to the fsync() return value.
 */
#define S3FS_IOC_SIGNAL_FLUSH \
	_IOW(S3FS_IOC_MAGIC, 0x0A, struct s3fs_signal_admit)

/*
 * S3FS_IOC_POP_DELETE — pop one queued delete event (E-02).
 *
 * kmod's .unlink enqueues here with the ino + basename. Daemon
 * drains the queue and issues S3 DELETE on the key.
 *
 * -EAGAIN when the queue is empty (daemon polls on a ticker).
 * .name shape matches s3fs_flush_event so daemon can reuse the
 * name→key resolution helper.
 */
struct s3fs_delete_event {
	__u64 ino;
	__u64 parent_ino; /* G-01: dir the file lived under. 0 == root. */
	__u32 name_len;
	__u32 _pad;
	char  name[S3FS_SYNC_NAME_MAX + 1]; /* basename only */
};

#define S3FS_IOC_POP_DELETE \
	_IOR(S3FS_IOC_MAGIC, 0x0B, struct s3fs_delete_event)

/*
 * S3FS_IOC_POP_RENAME — pop one queued rename event (E-02).
 *
 * kmod's .rename enqueues here with both names. Daemon implements
 * S3-side rename as CopyObject(old → new) + Delete(old).
 *
 * -EAGAIN when the queue is empty.
 */
struct s3fs_rename_event {
	__u64 ino;
	__u64 old_parent_ino; /* G-01: source dir. 0 == root. */
	__u64 new_parent_ino; /* G-01: destination dir. Equal to old_parent for in-place rename. */
	__u32 old_name_len;
	__u32 new_name_len;
	char  old_name[S3FS_SYNC_NAME_MAX + 1];
	char  new_name[S3FS_SYNC_NAME_MAX + 1];
};

#define S3FS_IOC_POP_RENAME \
	_IOR(S3FS_IOC_MAGIC, 0x0C, struct s3fs_rename_event)

/*
 * S3FS_IOC_POP_FLUSH_WAIT — blocking variant of POP_FLUSH for the
 * fsync path (S-08).
 *
 * Blocks until a flush event with FLUSH_FLAG_WAIT is available, then
 * pops and returns it. Returns -ERESTARTSYS on signal interruption, or
 * -ENODEV when the module is being unloaded. The daemon runs one
 * goroutine blocked here so fsync() callers are serviced immediately
 * instead of waiting up to one POP_FLUSH ticker period (~100 ms).
 *
 * Non-WAIT events (close/release path) are unaffected; they remain on
 * the regular POP_FLUSH queue and are handled by the coalesced 100 ms
 * ticker in runFlushLoop.
 */
#define S3FS_IOC_POP_FLUSH_WAIT \
	_IOR(S3FS_IOC_MAGIC, 0x0D, struct s3fs_flush_event)

/*
 * S3FS_IOC_POPULATE_PAGES — AB-04: write file data directly into the
 * kernel page cache for an admitted inode, bypassing the BPF arena /
 * GUP path entirely.
 *
 * The daemon fetches an S3 object range and hands the raw bytes to kmod
 * via this ioctl. kmod locks each page-aligned folio in the inode's
 * address_space, copies the data from userspace, marks it uptodate, and
 * releases the lock. Subsequent reads via generic_file_read_iter find
 * the data in the page cache without any GUP.
 *
 * After the copy, kmod inserts an admit entry with the
 * S3FS_ADMIT_FLAG_PCACHE flag set (uaddr=0, no daemon mm needed).
 * .read_iter skips the arena fast path and falls through to
 * generic_file_read_iter so the page cache serves the data.
 * If the kernel evicts the folios later, .read_folio re-triggers the
 * daemon upcall which calls this ioctl again to re-populate.
 *
 * data_ptr is a user-space pointer to `size` bytes of file content
 * starting at file position `offset`.
 */
struct s3fs_populate_pages {
	__u64 ino;
	__u64 offset;    /* file offset where data starts */
	__u64 size;      /* number of bytes in data */
	__u64 data_ptr;  /* user pointer to data bytes */
};

/* AB-04: admit entry flag — data is in page cache, not in BPF arena. */
#define S3FS_ADMIT_FLAG_PCACHE  (1U << 0)

#define S3FS_IOC_POPULATE_PAGES \
	_IOW(S3FS_IOC_MAGIC, 0x0E, struct s3fs_populate_pages)

/*
 * AC-51: S3FS_IOC_DIRTY_LAYOUT -- the chunk states of a dirty file, so the
 * daemon can flush the unchanged ("remote") chunks with a server-side copy
 * (UploadPartCopy) and only send the others.  `states` gets one byte per
 * chunk; -ENOSPC with nr_chunks set when `cap` is too small.  Like
 * READ_DIRTY it claims the flush events enqueued so far.
 */
#define S3FS_DIRTY_CHUNK_BYTES  (4U << 20)  /* = S3FS_CHUNK_SIZE in kmod */
#define S3FS_DIRTY_CHUNK_DATA   0  /* kmod holds the bytes (READ_DIRTY_RANGE) */
#define S3FS_DIRTY_CHUNK_HOLE   1  /* zeros: past the original object */
#define S3FS_DIRTY_CHUNK_REMOTE 2  /* unchanged from the object in S3 */

struct s3fs_dirty_layout {
	__u64 ino;
	__u64 states_uaddr;  /* in:  one byte per chunk */
	__u32 cap;           /* in:  capacity of states, in chunks */
	__u32 nr_chunks;     /* out: chunks in the file */
	__u64 size;          /* out: file size */
	__u64 chunk_size;    /* out: bytes per chunk */
};

#define S3FS_IOC_DIRTY_LAYOUT \
	_IOWR(S3FS_IOC_MAGIC, 0x0F, struct s3fs_dirty_layout)

/* AC-51: READ_DIRTY from `offset`; used for the data / hole chunks. */
struct s3fs_read_dirty_range {
	__u64 ino;
	__u64 uaddr;      /* in:  daemon-side buffer address */
	__u64 offset;     /* in:  file offset to start at */
	__u64 size;       /* in:  buffer capacity */
	__u64 out_bytes;  /* out: bytes copied */
};

#define S3FS_IOC_READ_DIRTY_RANGE \
	_IOWR(S3FS_IOC_MAGIC, 0x10, struct s3fs_read_dirty_range)

#endif /* _S3FS_EBPF_UAPI_H */
