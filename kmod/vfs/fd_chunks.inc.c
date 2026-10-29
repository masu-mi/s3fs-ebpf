/* SPDX-License-Identifier: GPL-2.0 */
/* per-inode content buffer (4 MiB chunk table).  Part of vfs_ops.c: included from there, one translation
 * unit, never compiled on its own. */

/* -- Per-inode content buffer -------------------------------------------- */

/* S11: daemon always fetches in 4 MiB chunks (partialAdmitChunk=4MiB=2^22). */
#define S3FS_PCACHE_CHUNK_SHIFT	22u
#define S3FS_CHUNK_SIZE		(1UL << S3FS_PCACHE_CHUNK_SHIFT)
/* Upper bound on the chunk table (1 TiB of file offset): keeps a bogus
 * populate/write offset from asking for a huge table. */
#define S3FS_MAX_CHUNKS		(1u << 18)

/* AC-45: fd content is a table of 4 MiB chunks instead of one contiguous
 * buffer, so only the chunks a reader touched are allocated (a tail read
 * of a 9 MiB file costs its chunk, not 9 MiB) and growth never copies
 * more than one chunk.
 *
 * Invariants (all under fd->lock):
 *  - chunk ci covers file bytes [ci << SHIFT, (ci + 1) << SHIFT).
 *  - p == NULL means "not allocated": a hole that reads as zeros while the
 *    file is dirty/flushing, and "not cached" otherwise.
 *  - bytes of an allocated chunk at or beyond fd->size are zero.
 *  - fd->cap == sum of chunks[].cap, and s3fs_fd_buf_bytes moves with it
 *    (M1): only s3fs_fd_chunk_ensure / s3fs_fd_chunk_free change either. */
struct s3fs_chunk {
	u8   *p;
	u32   cap;       /* allocated bytes, <= S3FS_CHUNK_SIZE */
	bool  populated; /* S11: holds valid S3 data (T2 uses it for every ci) */
	/* AC-51: only in a dirty fd.  The chunk is unchanged from the object
	 * in S3 and not held here (p may be stale or NULL); it reads through a
	 * load upcall and flushes as a server-side copy.  Counted in
	 * fd->nr_remote; cleared by a store, a free and a successful flush. */
	bool  remote;
	/* U8 (per chunk): pcache readers in the upcall retry loop waiting on
	 * this chunk.  While > 0 s3fs_file_data_reset keeps the chunk, so the
	 * data-based fill still finds what the daemon populated for them even
	 * after the PCACHE entry was evicted.  Survives s3fs_fd_chunk_free so
	 * the pin/unpin counts stay balanced. */
	u32   readers;
};

struct s3fs_hit_acc {
	u64 count;     /* accesses counted on this CPU (monotonic) */
	u64 last_tick; /* jiffies of the last one */
};

struct s3fs_file_data {
	struct mutex lock;
	struct s3fs_chunk *chunks;
	u32   nr_chunks;  /* length of the chunks table (entries may be empty) */
	size_t size;
	size_t cap;
	bool  dirty;    /* set by .write, cleared after successful daemon flush */
	bool  flushing; /* flush enqueued; cleared by signal_flush on PUT ACK */
	u32   nr_remote; /* AC-51: chunks with .remote set */
	/* Issue #22 (hit_accum).  hit is allocated with the fd; hit_flushed
	 * and policy_seen are only touched under s3fs_policy_evict_lock. */
	struct s3fs_hit_acc __percpu *hit;
	u64   hit_base;    /* sum of counts when populate admitted the ino */
	bool  hit_base_set;
	u64   hit_flushed; /* sum of counts already replayed to the policy */
	bool  policy_seen; /* policy has on_insert-ed this ino */
	/* U8 is per chunk now (struct s3fs_chunk.readers). */
	/* W1: unflushed-data pin.  maybe_enqueue_flush takes one inode ref
	 * (ihold) per fd while a flush is outstanding so evict_inode cannot
	 * free fd->buf before the daemon has read and PUT it (drop_caches or
	 * memory pressure after close would otherwise lose the write).
	 * flush_seq counts enqueues; read_dirty snapshots it into
	 * flush_read_seq.  signal_flush releases the pin only when the PUT
	 * succeeded and covered the latest enqueue (seqs equal, !dirty).
	 * Pinned iff pin_link is on s3fs_flush_pins (s3fs_flush_pins_lock);
	 * the seqs are protected by fd->lock. */
	u64   flush_seq;
	u64   flush_read_seq;
	struct list_head pin_link;
	struct inode *pin_inode;
};

