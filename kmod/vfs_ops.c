// SPDX-License-Identifier: GPL-2.0
/*
 * s3fs-ebpf: VFS glue.
 *
 * Week 3 materialises the mount point as a small in-memory filesystem
 * with three demo objects that stand in for S3 bucket contents:
 *
 *   hello.txt              ino=100  size=17
 *   world.txt              ino=101  size=13
 *   models_weights.bin     ino=102  size=23
 *
 * Content storage is per-inode kmalloc for now. Each inode carries a
 * `struct s3fs_file_data` on i_private that owns a growable buffer,
 * initialised from compile-time initial content at first lookup.
 * read() and write() work against that buffer, so `cat` returns the
 * initial content and `echo foo > hello.txt` mutates it in-place.
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
#include <linux/hashtable.h>
#include <linux/highmem.h>
#include <linux/mm.h>
#include <linux/mm_types.h>
#include <linux/module.h>
#include <linux/mutex.h>
#include <linux/namei.h>
#include <linux/overflow.h>
#include <linux/pagemap.h>
#include <linux/sched/mm.h>
#include <linux/slab.h>
#include <linux/spinlock.h>
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
	const char *initial_content;   /* NUL-terminated initial bytes */
	size_t initial_content_size;   /* size in bytes, excludes NUL */
};

static const char hello_content[]  = "hello, s3fs-ebpf\n";       /* 17 bytes */
static const char world_content[]  = "hello world!\n";           /* 13 bytes */
static const char models_content[] = "AI weights placeholder\n"; /* 23 bytes */

/*
 * Entry table. Ordered by name (iterate_shared emits in listed order).
 * Update this if the demo bucket contents change; the daemon's
 * pre-warmer keeps an independent mirror until Week 4+ adds a
 * bucket-sync ioctl.
 */
static const struct s3fs_entry s3fs_entries[] = {
	{ .name = "hello.txt",          .ino = 100, .mode = S_IFREG | 0644,
	  .initial_content = hello_content,
	  .initial_content_size = sizeof(hello_content)  - 1 },
	{ .name = "models_weights.bin", .ino = 102, .mode = S_IFREG | 0644,
	  .initial_content = models_content,
	  .initial_content_size = sizeof(models_content) - 1 },
	{ .name = "world.txt",          .ino = 101, .mode = S_IFREG | 0644,
	  .initial_content = world_content,
	  .initial_content_size = sizeof(world_content)  - 1 },
};
#define S3FS_NR_ENTRIES ARRAY_SIZE(s3fs_entries)

/* -- Admit table: ino -> daemon user_vm backing --------------------------- */
/*
 * Populated by /dev/s3fs_ebpf's ADMIT_ARENA ioctl once the daemon has
 * written each object's bytes into its arena mmap. .read pins the
 * relevant pages via pin_user_pages_remote against the admitted mm,
 * kmap_local_page's them, and copy_to_user's out of the resulting
 * kernel VA. This routes around 6.12's arena kern_vm having no
 * generic-context fault handler; the daemon's user_vm mapping does
 * have arena_vm_fault, which GUP invokes on any missing PTE.
 */
struct s3fs_admit {
	struct hlist_node node;
	u64 ino;
	u64 uaddr;
	u64 size;
};

#define S3FS_ADMIT_HASH_BITS 6

static DEFINE_HASHTABLE(s3fs_admits, S3FS_ADMIT_HASH_BITS);
static DEFINE_SPINLOCK(s3fs_admits_lock);

/* Reference on the daemon's mm, held while the admit table is
 * non-empty. Dropped by shutdown or by replacing the admits with
 * count=0. All entries in the table share this single mm. */
static struct mm_struct *s3fs_admitted_mm;

/* Set in fill_super so admit_replace can ilookup() cached inodes and
 * refresh their i_size when the daemon publishes a new content size. */
static struct super_block *s3fs_active_sb;

static struct s3fs_admit *admit_lookup_locked(u64 ino)
{
	struct s3fs_admit *a;

	hash_for_each_possible(s3fs_admits, a, node, ino)
		if (a->ino == ino)
			return a;
	return NULL;
}

static void admit_clear_locked(void)
{
	struct s3fs_admit *a;
	struct hlist_node *tmp;
	unsigned int i;

	hash_for_each_safe(s3fs_admits, i, tmp, a, node) {
		hash_del(&a->node);
		kfree(a);
	}
}

int s3fs_ebpf_admit_replace(struct mm_struct *mm,
			    const struct s3fs_admit_entry *entries,
			    u32 count)
{
	struct s3fs_admit **staged = NULL;
	struct mm_struct *prev_mm;
	u32 i;
	int err = 0;

	if (count && (!entries || !mm))
		return -EINVAL;

	for (i = 0; i < count; i++) {
		u64 end;

		if (check_add_overflow(entries[i].uaddr,
				       entries[i].size, &end))
			return -EINVAL;
	}

	if (count) {
		staged = kcalloc(count, sizeof(*staged), GFP_KERNEL);
		if (!staged)
			return -ENOMEM;
		for (i = 0; i < count; i++) {
			staged[i] = kmalloc(sizeof(*staged[i]), GFP_KERNEL);
			if (!staged[i]) {
				err = -ENOMEM;
				goto out_free_staged;
			}
			INIT_HLIST_NODE(&staged[i]->node);
			staged[i]->ino = entries[i].ino;
			staged[i]->uaddr = entries[i].uaddr;
			staged[i]->size = entries[i].size;
		}
	}

	spin_lock(&s3fs_admits_lock);
	admit_clear_locked();
	for (i = 0; i < count; i++)
		hash_add(s3fs_admits, &staged[i]->node, staged[i]->ino);
	prev_mm = s3fs_admitted_mm;
	s3fs_admitted_mm = count ? mm : NULL;
	spin_unlock(&s3fs_admits_lock);

	kfree(staged); /* array itself; nodes now live in the hash */
	staged = NULL;

	if (prev_mm)
		mmput(prev_mm);

	if (s3fs_active_sb) {
		for (i = 0; i < count; i++) {
			struct inode *inode = ilookup(s3fs_active_sb,
						      entries[i].ino);
			if (!inode)
				continue;
			i_size_write(inode, entries[i].size);
			iput(inode);
		}
	}
	return 0;

out_free_staged:
	while (i--)
		kfree(staged[i]);
	kfree(staged);
	return err;
}

