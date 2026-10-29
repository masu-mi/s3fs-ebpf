/* SPDX-License-Identifier: GPL-2.0 */
/* G-02 per-ino attr overrides, entries shutdown.  Part of vfs_ops.c: included from there, one translation
 * unit, never compiled on its own. */

/* -- G-02 per-ino attr overrides ---------------------------------------
 *
 * setattr can carry ATTR_MODE / ATTR_UID / ATTR_GID. Without a
 * side-table these changes only live on the in-memory inode and get
 * lost the moment icache evicts the inode: the next iget_locked
 * rebuilds from the bootstrap entry's mode and GLOBAL_ROOT_UID/GID.
 *
 * Solution: record per-ino overrides in a small hash. make_inode
 * consults it after applying the entry defaults; setattr writes to
 * it whenever any of the three attrs is set. Non-persistent across
 * rmmod (that's H-01 scope), but survives icache eviction which is
 * what applications like chmod-then-`stat --format=%a` care about.
 */
struct s3fs_attr_override {
	struct hlist_node node;
	u64 ino;
	u32 valid; /* bitmask of S3FS_ATTR_OV_{MODE,UID,GID} */
	umode_t mode;
	kuid_t uid;
	kgid_t gid;
};

#define S3FS_ATTR_OV_MODE (1u << 0)
#define S3FS_ATTR_OV_UID  (1u << 1)
#define S3FS_ATTR_OV_GID  (1u << 2)

#define S3FS_ATTR_OV_HASH_BITS 6 /* 64 buckets — light traffic */
static DEFINE_HASHTABLE(s3fs_attr_overrides, S3FS_ATTR_OV_HASH_BITS);
static DEFINE_SPINLOCK(s3fs_attr_ov_lock);

static struct s3fs_attr_override *attr_override_lookup_locked(u64 ino)
{
	struct s3fs_attr_override *o;

	hash_for_each_possible(s3fs_attr_overrides, o, node, ino)
		if (o->ino == ino)
			return o;
	return NULL;
}

/* Merge (mode, uid, gid) from an attr change into the per-ino
 * override. Allocates on first sight. Silently drops on ENOMEM —
 * the in-memory inode already has the new attrs from setattr_copy,
 * we're only losing icache-eviction resilience.
 */
static void attr_override_upsert(u64 ino, u32 add_valid,
				 umode_t mode, kuid_t uid, kgid_t gid)
{
	struct s3fs_attr_override *o, *fresh = NULL;

	spin_lock(&s3fs_attr_ov_lock);
	o = attr_override_lookup_locked(ino);
	if (!o) {
		spin_unlock(&s3fs_attr_ov_lock);
		fresh = kzalloc(sizeof(*fresh), GFP_KERNEL);
		if (!fresh)
			return;
		fresh->ino = ino;
		spin_lock(&s3fs_attr_ov_lock);
		o = attr_override_lookup_locked(ino);
		if (!o) {
			hash_add(s3fs_attr_overrides, &fresh->node, ino);
			o = fresh;
			fresh = NULL;
		}
	}
	if (add_valid & S3FS_ATTR_OV_MODE)
		o->mode = mode;
	if (add_valid & S3FS_ATTR_OV_UID)
		o->uid = uid;
	if (add_valid & S3FS_ATTR_OV_GID)
		o->gid = gid;
	o->valid |= add_valid;
	spin_unlock(&s3fs_attr_ov_lock);
	kfree(fresh); /* NULL on the hot path */
}

/* Copy any override for `ino` onto `inode`. Called from make_inode
 * so a re-lookup after icache eviction restores the last setattr.
 */
static void attr_override_apply(u64 ino, struct inode *inode)
{
	struct s3fs_attr_override *o;

	spin_lock(&s3fs_attr_ov_lock);
	o = attr_override_lookup_locked(ino);
	if (o) {
		if (o->valid & S3FS_ATTR_OV_MODE) {
			/* Preserve the file-type bits from the entry so a
			 * mode override on a regular file can't turn it into
			 * a directory. */
			inode->i_mode = (inode->i_mode & S_IFMT) | (o->mode & ~S_IFMT);
		}
		if (o->valid & S3FS_ATTR_OV_UID)
			inode->i_uid = o->uid;
		if (o->valid & S3FS_ATTR_OV_GID)
			inode->i_gid = o->gid;
	}
	spin_unlock(&s3fs_attr_ov_lock);
}

static void attr_override_drain(void)
{
	struct s3fs_attr_override *o;
	struct hlist_node *tmp;
	unsigned int i;

	spin_lock(&s3fs_attr_ov_lock);
	hash_for_each_safe(s3fs_attr_overrides, i, tmp, o, node) {
		hash_del(&o->node);
		kfree(o);
	}
	spin_unlock(&s3fs_attr_ov_lock);
}

void s3fs_ebpf_entries_shutdown(void)
{
	(void)s3fs_ebpf_entries_replace(NULL, 0);
	local_entries_drain();
	attr_override_drain();
}

