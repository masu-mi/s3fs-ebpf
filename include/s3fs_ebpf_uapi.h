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

/* nr 0x01 was PROBE_KVBASE (a Week 4 spike, removed); do not reuse. */

/*
 * S3FS_IOC_EVAL — run the kmod's capacity / policy pass now: replay the
 * accumulated hits into the policy, track new inodes (on_insert / on_size),
 * take the votes and evict down to capacity_bytes.  The daemon calls it after
 * the last populate of a miss (before it wakes the reader, so the chunk just
 * loaded is still pinned) and from POST /policy/eval.  No argument.
 *
 * (Was S3FS_IOC_ADMIT_ARENA, which registered per-ino userspace ranges for
 * the arena read path.  The file content lives in the kmod's chunk table;
 * nr 0x02 now carries this command.  Opening the device arms the read-miss
 * upcall path.)
 */
#define S3FS_IOC_EVAL \
	_IO(S3FS_IOC_MAGIC, 0x02)

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
 * Request stream (v2 Phase 4): everything kmod asks of the daemon arrives as
 * one `struct s3fs_req_event` per read(2) of /dev/s3fs_ebpf, in the order kmod
 * queued it (one FIFO, `reqq`).  A read needs a buffer of at least
 * sizeof(struct s3fs_req_event) (a shorter one gets -EINVAL, never a partial
 * event).  O_NONBLOCK is honored; poll(2) returns POLLIN when an event is
 * queued.
 *
 *   MISS  : a .read_folio / .open / write-prep that needs the daemon and waits
 *           for it (see s3fs_signal_admit).  ino, offset (where the miss was),
 *           count (bytes requested), flags (S3FS_MISS_FLAG_*), seq_count /
 *           rand_count (read pattern so far; 0 for a miss that is not a read).
 *           No seq.
 *   FLUSH : .release / .create placeholder (async) or .fsync (flags has
 *           S3FS_FLUSH_FLAG_WAIT; the caller waits for SIGNAL_FLUSH).  ino,
 *           parent_ino (0 == root), size (buffer size at enqueue time), name
 *           (basename), seq.
 *   DELETE: .unlink.  ino, parent_ino, name, seq.
 *   RENAME: .rename.  ino, parent_ino (source dir) / new_parent_ino, name
 *           (old) / new_name, seq.
 *
 * FLUSH / DELETE / RENAME carry one kmod-wide, strictly increasing `seq`,
 * assigned under the lock that queues the event (AC-84), so they are in seq
 * order in the FIFO and a daemon that handles them in read order cannot let a
 * rename overtake the flush of a file created just before it.  Unused fields
 * are 0.
 */
/* longest basename (and name in a request); see also struct s3fs_sync_entry */
#define S3FS_SYNC_NAME_MAX 255

#define S3FS_REQ_MISS   1U
#define S3FS_REQ_FLUSH  2U
#define S3FS_REQ_DELETE 3U
#define S3FS_REQ_RENAME 4U

struct s3fs_req_event {
	__u32 type;            /* S3FS_REQ_* */
	__u32 flags;           /* MISS: S3FS_MISS_FLAG_*, FLUSH: S3FS_FLUSH_FLAG_* */
	__u64 seq;
	__u64 ino;
	__u64 parent_ino;
	__u64 new_parent_ino;
	__u64 size;            /* FLUSH */
	__s64 offset;          /* MISS */
	__u32 count;           /* MISS */
	__u32 seq_count;       /* MISS: sequential reads so far */
	__u32 rand_count;      /* MISS: random reads so far */
	__u32 name_len;
	__u32 new_name_len;
	__u32 _pad;
	char  name[S3FS_SYNC_NAME_MAX + 1];
	char  new_name[S3FS_SYNC_NAME_MAX + 1];
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
/* open_check: not a read; HEAD the object and signal 0 (exists) or -ENOENT /
 * -EIO.  chunk_offset of the pending is S3FS_OPEN_CHECK_KEY (all ones). */
#define S3FS_MISS_FLAG_OPEN_CHECK	0x4u
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
	/*
	 * AC-85: every flush / rename / delete event with seq <= applied_seq had
	 * been applied to S3 before the daemon listed the bucket for this sync, so
	 * the listing reflects it. kmod drops a tombstone (an entry hidden by
	 * rename / unlink) and promotes a local entry (made by create / rename)
	 * only when the listing agrees AND the operation's seq is <= applied_seq:
	 * a listing taken earlier still shows the old state of S3.
	 * See docs/design/kmod-entry-table-state-machine.md.
	 */
	__u64 applied_seq;
};

#define S3FS_IOC_SYNC_ENTRIES \
	_IOW(S3FS_IOC_MAGIC, 0x04, struct s3fs_sync_entries)

/*
 * flush flags (E-03), in s3fs_req_event.flags of a FLUSH:
 *   FLUSH_FLAG_WAIT — fsync() caller is blocked on signal_flush.
 *                     daemon must PUT immediately (no coalescing).
 * A .create fop enqueues an empty flush (size=0) so `touch` results in a
 * zero-byte PUT.  The event carries the file's name (basename in the mount) so
 * the daemon can resolve it back to an S3 key even for freshly-created inos
 * that haven't hit an ino allocator yet.
 */
#define S3FS_FLUSH_FLAG_WAIT 0x1U

/*
 * S3FS_IOC_READ_DIRTY — pull `size` bytes from kmod's per-inode
 * scratch buffer into the daemon's `uaddr` for PUT to S3.
 *
 * The buffer must hold the whole content: a shorter file copies fd->size
 * bytes, a larger one is refused with -ENOSPC (AC-71) -- nothing is copied,
 * the flush is not claimed (W1) and `out_bytes` is the size to ask for. The
 * kmod validates `uaddr` via copy_to_user; caller must have the buffer
 * allocated in its own mm. Returns 0 on success and sets `out_bytes` to the
 * actual copy size.
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
 *   [3]  s3fs_read_iter_calls — .read_iter calls (all reads; was the GUP
 *                              call count)
 *   [4]  s3fs_read_iter_pos0 — .read_iter calls at ki_pos == 0 (first read
 *                              per open; was the GUP latency)
 *   [14] s3fs_hits_replayed_total — hit_accum: accesses counted in
 *                              .read_iter and replayed into the policy
 *                              (before the hit_replay_max cut)
 *   [15] s3fs_evseq          — AC-85: the highest seq given to a flush /
 *                              rename / delete event so far.  A sync reads it,
 *                              handles every queued event up to it, and reports
 *                              that as "applied up to" (survives a daemon
 *                              restart: kmod's counter keeps counting).
 */
#define S3FS_STATS_MAX 16
struct s3fs_stats {
	__u32 count;             /* in: number of slots caller allocated */
	__u32 filled;            /* out: number of slots kmod wrote */
	__u64 values[S3FS_STATS_MAX];
};

#define S3FS_IOC_STATS \
	_IOWR(S3FS_IOC_MAGIC, 0x08, struct s3fs_stats)

/* nr 0x09 was DRAIN_EVICTIONS (the daemon no longer keeps an admit snapshot
 * that a kmod eviction could oscillate with); do not reuse. */

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

/* nr 0x05 POP_FLUSH, 0x0B POP_DELETE, 0x0C POP_RENAME, 0x0D POP_FLUSH_WAIT, 0x11 POP_WB:
 * replaced by read(2) of struct s3fs_req_event; do not reuse. */

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
 * After the copy the chunks are marked populated: a read of them is a hit
 * (served from the fd by .read_folio into the page cache) and the capacity pass
 * counts the inode.
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
