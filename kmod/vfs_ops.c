// SPDX-License-Identifier: GPL-2.0
/*
 * s3fs-ebpf: VFS glue.
 *
 * Week 3 materialises the mount point as a small in-memory filesystem
 * with three seed objects that stand in for S3 bucket contents:
 *
 *   hello.txt              ino=100  size=17
 *   world.txt              ino=101  size=13
 *   models_weights.bin     ino=102  size=23
 *
 * Content storage is per-inode kmalloc for now. Each inode carries a
 * `struct s3fs_file_data` on i_private that owns a growable buffer,
 * seeded from compile-time defaults at first lookup. read() and
 * write() work against that buffer, so `cat` returns the seed data and
 * `echo foo > hello.txt` mutates it in-place.
 *
 * The Meetup-shape story is "arena is the zero-copy fabric between
 * daemon and kmod". Wiring kmod's file ops to serve from bpf_arena
 * (instead of kmalloc) will land in Week 4 once we have a KFunc that
 * exposes the arena's kernel-VM base to the module. Keeping the
 * storage layer behind s3fs_file_data means the swap is local: no
 * VFS-facing signature change.
 */

#include <linux/fs.h>
#include <linux/fs_context.h>
#include <linux/module.h>
#include <linux/mutex.h>
#include <linux/namei.h>
#include <linux/pagemap.h>
#include <linux/slab.h>
#include <linux/statfs.h>
#include <linux/string.h>
#include <linux/uaccess.h>
#include <linux/version.h>

#include "vfs_ops.h"

/* S3FS_EBPF_MAGIC lives in the shared header so the BPF fentry hook can
 * filter by super_block::s_magic without a second source of truth. */
#include "s3fs_shared_types.h"

#define S3FS_ROOT_INO 1

struct s3fs_entry {
	const char *name;
	unsigned long ino;
	umode_t mode;
	const char *seed;   /* seed content, NUL-terminated */
	size_t seed_size;   /* seed size in bytes, excludes NUL */
};

static const char hello_seed[]  = "hello, s3fs-ebpf\n";       /* 17 bytes */
static const char world_seed[]  = "hello world!\n";           /* 13 bytes */
static const char models_seed[] = "AI weights placeholder\n"; /* 23 bytes */

/*
 * Seed table. Ordered by name (dcache_readdir emits in listed order).
 * Update this if the demo bucket contents change; the daemon's
 * pre-warmer takes the same list from list_bucket().
 */
static const struct s3fs_entry s3fs_entries[] = {
	{ .name = "hello.txt",          .ino = 100, .mode = S_IFREG | 0644,
	  .seed = hello_seed,  .seed_size = sizeof(hello_seed)  - 1 },
	{ .name = "models_weights.bin", .ino = 102, .mode = S_IFREG | 0644,
	  .seed = models_seed, .seed_size = sizeof(models_seed) - 1 },
	{ .name = "world.txt",          .ino = 101, .mode = S_IFREG | 0644,
	  .seed = world_seed,  .seed_size = sizeof(world_seed)  - 1 },
};
#define S3FS_NR_ENTRIES ARRAY_SIZE(s3fs_entries)

static const struct s3fs_entry *find_entry(const struct qstr *qname)
{
	size_t i;

	for (i = 0; i < S3FS_NR_ENTRIES; i++) {
		const struct s3fs_entry *e = &s3fs_entries[i];
		size_t elen = strlen(e->name);

		if (elen == qname->len &&
		    memcmp(e->name, qname->name, elen) == 0)
			return e;
	}
	return NULL;
}

/* -- Per-inode content buffer -------------------------------------------- */

struct s3fs_file_data {
	struct mutex lock;
	u8    *buf;
	size_t size;
	size_t cap;
};

static struct s3fs_file_data *s3fs_file_data_new(const void *seed, size_t seed_size)
{
	struct s3fs_file_data *fd = kzalloc(sizeof(*fd), GFP_KERNEL);

	if (!fd)
		return NULL;
	mutex_init(&fd->lock);

