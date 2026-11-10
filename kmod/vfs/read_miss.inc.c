/* SPDX-License-Identifier: GPL-2.0 */
/* inflight table (what syscalls wait on) + MISS queueing + DONE; also the entry table helpers.  Part of vfs_ops.c: included from there, one translation
 * unit, never compiled on its own. */

/* -- inflight: what a waiting syscall sleeps on until the daemon's DONE ----- */
/*
 * One table, one lock, for everything kmod makes a syscall wait for (v2 Phase 4):
 *
 *   MISS  — a reader blocked on a chunk (.read_folio), an open check, or a load
 *           for write.  Keyed by (ino, chunk_offset, cls); readers of the same key
 *           share one record.  Only the reader that CREATES the record queues the
 *           request (Q-1), and the record remembers its seq, so a DONE that names
 *           another seq cannot complete it (Q-2).
 *   FLUSH — fsync()s blocked on the daemon's PUT of an inode.  One record per ino;
 *           fsyncs share it.  A flush DONE is by ino: the daemon's debounce
 *           collapses several close() flushes into one PUT.
 *
 * The request itself is in reqq (reqq.inc.c).  Both kinds are completed by the one
 * DONE ioctl (s3fs_ebpf_done): before this it was SIGNAL_ADMIT, SIGNAL_FLUSH and
 * MARK_DIRTY, and two hash tables with two locks.
 *
 * .read on an uncached chunk:  get_or_create  → (creator) queue MISS  →
 *                              wait_for_completion  → retry.
 * DONE ioctl:                  find the record  → complete + err.
 * Char dev .read (daemon):     pop the request at the head, block if empty.
 */

struct s3fs_inflight {
	struct hlist_node node;
	u64 ino;
	u64 chunk_offset;     /* MISS: chunk boundary (0 = full-file) or S3FS_OPEN_CHECK_KEY; FLUSH: S3FS_FLUSH_KEY */
	u64 seq;              /* MISS: seq of the request queued for this record */
	u32 type;             /* S3FS_REQ_MISS | S3FS_REQ_FLUSH */
	u32 cls;              /* MISS: S3FS_MISS_FLAG_FOR_WRITE or 0 (separate records) */
	struct completion done;
	int err;
	refcount_t refs;
};

/* pending key of a flush waiter: not a multiple of the chunk size, never an open
 * check key (all ones) and never a chunk. */
#define S3FS_FLUSH_KEY ((u64)-2)

/* MISS seqs: S3FS_REQ_SEQ_MISS | n, apart from the write-back seqs (AC-84, applied_seq). */
static atomic64_t s3fs_miss_seq = ATOMIC64_INIT(0);

#define S3FS_INFLIGHT_HASH_BITS 4  /* 16 buckets — few concurrent misses and fsyncs */

static DEFINE_HASHTABLE(s3fs_inflight, S3FS_INFLIGHT_HASH_BITS);
static DEFINE_SPINLOCK(s3fs_inflight_lock);

/* P1: the bucket key is the ino alone so all records of an ino share a bucket
 * and inflight_miss_exists(ino) sees them all.  (A key mixing in the chunk offset
 * scattered chunk != 0 entries into other buckets, letting eviction pick an ino
 * with a reader still blocked on a non-first chunk.) */
static struct s3fs_inflight *inflight_find_locked(u32 type, u64 ino, u64 chunk_offset, u32 cls)
{
	struct s3fs_inflight *p;

	hash_for_each_possible(s3fs_inflight, p, node, ino)
		if (p->ino == ino && p->type == type &&
		    p->chunk_offset == chunk_offset && p->cls == cls)
			return p;
	return NULL;
}

/* Look up the record for (type, ino, chunk_offset, cls) and bump its refcount, or
 * allocate a new one (*created = true).  The caller owns one reference on the
 * returned record (must inflight_put on the way out).  Only the creator queues a
 * request for it. */
static struct s3fs_inflight *inflight_get_or_create(u32 type, u64 ino, u64 chunk_offset,
						     u32 cls, bool *created)
{
	struct s3fs_inflight *p, *fresh;

	if (created)
		*created = false;
	spin_lock(&s3fs_inflight_lock);
	p = inflight_find_locked(type, ino, chunk_offset, cls);
	if (p) {
		refcount_inc(&p->refs);
		spin_unlock(&s3fs_inflight_lock);
		return p;
	}
	spin_unlock(&s3fs_inflight_lock);

	fresh = kmalloc(sizeof(*fresh), GFP_KERNEL);
	if (!fresh)
		return NULL;
	INIT_HLIST_NODE(&fresh->node);
	fresh->ino          = ino;
	fresh->chunk_offset = chunk_offset;
	fresh->seq          = 0;
	fresh->type         = type;
	fresh->cls          = cls;
	fresh->err          = 0;
	init_completion(&fresh->done);
	refcount_set(&fresh->refs, 2); /* one for hash, one for caller */

	spin_lock(&s3fs_inflight_lock);
	/* Re-check for a racing insert. */
	p = inflight_find_locked(type, ino, chunk_offset, cls);
	if (p) {
		refcount_inc(&p->refs);
		spin_unlock(&s3fs_inflight_lock);
		kfree(fresh);
		return p;
	}
	hash_add(s3fs_inflight, &fresh->node, ino);
	spin_unlock(&s3fs_inflight_lock);
	if (created)
		*created = true;
	return fresh;
}

