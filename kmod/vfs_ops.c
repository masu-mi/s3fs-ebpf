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

#include <linux/completion.h>
#include <linux/fs.h>
#include <linux/fs_context.h>
#include <linux/hashtable.h>
#include <linux/highmem.h>
#include <linux/jiffies.h>
#include <linux/list.h>
#include <linux/mm.h>
#include <linux/mm_types.h>
#include <linux/module.h>
#include <linux/mutex.h>
#include <linux/namei.h>
#include <linux/overflow.h>
#include <linux/pagemap.h>
#include <linux/poll.h>
#include <linux/refcount.h>
#include <linux/sched/mm.h>
#include <linux/slab.h>
#include <linux/spinlock.h>
#include <linux/statfs.h>
#include <linux/string.h>
#include <linux/uaccess.h>
#include <linux/version.h>
#include <linux/wait.h>

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
static const char lazy_content[]   = "(not fetched yet)\n";      /* 18 bytes */
static const char big_content[]    = "(1 MiB blob — daemon will admit real bytes)\n"; /* 45 bytes */

/*
 * Entry table. Ordered by name (iterate_shared emits in listed order).
 * Update this if the demo bucket contents change; the daemon's
 * pre-warmer keeps an independent mirror until Week 4+ adds a
 * bucket-sync ioctl.
 *
 * lazy.txt (ino=103) is intentionally NOT prewarmed by the daemon so
 * `cat` on it exercises the Week 4 (a) C upcall path: kmod's .read
 * misses admit → emits a read-miss event → daemon fetches from S3 →
 * admits → signals → kmod retries → arena bytes returned.
 */
static const struct s3fs_entry s3fs_entries[] = {
	{ .name = "big.bin",            .ino = 104, .mode = S_IFREG | 0644,
	  .initial_content = big_content,
	  .initial_content_size = sizeof(big_content)    - 1 },
	{ .name = "hello.txt",          .ino = 100, .mode = S_IFREG | 0644,
	  .initial_content = hello_content,
	  .initial_content_size = sizeof(hello_content)  - 1 },
	{ .name = "lazy.txt",           .ino = 103, .mode = S_IFREG | 0644,
	  .initial_content = lazy_content,
	  .initial_content_size = sizeof(lazy_content)   - 1 },
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

static void s3fs_ebpf_upcall_shutdown(void); /* forward */

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

	/* Same lifecycle: daemon closed / module unloading. Fail any
	 * blocked readers and drop queued events so nothing survives
	 * into the next daemon incarnation. */
	s3fs_ebpf_upcall_shutdown();
}

/* -- Read-miss upcall: pending completions + event queue ----------------- */
/*
 * Two data structures collaborate:
 *
 *   s3fs_pending — one entry per ino currently waiting for admit.
 *                  refcounted so multiple concurrent readers on the
 *                  same ino share the same completion.
 *
 *   s3fs_miss_events — FIFO of read_miss_event drained by the daemon
 *                      via /dev/s3fs_ebpf's .read.
 *
 * .read on an unadmitted ino:  create pending  → enqueue event  →
 *                              wait_for_completion  → retry admit.
 * SIGNAL_ADMIT ioctl:          find pending  → complete + err.
 * Char dev .read (daemon):     pop event head, block if empty.
 */

struct s3fs_pending {
	struct hlist_node node;
	u64 ino;
	struct completion done;
	int err;
	refcount_t refs;
};

struct s3fs_miss_node {
	struct list_head link;
	struct s3fs_read_miss_event ev;
};

#define S3FS_PENDING_HASH_BITS 4  /* 16 buckets — few concurrent misses */

static DEFINE_HASHTABLE(s3fs_pending, S3FS_PENDING_HASH_BITS);
static DEFINE_SPINLOCK(s3fs_pending_lock);

static LIST_HEAD(s3fs_miss_events);
static DEFINE_SPINLOCK(s3fs_miss_lock);
static DECLARE_WAIT_QUEUE_HEAD(s3fs_miss_wq);

/* Look up an existing pending entry for `ino` and bump its refcount,
 * or allocate a new one. Caller owns one reference on the returned
 * entry (must pending_put on the way out). */
