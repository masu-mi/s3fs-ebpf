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
#include <linux/hash.h>
#include <linux/hashtable.h>
#include <linux/highmem.h>
#include <linux/jhash.h>
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
#include <linux/sort.h>
#include <linux/spinlock.h>
#include <linux/statfs.h>
#include <linux/string.h>
#include <linux/uaccess.h>
#include <linux/version.h>
#include <linux/wait.h>

#include "vfs_ops.h"
#include "policy_struct_ops.h"

/* S3FS_EBPF_MAGIC lives in the shared header so the BPF fentry hook can
 * filter by super_block::s_magic without a second source of truth. */
#include "s3fs_shared_types.h"


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
	u64 uaddr;   /* daemon user_vm address of the range's bytes (0 in PCACHE mode) */
	u64 size;    /* range length */
	u64 offset;  /* file offset where the range starts (D-01) */
	/* AB-04: S3FS_ADMIT_FLAG_PCACHE set means data is in Linux page cache,
	 * not in the BPF arena. GUP path is skipped; generic_file_read_iter
	 * serves reads from page cache directly. */
	u32 flags;
};

/*
 * R1 invariant: if a PCACHE entry exists for ino in the hash, then fd->buf
 * for that ino is non-NULL.  Maintained by two rules:
 *   - s3fs_ebpf_populate_pages calls admit_insert_pcache while holding
 *     fd->lock, so no eviction can free fd->buf between the copy and insert.
 *   - s3fs_file_data_reset re-evicts all remaining PCACHE entries for ino
 *     inside fd->lock before freeing fd->buf.
 * Lock order: fd->lock (mutex, outer) → s3fs_admits_lock (rwlock, inner).
 * No path takes s3fs_admits_lock before fd->lock, so no deadlock is possible.
 *
 * S1 corollary: the R1 invariant holds only under fd->lock.  Between
 * fc_pcache_check (lockless fast path) and the moment fill_from_kmalloc
 * takes fd->lock, an eviction + re-populate can run, leaving a stale
 * fd->buf that now covers a different range.  fill_from_kmalloc therefore
 * re-verifies the PCACHE entry's existence inside fd->lock whenever the
 * global admit generation changed since the check.
 */

#define S3FS_ADMIT_HASH_BITS 6

static DEFINE_HASHTABLE(s3fs_admits, S3FS_ADMIT_HASH_BITS);
/* AA-01: rwlock instead of spinlock so concurrent .read_folio calls on
 * distinct inos can take the read side in parallel. Write side is held
 * only by admit_replace / admit_shutdown (infrequent daemon ioctls). */
static DEFINE_RWLOCK(s3fs_admits_lock);

/*
 * AB-10: admit generation counter. Incremented (inside write_lock) by every
 * admit table mutation: admit_replace, admit_drop_all_ranges,
 * admit_insert_pcache, admit_shutdown. Per-file caches keyed on the current
 * generation; a stale gen forces one rwlock re-check, after which the cache
 * is refreshed and subsequent reads are lock-free.
 */
static atomic_t s3fs_admit_gen = ATOMIC_INIT(1);

/*
 * AB-10: per-file admit snapshot cached in file->private_data.
 * Allocated in .open, freed in .release.
 *
 * P1 fix: cache the last confirmed pcache range [off, end) so that
 * the fast path is only taken when pos is within that range.  A
 * prior implementation cached only the bool, causing the fast path
 * to fire for any pos once a pcache entry was found for a different
 * offset on the same fd — leading to zero-reads from the unfilled
 * portion of fd->buf on split (multi-chunk) admits.
 */
struct s3fs_file_admit_cache {
	u32  gen;    /* s3fs_admit_gen value when range was last confirmed */
	bool pcache; /* true = PCACHE admit exists for this ino at [off,end) */
	u64  off;    /* start of the confirmed pcache range */
	u64  end;    /* off+size of the confirmed pcache range (exclusive) */
};

/* Running size of the admit hash. Updated in lock-step with hash_add
 * / hash_del calls under s3fs_admits_lock. Exposed to BPF programs
 * via the s3fs_kfunc_admit_count() KFunc so policies can make
 * pressure-aware decisions (see bpf/policy_pressure_ops.bpf.c). */
atomic_t s3fs_admits_count = ATOMIC_INIT(0);

/*
 * Eviction limits (S-02). Both are module parameters, writable at
 * runtime under /sys/module/s3fs_kfunc/parameters/ (the daemon sets
 * them from --softcap / --cache-capacity). Exported to BPF via
 * s3fs_kfunc_softcap() / s3fs_kfunc_capacity_bytes().
 *
 * capacity_bytes == 0 (default) — legacy vote mode: once the pushed
 *   snapshot holds more than `softcap` entries, every entry the policy
 *   votes for is evicted. softcap is a trigger, not a capacity: the
 *   result can end anywhere between 0 and all entries.
 *
 * capacity_bytes > 0 — capacity mode: after the votes, while the
 *   admitted bytes exceed capacity_bytes, evict in order of policy
 *   score (on_evict return value, higher = evict first; existing
 *   yes/no policies return 1), then oldest-first among entries the
 *   policy wanted to keep. Stops as soon as the set fits, so the bound
 *   holds for every policy and nothing is evicted below it. softcap is
 *   ignored in this mode.
 *
 * Either way an ino with a reader blocked on its miss upcall is never
 * evicted: that reader retries the admit lookup right after
 * SIGNAL_ADMIT and would otherwise fall back to the placeholder buffer.
 */
unsigned int s3fs_softcap = S3FS_EVICT_SOFTCAP;
module_param_named(softcap, s3fs_softcap, uint, 0644);
MODULE_PARM_DESC(softcap,
	"Legacy mode: apply eviction votes when the admit snapshot exceeds this many entries (default 8)");

unsigned long long s3fs_capacity_bytes;
module_param_named(capacity_bytes, s3fs_capacity_bytes, ullong, 0644);
MODULE_PARM_DESC(capacity_bytes,
	"Admit-set byte capacity; >0 enables capacity mode (evict by policy score until it fits). 0 = legacy vote mode");

/* AC-51: leave the chunks a first write / truncate does not touch in S3
 * (struct s3fs_chunk.remote) instead of loading them; the daemon flushes
 * them with a server-side copy.  Off by default: it needs the daemon's
 * multipart flush (docs/design/ac51-partial-modify.md). */
bool s3fs_partial_modify __read_mostly;
module_param_named(partial_modify, s3fs_partial_modify, bool, 0644);
MODULE_PARM_DESC(partial_modify,
	"AC-51: keep untouched chunks of a modified existing object remote (default off)");

unsigned long long s3fs_partial_modify_min __read_mostly = 16ULL << 20;
module_param_named(partial_modify_min, s3fs_partial_modify_min, ullong, 0644);
MODULE_PARM_DESC(partial_modify_min,
	"AC-51: smallest object size (bytes) that uses partial_modify (default 16 MiB)");

/* AB-02: bench-only bypass — skip arena fast path, always use page cache. */
bool s3fs_arena_bypass __read_mostly;
module_param_named(arena_bypass, s3fs_arena_bypass, bool, 0644);
MODULE_PARM_DESC(arena_bypass,
	"AB-02: skip arena fast path in .read_iter; use generic_file_read_iter for all reads (bench only, default off)");

/* Cumulative count of admit entries removed by policy_ops eviction
 * votes. Exposed via S3FS_IOC_STATS so the daemon can mirror it into
 * the Prometheus endpoint. Monotonically increasing counter. */
atomic_long_t s3fs_evictions_total = ATOMIC_LONG_INIT(0);

/*
 * Cumulative count of .read_folio calls that served from the arena
 * (admit_snapshot() hit → s3fs_folio_fill_from_arena). Different
 * from the fentry-side fast_path_hits counter — this measures the
 * kmod's read cache directly, whereas fentry counts getattr hits on
 * the BPF-side inode_index. Divide by (folio_hits + read_miss_upcalls)
 * for the true cache hit ratio.
 */
atomic_long_t s3fs_folio_hits = ATOMIC_LONG_INIT(0);

/*
 * Cumulative page-cache invalidations done by admit_replace for new or
 * moved ranges (S-01). With diff invalidation this grows only with real
 * content changes, not with every full-list re-send.
 */
atomic_long_t s3fs_admit_invalidations = ATOMIC_LONG_INIT(0);

/* AB-01: GUP call count and accumulated latency (ns) in s3fs_arena_copy_to_iter.
 * Exported via IOC_STATS slots 3/4. bench-ab-arena-vs-pagecache.sh reads
 * delta values to compute mean ns/GUP call without needing bpftrace. */
atomic_long_t s3fs_gup_calls_total = ATOMIC_LONG_INIT(0);
atomic_long_t s3fs_gup_ns_total    = ATOMIC_LONG_INIT(0);

/* T4: .read_folio cases where S3FS_UPCALL_MAX_RETRIES were exhausted with the
 * daemon alive — eviction fired in the retry window every time.  Reached when
 * capacity=1 chunk and many concurrent readers compete for the same ino.
 * Exported via IOC_STATS slot 5.  Non-zero distinguishes "capacity exhaustion"
 * -EIO from "data corruption" -EIO in the evict-race report. */
atomic_long_t s3fs_upcall_retry_exhausted = ATOMIC_LONG_INIT(0);

/* U11: .read_folio upcall waits that timed out (remaining==0) before the
 * daemon signalled.  Caused by goroutine-pool back-pressure (readMissWorkers=16
 * can queue 16+ goroutines; the blocking sem send in runReadMissLoop stalls the
 * main read loop so kernel miss events sit in the list > 2 s).  U11 treats
 * timeout as a retry (counts against the budget) instead of falling to gen=0
 * with potentially NULL fd->buf, eliminating -EIO under load.
 * Exported via IOC_STATS slot 6. */
atomic_long_t s3fs_upcall_timeout = ATOMIC_LONG_INIT(0);

/* D-diag: gen=0 fallback failure breakdown — which sub-condition failed?
 * These are incremented inside s3fs_folio_fill_from_kmalloc when gen==0
 * and buf_valid would be false.  Used to identify the root cause of
 * -EIO in the evict-race test.  Exported via IOC_STATS slots 7/8/9. */
atomic_long_t s3fs_gen0_fail_buf_null = ATOMIC_LONG_INIT(0); /* buf==NULL    */
atomic_long_t s3fs_gen0_fail_bitmap   = ATOMIC_LONG_INIT(0); /* bitmap 0     */
atomic_long_t s3fs_gen0_fail_other    = ATOMIC_LONG_INIT(0); /* pos/ci range */

/* M1: bytes currently allocated for per-inode fd->buf (sum of fd->cap),
 * maintained at every kvmalloc/kvfree of fd->buf.  capacity_bytes only
 * bounds admit entries; this shows what fd->buf actually holds, e.g. a
 * buffer kept alive by skip_reset after its admit entry was dropped.
 * Exported via IOC_STATS slot 10 (gauge). */
atomic_long_t s3fs_fd_buf_bytes = ATOMIC_LONG_INIT(0);

/* M1: s3fs_file_data_reset calls that deferred the fd->buf free because
 * buf_readers > 0 (U8).  Exported via IOC_STATS slot 11. */
atomic_long_t s3fs_reset_skipped = ATOMIC_LONG_INIT(0);

/* Ring buffer of ino values evicted by policy vote since the last
 * daemon drain. S3FS_IOC_DRAIN_EVICTIONS copies them out and resets
 * head/tail. Overflow (queue full) is counted separately so the
 * daemon can degrade to a full snapshot reconcile. */
#define S3FS_EVICT_LOG_CAP 512
static DEFINE_SPINLOCK(s3fs_evict_log_lock);
static u64 s3fs_evict_log[S3FS_EVICT_LOG_CAP];
static u32 s3fs_evict_log_len; /* items in the buffer, [0, S3FS_EVICT_LOG_CAP] */
static u32 s3fs_evict_log_overflow; /* dropped since last drain */

static void evict_log_push(u64 ino)
{
	spin_lock(&s3fs_evict_log_lock);
	if (s3fs_evict_log_len < S3FS_EVICT_LOG_CAP)
		s3fs_evict_log[s3fs_evict_log_len++] = ino;
	else
		s3fs_evict_log_overflow++;
	spin_unlock(&s3fs_evict_log_lock);
}

/* Copy up to `cap` entries into `out` and reset the buffer. Returns
 * (filled, overflow) via pointers. */
static void evict_log_drain(u64 *out, u32 cap, u32 *filled, u32 *overflow)
{
	u32 n;

	spin_lock(&s3fs_evict_log_lock);
	n = min(cap, s3fs_evict_log_len);
	memcpy(out, s3fs_evict_log, n * sizeof(u64));
	*filled = n;
	*overflow = s3fs_evict_log_overflow;
	/* Shift any leftover down (rare — daemon should keep up). */
	if (n < s3fs_evict_log_len)
		memmove(s3fs_evict_log, s3fs_evict_log + n,
			(s3fs_evict_log_len - n) * sizeof(u64));
	s3fs_evict_log_len -= n;
	s3fs_evict_log_overflow = 0;
	spin_unlock(&s3fs_evict_log_lock);
}

void s3fs_ebpf_drain_evictions(u64 *out, u32 cap, u32 *filled, u32 *overflow)
{
	evict_log_drain(out, cap, filled, overflow);
}

/* Reference on the daemon's mm, set by every admit_replace that passes
 * an mm (count=0 included, AC-43) and dropped only by admit_shutdown.
 * Also gates the read-miss upcall. All entries share this single mm. */
static struct mm_struct *s3fs_admitted_mm;

/* Set in fill_super so admit_replace can ilookup() cached inodes, grow
 * their i_size to newly admitted range ends (AC-48) and drop stale page
 * cache. */
static struct super_block *s3fs_active_sb;

/*
 * "Any range" lookup — returns the first admit entry for `ino`.
 * Used by eviction / debugging where the caller doesn't care which
 * specific range covered a particular offset.
 */
static struct s3fs_admit *admit_lookup_locked(u64 ino)
{
	struct s3fs_admit *a;

	hash_for_each_possible(s3fs_admits, a, node, ino)
		if (a->ino == ino)
			return a;
	return NULL;
}

/*
 * Range-aware lookup (D-01): returns the entry whose [offset,
 * offset+size) covers `pos`. When the daemon admits a whole file as
 * one range (offset=0, size=full_size), this collapses to the
 * legacy behavior. Multiple entries per ino live in the same hash
 * bucket (hash_add uses ino as key); the walk picks the range that
 * matches the read position.
 */
static struct s3fs_admit *admit_lookup_at_locked(u64 ino, u64 pos)
{
	struct s3fs_admit *a;

	hash_for_each_possible(s3fs_admits, a, node, ino) {
		if (a->ino != ino)
			continue;
		if (pos >= a->offset && pos < a->offset + a->size)
			return a;
	}
	return NULL;
}

/*
 * Drop every admitted range of `ino` (partial admits can register
 * several). Takes the admit lock. Returns how many were dropped.
 */
/* S4: pcache_out is set to true if at least one PCACHE entry was removed.
 * Callers that don't need the flag pass NULL.
 * AC-41: *bytes sums arena entries only — a pcache ino's memory is its
 * fd->buf (fd->cap), which the capacity loop measures directly. */
static unsigned int admit_drop_all_ranges(u64 ino, u64 *bytes, bool *pcache_out)
{
	struct s3fs_admit *a;
	struct hlist_node *tmp;
	unsigned int n = 0;
	u64 b = 0;
	bool saw_pcache = false;

	write_lock(&s3fs_admits_lock);
	hash_for_each_possible_safe(s3fs_admits, a, tmp, node, ino) {
		if (a->ino != ino)
			continue;
		if (a->flags & S3FS_ADMIT_FLAG_PCACHE)
			saw_pcache = true;
		hash_del(&a->node);
		atomic_dec(&s3fs_admits_count);
		if (!(a->flags & S3FS_ADMIT_FLAG_PCACHE))
			b += a->size;
		kfree(a);
		n++;
	}
	if (n)
		atomic_inc(&s3fs_admit_gen); /* AB-10: invalidate per-file caches */
	write_unlock(&s3fs_admits_lock);
	if (bytes)
		*bytes = b;
	if (pcache_out)
		*pcache_out = saw_pcache;
	return n;
}

/*
 * AB-10 / P1: per-file admit cache check.
 *
 * Fast path: if fc->gen matches the current global gen AND
 *   - pcache is false  → no pcache entry for any pos in this ino, OR
 *   - pcache is true   → pos is within the confirmed range [off, end)
 * return the cached result without touching the rwlock.
 *
 * Slow path (stale gen or pos outside cached range): take read_lock,
 * probe the hash for the entry that covers pos, save the range while
 * still under the lock, update the cache, unlock.
 *
 * P1 fix: the prior implementation returned the cached bool for *any*
 * pos once the gen matched, ignoring whether pos was within the
 * confirmed range.  On split admits (one fd, two chunks), a pos in the
 * second chunk would hit the fast path from the first chunk's check,
 * calling s3fs_folio_fill_from_kmalloc on unfilled fd->buf → zeros.
 *
 * fc may be NULL (file->private_data not allocated yet, or readahead
 * path where file == NULL).  In that case we always take the rwlock.
 */
/* S1: gen_out receives the s3fs_admit_gen value used for the admit-table
 * probe so that fill_from_kmalloc can detect table mutations that occurred
 * in the TOCTOU window between this check and taking fd->lock.  NULL is
 * accepted (read_iter fast path, which goes directly to generic_file_read_iter
 * without calling fill_from_kmalloc). */
