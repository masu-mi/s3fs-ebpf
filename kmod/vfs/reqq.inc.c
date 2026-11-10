/* SPDX-License-Identifier: GPL-2.0 */
/* reqq: everything kmod asks of the daemon, in one FIFO.  Part of vfs_ops.c: included from
 * there, one translation unit, never compiled on its own. */

/* -- reqq: miss / flush / delete / rename requests ------------------------ */
/*
 * Read-miss upcalls (MISS), write-back flushes (FLUSH, async for .release /
 * .create, WAIT for .fsync), .unlink (DELETE) and .rename (RENAME) are queued
 * here; the daemon takes them with read(2) / poll(2) on /dev/s3fs_ebpf, one
 * struct s3fs_req_event per read, in the order they were queued.  Before v2
 * Phase 4 these were five queues (miss, flush, flush_wait, delete, rename) with
 * three different ways of being drained, merged back into order by `seq` in the
 * daemon.
 *
 * Best-effort: a failed S3 call stays best-effort at PoC scope.  If the daemon
 * crashes between read and the S3 call the event is lost from kmod; the next
 * sync round re-adds a deleted key to the dyn table (a delete "un-deletes").
 * name[] of a flush carries the dentry's basename so the daemon can PUT even for
 * inos minted locally by .create that have no S3 key yet.
 *
 * Lock order: s3fs_evseq_lock (outer) -> s3fs_reqq_lock.
 */
struct s3fs_req_item {
	struct list_head link;
	struct s3fs_req_event ev;
};

static LIST_HEAD(s3fs_reqq);
static DEFINE_SPINLOCK(s3fs_reqq_lock);
static DECLARE_WAIT_QUEUE_HEAD(s3fs_reqq_wq);

/* AC-84: one order for flush / delete / rename.  `seq` is taken under
 * s3fs_evseq_lock together with the list_add, so an event with a smaller seq is
 * always already queued when one with a larger seq can be read.  The lock is
 * outer to s3fs_reqq_lock (it is held while that is taken).  A MISS has no seq. */
static DEFINE_SPINLOCK(s3fs_evseq_lock);
static u64 s3fs_evseq;

/* The highest seq handed out so far (S3FS_IOC_STATS slot 15). */
u64 s3fs_ebpf_event_seq(void)
{
	u64 seq;

	spin_lock(&s3fs_evseq_lock);
	seq = s3fs_evseq;
	spin_unlock(&s3fs_evseq_lock);
	return seq;
}

/* Consumes w.  Returns the event's seq (0 for a MISS). */
static u64 req_enqueue(struct s3fs_req_item *w)
{
	u64 seq = 0;

	if (w->ev.type == S3FS_REQ_MISS) {
		spin_lock(&s3fs_reqq_lock);
		list_add_tail(&w->link, &s3fs_reqq);
		spin_unlock(&s3fs_reqq_lock);
	} else {
		spin_lock(&s3fs_evseq_lock);
		w->ev.seq = ++s3fs_evseq;
		seq = w->ev.seq;
		spin_lock(&s3fs_reqq_lock);
		list_add_tail(&w->link, &s3fs_reqq);
		spin_unlock(&s3fs_reqq_lock);
		spin_unlock(&s3fs_evseq_lock);
	}
	wake_up_interruptible(&s3fs_reqq_wq);
	return seq;
}

static struct s3fs_req_item *req_item_new(u32 type, u64 ino, u64 parent_ino)
{
	struct s3fs_req_item *w = kzalloc(sizeof(*w), GFP_KERNEL);

	if (!w)
		return NULL;
	w->ev.type = type;
	w->ev.ino = ino;
	w->ev.parent_ino = parent_ino;
	return w;
}

/* Returns the event's seq (0 if nothing was queued). */
static u64 delete_enqueue(u64 ino, u64 parent_ino, const char *name, size_t name_len)
{
	struct s3fs_req_item *w;

	if (name_len == 0 || name_len > S3FS_SYNC_NAME_MAX)
		return 0;
	w = req_item_new(S3FS_REQ_DELETE, ino, parent_ino);
	if (!w)
		return 0;
	w->ev.name_len = name_len;
	memcpy(w->ev.name, name, name_len);
	return req_enqueue(w);
}

