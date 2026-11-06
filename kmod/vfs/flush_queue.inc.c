/* SPDX-License-Identifier: GPL-2.0 */
/* write-back flush queue.  Part of vfs_ops.c: included from there, one translation
 * unit, never compiled on its own. */

/* -- Write-back flush queue --------------------------------------------- */
/*
 * .release / .fsync on a dirty inode appends here; the daemon drains
 * via S3FS_IOC_POP_FLUSH. Single global list — one mount, one daemon.
 * name[] carries the dentry's basename so the daemon can PUT even for
 * inos that were minted locally by .create and haven't hit the ino
 * allocator's S3Key map yet.
 */
struct s3fs_flush_item {
	struct list_head link;
	u64 ino;
	u64 parent_ino; /* G-01: dir the file lives under, for daemon full-path resolution */
	u64 size;
	u32 name_len;
	u32 flags;  /* S3FS_FLUSH_FLAG_* (E-03) */
	char name[S3FS_SYNC_NAME_MAX + 1];
};

static LIST_HEAD(s3fs_flush_queue);
static DEFINE_SPINLOCK(s3fs_flush_lock);

/* Separate queue + wait-queue for FLUSH_FLAG_WAIT events (S-08).
 * These are fsync()-triggered; the daemon goroutine blocks on
 * S3FS_IOC_POP_FLUSH_WAIT so they are picked up with near-zero latency
 * instead of waiting up to 100 ms for the ticker-based flush poll. */
static LIST_HEAD(s3fs_flush_wait_queue);
static DEFINE_SPINLOCK(s3fs_flush_wait_lock);
static DECLARE_WAIT_QUEUE_HEAD(s3fs_flush_wait_wq);
static bool s3fs_flush_wait_shutdown;

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

static void flush_enqueue(u64 ino, u64 parent_ino, u64 size,
			  const char *name, size_t name_len, u32 flags)
{
	struct s3fs_flush_item *f;

	if (name_len == 0 || name_len > S3FS_SYNC_NAME_MAX)
		return;
	f = kzalloc(sizeof(*f), GFP_KERNEL);
	if (!f)
		return;
	f->ino = ino;
	f->parent_ino = parent_ino;
	f->size = size;
	f->name_len = name_len;
	f->flags = flags;
	memcpy(f->name, name, name_len);
	if (flags & S3FS_FLUSH_FLAG_WAIT) {
		struct s3fs_flush_item *q, *tmp;
		LIST_HEAD(stale);

		/* The wait queue is served at once, the normal queue by the
		 * daemon's ticker, so a normal item for this ino that is still
		 * queued (typically the empty placeholder .create enqueues) would
		 * be handled after this fsync's PUT and overwrite it with older
		 * state.  The PUT reads the buffer when it runs, so this event
		 * covers those items: drop them. */
		spin_lock(&s3fs_flush_lock);
		list_for_each_entry_safe(q, tmp, &s3fs_flush_queue, link)
			if (q->ino == ino)
				list_move(&q->link, &stale);
		spin_unlock(&s3fs_flush_lock);
		list_for_each_entry_safe(q, tmp, &stale, link) {
			list_del(&q->link);
			kfree(q);
		}
		spin_lock(&s3fs_flush_wait_lock);
		list_add_tail(&f->link, &s3fs_flush_wait_queue);
		spin_unlock(&s3fs_flush_wait_lock);
		wake_up_interruptible(&s3fs_flush_wait_wq);
	} else {
		spin_lock(&s3fs_flush_lock);
		list_add_tail(&f->link, &s3fs_flush_queue);
		spin_unlock(&s3fs_flush_lock);
	}
}

/* Pop one item into *out; returns true if queue had something.
 * Caller owns the copy; the internal item is freed here. */
static bool flush_dequeue(struct s3fs_flush_item *out)
{
	struct s3fs_flush_item *f = NULL;

	spin_lock(&s3fs_flush_lock);
	if (!list_empty(&s3fs_flush_queue)) {
		f = list_first_entry(&s3fs_flush_queue,
				     struct s3fs_flush_item, link);
		list_del(&f->link);
	}
	spin_unlock(&s3fs_flush_lock);
	if (!f)
		return false;
	*out = *f;
	kfree(f);
	return true;
}

/* Called from unregister / put_super to drop any queued events so
 * they don't linger past a daemon restart. Also wakes any goroutine
 * blocked in s3fs_ebpf_flush_pop_wait so it exits cleanly. */
static void flush_queue_drain(void)
{
	struct s3fs_flush_item *f, *tmp;

	spin_lock(&s3fs_flush_lock);
	list_for_each_entry_safe(f, tmp, &s3fs_flush_queue, link) {
		list_del(&f->link);
		kfree(f);
	}
	spin_unlock(&s3fs_flush_lock);

	WRITE_ONCE(s3fs_flush_wait_shutdown, true);
	spin_lock(&s3fs_flush_wait_lock);
	list_for_each_entry_safe(f, tmp, &s3fs_flush_wait_queue, link) {
		list_del(&f->link);
		kfree(f);
	}
	spin_unlock(&s3fs_flush_wait_lock);
	wake_up_all(&s3fs_flush_wait_wq);
}

static void flush_wait_reopen(void)
{
	WRITE_ONCE(s3fs_flush_wait_shutdown, false);
}

int s3fs_ebpf_flush_pop(struct s3fs_flush_event *out)
{
	struct s3fs_flush_item item;

	if (!flush_dequeue(&item))
		return -EAGAIN;
	memset(out, 0, sizeof(*out));
	out->ino        = item.ino;
	out->parent_ino = item.parent_ino;
	out->size       = item.size;
	out->name_len   = item.name_len;
	out->flags      = item.flags;
	memcpy(out->name, item.name, item.name_len);
	out->name[item.name_len] = '\0';
	return 0;
}

/*
 * Blocking variant of flush_pop for WAIT-flagged events (S-08).
 * Waits up to 200 ms per call; returns:
 *   0        — item dequeued into *out
 *  -EAGAIN   — timeout (no item arrived); caller should re-check ctx
 *  -EINTR    — interrupted by signal
 *  -ENODEV   — shutdown in progress (flush_queue_drain called)
 */
int s3fs_ebpf_flush_pop_wait(struct s3fs_flush_event *out)
{
	struct s3fs_flush_item *f;
	long ret;

again:
	ret = wait_event_interruptible_timeout(s3fs_flush_wait_wq,
		!list_empty(&s3fs_flush_wait_queue) ||
		READ_ONCE(s3fs_flush_wait_shutdown),
		msecs_to_jiffies(200));
	if (ret < 0)
		return -EINTR;
	if (ret == 0)
		return -EAGAIN;

	spin_lock(&s3fs_flush_wait_lock);
	if (list_empty(&s3fs_flush_wait_queue)) {
		spin_unlock(&s3fs_flush_wait_lock);
		if (READ_ONCE(s3fs_flush_wait_shutdown))
			return -ENODEV;
		goto again;
	}
	f = list_first_entry(&s3fs_flush_wait_queue,
			     struct s3fs_flush_item, link);
	list_del(&f->link);
	spin_unlock(&s3fs_flush_wait_lock);

	memset(out, 0, sizeof(*out));
	out->ino        = f->ino;
	out->parent_ino = f->parent_ino;
	out->size       = f->size;
	out->name_len   = f->name_len;
	out->flags      = f->flags;
	memcpy(out->name, f->name, f->name_len);
	out->name[f->name_len] = '\0';
	kfree(f);
	return 0;
}

