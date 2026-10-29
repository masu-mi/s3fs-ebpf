/* SPDX-License-Identifier: GPL-2.0 */
/* directory and super ops, fill_super.  Part of vfs_ops.c: included from there, one translation
 * unit, never compiled on its own. */

/* -- Directory / super ops ------------------------------------------------ */

/* Forward declarations for the dir iops/fops that s3fs_ebpf_make_inode
 * attaches to synthetic subdir inodes (G-01). Definitions live below,
 * next to the .lookup / .iterate_shared implementations. */
static const struct inode_operations s3fs_ebpf_dir_iops;
static const struct file_operations s3fs_ebpf_dir_fops;

static struct inode *s3fs_ebpf_make_inode(struct super_block *sb,
					  const struct s3fs_entry_snap *e)
{
	struct inode *inode;
	struct s3fs_file_data *fd;

	/* iget_locked hashes the inode so admit_replace's ilookup(ino)
	 * finds it. new_inode() would skip the hash and defeat that. */
	inode = iget_locked(sb, e->ino);
	if (!inode)
		return NULL;
	if (!(inode->i_state & I_NEW))
		return inode;

	inode->i_mode = e->mode;
	inode->i_uid  = GLOBAL_ROOT_UID;
	inode->i_gid  = GLOBAL_ROOT_GID;
	/* G-02: apply any per-ino chmod/chown override so re-lookups
	 * after icache eviction return the last set attrs. */
	attr_override_apply(e->ino, inode);
#if LINUX_VERSION_CODE >= KERNEL_VERSION(6, 7, 0)
	simple_inode_init_ts(inode);
#else
	inode->i_atime = inode->i_mtime = inode->i_ctime = current_time(inode);
#endif

	if (S_ISREG(e->mode)) {
		u64 admit_uaddr, admit_size;
		struct mm_struct *admit_mm;

		fd = s3fs_file_data_new(e->initial_content,
					e->initial_content_size);
		if (!fd) {
			iget_failed(inode);
			return NULL;
		}
		inode->i_private = fd;
		/* Size resolution priority:
		 *   1. sync entry's size (S3-authoritative)
		 *   2. fd->size (compile-time placeholder for bootstrap)
		 * Formerly we peeked at the admit table (arena bytes) but
		 * with range-list admits (D-01) a single admit entry no
		 * longer reflects the whole file size — a partial range
		 * would falsely report a smaller i_size. Daemon must push
		 * the authoritative size via sync entries.
		 */
		(void)admit_uaddr; (void)admit_size; (void)admit_mm;
		if (e->size > 0)
			inode->i_size = e->size;
		else
			inode->i_size = fd->size;
		inode->i_op      = &s3fs_ebpf_file_iops;
		inode->i_fop     = &s3fs_ebpf_file_fops;
		inode->i_mapping->a_ops = &s3fs_ebpf_aops;
		/* No dirty tracking: content is either in-arena (read-only
		 * from VFS's view) or in the kmalloc scratch buffer that
		 * .write mutates directly, bypassing the page cache. */
		mapping_set_gfp_mask(inode->i_mapping, GFP_KERNEL);
	} else if (S_ISDIR(e->mode)) {
		/* G-01: synthetic subdirectory inode. Same iops/fops as
		 * root — lookup + iterate_shared filter by dir->i_ino via
		 * norm_parent, so this dir sees only entries whose
		 * parent_ino == e->ino. nlink=2 keeps `find` happy; real
		 * subdir count would require walking the entry table on
		 * every stat, which the PoC doesn't need. */
		inode->i_op  = &s3fs_ebpf_dir_iops;
		inode->i_fop = &s3fs_ebpf_dir_fops;
		set_nlink(inode, 2);
		inode->i_size = 0;
	} else {
		/* Only regular files and directories are supported. */
		iget_failed(inode);
		return NULL;
	}
	unlock_new_inode(inode);
	return inode;
}

static struct dentry *s3fs_ebpf_lookup(struct inode *dir,
				       struct dentry *dentry,
				       unsigned int flags)
{
	struct s3fs_entry_snap snap;
	struct inode *inode;

	if (dentry->d_name.len > NAME_MAX)
		return ERR_PTR(-ENAMETOOLONG);

	if (!find_entry(dir->i_ino, &dentry->d_name, &snap)) {
		/* Cache the negative dentry so repeated ENOENT lookups
		 * don't come back to us. Small win but honest. */
		d_add(dentry, NULL);
		return NULL;
	}

	inode = s3fs_ebpf_make_inode(dir->i_sb, &snap);
	if (!inode)
		return ERR_PTR(-ENOMEM);
	d_add(dentry, inode);
	return NULL;
}