/* Returns the event's seq (0 if nothing was queued). */
static u64 rename_enqueue(u64 ino,
			   u64 old_parent_ino, u64 new_parent_ino,
			   const char *old_name, size_t old_len,
			   const char *new_name, size_t new_len)
{
	struct s3fs_req_item *w;

	if (old_len == 0 || old_len > S3FS_SYNC_NAME_MAX ||
	    new_len == 0 || new_len > S3FS_SYNC_NAME_MAX)
		return 0;
	w = req_item_new(S3FS_REQ_RENAME, ino, old_parent_ino);
	if (!w)
		return 0;
	w->ev.new_parent_ino = new_parent_ino;
	w->ev.name_len = old_len;
	w->ev.new_name_len = new_len;
	memcpy(w->ev.name, old_name, old_len);
	memcpy(w->ev.new_name, new_name, new_len);
	return req_enqueue(w);
}

/* Returns the event's seq (0 if nothing was queued).  A WAIT flush (fsync) first
 * drops the queued non-WAIT flushes of the same ino (F1). */
static u64 flush_enqueue(u64 ino, u64 parent_ino, u64 size,
			  const char *name, size_t name_len, u32 flags)
{
	struct s3fs_req_item *w, *q, *tmp;
	LIST_HEAD(stale);

	if (name_len == 0 || name_len > S3FS_SYNC_NAME_MAX)
		return 0;
	w = req_item_new(S3FS_REQ_FLUSH, ino, parent_ino);
	if (!w)
		return 0;
	w->ev.size = size;
	w->ev.flags = flags;
	w->ev.name_len = name_len;
	memcpy(w->ev.name, name, name_len);
	if (flags & S3FS_FLUSH_FLAG_WAIT) {
		/* A flush for this ino that is still queued (typically the empty
		 * placeholder .create enqueues) would be handled after this fsync's
		 * PUT and overwrite it with older state.  The PUT reads the buffer
		 * when it runs, so this event covers those items: drop them.
		 * Deletes, renames, misses and other inos' flushes stay. */
		spin_lock(&s3fs_reqq_lock);
		list_for_each_entry_safe(q, tmp, &s3fs_reqq, link)
			if (q->ev.type == S3FS_REQ_FLUSH && q->ev.ino == ino &&
			    !(q->ev.flags & S3FS_FLUSH_FLAG_WAIT))
				list_move(&q->link, &stale);
		spin_unlock(&s3fs_reqq_lock);
		list_for_each_entry_safe(q, tmp, &stale, link) {
			list_del(&q->link);
			kfree(q);
		}
	}
	return req_enqueue(w);
}

/* Take the oldest request into *out.  Blocks while the queue is empty unless
 * @nonblocking.  Returns sizeof(*out), -EAGAIN (empty, nonblocking) or a
 * negative errno from the wait. */
ssize_t s3fs_req_dequeue(struct s3fs_req_event *out, bool nonblocking)
{
	struct s3fs_req_item *w = NULL;
	int err;

	for (;;) {
		spin_lock(&s3fs_reqq_lock);
		if (!list_empty(&s3fs_reqq)) {
			w = list_first_entry(&s3fs_reqq, struct s3fs_req_item, link);
			list_del(&w->link);
		}
		spin_unlock(&s3fs_reqq_lock);
		if (w)
			break;
		if (nonblocking)
			return -EAGAIN;
		err = wait_event_interruptible(s3fs_reqq_wq,
					       !list_empty(&s3fs_reqq));
		if (err)
			return err;
	}
	*out = w->ev;
	kfree(w);
	return sizeof(*out);
}

__poll_t s3fs_req_poll(struct file *filp, struct poll_table_struct *pt)
{
	__poll_t mask = 0;

	poll_wait(filp, &s3fs_reqq_wq, pt);
	spin_lock(&s3fs_reqq_lock);
	if (!list_empty(&s3fs_reqq))
		mask |= POLLIN | POLLRDNORM;
	spin_unlock(&s3fs_reqq_lock);
	return mask;
}

/* Drop queued requests of one type (@type 0: all) and wake blocked readers.
 * Called from unregister / put_super (everything, so queued events do not
 * linger past a daemon restart) and from the upcall shutdown (misses only). */
static void reqq_drain(u32 type)
{
	struct s3fs_req_item *w, *tmp;

	spin_lock(&s3fs_reqq_lock);
	list_for_each_entry_safe(w, tmp, &s3fs_reqq, link) {
		if (type && w->ev.type != type)
			continue;
		list_del(&w->link);
		kfree(w);
	}
	spin_unlock(&s3fs_reqq_lock);
	wake_up_all(&s3fs_reqq_wq);
}
