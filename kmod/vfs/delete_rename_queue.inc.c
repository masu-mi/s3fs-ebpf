/* SPDX-License-Identifier: GPL-2.0 */
/* delete (.unlink) and rename queues.  Part of vfs_ops.c: included from there, one translation
 * unit, never compiled on its own. */

/* -- Delete queue (E-02: .unlink) --------------------------------------- */
/*
 * .unlink appends here; daemon drains via S3FS_IOC_POP_DELETE and
 * issues S3 DELETE. Similar shape to the flush queue but distinct
 * because delete has no `size` field and different daemon consumer
 * (no readDirty / no admit refresh — the object goes away).
 *
 * Best-effort: a failed DELETE stays best-effort at PoC scope. If
 * the daemon crashes between dequeue and PUT-DELETE the entry is
 * lost from kmod but S3 still has it — next sync round will re-add
 * it to the dyn table, effectively "un-deleting" from user's POV.
 * Production would need a persistent delete log (backlog H-01 area).
 */
struct s3fs_delete_item {
	struct list_head link;
	u64 ino;
	u64 parent_ino; /* G-01 */
	u32 name_len;
	char name[S3FS_SYNC_NAME_MAX + 1];
};

static LIST_HEAD(s3fs_delete_queue);
static DEFINE_SPINLOCK(s3fs_delete_lock);

static void delete_enqueue(u64 ino, u64 parent_ino, const char *name, size_t name_len)
{
	struct s3fs_delete_item *d;

	if (name_len == 0 || name_len > S3FS_SYNC_NAME_MAX)
		return;
	d = kzalloc(sizeof(*d), GFP_KERNEL);
	if (!d)
		return;
	d->ino = ino;
	d->parent_ino = parent_ino;
	d->name_len = name_len;
	memcpy(d->name, name, name_len);
	spin_lock(&s3fs_delete_lock);
	list_add_tail(&d->link, &s3fs_delete_queue);
	spin_unlock(&s3fs_delete_lock);
}

static bool delete_dequeue(struct s3fs_delete_item *out)
{
	struct s3fs_delete_item *d = NULL;

	spin_lock(&s3fs_delete_lock);
	if (!list_empty(&s3fs_delete_queue)) {
		d = list_first_entry(&s3fs_delete_queue,
				     struct s3fs_delete_item, link);
		list_del(&d->link);
	}
	spin_unlock(&s3fs_delete_lock);
	if (!d)
		return false;
	*out = *d;
	kfree(d);
	return true;
}

static void delete_queue_drain(void)
{
	struct s3fs_delete_item *d, *tmp;

	spin_lock(&s3fs_delete_lock);
	list_for_each_entry_safe(d, tmp, &s3fs_delete_queue, link) {
		list_del(&d->link);
		kfree(d);
	}
	spin_unlock(&s3fs_delete_lock);
}

int s3fs_ebpf_delete_pop(struct s3fs_delete_event *out)
{
	struct s3fs_delete_item item;

	if (!delete_dequeue(&item))
		return -EAGAIN;
	memset(out, 0, sizeof(*out));
	out->ino        = item.ino;
	out->parent_ino = item.parent_ino;
	out->name_len   = item.name_len;
	memcpy(out->name, item.name, item.name_len);
	out->name[item.name_len] = '\0';
	return 0;
}

/* -- Rename queue (E-02: .rename) --------------------------------------- */
/*
 * .rename enqueues here with both old and new name. Daemon issues
 * S3 CopyObject(old → new) + RemoveObject(old). Not atomic on S3;
 * a partial failure leaves both keys. */
struct s3fs_rename_item {
	struct list_head link;
	u64 ino;
	u64 old_parent_ino; /* G-01 */
	u64 new_parent_ino; /* G-01 */
	u32 old_name_len;
	u32 new_name_len;
	char old_name[S3FS_SYNC_NAME_MAX + 1];
	char new_name[S3FS_SYNC_NAME_MAX + 1];
};

static LIST_HEAD(s3fs_rename_queue);
static DEFINE_SPINLOCK(s3fs_rename_lock);

static void rename_enqueue(u64 ino,
			   u64 old_parent_ino, u64 new_parent_ino,
			   const char *old_name, size_t old_len,
			   const char *new_name, size_t new_len)
{
	struct s3fs_rename_item *r;

	if (old_len == 0 || old_len > S3FS_SYNC_NAME_MAX ||
	    new_len == 0 || new_len > S3FS_SYNC_NAME_MAX)
		return;
	r = kzalloc(sizeof(*r), GFP_KERNEL);
	if (!r)
		return;
	r->ino = ino;
	r->old_parent_ino = old_parent_ino;
	r->new_parent_ino = new_parent_ino;
	r->old_name_len = old_len;
	r->new_name_len = new_len;
	memcpy(r->old_name, old_name, old_len);
	memcpy(r->new_name, new_name, new_len);
	spin_lock(&s3fs_rename_lock);
	list_add_tail(&r->link, &s3fs_rename_queue);
	spin_unlock(&s3fs_rename_lock);
}

static bool rename_dequeue(struct s3fs_rename_item *out)
{
	struct s3fs_rename_item *r = NULL;

	spin_lock(&s3fs_rename_lock);
	if (!list_empty(&s3fs_rename_queue)) {
		r = list_first_entry(&s3fs_rename_queue,
				     struct s3fs_rename_item, link);
		list_del(&r->link);
	}
	spin_unlock(&s3fs_rename_lock);
	if (!r)
		return false;
	*out = *r;
	kfree(r);
	return true;
}

static void rename_queue_drain(void)
{
	struct s3fs_rename_item *r, *tmp;

	spin_lock(&s3fs_rename_lock);
	list_for_each_entry_safe(r, tmp, &s3fs_rename_queue, link) {
		list_del(&r->link);
		kfree(r);
	}
	spin_unlock(&s3fs_rename_lock);
}

int s3fs_ebpf_rename_pop(struct s3fs_rename_event *out)
{
	struct s3fs_rename_item item;

	if (!rename_dequeue(&item))
		return -EAGAIN;
	memset(out, 0, sizeof(*out));
	out->ino            = item.ino;
	out->old_parent_ino = item.old_parent_ino;
	out->new_parent_ino = item.new_parent_ino;
	out->old_name_len   = item.old_name_len;
	out->new_name_len   = item.new_name_len;
	memcpy(out->old_name, item.old_name, item.old_name_len);
	out->old_name[item.old_name_len] = '\0';
	memcpy(out->new_name, item.new_name, item.new_name_len);
	out->new_name[item.new_name_len] = '\0';
	return 0;
}

