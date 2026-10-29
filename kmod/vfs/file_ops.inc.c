/* SPDX-License-Identifier: GPL-2.0 */
/* regular file ops: partial modify, write, setattr.  Part of vfs_ops.c: included from there, one translation
 * unit, never compiled on its own. */

/* -- File ops (regular files) --------------------------------------------- */

/* How long .read_folio waits for a daemon upcall to admit the ino
 * before falling back to the compile-time kmalloc buffer. */
#define S3FS_UPCALL_WAIT_JIFFIES  (2 * HZ)

/*
 * Range-aware admit snapshot (D-01). Finds the admit entry that
 * covers file offset `pos` and returns arena bytes relative to it:
 *
 *   *out_uaddr    daemon user_vm address of `pos` within the range
 *   *out_avail    bytes remaining in the range starting at `pos`
 *
 * Callers pass `pos` as the folio's file offset. On hit, they can
 * read up to `*out_avail` bytes from `*out_uaddr` via GUP. When
 * the daemon still uses whole-file admits (offset=0, size=file_size)
 * this collapses to the legacy behavior (`*out_uaddr = base + pos`,
 * `*out_avail = size - pos`).
 */
static bool admit_snapshot(u64 ino, u64 pos,
			   u64 *out_uaddr, u64 *out_avail,
			   struct mm_struct **out_mm)
{
	struct s3fs_admit *a;
	bool hit = false;

	read_lock(&s3fs_admits_lock);
	a = admit_lookup_at_locked(ino, pos);
	/* S10: pcache entries have uaddr=0 — their data lives in fd->buf, not
	 * the daemon's arena.  Returning true here with uaddr≈0 causes
	 * fill_from_arena to call GUP on a near-NULL daemon address, which
	 * silently zero-fills the folio and marks it uptodate, bypassing the
	 * upcall retry path that would correctly re-populate fd->buf. */
	if (a && !(a->flags & S3FS_ADMIT_FLAG_PCACHE)) {
		u64 within = pos - a->offset;
		*out_uaddr = a->uaddr + within;
		*out_avail = a->size - within;
		*out_mm    = s3fs_admitted_mm;
		hit = true;
	}
	read_unlock(&s3fs_admits_lock);
	return hit;
}

/* AC-50: attempts per chunk before giving up with -EIO (each waits up to
 * S3FS_UPCALL_WAIT_JIFFIES for the daemon). */
#define S3FS_LOAD_MAX_TRIES	6

/* AC-51: what a modification of the file does, for the loader.  Callers
 * describe it; s3fs_mop_prepare adapts it to the fd's current state. */
struct s3fs_mop {
	u64  end;	/* [0, end) of the current content must survive */
	u64  ow_lo, ow_hi; /* chunks inside this range are overwritten completely */
	u64  t_lo, t_hi;   /* bytes whose new value the change decides */
	bool partial;	/* chunks outside [t_lo, t_hi) may stay remote */
};

static bool s3fs_partial_wanted(struct inode *inode)
{
	return READ_ONCE(s3fs_partial_modify) &&
	       (u64)i_size_read(inode) >= READ_ONCE(s3fs_partial_modify_min);
}

/* The chunks [first, last] a partial op can need data for; with !partial,
 * every chunk of [0, end).  false: none. */
static bool s3fs_mop_range(const struct s3fs_mop *op, u32 *first, u32 *last)
{
	u64 nci = DIV_ROUND_UP(op->end, S3FS_CHUNK_SIZE);
	u64 lo = 0, hi;

	if (!nci)
		return false;
	hi = nci - 1;
	if (op->partial) {
		if (op->t_hi <= op->t_lo)
			return false;
		lo = max(lo, op->t_lo >> S3FS_PCACHE_CHUNK_SHIFT);
		hi = min(hi, (op->t_hi - 1) >> S3FS_PCACHE_CHUNK_SHIFT);
		if (lo > hi)
			return false;
	}
	*first = (u32)lo;
	*last = (u32)hi;
	return true;
}