	if (seed_size) {
		fd->buf = kmalloc(seed_size, GFP_KERNEL);
		if (!fd->buf) {
			kfree(fd);
			return NULL;
		}
		memcpy(fd->buf, seed, seed_size);
		fd->size = seed_size;
		fd->cap  = seed_size;
	}
	return fd;
}

static void s3fs_file_data_free(struct s3fs_file_data *fd)
{
	if (!fd)
		return;
	kfree(fd->buf);
	kfree(fd);
}

/* Ensure `fd->cap >= needed`. Caller holds fd->lock. */
static int s3fs_file_data_reserve(struct s3fs_file_data *fd, size_t needed)
{
	size_t new_cap;
	u8 *newbuf;

	if (needed <= fd->cap)
		return 0;

	/* Grow by rounding up to the next 4 KiB slab class. Prevents
	 * pathological O(N^2) allocs when a user writes 1 byte at a
	 * time. Cap is byte size, not element count. */
	new_cap = round_up(needed, 4096);
	newbuf  = krealloc(fd->buf, new_cap, GFP_KERNEL);
	if (!newbuf)
		return -ENOMEM;
	if (new_cap > fd->cap)
		memset(newbuf + fd->cap, 0, new_cap - fd->cap);
	fd->buf = newbuf;
	fd->cap = new_cap;
	return 0;
}

/* -- File ops (regular files) --------------------------------------------- */

static ssize_t s3fs_ebpf_file_read(struct file *file, char __user *user_buf,
				   size_t count, loff_t *pos)
{
	struct inode *inode = file_inode(file);
	struct s3fs_file_data *fd = inode->i_private;
	ssize_t ret;

	if (!fd)
		return -EIO;

	mutex_lock(&fd->lock);
	if (*pos < 0) {
		ret = -EINVAL;
		goto out;
	}
	if ((size_t)*pos >= fd->size) {
		ret = 0;
		goto out;
	}
	ret = min_t(size_t, count, fd->size - (size_t)*pos);
	if (copy_to_user(user_buf, fd->buf + *pos, ret)) {
		ret = -EFAULT;
		goto out;
	}
	*pos += ret;
out:
	mutex_unlock(&fd->lock);
	return ret;
}

static ssize_t s3fs_ebpf_file_write(struct file *file,
				    const char __user *user_buf,
				    size_t count, loff_t *pos)
{
	struct inode *inode = file_inode(file);
	struct s3fs_file_data *fd = inode->i_private;
	loff_t start;
	int err;

	if (!fd)
		return -EIO;
	if (!count)
		return 0;

	mutex_lock(&fd->lock);
	start = (file->f_flags & O_APPEND) ? (loff_t)fd->size : *pos;
	if (start < 0) {
		err = -EINVAL;
		goto out;
	}

	err = s3fs_file_data_reserve(fd, start + count);
	if (err)
		goto out;

	if (copy_from_user(fd->buf + start, user_buf, count)) {
		err = -EFAULT;
		goto out;
	}
	if (start + count > (loff_t)fd->size) {
		fd->size = start + count;
		i_size_write(inode, fd->size);
	}
	*pos = start + count;
#if LINUX_VERSION_CODE >= KERNEL_VERSION(6, 7, 0)
	inode_set_mtime_to_ts(inode, current_time(inode));
	inode_set_ctime_current(inode);
#else
	inode->i_mtime = inode->i_ctime = current_time(inode);
#endif
	mutex_unlock(&fd->lock);
	return count;
out:
	mutex_unlock(&fd->lock);
	return err;
}

static int s3fs_ebpf_file_setattr(struct mnt_idmap *idmap,
				  struct dentry *dentry, struct iattr *attr)
{
	struct inode *inode = d_inode(dentry);
	struct s3fs_file_data *fd = inode->i_private;
	int err;

	err = setattr_prepare(idmap, dentry, attr);
	if (err)
		return err;

