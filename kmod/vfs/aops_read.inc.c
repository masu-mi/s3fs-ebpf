/* SPDX-License-Identifier: GPL-2.0 */
/* address_space ops: read_folio / read_iter / open / release.  Part of vfs_ops.c: included from there, one translation
 * unit, never compiled on its own. */

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

/* Fill @folio from the fd's chunk table.  Returns true when the fd held valid
 * data for pos (a hit); false when the chunk is not there (the caller then
 * upcalls) - the folio is zeroed but must not be marked uptodate.
 *
 * What counts as valid is decided under fd->lock from the chunk alone (T1 / T2):
 * a clean fd needs the chunk allocated and `populated` and pos below fd->size;
 * a dirty or flushing fd owns its chunks (a missing chunk is a hole and reads as
 * zeros) except a remote chunk, which only the daemon can supply.  There is no
 * separate "admitted" state to consult or to race with (R1 / S1 are gone).
 *
 * @count_fail: this is the last-resort fill after the upcall retries; a failure
 * is attributed to its cause in the gen0_fail_* counters (IOC_STATS 7/8/9). */
static bool s3fs_folio_fill_from_kmalloc(struct folio *folio, size_t fs_size,
					  loff_t pos, struct s3fs_file_data *fd,
					  bool count_fail)
{
	size_t avail = 0;
	bool buf_valid;

	if (!fd) {
		folio_zero_range(folio, 0, fs_size);
		return false;
	}
	mutex_lock(&fd->lock);
	{
		u32 ci = (u32)((u64)pos >> S3FS_PCACHE_CHUNK_SHIFT);
		struct s3fs_chunk *c = ci < fd->nr_chunks ? &fd->chunks[ci] : NULL;

		if (fd->dirty || fd->flushing) {
			/* T1: dirty or in-flight flush - the chunks are owned by
			 * the write/flush path; bypass the populated flags.  A
			 * missing chunk is a hole and reads as zeros, and so does
			 * a folio past fd->size (a truncate that ran after
			 * read_folio checked i_size: zeros, not -EIO). */
			buf_valid = !(c && c->remote);
		} else {
			/* T2: a chunk may be allocated but not yet fetched (its
			 * tail is zero after a reset / partial populate); the
			 * populated flag is the proof that the data is there.
			 * Every chunk has a flag, so files > 256 MiB are checked
			 * like any other. */
			buf_valid = c && c->p && c->populated &&
				    (size_t)pos < fd->size;
			if (!buf_valid && count_fail) {
				if (!c || !c->p)
					atomic_long_inc(&s3fs_gen0_fail_buf_null);
				else if ((size_t)pos >= fd->size)
					atomic_long_inc(&s3fs_gen0_fail_other);
				else
					atomic_long_inc(&s3fs_gen0_fail_bitmap);
			}
		}
	}
	if (buf_valid && (size_t)pos < fd->size) {
		u32 ci = (u32)((u64)pos >> S3FS_PCACHE_CHUNK_SHIFT);
		size_t chunk_end = min_t(size_t,
					 (size_t)(ci + 1) << S3FS_PCACHE_CHUNK_SHIFT,
					 fd->size);
		avail = min_t(size_t, fs_size, chunk_end - (size_t)pos);
	}
	/* AC-76: the folio may be a large folio; memcpy_to_folio /
	 * folio_zero_range map it page by page. */
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
			memcpy_to_folio(folio, 0, fd->chunks[ci].p + off, have);
		if (have < avail)
			folio_zero_range(folio, have, avail - have);
	}
	if (avail < fs_size)
		folio_zero_range(folio, avail, fs_size - avail);
	mutex_unlock(&fd->lock);
	return buf_valid;
}

/* S5: max upcall retries before falling back to the bootstrap buffer.
 * Guards against eviction firing in the window between signal_admit
 * (which unhashes the pending, unblocking eviction) and the waiter
 * resuming to check the fast paths.  Each retry re-creates the pending,
 * which blocks eviction of this ino while the daemon re-populates.
 * U8: buf_readers on fd extends this protection to cover the window
 * between inflight_unhash and the gen=0 fallback taking fd->lock. */
#define S3FS_UPCALL_MAX_RETRIES 3