/* Does the op need chunk ci to hold the object's bytes?  Not when it
 * overwrites the chunk completely, and in partial mode not when the change
 * does not reach into it. */
static bool s3fs_mop_needs_data(const struct s3fs_mop *op, u32 ci)
{
	u64 cs = (u64)ci << S3FS_PCACHE_CHUNK_SHIFT;
	u64 ce = min_t(u64, cs + S3FS_CHUNK_SIZE, op->end);

	if (cs >= op->end)
		return false;
	if (op->ow_lo <= cs && op->ow_hi >= ce)
		return false;
	if (op->partial &&
	    (cs + S3FS_CHUNK_SIZE <= op->t_lo || cs >= op->t_hi))
		return false;
	return true;
}

/* fd->lock held.  Adapts the caller's op to the fd: end is limited to the
 * object size; a dirty fd only ever needs its remote chunks.  false: there is
 * nothing for the loader to do. */
static bool s3fs_mop_prepare(struct inode *inode, struct s3fs_file_data *fd,
			     const struct s3fs_mop *in, struct s3fs_mop *op)
{
	*op = *in;
	op->end = min_t(u64, in->end, (u64)i_size_read(inode));
	if (fd->dirty || fd->flushing) {
		if (!fd->nr_remote)
			return false;
		op->partial = true;
	}
	return op->end != 0;
}

/* fd->lock held: chunk ci holds what the op needs of it.  A clean fd must
 * have it populated; a dirty fd owns its content, apart from remote chunks. */
static bool s3fs_mop_chunk_ready(const struct s3fs_file_data *fd, u32 ci)
{
	const struct s3fs_chunk *c = ci < fd->nr_chunks ? &fd->chunks[ci] : NULL;

	if (fd->dirty || fd->flushing)
		return !(c && c->remote);
	return c && c->p && c->populated;
}

/* AC-50: pins held on the chunks a modification needs, from the start of
 * the load until the caller has applied the change under fd->lock.  A pinned
 * chunk is not freed by reset / shed (readers != 0), so what the load put in
 * stays until the fd is dirty and owns the content.  Unpinning between the
 * load and the write let a capacity pass free the chunk again, and a file
 * larger than capacity_bytes could never hold all its chunks that way.
 * Pins exactly the chunks s3fs_mop_needs_data() selects. */
struct s3fs_mpin {
	struct s3fs_mop op;
	bool held;
};

/* fd->lock held. */
static void s3fs_mpin_release_locked(struct s3fs_file_data *fd,
				     struct s3fs_mpin *mp)
{
	u32 ci, first, last;

	if (!mp->held)
		return;
	if (s3fs_mop_range(&mp->op, &first, &last))
		for (ci = first; ci <= last; ci++)
			if (s3fs_mop_needs_data(&mp->op, ci))
				s3fs_fd_chunk_unpin(fd, ci);
	mp->held = false;
}

static void s3fs_mpin_release(struct s3fs_file_data *fd, struct s3fs_mpin *mp)
{
	mutex_lock(&fd->lock);
	s3fs_mpin_release_locked(fd, mp);
	mutex_unlock(&fd->lock);
}

/* fd->lock held.  Returns 0 with the pins held, or -errno with none. */
static int s3fs_mpin_acquire_locked(struct s3fs_file_data *fd,
				    struct s3fs_mpin *mp,
				    const struct s3fs_mop *op)
{
	u32 ci, first, last;

	mp->op = *op;
	mp->held = true;
	if (!s3fs_mop_range(op, &first, &last))
		return 0;
	for (ci = first; ci <= last; ci++) {
		if (!s3fs_mop_needs_data(op, ci))
			continue;
		if (!s3fs_fd_chunk_pin(fd, ci)) {
			/* release what was pinned so far: chunks below ci */
			mp->op.end = (u64)ci << S3FS_PCACHE_CHUNK_SHIFT;
			s3fs_mpin_release_locked(fd, mp);
			return -ENOMEM;
		}
	}
	return 0;
}