/* Grow the chunk table so index ci is valid.  Caller holds fd->lock. */
static int s3fs_fd_chunks_grow(struct s3fs_file_data *fd, u32 ci)
{
	struct s3fs_chunk *nt;
	u32 n;

	if (ci < fd->nr_chunks)
		return 0;
	if (ci >= S3FS_MAX_CHUNKS)
		return -EFBIG;
	n = clamp_t(u32, max(ci + 1, fd->nr_chunks * 2), 8, S3FS_MAX_CHUNKS);
	nt = kvcalloc(n, sizeof(*nt), GFP_KERNEL);
	if (!nt)
		return -ENOMEM;
	if (fd->chunks)
		memcpy(nt, fd->chunks, fd->nr_chunks * sizeof(*nt));
	kvfree(fd->chunks);
	fd->chunks = nt;
	fd->nr_chunks = n;
	return 0;
}

/* Make chunk ci at least `want` bytes (<= S3FS_CHUNK_SIZE).  The caller is
 * about to overwrite [lo, hi) of the chunk, so only the rest of the new
 * space is zeroed (a freshly populated chunk is not memset and then copied
 * over).  *old_cap is the previous allocation (0 if none).  This and
 * s3fs_fd_chunk_free are the only places that change fd->cap and
 * s3fs_fd_buf_bytes (M1).  Caller holds fd->lock. */
static int s3fs_fd_chunk_ensure(struct s3fs_file_data *fd, u32 ci, u32 want,
				u32 lo, u32 hi, u32 *old_cap)
{
	struct s3fs_chunk *c;
	u32 oc;
	u8 *np;
	int err;

	err = s3fs_fd_chunks_grow(fd, ci);
	if (err)
		return err;
	c = &fd->chunks[ci];
	oc = c->p ? c->cap : 0;
	*old_cap = oc;
	if (oc >= want)
		return 0;
	np = kvmalloc(want, GFP_KERNEL);
	if (!np)
		return -ENOMEM;
	if (oc)
		memcpy(np, c->p, oc);
	if (lo > oc)
		memset(np + oc, 0, min(lo, want) - oc);
	if (hi < want)
		memset(np + max(hi, oc), 0, want - max(hi, oc));
	kvfree(c->p);
	atomic_long_add(want - oc, &s3fs_fd_buf_bytes);
	fd->cap += want - oc;
	c->p = np;
	c->cap = want;
	return 0;
}

/* Release chunk ci (data, accounting and populated flag).  fd->lock held. */
static void s3fs_fd_chunk_free(struct s3fs_file_data *fd, u32 ci)
{
	struct s3fs_chunk *c;

	if (ci >= fd->nr_chunks)
		return;
	c = &fd->chunks[ci];
	if (c->remote)
		fd->nr_remote--;
	if (c->p) {
		atomic_long_sub(c->cap, &s3fs_fd_buf_bytes);
		fd->cap -= c->cap;
		kvfree(c->p);
	}
	{
		u32 readers = c->readers;

		memset(c, 0, sizeof(*c));
		c->readers = readers; /* U8: pin/unpin stay balanced */
	}
}

/* U8: pin chunk ci for a reader about to wait on the daemon.  Needs a table
 * entry, so it can fail on allocation (the reader then runs unpinned, as
 * readers did before U8).  fd->lock held. */
