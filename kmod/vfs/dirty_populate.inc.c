/* SPDX-License-Identifier: GPL-2.0 */
/* dirty read/layout, file_data lifecycle, populate_pages.  Part of vfs_ops.c: included from there, one translation
 * unit, never compiled on its own. */

long s3fs_ebpf_read_dirty(u64 ino, void __user *uaddr, u64 cap)
{
	struct inode *inode;
	struct s3fs_file_data *fd;
	size_t n;
	long ret;

	if (!s3fs_active_sb)
		return -ENODEV;
	inode = ilookup(s3fs_active_sb, ino);
	if (!inode)
		return -ENOENT;
	fd = inode->i_private;
	if (!fd) {
		iput(inode);
		return -EIO;
	}

	mutex_lock(&fd->lock);
	/* W1: this read covers every enqueue so far (writes precede enqueue). */
	fd->flush_read_seq = fd->flush_seq;
	n = min_t(size_t, fd->size, cap);
	ret = n ? s3fs_fd_copy_out(fd, uaddr, n) : 0;
	mutex_unlock(&fd->lock);
	iput(inode);
	return ret;
}

/* AC-51: S3FS_IOC_READ_DIRTY_RANGE -- copy [offset, offset + cap) of the fd's
 * content (clamped to fd->size).  Remote chunks read as zeros here; the
 * daemon asks for data chunks only (S3FS_IOC_DIRTY_LAYOUT says which). */
long s3fs_ebpf_read_dirty_range(u64 ino, void __user *uaddr, u64 offset, u64 cap)
{
	struct inode *inode;
	struct s3fs_file_data *fd;
	size_t n;
	long ret;

	if (!s3fs_active_sb)
		return -ENODEV;
	inode = ilookup(s3fs_active_sb, ino);
	if (!inode)
		return -ENOENT;
	fd = inode->i_private;
	if (!fd) {
		iput(inode);
		return -EIO;
	}
	mutex_lock(&fd->lock);
	n = offset < fd->size ? min_t(u64, cap, fd->size - offset) : 0;
	ret = n ? s3fs_fd_copy_out_at(fd, uaddr, (size_t)offset, n) : 0;
	mutex_unlock(&fd->lock);
	iput(inode);
	return ret;
}

/* AC-51: S3FS_IOC_DIRTY_LAYOUT -- one byte per chunk of the fd's content
 * (S3FS_DIRTY_CHUNK_*), so the daemon can flush remote chunks as server-side
 * copies.  Like READ_DIRTY it claims every enqueue so far: the write that
 * enqueued the flush is in this layout (W1), and a later write enqueues its
 * own flush.  -ENOSPC (with *nr set) when `cap` is too small. */
long s3fs_ebpf_dirty_layout(u64 ino, u8 __user *states, u32 cap, u64 *size,
			    u32 *nr)
{
	struct inode *inode;
	struct s3fs_file_data *fd;
	u32 n, ci;
	long ret = 0;

	BUILD_BUG_ON(S3FS_DIRTY_CHUNK_BYTES != S3FS_CHUNK_SIZE);
	if (!s3fs_active_sb)
		return -ENODEV;
	inode = ilookup(s3fs_active_sb, ino);
	if (!inode)
		return -ENOENT;
	fd = inode->i_private;
	if (!fd) {
		iput(inode);
		return -EIO;
	}
	mutex_lock(&fd->lock);
	*size = fd->size;
	n = (u32)DIV_ROUND_UP(fd->size, S3FS_CHUNK_SIZE);
	*nr = n;
	if (n > cap) {
		ret = -ENOSPC;
		goto out;
	}
	for (ci = 0; ci < n;) {
		u8 tmp[128];
		u32 k, m = min_t(u32, n - ci, sizeof(tmp));

		for (k = 0; k < m; k++) {
			struct s3fs_chunk *c = ci + k < fd->nr_chunks ?
					       &fd->chunks[ci + k] : NULL;

			tmp[k] = c && c->remote ? S3FS_DIRTY_CHUNK_REMOTE :
				 (c && c->p ? S3FS_DIRTY_CHUNK_DATA :
				  S3FS_DIRTY_CHUNK_HOLE);
		}
		if (copy_to_user(states + ci, tmp, m)) {
			ret = -EFAULT;
			goto out;
		}
		ci += m;
	}
	fd->flush_read_seq = fd->flush_seq;
out:
	mutex_unlock(&fd->lock);
	iput(inode);
	return ret;
}