/*
 * .create: mint a new file entry locally and enqueue an empty flush
 * so the daemon PUTs a zero-byte object. Subsequent writes bump
 * fd->size and mark dirty; .release enqueues a full flush.
 *
 * The ino is picked from a kmod-local range so it can't collide with
 * daemon-assigned inos. On the next SYNC round the daemon includes
 * this key (allocator was seeded via ino registration after PUT),
 * local_prune_promoted_locked drops it from s3fs_local_entries, and
 * iterate_shared stops double-emitting.
 */
static int s3fs_ebpf_create(struct mnt_idmap *idmap, struct inode *dir,
			    struct dentry *dentry, umode_t mode, bool excl)
{
	struct s3fs_local_entry *le;
	struct s3fs_entry_snap snap = { 0 };
	struct inode *inode;
	size_t nlen = dentry->d_name.len;
	u64 ino;

	if (nlen == 0 || nlen > S3FS_SYNC_NAME_MAX)
		return -ENAMETOOLONG;

	le = kzalloc(sizeof(*le), GFP_KERNEL);
	if (!le)
		return -ENOMEM;

	mutex_lock(&s3fs_entries_mtx);
	ino = s3fs_local_next_ino++;
	le->ino         = ino;
	le->parent_ino  = dir->i_ino; /* G-01: remember which dir this was created in */
	le->mode        = S_IFREG | (mode & 0777);
	le->name_len    = nlen;
	memcpy(le->name, dentry->d_name.name, nlen);
	list_add_tail(&le->link, &s3fs_local_entries);
	s3fs_local_nr++;
	mutex_unlock(&s3fs_entries_mtx);

	snap.ino        = ino;
	snap.parent_ino = dir->i_ino;
	snap.size       = 0;
	snap.mode       = le->mode;
	snap.name_len   = nlen;
	memcpy(snap.name, le->name, nlen);
	inode = s3fs_ebpf_make_inode(dir->i_sb, &snap);
	if (!inode) {
		mutex_lock(&s3fs_entries_mtx);
		list_del(&le->link);
		s3fs_local_nr--;
		mutex_unlock(&s3fs_entries_mtx);
		kfree(le);
		return -ENOMEM;
	}
	d_instantiate(dentry, inode);

	/* Fire an empty flush so `touch newfile` becomes a 0-byte PUT
	 * even without a subsequent write. G-01: parent_ino from dir so
	 * the daemon builds "<dir_path>/newfile" as the S3 key when the
	 * file was created under a subdirectory. Coalescable — a
	 * subsequent write's release/fsync would supersede this
	 * placeholder without a network hit. */
	flush_enqueue(ino, dir->i_ino, 0, le->name, nlen, 0);
	return 0;
}

/*
 * Emit one entry at ctx->pos - 2. Snapshot the fields under the
 * entries mutex, then release before calling dir_emit (which can
 * page-fault into user via filldir → copy_to_user). Returns true if
 * the caller should advance ctx->pos and keep going, false if we're
 * past the end or dir_emit signaled the buffer is full.
 */
/*
 * G-01: pick the idx-th entry whose parent_ino matches `want_parent`
 * across the {dyn|bootstrap} + local additions concatenation. dyn /
 * bootstrap are visited first, then locals — same as the pre-G-01
 * order for root-level entries, so a `ls` after `touch newfile`
 * still shows newfile at the end.
 *
 * Bootstrap entries are all root-level; they only contribute when
 * want_parent == root. dyn / local entries carry parent_ino
 * themselves and the norm_parent() fixup handles the 0-means-root
 * back-compat case.
 */
