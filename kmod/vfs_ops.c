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

			if (norm_parent(e->parent_ino) != want_parent ||
			    e->name_len != name_len ||
			    memcmp(e->name, name, name_len) != 0)
				continue;
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
	u64 uaddr;   /* daemon user_vm address of the range's bytes */
	u64 size;    /* range length */
	u64 offset;  /* file offset where the range starts (D-01) */
};

#define S3FS_ADMIT_HASH_BITS 6

static DEFINE_HASHTABLE(s3fs_admits, S3FS_ADMIT_HASH_BITS);
static DEFINE_SPINLOCK(s3fs_admits_lock);

/* Running size of the admit hash. Updated in lock-step with hash_add
 * / hash_del calls under s3fs_admits_lock. Exposed to BPF programs
 * via the s3fs_kfunc_admit_count() KFunc so policies can make
 * pressure-aware decisions (see bpf/policy_pressure_ops.bpf.c). */
atomic_t s3fs_admits_count = ATOMIC_INIT(0);

/* Soft threshold — evict pressure fires when count exceeds this.
 * Exported via s3fs_kfunc_softcap() so BPF programs know the same
 * number kmod's admit_replace loop uses. Must match the local
 * `softcap` constant in s3fs_ebpf_admit_replace(). */
#define S3FS_EVICT_SOFTCAP 8U

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

/* Reference on the daemon's mm, held while the admit table is
 * non-empty. Dropped by shutdown or by replacing the admits with
 * count=0. All entries in the table share this single mm. */
static struct mm_struct *s3fs_admitted_mm;