static bool fc_pcache_check(struct s3fs_file_admit_cache *fc, u64 ino, u64 pos,
			     u32 *gen_out)
{
	struct s3fs_admit *a;
	bool found;
	u32 cur_gen;
	u64 a_off = 0, a_end = 0;

	if (fc) {
		cur_gen = atomic_read(&s3fs_admit_gen);
		/*
		 * P4: smp_load_acquire pairs with the smp_store_release on the
		 * write path below.  The acquire barrier guarantees that, once
		 * we observe the matching gen, all preceding stores (pcache,
		 * off, end) are also visible — even on weakly-ordered CPUs
		 * (ARM, POWER) where plain READ_ONCE would allow the new gen
		 * to be seen with stale pcache/off/end values.
		 */
		if (smp_load_acquire(&fc->gen) == cur_gen) {
			if (!fc->pcache) {
				if (gen_out)
					*gen_out = cur_gen;
				return false; /* no pcache entry for any pos */
			}
			/* pcache=true: fast-path only within the confirmed range */
			if (pos >= fc->off && pos < fc->end) {
				if (gen_out)
					*gen_out = cur_gen;
				return true;
			}
			/* pos outside cached range: fall through to slow path */
		}
	}

	/* Slow path: rwlock + read gen atomically with the table. */
	read_lock(&s3fs_admits_lock);
	a = admit_lookup_at_locked(ino, pos);
	found = a && (a->flags & S3FS_ADMIT_FLAG_PCACHE);
	/* Save range before unlock — a is only valid under the lock. */
	if (found) {
		a_off = a->offset;
		a_end = a->offset + a->size;
	}
	cur_gen = atomic_read(&s3fs_admit_gen);
	read_unlock(&s3fs_admits_lock);

	if (fc) {
		/* Write off/end/pcache before releasing gen (P4).  Any reader
		 * that observes the new gen via smp_load_acquire is guaranteed
		 * to see the updated off/end/pcache. */
		WRITE_ONCE(fc->off,    a_off);
		WRITE_ONCE(fc->end,    a_end);
		WRITE_ONCE(fc->pcache, found);
		smp_store_release(&fc->gen, cur_gen);
	}
	if (gen_out)
		*gen_out = cur_gen;
	return found;
}

/*
 * AB-04 / P3: insert or update a page-cache-mode admit entry (uaddr=0).
 *
 * P3 fix: prior code always hash_add'd a new entry, so repeated
 * POPULATE_PAGES calls for the same (ino, offset) — e.g. on a re-read
 * after admit_replace cleared the entry — accumulated duplicates and
 * inflated s3fs_admits_count without bound.  Now update in-place when
 * the same (ino, offset) already exists.
 */
static int admit_insert_pcache(u64 ino, u64 offset, u64 size)
{
	struct s3fs_admit *a, *existing;
	bool updated = false;

	a = kzalloc(sizeof(*a), GFP_KERNEL);
	if (!a)
		return -ENOMEM;
	a->ino    = ino;
	a->uaddr  = 0;
	a->size   = size;
	a->offset = offset;
	a->flags  = S3FS_ADMIT_FLAG_PCACHE;

	write_lock(&s3fs_admits_lock);
	hash_for_each_possible(s3fs_admits, existing, node, ino) {
		if (existing->ino == ino &&
		    existing->offset == offset &&
		    (existing->flags & S3FS_ADMIT_FLAG_PCACHE)) {
			if (existing->size != size) {
				existing->size = size;
				/* Range changed: stale per-file caches must re-check */
				atomic_inc(&s3fs_admit_gen);
			}
			updated = true;
			break;
		}
	}
	if (!updated) {
		hash_add(s3fs_admits, &a->node, ino);
		atomic_inc(&s3fs_admits_count);
		atomic_inc(&s3fs_admit_gen);
	}
	write_unlock(&s3fs_admits_lock);

	if (updated)
		kfree(a);
	return 0;
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
	atomic_set(&s3fs_admits_count, 0);
}

/*
 * P3: admit_replace-specific clear — removes only non-pcache (arena)
 * entries.  Pcache entries survive the snapshot replacement so they
 * are counted in capacity enforcement and remain eviction candidates.
 * Returns the number of pcache entries still in the table.
 * Caller must hold write_lock.
 */
static int admit_clear_arena_locked(void)
{
	struct s3fs_admit *a;
	struct hlist_node *tmp;
	unsigned int i;
	int pcache_count = 0;

	hash_for_each_safe(s3fs_admits, i, tmp, a, node) {
		if (a->flags & S3FS_ADMIT_FLAG_PCACHE) {
			pcache_count++;
			continue;
		}
		hash_del(&a->node);
		kfree(a);
	}
	return pcache_count;
}

/*
 * Return the admitted size for `ino`, or 0 if the ino is not in the
 * admit hash. Exposed to BPF policy programs via the
 * s3fs_kfunc_ino_size() KFunc (see kmod/tree_traversal.c). Acquires
 * the admit spinlock; safe from any non-IRQ context (BPF struct_ops
 * and tracing programs both qualify — the critical section is a
 * single bucket walk).
 *
 * D-01 note: with range-list admits a single entry no longer
 * necessarily equals full file size. This helper returns the size
 * of _some_ range for the ino; policies that want the full file
 * size should sum ranges (future kfunc) or read sync entries.
 */
u64 s3fs_admit_size(u64 ino)
{
	struct s3fs_admit *a;
	u64 size = 0;

	read_lock(&s3fs_admits_lock);
	a = admit_lookup_locked(ino);
	if (a)
		size = a->size;
	read_unlock(&s3fs_admits_lock);
	return size;
}

/*
 * Policy hooks — fentry attach anchors for Layer 2/3 BPF programs.
 *
 * Three hook points that BPF programs can attach to:
 *   - on_admit(count, hint): after each admit_replace, observation
 *     point for building access-freq sketches, ghost lists, etc.
 *   - on_evict(ino): asks the policy whether `ino` should be evicted
 *     from the admit table on capacity pressure. Return 1 = evict,
 *     0 = keep. BPF programs use their own state maps to make the
 *     decision (LRU tail check, freq threshold, etc.).
 *   - on_pressure(count, threshold): fires when admit count crosses
 *     the configured cap. Advisory only (no return value consumed);
 *     BPF can log, emit metrics, or trigger ghost-list refresh.
 *
 * noinline + non-static so kallsyms / BTF picks up the symbols for
 * fentry attachment. Bodies wrap args in asm volatile so -O2 doesn't
 * drop them. See docs/design/policy-engine-design.md §4.5.
 */
/* fmod_ret trampolines override our return value at runtime, but the
 * compiler doesn't know that — a plain `return 0;` gets constant-
 * propagated into the caller and the whole check becomes dead code.
 * `asm volatile("" : "+r"(ret) :: "memory")` marks the value opaque,
 * so GCC keeps the load and honors whatever the trampoline stores. */
/* Default returns are held in module-scope volatiles so GCC can't
 * prove them constant across the call. Without this the caller's
 * `if (s3fs_policy_on_evict(ino) != 0)` gets constant-folded to
 * `if (false)` and the entire evict-count loop is removed — fmod_ret
 * overrides the runtime return, but the compiler still sees zero. */
static volatile int s3fs_policy_default_ret;

noinline int s3fs_policy_on_admit(u64 count, u64 arena_used_hint)
{
	(void)count;
	(void)arena_used_hint;
	return s3fs_policy_default_ret;
}

noinline int s3fs_policy_on_evict(u64 ino)
{
	(void)ino;
	return s3fs_policy_default_ret;
}

noinline int s3fs_policy_on_pressure(u64 count, u64 threshold)
{
	(void)count;
	(void)threshold;
	return s3fs_policy_default_ret;
}

/*
 * on_hit: fires on every admit-hit in the read path. Policies use
 * this signal to refresh recency (LRU seed) / bump frequency
 * (freq2bit / real S3-FIFO 3-tier) / set visited bit (SIEVE clock). Return
 * value is currently ignored — hit signals are advisory.
 */
noinline int s3fs_policy_on_hit(u64 ino)
{
	(void)ino;
	return s3fs_policy_default_ret;
}

/*
 * Drop the page cache of a policy-evicted ino. Without this an evicted
 * file keeps being served from its page cache (.read_iter =
 * generic_file_read_iter never reaches .read_folio), so eviction would
 * neither cause a miss nor be visible to on_hit/hit-rate accounting —
 * the admit table would stop bounding what is actually cached.
 * T3: use non-blocking invalidate_mapping_pages (folio_trylock) instead of
 * invalidate_inode_pages2 (folio_lock) to avoid deadlock when a reader holds
 * a folio lock waiting for a daemon upcall while the daemon's ioctl eviction
 * path calls this function.  Folios skipped by trylock remain in page cache
 * until the next eviction; correctness is unaffected (they hold current S3
 * data).  miss-handling is serial in the daemon, so the window where a reader
 * is blocked with a folio lock is bounded by S3FS_UPCALL_WAIT_JIFFIES.
 */
static void s3fs_evict_page_cache(u64 ino)
{
	struct inode *inode;

	if (!s3fs_active_sb)
		return;
	inode = ilookup(s3fs_active_sb, ino);
	if (!inode)
		return;
	invalidate_mapping_pages(inode->i_mapping, 0, -1);
	iput(inode);
}

static bool pending_exists(u64 ino); /* defined with the upcall machinery */

/* P3 / Q1: defined after struct s3fs_file_data; frees fd->buf for non-dirty
 * inodes so capacity_bytes bounds actual allocations. */
static void s3fs_file_data_reset(u64 ino);

/* Evict every admitted range of `ino`: drop it from the table, report
 * it to the daemon (evict log), drop its page cache, and free its
 * fd->buf (P3/Q1: only for pcache inos; arena inos have no fd->buf).
 *
 * S4: admit_drop_all_ranges reports whether a PCACHE entry was dropped
 * atomically inside its write_lock, eliminating the prior TOCTOU window
 * where populate could insert a PCACHE entry after the old admit_has_pcache
 * check but before admit_drop_all_ranges, causing drop to remove the entry
 * while reset was never called — leaving fd->buf allocated outside the
 * capacity limit. */
static bool admit_evict_ino(u64 ino, u64 *freed)
{
	bool had_pcache = false;

	if (!admit_drop_all_ranges(ino, freed, &had_pcache))
		return false;
	evict_log_push(ino);
	s3fs_evict_page_cache(ino);
	if (had_pcache)
		s3fs_file_data_reset(ino);
	return true;
}

/* AC-41: defined after struct s3fs_file_data. */
static u64 s3fs_ino_buf_bytes(u64 ino);

struct s3fs_victim {
	int score;     /* policy on_evict return value, clamped >= 0 */
	u64 ino;       /* inode number (P3: replaces entries[] idx so pcache can be included) */
	u32 tiebreak;  /* hash_32(ino,32): decorrelates from ino/Zipf distribution (Y-02) */
	bool tried;    /* AC-58: pick_victim already returned this ino */
};

/* Highest score first; ties broken by tiebreak (ino hash) to avoid
 * systematic bias when scores are uniform (e.g. all score-0 on cold start). */
static int s3fs_victim_cmp(const void *a, const void *b)
{
	const struct s3fs_victim *x = a, *y = b;

	if (x->score != y->score)
		return x->score > y->score ? -1 : 1;
	return x->tiebreak < y->tiebreak ? -1 : (x->tiebreak > y->tiebreak);
}

static inline bool s3fs_fd_buf_owned(struct inode *inode); /* below */
static bool s3fs_ino_shed_unpinned(u64 ino); /* below */
static void s3fs_inode_grow_size_unowned(struct inode *inode, u64 end); /* below */

/* Evict one capacity-pass candidate and charge what it released against
 * *live_bytes.  A miss in flight means the ino is shed (idle chunks only)
 * rather than evicted whole (AC-45 C3).  @pv: the policy keeps its own list,
 * so tell it about a whole-ino eviction (AC-58). */
static void s3fs_capacity_evict_one(u64 ino, u64 *live_bytes, u32 *evicted,
				    bool pv)
{
	u64 freed = 0;
	u64 buf_before = s3fs_ino_buf_bytes(ino);

	if (pending_exists(ino)) {
		/* AC-45 C3: a miss is in flight, so the ino is not evicted
		 * whole (the chunk being populated and its entries must reach
		 * the waiting reader), but its idle chunks are shed —
		 * otherwise a file read front to back would keep everything it
		 * has served. */
		if (s3fs_ino_shed_unpinned(ino)) {
			(*evicted)++;
			buf_before -= min(s3fs_ino_buf_bytes(ino), buf_before);
			*live_bytes -= min(buf_before, *live_bytes);
		}
		return;
	}
	if (!admit_evict_ino(ino, &freed))
		return;
	(*evicted)++;
	if (pv)
		s3fs_policy_ops_remove(ino);
	/* AC-41: arena bytes from the dropped entries plus what reset actually
	 * released (0 on skip_reset, so the loop keeps evicting while still
	 * over capacity). */
	buf_before -= min(s3fs_ino_buf_bytes(ino), buf_before);
	freed += buf_before;
	*live_bytes -= min(freed, *live_bytes);
}