static bool emit_one_entry(struct dir_context *ctx, unsigned long want_parent,
			   size_t idx)
{
	unsigned long ino = 0;
	unsigned char dt = DT_UNKNOWN;
	char name[S3FS_SYNC_NAME_MAX + 1];
	u32 name_len = 0;
	size_t matched = 0;
	struct s3fs_local_entry *le;
	bool ok = false;

	mutex_lock(&s3fs_entries_mtx);
	if (s3fs_dyn_entries) {
		u32 first, cnt;

		dyn_children(want_parent, &first, &cnt);
		matched = cnt;
		if (idx < cnt) {
			const struct s3fs_entry_dyn *e =
				&s3fs_dyn_entries[s3fs_dyn_idx->order[first + idx]];

			ino      = e->ino;
			dt       = S_ISDIR(e->mode) ? DT_DIR : DT_REG;
			name_len = e->name_len;
			memcpy(name, e->name, name_len);
			ok = true;
		}
	} else if (want_parent == (unsigned long)S3FS_ROOT_INO) {
		size_t i;

		for (i = 0; i < S3FS_NR_BOOTSTRAP; i++) {
			const struct s3fs_entry *e = &s3fs_bootstrap_entries[i];
			size_t elen = strlen(e->name);

			if (elen > S3FS_SYNC_NAME_MAX)
				elen = S3FS_SYNC_NAME_MAX;
			if (matched++ != idx)
				continue;
			ino      = e->ino;
			dt       = S_ISDIR(e->mode) ? DT_DIR : DT_REG;
			name_len = elen;
			memcpy(name, e->name, elen);
			ok = true;
			break;
		}
	}
	if (!ok) {
		list_for_each_entry(le, &s3fs_local_entries, link) {
			if (norm_parent(le->parent_ino) != want_parent)
				continue;
			if (matched++ != idx)
				continue;
			ino      = le->ino;
			dt       = S_ISDIR(le->mode) ? DT_DIR : DT_REG;
			name_len = le->name_len;
			memcpy(name, le->name, name_len);
			ok = true;
			break;
		}
	}
	mutex_unlock(&s3fs_entries_mtx);

	if (!ok)
		return false;
	if (!dir_emit(ctx, name, name_len, ino, dt))
		return false;
	return true;
}

static int s3fs_ebpf_iterate_shared(struct file *file, struct dir_context *ctx)
{
	struct inode *dir = file_inode(file);
	unsigned long want_parent = norm_parent(dir->i_ino);

	if (!dir_emit_dots(file, ctx))
		return 0;

	/* ctx->pos: 0=".", 1="..", 2..N+1 = table entries filtered by
	 * parent_ino == this directory's ino. */
	while (emit_one_entry(ctx, want_parent, ctx->pos - 2))
		ctx->pos++;
	return 0;
}

static void s3fs_ebpf_evict_inode(struct inode *inode)
{
	struct s3fs_file_data *fd = inode->i_private;

	truncate_inode_pages_final(&inode->i_data);
	/* W2: s3fs_entries_mtx holders never iput, so no self-deadlock. */
	if (S_ISREG(inode->i_mode))
		s3fs_entry_record_size(inode->i_ino, (u64)i_size_read(inode));
	clear_inode(inode);
	if (fd) {
		s3fs_file_data_free(fd);
		inode->i_private = NULL;
	}
}

static void s3fs_ebpf_put_super(struct super_block *sb)
{
	if (s3fs_active_sb == sb)
		s3fs_active_sb = NULL;
	/* Queued flush / delete / rename events reference inos on this
	 * sb; drop them so a later mount doesn't see them. */
	flush_queue_drain();
	delete_queue_drain();
	rename_queue_drain();
	/* W1: evict_inodes already skipped pinned inodes; drop their pin
	 * refs here so the busy-inodes check after put_super passes. */
	s3fs_flush_pins_release_all();
}

static const struct super_operations s3fs_ebpf_super_ops = {
	.statfs		= simple_statfs,
	.drop_inode	= generic_delete_inode,
	.evict_inode	= s3fs_ebpf_evict_inode,
	.put_super	= s3fs_ebpf_put_super,
};

/*
 * Remove a `name` from s3fs_local_entries if present. Returns true if
 * a local entry was actually removed. Caller must hold no locks —
 * this acquires s3fs_entries_mtx internally.
 */