/* AC-50: true when, with fd->lock held, a modification may be applied: every
 * chunk the op needs holds the object's bytes (clean fd: populated; dirty fd:
 * not remote).  s3fs_fd_load_for_modify returns with those chunks still
 * pinned (struct s3fs_mpin), so a shed / evict -> reset cannot free them
 * between the load and the change.  The pins are not enough on their own,
 * though: a loader can return with a short set (a concurrent reset before it
 * pinned, a partial load).  Callers therefore check here under fd->lock, keep
 * that lock through s3fs_mpin_release_locked, the change and the dirty mark,
 * and go back to loading when the check fails. */
static bool s3fs_fd_modify_ready_locked(struct inode *inode,
					struct s3fs_file_data *fd,
					const struct s3fs_mop *in)
{
	struct s3fs_mop op;
	u32 ci, first, last;

	if (!s3fs_mop_prepare(inode, fd, in, &op) ||
	    !s3fs_mop_range(&op, &first, &last))
		return true;
	for (ci = first; ci <= last; ci++)
		if (s3fs_mop_needs_data(&op, ci) &&
		    !s3fs_mop_chunk_ready(fd, ci))
			return false;
	return true;
}

/* AC-51: fd->lock held, the fd clean and s3fs_fd_modify_ready_locked() true.
 * In partial mode, the chunks below op->end that the change neither needs nor
 * overwrites and that the fd does not hold become remote: the object keeps
 * them and the flush copies them.  Done before the change is applied, so a
 * failure leaves the fd as it was. */
static int s3fs_fd_mark_remote_locked(struct inode *inode,
				      struct s3fs_file_data *fd,
				      const struct s3fs_mop *in)
{
	struct s3fs_mop op;
	u32 nci, ci;
	int err;

	if (fd->dirty || fd->flushing)
		return 0;
	if (!s3fs_mop_prepare(inode, fd, in, &op) || !op.partial)
		return 0;
	nci = (u32)DIV_ROUND_UP(op.end, S3FS_CHUNK_SIZE);
	err = s3fs_fd_chunks_grow(fd, nci - 1);
	if (err)
		return err;
	for (ci = 0; ci < nci; ci++) {
		u64 cs = (u64)ci << S3FS_PCACHE_CHUNK_SHIFT;
		u64 ce = min_t(u64, cs + S3FS_CHUNK_SIZE, op.end);
		struct s3fs_chunk *c = &fd->chunks[ci];

		if (op.ow_lo <= cs && op.ow_hi >= ce)
			continue;	/* the write fills it */
		if (s3fs_mop_needs_data(&op, ci))
			continue;	/* loaded */
		if (c->p && c->populated)
			continue;	/* held: flushed as data */
		if (!c->remote) {
			c->remote = true;
			fd->nr_remote++;
		}
	}
	return 0;
}

/* AC-50: make the fd hold the parts of an existing object that a modification
 * leaves intact, before the modification is applied.
 *
 * open(2) loads nothing and a flush PUTs the whole buffer, so the first
 * write / truncate on a file nobody read used to start from an empty buffer:
 * the object was truncated to what was written, a shrink produced zeros and
 * `: > f` never reached S3.  Only the clean -> dirty transition needs this
 * (afterwards the fd owns the content) -- except, AC-51, a dirty fd's remote
 * chunks, which a later change may need.
 *
 * Loads the chunks s3fs_mop_needs_data() selects that are not there.  A chunk
 * is fetched with an upcall flagged S3FS_MISS_FLAG_FOR_WRITE; the daemon fills
 * it with POPULATE_PAGES (also in arena mode) and signals with
 * S3FS_SIGNAL_VERIFY_POPULATED.  Same protocol as .read_folio: pin the chunk
 * first, then create the pending.  Returns -EIO when a chunk cannot be
 * loaded (daemon absent or failing): the write is refused rather than
 * flushed over the object.  Sleeps; call with no lock held. */