static bool s3fs_fd_chunk_pin(struct s3fs_file_data *fd, u32 ci)
{
	if (s3fs_fd_chunks_grow(fd, ci))
		return false;
	fd->chunks[ci].readers++;
	return true;
}

static void s3fs_fd_chunk_unpin(struct s3fs_file_data *fd, u32 ci)
{
	if (ci < fd->nr_chunks && fd->chunks[ci].readers)
		fd->chunks[ci].readers--;
}

/* Release every chunk and the table.  Leaves fd->size alone.  fd->lock held
 * (or the fd is unreachable). */
static void s3fs_fd_free_all(struct s3fs_file_data *fd)
{
	u32 ci;

	for (ci = 0; ci < fd->nr_chunks; ci++)
		s3fs_fd_chunk_free(fd, ci);
	kvfree(fd->chunks);
	fd->chunks = NULL;
	fd->nr_chunks = 0;
}

/* Copy [off, off + len) into the chunks from `kbuf` (kernel) or `ubuf`
 * (user); exactly one is non-NULL.  The caller checked off + len for
 * overflow.  Chunks are allocated as needed:
 *  - alloc_limit != 0 (populate, initial content): a missing chunk gets
 *    exactly min(S3FS_CHUNK_SIZE, alloc_limit - chunk start) bytes, so a
 *    46 B object costs 46 B of capacity (AC-41) and a tail read costs only
 *    its chunk (AC-45).  alloc_limit is max(populate end, i_size).
 *  - alloc_limit == 0 (write): grow by doubling within the chunk, so
 *    sequential writes copy at most one chunk per growth (cf. AC-46).
 * On -EFAULT the part of the new memory that was not copied is zeroed, so
 * the "zero at or beyond fd->size" invariant survives a failed copy.
 * Caller holds fd->lock. */
static int s3fs_fd_store(struct s3fs_file_data *fd, u64 off, const void *kbuf,
			 const char __user *ubuf, u64 len, u64 alloc_limit)
{
	u64 pos = off, end = off + len;

	while (pos < end) {
		u32 seg_off, seg_len, want, cur, old_cap;
		struct s3fs_chunk *c;
		u64 cs;
		u32 ci;
		int err;

		if ((pos >> S3FS_PCACHE_CHUNK_SHIFT) >= S3FS_MAX_CHUNKS)
			return -EFBIG;
		ci = (u32)(pos >> S3FS_PCACHE_CHUNK_SHIFT);
		cs = (u64)ci << S3FS_PCACHE_CHUNK_SHIFT;
		seg_off = (u32)(pos - cs);
		seg_len = (u32)min_t(u64, S3FS_CHUNK_SIZE - seg_off, end - pos);

		if (alloc_limit) {
			want = (u32)min_t(u64, S3FS_CHUNK_SIZE, alloc_limit - cs);
		} else {
			cur = ci < fd->nr_chunks && fd->chunks[ci].p ?
			      fd->chunks[ci].cap : 0;
			want = max_t(u32, round_up(seg_off + seg_len, 4096),
				     min_t(u32, S3FS_CHUNK_SIZE, cur * 2));
		}
		want = max_t(u32, want, seg_off + seg_len);

		err = s3fs_fd_chunk_ensure(fd, ci, want, seg_off,
					   seg_off + seg_len, &old_cap);
		if (err)
			return err;
		c = &fd->chunks[ci];
		if (c->remote) {
			c->remote = false;
			fd->nr_remote--;
		}
		if (kbuf) {
			memcpy(c->p + seg_off, (const u8 *)kbuf + (pos - off),
			       seg_len);
		} else {
			unsigned long rem = copy_from_user(c->p + seg_off,
							   ubuf + (pos - off),
							   seg_len);

			if (rem) {
				u32 zfrom = max(seg_off + seg_len - (u32)rem,
						old_cap);

				if (zfrom < seg_off + seg_len)
					memset(c->p + zfrom, 0,
					       seg_off + seg_len - zfrom);
				return -EFAULT;
			}
		}
		pos += seg_len;
	}
	return 0;
}