int s3fs_ebpf_admit_replace(struct mm_struct *mm,
			    const struct s3fs_admit_entry *entries,
			    u32 count)
{
	struct s3fs_admit **staged = NULL;
	bool *changed = NULL;
	struct mm_struct *prev_mm;
	u32 i, n_changed = 0;
	int err = 0;
	/* P3: pcache entries survive admit_replace; collected for capacity
	 * accounting and eviction alongside the freshly-staged arena entries. */
	u64 *pcache_inos  = NULL;
	u64 *pcache_sizes = NULL;
	int pcache_remaining = 0;

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
		changed = kcalloc(count, sizeof(*changed), GFP_KERNEL);
		if (!changed) {
			kfree(staged);
			return -ENOMEM;
		}
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
			staged[i]->offset = entries[i].offset;
		}
	}

	write_lock(&s3fs_admits_lock);
	/*
	 * S-01: decide which entries actually changed before the old table
	 * is dropped. The daemon re-sends the whole list on every miss /
	 * sync / eval, so most entries are byte-identical to what is
	 * already admitted; invalidating their page cache would make one
	 * file's miss throw away every hot file's cache. A range is
	 * unchanged when the same (ino, offset, uaddr, size) is already
	 * admitted for the same daemon mm — new content always arrives at
	 * a fresh arena chunk (new uaddr), so that is enough to keep the
	 * page cache valid.
	 */
	for (i = 0; i < count; i++) {
		struct s3fs_admit *a;
		bool same = false;

		if (s3fs_admitted_mm == mm) {
			hash_for_each_possible(s3fs_admits, a, node,
					       staged[i]->ino) {
				if (a->ino == staged[i]->ino &&
				    a->offset == staged[i]->offset &&
				    a->uaddr == staged[i]->uaddr &&
				    a->size == staged[i]->size) {
					same = true;
					break;
				}
			}
		}
		changed[i] = !same;
	}
	/* P3: collect pcache entry info before clearing arena entries so we
	 * can include them in capacity enforcement outside the lock.
	 * GFP_ATOMIC is required because write_lock is a spinlock variant. */
	{
		struct s3fs_admit *ea;
		unsigned int bi;

		hash_for_each(s3fs_admits, bi, ea, node) {
			if (ea->flags & S3FS_ADMIT_FLAG_PCACHE)
				pcache_remaining++;
		}
		if (pcache_remaining > 0) {
			pcache_inos  = kcalloc(pcache_remaining, sizeof(u64), GFP_ATOMIC);
			pcache_sizes = kcalloc(pcache_remaining, sizeof(u64), GFP_ATOMIC);
			if (pcache_inos && pcache_sizes) {
				int pi = 0;

				hash_for_each(s3fs_admits, bi, ea, node) {
					if ((ea->flags & S3FS_ADMIT_FLAG_PCACHE) &&
					    pi < pcache_remaining) {
						pcache_inos[pi]  = ea->ino;
						pcache_sizes[pi] = ea->size;
						pi++;
					}
				}
				pcache_remaining = pi;
			} else {
				kfree(pcache_inos);
				kfree(pcache_sizes);
				pcache_inos = pcache_sizes = NULL;
				pcache_remaining = 0;
			}
		}
	}
	/* P3: preserve pcache entries; only remove arena entries */
	admit_clear_arena_locked();
	for (i = 0; i < count; i++)
		hash_add(s3fs_admits, &staged[i]->node, staged[i]->ino);
	atomic_set(&s3fs_admits_count, (int)count + pcache_remaining);
	prev_mm = s3fs_admitted_mm;
	/*
	 * Keep the daemon mm reference live when the admit set transiently
	 * empties (full policy eviction). The daemon is still alive and
	 * next .read_folio miss will upcall it to re-admit; nulling mm
	 * here would send that miss straight to the zero-page kmalloc
	 * fallback and break subsequent reads (P-07 hit-rate bench). The
	 * mm pointer only gets cleared through s3fs_ebpf_admit_shutdown()
	 * on daemon exit.
	 *
	 * AC-43: a count=0 call from the daemon also carries its mm, so an
	 * empty prewarm (e.g. prewarm-meta-only.cel in pcache mode) still
	 * arms the upcall path. Only a NULL mm keeps the previous pointer.
	 */
	if (mm)
		s3fs_admitted_mm = mm;
	atomic_inc(&s3fs_admit_gen); /* AB-10: table replaced → all per-file caches stale */
	write_unlock(&s3fs_admits_lock);

	kfree(staged); /* array itself; nodes now live in the hash */
	staged = NULL;

	/* Drop the previous mm ref only when we replaced it (mm != NULL).
	 * For mm=NULL we kept prev_mm as the active pointer — no put. */
	if (mm && prev_mm)
		mmput(prev_mm);

	if (s3fs_active_sb) {
		for (i = 0; i < count; i++) {
			struct inode *inode = ilookup(s3fs_active_sb,
						      entries[i].ino);
			if (!inode)
				continue;
			/* AC-48: an entry is a range (D-01), not the whole
			 * object — chunk 0 of a 10 MiB file has size 4 MiB.
			 * Writing entries[i].size shrank i_size to the chunk,
			 * so reads past it hit EOF (and W2 persisted the
			 * wrong size on evict).  Only grow i_size to the
			 * range end, only for new content, and never while
			 * write/flush owns the size.  Remote shrinks arrive
			 * via entries_replace (dcache shrink → re-lookup).
			 * Overflow of offset + size was rejected above. */
			if (changed[i])
				s3fs_inode_grow_size_unowned(inode,
					entries[i].offset + entries[i].size);
			/* Drop any stale page cache filled from the kmalloc
			 * fallback path (or from a prior admit with a
			 * different size) — only for ranges that are new or
			 * moved (S-01, see above). Uses folio_trylock, so
			 * folios held by a concurrent .read_folio waiting on
			 * the upcall are skipped rather than deadlocked —
			 * those are pre-uptodate anyway and will be filled
			 * from the arena on the retry after signal_admit
			 * fires. */
			if (changed[i]) {
				invalidate_mapping_pages(inode->i_mapping, 0, -1);
				n_changed++;
			}
			iput(inode);
		}
	}

	kfree(changed);
	changed = NULL;
	atomic_long_add(n_changed, &s3fs_admit_invalidations);

	/* Policy hook fires after the admit snapshot lands. Both attach
	 * paths (fmod_ret + struct_ops) are consulted so either can be
	 * active. See docs/design/policy-engine-design.md §4.5 Layer 2 and
	 * docs/design/policy-attach-comparison.md for the trade-off. */
	s3fs_policy_on_admit((u64)count, (u64)0);
	(void)s3fs_policy_ops_eval_admit((u64)count, (u64)0);

	/* Capacity pressure hook: fires when the new snapshot exceeds a
	 * soft threshold (half the hash's 64-bucket capacity at
	 * S3FS_ADMIT_HASH_BITS=6). BPF programs use this as an advisory
	 * signal to update ghost lists / freq sketches ahead of the
	 * eviction round. Threshold is soft on purpose — daemon-side
	 * pruneAdmitsTo is the actual enforcement path. */
	if (count > (1u << S3FS_ADMIT_HASH_BITS) / 2) {
		s3fs_policy_on_pressure((u64)count,
					(u64)((1u << S3FS_ADMIT_HASH_BITS) / 2));
		(void)s3fs_policy_ops_eval_pressure((u64)count,
			(u64)((1u << S3FS_ADMIT_HASH_BITS) / 2));
	}

	/* Consult the policy on each entry in the new snapshot: 1 = evict
	 * (skip re-add), 0 = keep. Return value now flows through fmod_ret
	 * (kmod registered these hooks via register_btf_fmodret_id_set;
	 * see tree_traversal.c). Enforcement itself is still an advisory
	 * count logged here — the daemon's prune loop consumes it via a
	 * followup ioctl. */
	/*
	 * Policy vote consumption. For every ino in the freshly-admitted
	 * snapshot (arena entries + surviving pcache entries), ask the loaded
	 * policy programs whether the entry should be evicted.  struct_ops
	 * wins when both are attached (primary path).
	 *
	 * P3: pcache entries are now included so capacity_bytes bounds the
	 * total admitted set, not just the arena subset.  eval_inos/cand are
	 * sized for count + pcache_remaining unique inos.
	 *
	 * Enforcement threshold: only apply eviction once the snapshot
	 * exceeds a soft cap (S3FS_EVICT_SOFTCAP).
	 */
	{
		u32 fmodret_votes = 0, structops_votes = 0, evicted = 0;
		const u32 softcap = READ_ONCE(s3fs_softcap);
		const u64 cap_bytes = READ_ONCE(s3fs_capacity_bytes);
		/* P3: total candidate entries = arena + pcache */
		u32 total_count = count + (u32)pcache_remaining;
		struct s3fs_victim *cand = NULL;
		u64 live_bytes = 0;
		u32 ncand = 0;
		/* X-01: call on_evict exactly once per unique ino. */
		u64 *eval_inos = NULL;
		int *eval_scores = NULL;
		u32 eval_count = 0;
		bool pv = s3fs_policy_has_pick_victim(); /* AC-58 */
		int pi;

		if (cap_bytes) {
			for (i = 0; i < count; i++)
				live_bytes += entries[i].size;
			/* P3/AC-41: charge each pcache ino once, by its fd->buf
			 * allocation rather than the admitted chunk sizes. */
			for (pi = 0; pi < pcache_remaining; pi++) {
				int pj;

				for (pj = 0; pj < pi; pj++) {
					if (pcache_inos[pj] == pcache_inos[pi])
						break;
				}
				if (pj == pi)
					live_bytes += s3fs_ino_buf_bytes(pcache_inos[pi]);
			}
			if (live_bytes > cap_bytes) {
				cand = kcalloc(total_count, sizeof(*cand), GFP_KERNEL);
				if (!cand)
					pr_warn_ratelimited("s3fs_ebpf: capacity eviction skipped (-ENOMEM)\n");
			}
		}

		eval_inos   = kcalloc(total_count, sizeof(*eval_inos),   GFP_KERNEL);
		eval_scores = kcalloc(total_count, sizeof(*eval_scores), GFP_KERNEL);
		if (!eval_inos || !eval_scores) {
			pr_warn_ratelimited("s3fs_ebpf: on_evict dedup alloc failed, skipping eviction\n");
			kfree(eval_inos);
			kfree(eval_scores);
			kfree(cand);
			goto out_free_pcache;
		}

		/* AC-58: on_insert / pick_victim / on_remove mutate policy state
		 * without a BPF-side lock, so one round holds the evict mutex. */
		if (pv)
			s3fs_policy_evict_lock();

		/* Phase 1: collect unique inos and call on_evict once each.
		 * First arena entries, then pcache entries (P3). */
		for (i = 0; i < count; i++) {
			u64 ino = entries[i].ino;
			u32 j;

			for (j = 0; j < eval_count; j++) {
				if (eval_inos[j] == ino)
					break;
			}
			if (j < eval_count)
				continue;

			{
				int fr_vote = s3fs_policy_on_evict(ino);
				int so_vote = s3fs_policy_ops_eval_evict(ino);
				int score   = max3(fr_vote, so_vote, 0);

				eval_inos[eval_count]   = ino;
				eval_scores[eval_count] = score;
				eval_count++;
				if (pv)
					s3fs_policy_ops_insert(ino);

				if (fr_vote != 0)
					fmodret_votes++;
				if (so_vote != 0)
					structops_votes++;
			}
		}
		/* P3: Phase 1 continued — pcache inos */
		for (pi = 0; pi < pcache_remaining; pi++) {
			u64 ino = pcache_inos[pi];
			u32 j;

			for (j = 0; j < eval_count; j++) {
				if (eval_inos[j] == ino)
					break;
			}
			if (j < eval_count)
				continue;

			{
				int fr_vote = s3fs_policy_on_evict(ino);
				int so_vote = s3fs_policy_ops_eval_evict(ino);
				int score   = max3(fr_vote, so_vote, 0);

				eval_inos[eval_count]   = ino;
				eval_scores[eval_count] = score;
				eval_count++;
				if (pv)
					s3fs_policy_ops_insert(ino);

				if (fr_vote != 0)
					fmodret_votes++;
				if (so_vote != 0)
					structops_votes++;
			}
		}

		/* Phase 2: build cand[] / drive legacy eviction.  In capacity
		 * mode each ino appears at most once (cand[].ino stores the ino
		 * directly — P3 replaced the entries[]-index approach so pcache
		 * entries can be represented without a parallel entries[] slot). */
		for (i = 0; i < count; i++) {
			u64 ino = entries[i].ino;
			int score = 0;
			u32 j;

			for (j = 0; j < eval_count; j++) {
				if (eval_inos[j] == ino) {
					score = eval_scores[j];
					break;
				}
			}

			if (cap_bytes) {
				if (cand) {
					u32 k;
					bool already = false;

					for (k = 0; k < ncand; k++) {
						if (cand[k].ino == ino) {
							already = true;
							break;
						}
					}
					if (!already) {
						cand[ncand].score    = score;
						cand[ncand].ino      = ino;
						cand[ncand].tiebreak = hash_32((u32)ino ^ (u32)(ino >> 32), 32);
						ncand++;
					}
				}
				continue;
			}

			/* Legacy mode */
			if (count <= softcap || !score)
				continue;
			if (pending_exists(ino))
				continue;
			if (admit_evict_ino(ino, NULL))
				evicted++;
		}
		/* P3: Phase 2 continued — pcache inos */
		for (pi = 0; pi < pcache_remaining; pi++) {
			u64 ino = pcache_inos[pi];
			int score = 0;
			u32 j;

			for (j = 0; j < eval_count; j++) {
				if (eval_inos[j] == ino) {
					score = eval_scores[j];
					break;
				}
			}

			if (cap_bytes) {
				if (cand) {
					u32 k;
					bool already = false;

					for (k = 0; k < ncand; k++) {
						if (cand[k].ino == ino) {
							already = true;
							break;
						}
					}
					if (!already) {
						cand[ncand].score    = score;
						cand[ncand].ino      = ino;
						cand[ncand].tiebreak = hash_32((u32)ino ^ (u32)(ino >> 32), 32);
						ncand++;
					}
				}
				continue;
			}

			/* Legacy mode: pcache entries evictable once over softcap */
			if (total_count <= softcap || !score)
				continue;
			if (pending_exists(ino))
				continue;
			if (admit_evict_ino(ino, NULL))
				evicted++;
		}

		kfree(eval_inos);
		kfree(eval_scores);

		if (cand) {
			u32 k;

			sort(cand, ncand, sizeof(*cand), s3fs_victim_cmp, NULL);
			/* AC-58: the policy's own order first.  Anything it
			 * cannot name (0, unknown ino, one already tried) hands
			 * the rest to the score order below. */
			for (k = 0; pv && k < ncand && live_bytes > cap_bytes; k++) {
				u64 v = s3fs_policy_ops_pick_victim();
				u32 j;

				if (!v)
					break;
				for (j = 0; j < ncand; j++)
					if (cand[j].ino == v)
						break;
				if (j == ncand) {
					/* Not a candidate any more (evicted by
					 * another route): let the policy forget
					 * it and ask again. */
					s3fs_policy_ops_remove(v);
					continue;
				}
				if (cand[j].tried)
					break;
				cand[j].tried = true;
				s3fs_capacity_evict_one(v, &live_bytes, &evicted, pv);
			}
			for (k = 0; k < ncand && live_bytes > cap_bytes; k++) {
				if (cand[k].tried)
					continue;
				s3fs_capacity_evict_one(cand[k].ino, &live_bytes,
							&evicted, pv);
			}
			kfree(cand);
			if (live_bytes > cap_bytes)
				pr_info_ratelimited("s3fs_ebpf: admit set %llu B still over capacity %llu B (in-flight misses)\n",
						    live_bytes, cap_bytes);
		}
		if (pv)
			s3fs_policy_evict_unlock();
		if (fmodret_votes)
			pr_info("s3fs_ebpf: fmod_ret voted evict on %u/%u entries\n",
				fmodret_votes, total_count);
		if (structops_votes)
			pr_info("s3fs_ebpf: struct_ops voted evict on %u/%u entries\n",
				structops_votes, total_count);
		if (evicted) {
			atomic_long_add(evicted, &s3fs_evictions_total);
			pr_info("s3fs_ebpf: policy evicted %u entries (softcap=%u capacity_bytes=%llu)\n",
				evicted, softcap, cap_bytes);
		}
	}

out_free_pcache:
	kfree(pcache_inos);
	kfree(pcache_sizes);
	return 0;

out_free_staged:
	while (i--)
		kfree(staged[i]);
	kfree(staged);
	kfree(changed);
	return err;
}

static void s3fs_ebpf_upcall_shutdown(void); /* forward */