int s3fs_ebpf_mark_dirty(u64 ino)
{
	struct inode *inode;
	struct s3fs_file_data *fd;

	if (!s3fs_active_sb)
		return -ENODEV;
	inode = ilookup(s3fs_active_sb, ino);
	if (!inode)
		return -ENOENT;
	fd = inode->i_private;
	if (!fd) {
		iput(inode);
		return -EIO;
	}
	mutex_lock(&fd->lock);
	fd->dirty = true;
	mutex_unlock(&fd->lock);
	iput(inode);
	return 0;
}

static struct s3fs_file_data *s3fs_file_data_new(const void *initial,
						 size_t initial_size)
{
	struct s3fs_file_data *fd = kzalloc(sizeof(*fd), GFP_KERNEL);

	if (!fd)
		return NULL;
	mutex_init(&fd->lock);
	INIT_LIST_HEAD(&fd->pin_link);
	fd->hit = alloc_percpu(struct s3fs_hit_acc);
	if (!fd->hit) {
		kfree(fd);
		return NULL;
	}
	if (initial_size) {
		u32 ci;

		/* Initial data is a single contiguous load, so every chunk it
		 * covers is valid (populated) and sized to the data. */
		if (s3fs_fd_store(fd, 0, initial, NULL, initial_size,
				  initial_size)) {
			s3fs_fd_free_all(fd);
			free_percpu(fd->hit);
			kfree(fd);
			return NULL;
		}
		fd->size = initial_size;
		for (ci = 0; ci * S3FS_CHUNK_SIZE < initial_size; ci++)
			fd->chunks[ci].populated = true;
	}
	return fd;
}

static void s3fs_file_data_free(struct s3fs_file_data *fd)
{
	if (!fd)
		return;
	s3fs_fd_free_all(fd);
	free_percpu(fd->hit);
	kfree(fd);
}

/* Q3: limit applies to per-call *size*, not offset+size, so large files can
 * be admitted in chunks at offsets > 4 GiB.  Daemon chunks are ≤ 4 MiB
 * (partialAdmitChunk); 64 MiB is 16× headroom while bounding kvmalloc cost
 * per call.  The per-ino buffer still grows to offset+size; files larger than
 * available kernel memory should use arena mode instead of pcache mode. */
#define S3FS_POPULATE_MAX_SIZE  (64ULL << 20)   /* 64 MiB per call */

/* AB-04: populate fd->buf with S3 data from userspace, then insert a
 * page-cache-mode admit entry. Reads served by .read_folio via
 * s3fs_folio_fill_from_kmalloc (no GUP); page cache accumulates
 * uptodate folios so subsequent reads via generic_file_read_iter
 * are served from page cache without touching .read_folio at all.
 *
 * Refuses to overwrite a dirty inode (user-written content takes
 * precedence over S3 data until the flush completes). */
/* AC-51: [offset, end) is made of whole remote chunks (up to i_size).  fd->lock
 * held.  The only populate a dirty fd accepts. */
static bool s3fs_fd_remote_covered(struct inode *inode, struct s3fs_file_data *fd,
				   u64 offset, u64 end)
{
	u64 eof = max_t(u64, end, (u64)i_size_read(inode));
	u32 ci, first = (u32)(offset >> S3FS_PCACHE_CHUNK_SHIFT);
	u32 last = (u32)((end - 1) >> S3FS_PCACHE_CHUNK_SHIFT);

	if (!fd->nr_remote)
		return false;
	for (ci = first; ci <= last; ci++) {
		u64 cs = (u64)ci << S3FS_PCACHE_CHUNK_SHIFT;
		u64 ce = min_t(u64, cs + S3FS_CHUNK_SIZE, eof);

		if (ci >= fd->nr_chunks || !fd->chunks[ci].remote ||
		    offset > cs || end < ce)
			return false;
	}
	return true;
}

/* T5: does a populate of [offset, end) leave chunk ci holding valid data
 * everywhere below eof?  Yes when it covers the whole chunk, or the chunk
 * from its start up to eof (the file's last chunk).  A range that starts
 * inside the chunk never does: the bytes in front of offset were not part
 * of this call (the tail case used to ignore that and mark them valid). */
static bool s3fs_populate_fills_chunk(u64 offset, u64 end, u64 eof, u32 ci)
{
	u64 cs = (u64)ci << S3FS_PCACHE_CHUNK_SHIFT;
	u64 ce = cs + S3FS_CHUNK_SIZE;

	if (offset > cs)
		return false;
	return end >= ce || end >= eof;
}

