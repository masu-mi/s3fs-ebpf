/* SPDX-License-Identifier: GPL-2.0 */
/* write-back flush queue.  Part of vfs_ops.c: included from there, one translation
 * unit, never compiled on its own. */

/* -- fsync flush waiters ---------------------------------------------------- */
/* The flush requests themselves are in reqq (reqq.inc.c); what is here is the
 * completion an fsync() sleeps on until the daemon's PUT is done. */

/*
 * Flush waiter — one per fsync() blocked on daemon PUT completion.
 * .release path does NOT register one (fire-and-forget for close(2)),
 * so signal_flush with no matching ino is a normal no-op.
 *
 * Shape mirrors s3fs_pending (read-miss upcall waiter). Refcount 2 on
 * create: one for the hash slot, one for the fsync caller. Hash drops
 * its ref on signal_flush; caller drops on wait return.
 */
struct s3fs_flush_pending {
	struct hlist_node node;
	u64 ino;
	struct completion done;
	int err;
	refcount_t refs;
};

#define S3FS_FLUSH_PENDING_HASH_BITS 4 /* 16 buckets — few concurrent fsyncs */
static DEFINE_HASHTABLE(s3fs_flush_pending, S3FS_FLUSH_PENDING_HASH_BITS);
static DEFINE_SPINLOCK(s3fs_flush_pending_lock);

static struct s3fs_flush_pending *flush_pending_get_or_create(u64 ino)
{
	struct s3fs_flush_pending *p, *fresh;

	spin_lock(&s3fs_flush_pending_lock);
	hash_for_each_possible(s3fs_flush_pending, p, node, ino) {
		if (p->ino == ino) {
			refcount_inc(&p->refs);
			spin_unlock(&s3fs_flush_pending_lock);
			return p;
		}
	}
	spin_unlock(&s3fs_flush_pending_lock);

	fresh = kmalloc(sizeof(*fresh), GFP_KERNEL);
	if (!fresh)
		return NULL;
	INIT_HLIST_NODE(&fresh->node);
	fresh->ino = ino;
	fresh->err = 0;
	init_completion(&fresh->done);
	refcount_set(&fresh->refs, 2);

	spin_lock(&s3fs_flush_pending_lock);
	hash_for_each_possible(s3fs_flush_pending, p, node, ino) {
		if (p->ino == ino) {
			refcount_inc(&p->refs);
			spin_unlock(&s3fs_flush_pending_lock);
			kfree(fresh);
			return p;
		}
	}
	hash_add(s3fs_flush_pending, &fresh->node, ino);
	spin_unlock(&s3fs_flush_pending_lock);
	return fresh;
}

static void flush_pending_put(struct s3fs_flush_pending *p)
{
	if (!p || !refcount_dec_and_test(&p->refs))
		return;
	kfree(p);
}

static void flush_pending_unhash(struct s3fs_flush_pending *p)
{
	spin_lock(&s3fs_flush_pending_lock);
	if (!hlist_unhashed(&p->node)) {
		hash_del(&p->node);
		spin_unlock(&s3fs_flush_pending_lock);
		flush_pending_put(p); /* drop hash's ref */
		return;
	}
	spin_unlock(&s3fs_flush_pending_lock);
}

/*
 * Daemon → kmod signal: PUT completed with `err`. Wakes any fsync
 * caller blocked on this ino. -ENOENT (no waiter) is a normal outcome
 * for the .release path — daemon signals unconditionally and doesn't
 * care whether anyone was listening.
 *
 * W3: a successful PUT whose read_dirty predates the latest enqueue (the
 * W1 seq check) does not hold what an fsync enqueued after it wrote, so
 * it leaves the waiter alone: the fsync's own WAIT event signals it.
 * Before, the PUT of an earlier close(2) finishing while an fsync waited
 * returned that fsync 0 before its data had been read.
 */
int s3fs_ebpf_signal_flush(u64 ino, int err)
{
	struct s3fs_flush_pending *p, *target = NULL;
	struct inode *inode;
	struct s3fs_file_data *fd;
	bool covered = true;

	spin_lock(&s3fs_flush_pending_lock);
	hash_for_each_possible(s3fs_flush_pending, p, node, ino) {
		if (p->ino == ino) {
			target = p;
			refcount_inc(&target->refs);
			break;
		}
	}
	spin_unlock(&s3fs_flush_pending_lock);

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
				 * On error keep the pin: the daemon has restored
				 * dirty and the data must survive until a later
				 * flush succeeds (or umount releases it). */
				covered = err != 0 ||
					  fd->flush_read_seq == fd->flush_seq;
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
		flush_pending_put(target);	/* W3: not this waiter's PUT */
		return 0;
	}

	target->err = err;
	complete_all(&target->done);
	flush_pending_unhash(target);
	flush_pending_put(target);
	return 0;
}