static int s3fs_fd_load_for_modify(struct inode *inode,
				   struct s3fs_file_data *fd,
				   const struct s3fs_mop *in,
				   struct s3fs_mpin *mp)
{
	struct s3fs_mop op;
	u32 ci, first, last;
	int err;

	mutex_lock(&fd->lock);
	s3fs_mpin_release_locked(fd, mp);
	if (!s3fs_mop_prepare(inode, fd, in, &op) ||
	    !s3fs_mop_range(&op, &first, &last)) {
		mutex_unlock(&fd->lock);
		return 0;
	}
	err = s3fs_mpin_acquire_locked(fd, mp, &op);
	mutex_unlock(&fd->lock);
	if (err)
		return err;

	for (ci = first; ci <= last; ci++) {
		u64 cs = (u64)ci << S3FS_PCACHE_CHUNK_SHIFT;
		int tries = 0;

		if (!s3fs_mop_needs_data(&op, ci))
			continue;
		for (;;) {
			struct s3fs_pending *p;
			struct mm_struct *mm;
			bool have, bound;
			long rem = 1;

			err = 0;
			mutex_lock(&fd->lock);
			have = s3fs_mop_chunk_ready(fd, ci);
			/* AC-51: a dirty / remote-holding fd depends on the pin. */
			bound = fd->dirty || fd->flushing || fd->nr_remote;
			mutex_unlock(&fd->lock);
			if (have)
				break;

			read_lock(&s3fs_admits_lock);
			mm = s3fs_admitted_mm;
			read_unlock(&s3fs_admits_lock);
			if (!mm) {
				err = -EIO; /* no daemon: cannot fetch the rest */
				p = NULL;
			} else {
				p = pending_get_or_create(inode->i_ino, cs);
				/* Count = what the chunk must hold up to i_size.
				 * The daemon's size for the object can be older
				 * (appended + flushed, entry not re-synced); with
				 * a shorter chunk it never becomes populated. */
				u64 isz = i_size_read(inode);
				u64 want = min_t(u64, S3FS_CHUNK_SIZE,
						 isz > cs ? isz - cs : 1);

				err = p ? miss_enqueue(inode->i_ino, cs, want,
						       S3FS_MISS_FLAG_FOR_WRITE |
						       (bound ? S3FS_MISS_FLAG_PINNED : 0))
					: -ENOMEM;
			}
			if (!err) {
				rem = wait_for_completion_interruptible_timeout(
					&p->done, S3FS_UPCALL_WAIT_JIFFIES);
				if (rem < 0)
					err = (int)rem;	/* -ERESTARTSYS */
				else if (rem > 0)
					err = p->err;	/* daemon-side failure */
			}
			if (p)
				pending_put(p);
			if (err == -ERESTARTSYS || err == -EINTR) {
				s3fs_mpin_release(fd, mp);
				return err;
			}
			if (err) {
				/* The daemon refused (it will not populate a
				 * chunk a concurrent writer already holds,
				 * -EBUSY): fine if the chunk is there now. */
				mutex_lock(&fd->lock);
				have = s3fs_mop_chunk_ready(fd, ci);
				mutex_unlock(&fd->lock);
				if (have)
					break;
				s3fs_mpin_release(fd, mp);
				return -EIO;
			}
			if (++tries >= S3FS_LOAD_MAX_TRIES) {
				s3fs_mpin_release(fd, mp);
				return -EIO;
			}
		}
	}
	return 0;
}