static int s3fs_ebpf_read_folio(struct file *file, struct folio *folio)
{
	struct inode *inode = folio->mapping->host;
	struct s3fs_file_data *fd = inode->i_private;
	bool armed = false;	/* a daemon is attached */
	loff_t pos = folio_pos(folio);
	size_t fs_size = folio_size(folio);
	int upcall_retries = 0;
	bool filled = false;
	bool pinned = false;	/* U8: chunk pin held (per chunk) */
	u32 pin_ci = 0;
	int ret = -EIO;		/* returned when the folio is not filled */

	/* A folio wholly past EOF holds zeros (filemap_read clips the copy at
	 * i_size).  A truncate racing the read gets here; an upcall or the
	 * fallback would turn it into -EIO. */
	if (pos >= i_size_read(inode)) {
		folio_zero_range(folio, 0, fs_size);
		folio_mark_uptodate(folio);
		folio_unlock(folio);
		return 0;
	}

retry:
	/* The chunk table is the only thing that says whether the content is here;
	 * on a retry after a wake this is also where the reader finds the chunk it
	 * pinned (a capacity pass may have dropped everything but the pinned
	 * chunk).  file may be NULL on readahead paths. */
	if (fd && s3fs_folio_fill_from_kmalloc(folio, fs_size, pos, fd, false)) {
		atomic_long_inc(&s3fs_folio_hits);
		filled = true;
		goto done;
	}

	/* Miss + daemon alive: trigger upcall, wait, retry fast paths.
	 * S5: retry up to S3FS_UPCALL_MAX_RETRIES times so that eviction
	 * firing in the signal_admit → wakeup window does not cause zeros. */
	armed = READ_ONCE(s3fs_daemon_armed);

	/* A dirty / flushing fd owns its content: a chunk it has (or a hole)
	 * is served from the fd, the daemon has nothing to add and would only
	 * be refused (populate -EBUSY) after a timeout per folio.  The
	 * exception is a remote chunk (AC-51), which the daemon loads. */
	if (armed && fd) {
		u32 lci = (u32)((u64)pos >> S3FS_PCACHE_CHUNK_SHIFT);

		mutex_lock(&fd->lock);
		if ((fd->dirty || fd->flushing) &&
		    !(lci < fd->nr_chunks && fd->chunks[lci].remote))
			armed = false;
		mutex_unlock(&fd->lock);
	}

	if (armed && upcall_retries < S3FS_UPCALL_MAX_RETRIES) {
		u64 chunk_offset = ((u64)pos >> S3FS_PCACHE_CHUNK_SHIFT)
				   << S3FS_PCACHE_CHUNK_SHIFT;
		struct s3fs_inflight *p;

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
		{
			int err;
			long remaining;
			loff_t mpos = pos;
			size_t mcount = fs_size;
			u32 mflags = 0;
			bool created;

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
			/* A load for write is another record than a plain read of the
			 * same chunk (the daemon answers them differently). */
			p = inflight_get_or_create(S3FS_REQ_MISS, inode->i_ino, chunk_offset,
						   mflags & S3FS_MISS_FLAG_FOR_WRITE, &created);
			err = p ? inflight_queue_miss(p, created, mpos, mcount, mflags, fd)
				: -ENOMEM;
			if (err == 0) {
				/* Killable, not interruptible: a plain signal (a Go
				 * runtime sends SIGURG to preempt goroutines, every few
				 * ms) must not cut the wait short.  An interrupted wait
				 * (remaining < 0) used to fall through to the fallback
				 * fill below, which finds no chunk in a cold read and
				 * returned -EIO.  Only a fatal signal ends it now. */
				remaining = wait_for_completion_killable_timeout(
					&p->done, S3FS_UPCALL_WAIT_JIFFIES);
				err = p->err;
				if (remaining == 0)
					inflight_unhash(p);	/* nobody answered: let the retry queue anew */
				inflight_put(p);
				if (remaining < 0) {
					ret = -EINTR;	/* fatal signal: the task is going away */
					goto done;
				}
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
				inflight_put(p);
			}
		}
	}

	/* T4: count retry exhaustion (daemon alive but pcache entry evicted on
	 * every retry, or U11 timeout retries exhausted).  U8: buf_readers
	 * protects fd->buf for the entire retry window so the gen=0 fallback
	 * finds valid data if the daemon populated at any point. */
	if (armed && upcall_retries >= S3FS_UPCALL_MAX_RETRIES)
		atomic_long_inc(&s3fs_upcall_retry_exhausted);

	/* Fallback: per-inode bootstrap buffer (gen=0, no TOCTOU re-probe).
	 * Reached when the daemon is absent, timed out, or all retries are
	 * exhausted.  U8: buf_readers > 0 prevents eviction from clearing
	 * fd->buf, so this path finds valid data from the last successful
	 * populate and returns true rather than -EIO. */
	filled = s3fs_folio_fill_from_kmalloc(folio, fs_size, pos,
					      inode->i_private, true);

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
	return filled ? 0 : ret;
}