static struct s3fs_pending *pending_get_or_create(u64 ino)
{
	struct s3fs_pending *p, *fresh;

	spin_lock(&s3fs_pending_lock);
	hash_for_each_possible(s3fs_pending, p, node, ino) {
		if (p->ino == ino) {
			refcount_inc(&p->refs);
			spin_unlock(&s3fs_pending_lock);
			return p;
		}
	}
	spin_unlock(&s3fs_pending_lock);

	fresh = kmalloc(sizeof(*fresh), GFP_KERNEL);
	if (!fresh)
		return NULL;
	INIT_HLIST_NODE(&fresh->node);
	fresh->ino = ino;
	fresh->err = 0;
	init_completion(&fresh->done);
	refcount_set(&fresh->refs, 2); /* one for hash, one for caller */

	spin_lock(&s3fs_pending_lock);
	/* Re-check for a racing insert. */
	hash_for_each_possible(s3fs_pending, p, node, ino) {
		if (p->ino == ino) {
			refcount_inc(&p->refs);
			spin_unlock(&s3fs_pending_lock);
			kfree(fresh);
			return p;
		}
	}
	hash_add(s3fs_pending, &fresh->node, ino);
	spin_unlock(&s3fs_pending_lock);
	return fresh;
}

static void pending_put(struct s3fs_pending *p)
{
	if (!p || !refcount_dec_and_test(&p->refs))
		return;
	kfree(p);
}

/* Remove pending entry from the hash. Called by SIGNAL_ADMIT after
 * completing it so subsequent misses on the same ino start fresh. */
static void pending_unhash(struct s3fs_pending *p)
{
	spin_lock(&s3fs_pending_lock);
	if (!hlist_unhashed(&p->node)) {
		hash_del(&p->node);
		spin_unlock(&s3fs_pending_lock);
		pending_put(p); /* drop the hash's reference */
		return;
	}
	spin_unlock(&s3fs_pending_lock);
}

static int miss_enqueue(u64 ino, loff_t offset, size_t count)
{
	struct s3fs_miss_node *node = kmalloc(sizeof(*node), GFP_KERNEL);

	if (!node)
		return -ENOMEM;
	INIT_LIST_HEAD(&node->link);
	node->ev.ino    = ino;
	node->ev.offset = offset;
	node->ev.count  = (u32)min_t(size_t, count, U32_MAX);
	node->ev._pad   = 0;

	spin_lock(&s3fs_miss_lock);
	list_add_tail(&node->link, &s3fs_miss_events);
	spin_unlock(&s3fs_miss_lock);
	wake_up_interruptible(&s3fs_miss_wq);
	return 0;
}

ssize_t s3fs_read_miss_dequeue(struct s3fs_read_miss_event *out,
			       bool nonblocking)
{
	struct s3fs_miss_node *node = NULL;
	int err;

	for (;;) {
		spin_lock(&s3fs_miss_lock);
		if (!list_empty(&s3fs_miss_events)) {
			node = list_first_entry(&s3fs_miss_events,
						struct s3fs_miss_node, link);
			list_del(&node->link);
		}
		spin_unlock(&s3fs_miss_lock);
		if (node)
			break;
		if (nonblocking)
			return -EAGAIN;
		err = wait_event_interruptible(s3fs_miss_wq,
			!list_empty(&s3fs_miss_events));
		if (err)
			return err;
	}

	*out = node->ev;
	kfree(node);
	return sizeof(*out);
}

__poll_t s3fs_read_miss_poll(struct file *filp, struct poll_table_struct *pt)
{
	__poll_t mask = 0;

	poll_wait(filp, &s3fs_miss_wq, pt);
	spin_lock(&s3fs_miss_lock);
	if (!list_empty(&s3fs_miss_events))
		mask |= POLLIN | POLLRDNORM;
	spin_unlock(&s3fs_miss_lock);
	return mask;
}

int s3fs_ebpf_signal_admit(u64 ino, int err)
{
	struct s3fs_pending *p, *target = NULL;

	spin_lock(&s3fs_pending_lock);
	hash_for_each_possible(s3fs_pending, p, node, ino) {
		if (p->ino == ino) {
			target = p;
			refcount_inc(&target->refs);
			break;
		}
	}
	spin_unlock(&s3fs_pending_lock);

	if (!target)
		return -ENOENT;

	target->err = err;
	complete_all(&target->done);
	pending_unhash(target); /* drops the hash's ref */
	pending_put(target);    /* drops our probe ref */
	return 0;
}