static ssize_t s3fs_ebpf_file_write(struct file *file,
				    const char __user *user_buf,
				    size_t count, loff_t *pos)
{
	struct inode *inode = file_inode(file);
	struct s3fs_file_data *fd = inode->i_private;
	struct s3fs_mpin mp = {};
	struct s3fs_mop op;
	loff_t start;
	int err, tries;

	if (!fd)
		return -EIO;
	if (!count)
		return 0;

	/* AC-50: the first modification of a clean file must start from its
	 * current content, not from an empty buffer.  Chunks the write covers
	 * completely are not fetched.  The loader leaves its chunks pinned
	 * and we confirm under fd->lock that they are all present; the lock
	 * is then held until the dirty mark, so nothing can free them.  If
	 * the check fails, drop the lock and load again. */
	for (tries = 0;; tries++) {
		loff_t lstart = (file->f_flags & O_APPEND) ?
				i_size_read(inode) : *pos;
		u64 isz = (u64)i_size_read(inode);

		op = (struct s3fs_mop){
			.end = isz,
			.ow_lo = (u64)max_t(loff_t, lstart, 0),
			.ow_hi = (u64)max_t(loff_t, lstart, 0) + count,
			.t_lo = min_t(u64, (u64)max_t(loff_t, lstart, 0), isz),
			.t_hi = (u64)max_t(loff_t, lstart, 0) + count,
			.partial = s3fs_partial_wanted(inode),
		};
		mutex_lock(&fd->lock);
		if (lstart < 0 || s3fs_fd_modify_ready_locked(inode, fd, &op))
			break;
		mutex_unlock(&fd->lock);
		if (tries >= S3FS_LOAD_MAX_TRIES)
			err = -EIO;
		else
			err = s3fs_fd_load_for_modify(inode, fd, &op, &mp);
		if (err) {
			s3fs_mpin_release(fd, &mp);
			return err;
		}
	}
	/* fd->lock is held from the check to the dirty mark below, so the pins
	 * are no longer needed. */
	s3fs_mpin_release_locked(fd, &mp);
	err = s3fs_fd_mark_remote_locked(inode, fd, &op);
	if (err) {
		mutex_unlock(&fd->lock);
		return err;
	}
	if (!fd->dirty && !fd->flushing)
		fd->size = max_t(size_t, fd->size, (size_t)i_size_read(inode));
	start = (file->f_flags & O_APPEND) ? (loff_t)fd->size : *pos;
	if (start < 0) {
		err = -EINVAL;
		goto out;
	}

	err = s3fs_fd_store(fd, (u64)start, NULL, user_buf, count, 0);
	if (err)
		goto out;
	if (start + count > (loff_t)fd->size) {
		fd->size = start + count;
		i_size_write(inode, fd->size);
	}
	*pos = start + count;
	fd->dirty = true;
#if LINUX_VERSION_CODE >= KERNEL_VERSION(6, 7, 0)
	inode_set_mtime_to_ts(inode, current_time(inode));
	inode_set_ctime_current(inode);
#else
	inode->i_mtime = inode->i_ctime = current_time(inode);
#endif
	mutex_unlock(&fd->lock);
	/* The page cache still holds the pre-write content of the range (it
	 * was filled by an earlier read); a later read would be served from
	 * it and miss this write.  Drop those folios so .read_folio refills
	 * them from the fd.  After the unlock: .read_folio takes fd->lock
	 * under the folio lock that invalidate waits for.  Folios that cannot
	 * be dropped (mmap'ed) stay; same as before. */
	invalidate_inode_pages2_range(inode->i_mapping,
				      start >> PAGE_SHIFT,
				      (start + count - 1) >> PAGE_SHIFT);
	return count;
out:
	mutex_unlock(&fd->lock);
	return err;
}

static int s3fs_ebpf_file_setattr(struct mnt_idmap *idmap,
				  struct dentry *dentry, struct iattr *attr)
{
	struct inode *inode = d_inode(dentry);
	struct s3fs_file_data *fd = inode->i_private;
	int err;

	err = setattr_prepare(idmap, dentry, attr);
	if (err)
		return err;