void s3fs_ebpf_admit_shutdown(void)
{
	struct mm_struct *prev;

	write_lock(&s3fs_admits_lock);
	admit_clear_locked();
	prev = s3fs_admitted_mm;
	s3fs_admitted_mm = NULL;
	atomic_inc(&s3fs_admit_gen); /* AB-10: shutdown clears all entries */
	write_unlock(&s3fs_admits_lock);

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

int s3fs_ebpf_entries_replace(struct s3fs_entry_dyn *entries, u32 count)
{
	struct s3fs_entry_dyn *prev;
	struct s3fs_dyn_index *ix = NULL, *prev_ix;

	/* Built before the mutex: it is O(count log count). */
	if (count && entries) {
		ix = dyn_index_build(entries, count);
		if (!ix)
			return -ENOMEM;
	}
	mutex_lock(&s3fs_entries_mtx);
	prev = s3fs_dyn_entries;
	prev_ix = s3fs_dyn_idx;
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

/* -- Per-inode content buffer -------------------------------------------- */

/* S11: daemon always fetches in 4 MiB chunks (partialAdmitChunk=4MiB=2^22). */
#define S3FS_PCACHE_CHUNK_SHIFT	22u
#define S3FS_CHUNK_SIZE		(1UL << S3FS_PCACHE_CHUNK_SHIFT)
/* Upper bound on the chunk table (1 TiB of file offset): keeps a bogus
 * populate/write offset from asking for a huge table. */
#define S3FS_MAX_CHUNKS		(1u << 18)

/* AC-45: fd content is a table of 4 MiB chunks instead of one contiguous
 * buffer, so only the chunks a reader touched are allocated (a tail read
 * of a 9 MiB file costs its chunk, not 9 MiB) and growth never copies
 * more than one chunk.
 *
 * Invariants (all under fd->lock):
 *  - chunk ci covers file bytes [ci << SHIFT, (ci + 1) << SHIFT).
 *  - p == NULL means "not allocated": a hole that reads as zeros while the
 *    file is dirty/flushing, and "not cached" otherwise.
 *  - bytes of an allocated chunk at or beyond fd->size are zero.
 *  - fd->cap == sum of chunks[].cap, and s3fs_fd_buf_bytes moves with it
 *    (M1): only s3fs_fd_chunk_ensure / s3fs_fd_chunk_free change either. */
struct s3fs_chunk {
	u8   *p;
	u32   cap;       /* allocated bytes, <= S3FS_CHUNK_SIZE */
	bool  populated; /* S11: holds valid S3 data (T2 uses it for every ci) */
	/* AC-51: only in a dirty fd.  The chunk is unchanged from the object
	 * in S3 and not held here (p may be stale or NULL); it reads through a
	 * load upcall and flushes as a server-side copy.  Counted in
	 * fd->nr_remote; cleared by a store, a free and a successful flush. */
	bool  remote;
	/* U8 (per chunk): pcache readers in the upcall retry loop waiting on
	 * this chunk.  While > 0 s3fs_file_data_reset keeps the chunk, so the
	 * data-based fill still finds what the daemon populated for them even
	 * after the PCACHE entry was evicted.  Survives s3fs_fd_chunk_free so
	 * the pin/unpin counts stay balanced. */
	u32   readers;
};

struct s3fs_file_data {
	struct mutex lock;
	struct s3fs_chunk *chunks;
	u32   nr_chunks;  /* length of the chunks table (entries may be empty) */
	size_t size;
	size_t cap;
	bool  dirty;    /* set by .write, cleared after successful daemon flush */
	bool  flushing; /* flush enqueued; cleared by signal_flush on PUT ACK */
	u32   nr_remote; /* AC-51: chunks with .remote set */
	/* U8 is per chunk now (struct s3fs_chunk.readers). */
	/* W1: unflushed-data pin.  maybe_enqueue_flush takes one inode ref
	 * (ihold) per fd while a flush is outstanding so evict_inode cannot
	 * free fd->buf before the daemon has read and PUT it (drop_caches or
	 * memory pressure after close would otherwise lose the write).
	 * flush_seq counts enqueues; read_dirty snapshots it into
	 * flush_read_seq.  signal_flush releases the pin only when the PUT
	 * succeeded and covered the latest enqueue (seqs equal, !dirty).
	 * Pinned iff pin_link is on s3fs_flush_pins (s3fs_flush_pins_lock);
	 * the seqs are protected by fd->lock. */
	u64   flush_seq;
	u64   flush_read_seq;
	struct list_head pin_link;
	struct inode *pin_inode;
};

/* Grow the chunk table so index ci is valid.  Caller holds fd->lock. */
static int s3fs_fd_chunks_grow(struct s3fs_file_data *fd, u32 ci)
{
	struct s3fs_chunk *nt;
	u32 n;

	if (ci < fd->nr_chunks)
		return 0;
	if (ci >= S3FS_MAX_CHUNKS)
		return -EFBIG;
	n = clamp_t(u32, max(ci + 1, fd->nr_chunks * 2), 8, S3FS_MAX_CHUNKS);
	nt = kvcalloc(n, sizeof(*nt), GFP_KERNEL);
	if (!nt)
		return -ENOMEM;
	if (fd->chunks)
		memcpy(nt, fd->chunks, fd->nr_chunks * sizeof(*nt));
	kvfree(fd->chunks);
	fd->chunks = nt;
	fd->nr_chunks = n;
	return 0;
}

/* Make chunk ci at least `want` bytes (<= S3FS_CHUNK_SIZE).  The caller is
 * about to overwrite [lo, hi) of the chunk, so only the rest of the new
 * space is zeroed (a freshly populated chunk is not memset and then copied
 * over).  *old_cap is the previous allocation (0 if none).  This and
 * s3fs_fd_chunk_free are the only places that change fd->cap and
 * s3fs_fd_buf_bytes (M1).  Caller holds fd->lock. */
static int s3fs_fd_chunk_ensure(struct s3fs_file_data *fd, u32 ci, u32 want,
				u32 lo, u32 hi, u32 *old_cap)
{
	struct s3fs_chunk *c;
	u32 oc;
	u8 *np;
	int err;

	err = s3fs_fd_chunks_grow(fd, ci);
	if (err)
		return err;
	c = &fd->chunks[ci];
	oc = c->p ? c->cap : 0;
	*old_cap = oc;
	if (oc >= want)
		return 0;
	np = kvmalloc(want, GFP_KERNEL);
	if (!np)
		return -ENOMEM;
	if (oc)
		memcpy(np, c->p, oc);
	if (lo > oc)
		memset(np + oc, 0, min(lo, want) - oc);
	if (hi < want)
		memset(np + max(hi, oc), 0, want - max(hi, oc));
	kvfree(c->p);
	atomic_long_add(want - oc, &s3fs_fd_buf_bytes);
	fd->cap += want - oc;
	c->p = np;
	c->cap = want;
	return 0;
}

/* Release chunk ci (data, accounting and populated flag).  fd->lock held. */
static void s3fs_fd_chunk_free(struct s3fs_file_data *fd, u32 ci)
{
	struct s3fs_chunk *c;

	if (ci >= fd->nr_chunks)
		return;
	c = &fd->chunks[ci];
	if (c->remote)
		fd->nr_remote--;
	if (c->p) {
		atomic_long_sub(c->cap, &s3fs_fd_buf_bytes);
		fd->cap -= c->cap;
		kvfree(c->p);
	}
	{
		u32 readers = c->readers;

		memset(c, 0, sizeof(*c));
		c->readers = readers; /* U8: pin/unpin stay balanced */
	}
}

/* U8: pin chunk ci for a reader about to wait on the daemon.  Needs a table
 * entry, so it can fail on allocation (the reader then runs unpinned, as
 * readers did before U8).  fd->lock held. */
static bool s3fs_fd_chunk_pin(struct s3fs_file_data *fd, u32 ci)
{
	if (s3fs_fd_chunks_grow(fd, ci))
		return false;
	fd->chunks[ci].readers++;
	return true;
}

static void s3fs_fd_chunk_unpin(struct s3fs_file_data *fd, u32 ci)
{
	if (ci < fd->nr_chunks && fd->chunks[ci].readers)
		fd->chunks[ci].readers--;
}

/* Release every chunk and the table.  Leaves fd->size alone.  fd->lock held
 * (or the fd is unreachable). */
static void s3fs_fd_free_all(struct s3fs_file_data *fd)
{
	u32 ci;

	for (ci = 0; ci < fd->nr_chunks; ci++)
		s3fs_fd_chunk_free(fd, ci);
	kvfree(fd->chunks);
	fd->chunks = NULL;
	fd->nr_chunks = 0;
}

/* Copy [off, off + len) into the chunks from `kbuf` (kernel) or `ubuf`
 * (user); exactly one is non-NULL.  The caller checked off + len for
 * overflow.  Chunks are allocated as needed:
 *  - alloc_limit != 0 (populate, initial content): a missing chunk gets
 *    exactly min(S3FS_CHUNK_SIZE, alloc_limit - chunk start) bytes, so a
 *    46 B object costs 46 B of capacity (AC-41) and a tail read costs only
 *    its chunk (AC-45).  alloc_limit is max(populate end, i_size).
 *  - alloc_limit == 0 (write): grow by doubling within the chunk, so
 *    sequential writes copy at most one chunk per growth (cf. AC-46).
 * On -EFAULT the part of the new memory that was not copied is zeroed, so
 * the "zero at or beyond fd->size" invariant survives a failed copy.
 * Caller holds fd->lock. */
static int s3fs_fd_store(struct s3fs_file_data *fd, u64 off, const void *kbuf,
			 const char __user *ubuf, u64 len, u64 alloc_limit)
{
	u64 pos = off, end = off + len;

	while (pos < end) {
		u32 seg_off, seg_len, want, cur, old_cap;
		struct s3fs_chunk *c;
		u64 cs;
		u32 ci;
		int err;

		if ((pos >> S3FS_PCACHE_CHUNK_SHIFT) >= S3FS_MAX_CHUNKS)
			return -EFBIG;
		ci = (u32)(pos >> S3FS_PCACHE_CHUNK_SHIFT);
		cs = (u64)ci << S3FS_PCACHE_CHUNK_SHIFT;
		seg_off = (u32)(pos - cs);
		seg_len = (u32)min_t(u64, S3FS_CHUNK_SIZE - seg_off, end - pos);

		if (alloc_limit) {
			want = (u32)min_t(u64, S3FS_CHUNK_SIZE, alloc_limit - cs);
		} else {
			cur = ci < fd->nr_chunks && fd->chunks[ci].p ?
			      fd->chunks[ci].cap : 0;
			want = max_t(u32, round_up(seg_off + seg_len, 4096),
				     min_t(u32, S3FS_CHUNK_SIZE, cur * 2));
		}
		want = max_t(u32, want, seg_off + seg_len);

		err = s3fs_fd_chunk_ensure(fd, ci, want, seg_off,
					   seg_off + seg_len, &old_cap);
		if (err)
			return err;
		c = &fd->chunks[ci];
		if (c->remote) {
			c->remote = false;
			fd->nr_remote--;
		}
		if (kbuf) {
			memcpy(c->p + seg_off, (const u8 *)kbuf + (pos - off),
			       seg_len);
		} else {
			unsigned long rem = copy_from_user(c->p + seg_off,
							   ubuf + (pos - off),
							   seg_len);

			if (rem) {
				u32 zfrom = max(seg_off + seg_len - (u32)rem,
						old_cap);

				if (zfrom < seg_off + seg_len)
					memset(c->p + zfrom, 0,
					       seg_off + seg_len - zfrom);
				return -EFAULT;
			}
		}
		pos += seg_len;
	}
	return 0;
}

/* Copy the first n bytes of the file to user space; missing chunks and
 * the unallocated tail of a short chunk read as zeros.  fd->lock held. */
static long s3fs_fd_copy_out_at(struct s3fs_file_data *fd, char __user *uaddr,
				size_t base, size_t n)
{
	size_t pos = 0;

	while (pos < n) {
		size_t fpos = base + pos;
		u32 ci = (u32)(fpos >> S3FS_PCACHE_CHUNK_SHIFT);
		u32 off = (u32)(fpos & (S3FS_CHUNK_SIZE - 1));
		size_t seg = min_t(size_t, S3FS_CHUNK_SIZE - off, n - pos);
		size_t have = 0;

		if (ci < fd->nr_chunks && fd->chunks[ci].p &&
		    fd->chunks[ci].cap > off)
			have = min_t(size_t, seg, fd->chunks[ci].cap - off);
		if (have && copy_to_user(uaddr + pos, fd->chunks[ci].p + off,
					 have))
			return -EFAULT;
		if (seg > have && clear_user(uaddr + pos + have, seg - have))
			return -EFAULT;
		pos += seg;
	}
	return (long)n;
}

static long s3fs_fd_copy_out(struct s3fs_file_data *fd, char __user *uaddr,
			     size_t n)
{
	return s3fs_fd_copy_out_at(fd, uaddr, 0, n);
}

/* True if the 4 MiB chunk at chunk_offset of ino is allocated and populated
 * in its fd.  Takes fd->lock; call with no other lock held. */
static bool s3fs_ino_chunk_populated(u64 ino, u64 chunk_offset)
{
	struct inode *inode;
	struct s3fs_file_data *fd;
	bool ok = false;

	if (!s3fs_active_sb ||
	    (chunk_offset >> S3FS_PCACHE_CHUNK_SHIFT) >= S3FS_MAX_CHUNKS)
		return false;
	inode = ilookup(s3fs_active_sb, (unsigned long)ino);
	if (!inode)
		return false;
	fd = inode->i_private;
	if (fd) {
		u32 ci = (u32)(chunk_offset >> S3FS_PCACHE_CHUNK_SHIFT);

		mutex_lock(&fd->lock);
		ok = ci < fd->nr_chunks && fd->chunks[ci].p &&
		     fd->chunks[ci].populated;
		mutex_unlock(&fd->lock);
	}
	iput(inode);
	return ok;
}

/* AC-45 C3: free the idle chunks of an ino that has a miss in flight.
 *
 * The capacity pass cannot evict such an ino whole (its PCACHE entries and
 * the chunk being populated must survive until the waiting reader is
 * served), but it must not let a file read front to back keep every chunk
 * it has already served.  This frees every chunk no reader waits on
 * (readers == 0; a reader pins before it creates its pending) and drops the
 * PCACHE entries overlapping them first, so R1 holds.  Returns true if
 * anything was freed.  Takes fd->lock; call with no other lock held. */
static bool s3fs_ino_shed_unpinned(u64 ino)
{
	struct inode *inode;
	struct s3fs_file_data *fd;
	bool freed_any = false;
	u32 ci;

	if (!s3fs_active_sb)
		return false;
	inode = ilookup(s3fs_active_sb, (unsigned long)ino);
	if (!inode)
		return false;
	fd = inode->i_private;
	if (!fd) {
		iput(inode);
		return false;
	}

	mutex_lock(&fd->lock);
	if (fd->dirty || fd->flushing)
		goto out;
	for (ci = 0; ci < fd->nr_chunks; ci++) {
		struct s3fs_chunk *c = &fd->chunks[ci];
		u64 cs = (u64)ci << S3FS_PCACHE_CHUNK_SHIFT;
		u64 ce = cs + S3FS_CHUNK_SIZE;
		struct s3fs_admit *a;
		struct hlist_node *tmp;
		bool removed = false;

		if (!c->p || c->readers)
			continue;
		write_lock(&s3fs_admits_lock);
		hash_for_each_possible_safe(s3fs_admits, a, tmp, node, ino) {
			if (a->ino != ino || !(a->flags & S3FS_ADMIT_FLAG_PCACHE))
				continue;
			if (a->offset >= ce || a->offset + a->size <= cs)
				continue;
			hash_del(&a->node);
			kfree(a);
			atomic_dec(&s3fs_admits_count);
			removed = true;
		}
		if (removed)
			atomic_inc(&s3fs_admit_gen);
		write_unlock(&s3fs_admits_lock);
		s3fs_fd_chunk_free(fd, ci);
		/* The page cache must not keep serving what capacity dropped
		 * (cf. s3fs_evict_page_cache; trylock-based, so no deadlock with
		 * a reader that holds a folio lock and waits for fd->lock). */
		invalidate_mapping_pages(inode->i_mapping, cs >> PAGE_SHIFT,
					 (ce - 1) >> PAGE_SHIFT);
		freed_any = true;
	}
out:
	mutex_unlock(&fd->lock);
	iput(inode);
	return freed_any;
}

/* Cut the file down to new_size (< fd->size): free the chunks wholly past
 * it and zero the tail of the boundary chunk, so the "zero beyond size"
 * invariant holds when the file is extended again.  fd->lock held. */
static void s3fs_fd_truncate(struct s3fs_file_data *fd, size_t new_size)
{
	u32 ci = (u32)(new_size >> S3FS_PCACHE_CHUNK_SHIFT);
	u32 off = (u32)(new_size & (S3FS_CHUNK_SIZE - 1));
	u32 i;

	if (off && ci < fd->nr_chunks && fd->chunks[ci].p &&
	    fd->chunks[ci].cap > off)
		memset(fd->chunks[ci].p + off, 0, fd->chunks[ci].cap - off);
	for (i = off ? ci + 1 : ci; i < fd->nr_chunks; i++)
		s3fs_fd_chunk_free(fd, i);
}

/* True when fd->buf is owned by the write/flush path and the arena may
 * hold stale prewarm content.  Lockless READ_ONCE is intentional: a
 * transient false negative just causes an unnecessary arena skip (miss),
 * never a stale-data read. */
static inline bool s3fs_fd_buf_owned(struct inode *inode)
{
	struct s3fs_file_data *fd = inode->i_private;

	return fd && (READ_ONCE(fd->dirty) || READ_ONCE(fd->flushing));
}

/* AC-56: grow i_size to @end unless write/flush owns the size.  write and
 * truncate change dirty and i_size under fd->lock, so the ownership check and
 * the i_size_write happen under it too — otherwise a truncate between the two
 * is undone.  fd->lock is the outer lock (the caller holds no admits_lock). */
static void s3fs_inode_grow_size_unowned(struct inode *inode, u64 end)
{
	struct s3fs_file_data *fd = inode->i_private;

	if (fd)
		mutex_lock(&fd->lock);
	if (!s3fs_fd_buf_owned(inode) && end > (u64)i_size_read(inode))
		i_size_write(inode, end);
	if (fd)
		mutex_unlock(&fd->lock);
}

/* AC-41: bytes actually allocated for ino's fd->buf.  The capacity loop
 * charges pcache inos by this instead of their admitted chunk sizes:
 * s3fs_file_data_reserve sizes fd->buf to at least the populate end (and,
 * AC-46, up to 2x cap bounded by the file size), so one tail chunk admits
 * 4 MiB but allocates the whole file.  Lockless read —
 * accounting only.  Must be called without s3fs_admits_lock held (iput). */
static u64 s3fs_ino_buf_bytes(u64 ino)
{
	struct inode *inode;
	struct s3fs_file_data *fd;
	u64 cap = 0;

	if (!s3fs_active_sb)
		return 0;
	inode = ilookup(s3fs_active_sb, (unsigned long)ino);
	if (!inode)
		return 0;
	fd = inode->i_private;
	if (fd)
		cap = READ_ONCE(fd->cap);
	iput(inode);
	return cap;
}

static int s3fs_u64_cmp(const void *a, const void *b)
{
	u64 x = *(const u64 *)a, y = *(const u64 *)b;

	return x < y ? -1 : x > y;
}

/* M2: cross-check for the M1 gauge, exported via IOC_STATS slots 12/13.
 *   *walk   = sum of fd->cap over every live regular inode on the sb.  The
 *             gauge is maintained incrementally, so gauge != walk at rest
 *             means a cap change that skipped the gauge.
 *   *pcache = sum of fd->cap over distinct pcache inos — exactly what the
 *             AC-41 capacity loop charges.  walk - pcache is fd->buf held
 *             by non-pcache inos (written files, skip_reset leftovers).
 * Lockless cap reads, so only meaningful when I/O is quiescent.  The walk
 * holds i_lock while reading i_private: evict frees fd only after
 * I_FREEING is set, which we check under the same lock. */
void s3fs_ebpf_fd_buf_audit(u64 *walk, u64 *pcache)
{
	struct super_block *sb = s3fs_active_sb;
	struct inode *inode;
	struct s3fs_admit *a;
	unsigned int bkt, n = 0, i;
	u64 *inos;

	*walk = 0;
	*pcache = 0;
	if (!sb)
		return;

	spin_lock(&sb->s_inode_list_lock);
	list_for_each_entry(inode, &sb->s_inodes, i_sb_list) {
		struct s3fs_file_data *fd;

		spin_lock(&inode->i_lock);
		if (!(inode->i_state & (I_FREEING | I_WILL_FREE | I_NEW)) &&
		    S_ISREG(inode->i_mode)) {
			fd = inode->i_private;
			if (fd)
				*walk += READ_ONCE(fd->cap);
		}
		spin_unlock(&inode->i_lock);
	}
	spin_unlock(&sb->s_inode_list_lock);

	read_lock(&s3fs_admits_lock);
	hash_for_each(s3fs_admits, bkt, a, node)
		if (a->flags & S3FS_ADMIT_FLAG_PCACHE)
			n++;
	inos = n ? kmalloc_array(n, sizeof(*inos), GFP_ATOMIC) : NULL;
	if (inos) {
		i = 0;
		hash_for_each(s3fs_admits, bkt, a, node)
			if ((a->flags & S3FS_ADMIT_FLAG_PCACHE) && i < n)
				inos[i++] = a->ino;
		n = i;
	}
	read_unlock(&s3fs_admits_lock);
	if (!inos)
		return;

	sort(inos, n, sizeof(*inos), s3fs_u64_cmp, NULL);
	for (i = 0; i < n; i++)
		if (i == 0 || inos[i] != inos[i - 1])
			*pcache += s3fs_ino_buf_bytes(inos[i]);
	kfree(inos);
}

/* W1: inodes pinned by an outstanding flush.  put_super releases any
 * that remain (daemon gone, PUT failed) so umount does not leak them. */
static LIST_HEAD(s3fs_flush_pins);
static DEFINE_SPINLOCK(s3fs_flush_pins_lock);

/* Caller holds fd->lock and a reference on inode.  No-op if pinned. */
static void s3fs_flush_pin(struct inode *inode, struct s3fs_file_data *fd)
{
	spin_lock(&s3fs_flush_pins_lock);
	if (list_empty(&fd->pin_link)) {
		ihold(inode);
		fd->pin_inode = inode;
		list_add_tail(&fd->pin_link, &s3fs_flush_pins);
	}
	spin_unlock(&s3fs_flush_pins_lock);
}

/* Returns true if the caller now owns the pin reference and must iput
 * the inode (after dropping fd->lock — the iput may evict it).  List
 * membership decides ownership so this cannot race release_all into a
 * double iput. */
static bool s3fs_flush_unpin(struct s3fs_file_data *fd)
{
	bool owned = false;

	spin_lock(&s3fs_flush_pins_lock);
	if (!list_empty(&fd->pin_link)) {
		list_del_init(&fd->pin_link);
		owned = true;
	}
	spin_unlock(&s3fs_flush_pins_lock);
	return owned;
}

static void s3fs_flush_pins_release_all(void)
{
	struct s3fs_file_data *fd;
	struct inode *inode;

	for (;;) {
		spin_lock(&s3fs_flush_pins_lock);
		fd = list_first_entry_or_null(&s3fs_flush_pins,
					      struct s3fs_file_data, pin_link);
		if (!fd) {
			spin_unlock(&s3fs_flush_pins_lock);
			return;
		}
		list_del_init(&fd->pin_link);
		inode = fd->pin_inode;
		spin_unlock(&s3fs_flush_pins_lock);
		pr_warn_ratelimited("s3fs_ebpf: ino=%lu released with unacknowledged flush\n",
				    inode->i_ino);
		iput(inode);
	}
}

/*
 * P3: free the per-inode kmalloc buffer when a pcache entry is evicted.
 * After eviction the page cache is also dropped, so subsequent reads
 * reach .read_folio which re-fetches from S3 and re-populates fd->buf.
 * Dirty inodes are skipped — their buffer holds user-written content
 * pending flush to S3.
 */
static void s3fs_file_data_reset(u64 ino)
{
	struct inode *inode;
	struct s3fs_file_data *fd;
	struct s3fs_admit *a;
	struct hlist_node *tmp;
	bool removed = false;

	if (!s3fs_active_sb)
		return;
	inode = ilookup(s3fs_active_sb, (unsigned long)ino);
	if (!inode)
		return;
	fd = inode->i_private;
	if (fd) {
		mutex_lock(&fd->lock);
		if (!fd->dirty && !fd->flushing) {
			bool kept = false;
			u32 ci;

			/* R1: drop every PCACHE entry of this ino, including any
			 * inserted between admit_drop_all_ranges and this point,
			 * before freeing chunks: an entry must never outlive the
			 * chunk it points at.
			 * Lock order: fd->lock (outer) → s3fs_admits_lock (inner). */
			write_lock(&s3fs_admits_lock);
			hash_for_each_possible_safe(s3fs_admits, a, tmp, node, ino) {
				if (a->ino != ino)
					continue;
				if (!(a->flags & S3FS_ADMIT_FLAG_PCACHE))
					continue;
				hash_del(&a->node);
				kfree(a);
				atomic_dec(&s3fs_admits_count);
				removed = true;
			}
			if (removed)
				atomic_inc(&s3fs_admit_gen);
			write_unlock(&s3fs_admits_lock);
			/* U8 (per chunk): a chunk some reader waits on is kept — it
			 * serves that reader via the data-based fill even though
			 * its PCACHE entry is gone.  Everything else is freed. */
			for (ci = 0; ci < fd->nr_chunks; ci++) {
				if (fd->chunks[ci].readers) {
					kept = true;
					continue;
				}
				s3fs_fd_chunk_free(fd, ci); /* S11: no longer valid */
			}
			if (kept) {
				atomic_long_inc(&s3fs_reset_skipped);
			} else {
				s3fs_fd_free_all(fd);
				fd->size = 0;
			}
			/* Q1: do NOT clear inode i_size — it reflects the S3 object
			 * size and must survive eviction so stat(2) and read_iter
			 * keep returning the correct file size. */
		}
		mutex_unlock(&fd->lock);
	}
	iput(inode);
}

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
	u64 parent_ino; /* G-01: dir the file lives under, for daemon full-path resolution */
	u64 size;
	u32 name_len;
	u32 flags;  /* S3FS_FLUSH_FLAG_* (E-03) */
	char name[S3FS_SYNC_NAME_MAX + 1];
};

static LIST_HEAD(s3fs_flush_queue);
static DEFINE_SPINLOCK(s3fs_flush_lock);

/* Separate queue + wait-queue for FLUSH_FLAG_WAIT events (S-08).
 * These are fsync()-triggered; the daemon goroutine blocks on
 * S3FS_IOC_POP_FLUSH_WAIT so they are picked up with near-zero latency
 * instead of waiting up to 100 ms for the ticker-based flush poll. */
static LIST_HEAD(s3fs_flush_wait_queue);
static DEFINE_SPINLOCK(s3fs_flush_wait_lock);
static DECLARE_WAIT_QUEUE_HEAD(s3fs_flush_wait_wq);
static bool s3fs_flush_wait_shutdown;

/*
 * Flush waiter — one per fsync() blocked on daemon PUT completion.
 * .release path does NOT register one (fire-and-forget for close(2)),
 * so signal_flush with no matching ino is a normal no-op.
 *
 * Shape mirrors s3fs_pending (read-miss upcall waiter). Refcount 2 on
 * create: one for the hash slot, one for the fsync caller. Hash drops
 * its ref on signal_flush; caller drops on wait return.
 */
struct s3fs_flush_pending {
	struct hlist_node node;
	u64 ino;
	struct completion done;
	int err;
	refcount_t refs;
};

#define S3FS_FLUSH_PENDING_HASH_BITS 4 /* 16 buckets — few concurrent fsyncs */
static DEFINE_HASHTABLE(s3fs_flush_pending, S3FS_FLUSH_PENDING_HASH_BITS);
static DEFINE_SPINLOCK(s3fs_flush_pending_lock);

static struct s3fs_flush_pending *flush_pending_get_or_create(u64 ino)
{
	struct s3fs_flush_pending *p, *fresh;

	spin_lock(&s3fs_flush_pending_lock);
	hash_for_each_possible(s3fs_flush_pending, p, node, ino) {
		if (p->ino == ino) {
			refcount_inc(&p->refs);
			spin_unlock(&s3fs_flush_pending_lock);
			return p;
		}
	}
	spin_unlock(&s3fs_flush_pending_lock);

	fresh = kmalloc(sizeof(*fresh), GFP_KERNEL);
	if (!fresh)
		return NULL;
	INIT_HLIST_NODE(&fresh->node);
	fresh->ino = ino;
	fresh->err = 0;
	init_completion(&fresh->done);
	refcount_set(&fresh->refs, 2);

	spin_lock(&s3fs_flush_pending_lock);
	hash_for_each_possible(s3fs_flush_pending, p, node, ino) {
		if (p->ino == ino) {
			refcount_inc(&p->refs);
			spin_unlock(&s3fs_flush_pending_lock);
			kfree(fresh);
			return p;
		}
	}
	hash_add(s3fs_flush_pending, &fresh->node, ino);
	spin_unlock(&s3fs_flush_pending_lock);
	return fresh;
}

static void flush_pending_put(struct s3fs_flush_pending *p)
{
	if (!p || !refcount_dec_and_test(&p->refs))
		return;
	kfree(p);
}

static void flush_pending_unhash(struct s3fs_flush_pending *p)
{
	spin_lock(&s3fs_flush_pending_lock);
	if (!hlist_unhashed(&p->node)) {
		hash_del(&p->node);
		spin_unlock(&s3fs_flush_pending_lock);
		flush_pending_put(p); /* drop hash's ref */
		return;
	}
	spin_unlock(&s3fs_flush_pending_lock);
}

/*
 * Daemon → kmod signal: PUT completed with `err`. Wakes any fsync
 * caller blocked on this ino. -ENOENT (no waiter) is a normal outcome
 * for the .release path — daemon signals unconditionally and doesn't
 * care whether anyone was listening.
 */
int s3fs_ebpf_signal_flush(u64 ino, int err)
{
	struct s3fs_flush_pending *p, *target = NULL;
	struct inode *inode;
	struct s3fs_file_data *fd;

	spin_lock(&s3fs_flush_pending_lock);
	hash_for_each_possible(s3fs_flush_pending, p, node, ino) {
		if (p->ino == ino) {
			target = p;
			refcount_inc(&target->refs);
			break;
		}
	}
	spin_unlock(&s3fs_flush_pending_lock);

	/* U5: clear the in-flight flush guard and mark bitmap bits now that
	 * the daemon has confirmed the PUT.  Done for both fsync-wait and
	 * fire-and-forget paths so reads after flush serve from fd->buf
	 * without a new daemon upcall.  Only set bitmap on success (err==0)
	 * so a failed PUT leaves the state for the retry path. */
	if (s3fs_active_sb) {
		inode = ilookup(s3fs_active_sb, (unsigned long)ino);
		if (inode) {
			fd = inode->i_private;
			if (fd) {
				bool unpin = false;

				mutex_lock(&fd->lock);
				/* W1: a successful PUT whose read_dirty predates
				 * the latest enqueue does not cover it — keep
				 * flushing (and the pin) until that one lands.
				 * On error keep the pin: the daemon has restored
				 * dirty and the data must survive until a later
				 * flush succeeds (or umount releases it). */
				if (err != 0 || fd->flush_read_seq == fd->flush_seq) {
					fd->flushing = false;
					if (err == 0 && !fd->dirty && fd->size > 0) {
						u32 last_ci = (u32)((fd->size - 1) >>
							      S3FS_PCACHE_CHUNK_SHIFT);
						u32 ci;

						/* Only allocated chunks hold the data
						 * that was just PUT; a hole is
						 * re-fetched on its next read.  A remote
						 * chunk may have a buffer (mark_remote
						 * leaves a half-filled one in place) but
						 * the flush copied the object's bytes, not
						 * the buffer's: it stays unpopulated
						 * (AC-51). */
						for (ci = 0; ci <= last_ci &&
						     ci < fd->nr_chunks; ci++)
							if (fd->chunks[ci].p &&
							    !fd->chunks[ci].remote)
								fd->chunks[ci].populated = true;
						/* AC-51: the object now equals
						 * the fd, so a remote chunk is
						 * just a chunk we do not hold. */
						for (ci = 0; fd->nr_remote &&
						     ci < fd->nr_chunks; ci++)
							if (fd->chunks[ci].remote) {
								fd->chunks[ci].remote = false;
								fd->nr_remote--;
							}
					}
					if (err == 0 && !fd->dirty)
						unpin = s3fs_flush_unpin(fd);
				}
				mutex_unlock(&fd->lock);
				if (unpin)
					iput(inode); /* pin ref; ilookup ref below keeps it live */
			}
			iput(inode);
		}
	}

	if (!target)
		return -ENOENT;

	target->err = err;
	complete_all(&target->done);
	flush_pending_unhash(target);
	flush_pending_put(target);
	return 0;
}

static void flush_enqueue(u64 ino, u64 parent_ino, u64 size,
			  const char *name, size_t name_len, u32 flags)
{
	struct s3fs_flush_item *f;

	if (name_len == 0 || name_len > S3FS_SYNC_NAME_MAX)
		return;
	f = kzalloc(sizeof(*f), GFP_KERNEL);
	if (!f)
		return;
	f->ino = ino;
	f->parent_ino = parent_ino;
	f->size = size;
	f->name_len = name_len;
	f->flags = flags;
	memcpy(f->name, name, name_len);
	if (flags & S3FS_FLUSH_FLAG_WAIT) {
		struct s3fs_flush_item *q, *tmp;
		LIST_HEAD(stale);

		/* The wait queue is served at once, the normal queue by the
		 * daemon's ticker, so a normal item for this ino that is still
		 * queued (typically the empty placeholder .create enqueues) would
		 * be handled after this fsync's PUT and overwrite it with older
		 * state.  The PUT reads the buffer when it runs, so this event
		 * covers those items: drop them. */
		spin_lock(&s3fs_flush_lock);
		list_for_each_entry_safe(q, tmp, &s3fs_flush_queue, link)
			if (q->ino == ino)
				list_move(&q->link, &stale);
		spin_unlock(&s3fs_flush_lock);
		list_for_each_entry_safe(q, tmp, &stale, link) {
			list_del(&q->link);
			kfree(q);
		}
		spin_lock(&s3fs_flush_wait_lock);
		list_add_tail(&f->link, &s3fs_flush_wait_queue);
		spin_unlock(&s3fs_flush_wait_lock);
		wake_up_interruptible(&s3fs_flush_wait_wq);
	} else {
		spin_lock(&s3fs_flush_lock);
		list_add_tail(&f->link, &s3fs_flush_queue);
		spin_unlock(&s3fs_flush_lock);
	}
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
 * they don't linger past a daemon restart. Also wakes any goroutine
 * blocked in s3fs_ebpf_flush_pop_wait so it exits cleanly. */
static void flush_queue_drain(void)
{
	struct s3fs_flush_item *f, *tmp;

	spin_lock(&s3fs_flush_lock);
	list_for_each_entry_safe(f, tmp, &s3fs_flush_queue, link) {
		list_del(&f->link);
		kfree(f);
	}
	spin_unlock(&s3fs_flush_lock);

	WRITE_ONCE(s3fs_flush_wait_shutdown, true);
	spin_lock(&s3fs_flush_wait_lock);
	list_for_each_entry_safe(f, tmp, &s3fs_flush_wait_queue, link) {
		list_del(&f->link);
		kfree(f);
	}
	spin_unlock(&s3fs_flush_wait_lock);
	wake_up_all(&s3fs_flush_wait_wq);
}

int s3fs_ebpf_flush_pop(struct s3fs_flush_event *out)
{
	struct s3fs_flush_item item;

	if (!flush_dequeue(&item))
		return -EAGAIN;
	memset(out, 0, sizeof(*out));
	out->ino        = item.ino;
	out->parent_ino = item.parent_ino;
	out->size       = item.size;
	out->name_len   = item.name_len;
	out->flags      = item.flags;
	memcpy(out->name, item.name, item.name_len);
	out->name[item.name_len] = '\0';
	return 0;
}

/*
 * Blocking variant of flush_pop for WAIT-flagged events (S-08).
 * Waits up to 200 ms per call; returns:
 *   0        — item dequeued into *out
 *  -EAGAIN   — timeout (no item arrived); caller should re-check ctx
 *  -EINTR    — interrupted by signal
 *  -ENODEV   — shutdown in progress (flush_queue_drain called)
 */
int s3fs_ebpf_flush_pop_wait(struct s3fs_flush_event *out)
{
	struct s3fs_flush_item *f;
	long ret;

again:
	ret = wait_event_interruptible_timeout(s3fs_flush_wait_wq,
		!list_empty(&s3fs_flush_wait_queue) ||
		READ_ONCE(s3fs_flush_wait_shutdown),
		msecs_to_jiffies(200));
	if (ret < 0)
		return -EINTR;
	if (ret == 0)
		return -EAGAIN;

	spin_lock(&s3fs_flush_wait_lock);
	if (list_empty(&s3fs_flush_wait_queue)) {
		spin_unlock(&s3fs_flush_wait_lock);
		if (READ_ONCE(s3fs_flush_wait_shutdown))
			return -ENODEV;
		goto again;
	}
	f = list_first_entry(&s3fs_flush_wait_queue,
			     struct s3fs_flush_item, link);
	list_del(&f->link);
	spin_unlock(&s3fs_flush_wait_lock);

	memset(out, 0, sizeof(*out));
	out->ino        = f->ino;
	out->parent_ino = f->parent_ino;
	out->size       = f->size;
	out->name_len   = f->name_len;
	out->flags      = f->flags;
	memcpy(out->name, f->name, f->name_len);
	out->name[f->name_len] = '\0';
	kfree(f);
	return 0;
}

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
	/* W1: this read covers every enqueue so far (writes precede enqueue). */
	fd->flush_read_seq = fd->flush_seq;
	n = min_t(size_t, fd->size, cap);
	ret = n ? s3fs_fd_copy_out(fd, uaddr, n) : 0;
	mutex_unlock(&fd->lock);
	iput(inode);
	return ret;
}

/* AC-51: S3FS_IOC_READ_DIRTY_RANGE -- copy [offset, offset + cap) of the fd's
 * content (clamped to fd->size).  Remote chunks read as zeros here; the
 * daemon asks for data chunks only (S3FS_IOC_DIRTY_LAYOUT says which). */
long s3fs_ebpf_read_dirty_range(u64 ino, void __user *uaddr, u64 offset, u64 cap)
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
	n = offset < fd->size ? min_t(u64, cap, fd->size - offset) : 0;
	ret = n ? s3fs_fd_copy_out_at(fd, uaddr, (size_t)offset, n) : 0;
	mutex_unlock(&fd->lock);
	iput(inode);
	return ret;
}

