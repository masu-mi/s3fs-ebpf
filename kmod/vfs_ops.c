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
 * Bootstrap table. Serves the mount when the daemon hasn't sync'd yet
 * (e.g. right after `mount` before `s3fs-ebpf fastpath` connects, or
 * after the daemon exits and clears the dynamic table). The daemon's
 * SYNC_ENTRIES ioctl replaces this with a live bucket snapshot at
 * runtime; a count=0 sync restores the bootstrap so the mount stays
 * browsable.
 */
static const struct s3fs_entry s3fs_bootstrap_entries[] = {
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
#define S3FS_NR_BOOTSTRAP ARRAY_SIZE(s3fs_bootstrap_entries)

/*
 * Live entry table. NULL means "use s3fs_bootstrap_entries";
 * non-NULL means the daemon has pushed a snapshot via
 * S3FS_IOC_SYNC_ENTRIES and readers must consult it instead. Names
 * are embedded in each entry so lifetime tracks the kmalloc'd array.
 *
 * Protected by s3fs_entries_mtx (mutex, not rwlock): the reader paths
 * — lookup and iterate_shared — call dir_emit / d_add which can sleep
 * (page faults during copy_to_user), and a spin/rw lock would forbid
 * that. Contention is fine at PoC scale: the writer (sync ioctl) is
 * infrequent and readers are cheap.
 */
static DEFINE_MUTEX(s3fs_entries_mtx);
static struct s3fs_entry_dyn *s3fs_dyn_entries;
static size_t s3fs_dyn_nr;

/*
 * Locally-created entries (via .create) live here until the next
 * SYNC round from the daemon promotes them into the dynamic table.
 * The daemon-side ino allocator is nudged after a successful PUT so
 * assignIno returns the same ino we picked, giving iterate_shared a
 * single view. When SYNC arrives, entries whose name+ino match a
 * dyn entry are dropped from this list (no duplicates emitted).
 *
 * Protected by s3fs_entries_mtx (same mutex as s3fs_dyn_entries).
 */
struct s3fs_local_entry {
	struct list_head link;
	unsigned long ino;
	umode_t mode;
	u32 name_len;
	char name[S3FS_SYNC_NAME_MAX + 1];
};

static LIST_HEAD(s3fs_local_entries);
static size_t s3fs_local_nr;

/* .create allocates from this range so ino doesn't collide with the
 * daemon's ino allocator (starts at 100). The daemon registers the
 * ino back with its allocator after a successful PUT, so the same
 * ino survives when the entry gets promoted to the dyn table. */
static u64 s3fs_local_next_ino = 1000000;

/*
 * Snapshot copy of one entry, filled under s3fs_entries_mtx and
 * returned to callers who then use the fields without holding the
 * lock. `initial_content` is NULL for dynamic entries (daemon-owned,
 * no compile-time placeholder); make_inode falls back to a 0-byte
 * scratch buffer in that case, and .read_folio serves from the arena
 * once the daemon admits or from the upcall miss path meanwhile.
 */
struct s3fs_entry_snap {
	unsigned long ino;
	u64 size;                  /* 0 = no authoritative size (bootstrap) */
	umode_t mode;
	char name[S3FS_SYNC_NAME_MAX + 1];
	u32 name_len;
	const void *initial_content;
	size_t initial_content_size;
};

static bool resolve_entry_by_name(const char *name, size_t name_len,
				  struct s3fs_entry_snap *out)
{
	struct s3fs_local_entry *le;
	size_t i;

	mutex_lock(&s3fs_entries_mtx);

	/* Local additions win: freshly created files that haven't been
	 * promoted into the dyn table yet must still be found here. */
	list_for_each_entry(le, &s3fs_local_entries, link) {
		if (le->name_len != name_len ||
		    memcmp(le->name, name, name_len) != 0)
			continue;
		out->ino = le->ino;
		out->size = 0; /* .create starts empty; fd->size covers subsequent writes */
		out->mode = le->mode;
		out->name_len = le->name_len;
		memcpy(out->name, le->name, le->name_len);
		out->name[le->name_len] = '\0';
		out->initial_content = NULL;
		out->initial_content_size = 0;
		mutex_unlock(&s3fs_entries_mtx);
		return true;
	}

	if (s3fs_dyn_entries) {
		for (i = 0; i < s3fs_dyn_nr; i++) {
			const struct s3fs_entry_dyn *e = &s3fs_dyn_entries[i];

			if (e->name_len != name_len ||
			    memcmp(e->name, name, name_len) != 0)
				continue;
			out->ino = e->ino;
			out->size = e->size;
			out->mode = e->mode;
			out->name_len = e->name_len;
			memcpy(out->name, e->name, e->name_len);
			out->name[e->name_len] = '\0';
			out->initial_content = NULL;
			out->initial_content_size = 0;
			mutex_unlock(&s3fs_entries_mtx);
			return true;
		}
		mutex_unlock(&s3fs_entries_mtx);
		return false;
	}
	for (i = 0; i < S3FS_NR_BOOTSTRAP; i++) {
		const struct s3fs_entry *e = &s3fs_bootstrap_entries[i];
		size_t elen = strlen(e->name);

		if (elen != name_len || memcmp(e->name, name, name_len) != 0)
			continue;
		out->ino = e->ino;
		out->size = 0; /* bootstrap: no authoritative size — fall back to fd->size */
		out->mode = e->mode;
		out->name_len = elen;
		memcpy(out->name, e->name, elen);
		out->name[elen] = '\0';
		out->initial_content = e->initial_content;
		out->initial_content_size = e->initial_content_size;
		mutex_unlock(&s3fs_entries_mtx);
		return true;
	}
	mutex_unlock(&s3fs_entries_mtx);
	return false;
}

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
			/* Drop any stale page cache filled from the kmalloc
			 * fallback path (or from a prior admit with a
			 * different size). Uses folio_trylock, so folios
			 * held by a concurrent .read_folio waiting on the
			 * upcall are skipped rather than deadlocked — those
			 * are pre-uptodate anyway and will be filled from
			 * the arena on the retry after signal_admit fires. */
			invalidate_mapping_pages(inode->i_mapping, 0, -1);
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

/* Backward-compatible wrapper used by lookup: fills `out` from
 * whichever table is live. Returns true on hit. */
static bool find_entry(const struct qstr *qname, struct s3fs_entry_snap *out)
{
	if (qname->len > S3FS_SYNC_NAME_MAX)
		return false;
	return resolve_entry_by_name(qname->name, qname->len, out);
}

/*
 * s3fs_ebpf_entries_replace — swap the live entry table.
 *
 * If `count == 0` (or `entries == NULL`), drop the dynamic table and
 * revert to the bootstrap set so the mount stays browsable after
 * daemon shutdown. Otherwise take ownership of `entries` (a kmalloc'd
 * array of `count` elements). Old dynamic array is kfree'd here.
 *
 * The dcache holds inodes minted from the previous table, so we
 * shrink it — a big hammer but correct for the PoC: negative dentries
 * are dropped so stale-lookup ENOENT clears, and stale positive
 * dentries are re-resolved on next access. i_size / content-visibility
 * of a still-admitted ino is handled by admit_replace's per-inode
 * invalidate.
 */
/* Called with s3fs_entries_mtx held. Drop any local additions whose
 * name+ino match an entry in the fresh dyn snapshot — the daemon has
 * promoted them (via ino-allocator registration + next list). Local
 * additions with a mismatched ino stay put so a rogue basename
 * collision doesn't silently eat someone's write. */
static void local_prune_promoted_locked(const struct s3fs_entry_dyn *entries,
					size_t count)
{
	struct s3fs_local_entry *le, *tmp;
	size_t i;

	list_for_each_entry_safe(le, tmp, &s3fs_local_entries, link) {
		for (i = 0; i < count; i++) {
			if (entries[i].ino != le->ino)
				continue;
			if (entries[i].name_len != le->name_len)
				continue;
			if (memcmp(entries[i].name, le->name,
				   le->name_len) != 0)
				continue;
			list_del(&le->link);
			s3fs_local_nr--;
			kfree(le);
			break;
		}
	}
}

int s3fs_ebpf_entries_replace(struct s3fs_entry_dyn *entries, u32 count)
{
	struct s3fs_entry_dyn *prev;

	mutex_lock(&s3fs_entries_mtx);
	prev = s3fs_dyn_entries;
	if (count && entries) {
		s3fs_dyn_entries = entries;
		s3fs_dyn_nr      = count;
		local_prune_promoted_locked(entries, count);
	} else {
		s3fs_dyn_entries = NULL;
		s3fs_dyn_nr      = 0;
	}
	mutex_unlock(&s3fs_entries_mtx);

	kfree(prev);

	if (s3fs_active_sb)
		shrink_dcache_sb(s3fs_active_sb);
	return 0;
}

/* Drop all local additions — called on module unload. Individual
 * inodes are torn down through the usual evict path. */
static void local_entries_drain(void)
{
	struct s3fs_local_entry *le, *tmp;

	mutex_lock(&s3fs_entries_mtx);
	list_for_each_entry_safe(le, tmp, &s3fs_local_entries, link) {
		list_del(&le->link);
		kfree(le);
	}
	s3fs_local_nr = 0;
	mutex_unlock(&s3fs_entries_mtx);
}

void s3fs_ebpf_entries_shutdown(void)
{
	(void)s3fs_ebpf_entries_replace(NULL, 0);
	local_entries_drain();
}

/* -- Per-inode content buffer -------------------------------------------- */

struct s3fs_file_data {
	struct mutex lock;
	u8    *buf;
	size_t size;
	size_t cap;
	bool   dirty;   /* set by .write, cleared after successful daemon flush */
};

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
	u64 size;
	u32 name_len;
	char name[S3FS_SYNC_NAME_MAX + 1];
};

