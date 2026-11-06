/* SPDX-License-Identifier: GPL-2.0 */
/* fd buffer audit, flush pins, s3fs_file_data_reset.  Part of vfs_ops.c: included from there, one translation
 * unit, never compiled on its own. */


/* The same set as s3fs_collect_resident, found the slow way: walking the
 * superblock's inodes (O(inodes of the sb); the Phase 3c implementation).  Used
 * only by the audit, as the ground truth the resident index is checked against.  *out gets a kmalloc'ed array (caller kfree), the count is
 * returned; each element carries the bytes fd->cap charges (AC-41).
 *
 * Reads fd->cap / dirty / flushing without fd->lock under i_lock (a snapshot: a
 * write or an evict racing it only makes the pass see the inode one round
 * early or late).  i_private is read under i_lock because evict frees fd only
 * after I_FREEING is set, which is checked under the same lock. */
static int s3fs_collect_resident_walk(struct s3fs_resident **out)
{
	struct super_block *sb = s3fs_active_sb;
	struct s3fs_resident *r;
	struct inode *inode;
	int cap, n;

	*out = NULL;
	if (!sb)
		return 0;
	for (;;) {
		cap = 0;
		spin_lock(&sb->s_inode_list_lock);
		list_for_each_entry(inode, &sb->s_inodes, i_sb_list)
			cap++;
		spin_unlock(&sb->s_inode_list_lock);
		if (!cap)
			return 0;
		cap += 16;	/* inodes that appear meanwhile */
		r = kmalloc_array(cap, sizeof(*r), GFP_KERNEL);
		if (!r)
			return 0;
		n = 0;
		spin_lock(&sb->s_inode_list_lock);
		list_for_each_entry(inode, &sb->s_inodes, i_sb_list) {
			struct s3fs_file_data *fd;

			spin_lock(&inode->i_lock);
			if (!(inode->i_state & (I_FREEING | I_WILL_FREE | I_NEW)) &&
			    S_ISREG(inode->i_mode) && n < cap) {
				fd = inode->i_private;
				if (fd && READ_ONCE(fd->cap) > 0 &&
				    !READ_ONCE(fd->dirty) && !READ_ONCE(fd->flushing)) {
					r[n].ino = inode->i_ino;
					r[n].bytes = READ_ONCE(fd->cap);
					n++;
				}
			}
			spin_unlock(&inode->i_lock);
		}
		spin_unlock(&sb->s_inode_list_lock);
		*out = r;
		return n;
	}
}

/* The inodes whose content the cache holds: clean regular files with allocated
 * chunks (fd->cap > 0), from the resident index (s3fs_resident_list, kept by
 * s3fs_fd_chunk_ensure / _free).  O(cached inodes), whatever the number of
 * inodes the sb has.  *out gets a kmalloc'ed array (caller kfree), the count is
 * returned; each element carries the bytes fd->cap charges (AC-41).
 *
 * Takes only s3fs_resident_lock (a spinlock, innermost), no fd->lock: fd->cap /
 * dirty / flushing are read with READ_ONCE (a snapshot: a write or an evict
 * racing it only makes the pass see the inode one round early or late).  An fd
 * is unlinked (cap == 0) before it is freed, so the fds on the list are alive
 * while the lock is held.  Dirty / flushing fds stay on the list but are not
 * returned. */
static int s3fs_collect_resident(struct s3fs_resident **out)
{
	struct s3fs_file_data *fd;
	struct s3fs_resident *r;
	int cap, n;
	bool more;

	*out = NULL;
	do {
		cap = atomic_read(&s3fs_resident_nodes);
		if (!cap)
			return 0;
		cap += 16;	/* fds that appear meanwhile */
		r = kmalloc_array(cap, sizeof(*r), GFP_KERNEL);
		if (!r)
			return 0;
		n = 0;
		more = false;
		spin_lock(&s3fs_resident_lock);
		list_for_each_entry(fd, &s3fs_resident_list, res_link) {
			if (n >= cap) {
				more = true;
				break;
			}
			if (!READ_ONCE(fd->dirty) && !READ_ONCE(fd->flushing)) {
				r[n].ino = fd->ino;
				r[n].bytes = READ_ONCE(fd->cap);
				n++;
			}
		}
		spin_unlock(&s3fs_resident_lock);
		if (more)
			kfree(r);
	} while (more);
	*out = r;
	return n;
}

/* M2: cross-check for the M1 gauge, exported via IOC_STATS slots 12/13.
 *   *walk   = sum of fd->cap over every live regular inode on the sb.  The
 *             gauge is maintained incrementally, so gauge != walk at rest
 *             means a cap change that skipped the gauge.
 *   *pcache = sum of fd->cap over the capacity candidates (what the AC-41
 *             capacity loop charges).  walk - pcache is fd->buf held by
 *             dirty / flushing inodes.
 * Lockless cap reads, so only meaningful when I/O is quiescent. */
