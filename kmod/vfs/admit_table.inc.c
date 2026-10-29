/* SPDX-License-Identifier: GPL-2.0 */
/* admit table (ino -> daemon backing), pcache check, policy hooks, ino evict.  Part of vfs_ops.c: included from there, one translation
 * unit, never compiled on its own. */

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
	unsigned long hit_tick; /* hit_accum: jiffies of the last counted access
				 * through this struct file; 0 = not counted yet */
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
 * Issue #22 (docs/design/hit-signal-percpu-accumulation.md).
 * hit_accum=1 (default): .read_iter counts one access per open file in a per-CPU
 * slot of the inode's s3fs_file_data; admit_replace Phase 1 replays the
 * delta into the policy's on_hit under the evict mutex.  The direct
 * on_hit calls (fentry KFunc, .read_folio, arena read) then do nothing.
 *   hit_rearm_jiffies  an open file counts again after this many jiffies
 *                      (0 = once per open)
 *   hit_replay_max     on_hit calls per ino per Phase 1 (delta is cut)
 */
bool s3fs_hit_accum = true;
module_param_named(hit_accum, s3fs_hit_accum, bool, 0644);
MODULE_PARM_DESC(hit_accum,
	"Issue #22: count hits per CPU in .read_iter and replay them to the policy from admit_replace (default on; 0 = the direct on_hit calls)");
static unsigned int s3fs_hit_rearm_jiffies = HZ / 10;
module_param_named(hit_rearm_jiffies, s3fs_hit_rearm_jiffies, uint, 0644);
MODULE_PARM_DESC(hit_rearm_jiffies,
	"hit_accum: jiffies before an open file counts another access (0 = once per open)");
static unsigned int s3fs_hit_replay_max = 15;
module_param_named(hit_replay_max, s3fs_hit_replay_max, uint, 0644);
MODULE_PARM_DESC(hit_replay_max,
	"hit_accum: max on_hit calls per ino per admit_replace");
atomic_long_t s3fs_hits_replayed_total = ATOMIC_LONG_INIT(0);

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
static void s3fs_hit_forget(u64 ino);
static bool admit_evict_ino(u64 ino, u64 *freed)
{
	bool had_pcache = false;

	if (!admit_drop_all_ranges(ino, freed, &had_pcache))
		return false;
	evict_log_push(ino);
	s3fs_evict_page_cache(ino);
	if (had_pcache)
		s3fs_file_data_reset(ino);
	/* #22: every whole-ino eviction ends here, so this is the one place
	 * that resets the per-ino hit state (the next populate starts afresh). */
	s3fs_hit_forget(ino);
	return true;
}