/* AC-51: S3FS_IOC_DIRTY_LAYOUT -- one byte per chunk of the fd's content
 * (S3FS_DIRTY_CHUNK_*), so the daemon can flush remote chunks as server-side
 * copies.  Like READ_DIRTY it claims every enqueue so far: the write that
 * enqueued the flush is in this layout (W1), and a later write enqueues its
 * own flush.  -ENOSPC (with *nr set) when `cap` is too small. */
long s3fs_ebpf_dirty_layout(u64 ino, u8 __user *states, u32 cap, u64 *size,
			    u32 *nr)
{
	struct inode *inode;
	struct s3fs_file_data *fd;
	u32 n, ci;
	long ret = 0;

	BUILD_BUG_ON(S3FS_DIRTY_CHUNK_BYTES != S3FS_CHUNK_SIZE);
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
	*size = fd->size;
	n = (u32)DIV_ROUND_UP(fd->size, S3FS_CHUNK_SIZE);
	*nr = n;
	if (n > cap) {
		ret = -ENOSPC;
		goto out;
	}
	for (ci = 0; ci < n;) {
		u8 tmp[128];
		u32 k, m = min_t(u32, n - ci, sizeof(tmp));

		for (k = 0; k < m; k++) {
			struct s3fs_chunk *c = ci + k < fd->nr_chunks ?
					       &fd->chunks[ci + k] : NULL;

			tmp[k] = c && c->remote ? S3FS_DIRTY_CHUNK_REMOTE :
				 (c && c->p ? S3FS_DIRTY_CHUNK_DATA :
				  S3FS_DIRTY_CHUNK_HOLE);
		}
		if (copy_to_user(states + ci, tmp, m)) {
			ret = -EFAULT;
			goto out;
		}
		ci += m;
	}
	fd->flush_read_seq = fd->flush_seq;
out:
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
	INIT_LIST_HEAD(&fd->pin_link);
	if (initial_size) {
		u32 ci;

		/* Initial data is a single contiguous load, so every chunk it
		 * covers is valid (populated) and sized to the data. */
		if (s3fs_fd_store(fd, 0, initial, NULL, initial_size,
				  initial_size)) {
			s3fs_fd_free_all(fd);
			kfree(fd);
			return NULL;
		}
		fd->size = initial_size;
		for (ci = 0; ci * S3FS_CHUNK_SIZE < initial_size; ci++)
			fd->chunks[ci].populated = true;
	}
	return fd;
}

static void s3fs_file_data_free(struct s3fs_file_data *fd)
{
	if (!fd)
		return;
	s3fs_fd_free_all(fd);
	kfree(fd);
}

/* Q3: limit applies to per-call *size*, not offset+size, so large files can
 * be admitted in chunks at offsets > 4 GiB.  Daemon chunks are ≤ 4 MiB
 * (partialAdmitChunk); 64 MiB is 16× headroom while bounding kvmalloc cost
 * per call.  The per-ino buffer still grows to offset+size; files larger than
 * available kernel memory should use arena mode instead of pcache mode. */
#define S3FS_POPULATE_MAX_SIZE  (64ULL << 20)   /* 64 MiB per call */

/* AB-04: populate fd->buf with S3 data from userspace, then insert a
 * page-cache-mode admit entry. Reads served by .read_folio via
 * s3fs_folio_fill_from_kmalloc (no GUP); page cache accumulates
 * uptodate folios so subsequent reads via generic_file_read_iter
 * are served from page cache without touching .read_folio at all.
 *
 * Refuses to overwrite a dirty inode (user-written content takes
 * precedence over S3 data until the flush completes). */
/* AC-51: [offset, end) is made of whole remote chunks (up to i_size).  fd->lock
 * held.  The only populate a dirty fd accepts. */
static bool s3fs_fd_remote_covered(struct inode *inode, struct s3fs_file_data *fd,
				   u64 offset, u64 end)
{
	u64 eof = max_t(u64, end, (u64)i_size_read(inode));
	u32 ci, first = (u32)(offset >> S3FS_PCACHE_CHUNK_SHIFT);
	u32 last = (u32)((end - 1) >> S3FS_PCACHE_CHUNK_SHIFT);

	if (!fd->nr_remote)
		return false;
	for (ci = first; ci <= last; ci++) {
		u64 cs = (u64)ci << S3FS_PCACHE_CHUNK_SHIFT;
		u64 ce = min_t(u64, cs + S3FS_CHUNK_SIZE, eof);

		if (ci >= fd->nr_chunks || !fd->chunks[ci].remote ||
		    offset > cs || end < ce)
			return false;
	}
	return true;
}