static LIST_HEAD(s3fs_flush_queue);
static DEFINE_SPINLOCK(s3fs_flush_lock);

static void flush_enqueue(u64 ino, u64 size,
			  const char *name, size_t name_len)
{
	struct s3fs_flush_item *f;

	if (name_len == 0 || name_len > S3FS_SYNC_NAME_MAX)
		return;
	f = kzalloc(sizeof(*f), GFP_KERNEL);
	if (!f)
		return;
	f->ino = ino;
	f->size = size;
	f->name_len = name_len;
	memcpy(f->name, name, name_len);
	spin_lock(&s3fs_flush_lock);
	list_add_tail(&f->link, &s3fs_flush_queue);
	spin_unlock(&s3fs_flush_lock);
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
 * they don't linger past a daemon restart. */
static void flush_queue_drain(void)
{
	struct s3fs_flush_item *f, *tmp;

	spin_lock(&s3fs_flush_lock);
	list_for_each_entry_safe(f, tmp, &s3fs_flush_queue, link) {
		list_del(&f->link);
		kfree(f);
	}
	spin_unlock(&s3fs_flush_lock);
}

int s3fs_ebpf_flush_pop(struct s3fs_flush_event *out)
{
	struct s3fs_flush_item item;

	if (!flush_dequeue(&item))
		return -EAGAIN;
	memset(out, 0, sizeof(*out));
	out->ino      = item.ino;
	out->size     = item.size;
	out->name_len = item.name_len;
	memcpy(out->name, item.name, item.name_len);
	out->name[item.name_len] = '\0';
	return 0;
}

long s3fs_ebpf_read_dirty(u64 ino, void __user *uaddr, u64 cap)
{
	struct inode *inode;
	struct s3fs_file_data *fd;
	size_t n;
	long ret;

	if (!s3fs_active_sb)
		return -ENODEV;
	inode = ilookup(s3fs_active_sb, ino);
	if (!inode)
		return -ENOENT;
	fd = inode->i_private;
	if (!fd) {
		iput(inode);
		return -EIO;
	}

	mutex_lock(&fd->lock);
	n = min_t(size_t, fd->size, cap);
	if (n && copy_to_user(uaddr, fd->buf, n)) {
		ret = -EFAULT;
	} else {
		ret = (long)n;
	}
	mutex_unlock(&fd->lock);
	iput(inode);
	return ret;
}

int s3fs_ebpf_mark_dirty(u64 ino)
{
	struct inode *inode;
	struct s3fs_file_data *fd;

	if (!s3fs_active_sb)
		return -ENODEV;
	inode = ilookup(s3fs_active_sb, ino);
	if (!inode)
		return -ENOENT;
	fd = inode->i_private;
	if (!fd) {
		iput(inode);
		return -EIO;
	}
	mutex_lock(&fd->lock);
	fd->dirty = true;
	mutex_unlock(&fd->lock);
	iput(inode);
	return 0;
}

static struct s3fs_file_data *s3fs_file_data_new(const void *initial,
						 size_t initial_size)
{
	struct s3fs_file_data *fd = kzalloc(sizeof(*fd), GFP_KERNEL);

	if (!fd)
		return NULL;
	mutex_init(&fd->lock);

	if (initial_size) {
		fd->buf = kvmalloc(initial_size, GFP_KERNEL);
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
	kvfree(fd->buf);
	kfree(fd);
}

/* Ensure `fd->cap >= needed`. Caller holds fd->lock.
 *
 * Uses kvmalloc + memcpy instead of krealloc so growth beyond the
 * MAX_ORDER (~4 MiB on x86_64) bucket falls back to vmalloc rather
 * than triggering the page allocator's "order too large" WARN. dd
 * bs=10M count=1 was reproducing that WARN with plain krealloc. */
static int s3fs_file_data_reserve(struct s3fs_file_data *fd, size_t needed)
{
	size_t new_cap;
	u8 *newbuf;

	if (needed <= fd->cap)
		return 0;

	/* Round up to 4 KiB slab class for small growth; > 4 MiB rounds
	 * to 2 MiB (vmalloc granularity) to keep amortized alloc cheap. */
	if (needed >= (4UL << 20))
		new_cap = round_up(needed, 2UL << 20);
	else
		new_cap = round_up(needed, 4096);

	newbuf = kvmalloc(new_cap, GFP_KERNEL);
	if (!newbuf)
		return -ENOMEM;
	if (fd->buf) {
		memcpy(newbuf, fd->buf, fd->size);
		kvfree(fd->buf);
	}
	memset(newbuf + fd->size, 0, new_cap - fd->size);
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
	fd->dirty = true;
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
			fd->dirty = true;
		} else if (new_size > (loff_t)fd->size) {
			err = s3fs_file_data_reserve(fd, new_size);
			if (err) {
				mutex_unlock(&fd->lock);
				return err;
			}
			fd->size = new_size;
			fd->dirty = true;
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

/*
 * If `fd->dirty`, snapshot ino/size/name under fd->lock and enqueue a
 * flush event. dirty is cleared here so a subsequent close/fsync
 * without new writes is a no-op. The daemon PUT is fire-and-forget
 * from kmod's POV; a failed PUT stays best-effort at PoC scope.
 */
static void s3fs_maybe_enqueue_flush(struct file *file)
{
	struct inode *inode = file_inode(file);
	struct s3fs_file_data *fd = inode->i_private;
	struct dentry *dentry = file->f_path.dentry;
	u64 size;
	char name[S3FS_SYNC_NAME_MAX + 1];
	size_t name_len;

	if (!fd || !dentry)
		return;

	name_len = dentry->d_name.len;
	if (name_len == 0 || name_len > S3FS_SYNC_NAME_MAX)
		return;
	memcpy(name, dentry->d_name.name, name_len);

	mutex_lock(&fd->lock);
	if (!fd->dirty) {
		mutex_unlock(&fd->lock);
		return;
	}
	size = fd->size;
	fd->dirty = false;
	mutex_unlock(&fd->lock);

	flush_enqueue(inode->i_ino, size, name, name_len);
}

static int s3fs_ebpf_file_release(struct inode *inode, struct file *file)
{
	s3fs_maybe_enqueue_flush(file);
	return 0;
}

static int s3fs_ebpf_file_fsync(struct file *file, loff_t start,
				loff_t end, int datasync)
{
	s3fs_maybe_enqueue_flush(file);
	return 0;
}

static const struct file_operations s3fs_ebpf_file_fops = {
	.llseek		= generic_file_llseek,
	.read_iter	= generic_file_read_iter,
	.write		= s3fs_ebpf_file_write,
	.mmap		= generic_file_mmap,
	.splice_read	= filemap_splice_read,
	.release	= s3fs_ebpf_file_release,
	.fsync		= s3fs_ebpf_file_fsync,
};

static const struct inode_operations s3fs_ebpf_file_iops = {
	.getattr	= simple_getattr,
	.setattr	= s3fs_ebpf_file_setattr,
};

/* -- Directory / super ops ------------------------------------------------ */

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
		 *   1. current admit (arena has the exact bytes)
		 *   2. sync entry's size (S3-authoritative but not yet in arena)
		 *   3. fd->size (compile-time placeholder for bootstrap)
		 * Without (2), a newly sync'd entry would report i_size=0
		 * and generic_file_read_iter would short-circuit before
		 * .read_folio could upcall the daemon. */
		if (admit_snapshot(e->ino, &admit_uaddr,
				   &admit_size, &admit_mm) && admit_mm)
			inode->i_size = admit_size;
		else if (e->size > 0)
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
	} else {
		/* Only regular files in the entry table for now. */
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

	if (!find_entry(&dentry->d_name, &snap)) {
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
	le->ino      = ino;
	le->mode     = S_IFREG | (mode & 0777);
	le->name_len = nlen;
	memcpy(le->name, dentry->d_name.name, nlen);
	list_add_tail(&le->link, &s3fs_local_entries);
	s3fs_local_nr++;
	mutex_unlock(&s3fs_entries_mtx);

	snap.ino      = ino;
	snap.size     = 0;
	snap.mode     = le->mode;
	snap.name_len = nlen;
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
	 * even without a subsequent write. The dentry name doubles as
	 * the S3 key at PoC scope (flat bucket). */
	flush_enqueue(ino, 0, le->name, nlen);
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
 * Emit ordering: dyn entries (or bootstrap when no dyn) first,
 * then local additions. `idx` walks the concatenation. This means
 * a `ls` after `touch newfile` shows newfile at the end — good
 * enough for PoC; sorted layout can come with the wider readdir
 * cleanup.
 */
static bool emit_one_entry(struct dir_context *ctx, size_t idx)
{
	unsigned long ino;
	unsigned char dt;
	char name[S3FS_SYNC_NAME_MAX + 1];
	u32 name_len;
	size_t base_nr;
	bool ok = false;

	mutex_lock(&s3fs_entries_mtx);
	base_nr = s3fs_dyn_entries ? s3fs_dyn_nr : S3FS_NR_BOOTSTRAP;
	if (idx < base_nr) {
		if (s3fs_dyn_entries) {
			const struct s3fs_entry_dyn *e = &s3fs_dyn_entries[idx];

			ino      = e->ino;
			dt       = S_ISDIR(e->mode) ? DT_DIR : DT_REG;
			name_len = e->name_len;
			memcpy(name, e->name, name_len);
		} else {
			const struct s3fs_entry *e =
				&s3fs_bootstrap_entries[idx];
			size_t elen = strlen(e->name);

			if (elen > S3FS_SYNC_NAME_MAX)
				elen = S3FS_SYNC_NAME_MAX;
			ino      = e->ino;
			dt       = S_ISDIR(e->mode) ? DT_DIR : DT_REG;
			name_len = elen;
			memcpy(name, e->name, elen);
		}
		ok = true;
	} else if (idx < base_nr + s3fs_local_nr) {
		size_t local_idx = idx - base_nr;
		struct s3fs_local_entry *le;
		size_t i = 0;

		list_for_each_entry(le, &s3fs_local_entries, link) {
			if (i++ != local_idx)
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
	if (!dir_emit_dots(file, ctx))
		return 0;

	/* ctx->pos: 0=".", 1="..", 2..N+1 = table entries. */
	while (emit_one_entry(ctx, ctx->pos - 2))
		ctx->pos++;
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
	/* Queued flush events reference inos on this sb; drop them so a
	 * later mount doesn't see them. */
	flush_queue_drain();
}

static const struct super_operations s3fs_ebpf_super_ops = {
	.statfs		= simple_statfs,
	.drop_inode	= generic_delete_inode,
	.evict_inode	= s3fs_ebpf_evict_inode,
	.put_super	= s3fs_ebpf_put_super,
};

static const struct inode_operations s3fs_ebpf_dir_iops = {
	.lookup		= s3fs_ebpf_lookup,
	.create		= s3fs_ebpf_create,
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