/* Drain state on module unload. */
static void s3fs_ebpf_upcall_shutdown(void)
{
	struct s3fs_pending *p;
	struct hlist_node *tmp;
	struct s3fs_miss_node *m, *mnext;
	unsigned int i;

	/* Fail every pending waiter so they wake up and return. */
	spin_lock(&s3fs_pending_lock);
	hash_for_each_safe(s3fs_pending, i, tmp, p, node) {
		hash_del(&p->node);
		p->err = -ESHUTDOWN;
		complete_all(&p->done);
		pending_put(p);
	}
	spin_unlock(&s3fs_pending_lock);

	spin_lock(&s3fs_miss_lock);
	list_for_each_entry_safe(m, mnext, &s3fs_miss_events, link) {
		list_del(&m->link);
		kfree(m);
	}
	spin_unlock(&s3fs_miss_lock);
	wake_up_all(&s3fs_miss_wq);
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

/* How long .read_folio waits for a daemon upcall to admit the ino
 * before falling back to the compile-time kmalloc buffer. */
#define S3FS_UPCALL_WAIT_JIFFIES  (2 * HZ)

/* Look up the admit table for `ino`, copying (uaddr, size) and grabbing
 * the current mm pointer under the spinlock. Returns true on hit. */
static bool admit_snapshot(u64 ino, u64 *out_uaddr, u64 *out_size,
			   struct mm_struct **out_mm)
{
	struct s3fs_admit *a;
	bool hit = false;

	spin_lock(&s3fs_admits_lock);
	a = admit_lookup_locked(ino);
	if (a) {
		*out_uaddr = a->uaddr;
		*out_size  = a->size;
		*out_mm    = s3fs_admitted_mm;
		hit = true;
	}
	spin_unlock(&s3fs_admits_lock);
	return hit;
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

/* -- address_space_operations: page-cache-backed read -------------------- */
/*
 * Week 4 (e) C: switch reads from a bespoke .read handler to
 * generic_file_read_iter, which uses the VFS page cache. Second and
 * later reads on the same offset hit the mapping's folio directly and
 * bypass this kmod entirely — matching s3fs-fuse's -o kernel_cache
 * behavior, and closing the "hot cache" latency gap in the bench.
 *
 * The upcall / arena / kmalloc fallback logic all lives in .read_folio
 * now; the folio serves as the destination buffer for a single page's
 * worth of file content, then the page cache does the copy_to_user.
 */

static void s3fs_folio_fill_from_arena(struct folio *folio, size_t fs_size,
				       loff_t pos,
				       struct mm_struct *mm,
				       u64 uaddr, u64 asize)
{
	size_t remaining, total = 0;
	void *fkva;

	if ((u64)pos >= asize) {
		folio_zero_range(folio, 0, fs_size);
		return;
	}
	remaining = min_t(size_t, fs_size, asize - (u64)pos);

	fkva = kmap_local_folio(folio, 0);
	while (remaining > 0) {
		struct page *ap = NULL;
		void *akva;
		u64 abs = uaddr + (u64)pos + total;
		unsigned long pgoff = abs & (PAGE_SIZE - 1);
		size_t chunk = min_t(size_t, remaining, PAGE_SIZE - pgoff);
		int locked = 1;
		long pinned;

		mmap_read_lock(mm);
		pinned = pin_user_pages_remote(mm, abs & PAGE_MASK, 1, 0,
					       &ap, &locked);
		if (locked)
			mmap_read_unlock(mm);
		if (pinned != 1)
			break; /* short — remainder gets zero-filled below */

		akva = kmap_local_page(ap);
		memcpy((u8 *)fkva + total, (u8 *)akva + pgoff, chunk);
		kunmap_local(akva);
		unpin_user_pages(&ap, 1);

		total     += chunk;
		remaining -= chunk;
	}
	if (total < fs_size)
		memset((u8 *)fkva + total, 0, fs_size - total);
	kunmap_local(fkva);
}

static void s3fs_folio_fill_from_kmalloc(struct folio *folio, size_t fs_size,
					 loff_t pos, struct s3fs_file_data *fd)
{
	void *fkva;
	size_t avail = 0;

	if (!fd) {
		folio_zero_range(folio, 0, fs_size);
		return;
	}
	mutex_lock(&fd->lock);
	if ((size_t)pos < fd->size)
		avail = min_t(size_t, fs_size, fd->size - (size_t)pos);
	fkva = kmap_local_folio(folio, 0);
	if (avail > 0)
		memcpy(fkva, fd->buf + pos, avail);
	if (avail < fs_size)
		memset((u8 *)fkva + avail, 0, fs_size - avail);
	kunmap_local(fkva);
	mutex_unlock(&fd->lock);
}

static int s3fs_ebpf_read_folio(struct file *file, struct folio *folio)
{
	struct inode *inode = folio->mapping->host;
	struct mm_struct *mm = NULL;
	u64 uaddr = 0, asize = 0;
	loff_t pos = folio_pos(folio);
	size_t fs_size = folio_size(folio);

	/* Fast path: admit hit. */
	if (admit_snapshot(inode->i_ino, &uaddr, &asize, &mm) && mm) {
		s3fs_folio_fill_from_arena(folio, fs_size, pos, mm, uaddr, asize);
		goto out;
	}

	/* Miss + daemon alive: trigger upcall, wait, retry. */
	spin_lock(&s3fs_admits_lock);
	mm = s3fs_admitted_mm;
	spin_unlock(&s3fs_admits_lock);

	if (mm) {
		struct s3fs_pending *p = pending_get_or_create(inode->i_ino);

		if (p) {
			int err;
			long remaining;

			err = miss_enqueue(inode->i_ino, pos, fs_size);
			if (err == 0) {
				remaining = wait_for_completion_interruptible_timeout(
					&p->done, S3FS_UPCALL_WAIT_JIFFIES);
				err = p->err;
				pending_put(p);
				if (remaining > 0 && err == 0 &&
				    admit_snapshot(inode->i_ino, &uaddr,
						   &asize, &mm) && mm) {
					s3fs_folio_fill_from_arena(folio,
						fs_size, pos, mm, uaddr, asize);
					goto out;
				}
			} else {
				pending_put(p);
			}
		}
	}

	/* Fallback: per-inode kmalloc buffer. */
	s3fs_folio_fill_from_kmalloc(folio, fs_size, pos, inode->i_private);

out:
	folio_mark_uptodate(folio);
	folio_unlock(folio);
	return 0;
}

static const struct address_space_operations s3fs_ebpf_aops = {
	.read_folio	= s3fs_ebpf_read_folio,
	.dirty_folio	= noop_dirty_folio,
};

static const struct file_operations s3fs_ebpf_file_fops = {
	.llseek		= generic_file_llseek,
	.read_iter	= generic_file_read_iter,
	.write		= s3fs_ebpf_file_write,
	.mmap		= generic_file_mmap,
	.splice_read	= filemap_splice_read,
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
		u64 admit_uaddr, admit_size;
		struct mm_struct *admit_mm;

		fd = s3fs_file_data_new(e->initial_content,
					e->initial_content_size);
		if (!fd) {
			iput(inode);
			return NULL;
		}
		inode->i_private = fd;
		/* Prefer admit size over the compile-time initial so that
		 * generic_file_read_iter reads up to the S3-authoritative
		 * length rather than being truncated to kmod's placeholder
		 * (e.g. 46 bytes for big.bin instead of the real 1 MiB). */
		if (admit_snapshot(e->ino, &admit_uaddr,
				   &admit_size, &admit_mm) && admit_mm)
			inode->i_size = admit_size;
		else
			inode->i_size = fd->size;
		inode->i_op      = &s3fs_ebpf_file_iops;
		inode->i_fop     = &s3fs_ebpf_file_fops;
		inode->i_mapping->a_ops = &s3fs_ebpf_aops;
		/* No dirty tracking: content is either in-arena (read-only
		 * from VFS's view) or in the kmalloc scratch buffer that
		 * .write mutates directly, bypassing the page cache. */
		mapping_set_gfp_mask(inode->i_mapping, GFP_KERNEL);
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