int s3fs_ebpf_populate_pages(u64 ino, u64 offset, u64 size,
			     const void __user *data_ptr)
{
	struct inode *inode;
	struct s3fs_file_data *fd;
	u64 end;
	bool owned;
	int err = 0;

	/* P2/Q3: reject degenerate or oversized requests before any allocation.
	 * size=0 underflows in invalidate_mapping_pages; size > MAX_SIZE would
	 * exhaust memory in a single call; overflow would wrap end past u64. */
	if (size == 0 || size > S3FS_POPULATE_MAX_SIZE)
		return -EINVAL;
	if (check_add_overflow(offset, size, &end))
		return -EINVAL;

	if (!s3fs_active_sb)
		return -ENODEV;
	inode = ilookup(s3fs_active_sb, (unsigned long)ino);
	if (!inode)
		return -ENOENT;
	fd = inode->i_private;
	if (!fd) {
		err = -EIO;
		goto out_iput;
	}

	mutex_lock(&fd->lock);
	owned = fd->dirty || fd->flushing;
	if (owned && !s3fs_fd_remote_covered(inode, fd, offset, end)) {
		/* Never overwrite buf with stale S3 data when user content is
		 * pending (dirty) or a flush PUT is still in-flight (flushing).
		 * AC-51 exception: chunks that are still the S3 object's own
		 * bytes (remote) may be loaded into a dirty fd, whole. */
		err = -EBUSY;
		goto out_unlock;
	}
	/* AC-45: store into 4 MiB chunks.  A missing chunk is allocated to
	 * min(4 MiB, max(end, i_size) - chunk start): exactly what the file
	 * needs (AC-41), and `end` keeps the limit above the range even when
	 * i_size is stale and smaller. */
	err = s3fs_fd_store(fd, offset, NULL, (const char __user *)data_ptr, size,
			    max_t(u64, end, (u64)i_size_read(inode)));
	if (err)
		goto out_unlock;
	/* S11: mark chunks covered by [offset, end) as populated.
	 * T5: only mark a chunk if it is FULLY covered by [offset, end), or if
	 * it is the last chunk and end reaches the file's EOF (so no valid
	 * content exists beyond end — the partial tail is safe to serve from
	 * the chunk because pos < fd->size already guards the read limit).
	 * Marking partially-covered inner chunks would expose stale/zero data in
	 * the fallback (gen==0) path for positions below offset.  T2: the flag
	 * exists for every chunk now, so the pcache path no longer skips the
	 * check for chunks >= 64 (files > 256 MiB). */
	{
		u64 chunk_size = (u64)1 << S3FS_PCACHE_CHUNK_SHIFT;
		u32 ci;
		u32 first_ci = (u32)(offset >> S3FS_PCACHE_CHUNK_SHIFT);
		u32 last_ci  = (u32)((end - 1) >> S3FS_PCACHE_CHUNK_SHIFT);
		/* U6: use i_size_read(inode) as the authoritative file size for
		 * the last-chunk condition.  After eviction fd->size is reset to
		 * 0, so using fd->size would treat any partial populate call as
		 * if it were at EOF and incorrectly mark incompletely-filled
		 * chunks as valid.  i_size_read preserves the S3 object size
		 * across evictions (Q1/S8 invariant). */
		u64 eof = max_t(u64, end, (u64)i_size_read(inode));

		for (ci = first_ci; ci <= last_ci; ci++) {
			u64 chunk_start = (u64)ci * chunk_size;
			u64 chunk_end   = chunk_start + chunk_size;
			if ((offset <= chunk_start && chunk_end <= end) ||
			    (ci == last_ci && end >= eof))
				fd->chunks[ci].populated = true;
		}
	}
	if (!owned && end > (u64)fd->size)
		fd->size = (size_t)end;
	/* S8: only grow i_size, never shrink it.  Eviction zeroes fd->size (Q1)
	 * but preserves i_size at the S3 object size so that a partial
	 * re-populate does not truncate the file visible to user-space. */
	if (!owned && (loff_t)fd->size > i_size_read(inode))
		i_size_write(inode, (loff_t)fd->size);
	/* Invalidate any stale page-cache folios for this range so the
	 * next read triggers .read_folio and picks up the new fd->buf data.
	 * end > 0 is guaranteed by the size==0 check above. */
	invalidate_mapping_pages(inode->i_mapping,
				 offset >> PAGE_SHIFT,
				 (end - 1) >> PAGE_SHIFT);

	/* R1: insert the pcache entry while fd->lock is still held so no
	 * eviction can free fd->buf between the copy above and the insert
	 * below.  kzalloc inside admit_insert_pcache runs before write_lock,
	 * so sleeping with fd->lock (mutex) held is safe. */
	if (!owned)	/* a dirty fd is not capacity-managed */
		err = admit_insert_pcache(ino, offset, size);
out_unlock:
	mutex_unlock(&fd->lock);
out_iput:
	iput(inode);
	return err;
}

/* -- File ops (regular files) --------------------------------------------- */

/* How long .read_folio waits for a daemon upcall to admit the ino
 * before falling back to the compile-time kmalloc buffer. */
#define S3FS_UPCALL_WAIT_JIFFIES  (2 * HZ)

/*
 * Range-aware admit snapshot (D-01). Finds the admit entry that
 * covers file offset `pos` and returns arena bytes relative to it:
 *
 *   *out_uaddr    daemon user_vm address of `pos` within the range
 *   *out_avail    bytes remaining in the range starting at `pos`
 *
 * Callers pass `pos` as the folio's file offset. On hit, they can
 * read up to `*out_avail` bytes from `*out_uaddr` via GUP. When
 * the daemon still uses whole-file admits (offset=0, size=file_size)
 * this collapses to the legacy behavior (`*out_uaddr = base + pos`,
 * `*out_avail = size - pos`).
 */
static bool admit_snapshot(u64 ino, u64 pos,
			   u64 *out_uaddr, u64 *out_avail,
			   struct mm_struct **out_mm)
{
	struct s3fs_admit *a;
	bool hit = false;

	read_lock(&s3fs_admits_lock);
	a = admit_lookup_at_locked(ino, pos);
	/* S10: pcache entries have uaddr=0 — their data lives in fd->buf, not
	 * the daemon's arena.  Returning true here with uaddr≈0 causes
	 * fill_from_arena to call GUP on a near-NULL daemon address, which
	 * silently zero-fills the folio and marks it uptodate, bypassing the
	 * upcall retry path that would correctly re-populate fd->buf. */
	if (a && !(a->flags & S3FS_ADMIT_FLAG_PCACHE)) {
		u64 within = pos - a->offset;
		*out_uaddr = a->uaddr + within;
		*out_avail = a->size - within;
		*out_mm    = s3fs_admitted_mm;
		hit = true;
	}
	read_unlock(&s3fs_admits_lock);
	return hit;
}

/* AC-50: attempts per chunk before giving up with -EIO (each waits up to
 * S3FS_UPCALL_WAIT_JIFFIES for the daemon). */
#define S3FS_LOAD_MAX_TRIES	6

/* AC-51: what a modification of the file does, for the loader.  Callers
 * describe it; s3fs_mop_prepare adapts it to the fd's current state. */
struct s3fs_mop {
	u64  end;	/* [0, end) of the current content must survive */
	u64  ow_lo, ow_hi; /* chunks inside this range are overwritten completely */
	u64  t_lo, t_hi;   /* bytes whose new value the change decides */
	bool partial;	/* chunks outside [t_lo, t_hi) may stay remote */
};

static bool s3fs_partial_wanted(struct inode *inode)
{
	return READ_ONCE(s3fs_partial_modify) &&
	       (u64)i_size_read(inode) >= READ_ONCE(s3fs_partial_modify_min);
}

/* The chunks [first, last] a partial op can need data for; with !partial,
 * every chunk of [0, end).  false: none. */
static bool s3fs_mop_range(const struct s3fs_mop *op, u32 *first, u32 *last)
{
	u64 nci = DIV_ROUND_UP(op->end, S3FS_CHUNK_SIZE);
	u64 lo = 0, hi;

	if (!nci)
		return false;
	hi = nci - 1;
	if (op->partial) {
		if (op->t_hi <= op->t_lo)
			return false;
		lo = max(lo, op->t_lo >> S3FS_PCACHE_CHUNK_SHIFT);
		hi = min(hi, (op->t_hi - 1) >> S3FS_PCACHE_CHUNK_SHIFT);
		if (lo > hi)
			return false;
	}
	*first = (u32)lo;
	*last = (u32)hi;
	return true;
}

/* Does the op need chunk ci to hold the object's bytes?  Not when it
 * overwrites the chunk completely, and in partial mode not when the change
 * does not reach into it. */
static bool s3fs_mop_needs_data(const struct s3fs_mop *op, u32 ci)
{
	u64 cs = (u64)ci << S3FS_PCACHE_CHUNK_SHIFT;
	u64 ce = min_t(u64, cs + S3FS_CHUNK_SIZE, op->end);

	if (cs >= op->end)
		return false;
	if (op->ow_lo <= cs && op->ow_hi >= ce)
		return false;
	if (op->partial &&
	    (cs + S3FS_CHUNK_SIZE <= op->t_lo || cs >= op->t_hi))
		return false;
	return true;
}

/* fd->lock held.  Adapts the caller's op to the fd: end is limited to the
 * object size; a dirty fd only ever needs its remote chunks.  false: there is
 * nothing for the loader to do. */
static bool s3fs_mop_prepare(struct inode *inode, struct s3fs_file_data *fd,
			     const struct s3fs_mop *in, struct s3fs_mop *op)
{
	*op = *in;
	op->end = min_t(u64, in->end, (u64)i_size_read(inode));
	if (fd->dirty || fd->flushing) {
		if (!fd->nr_remote)
			return false;
		op->partial = true;
	}
	return op->end != 0;
}

/* fd->lock held: chunk ci holds what the op needs of it.  A clean fd must
 * have it populated; a dirty fd owns its content, apart from remote chunks. */
static bool s3fs_mop_chunk_ready(const struct s3fs_file_data *fd, u32 ci)
{
	const struct s3fs_chunk *c = ci < fd->nr_chunks ? &fd->chunks[ci] : NULL;

	if (fd->dirty || fd->flushing)
		return !(c && c->remote);
	return c && c->p && c->populated;
}

/* AC-50: pins held on the chunks a modification needs, from the start of
 * the load until the caller has applied the change under fd->lock.  A pinned
 * chunk is not freed by reset / shed (readers != 0), so what the load put in
 * stays until the fd is dirty and owns the content.  Unpinning between the
 * load and the write let a capacity pass free the chunk again, and a file
 * larger than capacity_bytes could never hold all its chunks that way.
 * Pins exactly the chunks s3fs_mop_needs_data() selects. */
struct s3fs_mpin {
	struct s3fs_mop op;
	bool held;
};

/* fd->lock held. */
static void s3fs_mpin_release_locked(struct s3fs_file_data *fd,
				     struct s3fs_mpin *mp)
{
	u32 ci, first, last;

	if (!mp->held)
		return;
	if (s3fs_mop_range(&mp->op, &first, &last))
		for (ci = first; ci <= last; ci++)
			if (s3fs_mop_needs_data(&mp->op, ci))
				s3fs_fd_chunk_unpin(fd, ci);
	mp->held = false;
}

static void s3fs_mpin_release(struct s3fs_file_data *fd, struct s3fs_mpin *mp)
{
	mutex_lock(&fd->lock);
	s3fs_mpin_release_locked(fd, mp);
	mutex_unlock(&fd->lock);
}

/* fd->lock held.  Returns 0 with the pins held, or -errno with none. */
static int s3fs_mpin_acquire_locked(struct s3fs_file_data *fd,
				    struct s3fs_mpin *mp,
				    const struct s3fs_mop *op)
{
	u32 ci, first, last;

	mp->op = *op;
	mp->held = true;
	if (!s3fs_mop_range(op, &first, &last))
		return 0;
	for (ci = first; ci <= last; ci++) {
		if (!s3fs_mop_needs_data(op, ci))
			continue;
		if (!s3fs_fd_chunk_pin(fd, ci)) {
			/* release what was pinned so far: chunks below ci */
			mp->op.end = (u64)ci << S3FS_PCACHE_CHUNK_SHIFT;
			s3fs_mpin_release_locked(fd, mp);
			return -ENOMEM;
		}
	}
	return 0;
}

/* AC-50: true when, with fd->lock held, a modification may be applied: every
 * chunk the op needs holds the object's bytes (clean fd: populated; dirty fd:
 * not remote).  s3fs_fd_load_for_modify returns with those chunks still
 * pinned (struct s3fs_mpin), so a shed / evict -> reset cannot free them
 * between the load and the change.  The pins are not enough on their own,
 * though: a loader can return with a short set (a concurrent reset before it
 * pinned, a partial load).  Callers therefore check here under fd->lock, keep
 * that lock through s3fs_mpin_release_locked, the change and the dirty mark,
 * and go back to loading when the check fails. */
static bool s3fs_fd_modify_ready_locked(struct inode *inode,
					struct s3fs_file_data *fd,
					const struct s3fs_mop *in)
{
	struct s3fs_mop op;
	u32 ci, first, last;

	if (!s3fs_mop_prepare(inode, fd, in, &op) ||
	    !s3fs_mop_range(&op, &first, &last))
		return true;
	for (ci = first; ci <= last; ci++)
		if (s3fs_mop_needs_data(&op, ci) &&
		    !s3fs_mop_chunk_ready(fd, ci))
			return false;
	return true;
}

/* AC-51: fd->lock held, the fd clean and s3fs_fd_modify_ready_locked() true.
 * In partial mode, the chunks below op->end that the change neither needs nor
 * overwrites and that the fd does not hold become remote: the object keeps
 * them and the flush copies them.  Done before the change is applied, so a
 * failure leaves the fd as it was. */
static int s3fs_fd_mark_remote_locked(struct inode *inode,
				      struct s3fs_file_data *fd,
				      const struct s3fs_mop *in)
{
	struct s3fs_mop op;
	u32 nci, ci;
	int err;

	if (fd->dirty || fd->flushing)
		return 0;
	if (!s3fs_mop_prepare(inode, fd, in, &op) || !op.partial)
		return 0;
	nci = (u32)DIV_ROUND_UP(op.end, S3FS_CHUNK_SIZE);
	err = s3fs_fd_chunks_grow(fd, nci - 1);
	if (err)
		return err;
	for (ci = 0; ci < nci; ci++) {
		u64 cs = (u64)ci << S3FS_PCACHE_CHUNK_SHIFT;
		u64 ce = min_t(u64, cs + S3FS_CHUNK_SIZE, op.end);
		struct s3fs_chunk *c = &fd->chunks[ci];

		if (op.ow_lo <= cs && op.ow_hi >= ce)
			continue;	/* the write fills it */
		if (s3fs_mop_needs_data(&op, ci))
			continue;	/* loaded */
		if (c->p && c->populated)
			continue;	/* held: flushed as data */
		if (!c->remote) {
			c->remote = true;
			fd->nr_remote++;
		}
	}
	return 0;
}

/* AC-50: make the fd hold the parts of an existing object that a modification
 * leaves intact, before the modification is applied.
 *
 * open(2) loads nothing and a flush PUTs the whole buffer, so the first
 * write / truncate on a file nobody read used to start from an empty buffer:
 * the object was truncated to what was written, a shrink produced zeros and
 * `: > f` never reached S3.  Only the clean -> dirty transition needs this
 * (afterwards the fd owns the content) -- except, AC-51, a dirty fd's remote
 * chunks, which a later change may need.
 *
 * Loads the chunks s3fs_mop_needs_data() selects that are not there.  A chunk
 * is fetched with an upcall flagged S3FS_MISS_FLAG_FOR_WRITE; the daemon fills
 * it with POPULATE_PAGES (also in arena mode) and signals with
 * S3FS_SIGNAL_VERIFY_POPULATED.  Same protocol as .read_folio: pin the chunk
 * first, then create the pending.  Returns -EIO when a chunk cannot be
 * loaded (daemon absent or failing): the write is refused rather than
 * flushed over the object.  Sleeps; call with no lock held. */
static int s3fs_fd_load_for_modify(struct inode *inode,
				   struct s3fs_file_data *fd,
				   const struct s3fs_mop *in,
				   struct s3fs_mpin *mp)
{
	struct s3fs_mop op;
	u32 ci, first, last;
	int err;

	mutex_lock(&fd->lock);
	s3fs_mpin_release_locked(fd, mp);
	if (!s3fs_mop_prepare(inode, fd, in, &op) ||
	    !s3fs_mop_range(&op, &first, &last)) {
		mutex_unlock(&fd->lock);
		return 0;
	}
	err = s3fs_mpin_acquire_locked(fd, mp, &op);
	mutex_unlock(&fd->lock);
	if (err)
		return err;

	for (ci = first; ci <= last; ci++) {
		u64 cs = (u64)ci << S3FS_PCACHE_CHUNK_SHIFT;
		int tries = 0;

		if (!s3fs_mop_needs_data(&op, ci))
			continue;
		for (;;) {
			struct s3fs_pending *p;
			struct mm_struct *mm;
			bool have, bound;
			long rem = 1;

			err = 0;
			mutex_lock(&fd->lock);
			have = s3fs_mop_chunk_ready(fd, ci);
			/* AC-51: a dirty / remote-holding fd depends on the pin. */
			bound = fd->dirty || fd->flushing || fd->nr_remote;
			mutex_unlock(&fd->lock);
			if (have)
				break;

			read_lock(&s3fs_admits_lock);
			mm = s3fs_admitted_mm;
			read_unlock(&s3fs_admits_lock);
			if (!mm) {
				err = -EIO; /* no daemon: cannot fetch the rest */
				p = NULL;
			} else {
				p = pending_get_or_create(inode->i_ino, cs);
				/* Count = what the chunk must hold up to i_size.
				 * The daemon's size for the object can be older
				 * (appended + flushed, entry not re-synced); with
				 * a shorter chunk it never becomes populated. */
				u64 isz = i_size_read(inode);
				u64 want = min_t(u64, S3FS_CHUNK_SIZE,
						 isz > cs ? isz - cs : 1);

				err = p ? miss_enqueue(inode->i_ino, cs, want,
						       S3FS_MISS_FLAG_FOR_WRITE |
						       (bound ? S3FS_MISS_FLAG_PINNED : 0))
					: -ENOMEM;
			}
			if (!err) {
				rem = wait_for_completion_interruptible_timeout(
					&p->done, S3FS_UPCALL_WAIT_JIFFIES);
				if (rem < 0)
					err = (int)rem;	/* -ERESTARTSYS */
				else if (rem > 0)
					err = p->err;	/* daemon-side failure */
			}
			if (p)
				pending_put(p);
			if (err == -ERESTARTSYS || err == -EINTR) {
				s3fs_mpin_release(fd, mp);
				return err;
			}
			if (err) {
				/* The daemon refused (it will not populate a
				 * chunk a concurrent writer already holds,
				 * -EBUSY): fine if the chunk is there now. */
				mutex_lock(&fd->lock);
				have = s3fs_mop_chunk_ready(fd, ci);
				mutex_unlock(&fd->lock);
				if (have)
					break;
				s3fs_mpin_release(fd, mp);
				return -EIO;
			}
			if (++tries >= S3FS_LOAD_MAX_TRIES) {
				s3fs_mpin_release(fd, mp);
				return -EIO;
			}
		}
	}
	return 0;
}

