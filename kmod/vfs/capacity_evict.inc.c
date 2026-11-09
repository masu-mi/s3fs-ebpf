/* SPDX-License-Identifier: GPL-2.0 */
/* policy tracking, capacity eviction, admit_replace, admit shutdown.  Part of vfs_ops.c: included from there, one translation
 * unit, never compiled on its own. */


/* AC-41: defined after struct s3fs_file_data. */
static u64 s3fs_ino_buf_bytes(u64 ino);

/* A capacity-pass candidate: a clean inode whose chunks the cache holds, and
 * the bytes it is charged (fd->cap).  Collected by walking the superblock's
 * inodes (s3fs_collect_resident, defined after struct s3fs_file_data). */
struct s3fs_resident {
	u64 ino;
	u64 bytes;
};
static int s3fs_collect_resident(struct s3fs_resident **out);

/* The candidates of the pass in progress, for the KFunc s3fs_kfunc_ino_size
 * (s3fs_admit_size): valid only while s3fs_policy_evict_lock is held by the
 * pass, which is the only place a policy hook runs. */
static const struct s3fs_resident *s3fs_pass_res;
static int s3fs_pass_nres;

/* Issue #22: hit accumulation; defined after struct s3fs_file_data. */
struct s3fs_file_data;
struct s3fs_hit_rec {
	u64 ino;
	u64 d;          /* accesses since the last replay (for a new ino: after its populate) */
	u64 d_pre;      /* new ino only: accesses before its populate, incl. the one that missed */
	u64 last_tick;  /* newest jiffies among the CPUs' last accesses */
	bool first;     /* policy had not seen the ino: d is replayed after on_insert */
};
enum { S3FS_HIT_BEFORE_INSERT, S3FS_HIT_AFTER_INSERT };
static bool s3fs_hit_collect(u64 ino, struct s3fs_hit_rec *rec);
static void s3fs_hit_replay(struct s3fs_hit_rec *recs, u32 n, bool after_insert,
			    void (*hit)(u64 ino));
static void s3fs_hit_forget(u64 ino);
static void s3fs_hit_signal(u64 ino);
static void s3fs_hit_set_base(struct s3fs_file_data *fd);

/* Policy bookkeeping.  The only callers of the policy's on_insert / on_size /
 * on_remove: keep the rules about WHEN here, not at each call site.
 *  - A miss in flight (pending) is not a candidate yet.  Pending keeps the
 *    kmod from evicting the ino, but a policy that picked it would already
 *    have moved it to its ghost / hand.  It is tracked next round.
 *  - Whatever leaves the policy also forgets its hit state (idempotent with
 *    admit_evict_ino, which forgets for policies without pick_victim).
 * scripts/check-kmod-invariants.sh fails if another file position calls the
 * raw ops. */
static bool s3fs_policy_trackable(u64 ino)
{
	return !pending_exists(ino);
}

static void s3fs_policy_track(u64 ino, u64 bytes, bool sized)
{
	s3fs_policy_ops_insert(ino);
	if (sized)
		s3fs_policy_ops_size(ino, bytes);
}

static void s3fs_policy_untrack(u64 ino)
{
	s3fs_policy_ops_remove(ino);
	s3fs_hit_forget(ino);
}

struct s3fs_victim {
	int score;     /* policy on_evict return value, clamped >= 0 */
	u64 ino;       /* inode number (P3: replaces entries[] idx so pcache can be included) */
	u32 tiebreak;  /* hash_32(ino,32): decorrelates from ino/Zipf distribution (Y-02) */
	bool tried;    /* AC-58: pick_victim already returned this ino */
	u32 ord;       /* position in ino order (= index into eval_inos[]) */
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
		s3fs_policy_untrack(ino);
	/* AC-41: arena bytes from the dropped entries plus what reset actually
	 * released (0 on skip_reset, so the loop keeps evicting while still
	 * over capacity). */
	buf_before -= min(s3fs_ino_buf_bytes(ino), buf_before);
	freed += buf_before;
	*live_bytes -= min(freed, *live_bytes);
}

/* The vote recorded for @ino in the ino-sorted eval arrays (0 if absent). */
static int s3fs_eval_score(const u64 *inos, const int *scores, u32 n, u64 ino)
{
	int lo = 0, hi = (int)n - 1;

	while (lo <= hi) {
		int mid = lo + (hi - lo) / 2;

		if (inos[mid] == ino)
			return scores[mid];
		if (inos[mid] < ino)
			lo = mid + 1;
		else
			hi = mid - 1;
	}
	return 0;
}

static int s3fs_resident_cmp(const void *a, const void *b)
{
	const struct s3fs_resident *x = a, *y = b;

	return x->ino < y->ino ? -1 : (x->ino > y->ino);
}

/* The size the cache holds for @ino (its fd->cap), or 0 when it is not a
 * candidate.  Exposed to BPF policies as the s3fs_kfunc_ino_size() KFunc
 * (kmod/tree_traversal.c); only meaningful inside a capacity pass, which is
 * where every policy hook runs - elsewhere it answers 0. */