	if ((attr->ia_valid & ATTR_SIZE) && fd) {
		loff_t new_size = attr->ia_size;

		mutex_lock(&fd->lock);
		if (new_size < (loff_t)fd->size) {
			fd->size = new_size;
		} else if (new_size > (loff_t)fd->size) {
			err = s3fs_file_data_reserve(fd, new_size);
			if (err) {
				mutex_unlock(&fd->lock);
				return err;
			}
			fd->size = new_size;
		}
		i_size_write(inode, new_size);
		mutex_unlock(&fd->lock);
	}
	setattr_copy(idmap, inode, attr);
	mark_inode_dirty(inode);
	return 0;
}

static const struct file_operations s3fs_ebpf_file_fops = {
	.llseek	= generic_file_llseek,
	.read	= s3fs_ebpf_file_read,
	.write	= s3fs_ebpf_file_write,
};

static const struct inode_operations s3fs_ebpf_file_iops = {
	.getattr	= simple_getattr,
	.setattr	= s3fs_ebpf_file_setattr,
};

/* -- Directory / super ops ------------------------------------------------ */

static struct inode *s3fs_ebpf_make_inode(struct super_block *sb,
					  const struct s3fs_entry *e)
{
	struct inode *inode = new_inode(sb);
	struct s3fs_file_data *fd;

	if (!inode)
		return NULL;

	inode->i_ino  = e->ino;
	inode->i_mode = e->mode;
	inode->i_uid  = GLOBAL_ROOT_UID;
	inode->i_gid  = GLOBAL_ROOT_GID;
#if LINUX_VERSION_CODE >= KERNEL_VERSION(6, 7, 0)
	simple_inode_init_ts(inode);
#else
	inode->i_atime = inode->i_mtime = inode->i_ctime = current_time(inode);
#endif

	if (S_ISREG(e->mode)) {
		fd = s3fs_file_data_new(e->seed, e->seed_size);
		if (!fd) {
			iput(inode);
			return NULL;
		}
		inode->i_private = fd;
		inode->i_size    = fd->size;
		inode->i_op      = &s3fs_ebpf_file_iops;
		inode->i_fop     = &s3fs_ebpf_file_fops;
	} else {
		/* Only regular files in the seed table for now. */
		iput(inode);
		return NULL;
	}
	return inode;
}

static struct dentry *s3fs_ebpf_lookup(struct inode *dir,
				       struct dentry *dentry,
				       unsigned int flags)
{
	const struct s3fs_entry *e;
	struct inode *inode;

	if (dentry->d_name.len > NAME_MAX)
		return ERR_PTR(-ENAMETOOLONG);

	e = find_entry(&dentry->d_name);
	if (!e) {
		/* Cache the negative dentry so repeated ENOENT lookups
		 * don't come back to us. Small win but honest. */
		d_add(dentry, NULL);
		return NULL;
	}

	inode = s3fs_ebpf_make_inode(dir->i_sb, e);
	if (!inode)
		return ERR_PTR(-ENOMEM);
	d_add(dentry, inode);
	return NULL;
}

static int s3fs_ebpf_iterate_shared(struct file *file, struct dir_context *ctx)
{
	size_t i;

	if (!dir_emit_dots(file, ctx))
		return 0;

	/* ctx->pos: 0=".", 1="..", 2..N+1 = table entries. */
	for (i = ctx->pos - 2; i < S3FS_NR_ENTRIES; i++) {
		const struct s3fs_entry *e = &s3fs_entries[i];

		if (!dir_emit(ctx, e->name, strlen(e->name),
			      e->ino, DT_REG))
			break;
		ctx->pos++;
	}
	return 0;
}

static void s3fs_ebpf_evict_inode(struct inode *inode)
{
	struct s3fs_file_data *fd = inode->i_private;

	truncate_inode_pages_final(&inode->i_data);
	clear_inode(inode);
	if (fd) {
		s3fs_file_data_free(fd);
		inode->i_private = NULL;
	}
}

static const struct super_operations s3fs_ebpf_super_ops = {
	.statfs		= simple_statfs,
	.drop_inode	= generic_delete_inode,
	.evict_inode	= s3fs_ebpf_evict_inode,
};

static const struct inode_operations s3fs_ebpf_dir_iops = {
	.lookup		= s3fs_ebpf_lookup,
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

	return 0;
}
