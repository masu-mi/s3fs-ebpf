/* SPDX-License-Identifier: GPL-2.0 */
/* write-back queue: flush, delete and rename events in one FIFO.  Part of vfs_ops.c: included
 * from there, one translation unit, never compiled on its own. */

/* -- Write-back queue (flush / .unlink / .rename) ------------------------- */
/*
 * .release / .create (flush), .unlink (delete) and .rename append here; the
 * daemon drains the head via S3FS_IOC_POP_WB.  One FIFO, so the daemon handles
 * the events in the order the operations happened (AC-84).  Before v2 Phase 4
 * these were three queues merged back into order by `seq` in the daemon.
 *
 * Best-effort: a failed S3 call stays best-effort at PoC scope.  If the daemon
 * crashes between pop and the S3 call the event is lost from kmod; the next sync
 * round re-adds a deleted key to the dyn table (a delete "un-deletes").
 * name[] of a flush carries the dentry's basename so the daemon can PUT even for
 * inos minted locally by .create that have no S3 key yet.
 *
 * Lock order: s3fs_evseq_lock (outer) -> s3fs_wb_lock / s3fs_flush_wait_lock.
 */
struct s3fs_wb_item {
	struct list_head link;
	u32 type;           /* S3FS_WB_* */
	u32 flags;          /* flush: S3FS_FLUSH_FLAG_* */
	u64 ino;
	u64 parent_ino;     /* G-01: dir the file lives under (rename: source dir) */
	u64 new_parent_ino; /* rename: destination dir */
	u64 size;           /* flush */
	u64 seq;
	u32 name_len;
	u32 new_name_len;
	char name[S3FS_SYNC_NAME_MAX + 1];
	char new_name[S3FS_SYNC_NAME_MAX + 1];
};

static LIST_HEAD(s3fs_wb_queue);
static DEFINE_SPINLOCK(s3fs_wb_lock);

/* AC-84: one order for the WB queue and the WAIT-flush queue.  `seq` is taken
 * under s3fs_evseq_lock together with the list_add, so an event with a smaller
 * seq is always already queued when one with a larger seq can be popped.  The
 * lock is outer to the two queue locks (it is held while they are taken). */
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

/* Consumes w.  Returns the event's seq. */
static u64 wb_enqueue(struct s3fs_wb_item *w)
{
	u64 seq;

	spin_lock(&s3fs_evseq_lock);
	w->seq = ++s3fs_evseq;
	seq = w->seq;
	spin_lock(&s3fs_wb_lock);
	list_add_tail(&w->link, &s3fs_wb_queue);
	spin_unlock(&s3fs_wb_lock);
	spin_unlock(&s3fs_evseq_lock);
	return seq;
}

static struct s3fs_wb_item *wb_item_new(u32 type, u64 ino, u64 parent_ino)
{
	struct s3fs_wb_item *w = kzalloc(sizeof(*w), GFP_KERNEL);

	if (!w)
		return NULL;
	w->type = type;
	w->ino = ino;
	w->parent_ino = parent_ino;
	return w;
}

/* Returns the event's seq (0 if nothing was queued). */
static u64 delete_enqueue(u64 ino, u64 parent_ino, const char *name, size_t name_len)
{
	struct s3fs_wb_item *w;

	if (name_len == 0 || name_len > S3FS_SYNC_NAME_MAX)
		return 0;
	w = wb_item_new(S3FS_WB_DELETE, ino, parent_ino);
	if (!w)
		return 0;
	w->name_len = name_len;
	memcpy(w->name, name, name_len);
	return wb_enqueue(w);
}

/* Returns the event's seq (0 if nothing was queued). */
static u64 rename_enqueue(u64 ino,
			   u64 old_parent_ino, u64 new_parent_ino,
			   const char *old_name, size_t old_len,
			   const char *new_name, size_t new_len)
{
	struct s3fs_wb_item *w;

	if (old_len == 0 || old_len > S3FS_SYNC_NAME_MAX ||
	    new_len == 0 || new_len > S3FS_SYNC_NAME_MAX)
		return 0;
	w = wb_item_new(S3FS_WB_RENAME, ino, old_parent_ino);
	if (!w)
		return 0;
	w->new_parent_ino = new_parent_ino;
	w->name_len = old_len;
	w->new_name_len = new_len;
	memcpy(w->name, old_name, old_len);
	memcpy(w->new_name, new_name, new_len);
	return wb_enqueue(w);
}

static bool wb_dequeue(struct s3fs_wb_item *out)
{
	struct s3fs_wb_item *w = NULL;

	spin_lock(&s3fs_wb_lock);
	if (!list_empty(&s3fs_wb_queue)) {
		w = list_first_entry(&s3fs_wb_queue, struct s3fs_wb_item, link);
		list_del(&w->link);
	}
	spin_unlock(&s3fs_wb_lock);
	if (!w)
		return false;
	*out = *w;
	kfree(w);
	return true;
}

/* Called from unregister / put_super: drop queued events so they don't linger
 * past a daemon restart. */
static void wb_queue_drain(void)
{
	struct s3fs_wb_item *w, *tmp;

	spin_lock(&s3fs_wb_lock);
	list_for_each_entry_safe(w, tmp, &s3fs_wb_queue, link) {
		list_del(&w->link);
		kfree(w);
	}
	spin_unlock(&s3fs_wb_lock);
}

/* Pop the oldest event into *out; -EAGAIN if the queue is empty. */
int s3fs_ebpf_wb_pop(struct s3fs_wb_event *out)
{
	struct s3fs_wb_item *item = kmalloc(sizeof(*item), GFP_KERNEL);
	int ret = -EAGAIN;

	if (!item)
		return -ENOMEM;
	if (wb_dequeue(item)) {
		memset(out, 0, sizeof(*out));
		out->type           = item->type;
		out->flags          = item->flags;
		out->ino            = item->ino;
		out->parent_ino     = item->parent_ino;
		out->new_parent_ino = item->new_parent_ino;
		out->size           = item->size;
		out->seq            = item->seq;
		out->name_len       = item->name_len;
		out->new_name_len   = item->new_name_len;
		memcpy(out->name, item->name, item->name_len);
		memcpy(out->new_name, item->new_name, item->new_name_len);
		ret = 0;
	}
	kfree(item);
	return ret;
}