u64 s3fs_admit_size(u64 ino)
{
	const struct s3fs_resident *r = READ_ONCE(s3fs_pass_res);
	int lo = 0, hi = READ_ONCE(s3fs_pass_nres) - 1;

	while (r && lo <= hi) {
		int mid = lo + (hi - lo) / 2;

		if (r[mid].ino == ino)
			return r[mid].bytes;
		if (r[mid].ino < ino)
			lo = mid + 1;
		else
			hi = mid - 1;
	}
	return 0;
}

/*
 * The capacity / policy pass (formerly admit_replace, which also swapped the
 * daemon's arena admit list; arena ranges are gone).  Collects the cached
 * inodes (clean, with allocated chunks) and, for each: replays the accumulated hits, tells the
 * policy (on_insert / on_size), takes the votes, and evicts down to
 * capacity_bytes (pick_victim or score order).  Called by the daemon's EVAL
 * ioctl (after a populate, before it wakes the reader; on /policy/eval).
 * Takes s3fs_policy_evict_lock; no other lock is held on entry.
 */
int s3fs_ebpf_capacity_pass(void)
{
	/* The candidates for capacity accounting and eviction. */
	struct s3fs_resident *res = NULL;
	int nres;
	int pcache_remaining;	/* unique inodes */

	s3fs_pc_retry_run();
	nres = s3fs_collect_resident(&res);
	pcache_remaining = nres;

	/* By ino: a deterministic order for the policy, and s3fs_admit_size()
	 * can binary-search it. */
	if (nres > 1)
		sort(res, nres, sizeof(*res), s3fs_resident_cmp, NULL);
	atomic_set(&s3fs_resident_count, nres);

	/* Policy hook (struct_ops only): the admit set changed. */
	(void)s3fs_policy_ops_eval_admit(0, 0);

	{
		u32 structops_votes = 0, evicted = 0;
		const u32 softcap = READ_ONCE(s3fs_softcap);
		const u64 cap_bytes = READ_ONCE(s3fs_capacity_bytes);
		/* P3: total candidate entries = arena + pcache */
		u32 total_count = (u32)pcache_remaining;
		struct s3fs_victim *cand = NULL;
		u64 live_bytes = 0;
		u32 ncand = 0;
		/* X-01: call on_evict exactly once per unique ino. */
		u64 *eval_inos = NULL;
		int *eval_scores = NULL;
		u32 eval_count = 0;
		bool pv = s3fs_policy_has_pick_victim(); /* AC-58 */
		struct s3fs_hit_rec *hrec = NULL;
		u32 nhrec = 0;
		int pi;

		if (cap_bytes) {
			/* P3/AC-41: charge each inode by its fd->buf allocation. */
			for (pi = 0; pi < pcache_remaining; pi++)
				live_bytes += res[pi].bytes;
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
		hrec = kcalloc(total_count, sizeof(*hrec), GFP_KERNEL);
		s3fs_policy_evict_lock();
		s3fs_pass_res = res;
		s3fs_pass_nres = nres;

		/* Issue #22: hits on inos the policy already knows are older
		 * than anything this round inserts, so they go in first; hits
		 * on new inos follow their on_insert (after the loops). */
		if (hrec) {
			for (pi = 0; pi < pcache_remaining; pi++)
				if (s3fs_policy_trackable(res[pi].ino) &&
				    s3fs_hit_collect(res[pi].ino, &hrec[nhrec]))
					nhrec++;
			s3fs_hit_replay(hrec, nhrec, S3FS_HIT_BEFORE_INSERT, s3fs_hit_signal);
		}

		/* Phase 1: collect unique inos and call on_evict once each. */
		for (pi = 0; pi < pcache_remaining; pi++) {
			u64 ino = res[pi].ino;

			/* res[] is sorted by ino: a repeat is adjacent. */
			if (pi > 0 && res[pi - 1].ino == ino)
				continue;

			{
				/* Issue #22: with hit_accum the votes wait for
				 * the replay below. */
				int so_vote = hrec ? 0 : s3fs_policy_ops_eval_evict(ino);
				int score   = max(so_vote, 0);

				eval_inos[eval_count]   = ino;
				eval_scores[eval_count] = score;
				eval_count++;
				/* AC-66: charged by fd->cap, as live_bytes. */
				if (pv && s3fs_policy_trackable(ino))
					s3fs_policy_track(ino,
						cap_bytes ? res[pi].bytes : 0,
						cap_bytes);

				if (so_vote != 0)
					structops_votes++;
			}
		}

		/* Issue #22: replay the accumulated hits, then take the votes,
		 * so every decision below sees them. */
		if (hrec) {
			u32 e;

			s3fs_hit_replay(hrec, nhrec, S3FS_HIT_AFTER_INSERT, s3fs_hit_signal);
			for (e = 0; e < eval_count; e++) {
				int so_vote = s3fs_policy_ops_eval_evict(eval_inos[e]);

				eval_scores[e] = max(so_vote, 0);
				if (so_vote != 0)
					structops_votes++;
			}
			kfree(hrec);
		}

		/* Phase 2: build cand[] / drive legacy eviction.  In capacity
		 * mode each ino appears at most once. */
		for (pi = 0; pi < pcache_remaining; pi++) {
			u64 ino = res[pi].ino;
			int score = s3fs_eval_score(eval_inos, eval_scores,
						    eval_count, ino);

			if (cap_bytes) {
				if (cand) {
					/* cand[] is filled in res[] (ino)
					 * order: a repeat is the last one. */
					if (!ncand || cand[ncand - 1].ino != ino) {
						cand[ncand].score    = score;
						cand[ncand].ino      = ino;
						cand[ncand].ord      = ncand;
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

		kfree(eval_scores);

		if (cand) {
			u32 k;
			/* pos[ord] = where cand[] (score order) holds the ino at
			 * eval_inos[ord], so pick_victim's answer is found by a
			 * binary search + one lookup, not a scan. */
			u32 *pos = pv ? kcalloc(ncand, sizeof(*pos), GFP_KERNEL) : NULL;

			sort(cand, ncand, sizeof(*cand), s3fs_victim_cmp, NULL);
			if (pos)
				for (k = 0; k < ncand; k++)
					pos[cand[k].ord] = k;
			/* AC-58: the policy's own order first.  Anything it
			 * cannot name (0, unknown ino, one already tried) hands
			 * the rest to the score order below. */
			for (k = 0; pos && k < ncand && live_bytes > cap_bytes; k++) {
				u64 v = s3fs_policy_ops_pick_victim();
				u32 j;

				if (!v)
					break;
				j = ncand;
				if (pos) {
					int lo = 0, hi = (int)ncand - 1;

					while (lo <= hi) {
						int mid = lo + (hi - lo) / 2;

						if (eval_inos[mid] == v) {
							j = pos[mid];
							break;
						}
						if (eval_inos[mid] < v)
							lo = mid + 1;
						else
							hi = mid - 1;
					}
				}
				if (j == ncand) {
					/* Not a candidate any more (evicted by
					 * another route): let the policy forget
					 * it and ask again. */
					s3fs_policy_untrack(v);
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
			kfree(pos);
			kfree(cand);
			if (live_bytes > cap_bytes)
				pr_info_ratelimited("s3fs_ebpf: admit set %llu B still over capacity %llu B (in-flight misses)\n",
						    live_bytes, cap_bytes);
		}
		kfree(eval_inos);
		s3fs_pass_res = NULL;
		s3fs_pass_nres = 0;
		s3fs_policy_evict_unlock();
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
	kfree(res);
	return 0;
}

static void s3fs_ebpf_upcall_shutdown(void); /* forward */

void s3fs_ebpf_daemon_arm(void)
{
	WRITE_ONCE(s3fs_daemon_armed, true);
}

void s3fs_ebpf_admit_shutdown(void)
{
	WRITE_ONCE(s3fs_daemon_armed, false);

	/* Same lifecycle: daemon closed / module unloading. Fail any
	 * blocked readers and drop queued events so nothing survives
	 * into the next daemon incarnation. */
	s3fs_ebpf_upcall_shutdown();
}

/*
 * The pass that runs at the end of a successful POPULATE_PAGES (AC-45 C2; before
 * v2 Phase 3e the daemon asked for it with an extra EVAL ioctl after the last
 * populate of a miss).  Only in capacity mode (capacity_bytes > 0): without it the
 * pass is the policy's own business (EVAL / sync).  The chunk just populated is
 * either pinned by the reader that waits for it (pending is created after the pin)
 * or has no waiter; the daemon signals the miss only after this returns, so the
 * order populate -> pass -> signal is unchanged.
 *
 * Coalesced: one pass runs at a time, and callers that arrive while it runs just
 * ask for one more round (their chunk was populated before that round starts to
 * collect, or the running pass sees it), instead of queueing on
 * s3fs_policy_evict_lock.  No lock is held on entry.
 */
static atomic_t s3fs_pass_running = ATOMIC_INIT(0);
static atomic_t s3fs_pass_again = ATOMIC_INIT(0);

static void s3fs_ebpf_capacity_pass_after_populate(void)
{
	if (!READ_ONCE(s3fs_capacity_bytes))
		return;
	if (atomic_xchg(&s3fs_pass_running, 1)) {
		atomic_set(&s3fs_pass_again, 1);
		return;
	}
	for (;;) {
		atomic_set(&s3fs_pass_again, 0);
		(void)s3fs_ebpf_capacity_pass();
		if (!atomic_read(&s3fs_pass_again)) {
			atomic_set(&s3fs_pass_running, 0);
			/* a caller may have asked between the read and the clear */
			if (!atomic_read(&s3fs_pass_again) ||
			    atomic_xchg(&s3fs_pass_running, 1))
				return;
		}
	}
}