/* Copy the first n bytes of the file to user space; missing chunks and
 * the unallocated tail of a short chunk read as zeros.  fd->lock held. */
static long s3fs_fd_copy_out_at(struct s3fs_file_data *fd, char __user *uaddr,
				size_t base, size_t n)
{
	size_t pos = 0;

	while (pos < n) {
		size_t fpos = base + pos;
		u32 ci = (u32)(fpos >> S3FS_PCACHE_CHUNK_SHIFT);
		u32 off = (u32)(fpos & (S3FS_CHUNK_SIZE - 1));
		size_t seg = min_t(size_t, S3FS_CHUNK_SIZE - off, n - pos);
		size_t have = 0;

		if (ci < fd->nr_chunks && fd->chunks[ci].p &&
		    fd->chunks[ci].cap > off)
			have = min_t(size_t, seg, fd->chunks[ci].cap - off);
		if (have && copy_to_user(uaddr + pos, fd->chunks[ci].p + off,
					 have))
			return -EFAULT;
		if (seg > have && clear_user(uaddr + pos + have, seg - have))
			return -EFAULT;
		pos += seg;
	}
	return (long)n;
}

static long s3fs_fd_copy_out(struct s3fs_file_data *fd, char __user *uaddr,
			     size_t n)
{
	return s3fs_fd_copy_out_at(fd, uaddr, 0, n);
}

/* True if the 4 MiB chunk at chunk_offset of ino is allocated and populated
 * in its fd.  Takes fd->lock; call with no other lock held. */
static bool s3fs_ino_chunk_populated(u64 ino, u64 chunk_offset)
{
	struct inode *inode;
	struct s3fs_file_data *fd;
	bool ok = false;

	if (!s3fs_active_sb ||
	    (chunk_offset >> S3FS_PCACHE_CHUNK_SHIFT) >= S3FS_MAX_CHUNKS)
		return false;
	inode = ilookup(s3fs_active_sb, (unsigned long)ino);
	if (!inode)
		return false;
	fd = inode->i_private;
	if (fd) {
		u32 ci = (u32)(chunk_offset >> S3FS_PCACHE_CHUNK_SHIFT);

		mutex_lock(&fd->lock);
		ok = ci < fd->nr_chunks && fd->chunks[ci].p &&
		     fd->chunks[ci].populated;
		mutex_unlock(&fd->lock);
	}
	iput(inode);
	return ok;
}

/* AC-45 C3: free the idle chunks of an ino that has a miss in flight.
 *
 * The capacity pass cannot evict such an ino whole (its PCACHE entries and
 * the chunk being populated must survive until the waiting reader is
 * served), but it must not let a file read front to back keep every chunk
 * it has already served.  This frees every chunk no reader waits on
 * (readers == 0; a reader pins before it creates its pending) and drops the
 * PCACHE entries overlapping them first, so R1 holds.  Returns true if
 * anything was freed.  Takes fd->lock; call with no other lock held. */
static bool s3fs_ino_shed_unpinned(u64 ino)
{
	struct inode *inode;
	struct s3fs_file_data *fd;
	bool freed_any = false;
	u32 ci;

	if (!s3fs_active_sb)
		return false;
	inode = ilookup(s3fs_active_sb, (unsigned long)ino);
	if (!inode)
		return false;
	fd = inode->i_private;
	if (!fd) {
		iput(inode);
		return false;
	}

	mutex_lock(&fd->lock);
	if (fd->dirty || fd->flushing)
		goto out;
	for (ci = 0; ci < fd->nr_chunks; ci++) {
		struct s3fs_chunk *c = &fd->chunks[ci];
		u64 cs = (u64)ci << S3FS_PCACHE_CHUNK_SHIFT;
		u64 ce = cs + S3FS_CHUNK_SIZE;
		struct s3fs_admit *a;
		struct hlist_node *tmp;
		bool removed = false;

		if (!c->p || c->readers)
			continue;
		write_lock(&s3fs_admits_lock);
		hash_for_each_possible_safe(s3fs_admits, a, tmp, node, ino) {
			if (a->ino != ino || !(a->flags & S3FS_ADMIT_FLAG_PCACHE))
				continue;
			if (a->offset >= ce || a->offset + a->size <= cs)
				continue;
			hash_del(&a->node);
			kfree(a);
			atomic_dec(&s3fs_admits_count);
			removed = true;
		}
		if (removed)
			atomic_inc(&s3fs_admit_gen);
		write_unlock(&s3fs_admits_lock);
		s3fs_fd_chunk_free(fd, ci);
		/* The page cache must not keep serving what capacity dropped
		 * (cf. s3fs_evict_page_cache; trylock-based, so no deadlock with
		 * a reader that holds a folio lock and waits for fd->lock). */
		invalidate_mapping_pages(inode->i_mapping, cs >> PAGE_SHIFT,
					 (ce - 1) >> PAGE_SHIFT);
		freed_any = true;
	}
out:
	mutex_unlock(&fd->lock);
	iput(inode);
	return freed_any;
}

