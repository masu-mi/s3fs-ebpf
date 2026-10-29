/* SPDX-License-Identifier: GPL-2.0 */
/* fd buffer audit, flush pins, s3fs_file_data_reset.  Part of vfs_ops.c: included from there, one translation
 * unit, never compiled on its own. */


/* M2: cross-check for the M1 gauge, exported via IOC_STATS slots 12/13.
 *   *walk   = sum of fd->cap over every live regular inode on the sb.  The
 *             gauge is maintained incrementally, so gauge != walk at rest
 *             means a cap change that skipped the gauge.
 *   *pcache = sum of fd->cap over distinct pcache inos — exactly what the
 *             AC-41 capacity loop charges.  walk - pcache is fd->buf held
 *             by non-pcache inos (written files, skip_reset leftovers).
 * Lockless cap reads, so only meaningful when I/O is quiescent.  The walk
 * holds i_lock while reading i_private: evict frees fd only after
 * I_FREEING is set, which we check under the same lock. */
void s3fs_ebpf_fd_buf_audit(u64 *walk, u64 *pcache)
{
	struct super_block *sb = s3fs_active_sb;
	struct inode *inode;
	struct s3fs_admit *a;
	unsigned int bkt, n = 0, i;
	u64 *inos;

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

	read_lock(&s3fs_admits_lock);
	hash_for_each(s3fs_admits, bkt, a, node)
		if (a->flags & S3FS_ADMIT_FLAG_PCACHE)
			n++;
	inos = n ? kmalloc_array(n, sizeof(*inos), GFP_ATOMIC) : NULL;
	if (inos) {
		i = 0;
		hash_for_each(s3fs_admits, bkt, a, node)
			if ((a->flags & S3FS_ADMIT_FLAG_PCACHE) && i < n)
				inos[i++] = a->ino;
		n = i;
	}
	read_unlock(&s3fs_admits_lock);
	if (!inos)
		return;

	sort(inos, n, sizeof(*inos), s3fs_u64_cmp, NULL);
	for (i = 0; i < n; i++)
		if (i == 0 || inos[i] != inos[i - 1])
			*pcache += s3fs_ino_buf_bytes(inos[i]);
	kfree(inos);
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
 * P3: free the per-inode kmalloc buffer when a pcache entry is evicted.
 * After eviction the page cache is also dropped, so subsequent reads
 * reach .read_folio which re-fetches from S3 and re-populates fd->buf.
 * Dirty inodes are skipped — their buffer holds user-written content
 * pending flush to S3.
 */
static void s3fs_file_data_reset(u64 ino)
{
	struct inode *inode;
	struct s3fs_file_data *fd;
	struct s3fs_admit *a;
	struct hlist_node *tmp;
	bool removed = false;

	if (!s3fs_active_sb)
		return;
	inode = ilookup(s3fs_active_sb, (unsigned long)ino);
	if (!inode)
		return;
	fd = inode->i_private;
	if (fd) {
		mutex_lock(&fd->lock);
		if (!fd->dirty && !fd->flushing) {
			bool kept = false;
			u32 ci;

			/* R1: drop every PCACHE entry of this ino, including any
			 * inserted between admit_drop_all_ranges and this point,
			 * before freeing chunks: an entry must never outlive the
			 * chunk it points at.
			 * Lock order: fd->lock (outer) → s3fs_admits_lock (inner). */
			write_lock(&s3fs_admits_lock);
			hash_for_each_possible_safe(s3fs_admits, a, tmp, node, ino) {
				if (a->ino != ino)
					continue;
				if (!(a->flags & S3FS_ADMIT_FLAG_PCACHE))
					continue;
				hash_del(&a->node);
				kfree(a);
				atomic_dec(&s3fs_admits_count);
				removed = true;
			}
			if (removed)
				atomic_inc(&s3fs_admit_gen);
			write_unlock(&s3fs_admits_lock);
			/* U8 (per chunk): a chunk some reader waits on is kept — it
			 * serves that reader via the data-based fill even though
			 * its PCACHE entry is gone.  Everything else is freed. */
			for (ci = 0; ci < fd->nr_chunks; ci++) {
				if (fd->chunks[ci].readers) {
					kept = true;
					continue;
				}
				s3fs_fd_chunk_free(fd, ci); /* S11: no longer valid */
			}
			if (kept) {
				atomic_long_inc(&s3fs_reset_skipped);
			} else {
				s3fs_fd_free_all(fd);
				fd->size = 0;
			}
			/* Q1: do NOT clear inode i_size — it reflects the S3 object
			 * size and must survive eviction so stat(2) and read_iter
			 * keep returning the correct file size. */
		}
		mutex_unlock(&fd->lock);
	}
	iput(inode);
}