static ssize_t s3fs_ebpf_file_write(struct file *file,
				    const char __user *user_buf,
				    size_t count, loff_t *pos)
{
	struct inode *inode = file_inode(file);
	struct s3fs_file_data *fd = inode->i_private;
	struct s3fs_mpin mp = {};
	struct s3fs_mop op;
	loff_t start;
	int err, tries;

	if (!fd)
		return -EIO;
	if (!count)
		return 0;

	/* AC-50: the first modification of a clean file must start from its
	 * current content, not from an empty buffer.  Chunks the write covers
	 * completely are not fetched.  The loader leaves its chunks pinned
	 * and we confirm under fd->lock that they are all present; the lock
	 * is then held until the dirty mark, so nothing can free them.  If
	 * the check fails, drop the lock and load again. */
	for (tries = 0;; tries++) {
		loff_t lstart = (file->f_flags & O_APPEND) ?
				i_size_read(inode) : *pos;
		u64 isz = (u64)i_size_read(inode);

		op = (struct s3fs_mop){
			.end = isz,
			.ow_lo = (u64)max_t(loff_t, lstart, 0),
			.ow_hi = (u64)max_t(loff_t, lstart, 0) + count,
			.t_lo = min_t(u64, (u64)max_t(loff_t, lstart, 0), isz),
			.t_hi = (u64)max_t(loff_t, lstart, 0) + count,
			.partial = s3fs_partial_wanted(inode),
		};
		mutex_lock(&fd->lock);
		if (lstart < 0 || s3fs_fd_modify_ready_locked(inode, fd, &op))
			break;
		mutex_unlock(&fd->lock);
		if (tries >= S3FS_LOAD_MAX_TRIES)
			err = -EIO;
		else
			err = s3fs_fd_load_for_modify(inode, fd, &op, &mp);
		if (err) {
			s3fs_mpin_release(fd, &mp);
			return err;
		}
	}
	/* fd->lock is held from the check to the dirty mark below, so the pins
	 * are no longer needed. */
	s3fs_mpin_release_locked(fd, &mp);
	err = s3fs_fd_mark_remote_locked(inode, fd, &op);
	if (err) {
		mutex_unlock(&fd->lock);
		return err;
	}
	if (!fd->dirty && !fd->flushing)
		fd->size = max_t(size_t, fd->size, (size_t)i_size_read(inode));
	start = (file->f_flags & O_APPEND) ? (loff_t)fd->size : *pos;
	if (start < 0) {
		err = -EINVAL;
		goto out;
	}

	err = s3fs_fd_store(fd, (u64)start, NULL, user_buf, count, 0);
	if (err)
		goto out;
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
	/* The page cache still holds the pre-write content of the range (it
	 * was filled by an earlier read); a later read would be served from
	 * it and miss this write.  Drop those folios so .read_folio refills
	 * them from the fd.  After the unlock: .read_folio takes fd->lock
	 * under the folio lock that invalidate waits for.  Folios that cannot
	 * be dropped (mmap'ed) stay; same as before. */
	invalidate_inode_pages2_range(inode->i_mapping,
				      start >> PAGE_SHIFT,
				      (start + count - 1) >> PAGE_SHIFT);
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
		bool owned;
		struct s3fs_mpin mp = {};
		struct s3fs_mop op;
		u64 cur;
		int tries;

		/* AC-50: a clean file's size is the object's (i_size); fd->size
		 * only covers what was fetched.  Changing it must start from the
		 * content that survives, and even a truncate that fetches
		 * nothing (`: > f`) has to mark the file dirty so it is PUT. */
		for (tries = 0;; tries++) {
			mutex_lock(&fd->lock);
			owned = fd->dirty || fd->flushing;
			cur = owned ? (u64)fd->size : (u64)i_size_read(inode);
			op = (struct s3fs_mop){
				.end = min_t(u64, new_size, cur),
				.t_lo = min_t(u64, new_size, cur),
				.t_hi = max_t(u64, new_size, cur),
				.partial = s3fs_partial_wanted(inode),
			};
			if ((u64)new_size == cur ||
			    s3fs_fd_modify_ready_locked(inode, fd, &op))
				break;
			mutex_unlock(&fd->lock);
			if (tries >= S3FS_LOAD_MAX_TRIES)
				err = -EIO;
			else
				err = s3fs_fd_load_for_modify(inode, fd, &op, &mp);
			if (err) {
				s3fs_mpin_release(fd, &mp);
				return err;
			}
		}
		/* fd->lock stays held until the size change is applied. */
		s3fs_mpin_release_locked(fd, &mp);
		if ((u64)new_size != cur) {
			err = s3fs_fd_mark_remote_locked(inode, fd, &op);
			if (err) {
				mutex_unlock(&fd->lock);
				return err;
			}
		}
		if (!fd->dirty && !fd->flushing && (u64)new_size != cur)
			fd->size = max_t(size_t, fd->size, (size_t)cur);
		if (new_size < (loff_t)fd->size) {
			/* AC-45: free the chunks past the cut and zero the
			 * boundary tail; an extension later reads zeros, not
			 * the bytes that were cut off. */
			s3fs_fd_truncate(fd, new_size);
			fd->size = new_size;
			fd->dirty = true;
		} else if (new_size > (loff_t)fd->size) {
			/* Sparse extension: the new range is a hole (no chunk
			 * allocated) that reads as zeros while dirty. */
			if (((u64)new_size >> S3FS_PCACHE_CHUNK_SHIFT) >=
			    S3FS_MAX_CHUNKS) {
				mutex_unlock(&fd->lock);
				return -EFBIG;
			}
			fd->size = new_size;
			fd->dirty = true;
		}
		i_size_write(inode, new_size);
		mutex_unlock(&fd->lock);
		/* Folios cached before the size change hold the old tail (or
		 * the pre-truncate bytes of a later extension).  After the
		 * unlock for the same reason as in .write. */
		if ((u64)new_size != cur)
			truncate_pagecache(inode, new_size);
	}
	setattr_copy(idmap, inode, attr);

	/* G-02: record mode / uid / gid so a re-lookup after icache
	 * eviction restores what chmod / chown just set. Without this
	 * `chmod 755 x; sysctl -w vm.drop_caches=2; stat x` regresses
	 * to the bootstrap mode. */
	if (attr->ia_valid & (ATTR_MODE | ATTR_UID | ATTR_GID)) {
		u32 valid = 0;
		if (attr->ia_valid & ATTR_MODE)
			valid |= S3FS_ATTR_OV_MODE;
		if (attr->ia_valid & ATTR_UID)
			valid |= S3FS_ATTR_OV_UID;
		if (attr->ia_valid & ATTR_GID)
			valid |= S3FS_ATTR_OV_GID;
		attr_override_upsert(inode->i_ino, valid,
				     attr->ia_mode, attr->ia_uid, attr->ia_gid);
	}
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

/* Returns true when at least one page was pinned (folio fully or partially
 * filled from arena).  Returns false when pin_user_pages_remote failed on
 * the very first iteration — the folio was zeroed but must NOT be marked
 * uptodate; the caller should fall through to the upcall path instead.
 * Partial fills (some pages pinned, later ones failing) still return true —
 * the filled region is correct and the tail is zero-padded as usual. */
