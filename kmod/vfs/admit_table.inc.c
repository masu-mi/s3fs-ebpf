/* SPDX-License-Identifier: GPL-2.0 */
/* admit table (ino -> daemon backing), pcache check, policy hooks, ino evict.  Part of vfs_ops.c: included from there, one translation
 * unit, never compiled on its own. */

/* -- Capacity / policy state ---------------------------------------------- */
/*
 * There is no admit table.  What the cache holds is what the inodes' chunk
 * tables hold (s3fs_file_data.chunks[], `p` and `populated`): a read is a hit
 * when its chunk is populated, the capacity pass finds its candidates by
 * walking the superblock's inodes (s3fs_collect_resident) and a whole-ino
 * eviction frees the idle chunks (s3fs_file_data_reset).  One state, so the
 * old "an entry implies a chunk" invariant (R1), its lockless fast path and
 * re-check (S1), the generation counter and the rwlock are gone.
 *
 * Lock order: s3fs_policy_evict_lock (outermost, the capacity pass) -> fd->lock.
 * s3fs_pass_lock and s3fs_flush_pins_lock etc. are leaves.
 */

/* Per-open-file state, kept in file->private_data (allocated in .open, freed
 * in .release). */
struct s3fs_file_admit_cache {
	unsigned long hit_tick; /* hit_accum: jiffies of the last counted access
				 * through this struct file; 0 = not counted yet */
};

/* Number of cached inodes the last capacity pass saw (the candidates).  Read
 * by BPF policies through the s3fs_kfunc_admit_count() KFunc for
 * pressure-aware decisions (bpf/policy_pressure_ops.bpf.c). */
atomic_t s3fs_resident_count = ATOMIC_INIT(0);

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
 * .read_iter counts one access per open file in a per-CPU
 * slot of the inode's s3fs_file_data; admit_replace Phase 1 replays the
 * delta into the policy's on_hit under the evict mutex.
 *   hit_rearm_jiffies  an open file counts again after this many jiffies
 *                      (0 = once per open)
 *   hit_replay_max     on_hit calls per ino per Phase 1 (delta is cut)
 */
static unsigned int s3fs_hit_rearm_jiffies = HZ / 10;
module_param_named(hit_rearm_jiffies, s3fs_hit_rearm_jiffies, uint, 0644);
MODULE_PARM_DESC(hit_rearm_jiffies,
	"hit_accum: jiffies before an open file counts another access (0 = once per open)");
static unsigned int s3fs_hit_replay_max = 15;
module_param_named(hit_replay_max, s3fs_hit_replay_max, uint, 0644);
MODULE_PARM_DESC(hit_replay_max,
	"hit_accum: max on_hit calls per ino per capacity pass");
atomic_long_t s3fs_hits_replayed_total = ATOMIC_LONG_INIT(0);

/*
 * Cumulative page-cache invalidations done by admit_replace for new or
 * moved ranges (S-01). With diff invalidation this grows only with real
 * content changes, not with every full-list re-send.
 */
atomic_long_t s3fs_admit_invalidations = ATOMIC_LONG_INIT(0);

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

/* A daemon has opened /dev/s3fs_ebpf (AC-43: even with nothing admitted the
 * upcall path must be armed).  Gates the read-miss upcall; cleared by
 * admit_shutdown (umount / daemon-less module state). */
static bool s3fs_daemon_armed;

/* Set in fill_super so the capacity pass and eviction can ilookup() cached
 * inodes and walk the superblock's inodes. */
static struct super_block *s3fs_active_sb;

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
static bool s3fs_file_data_reset(u64 ino);

/* Evict the cached content of `ino`: free its idle chunks and drop its page
 * cache (P3/Q1).  Returns true when something was freed; *freed gets the bytes
 * (0 here: the caller measures the fd->cap difference).  Dirty / flushing
 * inodes and chunks a reader has pinned stay (U8). */
static void s3fs_hit_forget(u64 ino);
static bool admit_evict_ino(u64 ino, u64 *freed)
{
	if (freed)
		*freed = 0;
	if (!s3fs_file_data_reset(ino))
		return false;
	s3fs_evict_page_cache(ino);
	/* #22: every whole-ino eviction ends here, so this is the one place
	 * that resets the per-ino hit state (the next populate starts afresh). */
	s3fs_hit_forget(ino);
	return true;
}