	if ((attr->ia_valid & ATTR_SIZE) && fd) {
		loff_t new_size = attr->ia_size;
		bool owned;
		struct s3fs_mpin mp = {};
		struct s3fs_mop op;
		u64 cur;
		int tries;

		/* AC-50: a clean file's size is the object's (i_size); fd->size
		 * only covers what was fetched.  Changing it must start from the
		 * content that survives, and even a truncate that fetches
		 * nothing (`: > f`) has to mark the file dirty so it is PUT. */
		for (tries = 0;; tries++) {
			mutex_lock(&fd->lock);
			owned = fd->dirty || fd->flushing;
			cur = owned ? (u64)fd->size : (u64)i_size_read(inode);
			op = (struct s3fs_mop){
				.end = min_t(u64, new_size, cur),
				.t_lo = min_t(u64, new_size, cur),
				.t_hi = max_t(u64, new_size, cur),
				.partial = s3fs_partial_wanted(inode),
			};
			if ((u64)new_size == cur ||
			    s3fs_fd_modify_ready_locked(inode, fd, &op))
				break;
			mutex_unlock(&fd->lock);
			if (tries >= S3FS_LOAD_MAX_TRIES)
				err = -EIO;
			else
				err = s3fs_fd_load_for_modify(inode, fd, &op, &mp);
			if (err) {
				s3fs_mpin_release(fd, &mp);
				return err;
			}
		}
		/* fd->lock stays held until the size change is applied. */
		s3fs_mpin_release_locked(fd, &mp);
		if ((u64)new_size != cur) {
			err = s3fs_fd_mark_remote_locked(inode, fd, &op);
			if (err) {
				mutex_unlock(&fd->lock);
				return err;
			}
		}
		if (!fd->dirty && !fd->flushing && (u64)new_size != cur)
			fd->size = max_t(size_t, fd->size, (size_t)cur);
		if (new_size < (loff_t)fd->size) {
			/* AC-45: free the chunks past the cut and zero the
			 * boundary tail; an extension later reads zeros, not
			 * the bytes that were cut off. */
			s3fs_fd_truncate(fd, new_size);
			fd->size = new_size;
			fd->dirty = true;
		} else if (new_size > (loff_t)fd->size) {
			/* Sparse extension: the new range is a hole (no chunk
			 * allocated) that reads as zeros while dirty. */
			if (((u64)new_size >> S3FS_PCACHE_CHUNK_SHIFT) >=
			    S3FS_MAX_CHUNKS) {
				mutex_unlock(&fd->lock);
				return -EFBIG;
			}
			fd->size = new_size;
			fd->dirty = true;
		}
		i_size_write(inode, new_size);
		mutex_unlock(&fd->lock);
		/* Folios cached before the size change hold the old tail (or
		 * the pre-truncate bytes of a later extension).  After the
		 * unlock for the same reason as in .write. */
		if ((u64)new_size != cur)
			truncate_pagecache(inode, new_size);
	}
	setattr_copy(idmap, inode, attr);

	/* G-02: record mode / uid / gid so a re-lookup after icache
	 * eviction restores what chmod / chown just set. Without this
	 * `chmod 755 x; sysctl -w vm.drop_caches=2; stat x` regresses
	 * to the bootstrap mode. */
	if (attr->ia_valid & (ATTR_MODE | ATTR_UID | ATTR_GID)) {
		u32 valid = 0;
		if (attr->ia_valid & ATTR_MODE)
			valid |= S3FS_ATTR_OV_MODE;
		if (attr->ia_valid & ATTR_UID)
			valid |= S3FS_ATTR_OV_UID;
		if (attr->ia_valid & ATTR_GID)
			valid |= S3FS_ATTR_OV_GID;
		attr_override_upsert(inode->i_ino, valid,
				     attr->ia_mode, attr->ia_uid, attr->ia_gid);
	}
	mark_inode_dirty(inode);
	return 0;
}