void s3fs_ebpf_admit_shutdown(void)
{
	struct mm_struct *prev;

	spin_lock(&s3fs_admits_lock);
	admit_clear_locked();
	prev = s3fs_admitted_mm;
	s3fs_admitted_mm = NULL;
	spin_unlock(&s3fs_admits_lock);

	if (prev)
		mmput(prev);
}

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

static struct s3fs_file_data *s3fs_file_data_new(const void *initial,
						 size_t initial_size)
{
	struct s3fs_file_data *fd = kzalloc(sizeof(*fd), GFP_KERNEL);

	if (!fd)
		return NULL;
	mutex_init(&fd->lock);

	if (initial_size) {
		fd->buf = kmalloc(initial_size, GFP_KERNEL);
		if (!fd->buf) {
			kfree(fd);
			return NULL;
		}
		memcpy(fd->buf, initial, initial_size);
		fd->size = initial_size;
		fd->cap  = initial_size;
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

/*
 * Serve a read out of the admitted user_vm range via GUP + kmap.
 * Caller has copied (uaddr, size) out of the admit table under lock
 * and dropped it; mm is a raw pointer whose lifetime is guaranteed by
 * the reference kmod holds while the admit table is non-empty.
 *
 * PoC scope: single-page reads. All demo objects are <= PAGE_SIZE and
 * daemon-side arena writes never span pages, so a one-page pin
 * suffices. If a request straddles a page boundary we clamp to what
 * fits in the first page; the caller (VFS) will call again for the
 * remainder.
 */
static ssize_t s3fs_admit_read(struct mm_struct *mm,
			       u64 uaddr, u64 asize,
			       char __user *user_buf,
			       size_t count, loff_t *pos)
{
	struct page *page = NULL;
	long pinned;
	void *kva;
	u64 abs_uaddr;
	unsigned long page_offset;
	size_t max_in_page;
	ssize_t ret;

	if ((u64)*pos >= asize)
		return 0;

	abs_uaddr   = uaddr + (u64)*pos;
	page_offset = abs_uaddr & (PAGE_SIZE - 1);
	max_in_page = PAGE_SIZE - page_offset;

	ret = min_t(size_t, count, asize - (u64)*pos);
	ret = min_t(size_t, ret, max_in_page);

	pinned = pin_user_pages_remote(mm, abs_uaddr & PAGE_MASK, 1,
				       0 /* read-only */, &page, NULL);
	if (pinned != 1)
		return pinned < 0 ? pinned : -EFAULT;

	kva = kmap_local_page(page);
	if (copy_to_user(user_buf, (u8 *)kva + page_offset, ret))
		ret = -EFAULT;
	kunmap_local(kva);

	unpin_user_pages(&page, 1);
	if (ret > 0)
		*pos += ret;
	return ret;
}

static ssize_t s3fs_ebpf_file_read(struct file *file, char __user *user_buf,
				   size_t count, loff_t *pos)
{
	struct inode *inode = file_inode(file);
	struct s3fs_file_data *fd;
	struct s3fs_admit *admit;
	struct mm_struct *mm = NULL;
	u64 uaddr = 0;
	u64 asize = 0;
	ssize_t ret;

	if (*pos < 0)
		return -EINVAL;

	/*
	 * Fast path: if the daemon has admitted this ino, serve out of
	 * its arena mmap via GUP. Copy the (uaddr, size) tuple under the
	 * spinlock and grab the mm pointer; the mm reference held by
	 * kmod keeps it alive across the actual pin/kmap/copy.
	 */
	spin_lock(&s3fs_admits_lock);
	admit = admit_lookup_locked(inode->i_ino);
	if (admit) {
		uaddr = admit->uaddr;
		asize = admit->size;
		mm = s3fs_admitted_mm;
	}
	spin_unlock(&s3fs_admits_lock);

	if (mm)
		return s3fs_admit_read(mm, uaddr, asize, user_buf, count, pos);

	/* Fallback: per-inode kmalloc scratch buffer. */
	fd = inode->i_private;
	if (!fd)
		return -EIO;

	mutex_lock(&fd->lock);
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
		fd = s3fs_file_data_new(e->initial_content,
					e->initial_content_size);
		if (!fd) {
			iput(inode);
			return NULL;
		}
		inode->i_private = fd;
		inode->i_size    = fd->size;
		inode->i_op      = &s3fs_ebpf_file_iops;
		inode->i_fop     = &s3fs_ebpf_file_fops;
	} else {
		/* Only regular files in the entry table for now. */
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

static void s3fs_ebpf_put_super(struct super_block *sb)
{
	if (s3fs_active_sb == sb)
		s3fs_active_sb = NULL;
}

static const struct super_operations s3fs_ebpf_super_ops = {
	.statfs		= simple_statfs,
	.drop_inode	= generic_delete_inode,
	.evict_inode	= s3fs_ebpf_evict_inode,
	.put_super	= s3fs_ebpf_put_super,
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

	/* Single-mount PoC: remember the sb so admit_replace can walk
	 * cached inodes and refresh their i_size. Cleared in put_super. */
	s3fs_active_sb = sb;
	return 0;
}
