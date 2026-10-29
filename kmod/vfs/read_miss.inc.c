/* SPDX-License-Identifier: GPL-2.0 */
/* read-miss upcall: pending completions + event queue.  Part of vfs_ops.c: included from there, one translation
 * unit, never compiled on its own. */

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
	u64 chunk_offset;     /* U13: chunk boundary (0 = full-file) */
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

/* U13: readers for different chunks of the same ino get independent
 * completions — every lookup matches on (ino, chunk_offset).
 * P1: the bucket key is the ino alone so all chunks of an ino share a
 * bucket and pending_exists(ino) sees them.  The former
 * ino ^ (chunk_offset >> 22) key scattered chunk != 0 entries into other
 * buckets, letting eviction pick an ino with a reader still blocked on
 * a non-first chunk. */
static u64 pending_hash_key(u64 ino, u64 chunk_offset __always_unused)
{
	return ino;
}

/* Look up an existing pending entry for (ino, chunk_offset) and bump
 * its refcount, or allocate a new one. Caller owns one reference on
 * the returned entry (must pending_put on the way out). */
static struct s3fs_pending *pending_get_or_create(u64 ino, u64 chunk_offset)
{
	struct s3fs_pending *p, *fresh;
	u64 key = pending_hash_key(ino, chunk_offset);