/* Cut the file down to new_size (< fd->size): free the chunks wholly past
 * it and zero the tail of the boundary chunk, so the "zero beyond size"
 * invariant holds when the file is extended again.  fd->lock held. */
static void s3fs_fd_truncate(struct s3fs_file_data *fd, size_t new_size)
{
	u32 ci = (u32)(new_size >> S3FS_PCACHE_CHUNK_SHIFT);
	u32 off = (u32)(new_size & (S3FS_CHUNK_SIZE - 1));
	u32 i;

	if (off && ci < fd->nr_chunks && fd->chunks[ci].p &&
	    fd->chunks[ci].cap > off)
		memset(fd->chunks[ci].p + off, 0, fd->chunks[ci].cap - off);
	for (i = off ? ci + 1 : ci; i < fd->nr_chunks; i++)
		s3fs_fd_chunk_free(fd, i);
}

/* True when fd->buf is owned by the write/flush path and the arena may
 * hold stale prewarm content.  Lockless READ_ONCE is intentional: a
 * transient false negative just causes an unnecessary arena skip (miss),
 * never a stale-data read. */
static inline bool s3fs_fd_buf_owned(struct inode *inode)
{
	struct s3fs_file_data *fd = inode->i_private;

	return fd && (READ_ONCE(fd->dirty) || READ_ONCE(fd->flushing));
}

/* AC-56: grow i_size to @end unless write/flush owns the size.  write and
 * truncate change dirty and i_size under fd->lock, so the ownership check and
 * the i_size_write happen under it too — otherwise a truncate between the two
 * is undone.  fd->lock is the outer lock (the caller holds no admits_lock). */
static void s3fs_inode_grow_size_unowned(struct inode *inode, u64 end)
{
	struct s3fs_file_data *fd = inode->i_private;

	if (fd)
		mutex_lock(&fd->lock);
	if (!s3fs_fd_buf_owned(inode) && end > (u64)i_size_read(inode))
		i_size_write(inode, end);
	if (fd)
		mutex_unlock(&fd->lock);
}

/* AC-41: bytes actually allocated for ino's fd->buf.  The capacity loop
 * charges pcache inos by this instead of their admitted chunk sizes:
 * s3fs_file_data_reserve sizes fd->buf to at least the populate end (and,
 * AC-46, up to 2x cap bounded by the file size), so one tail chunk admits
 * 4 MiB but allocates the whole file.  Lockless read —
 * accounting only.  Must be called without s3fs_admits_lock held (iput). */
static u64 s3fs_ino_buf_bytes(u64 ino)
{
	struct inode *inode;
	struct s3fs_file_data *fd;
	u64 cap = 0;

	if (!s3fs_active_sb)
		return 0;
	inode = ilookup(s3fs_active_sb, (unsigned long)ino);
	if (!inode)
		return 0;
	fd = inode->i_private;
	if (fd)
		cap = READ_ONCE(fd->cap);
	iput(inode);
	return cap;
}

