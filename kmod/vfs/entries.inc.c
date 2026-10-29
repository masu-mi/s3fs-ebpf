/* SPDX-License-Identifier: GPL-2.0 */
/* bootstrap / dynamic / local entry tables, name index, resolve.  Part of vfs_ops.c: included from there, one translation
 * unit, never compiled on its own. */



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
 * AC-55: indexes over s3fs_dyn_entries, built by s3fs_ebpf_entries_replace
 * before it takes the mutex and swapped in with the array.  Without them a
 * lookup scanned the whole table, so `ls -l` of a directory with N entries
 * cost O(N^2) (every lookup, plus a rescan per readdir position).
 *
 *  by_name  open-addressing table of entry index + 1 (0 = empty), keyed by
 *           (parent_ino, name); load factor <= 1/2.
 *  by_ino   the same keyed by ino.  Duplicates are all kept (probe until an
 *           empty slot), because record_size updates every match.
 *  order    entry indexes sorted by (parent_ino, index): the children of one
 *           directory are contiguous and keep the daemon's order, so readdir
 *           position `idx` is order[first_child + idx].
 *
 * Protected by s3fs_entries_mtx like the array it indexes.
 */
struct s3fs_dyn_index {
	u32 mask;      /* slots - 1; slots is a power of two >= 2 * nr */
	u32 *by_name;
	u32 *by_ino;
	u32 *order;
};
static struct s3fs_dyn_index *s3fs_dyn_idx;

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
	unsigned long parent_ino; /* G-01: dir the file was .create'd under */
	umode_t mode;
	u64 size;   /* W2: last i_size seen at evict_inode (0 until then) */
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
	unsigned long parent_ino;  /* G-01: 0 == root == S3FS_ROOT_INO */
	u64 size;                  /* 0 = no authoritative size (bootstrap) */
	umode_t mode;
	char name[S3FS_SYNC_NAME_MAX + 1];
	u32 name_len;
	const void *initial_content;
	size_t initial_content_size;
};

/* G-01: parent_ino=0 means "root". Bootstrap entries live at root,
 * dyn entries carry their own parent_ino, local additions were
 * recorded with the dir ino they were .create'd under. */
static inline unsigned long norm_parent(unsigned long p)
{
	return p ? p : (unsigned long)S3FS_ROOT_INO;
}

static u32 dyn_name_hash(unsigned long parent, const char *name, u32 len)
{
	return jhash(name, len, (u32)parent);
}

static u32 dyn_ino_hash(unsigned long ino)
{
	return hash_long(ino, 32);
}

static void dyn_index_free(struct s3fs_dyn_index *ix)
{
	if (!ix)
		return;
	kvfree(ix->by_name);
	kvfree(ix->by_ino);
	kvfree(ix->order);
	kfree(ix);
}

static int dyn_order_cmp(const void *a, const void *b, const void *priv)
{
	const struct s3fs_entry_dyn *e = priv;
	u32 x = *(const u32 *)a, y = *(const u32 *)b;
	unsigned long px = norm_parent(e[x].parent_ino);
	unsigned long py = norm_parent(e[y].parent_ino);

	if (px != py)
		return px < py ? -1 : 1;
	return x < y ? -1 : x > y;
}

static struct s3fs_dyn_index *dyn_index_build(const struct s3fs_entry_dyn *e,
					      u32 n)
{
	struct s3fs_dyn_index *ix;
	u32 slots = roundup_pow_of_two(max_t(u32, 2 * n, 16)), i;

	ix = kzalloc(sizeof(*ix), GFP_KERNEL);
	if (!ix)
		return NULL;
	ix->mask = slots - 1;
	ix->by_name = kvcalloc(slots, sizeof(u32), GFP_KERNEL);
	ix->by_ino = kvcalloc(slots, sizeof(u32), GFP_KERNEL);
	ix->order = kvmalloc_array(n, sizeof(u32), GFP_KERNEL);
	if (!ix->by_name || !ix->by_ino || !ix->order) {
		dyn_index_free(ix);
		return NULL;
	}
	for (i = 0; i < n; i++) {
		u32 h = dyn_name_hash(norm_parent(e[i].parent_ino), e[i].name,
				      e[i].name_len) & ix->mask;

		while (ix->by_name[h])
			h = (h + 1) & ix->mask;
		ix->by_name[h] = i + 1;
		h = dyn_ino_hash(e[i].ino) & ix->mask;
		while (ix->by_ino[h])
			h = (h + 1) & ix->mask;
		ix->by_ino[h] = i + 1;
		ix->order[i] = i;
	}
	sort_r(ix->order, n, sizeof(u32), dyn_order_cmp, NULL, e);
	return ix;
}