static bool local_entry_remove(unsigned long parent_ino,
			       const char *name, size_t name_len)
{
	struct s3fs_local_entry *le, *tmp;
	unsigned long want_parent = norm_parent(parent_ino);
	bool removed = false;

	mutex_lock(&s3fs_entries_mtx);
	list_for_each_entry_safe(le, tmp, &s3fs_local_entries, link) {
		if (norm_parent(le->parent_ino) != want_parent ||
		    le->name_len != name_len ||
		    memcmp(le->name, name, name_len) != 0)
			continue;
		list_del(&le->link);
		s3fs_local_nr--;
		kfree(le);
		removed = true;
		break;
	}
	mutex_unlock(&s3fs_entries_mtx);
	return removed;
}

/*
 * Drop the ino from the admit hash if present. Symmetric with the
 * eviction-vote path; the ino won't be re-populated because the
 * daemon-side snapshot removes it via evict-drain feedback on the
 * next admit_replace round.
 */
static void admit_drop_ino_locked(u64 ino)
{
	struct s3fs_admit *a;
	struct hlist_node *tmp;

	write_lock(&s3fs_admits_lock);
	hash_for_each_possible_safe(s3fs_admits, a, tmp, node, ino) {
		if (a->ino != ino)
			continue;
		hash_del(&a->node);
		atomic_dec(&s3fs_admits_count);
		kfree(a);
		break;
	}
	write_unlock(&s3fs_admits_lock);
}

/*
 * .unlink — remove `dentry` from this FS. Enqueues a delete event so
 * the daemon can DELETE the S3 object. Best-effort at PoC scope:
 * local entries drop immediately, dyn entries only disappear on the
 * next sync round (S3 LIST no longer returns the deleted key).
 */
static int s3fs_ebpf_unlink(struct inode *dir, struct dentry *dentry)
{
	struct inode *inode = d_inode(dentry);
	size_t name_len = dentry->d_name.len;
	char name[S3FS_SYNC_NAME_MAX + 1];

	if (!inode)
		return -ENOENT;
	if (name_len == 0 || name_len > S3FS_SYNC_NAME_MAX)
		return -ENAMETOOLONG;
	memcpy(name, dentry->d_name.name, name_len);

	/* Enqueue delete for the daemon regardless of local vs dyn — the
	 * daemon is responsible for the S3 side. Failure to enqueue
	 * (OOM) still lets us drop the local FS view; the file stays
	 * on S3 until user retries or next daemon delete-drain.
	 * G-01: pass dir->i_ino so daemon rebuilds "<dir>/<name>". */
	delete_enqueue(inode->i_ino, dir->i_ino, name, name_len);

	/* Local-side cleanup. Local entry lookup is now parent-aware
	 * (G-01) so a sibling with the same basename under a different
	 * parent isn't accidentally scrubbed. */
	local_entry_remove(dir->i_ino, name, name_len);
	admit_drop_ino_locked(inode->i_ino);

	/* Drop link count so drop_inode fires evict_inode. */
	drop_nlink(inode);
	inode_set_ctime_current(inode);

	return 0;
}

/*
 * Rename a local entry's name in place. Returns true on success.
 * old/new parent inos scope the match so a sibling with the same
 * basename under a different parent doesn't accidentally get renamed
 * (F-02). new_parent may differ from old_parent for cross-dir rename.
 */
static bool local_entry_rename(unsigned long old_parent,
			       const char *old_name, size_t old_len,
			       unsigned long new_parent,
			       const char *new_name, size_t new_len)
{
	struct s3fs_local_entry *le;
	unsigned long want_old = norm_parent(old_parent);
	bool renamed = false;

	mutex_lock(&s3fs_entries_mtx);
	list_for_each_entry(le, &s3fs_local_entries, link) {
		if (norm_parent(le->parent_ino) != want_old ||
		    le->name_len != old_len ||
		    memcmp(le->name, old_name, old_len) != 0)
			continue;
		memcpy(le->name, new_name, new_len);
		le->name_len   = new_len;
		le->parent_ino = norm_parent(new_parent);
		renamed = true;
		break;
	}
	mutex_unlock(&s3fs_entries_mtx);
	return renamed;
}

/*
 * .rename — same-dir rename only (RENAME_EXCHANGE and cross-dir
 * are rejected for simplicity). S3-side rename runs as CopyObject
 * + Delete in the daemon; here we enqueue the event, update the
 * local entry name (if this was a local file), and let the VFS
 * side move the dcache dentry.
 */