static void inflight_put(struct s3fs_inflight *p)
{
	if (!p || !refcount_dec_and_test(&p->refs))
		return;
	kfree(p);
}

/* Remove the record from the hash (if it is still there), so the next miss for the
 * same key starts fresh.  Called by DONE after completing it, and by a waiter that
 * gave up (the daemon never answered). */
static void inflight_unhash(struct s3fs_inflight *p)
{
	spin_lock(&s3fs_inflight_lock);
	if (!hlist_unhashed(&p->node)) {
		hash_del(&p->node);
		spin_unlock(&s3fs_inflight_lock);
		inflight_put(p); /* drop the hash's reference */
		return;
	}
	spin_unlock(&s3fs_inflight_lock);
}

/* The creator could not queue its request: nobody will answer, so wake the
 * joiners (and the creator) with err instead of letting them time out. */
static void inflight_fail(struct s3fs_inflight *p, int err)
{
	p->err = err;
	complete_all(&p->done);
	inflight_unhash(p);
}

/* True while a .read_folio is blocked on a miss upcall for `ino` (the record
 * stays hashed until DONE unhashes it).  A flush waiter does not count: it does
 * not read the inode's chunks. */
static bool inflight_miss_exists(u64 ino)
{
	struct s3fs_inflight *p;
	bool found = false;

	spin_lock(&s3fs_inflight_lock);
	hash_for_each_possible(s3fs_inflight, p, node, ino) {
		if (p->ino == ino && p->type == S3FS_REQ_MISS) {
			found = true;
			break;
		}
	}
	spin_unlock(&s3fs_inflight_lock);
	return found;
}

struct s3fs_file_data;
static void s3fs_fd_read_pattern(struct s3fs_file_data *fd, u32 *seq, u32 *rnd);

/* `fd` (may be NULL) supplies the read pattern the daemon uses to decide
 * between a chunk and the whole file.  `seq` is the record's (inflight_new_seq). */
static u64 inflight_new_seq(void)
{
	return S3FS_REQ_SEQ_MISS | (u64)atomic64_inc_return(&s3fs_miss_seq);
}

static int miss_enqueue(u64 ino, loff_t offset, size_t count, u32 flags,
			struct s3fs_file_data *fd, u64 seq)
{
	struct s3fs_req_item *node = req_item_new(S3FS_REQ_MISS, ino, 0);

	if (!node)
		return -ENOMEM;
	node->ev.seq    = seq;
	node->ev.offset = offset;
	node->ev.count  = (u32)min_t(size_t, count, U32_MAX);
	node->ev.flags  = flags;
	s3fs_fd_read_pattern(fd, &node->ev.seq_count, &node->ev.rand_count);
	req_enqueue(node);
	return 0;
}

/* Queue the MISS for a record this caller created, or do nothing for a joiner
 * (Q-1: one request per record).  The seq is stored in the record BEFORE the
 * request is visible, so a fast DONE finds it. */
static int inflight_queue_miss(struct s3fs_inflight *p, bool created, loff_t offset,
			       size_t count, u32 flags, struct s3fs_file_data *fd)
{
	int err;

	if (!created)
		return 0;
	p->seq = inflight_new_seq();
	err = miss_enqueue(p->ino, offset, count, flags, fd, p->seq);
	if (err)
		inflight_fail(p, err);
	return err;
}

static bool s3fs_ino_chunk_populated(u64 ino, u64 chunk_offset); /* below */

/* DONE for a MISS.  The record is found by (ino, chunk_offset); with a seq it must
 * be the record that seq was queued for (Q-2: a stale handler cannot complete a
 * newer record, -ENOENT), without one (seq 0, a chunk load signalling a chunk
 * whose own request it has not seen) the plain-read record is meant. */
static int s3fs_done_miss(const struct s3fs_done *d)
{
	struct s3fs_inflight *p, *target = NULL;

	spin_lock(&s3fs_inflight_lock);
	hash_for_each_possible(s3fs_inflight, p, node, d->ino) {
		if (p->ino != d->ino || p->type != S3FS_REQ_MISS ||
		    p->chunk_offset != d->chunk_offset)
			continue;
		if (d->seq ? p->seq == d->seq : p->cls == 0) {
			target = p;
			refcount_inc(&target->refs);
			break;
		}
	}
	spin_unlock(&s3fs_inflight_lock);

	if (!target)
		return -ENOENT;

	/* AC-45: a success signal must mean the data is there.  The waiter
	 * registered buf_readers before it created this record, so once the
	 * chunk is seen populated here it stays until the waiter is done. */
	if (d->err == 0 && (d->flags & S3FS_DONE_VERIFY_POPULATED) &&
	    !s3fs_ino_chunk_populated(d->ino, d->chunk_offset)) {
		inflight_put(target);
		return -EAGAIN;
	}

	target->err = d->err;
	complete_all(&target->done);
	inflight_unhash(target); /* drops the hash's ref */
	inflight_put(target);    /* drops our probe ref */
	return 0;
}

