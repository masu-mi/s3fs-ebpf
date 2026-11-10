/* SPDX-License-Identifier: GPL-2.0 */
/* write-back flush queue.  Part of vfs_ops.c: included from there, one translation
 * unit, never compiled on its own. */

/* -- fsync flush waiters ---------------------------------------------------- */
/* The flush requests themselves are in reqq (reqq.inc.c); what is here is the
 * completion an fsync() sleeps on until the daemon's PUT is done. */

/*
 * DONE for a FLUSH (type FLUSH): the daemon finished the PUT of `ino`'s flush with
 * `err`.  Wakes any fsync caller blocked on this ino (an inflight FLUSH record) --
 * -ENOENT only when the inode is gone: for the .release path (nobody waits) there
 * is simply no record to wake.
 *
 * W3: a successful PUT whose read_dirty predates the latest enqueue (the W1 seq
 * check) does not hold what an fsync enqueued after it wrote, so it leaves the
 * waiter alone: the fsync's own request signals it.  Before, the PUT of an earlier
 * close(2) finishing while an fsync waited returned that fsync 0 before its data had
 * been read.
 *
 * err != 0 (AC-95): the PUT failed.  The dirty flag goes back on in the same
 * critical section that clears `flushing` (it was a separate MARK_DIRTY ioctl:
 * a close() between the two left flushing = 0, dirty = 0 with a request queued, and
 * a read miss in that window could overwrite fd->buf with S3's stale bytes -- model
 * FE-7).  The pin stays: the data must survive until a later flush succeeds.
 */
static int s3fs_done_flush(u64 ino, int err)
{
	struct s3fs_inflight *p, *target = NULL;
	struct inode *inode;
	struct s3fs_file_data *fd;
	bool covered = true;

	spin_lock(&s3fs_inflight_lock);
	p = inflight_find_locked(S3FS_REQ_FLUSH, ino, S3FS_FLUSH_KEY, 0);
	if (p) {
		target = p;
		refcount_inc(&target->refs);
	}
	spin_unlock(&s3fs_inflight_lock);

	/* U5: clear the in-flight flush guard and mark bitmap bits now that
	 * the daemon has confirmed the PUT.  Done for both fsync-wait and
	 * fire-and-forget paths so reads after flush serve from fd->buf
	 * without a new daemon upcall.  Only set bitmap on success (err==0)
	 * so a failed PUT leaves the state for the retry path. */
	if (s3fs_active_sb) {
		inode = ilookup(s3fs_active_sb, (unsigned long)ino);
		if (inode) {
			fd = inode->i_private;
			if (fd) {
				bool unpin = false;

				mutex_lock(&fd->lock);
				/* W1: a successful PUT whose read_dirty predates
				 * the latest enqueue does not cover it — keep
				 * flushing (and the pin) until that one lands.
				 * On error keep the pin: the data must survive
				 * until a later flush succeeds (or umount
				 * releases it). */
				covered = err != 0 ||
					  fd->flush_read_seq == fd->flush_seq;
				if (err != 0)
					fd->dirty = true;	/* AC-95: was MARK_DIRTY */
				if (covered) {
					fd->flushing = false;
					if (err == 0 && !fd->dirty && fd->size > 0) {
						u32 last_ci = (u32)((fd->size - 1) >>
							      S3FS_PCACHE_CHUNK_SHIFT);
						u32 ci;

						/* Only allocated chunks hold the data
						 * that was just PUT; a hole is
						 * re-fetched on its next read.  A remote
						 * chunk may have a buffer (mark_remote
						 * leaves a half-filled one in place) but
						 * the flush copied the object's bytes, not
						 * the buffer's: it stays unpopulated
						 * (AC-51). */
						for (ci = 0; ci <= last_ci &&
						     ci < fd->nr_chunks; ci++)
							if (fd->chunks[ci].p &&
							    !fd->chunks[ci].remote)
								fd->chunks[ci].populated = true;
						/* AC-51: the object now equals
						 * the fd, so a remote chunk is
						 * just a chunk we do not hold. */
						for (ci = 0; fd->nr_remote &&
						     ci < fd->nr_chunks; ci++)
							if (fd->chunks[ci].remote) {
								fd->chunks[ci].remote = false;
								fd->nr_remote--;
							}
					}
					if (err == 0 && !fd->dirty)
						unpin = s3fs_flush_unpin(fd);
				}
				mutex_unlock(&fd->lock);
				if (unpin)
					iput(inode); /* pin ref; ilookup ref below keeps it live */
			}
			iput(inode);
		}
	}

	if (!target)
		return -ENOENT;
	if (!covered) {
		inflight_put(target);	/* W3: not this waiter's PUT */
		return 0;
	}

	target->err = err;
	complete_all(&target->done);
	inflight_unhash(target);
	inflight_put(target);
	return 0;
}