/* splice / sendfile reach the page cache without .read_iter. */
static ssize_t s3fs_ebpf_splice_read(struct file *in, loff_t *ppos,
				     struct pipe_inode_info *pipe, size_t len,
				     unsigned int flags)
{
	{
		struct kiocb kio = { .ki_pos = *ppos };

		s3fs_hit_count(&kio, len, in, file_inode(in));
	}
	return filemap_splice_read(in, ppos, pipe, len, flags);
}

/* .read_iter: count the access for the policy (hit_accum), then let the page
 * cache serve the read; a missing folio reaches .read_folio, which fills it
 * from the fd's chunk table or upcalls the daemon. */
static ssize_t s3fs_ebpf_read_iter(struct kiocb *iocb, struct iov_iter *to)
{
	struct file *file = iocb->ki_filp;

	s3fs_hit_count(iocb, iov_iter_count(to), file, file_inode(file));
	return generic_file_read_iter(iocb, to);
}

/* AC-76: read_large_folios=1 (default) lets the page cache of a regular file
 * hold large folios (up to a chunk), and gives the superblock a chunk-sized
 * readahead window, so a cold sequential read is served by s3fs_ebpf_readahead
 * in a few large folios instead of one .read_folio per 4 KiB.  Read when an
 * inode is created, so it applies to inodes created after the change. */
static bool s3fs_read_large_folios = true;
module_param_named(read_large_folios, s3fs_read_large_folios, bool, 0644);
MODULE_PARM_DESC(read_large_folios,
	"Large folios + readahead for chunk-table reads (inodes created after the change)");

/* .readahead fills a run of folios from the chunk table under one fd->lock.
 * Per chunk it applies the conditions of the gen != 0 path of
 * s3fs_folio_fill_from_kmalloc (clean fd, chunk populated, S1 re-verify when
 * the admit generation moved); a folio it cannot fill is left not-uptodate and
 * unlocked, the rest stay in rac for the core to unlock, and filemap_read then
 * calls .read_folio for them (upcall, fallback).
 *
 * readahead_folio() hands out the folios in order; readahead_count/_pos lag
 * one folio behind it, so they are not used to peek. */
static void s3fs_ebpf_readahead(struct readahead_control *rac)
{
	struct inode *inode = rac->mapping->host;
	struct s3fs_file_data *fd = inode->i_private;
	struct s3fs_chunk *c = NULL;
	loff_t isize = i_size_read(inode);
	u32 cur_ci = U32_MAX;
	size_t chunk_end = 0;
	unsigned long hits = 0;
	struct folio *folio;

	if (!fd)
		return;
	mutex_lock(&fd->lock);
	while ((folio = readahead_folio(rac))) {
		loff_t pos = folio_pos(folio);
		size_t fs_size = folio_size(folio);
		u32 ci = (u32)((u64)pos >> S3FS_PCACHE_CHUNK_SHIFT);
		u64 cstart = (u64)ci << S3FS_PCACHE_CHUNK_SHIFT;
		size_t have = 0, off, avail;

		if (pos >= isize || fd->dirty || fd->flushing ||
		    (u64)pos + fs_size > cstart + S3FS_CHUNK_SIZE)
			goto stop;
		if (ci != cur_ci) {
			cur_ci = U32_MAX;
			if (ci >= fd->nr_chunks)
				goto stop;
			c = &fd->chunks[ci];
			if (!c->p || !c->populated || (size_t)pos >= fd->size)
				goto stop;
			chunk_end = min_t(size_t,
					  (size_t)(ci + 1) << S3FS_PCACHE_CHUNK_SHIFT,
					  fd->size);
			cur_ci = ci;
		}
		if ((size_t)pos >= chunk_end)
			goto stop;
		off = (size_t)(pos - cstart);
		avail = min_t(size_t, fs_size, chunk_end - (size_t)pos);
		if (c->cap > off)
			have = min_t(size_t, avail, c->cap - off);
		if (have)
			memcpy_to_folio(folio, 0, c->p + off, have);
		if (have < fs_size)
			folio_zero_range(folio, have, fs_size - have);
		folio_mark_uptodate(folio);
		folio_unlock(folio);
		hits++;
		continue;
stop:
		folio_unlock(folio);
		break;
	}
	mutex_unlock(&fd->lock);
	atomic_long_add(hits, &s3fs_folio_hits);
}