static int s3fs_done_flush(u64 ino, int err);	/* flush_queue.inc.c */

/* The one completion ioctl (S3FS_IOC_DONE): the daemon is done with a request. */
int s3fs_ebpf_done(const struct s3fs_done *d)
{
	switch (d->type) {
	case S3FS_REQ_MISS:
		return s3fs_done_miss(d);
	case S3FS_REQ_FLUSH:
		return s3fs_done_flush(d->ino, d->err);
	default:
		return -EINVAL;
	}
}

/* Drain state on module unload.
 *
 * H-06 invariant: inflight_get_or_create hands the caller its own
 * refcount on p (refs=2 on create, refcount_inc on re-use). Shutdown
 * below only drops the HASH's ref (via inflight_put after hash_del) —
 * the caller's ref keeps p alive until the waiter wakes from
 * complete_all, reads p->err, and drops its own ref via inflight_put.
 * Two-ref protocol makes the classic "waker frees before waiter
 * wakes" UAF impossible: even if CPU A (shutdown) beats CPU B
 * (waiter) to inflight_put, refs=caller-ref=1, so p stays live for
 * B's p->err load.
 */
static void s3fs_ebpf_upcall_shutdown(void)
{
	struct s3fs_inflight *p;
	struct hlist_node *tmp;
	unsigned int i;

	/* Fail every waiter (misses and fsyncs) so they wake up and return. */
	spin_lock(&s3fs_inflight_lock);
	hash_for_each_safe(s3fs_inflight, i, tmp, p, node) {
		hash_del(&p->node);
		p->err = p->type == S3FS_REQ_MISS ? -ESHUTDOWN : -EIO;
		complete_all(&p->done);
		inflight_put(p);
	}
	spin_unlock(&s3fs_inflight_lock);

	reqq_drain(S3FS_REQ_MISS);
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
					const struct s3fs_dyn_index *ix,
					u64 applied_seq)
{
	struct s3fs_local_entry *le, *tmp;

	list_for_each_entry_safe(le, tmp, &s3fs_local_entries, link) {
		u32 h = dyn_ino_hash(le->ino) & ix->mask, v;

		/* AC-85: a listing taken before the daemon applied the event that made
		 * this entry may show a state of S3 that a later operation changes
		 * (mv a b; mv b a: the listing has a, the next one only b).  Promote
		 * only when the listing reflects the entry's own operation. */
		if (le->seq > applied_seq)
			continue;

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

/*
 * Drop the tombstones the new listing has made unnecessary: those whose
 * operation the daemon had applied (seq <= applied_seq) and whose entry the
 * listing no longer has.  `ix` is NULL for an empty listing.  s3fs_entries_mtx
 * held, s3fs_dyn_entries / s3fs_dyn_idx already the new ones.
 */
static void hidden_prune_locked(u64 applied_seq)
{
	struct s3fs_hidden_entry *h, *tmp;

	list_for_each_entry_safe(h, tmp, &s3fs_hidden_entries, link) {
		const struct s3fs_entry_dyn *e = NULL;

		if (h->seq > applied_seq)
			continue;
		if (s3fs_dyn_entries)
			e = dyn_find_name(norm_parent(h->parent_ino), h->name,
					  h->name_len);
		if (e && e->ino == h->ino)
			continue;	/* S3 still lists it: keep hiding */
		list_del(&h->link);
		s3fs_hidden_nr--;
		kfree(h);
	}
}

int s3fs_ebpf_entries_replace(struct s3fs_entry_dyn *entries, u32 count)
{
	/* Callers without the daemon's report treat every operation as applied. */
	return s3fs_ebpf_entries_replace_applied(entries, count, U64_MAX);
}

int s3fs_ebpf_entries_replace_applied(struct s3fs_entry_dyn *entries, u32 count,
				      u64 applied_seq)
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
		local_prune_promoted_locked(entries, ix, applied_seq);
	} else {
		s3fs_dyn_entries = NULL;
		s3fs_dyn_nr      = 0;
		s3fs_dyn_idx     = NULL;
	}
	hidden_prune_locked(applied_seq);
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

	struct s3fs_hidden_entry *h, *htmp;

	mutex_lock(&s3fs_entries_mtx);
	list_for_each_entry_safe(le, tmp, &s3fs_local_entries, link) {
		list_del(&le->link);
		kfree(le);
	}
	s3fs_local_nr = 0;
	list_for_each_entry_safe(h, htmp, &s3fs_hidden_entries, link) {
		list_del(&h->link);
		kfree(h);
	}
	s3fs_hidden_nr = 0;
	mutex_unlock(&s3fs_entries_mtx);
}