int s3fs_ebpf_populate_pages(u64 ino, u64 offset, u64 size,
			     const void __user *data_ptr)
{
	struct inode *inode;
	struct s3fs_file_data *fd;
	u64 end;
	bool owned;
	int err = 0;

	/* P2/Q3: reject degenerate or oversized requests before any allocation.
	 * size=0 underflows in invalidate_mapping_pages; size > MAX_SIZE would
	 * exhaust memory in a single call; overflow would wrap end past u64. */
	if (size == 0 || size > S3FS_POPULATE_MAX_SIZE)
		return -EINVAL;
	if (check_add_overflow(offset, size, &end))
		return -EINVAL;

	if (!s3fs_active_sb)
		return -ENODEV;
	inode = ilookup(s3fs_active_sb, (unsigned long)ino);
	if (!inode)
		return -ENOENT;
	fd = inode->i_private;
	if (!fd) {
		err = -EIO;
		goto out_iput;
	}

	mutex_lock(&fd->lock);
	owned = fd->dirty || fd->flushing;
	if (owned && !s3fs_fd_remote_covered(inode, fd, offset, end)) {
		/* Never overwrite buf with stale S3 data when user content is
		 * pending (dirty) or a flush PUT is still in-flight (flushing).
		 * AC-51 exception: chunks that are still the S3 object's own
		 * bytes (remote) may be loaded into a dirty fd, whole. */
		err = -EBUSY;
		goto out_unlock;
	}
	/* AC-45: store into 4 MiB chunks.  A missing chunk is allocated to
	 * min(4 MiB, max(end, i_size) - chunk start): exactly what the file
	 * needs (AC-41), and `end` keeps the limit above the range even when
	 * i_size is stale and smaller. */
	err = s3fs_fd_store(fd, offset, NULL, (const char __user *)data_ptr, size,
			    max_t(u64, end, (u64)i_size_read(inode)));
	if (err)
		goto out_unlock;
	/* S11: mark chunks covered by [offset, end) as populated.
	 * T5: only mark a chunk if it is FULLY covered by [offset, end), or if
	 * it is the last chunk and end reaches the file's EOF (so no valid
	 * content exists beyond end — the partial tail is safe to serve from
	 * the chunk because pos < fd->size already guards the read limit).
	 * Marking partially-covered inner chunks would expose stale/zero data in
	 * the fallback (gen==0) path for positions below offset.  T2: the flag
	 * exists for every chunk now, so the pcache path no longer skips the
	 * check for chunks >= 64 (files > 256 MiB). */
	{
		u32 ci;
		u32 first_ci = (u32)(offset >> S3FS_PCACHE_CHUNK_SHIFT);
		u32 last_ci  = (u32)((end - 1) >> S3FS_PCACHE_CHUNK_SHIFT);
		/* U6: use i_size_read(inode) as the authoritative file size for
		 * the last-chunk condition.  After eviction fd->size is reset to
		 * 0, so using fd->size would treat any partial populate call as
		 * if it were at EOF and incorrectly mark incompletely-filled
		 * chunks as valid.  i_size_read preserves the S3 object size
		 * across evictions (Q1/S8 invariant). */
		u64 eof = max_t(u64, end, (u64)i_size_read(inode));

		for (ci = first_ci; ci <= last_ci; ci++)
			if (s3fs_populate_fills_chunk(offset, end, eof, ci))
				fd->chunks[ci].populated = true;
	}
	if (!owned && end > (u64)fd->size)
		fd->size = (size_t)end;
	/* S8: only grow i_size, never shrink it.  Eviction zeroes fd->size (Q1)
	 * but preserves i_size at the S3 object size so that a partial
	 * re-populate does not truncate the file visible to user-space. */
	if (!owned && (loff_t)fd->size > i_size_read(inode))
		i_size_write(inode, (loff_t)fd->size);
	/* Invalidate any stale page-cache folios for this range so the
	 * next read triggers .read_folio and picks up the new fd->buf data.
	 * end > 0 is guaranteed by the size==0 check above. */
	invalidate_mapping_pages(inode->i_mapping,
				 offset >> PAGE_SHIFT,
				 (end - 1) >> PAGE_SHIFT);

	/* R1: insert the pcache entry while fd->lock is still held so no
	 * eviction can free fd->buf between the copy above and the insert
	 * below.  kzalloc inside admit_insert_pcache runs before write_lock,
	 * so sleeping with fd->lock (mutex) held is safe. */
	if (!owned) {	/* a dirty fd is not capacity-managed */
		err = admit_insert_pcache(ino, offset, size);
		if (!err)
			s3fs_hit_set_base(fd);
	}
out_unlock:
	mutex_unlock(&fd->lock);
out_iput:
	iput(inode);
	return err;
}