static const struct address_space_operations s3fs_ebpf_aops = {
	.read_folio	= s3fs_ebpf_read_folio,
	.readahead	= s3fs_ebpf_readahead,
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
	struct s3fs_inflight *p = NULL;
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
	/* AC-96: a clean file whose flush is still in flight (a close() queued it and
	 * cleared dirty) is not durable yet: an fsync must wait for it, and queues its
	 * own request because the in-flight PUT may have read the buffer before the
	 * data the caller wrote (model FE-5/FE-6: a DONE that does not cover leaves
	 * nobody to re-flush).  Only a clean file with nothing in flight is a no-op. */
	if (!fd->dirty && !(wait && fd->flushing)) {
		mutex_unlock(&fd->lock);
		return 0;
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
		p = inflight_get_or_create(S3FS_REQ_FLUSH, ino, S3FS_FLUSH_KEY, 0, NULL);
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
		inflight_put(p);
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

/*
 * pcache_drop=1 — drop the page cache on every open, keep the rest.
 *
 * The second stage of nocache only: .open runs invalidate_inode_pages2()
 * but leaves the admit ranges and the chunk table alone, and does not
 * push an evict-log entry. Every read after an open therefore enters
 * .read_folio; a chunk-table hit returns without an upcall, a miss goes
 * through the daemon to S3. This is the shape of s3fs-fuse with
 * use_cache but without kernel_cache (page cache dropped per open, disk
 * cache serves the read), so the effect of the chunk table and the
 * policy can be measured on their own. mmap uses the page cache and is
 * not a target. Dirty inodes are left alone, as with nocache.
 * nocache wins when both are set. Runtime-toggleable:
 *   echo 1 > /sys/module/s3fs_kfunc/parameters/pcache_drop
 */
static bool s3fs_pcache_drop;
module_param_named(pcache_drop, s3fs_pcache_drop, bool, 0644);
MODULE_PARM_DESC(pcache_drop,
	"Drop only the page cache on every open; keep admit + chunk table (benchmark only; nocache wins)");

/*
 * open_check=1 — open(2) without O_CREAT verifies the object still exists.
 *
 * The entry table is the kmod's only knowledge of the bucket, and it is
 * refreshed by the daemon's periodic sync.  Without this, .open succeeds for
 * a key that was deleted from S3 after the last sync and the error only
 * shows later, at the first read (or never, when the page cache holds the
 * file).  s3fs-fuse asks S3 (HEAD) in its open, so a comparison with it must
 * pay the same round trip.  With open_check set, .open sends an
 * S3FS_MISS_FLAG_OPEN_CHECK upcall and fails with the daemon's error
 * (-ENOENT when the HEAD says 404).  Not done for O_CREAT (the file may not
 * exist yet), for a non-regular inode, or for an inode with an unflushed
 * write buffer (its local content is newer than S3).  Default on (AC-78): every open
 * costs one S3 HEAD (about 420 us), the price of s3fs-fuse-equivalent semantics.  Runtime-
 * toggleable:
 *   echo 1 > /sys/module/s3fs_kfunc/parameters/open_check
 */
static bool s3fs_open_check = true;
module_param_named(open_check, s3fs_open_check, bool, 0644);
MODULE_PARM_DESC(open_check,
	"open(2) without O_CREAT asks the daemon (S3 HEAD) whether the object exists; fail with its error (default on, AC-78)");

/* pending key of an open check: not a multiple of the 4 MiB chunk size, so it
 * never collides with a read's (ino, chunk_offset). */
#define S3FS_OPEN_CHECK_KEY	((u64)-1)

static int s3fs_open_verify(struct inode *inode)
{
	struct s3fs_file_data *fd = inode->i_private;
	struct s3fs_inflight *p;
	long remaining;
	bool local = false, created;
	int err;

	if (!S_ISREG(inode->i_mode))
		return 0;
	if (fd) {
		mutex_lock(&fd->lock);
		local = fd->dirty || fd->flushing;
		mutex_unlock(&fd->lock);
	}
	if (local)
		return 0;

	p = inflight_get_or_create(S3FS_REQ_MISS, inode->i_ino, S3FS_OPEN_CHECK_KEY, 0, &created);
	if (!p)
		return -ENOMEM;
	err = inflight_queue_miss(p, created, 0, 0, S3FS_MISS_FLAG_OPEN_CHECK, NULL);
	if (err) {
		inflight_put(p);
		return err;
	}
	remaining = wait_for_completion_interruptible_timeout(
		&p->done, S3FS_UPCALL_WAIT_JIFFIES);
	err = p->err;
	if (remaining == 0)
		inflight_unhash(p);	/* nobody answered: the next open queues anew */
	inflight_put(p);
	if (remaining < 0)
		return remaining;	/* -ERESTARTSYS */
	if (remaining == 0)
		return -EIO;		/* daemon absent or saturated */
	return err;
}

static int s3fs_ebpf_file_open(struct inode *inode, struct file *file)
{
	struct s3fs_file_data *fd = inode->i_private;
	bool dirty = false;
	bool drop_pcache_only = false;

	if (READ_ONCE(s3fs_open_check) && !(file->f_flags & O_CREAT)) {
		int verr = s3fs_open_verify(inode);

		if (verr)
			return verr;	/* before private_data: no release on failure */
	}

	/* Per-open state (hit_accum: when this open file last counted an access). */
	if (!file->private_data) {
		struct s3fs_file_admit_cache *fc =
			kzalloc(sizeof(*fc), GFP_KERNEL);
		if (fc)
			file->private_data = fc;
	}

	if (!READ_ONCE(s3fs_nocache)) {
		if (!READ_ONCE(s3fs_pcache_drop))
			return 0;
		drop_pcache_only = true;
	} else if (READ_ONCE(s3fs_pcache_drop)) {
		pr_info_once("s3fs_ebpf: nocache and pcache_drop are both set; nocache wins\n");
	}
	if (fd) {
		mutex_lock(&fd->lock);
		dirty = fd->dirty;
		mutex_unlock(&fd->lock);
	}
	if (dirty)
		return 0;

	if (drop_pcache_only) {
		/* Page cache only; no-op on a dirty folio, which the check
		 * above already excluded. */
		invalidate_inode_pages2(inode->i_mapping);
		return 0;
	}

	/*
	 * S-09 fix — two-part invalidation for reliable nocache behaviour:
	 *
	 * 1. Free the fd's idle chunks (s3fs_file_data_reset, a no-op for a
	 *    dirty fd), so the next read misses and the daemon fetches the
	 *    object again.
	 *
	 * 2. Use invalidate_inode_pages2 instead of invalidate_mapping_pages.
	 *    invalidate_mapping_pages uses folio_trylock and silently skips
	 *    folios that are locked or have elevated refcounts — common for
	 *    small (single-folio) files that were recently accessed.
	 *    invalidate_inode_pages2 waits for the lock and forcibly drops all
	 *    clean folios, so the next read_iter reaches .read_folio rather
	 *    than being served directly from the page cache.
	 */
	s3fs_file_data_reset(inode->i_ino);
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
	.splice_read	= s3fs_ebpf_splice_read,
	.release	= s3fs_ebpf_file_release,
	.fsync		= s3fs_ebpf_file_fsync,
};

static const struct inode_operations s3fs_ebpf_file_iops = {
	.getattr	= simple_getattr,
	.setattr	= s3fs_ebpf_file_setattr,
};