	spin_lock(&s3fs_pending_lock);
	hash_for_each_possible(s3fs_pending, p, node, key) {
		if (p->ino == ino && p->chunk_offset == chunk_offset) {
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
	fresh->ino          = ino;
	fresh->chunk_offset = chunk_offset;
	fresh->err          = 0;
	init_completion(&fresh->done);
	refcount_set(&fresh->refs, 2); /* one for hash, one for caller */

	spin_lock(&s3fs_pending_lock);
	/* Re-check for a racing insert. */
	hash_for_each_possible(s3fs_pending, p, node, key) {
		if (p->ino == ino && p->chunk_offset == chunk_offset) {
			refcount_inc(&p->refs);
			spin_unlock(&s3fs_pending_lock);
			kfree(fresh);
			return p;
		}
	}
	hash_add(s3fs_pending, &fresh->node, key);
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

/* True while a .read_folio is blocked on a miss upcall for `ino`
 * (the entry stays hashed until SIGNAL_ADMIT unhashes it). */
static bool pending_exists(u64 ino)
{
	struct s3fs_pending *p;
	bool found = false;

	spin_lock(&s3fs_pending_lock);
	hash_for_each_possible(s3fs_pending, p, node, ino) {
		if (p->ino == ino) {
			found = true;
			break;
		}
	}
	spin_unlock(&s3fs_pending_lock);
	return found;
}

static int miss_enqueue(u64 ino, loff_t offset, size_t count, u32 flags)
{
	struct s3fs_miss_node *node = kmalloc(sizeof(*node), GFP_KERNEL);

	if (!node)
		return -ENOMEM;
	INIT_LIST_HEAD(&node->link);
	node->ev.ino    = ino;
	node->ev.offset = offset;
	node->ev.count  = (u32)min_t(size_t, count, U32_MAX);
	node->ev.flags  = flags;

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

static bool s3fs_ino_chunk_populated(u64 ino, u64 chunk_offset); /* below */

int s3fs_ebpf_signal_admit(u64 ino, u64 chunk_offset, int err, u32 flags)
{
	struct s3fs_pending *p, *target = NULL;
	u64 key = pending_hash_key(ino, chunk_offset);

	spin_lock(&s3fs_pending_lock);
	hash_for_each_possible(s3fs_pending, p, node, key) {
		if (p->ino == ino && p->chunk_offset == chunk_offset) {
			target = p;
			refcount_inc(&target->refs);
			break;
		}
	}
	spin_unlock(&s3fs_pending_lock);

	if (!target)
		return -ENOENT;

	/* AC-45: a success signal must mean the data is there.  The waiter
	 * registered buf_readers before it created this pending, so once the
	 * chunk is seen populated here it stays until the waiter is done. */
	if (err == 0 && (flags & S3FS_SIGNAL_VERIFY_POPULATED) &&
	    !s3fs_ino_chunk_populated(ino, chunk_offset)) {
		pending_put(target);
		return -EAGAIN;
	}

	target->err = err;
	complete_all(&target->done);
	pending_unhash(target); /* drops the hash's ref */
	pending_put(target);    /* drops our probe ref */
	return 0;
}

/* Drain state on module unload.
 *
 * H-06 invariant: pending_get_or_create hands the caller its own
 * refcount on p (refs=2 on create, refcount_inc on re-use). Shutdown
 * below only drops the HASH's ref (via pending_put after hash_del) —
 * the caller's ref keeps p alive until the waiter wakes from
 * complete_all, reads p->err, and drops its own ref via pending_put.
 * Two-ref protocol makes the classic "waker frees before waiter
 * wakes" UAF impossible: even if CPU A (shutdown) beats CPU B
 * (waiter) to pending_put, refs=caller-ref=1, so p stays live for
 * B's p->err load.
 */
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
 * whichever table is live. `parent_ino` is the directory we're
 * looking under — G-01 filters entries so a subdir lookup doesn't
 * see root-level names. */
static bool find_entry(unsigned long parent_ino, const struct qstr *qname,
		       struct s3fs_entry_snap *out)
{
	if (qname->len > S3FS_SYNC_NAME_MAX)
		return false;
	return resolve_entry_by_name(parent_ino, qname->name, qname->len, out);
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
					const struct s3fs_dyn_index *ix)
{
	struct s3fs_local_entry *le, *tmp;

	list_for_each_entry_safe(le, tmp, &s3fs_local_entries, link) {
		u32 h = dyn_ino_hash(le->ino) & ix->mask, v;

		while ((v = ix->by_ino[h])) {
			const struct s3fs_entry_dyn *e = &entries[v - 1];

			h = (h + 1) & ix->mask;
			if (e->ino != le->ino)
				continue;
			/* G-01: name equality alone is not enough — two files
			 * with basename "foo.txt" under different parents must
			 * not cross-promote. Match parent_ino too. */
			if (norm_parent(e->parent_ino) !=
			    norm_parent(le->parent_ino))
				continue;
			if (e->name_len != le->name_len)
				continue;
			if (memcmp(e->name, le->name, le->name_len) != 0)
				continue;
			list_del(&le->link);
			s3fs_local_nr--;
			kfree(le);
			break;
		}
	}
}

/* Same listing?  A sync that re-sends the bucket unchanged must not cost the
 * cache: shrink_dcache_sb() drops every unused dentry, so every idle inode is
 * evicted and its chunks freed (the pcache content, the capacity accounting
 * and the admit entries' backing data with it).  Order-sensitive: the daemon
 * sends the list in a stable order, and a different order only means one
 * unnecessary shrink. */
static bool dyn_entries_equal(const struct s3fs_entry_dyn *a, u32 na,
			      const struct s3fs_entry_dyn *b, u32 nb)
{
	u32 i;

	if (na != nb)
		return false;
	for (i = 0; i < na; i++) {
		if (a[i].ino != b[i].ino || a[i].parent_ino != b[i].parent_ino ||
		    a[i].size != b[i].size || a[i].mode != b[i].mode ||
		    a[i].name_len != b[i].name_len ||
		    memcmp(a[i].name, b[i].name, a[i].name_len) != 0)
			return false;
	}
	return true;
}

int s3fs_ebpf_entries_replace(struct s3fs_entry_dyn *entries, u32 count)
{
	struct s3fs_entry_dyn *prev;
	struct s3fs_dyn_index *ix = NULL, *prev_ix;
	bool changed = true;
	u32 prev_nr;

	/* Built before the mutex: it is O(count log count). */
	if (count && entries) {
		ix = dyn_index_build(entries, count);
		if (!ix)
			return -ENOMEM;
	}
	mutex_lock(&s3fs_entries_mtx);
	prev = s3fs_dyn_entries;
	prev_ix = s3fs_dyn_idx;
	prev_nr = s3fs_dyn_nr;
	if (ix && prev)
		changed = !dyn_entries_equal(prev, prev_nr, entries, count);
	if (ix) {
		s3fs_dyn_entries = entries;
		s3fs_dyn_nr      = count;
		s3fs_dyn_idx     = ix;
		local_prune_promoted_locked(entries, ix);
	} else {
		s3fs_dyn_entries = NULL;
		s3fs_dyn_nr      = 0;
		s3fs_dyn_idx     = NULL;
	}
	mutex_unlock(&s3fs_entries_mtx);

	/* allocated with kvcalloc by the ioctl */
	kvfree(prev);
	dyn_index_free(prev_ix);

	/* Stale dentries (a changed size, a removed or moved key) are dropped so
	 * the next lookup sees the new listing; an identical listing keeps them. */
	if (changed && s3fs_active_sb)
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

