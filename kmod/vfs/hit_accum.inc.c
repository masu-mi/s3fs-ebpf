/* SPDX-License-Identifier: GPL-2.0 */
/* issue #22 per-CPU hit accumulation.  Part of vfs_ops.c: included from there, one translation
 * unit, never compiled on its own. */

/* -- Issue #22: hit accumulation ------------------------------------------ */

/* .read_iter: count one access for this open file.  The only shared-looking
 * state touched is the struct file's own hit_tick and a per-CPU slot. */
static void s3fs_hit_count(struct file *file, struct inode *inode)
{
	struct s3fs_file_admit_cache *fc = file->private_data;
	struct s3fs_file_data *fd = inode->i_private;
	struct s3fs_hit_acc *acc;
	unsigned long now = jiffies ?: 1, last;
	unsigned int rearm;

	if (!fc || !fd || !fd->hit)
		return;
	last = READ_ONCE(fc->hit_tick);
	rearm = READ_ONCE(s3fs_hit_rearm_jiffies);
	if (last && (!rearm || now - last < rearm))
		return;
	WRITE_ONCE(fc->hit_tick, now);
	preempt_disable();
	acc = this_cpu_ptr(fd->hit);
	acc->count++;
	acc->last_tick = now;
	preempt_enable();
}

static void s3fs_hit_signal(u64 ino)
{
	s3fs_policy_on_hit(ino);
	(void)s3fs_policy_ops_eval_hit(ino);
}

/* The direct (pre-#22) on_hit signal. */
static void s3fs_hit_signal_direct(u64 ino)
{
	if (READ_ONCE(s3fs_hit_accum))
		return;
	s3fs_hit_signal(ino);
}

/* fd-level core of s3fs_hit_collect.  Caller holds s3fs_policy_evict_lock
 * (hit_flushed / policy_seen).  Returns true when rec has accesses to replay.
 *
 * For an ino the policy knows: d = accesses since the last replay.
 * For one it does not (first look; never seen, or evicted and forgotten):
 *   d_pre = accesses up to its populate, including the read that missed;
 *           replayed BEFORE on_insert.  Frequency policies (the W-TinyLFU
 *           sketch) must see requests for inos that are not resident: that is
 *           what the admission duel compares.  The others ignore on_hit for
 *           an unknown ino.
 *   d     = accesses after the populate: hits, replayed after on_insert.
 * hit_flushed survives a forget, so accesses made while the ino was out of
 * the cache are counted at its next admission. */
static bool s3fs_hit_collect_fd(struct s3fs_file_data *fd, u64 ino,
				struct s3fs_hit_rec *rec)
{
	u64 sum = 0, tick = 0, base;
	int cpu;

	if (!fd || !fd->hit)
		return false;
	for_each_possible_cpu(cpu) {
		struct s3fs_hit_acc *acc = per_cpu_ptr(fd->hit, cpu);

		sum += READ_ONCE(acc->count);
		tick = max(tick, READ_ONCE(acc->last_tick));
	}
	rec->first = !fd->policy_seen;
	rec->d_pre = 0;
	if (rec->first) {
		/* No populate base (arena mode): everything so far is "before". */
		base = READ_ONCE(fd->hit_base_set) ? READ_ONCE(fd->hit_base) : sum;
		base = clamp(base, fd->hit_flushed, sum);
		rec->d_pre = base - fd->hit_flushed;
		fd->hit_flushed = base;
		fd->hit_base_set = false;
		fd->policy_seen = true;
	}
	if (sum <= fd->hit_flushed && !rec->d_pre)
		return false;
	rec->ino = ino;
	rec->d = sum - fd->hit_flushed;
	rec->last_tick = tick;
	fd->hit_flushed = sum;
	return true;
}

static bool s3fs_hit_collect(u64 ino, struct s3fs_hit_rec *rec)
{
	struct inode *inode;
	bool ret;

	if (!s3fs_active_sb)
		return false;
	inode = ilookup(s3fs_active_sb, (unsigned long)ino);
	if (!inode)
		return false;
	ret = s3fs_hit_collect_fd(inode->i_private, ino, rec);
	iput(inode);
	return ret;
}

/* populate (fd->lock held): remember the count so far as the point from which
 * the policy's hits are counted.  Only before the policy has seen the ino. */
static void s3fs_hit_set_base(struct s3fs_file_data *fd)
{
	u64 sum = 0;
	int cpu;

	if (!fd->hit || READ_ONCE(fd->policy_seen))
		return;
	for_each_possible_cpu(cpu)
		sum += READ_ONCE(per_cpu_ptr(fd->hit, cpu)->count);
	WRITE_ONCE(fd->hit_base, sum);
	WRITE_ONCE(fd->hit_base_set, true);
}

/* After on_remove / a whole-ino evict: the next collect is a first look. */
static void s3fs_hit_forget(u64 ino)
{
	struct inode *inode;
	struct s3fs_file_data *fd;

	if (!s3fs_active_sb)
		return;
	inode = ilookup(s3fs_active_sb, (unsigned long)ino);
	if (!inode)
		return;
	fd = inode->i_private;
	if (fd)
		fd->policy_seen = false;
	iput(inode);
}

static int s3fs_hit_rec_cmp(const void *a, const void *b)
{
	const struct s3fs_hit_rec *x = a, *y = b;

	if (x->last_tick != y->last_tick)
		return x->last_tick < y->last_tick ? -1 : 1;
	if (x->ino != y->ino)
		return x->ino < y->ino ? -1 : 1;
	return 0;
}

/* Replay into the policy, oldest last_tick first (ties by ino), at most
 * hit_replay_max on_hit calls per ino and pass.  Before on_insert
 * (@after_insert false): d for a known ino, d_pre for a new one.  After it: d
 * for the new inos.  Caller holds s3fs_policy_evict_lock. */
static void s3fs_hit_replay(struct s3fs_hit_rec *recs, u32 n, bool after_insert,
			    void (*hit)(u64 ino))
{
	const u64 max_calls = READ_ONCE(s3fs_hit_replay_max);
	u32 i;

	sort(recs, n, sizeof(*recs), s3fs_hit_rec_cmp, NULL);
	for (i = 0; i < n; i++) {
		u64 k, d, calls;

		if (after_insert && !recs[i].first)
			continue;
		d = (!after_insert && recs[i].first) ? recs[i].d_pre : recs[i].d;
		if (!d)
			continue;
		calls = min(d, max_calls);
		atomic_long_add((long)d, &s3fs_hits_replayed_total);
		for (k = 0; k < calls; k++)
			hit(recs[i].ino);
	}
}

static int s3fs_u64_cmp(const void *a, const void *b)
{
	u64 x = *(const u64 *)a, y = *(const u64 *)b;

	return x < y ? -1 : x > y;
}