static bool s3fs_folio_fill_from_arena(struct folio *folio, size_t fs_size,
				       loff_t pos,
				       struct mm_struct *mm,
				       u64 uaddr, u64 asize)
{
	size_t remaining, total = 0;
	void *fkva;

	if ((u64)pos >= asize) {
		folio_zero_range(folio, 0, fs_size);
		return false;
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
	return total > 0;
}

/* Q2 / S1: returns true when fd->buf held valid data for pos (pcache hit).
 * Returns false when buf is NULL, pos is beyond buf, or the admit entry
 * disappeared between fc_pcache_check and here.  Caller must invalidate fc
 * and fall through to the upcall path rather than serving stale data as a hit.
 *
 * gen: the s3fs_admit_gen value captured by fc_pcache_check.  If the global
 * gen advanced before we take fd->lock (TOCTOU window), we re-verify the
 * PCACHE entry's existence under read_lock before trusting fd->buf.
 * gen == 0 disables this check (fallback path where no admit entry is expected).
 * Lock order when re-probing: fd->lock (outer) → s3fs_admits_lock (inner, read). */
static bool s3fs_folio_fill_from_kmalloc(struct folio *folio, size_t fs_size,
					  loff_t pos, struct s3fs_file_data *fd,
					  u64 ino, u32 gen, bool quiet)
{
	void *fkva;
	size_t avail = 0;
	bool buf_valid;

	if (!fd) {
		folio_zero_range(folio, 0, fs_size);
		return false;
	}
	mutex_lock(&fd->lock);
	/* S1: if gen changed since fc_pcache_check, re-verify the entry still
	 * covers pos before trusting fd->buf.  A mismatch means eviction (or
	 * a concurrent populate on a different range) ran in the TOCTOU window;
	 * we must not serve whatever happens to be in buf as a hit.
	 * S11: per-chunk bitmask — check bit for the 4-MiB chunk containing pos.
	 * This handles out-of-order chunk delivery and single-chunk re-populate
	 * after eviction (the old contiguous watermark buf_valid_end failed both).
	 * T1: dirty inodes own their buf entirely — the bitmap is never maintained
	 * for the write path, so bypass it when fd->dirty is set.
	 * T2: pcache path (gen != 0) — R1 invariant guarantees fd->buf covers any
	 * range with a live pcache entry; skip the 64-chunk bitmap cap so files
	 * > 256 MiB read correctly.  The S1 TOCTOU re-verify below handles the
	 * evict-race window (eviction+re-populate between fc_pcache_check and
	 * fd->lock) without needing a bitmap for ci >= 64. */
	{
		u32 ci = (u32)((u64)pos >> S3FS_PCACHE_CHUNK_SHIFT);
		struct s3fs_chunk *c = ci < fd->nr_chunks ? &fd->chunks[ci] : NULL;

		if (fd->dirty || fd->flushing) {
			/* T1: dirty or in-flight flush — the chunks are owned by
			 * the write/flush path; bypass the populated flags.  A
			 * missing chunk is a hole and reads as zeros. */
			buf_valid = (size_t)pos < fd->size &&
				    !(c && c->remote);
		} else if (gen != 0) {
			/* T2: after reset()/partial re-populate a chunk may be
			 * allocated but not yet fetched (its tail is zero); the
			 * pcache entry is only inserted after the copy succeeds
			 * and the populated flag is the reliable proof that the
			 * data is present.  Every chunk has a flag, so files
			 * > 256 MiB are checked like any other. */
			buf_valid = c && c->p && c->populated &&
				    (size_t)pos < fd->size;
		} else {
			buf_valid = c && c->p && c->populated &&
				    (size_t)pos < fd->size;
			/* D-diag: instrument gen=0 failure reason (the
			 * data-based probe after a wake is a normal try, not
			 * a fallback failure — `quiet`). */
			if (!buf_valid && !quiet) {
				if (!c || !c->p)
					atomic_long_inc(&s3fs_gen0_fail_buf_null);
				else if ((size_t)pos >= fd->size)
					atomic_long_inc(&s3fs_gen0_fail_other);
				else
					atomic_long_inc(&s3fs_gen0_fail_bitmap);
			}
		}
	}
	/* AC-45 C3: the data-based probe after a wake serves a chunk whose
	 * PCACHE entry a capacity pass dropped (the chunk survived because this
	 * reader pinned it).  Give it back an entry: otherwise the folio we fill
	 * would sit in the page cache outside the capacity accounting, never
	 * evicted.  Once the pin is gone the next pass evicts it normally. */
	/* AC-57: only for a clean fd and a populated chunk.  A dirty/flushing
	 * fd is not capacity-managed, and its chunk may be a hole (buf_valid is
	 * true for a hole there): an entry over it would inflate the capacity
	 * accounting. */
	if (buf_valid && quiet && !fd->dirty && !fd->flushing) {
		u32 ci = (u32)((u64)pos >> S3FS_PCACHE_CHUNK_SHIFT);
		u64 cs = (u64)ci << S3FS_PCACHE_CHUNK_SHIFT;

		if (ci < fd->nr_chunks && fd->chunks[ci].p &&
		    fd->chunks[ci].populated)
			(void)admit_insert_pcache(ino, cs,
						  min_t(u64, S3FS_CHUNK_SIZE,
							(u64)fd->size - cs));
	}
	if (buf_valid && gen != 0 &&
	    atomic_read(&s3fs_admit_gen) != gen) {
		struct s3fs_admit *a;
		read_lock(&s3fs_admits_lock);
		a = admit_lookup_at_locked(ino, (u64)pos);
		if (!a || !(a->flags & S3FS_ADMIT_FLAG_PCACHE))
			buf_valid = false;
		read_unlock(&s3fs_admits_lock);
	}
	if (buf_valid) {
		u32 ci = (u32)((u64)pos >> S3FS_PCACHE_CHUNK_SHIFT);
		size_t chunk_end = min_t(size_t,
					 (size_t)(ci + 1) << S3FS_PCACHE_CHUNK_SHIFT,
					 fd->size);
		avail = min_t(size_t, fs_size, chunk_end - (size_t)pos);
	}
	fkva = kmap_local_folio(folio, 0);
	if (avail > 0) {
		u32 ci = (u32)((u64)pos >> S3FS_PCACHE_CHUNK_SHIFT);
		u32 off = (u32)((u64)pos & (S3FS_CHUNK_SIZE - 1));
		size_t have = 0;

		/* A chunk can be shorter than fd->size reaches (sparse
		 * extension, or a short tail): the rest reads as zeros. */
		if (ci < fd->nr_chunks && fd->chunks[ci].p &&
		    fd->chunks[ci].cap > off)
			have = min_t(size_t, avail, fd->chunks[ci].cap - off);
		if (have)
			memcpy(fkva, fd->chunks[ci].p + off, have);
		if (have < avail)
			memset((u8 *)fkva + have, 0, avail - have);
	}
	if (avail < fs_size)
		memset((u8 *)fkva + avail, 0, fs_size - avail);
	kunmap_local(fkva);
	mutex_unlock(&fd->lock);
	return buf_valid;
}

/* AA-03: copy arena data directly into a user iov_iter, bypassing the
 * page-cache folio path entirely. One page at a time via GUP so we stay
 * within the pin_user_pages_remote() contract (no sleeping while pinned). */
static ssize_t s3fs_arena_copy_to_iter(struct mm_struct *mm,
					u64 uaddr, u64 avail,
					struct iov_iter *to, size_t copy_len)
{
	size_t remaining = copy_len;
	ssize_t total = 0;

	while (remaining > 0) {
		struct page *ap = NULL;
		void *akva;
		unsigned long abs = (unsigned long)(uaddr + (u64)total);
		unsigned long pgoff = abs & (PAGE_SIZE - 1);
		size_t chunk = min_t(size_t, remaining, PAGE_SIZE - pgoff);
		long pinned;
		int locked = 1;
		size_t copied;

		mmap_read_lock(mm);
		{
			ktime_t t0 = ktime_get();
			pinned = pin_user_pages_remote(mm, abs & PAGE_MASK, 1, 0, &ap, &locked);
			atomic_long_inc(&s3fs_gup_calls_total);
			atomic_long_add(ktime_to_ns(ktime_sub(ktime_get(), t0)), &s3fs_gup_ns_total);
		}
		if (locked)
			mmap_read_unlock(mm);
		if (pinned != 1)
			break;
		akva = kmap_local_page(ap);
		copied = copy_to_iter((u8 *)akva + pgoff, chunk, to);
		kunmap_local(akva);
		unpin_user_pages(&ap, 1);
		if (!copied)
			break;
		total += (ssize_t)copied;
		remaining -= copied;
	}
	return total;
}

/* S5: max upcall retries before falling back to the bootstrap buffer.
 * Guards against eviction firing in the window between signal_admit
 * (which unhashes the pending, unblocking eviction) and the waiter
 * resuming to check the fast paths.  Each retry re-creates the pending,
 * which blocks eviction of this ino while the daemon re-populates.
 * U8: buf_readers on fd extends this protection to cover the window
 * between pending_unhash and the gen=0 fallback taking fd->lock. */
#define S3FS_UPCALL_MAX_RETRIES 3

static int s3fs_ebpf_read_folio(struct file *file, struct folio *folio)
{
	struct inode *inode = folio->mapping->host;
	struct s3fs_file_data *fd = inode->i_private;
	struct mm_struct *mm = NULL;
	u64 uaddr = 0, asize = 0;
	loff_t pos = folio_pos(folio);
	size_t fs_size = folio_size(folio);
	u32 pcache_gen;
	int upcall_retries = 0;
	bool filled = false;
	bool pinned = false;	/* U8: chunk pin held (per chunk) */
	u32 pin_ci = 0;

retry:
	/* AB-04 / AB-10 fast path: page-cache mode admit — data is in fd->buf.
	 * fc_pcache_check avoids the rwlock when the per-file cache is warm.
	 * file may be NULL on readahead paths; fc_pcache_check handles NULL fc. */
	pcache_gen = 0;
	if (fc_pcache_check(file ? file->private_data : NULL,
			    inode->i_ino, (u64)pos, &pcache_gen)) {
		/* U10 (buf_readers++ on this hit path) was removed: a buf freed
		 * between a failed fill and the upcall below is re-populated by
		 * that upcall, and P1 keeps the ino unevictable while it waits.
		 * A/B on evict-race (120 s, 32 readers): read_err 0 with and
		 * without it; ab-pcache unchanged.  U8 below still covers the
		 * retry window. */
		if (s3fs_folio_fill_from_kmalloc(folio, fs_size, pos,
						 inode->i_private,
						 inode->i_ino, pcache_gen,
						 false)) {
			atomic_long_inc(&s3fs_folio_hits);
			s3fs_policy_on_hit(inode->i_ino);
			(void)s3fs_policy_ops_eval_hit(inode->i_ino);
			filled = true;
			goto done;
		}
		/* Q2/S1: buf was invalid (evicted or TOCTOU); invalidate fc so
		 * the next call re-probes the table, then fall to upcall. */
		if (file && file->private_data) {
			struct s3fs_file_admit_cache *fc_local = file->private_data;
			smp_store_release(&fc_local->gen, 0);
		}
	}

	/* Fast path: arena admit hit. admit_snapshot() returns the arena
	 * address for `pos` and the bytes available in the covering
	 * range. fill_from_arena reads from the returned uaddr with
	 * pos=0 (range-relative), copying up to `avail` bytes.
	 * Skip when buf is owned by write/flush — arena may have stale
	 * prewarm content from an earlier S3 version of this file. */
	if (!s3fs_fd_buf_owned(inode) &&
	    admit_snapshot(inode->i_ino, (u64)pos, &uaddr, &asize, &mm) && mm) {
		if (s3fs_folio_fill_from_arena(folio, fs_size, 0, mm, uaddr, asize)) {
			atomic_long_inc(&s3fs_folio_hits);
			s3fs_policy_on_hit(inode->i_ino);
			(void)s3fs_policy_ops_eval_hit(inode->i_ino);
			filled = true;
			goto done;
		}
		/* GUP failed: arena pages inaccessible; fall through to upcall. */
	}

	/* AC-45 C3: we were woken by a verified success signal (the chunk was
	 * populated and this reader pinned it), but the PCACHE entry can
	 * already be gone — the daemon's capacity pass evicts entries right
	 * after a populate.  Serve from the chunk instead of upcalling again,
	 * which the next pass would undo the same way. */
	if (upcall_retries > 0 && fd &&
	    s3fs_folio_fill_from_kmalloc(folio, fs_size, pos, fd, inode->i_ino,
					 0, true)) {
		filled = true;
		goto done;
	}

	/* Miss + daemon alive: trigger upcall, wait, retry fast paths.
	 * S5: retry up to S3FS_UPCALL_MAX_RETRIES times so that eviction
	 * firing in the signal_admit → wakeup window does not cause zeros. */
	read_lock(&s3fs_admits_lock);
	mm = s3fs_admitted_mm;
	read_unlock(&s3fs_admits_lock);

	/* A dirty / flushing fd owns its content: a chunk it has (or a hole)
	 * is served from the fd, the daemon has nothing to add and would only
	 * be refused (populate -EBUSY) after a timeout per folio.  The
	 * exception is a remote chunk (AC-51), which the daemon loads. */
	if (mm && fd) {
		u32 lci = (u32)((u64)pos >> S3FS_PCACHE_CHUNK_SHIFT);

		mutex_lock(&fd->lock);
		if ((fd->dirty || fd->flushing) &&
		    !(lci < fd->nr_chunks && fd->chunks[lci].remote))
			mm = NULL;
		mutex_unlock(&fd->lock);
	}

	if (mm && upcall_retries < S3FS_UPCALL_MAX_RETRIES) {
		u64 chunk_offset = ((u64)pos >> S3FS_PCACHE_CHUNK_SHIFT)
				   << S3FS_PCACHE_CHUNK_SHIFT;
		struct s3fs_pending *p;

		/* U8: protect fd->buf against eviction for the whole retry
		 * window (set once; later retries reuse it).  AC-45: register
		 * BEFORE the pending exists, so a success signal that finds
		 * the pending can rely on this reader already being counted. */
		if (!pinned && fd) {
			pin_ci = (u32)((u64)pos >> S3FS_PCACHE_CHUNK_SHIFT);
			mutex_lock(&fd->lock);
			pinned = s3fs_fd_chunk_pin(fd, pin_ci);
			mutex_unlock(&fd->lock);
		}
		p = pending_get_or_create(inode->i_ino, chunk_offset);

		if (p) {
			int err;
			long remaining;
			loff_t mpos = pos;
			size_t mcount = fs_size;
			u32 mflags = 0;

			/* AC-51: a remote chunk of a dirty fd is the S3
			 * object's own bytes that the fd does not hold: have
			 * the daemon load the whole chunk (same request as a
			 * load for write; kmod accepts it because the chunk is
			 * remote). */
			if (fd) {
				u32 mci = (u32)(chunk_offset >>
						S3FS_PCACHE_CHUNK_SHIFT);

				mutex_lock(&fd->lock);
				if ((fd->dirty || fd->flushing) &&
				    mci < fd->nr_chunks &&
				    fd->chunks[mci].remote) {
					u64 isz = i_size_read(inode);

					mflags = S3FS_MISS_FLAG_FOR_WRITE |
						 S3FS_MISS_FLAG_PINNED;
					mpos = chunk_offset;
					mcount = min_t(u64, S3FS_CHUNK_SIZE,
						       isz > chunk_offset ?
						       isz - chunk_offset : 1);
				}
				mutex_unlock(&fd->lock);
			}
			err = miss_enqueue(inode->i_ino, mpos, mcount, mflags);
			if (err == 0) {
				remaining = wait_for_completion_interruptible_timeout(
					&p->done, S3FS_UPCALL_WAIT_JIFFIES);
				err = p->err;
				pending_put(p);
				if (remaining > 0 && err == 0) {
					upcall_retries++;
					goto retry;
				}
				/* U11: timed out waiting for daemon signal — daemon
				 * goroutine pool saturated (readMissWorkers=16),
				 * miss queued > S3FS_UPCALL_WAIT_JIFFIES.  Count
				 * against the retry budget and loop back; the next
				 * pass finds buf already populated by the delayed
				 * goroutine or gets a fast reply once pool pressure
				 * eases.  Without U11 gen=0 hit NULL buf → -EIO. */
				if (remaining == 0) {
					atomic_long_inc(&s3fs_upcall_timeout);
					upcall_retries++;
					goto retry;
				}
			} else {
				pending_put(p);
			}
		}
	}

	/* T4: count retry exhaustion (daemon alive but pcache entry evicted on
	 * every retry, or U11 timeout retries exhausted).  U8: buf_readers
	 * protects fd->buf for the entire retry window so the gen=0 fallback
	 * finds valid data if the daemon populated at any point. */
	if (mm && upcall_retries >= S3FS_UPCALL_MAX_RETRIES)
		atomic_long_inc(&s3fs_upcall_retry_exhausted);

	/* Fallback: per-inode bootstrap buffer (gen=0, no TOCTOU re-probe).
	 * Reached when the daemon is absent, timed out, or all retries are
	 * exhausted.  U8: buf_readers > 0 prevents eviction from clearing
	 * fd->buf, so this path finds valid data from the last successful
	 * populate and returns true rather than -EIO. */
	filled = s3fs_folio_fill_from_kmalloc(folio, fs_size, pos,
					      inode->i_private, inode->i_ino, 0,
					      false);

done:
	/* U8: release buf reader protection so the next eviction cycle can
	 * reclaim fd->buf once no more retries are in flight. */
	if (pinned && fd) {
		mutex_lock(&fd->lock);
		s3fs_fd_chunk_unpin(fd, pin_ci);
		mutex_unlock(&fd->lock);
	}
	if (filled)
		folio_mark_uptodate(folio);
	folio_unlock(folio);
	return filled ? 0 : -EIO;
}

/* AA-03 / AB-04: .read_iter fast path.
 *
 * Priority order:
 *   1. AB-04 page-cache mode (PCACHE admit flag): skip arena entirely,
 *      fall through to generic_file_read_iter so page cache serves the data.
 *      .read_folio fills fd->buf → folio on the first miss; subsequent
 *      reads are served from the uptodate folio without touching .read_folio.
 *   2. Arena mode (AA-03): admit hit → s3fs_arena_copy_to_iter (GUP path).
 *      Bypassed when s3fs_arena_bypass=1 (AB-02 bench flag).
 *   3. Fallback: generic_file_read_iter (page cache + .read_folio upcall). */
static ssize_t s3fs_ebpf_read_iter(struct kiocb *iocb, struct iov_iter *to)
{
	struct file *file = iocb->ki_filp;
	struct inode *inode = file_inode(file);
	loff_t pos = iocb->ki_pos;
	loff_t size = i_size_read(inode);
	u64 uaddr = 0, avail = 0;
	struct mm_struct *mm = NULL;

	/* AB-04 / AB-10: page-cache mode — data in fd->buf / page cache.
	 * fc_pcache_check avoids the rwlock on the hot path once the cache
	 * entry is confirmed for this file descriptor. */
	if (pos < size &&
	    fc_pcache_check(file->private_data, inode->i_ino, (u64)pos, NULL))
		return generic_file_read_iter(iocb, to);

	if (!s3fs_arena_bypass && !s3fs_fd_buf_owned(inode) && pos < size &&
	    admit_snapshot(inode->i_ino, (u64)pos, &uaddr, &avail, &mm) && mm) {
		size_t count = iov_iter_count(to);
		size_t copy_len = min3(count, (size_t)avail,
				       (size_t)(size - pos));
		ssize_t copied = s3fs_arena_copy_to_iter(mm, uaddr, avail,
							  to, copy_len);
		if (copied > 0) {
			atomic_long_inc(&s3fs_folio_hits);
			s3fs_policy_on_hit(inode->i_ino);
			(void)s3fs_policy_ops_eval_hit(inode->i_ino);
			iocb->ki_pos += copied;
			return copied;
		}
	}
	return generic_file_read_iter(iocb, to);
}

static const struct address_space_operations s3fs_ebpf_aops = {
	.read_folio	= s3fs_ebpf_read_folio,
	.dirty_folio	= noop_dirty_folio,
};

/* Wait up to this many jiffies for the daemon PUT to complete on
 * fsync path. Longer than the read-miss timeout because PUT can be
 * larger and involve dirty-buffer copy — 30 s covers big-blob upload
 * to real S3 in most configs. Interruptible so SIGINT / Ctrl+C on
 * the caller returns -EINTR promptly. */
#define S3FS_FSYNC_WAIT_JIFFIES  (30 * HZ)

/*
 * If `fd->dirty`, snapshot ino/size/name under fd->lock and enqueue a
 * flush event. dirty is cleared here so a subsequent close/fsync
 * without new writes is a no-op.
 *
 * `wait` == false (called from .release):
 *   Fire and forget. Daemon PUTs in the background; close(2) returns
 *   immediately. Failure surfaces later on next fsync/close or in
 *   metrics (s3fs_ebpf_flush_errors_total).
 *
 * `wait` == true (called from .fsync):
 *   Register a completion under `ino` BEFORE enqueue so
 *   signal_flush from the daemon can find us. Wait up to
 *   S3FS_FSYNC_WAIT_JIFFIES for the completion, propagate PUT result
 *   as fsync() return value. This gives POSIX-conforming fsync
 *   semantics (E-01, `docs/journal/backlog.md`).
 *
 * Returns 0 on success, negative errno on wait timeout / PUT failure.
 */
static int s3fs_maybe_enqueue_flush(struct file *file, bool wait)
{
	struct inode *inode = file_inode(file);
	struct s3fs_file_data *fd = inode->i_private;
	struct dentry *dentry = file->f_path.dentry;
	struct dentry *parent_dentry;
	struct s3fs_flush_pending *p = NULL;
	u64 size, ino, parent_ino;
	char name[S3FS_SYNC_NAME_MAX + 1];
	size_t name_len;
	int err = 0;

	if (!fd || !dentry)
		return 0;

	name_len = dentry->d_name.len;
	if (name_len == 0 || name_len > S3FS_SYNC_NAME_MAX)
		return 0;
	memcpy(name, dentry->d_name.name, name_len);

	/* G-01: pick parent ino from the dentry parent so daemon can
	 * rebuild the full S3 key. Falls back to root when the file was
	 * opened via a bare fd path (rare — file_inode's parent link
	 * is present for regular open(2) / creat(2)). */
	parent_dentry = dentry->d_parent;
	parent_ino = parent_dentry ? d_inode(parent_dentry)->i_ino
				   : (u64)S3FS_ROOT_INO;

	mutex_lock(&fd->lock);
	if (!fd->dirty) {
		mutex_unlock(&fd->lock);
		return 0; /* nothing to flush — fsync on clean file is a no-op */
	}
	size = fd->size;
	fd->dirty = false;
	/* U5: guard the in-flight flush window.  POPULATE_PAGES and the
	 * read-folio dirty bypass both check fd->flushing so that a read miss
	 * racing the PUT cannot overwrite fd->buf with stale S3 data.
	 * signal_flush clears this flag and marks the bitmap once the daemon
	 * confirms the PUT succeeded. */
	fd->flushing = true;
	/* W1: hold the inode until the daemon acknowledges this flush. */
	fd->flush_seq++;
	s3fs_flush_pin(inode, fd);
	mutex_unlock(&fd->lock);

	ino = inode->i_ino;

	if (wait) {
		/* Register BEFORE enqueue so a fast daemon can't miss us. */
		p = flush_pending_get_or_create(ino);
		if (!p) {
			/* Fall back to non-blocking on OOM — better a stale
			 * dirty than a lost write. */
			flush_enqueue(ino, parent_ino, size, name, name_len, 0);
			return -ENOMEM;
		}
	}

	/* wait=true marks the event as fsync-triggered so daemon skips
	 * coalescing (E-03) — the caller is blocked, latency matters. */
	flush_enqueue(ino, parent_ino, size, name, name_len,
		      wait ? S3FS_FLUSH_FLAG_WAIT : 0);

	if (p) {
		long remaining = wait_for_completion_interruptible_timeout(
			&p->done, S3FS_FSYNC_WAIT_JIFFIES);
		if (remaining < 0) {
			err = -EINTR;
		} else if (remaining == 0) {
			err = -EIO; /* daemon dead / stuck */
		} else {
			err = p->err;
		}
		flush_pending_put(p);
	}
	return err;
}

static int s3fs_ebpf_file_release(struct inode *inode, struct file *file)
{
	/* close(2): fire and forget, do not block. */
	(void)s3fs_maybe_enqueue_flush(file, false);
	/* AB-10: free per-file admit cache */
	kfree(file->private_data);
	file->private_data = NULL;
	return 0;
}

static int s3fs_ebpf_file_fsync(struct file *file, loff_t start,
				loff_t end, int datasync)
{
	/* POSIX fsync: block until the daemon confirms PUT durability. */
	return s3fs_maybe_enqueue_flush(file, true);
}

/*
 * nocache=1 — FUSE-equivalent read path, for A/B benchmarks only.
 *
 * s3fs-fuse without `-o kernel_cache` (the "fuse-raw" baseline) drops an
 * inode's page cache on every open and has no content cache of its own,
 * so each open+read goes kernel → userspace daemon → S3. With nocache
 * set, .open does the same here: it drops the inode's page cache and
 * its arena admit ranges, so the next .read_folio misses and upcalls
 * the daemon, which GETs from S3 (run the daemon without --cache-dir).
 * What remains different from FUSE is that metadata stays in kmod (no
 * getattr/open upcall) and the transport (miss queue + arena + ioctl
 * vs /dev/fuse).
 *
 * Inodes with an unflushed write buffer are left alone so a read-back
 * before the PUT still sees local data. Runtime-toggleable:
 *   echo 1 > /sys/module/s3fs_kfunc/parameters/nocache
 */
static bool s3fs_nocache;
module_param_named(nocache, s3fs_nocache, bool, 0644);
MODULE_PARM_DESC(nocache,
	"Drop page cache + arena admit on every open (FUSE-raw-like reads, benchmark only)");

static int s3fs_ebpf_file_open(struct inode *inode, struct file *file)
{
	struct s3fs_file_data *fd = inode->i_private;
	bool dirty = false;

	/* AB-10: allocate per-file admit cache; gen=0 forces first-read re-check. */
	if (!file->private_data) {
		struct s3fs_file_admit_cache *fc =
			kzalloc(sizeof(*fc), GFP_KERNEL);
		if (fc)
			file->private_data = fc;
	}

	if (!READ_ONCE(s3fs_nocache))
		return 0;
	if (fd) {
		mutex_lock(&fd->lock);
		dirty = fd->dirty;
		mutex_unlock(&fd->lock);
	}
	if (dirty)
		return 0;

	/*
	 * S-09 fix — two-part invalidation for reliable nocache behaviour:
	 *
	 * 1. Drop admit from kmod hash AND push to the evict log so the
	 *    daemon's reconcileEvictionsLocked picks it up before the next
	 *    admit_replace.  Without the evict_log_push the daemon still
	 *    holds the entry in its `admits` snapshot and will re-send it on
	 *    the next push (e.g. on a miss for a different ino), re-admitting
	 *    the file before the blocked .read_folio wakes — visible as an
	 *    arena hit instead of a miss for tiny files.
	 *
	 * 2. Use invalidate_inode_pages2 instead of invalidate_mapping_pages.
	 *    invalidate_mapping_pages uses folio_trylock and silently skips
	 *    folios that are locked or have elevated refcounts — common for
	 *    small (single-folio) files that were recently accessed.
	 *    invalidate_inode_pages2 waits for the lock and forcibly drops all
	 *    clean folios, so the next read_iter reaches .read_folio rather
	 *    than being served directly from the page cache.
	 */
	/* AB-10 / P4: reset per-file cache before dropping admits so next read
	 * re-checks via rwlock.  smp_store_release ensures a concurrent
	 * reader that loads gen=0 via smp_load_acquire also sees pcache=false
	 * (the zeroed struct from kzalloc), not a stale pcache=true. */
	if (file->private_data) {
		struct s3fs_file_admit_cache *fc = file->private_data;
		WRITE_ONCE(fc->pcache, false);
		WRITE_ONCE(fc->off, 0);
		WRITE_ONCE(fc->end, 0);
		smp_store_release(&fc->gen, 0);
	}
	admit_drop_all_ranges(inode->i_ino, NULL, NULL);
	evict_log_push(inode->i_ino);
	if (invalidate_inode_pages2(inode->i_mapping) != 0)
		/* Dirty folios present — harmless, dirty-file path is skipped above */
		invalidate_mapping_pages(inode->i_mapping, 0, -1);
	return 0;
}

static const struct file_operations s3fs_ebpf_file_fops = {
	.open		= s3fs_ebpf_file_open,
	.llseek		= generic_file_llseek,
	.read_iter	= s3fs_ebpf_read_iter,   /* AA-03 */
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