static int s3fs_ebpf_rename(struct mnt_idmap *idmap,
			    struct inode *old_dir, struct dentry *old_dentry,
			    struct inode *new_dir, struct dentry *new_dentry,
			    unsigned int flags)
{
	struct inode *inode = d_inode(old_dentry);
	size_t old_len = old_dentry->d_name.len;
	size_t new_len = new_dentry->d_name.len;
	char old_name[S3FS_SYNC_NAME_MAX + 1];
	char new_name[S3FS_SYNC_NAME_MAX + 1];

	if (flags & ~RENAME_NOREPLACE)
		return -EINVAL; /* no RENAME_EXCHANGE / RENAME_WHITEOUT */
	if (!inode)
		return -ENOENT;
	if (old_len == 0 || old_len > S3FS_SYNC_NAME_MAX ||
	    new_len == 0 || new_len > S3FS_SYNC_NAME_MAX)
		return -ENAMETOOLONG;
	memcpy(old_name, old_dentry->d_name.name, old_len);
	memcpy(new_name, new_dentry->d_name.name, new_len);

	/* G-01: cross-dir rename now works. Daemon translates each
	 * parent_ino to a full path prefix and issues a single
	 * CopyObject(<old_dir>/old_name → <new_dir>/new_name) + Delete. */
	rename_enqueue(inode->i_ino, old_dir->i_ino, new_dir->i_ino,
		       old_name, old_len, new_name, new_len);

	/* Local-side: if the file lives in the local list, update its
	 * name + parent so lookups under new_dir succeed immediately.
	 * Dyn entries wait for the next sync to refresh — VFS-side
	 * rename below still makes the new name lookupable via dcache. */
	local_entry_rename(old_dir->i_ino, old_name, old_len,
			   new_dir->i_ino, new_name, new_len);

	inode_set_ctime_current(inode);
	return 0;
}

static const struct inode_operations s3fs_ebpf_dir_iops = {
	.lookup		= s3fs_ebpf_lookup,
	.create		= s3fs_ebpf_create,
	.unlink		= s3fs_ebpf_unlink,
	.rename		= s3fs_ebpf_rename,
};

static const struct file_operations s3fs_ebpf_dir_fops = {
	.llseek		= generic_file_llseek,
	.read		= generic_read_dir,
	.iterate_shared	= s3fs_ebpf_iterate_shared,
	.fsync		= noop_fsync,
};

int s3fs_ebpf_fill_super(struct super_block *sb, struct fs_context *fc)
{
	struct inode *root;

	sb->s_magic		= S3FS_EBPF_MAGIC;
	sb->s_blocksize		= PAGE_SIZE;
	sb->s_blocksize_bits	= PAGE_SHIFT;
	/* alloc_super leaves MAX_NON_LFS (2 GiB - 1): generic_file_read_iter
	 * then returned EOF at 2 GiB for larger objects.  The chunk table
	 * stops at S3FS_MAX_CHUNKS (1 TiB) and fails beyond it (-EFBIG on
	 * store, -EIO on read), which is better than a silent short file. */
	sb->s_maxbytes		= MAX_LFS_FILESIZE;
	sb->s_op		= &s3fs_ebpf_super_ops;
	sb->s_time_gran		= 1;

	root = new_inode(sb);
	if (!root)
		return -ENOMEM;

	root->i_ino  = S3FS_ROOT_INO;
	root->i_mode = S_IFDIR | 0755;
	root->i_uid  = GLOBAL_ROOT_UID;
	root->i_gid  = GLOBAL_ROOT_GID;
#if LINUX_VERSION_CODE >= KERNEL_VERSION(6, 7, 0)
	simple_inode_init_ts(root);
#else
	root->i_atime = root->i_mtime = root->i_ctime = current_time(root);
#endif
	root->i_op  = &s3fs_ebpf_dir_iops;
	root->i_fop = &s3fs_ebpf_dir_fops;
	set_nlink(root, 2);

	sb->s_root = d_make_root(root);
	if (!sb->s_root)
		return -ENOMEM;

	/* Single-mount PoC: remember the sb so admit_replace can walk
	 * cached inodes and refresh their i_size. Cleared in put_super. */
	s3fs_active_sb = sb;
	/* put_super of an earlier mount left the fsync queue shut down; a
	 * remount without rmmod must reopen it, or POP_FLUSH_WAIT returns
	 * -ENODEV at once and the daemon's fsync loop exits. */
	flush_wait_reopen();
	return 0;
}