void s3fs_ebpf_fd_buf_audit(u64 *walk, u64 *pcache)
{
	struct super_block *sb = s3fs_active_sb;
	struct s3fs_resident *res;
	struct inode *inode;
	u64 walk_pcache = 0;
	int n, i;

	*walk = 0;
	*pcache = 0;
	if (!sb)
		return;

	spin_lock(&sb->s_inode_list_lock);
	list_for_each_entry(inode, &sb->s_inodes, i_sb_list) {
		struct s3fs_file_data *fd;

		spin_lock(&inode->i_lock);
		if (!(inode->i_state & (I_FREEING | I_WILL_FREE | I_NEW)) &&
		    S_ISREG(inode->i_mode)) {
			fd = inode->i_private;
			if (fd)
				*walk += READ_ONCE(fd->cap);
		}
		spin_unlock(&inode->i_lock);
	}
	spin_unlock(&sb->s_inode_list_lock);

	/* The resident index is what the capacity pass charges; the sb walk is the
	 * ground truth.  They differ only while I/O is in flight, so a mismatch is
	 * reported (rate limited) rather than treated as a bug. */
	n = s3fs_collect_resident(&res);
	for (i = 0; i < n; i++)
		*pcache += res[i].bytes;
	kfree(res);
	n = s3fs_collect_resident_walk(&res);
	for (i = 0; i < n; i++)
		walk_pcache += res[i].bytes;
	kfree(res);
	if (walk_pcache != *pcache)
		pr_warn_ratelimited("s3fs_ebpf: resident index %llu B != sb walk %llu B (ok while I/O is in flight)\n",
				    *pcache, walk_pcache);
}

/* W1: inodes pinned by an outstanding flush.  put_super releases any
 * that remain (daemon gone, PUT failed) so umount does not leak them. */
static LIST_HEAD(s3fs_flush_pins);
static DEFINE_SPINLOCK(s3fs_flush_pins_lock);

/* Caller holds fd->lock and a reference on inode.  No-op if pinned. */
static void s3fs_flush_pin(struct inode *inode, struct s3fs_file_data *fd)
{
	spin_lock(&s3fs_flush_pins_lock);
	if (list_empty(&fd->pin_link)) {
		ihold(inode);
		fd->pin_inode = inode;
		list_add_tail(&fd->pin_link, &s3fs_flush_pins);
	}
	spin_unlock(&s3fs_flush_pins_lock);
}

/* Returns true if the caller now owns the pin reference and must iput
 * the inode (after dropping fd->lock — the iput may evict it).  List
 * membership decides ownership so this cannot race release_all into a
 * double iput. */
static bool s3fs_flush_unpin(struct s3fs_file_data *fd)
{
	bool owned = false;

	spin_lock(&s3fs_flush_pins_lock);
	if (!list_empty(&fd->pin_link)) {
		list_del_init(&fd->pin_link);
		owned = true;
	}
	spin_unlock(&s3fs_flush_pins_lock);
	return owned;
}

static void s3fs_flush_pins_release_all(void)
{
	struct s3fs_file_data *fd;
	struct inode *inode;

	for (;;) {
		spin_lock(&s3fs_flush_pins_lock);
		fd = list_first_entry_or_null(&s3fs_flush_pins,
					      struct s3fs_file_data, pin_link);
		if (!fd) {
			spin_unlock(&s3fs_flush_pins_lock);
			return;
		}
		list_del_init(&fd->pin_link);
		inode = fd->pin_inode;
		spin_unlock(&s3fs_flush_pins_lock);
		pr_warn_ratelimited("s3fs_ebpf: ino=%lu released with unacknowledged flush\n",
				    inode->i_ino);
		iput(inode);
	}
}

/*
 * P3: free the idle chunks of a clean inode (capacity eviction).  After this
 * the page cache is dropped too (s3fs_evict_page_cache), so the next read
 * reaches .read_folio, misses, and the daemon populates it again.  Dirty or
 * flushing inodes are skipped - their chunks hold user-written content pending
 * a flush.  A chunk some reader has pinned (U8) stays: it serves that reader
 * (the retry after the wake finds it populated) and is freed by a later pass.
 *
 * Returns true when at least one chunk was freed.
 */
static bool s3fs_file_data_reset(u64 ino)
{
	struct inode *inode;
	struct s3fs_file_data *fd;
	bool freed_any = false;

	if (!s3fs_active_sb)
		return false;
	inode = ilookup(s3fs_active_sb, (unsigned long)ino);
	if (!inode)
		return false;
	fd = inode->i_private;
	if (fd) {
		mutex_lock(&fd->lock);
		if (!fd->dirty && !fd->flushing) {
			bool kept = false;
			u32 ci;

			for (ci = 0; ci < fd->nr_chunks; ci++) {
				/* U8: a reader pins its chunk BEFORE it creates the
				 * pending, i.e. possibly before the chunk is even
				 * allocated (p == NULL).  That pin must count: dropping
				 * the table below would lose it, and the chunk the
				 * daemon then populates would be unprotected between
				 * its wake and the reader's re-check. */
				if (fd->chunks[ci].readers) {
					kept = true;
					continue;
				}
				if (!fd->chunks[ci].p)
					continue;
				s3fs_fd_chunk_free(fd, ci); /* S11: no longer valid */
				freed_any = true;
			}
			if (kept) {
				atomic_long_inc(&s3fs_reset_skipped);
			} else {
				s3fs_fd_free_all(fd);
				fd->size = 0;
			}
			/* Q1: do NOT clear inode i_size - it reflects the S3 object
			 * size and must survive eviction so stat(2) and read_iter
			 * keep returning the correct file size. */
		}
		mutex_unlock(&fd->lock);
	}
	iput(inode);
	return freed_any;
}
