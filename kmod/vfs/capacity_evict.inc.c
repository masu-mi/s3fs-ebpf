/* SPDX-License-Identifier: GPL-2.0 */
/* policy tracking, capacity eviction, admit_replace, admit shutdown.  Part of vfs_ops.c: included from there, one translation
 * unit, never compiled on its own. */


/* AC-41: defined after struct s3fs_file_data. */
static u64 s3fs_ino_buf_bytes(u64 ino);

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

/* Bytes the arena entries from index @from on charge to @ino. */
static u64 s3fs_arena_ino_bytes(const struct s3fs_admit_entry *entries,
				u32 count, u32 from, u64 ino)
{
	u64 b = 0;

	for (; from < count; from++)
		if (entries[from].ino == ino)
			b += entries[from].size;
	return b;
}

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
		s3fs_policy_untrack(ino);
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
		/* Issue #22: replay accumulated hits for every ino. */
		bool ha = READ_ONCE(s3fs_hit_accum);
		struct s3fs_hit_rec *hrec = NULL;
		u32 nhrec = 0;
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
		if (ha)
			hrec = kcalloc(total_count, sizeof(*hrec), GFP_KERNEL);
		if (pv || ha)
			s3fs_policy_evict_lock();

		/* Issue #22: hits on inos the policy already knows are older
		 * than anything this round inserts, so they go in first; hits
		 * on new inos follow their on_insert (after the loops). */
		if (hrec) {
			for (i = 0; i < count; i++)
				if (s3fs_policy_trackable(entries[i].ino) &&
				    s3fs_hit_collect(entries[i].ino, &hrec[nhrec]))
					nhrec++;
			for (pi = 0; pi < pcache_remaining; pi++)
				if (s3fs_policy_trackable(pcache_inos[pi]) &&
				    s3fs_hit_collect(pcache_inos[pi], &hrec[nhrec]))
					nhrec++;
			s3fs_hit_replay(hrec, nhrec, S3FS_HIT_BEFORE_INSERT, s3fs_hit_signal);
		}

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
				/* Issue #22: with hit_accum the votes wait for
				 * the replay below. */
				int fr_vote = hrec ? 0 : s3fs_policy_on_evict(ino);
				int so_vote = hrec ? 0 : s3fs_policy_ops_eval_evict(ino);
				int score   = max3(fr_vote, so_vote, 0);

				eval_inos[eval_count]   = ino;
				eval_scores[eval_count] = score;
				eval_count++;
				/* AC-66: the bytes this ino adds to live_bytes
				 * (all of its ranges). */
				if (pv && s3fs_policy_trackable(ino))
					s3fs_policy_track(ino,
						cap_bytes ? s3fs_arena_ino_bytes(entries, count, i, ino) : 0,
						cap_bytes);

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
				/* Issue #22: with hit_accum the votes wait for
				 * the replay below. */
				int fr_vote = hrec ? 0 : s3fs_policy_on_evict(ino);
				int so_vote = hrec ? 0 : s3fs_policy_ops_eval_evict(ino);
				int score   = max3(fr_vote, so_vote, 0);

				eval_inos[eval_count]   = ino;
				eval_scores[eval_count] = score;
				eval_count++;
				/* AC-66: charged by fd->cap, as live_bytes. */
				if (pv && s3fs_policy_trackable(ino))
					s3fs_policy_track(ino,
						cap_bytes ? s3fs_ino_buf_bytes(ino) : 0,
						cap_bytes);

				if (fr_vote != 0)
					fmodret_votes++;
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
				int fr_vote = s3fs_policy_on_evict(eval_inos[e]);
				int so_vote = s3fs_policy_ops_eval_evict(eval_inos[e]);

				eval_scores[e] = max3(fr_vote, so_vote, 0);
				if (fr_vote != 0)
					fmodret_votes++;
				if (so_vote != 0)
					structops_votes++;
			}
			kfree(hrec);
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
			kfree(cand);
			if (live_bytes > cap_bytes)
				pr_info_ratelimited("s3fs_ebpf: admit set %llu B still over capacity %llu B (in-flight misses)\n",
						    live_bytes, cap_bytes);
		}
		if (pv || ha)
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