/* s3fs_entries_mtx held, s3fs_dyn_entries non-NULL. */
static const struct s3fs_entry_dyn *dyn_find_name(unsigned long parent,
						  const char *name, size_t len)
{
	u32 h = dyn_name_hash(parent, name, len) & s3fs_dyn_idx->mask, v;

	while ((v = s3fs_dyn_idx->by_name[h])) {
		const struct s3fs_entry_dyn *e = &s3fs_dyn_entries[v - 1];

		if (norm_parent(e->parent_ino) == parent &&
		    e->name_len == len && memcmp(e->name, name, len) == 0)
			return e;
		h = (h + 1) & s3fs_dyn_idx->mask;
	}
	return NULL;
}

/* The children of `parent` are order[*first, *first + *cnt). */
static void dyn_children(unsigned long parent, u32 *first, u32 *cnt)
{
	const u32 *order = s3fs_dyn_idx->order;
	u32 lo = 0, hi = s3fs_dyn_nr, up;

	while (lo < hi) {
		u32 mid = lo + (hi - lo) / 2;

		if (norm_parent(s3fs_dyn_entries[order[mid]].parent_ino) < parent)
			lo = mid + 1;
		else
			hi = mid;
	}
	*first = lo;
	hi = s3fs_dyn_nr;
	up = lo;
	while (up < hi) {
		u32 mid = up + (hi - up) / 2;

		if (norm_parent(s3fs_dyn_entries[order[mid]].parent_ino) <= parent)
			up = mid + 1;
		else
			hi = mid;
	}
	*cnt = up - lo;
}

static bool resolve_entry_by_name(unsigned long parent_ino,
				  const char *name, size_t name_len,
				  struct s3fs_entry_snap *out)
{
	struct s3fs_local_entry *le;
	size_t i;
	unsigned long want_parent = norm_parent(parent_ino);

	mutex_lock(&s3fs_entries_mtx);

	/* Local additions win: freshly created files that haven't been
	 * promoted into the dyn table yet must still be found here. */
	list_for_each_entry(le, &s3fs_local_entries, link) {
		if (norm_parent(le->parent_ino) != want_parent ||
		    le->name_len != name_len ||
		    memcmp(le->name, name, name_len) != 0)
			continue;
		out->ino = le->ino;
		out->parent_ino = want_parent;
		/* .create starts empty and fd->size covers writes while the
		 * inode lives; W2: after eviction le->size carries it. */
		out->size = le->size;
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
		const struct s3fs_entry_dyn *e =
			dyn_find_name(want_parent, name, name_len);

		if (e) {
			out->ino = e->ino;
			out->parent_ino = norm_parent(e->parent_ino);
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
	/* Bootstrap entries all live at root — only match when the
	 * caller is looking up under root. */
	if (want_parent != (unsigned long)S3FS_ROOT_INO) {
		mutex_unlock(&s3fs_entries_mtx);
		return false;
	}
	for (i = 0; i < S3FS_NR_BOOTSTRAP; i++) {
		const struct s3fs_entry *e = &s3fs_bootstrap_entries[i];
		size_t elen = strlen(e->name);

		if (elen != name_len || memcmp(e->name, name, name_len) != 0)
			continue;
		out->ino = e->ino;
		out->parent_ino = (unsigned long)S3FS_ROOT_INO;
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

/* W2: persist an evicted inode's size into its entry so the next lookup
 * re-creates the inode with the right i_size.  Without this a .create'd
 * file (local entry, size 0) read back empty once its inode was evicted
 * after flush, and an overwritten S3 file kept its stale dyn size until
 * the daemon's next entries_replace. */
static void s3fs_entry_record_size(unsigned long ino, u64 size)
{
	struct s3fs_local_entry *le;

	mutex_lock(&s3fs_entries_mtx);
	list_for_each_entry(le, &s3fs_local_entries, link) {
		if (le->ino == ino)
			le->size = size;
	}
	if (s3fs_dyn_entries) {
		u32 h = dyn_ino_hash(ino) & s3fs_dyn_idx->mask, v;

		while ((v = s3fs_dyn_idx->by_ino[h])) {
			if (s3fs_dyn_entries[v - 1].ino == ino)
				s3fs_dyn_entries[v - 1].size = size;
			h = (h + 1) & s3fs_dyn_idx->mask;
		}
	}
	mutex_unlock(&s3fs_entries_mtx);
}