/* Set in fill_super so admit_replace can ilookup() cached inodes and
 * refresh their i_size when the daemon publishes a new content size. */
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

	spin_lock(&s3fs_admits_lock);
	a = admit_lookup_locked(ino);
	if (a)
		size = a->size;
	spin_unlock(&s3fs_admits_lock);
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
 * invalidate_mapping_pages() only drops clean, unlocked, unmapped
 * folios, same as the snapshot refresh above.
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
			staged[i]->offset = entries[i].offset;
		}
	}

	spin_lock(&s3fs_admits_lock);
	admit_clear_locked();
	for (i = 0; i < count; i++)
		hash_add(s3fs_admits, &staged[i]->node, staged[i]->ino);
	atomic_set(&s3fs_admits_count, count);
	prev_mm = s3fs_admitted_mm;
	/*
	 * Keep the daemon mm reference live when the admit set transiently
	 * empties (full policy eviction). The daemon is still alive and
	 * next .read_folio miss will upcall it to re-admit; nulling mm
	 * here would send that miss straight to the zero-page kmalloc
	 * fallback and break subsequent reads (P-07 hit-rate bench). The
	 * mm pointer only gets cleared through s3fs_ebpf_admit_shutdown()
	 * on daemon exit.
	 */
	if (count)
		s3fs_admitted_mm = mm;
	spin_unlock(&s3fs_admits_lock);

	kfree(staged); /* array itself; nodes now live in the hash */
	staged = NULL;

	/* Drop the previous mm ref only when we replaced it (count>0).
	 * For count=0 we kept prev_mm as the active pointer — no put. */
	if (count && prev_mm)
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
	 * snapshot, ask the loaded policy programs (fmod_ret and
	 * struct_ops, whichever is attached) whether the entry should be
	 * evicted. struct_ops wins when both are attached because it is
	 * the primary path (see docs/design/policy-attach-comparison.md).
	 *
	 * Enforcement threshold: only apply eviction once the snapshot
	 * exceeds a soft cap (S3FS_EVICT_SOFTCAP). This mirrors real
	 * cache-management systems that let a warm-up phase happen
	 * before applying pressure, and avoids gratuitous churn on tiny
	 * working sets.
	 */
	{
		u32 fmodret_votes = 0, structops_votes = 0, evicted = 0;
		const u32 softcap = S3FS_EVICT_SOFTCAP;

		for (i = 0; i < count; i++) {
			int fr_vote = s3fs_policy_on_evict(entries[i].ino);
			int so_vote = s3fs_policy_ops_eval_evict(entries[i].ino);

			if (fr_vote != 0)
				fmodret_votes++;
			if (so_vote != 0)
				structops_votes++;

			/* Enforcement: struct_ops has priority; fmod_ret is
			 * advisory when both are attached. */
			if (count <= softcap)
				continue;
			if (so_vote != 0 || fr_vote != 0) {
				struct s3fs_admit *a;
				struct hlist_node *tmp;
				bool dropped = false;

				spin_lock(&s3fs_admits_lock);
				hash_for_each_possible_safe(s3fs_admits, a, tmp,
							    node, entries[i].ino) {
					if (a->ino != entries[i].ino)
						continue;
					hash_del(&a->node);
					atomic_dec(&s3fs_admits_count);
					kfree(a);
					dropped = true;
					break;
				}
				spin_unlock(&s3fs_admits_lock);
				if (dropped) {
					evicted++;
					/* Log for daemon feedback so it stops
					 * re-sending this ino on the next
					 * admit_replace round. */
					evict_log_push(entries[i].ino);
					s3fs_evict_page_cache(entries[i].ino);
				}
			}
		}
		if (fmodret_votes)
			pr_info("s3fs_ebpf: fmod_ret voted evict on %u/%u entries\n",
				fmodret_votes, count);
		if (structops_votes)
			pr_info("s3fs_ebpf: struct_ops voted evict on %u/%u entries\n",
				structops_votes, count);
		if (evicted) {
			atomic_long_add(evicted, &s3fs_evictions_total);
			pr_info("s3fs_ebpf: policy evicted %u entries (softcap=%u)\n",
				evicted, softcap);
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
					size_t count)
{
	struct s3fs_local_entry *le, *tmp;
	size_t i;

	list_for_each_entry_safe(le, tmp, &s3fs_local_entries, link) {
		for (i = 0; i < count; i++) {
			if (entries[i].ino != le->ino)
				continue;
			/* G-01: name equality alone is not enough — two files
			 * with basename "foo.txt" under different parents must
			 * not cross-promote. Match parent_ino too. */
			if (norm_parent(entries[i].parent_ino) !=
			    norm_parent(le->parent_ino))
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
	u64 parent_ino; /* G-01: dir the file lives under, for daemon full-path resolution */
	u64 size;
	u32 name_len;
	u32 flags;  /* S3FS_FLUSH_FLAG_* (E-03) */
	char name[S3FS_SYNC_NAME_MAX + 1];
};

static LIST_HEAD(s3fs_flush_queue);
static DEFINE_SPINLOCK(s3fs_flush_lock);

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

	spin_lock(&s3fs_flush_pending_lock);
	hash_for_each_possible(s3fs_flush_pending, p, node, ino) {
		if (p->ino == ino) {
			target = p;
			refcount_inc(&target->refs);
			break;
		}
	}
	spin_unlock(&s3fs_flush_pending_lock);

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
	out->ino        = item.ino;
	out->parent_ino = item.parent_ino;
	out->size       = item.size;
	out->name_len   = item.name_len;
	out->flags      = item.flags;
	memcpy(out->name, item.name, item.name_len);
	out->name[item.name_len] = '\0';
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

	spin_lock(&s3fs_admits_lock);
	a = admit_lookup_at_locked(ino, pos);
	if (a) {
		u64 within = pos - a->offset;
		*out_uaddr = a->uaddr + within;
		*out_avail = a->size - within;
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

	/* Fast path: admit hit. admit_snapshot() returns the arena
	 * address for `pos` and the bytes available in the covering
	 * range. fill_from_arena reads from the returned uaddr with
	 * pos=0 (range-relative), copying up to `avail` bytes. */
	if (admit_snapshot(inode->i_ino, (u64)pos, &uaddr, &asize, &mm) && mm) {
		atomic_long_inc(&s3fs_folio_hits);
		/* Notify policy programs — recency refresh / freq bump /
		 * visited-bit set. Fires per folio in the hot read path;
		 * keep the wrappers lean (see policy_struct_ops.c). */
		s3fs_policy_on_hit(inode->i_ino);
		(void)s3fs_policy_ops_eval_hit(inode->i_ino);
		s3fs_folio_fill_from_arena(folio, fs_size, 0, mm, uaddr, asize);
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
				    admit_snapshot(inode->i_ino, (u64)pos,
						   &uaddr, &asize, &mm) && mm) {
					s3fs_folio_fill_from_arena(folio,
						fs_size, 0, mm, uaddr, asize);
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
	return 0;
}

static int s3fs_ebpf_file_fsync(struct file *file, loff_t start,
				loff_t end, int datasync)
{
	/* POSIX fsync: block until the daemon confirms PUT durability. */
	return s3fs_maybe_enqueue_flush(file, true);
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
		size_t i;

		for (i = 0; i < s3fs_dyn_nr; i++) {
			const struct s3fs_entry_dyn *e = &s3fs_dyn_entries[i];

			if (norm_parent(e->parent_ino) != want_parent)
				continue;
			if (matched++ != idx)
				continue;
			ino      = e->ino;
			dt       = S_ISDIR(e->mode) ? DT_DIR : DT_REG;
			name_len = e->name_len;
			memcpy(name, e->name, name_len);
			ok = true;
			break;
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

	spin_lock(&s3fs_admits_lock);
	hash_for_each_possible_safe(s3fs_admits, a, tmp, node, ino) {
		if (a->ino != ino)
			continue;
		hash_del(&a->node);
		atomic_dec(&s3fs_admits_count);
		kfree(a);
		break;
	}
	spin_unlock(&s3fs_admits_lock);
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
