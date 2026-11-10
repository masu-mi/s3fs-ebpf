// SPDX-License-Identifier: GPL-2.0
/*
 * KUnit suites for vfs_ops.c.  #included at the end of vfs_ops.c when the
 * module is built with KUNIT=1 (-DS3FS_KUNIT), so the suites reach the
 * static helpers directly.  Run them with scripts/kunit.sh (UML) or by
 * loading a KUNIT=1 build into a kernel with CONFIG_KUNIT.
 *
 * The data-structure suites touch the module's global tables (admit hash,
 * pending hash, queues).  They use inos no mount produces (S3K_INO_BASE
 * and up) and remove what they add, but they are not meant to run next to
 * a live mount and daemon.
 *
 * The s3fs_ebpf_vfs suite mounts the filesystem in-kernel (kern_mount) and
 * drives it through the VFS: no daemon, so it covers the bootstrap entries,
 * populate / signal ioctl entry points called directly, and the flush queue.
 */

#include <kunit/test.h>
#include <linux/delay.h>
#include <linux/file.h>
#include <linux/kthread.h>
#include <linux/mman.h>
#include <linux/mount.h>
#include <linux/random.h>
#include <linux/sched/mm.h>

#define S3K_INO_BASE	0x5300000000ULL
#define S3K_MiB		(1ULL << 20)

/* -- populate: which chunks a populate makes valid (T5) ------------------- */

static void s3k_populate_fills_chunk(struct kunit *test)
{
	const u64 C = S3FS_CHUNK_SIZE;

	/* whole chunk */
	KUNIT_EXPECT_TRUE(test, s3fs_populate_fills_chunk(0, C, 3 * C, 0));
	KUNIT_EXPECT_TRUE(test, s3fs_populate_fills_chunk(0, 3 * C, 3 * C, 1));
	/* front of a chunk that is not the file's last */
	KUNIT_EXPECT_FALSE(test, s3fs_populate_fills_chunk(C, C + 4096, 3 * C, 1));
	/* last chunk, from its start to eof */
	KUNIT_EXPECT_TRUE(test, s3fs_populate_fills_chunk(2 * C, 2 * C + 46,
							  2 * C + 46, 2));
	/* a small file: one short chunk */
	KUNIT_EXPECT_TRUE(test, s3fs_populate_fills_chunk(0, 46, 46, 0));
	/* last chunk reached at eof but the range starts inside it: the bytes
	 * in front of offset were never copied */
	KUNIT_EXPECT_FALSE(test, s3fs_populate_fills_chunk(C + S3K_MiB,
							   C + 2 * S3K_MiB,
							   C + 2 * S3K_MiB, 1));
	/* an earlier chunk fully inside a range that starts mid-chunk */
	KUNIT_EXPECT_TRUE(test, s3fs_populate_fills_chunk(S3K_MiB, 3 * C, 3 * C, 1));
	KUNIT_EXPECT_FALSE(test, s3fs_populate_fills_chunk(S3K_MiB, 3 * C, 3 * C, 0));
}

/* -- chunk table (AC-45, M1) ---------------------------------------------- */

static struct s3fs_file_data *s3k_fd_new(struct kunit *test)
{
	struct s3fs_file_data *fd = s3fs_file_data_new(S3K_INO_BASE, NULL, 0);

	KUNIT_ASSERT_NOT_NULL(test, fd);
	return fd;
}

static void s3k_fd_free(struct s3fs_file_data *fd)
{
	s3fs_fd_free_all(fd);
	kfree(fd);
}

/* fd->cap == sum of chunks[].cap, and allocated bytes past fd->size are 0. */
static void s3k_fd_check(struct kunit *test, struct s3fs_file_data *fd)
{
	size_t sum = 0;
	u32 ci, i;

	for (ci = 0; ci < fd->nr_chunks; ci++) {
		struct s3fs_chunk *c = &fd->chunks[ci];
		u64 cs = (u64)ci << S3FS_PCACHE_CHUNK_SHIFT;

		if (!c->p) {
			KUNIT_EXPECT_EQ(test, c->cap, 0u);
			continue;
		}
		KUNIT_EXPECT_LE(test, (u64)c->cap, (u64)S3FS_CHUNK_SIZE);
		sum += c->cap;
		for (i = 0; i < c->cap; i++)
			if (cs + i >= fd->size && c->p[i]) {
				KUNIT_FAIL(test, "chunk %u byte %u (file %llu) = %#x past size %zu",
					   ci, i, cs + i, c->p[i], fd->size);
				break;
			}
	}
	KUNIT_EXPECT_EQ(test, fd->cap, sum);
}

static void s3k_chunk_populate_sizes(struct kunit *test)
{
	struct s3fs_file_data *fd = s3k_fd_new(test);
	long before = atomic_long_read(&s3fs_fd_buf_bytes);
	char data[46];

	memset(data, 'o', sizeof(data));
	/* AC-41: a 46 B object costs 46 B */
	KUNIT_ASSERT_EQ(test, s3fs_fd_store(fd, 0, data, NULL, 46, 46), 0);
	fd->size = 46;
	KUNIT_EXPECT_EQ(test, fd->chunks[0].cap, 46u);
	KUNIT_EXPECT_EQ(test, fd->cap, (size_t)46);
	KUNIT_EXPECT_EQ(test, atomic_long_read(&s3fs_fd_buf_bytes) - before, 46L);
	KUNIT_EXPECT_EQ(test, memcmp(fd->chunks[0].p, data, 46), 0);
	s3k_fd_check(test, fd);
	s3k_fd_free(fd);
	KUNIT_EXPECT_EQ(test, atomic_long_read(&s3fs_fd_buf_bytes), before);
}

static void s3k_chunk_tail_populate(struct kunit *test)
{
	struct s3fs_file_data *fd = s3k_fd_new(test);
	const u64 isz = 9 * S3K_MiB;	/* chunks 0, 1 full, 2 = 1 MiB */
	u8 *buf = kunit_kzalloc(test, S3K_MiB, GFP_KERNEL);

	KUNIT_ASSERT_NOT_NULL(test, buf);
	memset(buf, 0xab, S3K_MiB);
	/* AC-45: a tail read allocates only its chunk, sized to the file */
	KUNIT_ASSERT_EQ(test, s3fs_fd_store(fd, 8 * S3K_MiB, buf, NULL, S3K_MiB,
					    isz), 0);
	fd->size = isz;
	KUNIT_ASSERT_GE(test, fd->nr_chunks, 3u);
	KUNIT_EXPECT_NULL(test, fd->chunks[0].p);
	KUNIT_EXPECT_NULL(test, fd->chunks[1].p);
	KUNIT_EXPECT_EQ(test, fd->chunks[2].cap, (u32)S3K_MiB);
	KUNIT_EXPECT_EQ(test, fd->cap, (size_t)S3K_MiB);
	s3k_fd_check(test, fd);
	s3k_fd_free(fd);
}

static void s3k_chunk_write_growth(struct kunit *test)
{
	struct s3fs_file_data *fd = s3k_fd_new(test);
	u8 *buf = kunit_kzalloc(test, 8192, GFP_KERNEL);
	u64 off;

	KUNIT_ASSERT_NOT_NULL(test, buf);
	memset(buf, 'w', 8192);
	/* write path (alloc_limit 0): at least 4 KiB, then doubling */
	KUNIT_ASSERT_EQ(test, s3fs_fd_store(fd, 0, buf, NULL, 10, 0), 0);
	fd->size = 10;
	KUNIT_EXPECT_EQ(test, fd->chunks[0].cap, 4096u);
	s3k_fd_check(test, fd);
	KUNIT_ASSERT_EQ(test, s3fs_fd_store(fd, 10, buf, NULL, 4096, 0), 0);
	fd->size = 4106;
	KUNIT_EXPECT_EQ(test, fd->chunks[0].cap, 8192u);
	s3k_fd_check(test, fd);
	/* sequential appends up to one chunk never exceed it */
	for (off = 4106; off + 8192 <= S3FS_CHUNK_SIZE; off += 8192) {
		KUNIT_ASSERT_EQ(test, s3fs_fd_store(fd, off, buf, NULL, 8192, 0), 0);
		fd->size = off + 8192;
	}
	KUNIT_EXPECT_EQ(test, fd->chunks[0].cap, (u32)S3FS_CHUNK_SIZE);
	s3k_fd_check(test, fd);
	/* a write across the chunk boundary starts chunk 1 small */
	KUNIT_ASSERT_EQ(test, s3fs_fd_store(fd, S3FS_CHUNK_SIZE - 100, buf, NULL,
					    200, 0), 0);
	fd->size = S3FS_CHUNK_SIZE + 100;
	KUNIT_EXPECT_EQ(test, fd->chunks[1].cap, 4096u);
	KUNIT_EXPECT_EQ(test, fd->chunks[1].p[99], (u8)'w');
	s3k_fd_check(test, fd);
	s3k_fd_free(fd);
}

static void s3k_chunk_sparse_write_zeroes_gap(struct kunit *test)
{
	struct s3fs_file_data *fd = s3k_fd_new(test);
	static const char a[] = "AAAA", b[] = "BBBB";
	u32 i;

	KUNIT_ASSERT_EQ(test, s3fs_fd_store(fd, 0, a, NULL, 4, 0), 0);
	fd->size = 4;
	/* the grown chunk keeps [0,4), zeroes the gap, then holds b */
	KUNIT_ASSERT_EQ(test, s3fs_fd_store(fd, 10000, b, NULL, 4, 0), 0);
	fd->size = 10004;
	KUNIT_EXPECT_EQ(test, memcmp(fd->chunks[0].p, a, 4), 0);
	for (i = 4; i < 10000; i++)
		if (fd->chunks[0].p[i]) {
			KUNIT_FAIL(test, "gap byte %u = %#x", i, fd->chunks[0].p[i]);
			break;
		}
	KUNIT_EXPECT_EQ(test, memcmp(fd->chunks[0].p + 10000, b, 4), 0);
	s3k_fd_check(test, fd);
	s3k_fd_free(fd);
}

static void s3k_chunk_truncate(struct kunit *test)
{
	struct s3fs_file_data *fd = s3k_fd_new(test);
	long before = atomic_long_read(&s3fs_fd_buf_bytes);
	u8 *buf = kunit_kzalloc(test, S3K_MiB, GFP_KERNEL);
	u64 off;

	KUNIT_ASSERT_NOT_NULL(test, buf);
	memset(buf, 0x5a, S3K_MiB);
	for (off = 0; off < 10 * S3K_MiB; off += S3K_MiB)
		KUNIT_ASSERT_EQ(test, s3fs_fd_store(fd, off, buf, NULL, S3K_MiB,
						    10 * S3K_MiB), 0);
	fd->size = 10 * S3K_MiB;
	s3k_fd_check(test, fd);

	/* cut inside chunk 1: chunk 2 freed, the rest of chunk 1 zeroed */
	s3fs_fd_truncate(fd, 5 * S3K_MiB + 3);
	fd->size = 5 * S3K_MiB + 3;
	KUNIT_EXPECT_NOT_NULL(test, fd->chunks[1].p);
	KUNIT_EXPECT_NULL(test, fd->chunks[2].p);
	KUNIT_EXPECT_EQ(test, fd->chunks[1].p[S3K_MiB + 2], (u8)0x5a);
	s3k_fd_check(test, fd);

	/* cut on a chunk boundary: chunk 1 goes too */
	s3fs_fd_truncate(fd, S3FS_CHUNK_SIZE);
	fd->size = S3FS_CHUNK_SIZE;
	KUNIT_EXPECT_NULL(test, fd->chunks[1].p);
	KUNIT_EXPECT_EQ(test, fd->cap, (size_t)S3FS_CHUNK_SIZE);
	s3k_fd_check(test, fd);

	s3fs_fd_truncate(fd, 0);
	fd->size = 0;
	KUNIT_EXPECT_EQ(test, fd->cap, (size_t)0);
	KUNIT_EXPECT_EQ(test, atomic_long_read(&s3fs_fd_buf_bytes), before);
	s3k_fd_free(fd);
}

static void s3k_chunk_limits_and_pins(struct kunit *test)
{
	struct s3fs_file_data *fd = s3k_fd_new(test);
	static const char x = 'x';

	/* past the 1 TiB table */
	KUNIT_EXPECT_EQ(test, s3fs_fd_store(fd, (u64)S3FS_MAX_CHUNKS <<
					    S3FS_PCACHE_CHUNK_SHIFT, &x, NULL,
					    1, 0), -EFBIG);
	KUNIT_EXPECT_FALSE(test, s3fs_fd_chunk_pin(fd, S3FS_MAX_CHUNKS));

	/* U8: a pin survives the chunk being freed, and unpin stays balanced */
	KUNIT_ASSERT_TRUE(test, s3fs_fd_chunk_pin(fd, 3));
	KUNIT_ASSERT_EQ(test, s3fs_fd_store(fd, 3 * S3FS_CHUNK_SIZE, &x, NULL, 1,
					    0), 0);
	s3fs_fd_chunk_free(fd, 3);
	KUNIT_EXPECT_NULL(test, fd->chunks[3].p);
	KUNIT_EXPECT_EQ(test, fd->chunks[3].readers, 1u);
	s3fs_fd_chunk_unpin(fd, 3);
	s3fs_fd_chunk_unpin(fd, 3);	/* extra unpin does not underflow */
	s3fs_fd_chunk_unpin(fd, 1000);	/* nor past the table */
	KUNIT_EXPECT_EQ(test, fd->chunks[3].readers, 0u);

	/* a free clears remote and keeps nr_remote in step */
	fd->chunks[2].remote = true;
	fd->nr_remote = 1;
	KUNIT_ASSERT_EQ(test, s3fs_fd_store(fd, 2 * S3FS_CHUNK_SIZE, &x, NULL, 1,
					    0), 0);
	KUNIT_EXPECT_FALSE(test, fd->chunks[2].remote);
	KUNIT_EXPECT_EQ(test, fd->nr_remote, 0u);
	s3k_fd_free(fd);
}

static void s3k_chunk_initial_content(struct kunit *test)
{
	static const char init[] = "hello, s3fs-ebpf\n";
	struct s3fs_file_data *fd = s3fs_file_data_new(S3K_INO_BASE, init, sizeof(init) - 1);

	KUNIT_ASSERT_NOT_NULL(test, fd);
	KUNIT_EXPECT_EQ(test, fd->size, sizeof(init) - 1);
	KUNIT_EXPECT_TRUE(test, fd->chunks[0].populated);
	KUNIT_EXPECT_EQ(test, memcmp(fd->chunks[0].p, init, sizeof(init) - 1), 0);
	s3k_fd_check(test, fd);
	s3k_fd_free(fd);
}

/* -- modify ops (AC-50 / AC-51) ------------------------------------------- */

static struct inode *s3k_inode(struct kunit *test, u64 size)
{
	struct inode *inode = kunit_kzalloc(test, sizeof(*inode), GFP_KERNEL);

	KUNIT_ASSERT_NOT_NULL(test, inode);
	inode->i_size = size;
	return inode;
}

static void s3k_mop_range(struct kunit *test)
{
	const u64 C = S3FS_CHUNK_SIZE;
	struct s3fs_mop op = { .end = 0 };
	u32 first = 99, last = 99;

	KUNIT_EXPECT_FALSE(test, s3fs_mop_range(&op, &first, &last));
	op.end = 10 * C + 1;	/* 11 chunks */
	KUNIT_ASSERT_TRUE(test, s3fs_mop_range(&op, &first, &last));
	KUNIT_EXPECT_EQ(test, first, 0u);
	KUNIT_EXPECT_EQ(test, last, 10u);

	op.partial = true;
	op.t_lo = 3 * C + 5;
	op.t_hi = 5 * C;	/* exclusive: chunk 5 not touched */
	KUNIT_ASSERT_TRUE(test, s3fs_mop_range(&op, &first, &last));
	KUNIT_EXPECT_EQ(test, first, 3u);
	KUNIT_EXPECT_EQ(test, last, 4u);

	op.t_lo = op.t_hi;	/* empty change */
	KUNIT_EXPECT_FALSE(test, s3fs_mop_range(&op, &first, &last));
	op.t_lo = 20 * C;	/* entirely past the object */
	op.t_hi = 21 * C;
	KUNIT_EXPECT_FALSE(test, s3fs_mop_range(&op, &first, &last));
}

static void s3k_mop_needs_data(struct kunit *test)
{
	const u64 C = S3FS_CHUNK_SIZE;
	/* write [C/2, 2C + 10) into a 3.5-chunk object */
	struct s3fs_mop op = {
		.end = 3 * C + C / 2,
		.ow_lo = C / 2, .ow_hi = 2 * C + 10,
		.t_lo = C / 2, .t_hi = 2 * C + 10,
	};

	KUNIT_EXPECT_TRUE(test, s3fs_mop_needs_data(&op, 0));	/* half kept */
	KUNIT_EXPECT_FALSE(test, s3fs_mop_needs_data(&op, 1));	/* overwritten */
	KUNIT_EXPECT_TRUE(test, s3fs_mop_needs_data(&op, 2));
	KUNIT_EXPECT_TRUE(test, s3fs_mop_needs_data(&op, 3));	/* full mode */
	KUNIT_EXPECT_FALSE(test, s3fs_mop_needs_data(&op, 4));	/* past end */
	op.partial = true;
	KUNIT_EXPECT_FALSE(test, s3fs_mop_needs_data(&op, 3));	/* untouched */
	KUNIT_EXPECT_TRUE(test, s3fs_mop_needs_data(&op, 0));
	KUNIT_EXPECT_TRUE(test, s3fs_mop_needs_data(&op, 2));

	/* a write covering the short last chunk up to the object end */
	op = (struct s3fs_mop){ .end = C + 100, .ow_lo = C, .ow_hi = C + 100,
				.t_lo = C, .t_hi = C + 100 };
	KUNIT_EXPECT_FALSE(test, s3fs_mop_needs_data(&op, 1));
}

static void s3k_mpin_balanced(struct kunit *test)
{
	const u64 C = S3FS_CHUNK_SIZE;
	struct s3fs_file_data *fd = s3k_fd_new(test);
	struct s3fs_mop op = {
		.end = 6 * C, .ow_lo = C, .ow_hi = 2 * C,
		.t_lo = C, .t_hi = 2 * C,
	};
	struct s3fs_mpin mp = {};
	u32 ci;

	mutex_lock(&fd->lock);
	KUNIT_ASSERT_EQ(test, s3fs_mpin_acquire_locked(fd, &mp, &op), 0);
	for (ci = 0; ci < 6; ci++)
		KUNIT_EXPECT_EQ_MSG(test, fd->chunks[ci].readers, ci == 1 ? 0u : 1u,
				    "chunk %u", ci);
	s3fs_mpin_release_locked(fd, &mp);
	s3fs_mpin_release_locked(fd, &mp);	/* idempotent */
	for (ci = 0; ci < fd->nr_chunks; ci++)
		KUNIT_EXPECT_EQ_MSG(test, fd->chunks[ci].readers, 0u, "chunk %u", ci);

	/* partial: only the chunks the change reaches */
	op.partial = true;
	KUNIT_ASSERT_EQ(test, s3fs_mpin_acquire_locked(fd, &mp, &op), 0);
	for (ci = 0; ci < 6; ci++)
		KUNIT_EXPECT_EQ_MSG(test, fd->chunks[ci].readers, 0u, "chunk %u", ci);
	op.ow_hi = C + 10;
	op.t_hi = C + 10;
	s3fs_mpin_release_locked(fd, &mp);
	KUNIT_ASSERT_EQ(test, s3fs_mpin_acquire_locked(fd, &mp, &op), 0);
	KUNIT_EXPECT_EQ(test, fd->chunks[1].readers, 1u);
	KUNIT_EXPECT_EQ(test, fd->chunks[0].readers, 0u);
	s3fs_mpin_release_locked(fd, &mp);
	KUNIT_EXPECT_EQ(test, fd->chunks[1].readers, 0u);
	mutex_unlock(&fd->lock);
	s3k_fd_free(fd);
}

static void s3k_modify_ready_and_remote(struct kunit *test)
{
	const u64 C = S3FS_CHUNK_SIZE;
	struct inode *inode = s3k_inode(test, 4 * C);
	struct s3fs_file_data *fd = s3k_fd_new(test);
	static const char x = 'x';
	/* partial write of 10 bytes into chunk 2 */
	struct s3fs_mop op = {
		.end = 4 * C, .ow_lo = 2 * C + 5, .ow_hi = 2 * C + 15,
		.t_lo = 2 * C + 5, .t_hi = 2 * C + 15, .partial = true,
	};
	u32 ci;

	mutex_lock(&fd->lock);
	KUNIT_EXPECT_FALSE(test, s3fs_fd_modify_ready_locked(inode, fd, &op));
	/* the populated chunk 2 is all a partial write needs */
	KUNIT_ASSERT_EQ(test, s3fs_fd_store(fd, 2 * C, &x, NULL, 1, 4 * C), 0);
	KUNIT_EXPECT_FALSE(test, s3fs_fd_modify_ready_locked(inode, fd, &op));
	fd->chunks[2].populated = true;
	KUNIT_EXPECT_TRUE(test, s3fs_fd_modify_ready_locked(inode, fd, &op));
	/* chunk 0 is held: it stays data; 1 and 3 become remote */
	KUNIT_ASSERT_EQ(test, s3fs_fd_store(fd, 0, &x, NULL, 1, 4 * C), 0);
	fd->chunks[0].populated = true;
	KUNIT_ASSERT_EQ(test, s3fs_fd_mark_remote_locked(inode, fd, &op), 0);
	for (ci = 0; ci < 4; ci++)
		KUNIT_EXPECT_EQ_MSG(test, fd->chunks[ci].remote, ci == 1 || ci == 3,
				    "chunk %u", ci);
	KUNIT_EXPECT_EQ(test, fd->nr_remote, 2u);

	/* dirty: ready means "not remote" */
	fd->dirty = true;
	op.t_lo = op.ow_lo = 3 * C + 1;
	op.t_hi = op.ow_hi = 3 * C + 2;
	KUNIT_EXPECT_FALSE(test, s3fs_fd_modify_ready_locked(inode, fd, &op));
	KUNIT_EXPECT_TRUE(test, s3fs_fd_remote_covered(inode, fd, 3 * C, 4 * C));
	KUNIT_EXPECT_FALSE(test, s3fs_fd_remote_covered(inode, fd, 3 * C, 4 * C - 1));
	KUNIT_EXPECT_FALSE(test, s3fs_fd_remote_covered(inode, fd, 2 * C, 4 * C));
	/* a store into the remote chunk makes it data */
	KUNIT_ASSERT_EQ(test, s3fs_fd_store(fd, 3 * C, &x, NULL, 1, 0), 0);
	KUNIT_EXPECT_TRUE(test, s3fs_fd_modify_ready_locked(inode, fd, &op));
	KUNIT_EXPECT_EQ(test, fd->nr_remote, 1u);
	/* a dirty fd without remote chunks needs nothing */
	s3fs_fd_chunk_free(fd, 1);
	KUNIT_EXPECT_EQ(test, fd->nr_remote, 0u);
	op.partial = false;
	op.t_lo = op.ow_lo = 0;
	KUNIT_EXPECT_TRUE(test, s3fs_fd_modify_ready_locked(inode, fd, &op));
	mutex_unlock(&fd->lock);
	s3k_fd_free(fd);
}

/* -- dynamic entry index (AC-55) ------------------------------------------ */

static void s3k_dyn_set(struct s3fs_entry_dyn *e, unsigned long ino,
			unsigned long parent, const char *name)
{
	e->ino = ino;
	e->parent_ino = parent;
	e->mode = S_IFREG | 0644;
	e->name_len = strlen(name);
	memcpy(e->name, name, e->name_len);
}

static void s3k_dyn_index(struct kunit *test)
{
	static const struct { unsigned long ino, parent; const char *name; } t[] = {
		{ 200, 0, "a" }, { 201, 300, "a" }, { 202, 1, "b" },
		{ 300, 1, "dir" }, { 203, 300, "c" }, { 204, 0, "z" },
	};
	struct s3fs_entry_dyn *e;
	const struct s3fs_entry_dyn *hit;
	u32 i, first, cnt;

	e = kvcalloc(ARRAY_SIZE(t), sizeof(*e), GFP_KERNEL);
	KUNIT_ASSERT_NOT_NULL(test, e);
	for (i = 0; i < ARRAY_SIZE(t); i++)
		s3k_dyn_set(&e[i], t[i].ino, t[i].parent, t[i].name);
	KUNIT_ASSERT_EQ(test, s3fs_ebpf_entries_replace(e, ARRAY_SIZE(t)), 0);

	mutex_lock(&s3fs_entries_mtx);
	hit = dyn_find_name(S3FS_ROOT_INO, "a", 1);
	KUNIT_EXPECT_TRUE(test, hit && hit->ino == 200);
	hit = dyn_find_name(300, "a", 1);	/* same name, other dir */
	KUNIT_EXPECT_TRUE(test, hit && hit->ino == 201);
	KUNIT_EXPECT_NULL(test, dyn_find_name(300, "b", 1));
	KUNIT_EXPECT_NULL(test, dyn_find_name(S3FS_ROOT_INO, "ab", 2));
	hit = dyn_find_name(S3FS_ROOT_INO, "z", 1);	/* parent 0 == root */
	KUNIT_EXPECT_TRUE(test, hit && hit->ino == 204);

	/* readdir order: a directory's children, in the daemon's order */
	dyn_children(S3FS_ROOT_INO, &first, &cnt);
	KUNIT_ASSERT_EQ(test, cnt, 4u);
	KUNIT_EXPECT_EQ(test, s3fs_dyn_entries[s3fs_dyn_idx->order[first]].ino, 200ul);
	KUNIT_EXPECT_EQ(test, s3fs_dyn_entries[s3fs_dyn_idx->order[first + 1]].ino, 202ul);
	KUNIT_EXPECT_EQ(test, s3fs_dyn_entries[s3fs_dyn_idx->order[first + 3]].ino, 204ul);
	dyn_children(300, &first, &cnt);
	KUNIT_ASSERT_EQ(test, cnt, 2u);
	KUNIT_EXPECT_EQ(test, s3fs_dyn_entries[s3fs_dyn_idx->order[first]].ino, 201ul);
	dyn_children(12345, &first, &cnt);
	KUNIT_EXPECT_EQ(test, cnt, 0u);
	mutex_unlock(&s3fs_entries_mtx);

	/* W2: record_size reaches the dyn entry through the ino index */
	s3fs_entry_record_size(203, 777);
	mutex_lock(&s3fs_entries_mtx);
	KUNIT_EXPECT_EQ(test, s3fs_dyn_entries[4].size, 777ull);
	mutex_unlock(&s3fs_entries_mtx);

	KUNIT_EXPECT_EQ(test, s3fs_ebpf_entries_replace(NULL, 0), 0);
	KUNIT_EXPECT_NULL(test, s3fs_dyn_entries);
}

static void s3k_dyn_index_many(struct kunit *test)
{
	const u32 n = 5000;
	struct s3fs_entry_dyn *e;
	char name[16];
	u32 i, first, cnt;

	e = kvcalloc(n, sizeof(*e), GFP_KERNEL);
	KUNIT_ASSERT_NOT_NULL(test, e);
	for (i = 0; i < n; i++) {
		snprintf(name, sizeof(name), "f%u", i);
		/* two directories interleaved, and duplicated inos */
		s3k_dyn_set(&e[i], 1000 + i / 2, i % 2 ? 2 : 0, name);
	}
	KUNIT_ASSERT_EQ(test, s3fs_ebpf_entries_replace(e, n), 0);
	mutex_lock(&s3fs_entries_mtx);
	KUNIT_EXPECT_GE(test, s3fs_dyn_idx->mask + 1, 2 * n);
	for (i = 0; i < n; i++) {
		const struct s3fs_entry_dyn *hit;

		snprintf(name, sizeof(name), "f%u", i);
		hit = dyn_find_name(i % 2 ? 2 : S3FS_ROOT_INO, name, strlen(name));
		if (!hit || hit != &s3fs_dyn_entries[i]) {
			KUNIT_FAIL(test, "lookup of %s failed", name);
			break;
		}
	}
	dyn_children(2, &first, &cnt);
	KUNIT_EXPECT_EQ(test, cnt, n / 2);
	for (i = 1; i < cnt; i++)
		if (s3fs_dyn_idx->order[first + i] <= s3fs_dyn_idx->order[first + i - 1]) {
			KUNIT_FAIL(test, "order not stable at %u", i);
			break;
		}
	mutex_unlock(&s3fs_entries_mtx);
	KUNIT_EXPECT_EQ(test, s3fs_ebpf_entries_replace(NULL, 0), 0);
}

/* -- tombstones, local-entry promotion, event seq (AC-84, AC-85) ------------ */

static bool s3k_visible(const char *name)
{
	struct s3fs_entry_snap snap;
	struct qstr q = QSTR_INIT(name, strlen(name));

	return find_entry(S3FS_ROOT_INO, &q, &snap);
}

static struct s3fs_entry_dyn *s3k_listing(struct kunit *test, u32 n,
					  const unsigned long *inos,
					  const char *const *names)
{
	struct s3fs_entry_dyn *e = kvcalloc(n, sizeof(*e), GFP_KERNEL);
	u32 i;

	KUNIT_ASSERT_NOT_NULL(test, e);
	for (i = 0; i < n; i++)
		s3k_dyn_set(&e[i], inos[i], 0, names[i]);
	return e;
}

/* A renamed / unlinked S3-derived entry is hidden from lookup until the daemon has
 * applied the operation (seq <= applied) AND the listing no longer has it. */
static void s3k_hidden_entry_lifecycle(struct kunit *test)
{
	static const unsigned long ab_inos[] = { 200, 201 };
	static const char *const ab[] = { "a", "b" };
	static const unsigned long b_inos[] = { 201 };
	static const char *const b[] = { "b" };

	KUNIT_ASSERT_EQ(test, s3fs_ebpf_entries_replace(s3k_listing(test, 2, ab_inos, ab), 2), 0);
	KUNIT_EXPECT_TRUE(test, s3k_visible("a"));

	entry_forget(S3FS_ROOT_INO, "a", 1, 200, 5);	/* unlink a: event #5 */
	KUNIT_EXPECT_FALSE(test, s3k_visible("a"));
	KUNIT_EXPECT_TRUE(test, s3k_visible("b"));
	KUNIT_EXPECT_EQ(test, s3fs_hidden_nr, 1ul);

	/* a sync whose listing predates the delete (applied < 5) keeps hiding it */
	KUNIT_ASSERT_EQ(test, s3fs_ebpf_entries_replace_applied(s3k_listing(test, 2, ab_inos, ab), 2, 4), 0);
	KUNIT_EXPECT_FALSE(test, s3k_visible("a"));
	/* applied, but S3 still lists it (the delete failed): still hidden */
	KUNIT_ASSERT_EQ(test, s3fs_ebpf_entries_replace_applied(s3k_listing(test, 2, ab_inos, ab), 2, 5), 0);
	KUNIT_EXPECT_FALSE(test, s3k_visible("a"));
	KUNIT_EXPECT_EQ(test, s3fs_hidden_nr, 1ul);
	/* not applied yet although the listing lacks it: kept (it was listed before) */
	KUNIT_ASSERT_EQ(test, s3fs_ebpf_entries_replace_applied(s3k_listing(test, 1, b_inos, b), 1, 4), 0);
	KUNIT_EXPECT_EQ(test, s3fs_hidden_nr, 1ul);
	/* applied and gone from the listing: dropped */
	KUNIT_ASSERT_EQ(test, s3fs_ebpf_entries_replace_applied(s3k_listing(test, 1, b_inos, b), 1, 5), 0);
	KUNIT_EXPECT_EQ(test, s3fs_hidden_nr, 0ul);
	KUNIT_EXPECT_FALSE(test, s3k_visible("a"));	/* not in the listing either */

	/* no event was queued (seq 0): S3 keeps the object, so no tombstone that nothing
	 * would ever drop */
	KUNIT_ASSERT_EQ(test, s3fs_ebpf_entries_replace(s3k_listing(test, 2, ab_inos, ab), 2), 0);
	entry_forget(S3FS_ROOT_INO, "a", 1, 200, 0);
	KUNIT_EXPECT_EQ(test, s3fs_hidden_nr, 0ul);
	KUNIT_EXPECT_TRUE(test, s3k_visible("a"));

	KUNIT_EXPECT_EQ(test, s3fs_ebpf_entries_replace(NULL, 0), 0);
}

/* mv a c; mv c a: the tombstone for (a, ino) from the first rename must not hide a
 * once the inode is back under that name. */
static void s3k_hidden_entry_revived(struct kunit *test)
{
	static const unsigned long a_inos[] = { 200 };
	static const char *const a[] = { "a" };
	struct s3fs_hidden_entry *h = kzalloc(sizeof(*h), GFP_KERNEL);

	KUNIT_ASSERT_NOT_NULL(test, h);
	KUNIT_ASSERT_EQ(test, s3fs_ebpf_entries_replace(s3k_listing(test, 1, a_inos, a), 1), 0);
	mutex_lock(&s3fs_entries_mtx);
	hidden_add_locked(h, S3FS_ROOT_INO, "a", 1, 200, 3);	/* renamed away */
	mutex_unlock(&s3fs_entries_mtx);
	KUNIT_EXPECT_FALSE(test, s3k_visible("a"));

	mutex_lock(&s3fs_entries_mtx);
	hidden_revive_locked(S3FS_ROOT_INO, "a", 1, 200);	/* renamed back */
	mutex_unlock(&s3fs_entries_mtx);
	KUNIT_EXPECT_TRUE(test, s3k_visible("a"));
	KUNIT_EXPECT_EQ(test, s3fs_hidden_nr, 0ul);
	KUNIT_EXPECT_EQ(test, s3fs_ebpf_entries_replace(NULL, 0), 0);
}

/* A local entry (create, the new name of a rename) is promoted by a sync only when
 * the listing has it AND the daemon had applied the event that made it. */
static void s3k_local_entry_promotion_needs_applied(struct kunit *test)
{
	static const unsigned long x_inos[] = { 2000000 };
	static const char *const x[] = { "x" };
	struct s3fs_local_entry *le = kzalloc(sizeof(*le), GFP_KERNEL);

	KUNIT_ASSERT_NOT_NULL(test, le);
	le->ino = 2000000;
	le->parent_ino = 0;
	le->mode = S_IFREG | 0644;
	le->seq = 7;
	le->name_len = 1;
	le->name[0] = 'x';
	mutex_lock(&s3fs_entries_mtx);
	list_add_tail(&le->link, &s3fs_local_entries);
	s3fs_local_nr++;
	mutex_unlock(&s3fs_entries_mtx);

	KUNIT_ASSERT_EQ(test, s3fs_ebpf_entries_replace_applied(s3k_listing(test, 1, x_inos, x), 1, 6), 0);
	KUNIT_EXPECT_EQ(test, s3fs_local_nr, 1ul);	/* listed, but event #7 not applied: kept */
	KUNIT_ASSERT_EQ(test, s3fs_ebpf_entries_replace_applied(s3k_listing(test, 1, x_inos, x), 1, 7), 0);
	KUNIT_EXPECT_EQ(test, s3fs_local_nr, 0ul);	/* applied and listed: promoted */
	KUNIT_EXPECT_EQ(test, s3fs_ebpf_entries_replace(NULL, 0), 0);
}

/* Pop the oldest write-back event; a flush comes back as a flush event.  -EINVAL
 * when the head is a delete or rename (the caller expected a flush). */
static int s3k_pop_flush(struct s3fs_flush_event *out)
{
	struct s3fs_wb_event *w = kmalloc(sizeof(*w), GFP_KERNEL);
	int err;

	if (!w)
		return -ENOMEM;
	err = s3fs_ebpf_wb_pop(w);
	if (!err && w->type != S3FS_WB_FLUSH)
		err = -EINVAL;
	if (!err) {
		memset(out, 0, sizeof(*out));
		out->ino = w->ino;
		out->parent_ino = w->parent_ino;
		out->size = w->size;
		out->seq = w->seq;
		out->name_len = w->name_len;
		out->flags = w->flags;
		memcpy(out->name, w->name, w->name_len);
	}
	kfree(w);
	return err;
}

/* flush / delete / rename events share one strictly increasing seq (AC-84), and
 * s3fs_ebpf_event_seq() is the highest handed out. */
static void s3k_event_seq_is_shared_and_increasing(struct kunit *test)
{
	struct s3fs_wb_event *w = kunit_kzalloc(test, sizeof(*w), GFP_KERNEL);
	u64 s1, s2, s3, s4;

	KUNIT_ASSERT_NOT_NULL(test, w);
	while (s3fs_ebpf_wb_pop(w) == 0)
		;

	s1 = flush_enqueue(5000001, 0, 0, "f", 1, 0);
	s2 = rename_enqueue(5000001, 0, 0, "f", 1, "g", 1);
	s3 = delete_enqueue(5000002, 0, "h", 1);
	s4 = flush_enqueue(5000003, 0, 0, "i", 1, 0);
	KUNIT_EXPECT_GT(test, s1, 0ull);
	KUNIT_EXPECT_LT(test, s1, s2);
	KUNIT_EXPECT_LT(test, s2, s3);
	KUNIT_EXPECT_LT(test, s3, s4);
	KUNIT_EXPECT_EQ(test, s3fs_ebpf_event_seq(), s4);

	/* one FIFO: the events come back in the order they were queued, with
	 * their seq and their own fields */
	KUNIT_ASSERT_EQ(test, s3fs_ebpf_wb_pop(w), 0);
	KUNIT_EXPECT_EQ(test, w->type, S3FS_WB_FLUSH);
	KUNIT_EXPECT_EQ(test, w->seq, s1);
	KUNIT_EXPECT_STREQ(test, w->name, "f");
	KUNIT_ASSERT_EQ(test, s3fs_ebpf_wb_pop(w), 0);
	KUNIT_EXPECT_EQ(test, w->type, S3FS_WB_RENAME);
	KUNIT_EXPECT_EQ(test, w->seq, s2);
	KUNIT_EXPECT_STREQ(test, w->name, "f");
	KUNIT_EXPECT_STREQ(test, w->new_name, "g");
	KUNIT_ASSERT_EQ(test, s3fs_ebpf_wb_pop(w), 0);
	KUNIT_EXPECT_EQ(test, w->type, S3FS_WB_DELETE);
	KUNIT_EXPECT_EQ(test, w->seq, s3);
	KUNIT_ASSERT_EQ(test, s3fs_ebpf_wb_pop(w), 0);
	KUNIT_EXPECT_EQ(test, w->type, S3FS_WB_FLUSH);
	KUNIT_EXPECT_EQ(test, w->seq, s4);
	KUNIT_EXPECT_EQ(test, s3fs_ebpf_wb_pop(w), -EAGAIN);

	/* nothing queued: no seq */
	KUNIT_EXPECT_EQ(test, flush_enqueue(5000004, 0, 0, "", 0, 0), 0ull);
	KUNIT_EXPECT_EQ(test, s3fs_ebpf_event_seq(), s4);
}

/* -- read-miss upcall (U13, P1, H-06, AC-45 C2) --------------------------- */

static void s3k_drain_miss_events(void)
{
	struct s3fs_read_miss_event ev;

	while (s3fs_read_miss_dequeue(&ev, true) > 0)
		;
}

static void s3k_pending_per_chunk(struct kunit *test)
{
	const u64 ino = S3K_INO_BASE + 3, C = S3FS_CHUNK_SIZE;
	struct s3fs_pending *p0, *p0b, *p1;

	KUNIT_EXPECT_FALSE(test, pending_exists(ino));
	p0 = pending_get_or_create(ino, 0);
	p0b = pending_get_or_create(ino, 0);
	p1 = pending_get_or_create(ino, C);
	KUNIT_ASSERT_NOT_NULL(test, p0);
	KUNIT_ASSERT_NOT_NULL(test, p1);
	KUNIT_EXPECT_PTR_EQ(test, p0, p0b);	/* readers share a completion */
	KUNIT_EXPECT_PTR_NE(test, p0, p1);	/* chunks do not */
	KUNIT_EXPECT_EQ(test, refcount_read(&p0->refs), 3u);
	KUNIT_EXPECT_TRUE(test, pending_exists(ino));

	/* completing chunk 1 leaves chunk 0 waiting, and pending_exists(ino)
	 * still sees it (P1: all chunks of an ino share a bucket) */
	KUNIT_EXPECT_EQ(test, s3fs_ebpf_signal_admit(ino, C, -EIO, 0), 0);
	KUNIT_EXPECT_TRUE(test, completion_done(&p1->done));
	KUNIT_EXPECT_EQ(test, p1->err, -EIO);
	KUNIT_EXPECT_FALSE(test, completion_done(&p0->done));
	KUNIT_EXPECT_TRUE(test, pending_exists(ino));
	KUNIT_EXPECT_EQ(test, s3fs_ebpf_signal_admit(ino, C, 0, 0), -ENOENT);
	pending_put(p1);

	KUNIT_EXPECT_EQ(test, s3fs_ebpf_signal_admit(ino, 0, 0, 0), 0);
	KUNIT_EXPECT_TRUE(test, completion_done(&p0->done));
	KUNIT_EXPECT_EQ(test, p0->err, 0);
	KUNIT_EXPECT_FALSE(test, pending_exists(ino));
	/* H-06: the waiters' references keep it alive after the unhash */
	KUNIT_EXPECT_EQ(test, refcount_read(&p0->refs), 2u);
	pending_put(p0b);
	pending_put(p0);
}

static void s3k_pending_verify_populated(struct kunit *test)
{
	const u64 ino = S3K_INO_BASE + 4;
	struct s3fs_pending *p = pending_get_or_create(ino, 0);

	KUNIT_ASSERT_NOT_NULL(test, p);
	/* AC-45 C2: a verified success without a populated chunk (here: no
	 * inode at all) leaves the reader waiting */
	KUNIT_EXPECT_EQ(test, s3fs_ebpf_signal_admit(ino, 0, 0,
						     S3FS_SIGNAL_VERIFY_POPULATED),
			-EAGAIN);
	KUNIT_EXPECT_FALSE(test, completion_done(&p->done));
	/* an error is never verified */
	KUNIT_EXPECT_EQ(test, s3fs_ebpf_signal_admit(ino, 0, -EIO,
						     S3FS_SIGNAL_VERIFY_POPULATED), 0);
	KUNIT_EXPECT_TRUE(test, completion_done(&p->done));
	pending_put(p);
}

static void s3k_miss_queue_fifo(struct kunit *test)
{
	struct s3fs_read_miss_event ev;
	int i;

	s3k_drain_miss_events();
	KUNIT_EXPECT_EQ(test, s3fs_read_miss_dequeue(&ev, true), (ssize_t)-EAGAIN);
	for (i = 0; i < 3; i++)
		KUNIT_ASSERT_EQ(test, miss_enqueue(S3K_INO_BASE + i, i * 4096,
						   (size_t)U32_MAX + 5, i, NULL), 0);
	for (i = 0; i < 3; i++) {
		KUNIT_ASSERT_EQ(test, s3fs_read_miss_dequeue(&ev, true),
				(ssize_t)sizeof(ev));
		KUNIT_EXPECT_EQ(test, ev.ino, S3K_INO_BASE + i);
		KUNIT_EXPECT_EQ(test, ev.offset, (s64)i * 4096);
		KUNIT_EXPECT_EQ(test, ev.count, U32_MAX);	/* clamped */
		KUNIT_EXPECT_EQ(test, ev.flags, (u32)i);
	}
	KUNIT_EXPECT_EQ(test, s3fs_read_miss_dequeue(&ev, true), (ssize_t)-EAGAIN);
}

/* -- flush / evict queues (F1, E-03) -------------------------------------- */

static void s3k_drain_flush_queues(void)
{
	struct s3fs_wb_event *w = kmalloc(sizeof(*w), GFP_KERNEL);
	struct s3fs_flush_item *f, *tmp;

	while (w && !s3fs_ebpf_wb_pop(w))
		;
	kfree(w);
	spin_lock(&s3fs_flush_wait_lock);
	list_for_each_entry_safe(f, tmp, &s3fs_flush_wait_queue, link) {
		list_del(&f->link);
		kfree(f);
	}
	spin_unlock(&s3fs_flush_wait_lock);
}

static void s3k_flush_queue_f1(struct kunit *test)
{
	const u64 a = S3K_INO_BASE + 10, b = S3K_INO_BASE + 11;
	struct s3fs_flush_event *ev = kunit_kzalloc(test, sizeof(*ev), GFP_KERNEL);

	KUNIT_ASSERT_NOT_NULL(test, ev);
	s3k_drain_flush_queues();
	flush_enqueue(a, 1, 0, "a.txt", 5, 0);		/* .create placeholder */
	flush_enqueue(b, 1, 3, "b.txt", 5, 0);
	rename_enqueue(a, 1, 1, "a.txt", 5, "c.txt", 5);	/* F1 must not drop this */
	flush_enqueue(a, 1, 9, "a.txt", 5, S3FS_FLUSH_FLAG_WAIT);
	flush_enqueue(a, 1, 9, "", 0, 0);		/* rejected: no name */
	flush_enqueue(a, 1, 9, "x", S3FS_SYNC_NAME_MAX + 1, 0);

	/* F1: the WAIT event dropped a's queued item, b's stays */
	KUNIT_ASSERT_EQ(test, s3k_pop_flush(ev), 0);
	KUNIT_EXPECT_EQ(test, ev->ino, b);
	KUNIT_EXPECT_EQ(test, ev->size, 3ull);
	KUNIT_EXPECT_STREQ(test, ev->name, "b.txt");
	KUNIT_EXPECT_EQ(test, s3k_pop_flush(ev), -EINVAL);	/* the rename, not a flush */
	KUNIT_EXPECT_EQ(test, s3k_pop_flush(ev), -EAGAIN);

	KUNIT_ASSERT_EQ(test, s3fs_ebpf_flush_pop_wait(ev), 0);
	KUNIT_EXPECT_EQ(test, ev->ino, a);
	KUNIT_EXPECT_EQ(test, ev->size, 9ull);
	KUNIT_EXPECT_EQ(test, ev->flags, S3FS_FLUSH_FLAG_WAIT);
	KUNIT_EXPECT_STREQ(test, ev->name, "a.txt");
	KUNIT_EXPECT_EQ(test, s3fs_ebpf_flush_pop_wait(ev), -EAGAIN);
}

static void s3k_flush_wait_reopen(struct kunit *test)
{
	struct s3fs_flush_event *ev = kunit_kzalloc(test, sizeof(*ev), GFP_KERNEL);

	KUNIT_ASSERT_NOT_NULL(test, ev);
	flush_queue_drain();	/* what put_super does */
	KUNIT_EXPECT_EQ(test, s3fs_ebpf_flush_pop_wait(ev), -ENODEV);
	flush_wait_reopen();	/* what fill_super does */
	KUNIT_EXPECT_EQ(test, s3fs_ebpf_flush_pop_wait(ev), -EAGAIN);
	flush_enqueue(S3K_INO_BASE + 12, 1, 1, "c", 1, S3FS_FLUSH_FLAG_WAIT);
	KUNIT_EXPECT_EQ(test, s3fs_ebpf_flush_pop_wait(ev), 0);
}

/* -- the filesystem, mounted in-kernel ------------------------------------ */

struct s3k_vfs {
	struct vfsmount *mnt;
	char __user *ubuf;	/* user memory for .write / ioctl-style calls */
	size_t ubuf_len;
};

#define S3K_UBUF_LEN	(4 * S3K_MiB)

static int s3k_vfs_init(struct kunit *test)
{
	struct file_system_type *type = get_fs_type("s3fs_ebpf");
	struct s3k_vfs *v = kunit_kzalloc(test, sizeof(*v), GFP_KERNEL);
	unsigned long u;

	if (!type || !v)
		return -ENODEV;
	s3fs_open_check = false;	/* default is on (AC-78); no daemon answers here */
	v->mnt = kern_mount(type);
	module_put(type->owner);	/* put_filesystem() is not exported */
	if (IS_ERR(v->mnt))
		return PTR_ERR(v->mnt);
	u = kunit_vm_mmap(test, NULL, 0, S3K_UBUF_LEN, PROT_READ | PROT_WRITE,
			  MAP_ANONYMOUS | MAP_PRIVATE, 0);
	if (IS_ERR_VALUE(u)) {
		kern_unmount(v->mnt);
		return (int)u;
	}
	v->ubuf = (char __user *)u;
	v->ubuf_len = S3K_UBUF_LEN;
	s3k_drain_flush_queues();
	test->priv = v;
	return 0;
}

static void s3k_vfs_exit(struct kunit *test)
{
	struct s3k_vfs *v = test->priv;

	flush_delayed_fput();
	kern_unmount(v->mnt);
	s3fs_ebpf_entries_replace(NULL, 0);
	flush_wait_reopen();	/* put_super shut the fsync queue */
}

static struct file *s3k_open(struct kunit *test, const char *name, int flags)
{
	struct s3k_vfs *v = test->priv;
	struct file *f = file_open_root_mnt(v->mnt, name, flags, 0);

	KUNIT_ASSERT_FALSE_MSG(test, IS_ERR(f), "open %s: %ld", name, PTR_ERR(f));
	return f;
}

static void s3k_close(struct file *f)
{
	fput(f);
	flush_delayed_fput();
}

static ssize_t s3k_write(struct kunit *test, struct file *f, loff_t pos,
			 const void *data, size_t n)
{
	struct s3k_vfs *v = test->priv;

	KUNIT_ASSERT_LE(test, n, v->ubuf_len);
	KUNIT_ASSERT_EQ(test, copy_to_user(v->ubuf, data, n), 0ul);
	return s3fs_ebpf_file_write(f, v->ubuf, n, &pos);
}

static int s3k_populate(struct kunit *test, u64 ino, u64 off, u8 fill, size_t n)
{
	struct s3k_vfs *v = test->priv;
	size_t done;

	KUNIT_ASSERT_LE(test, n, v->ubuf_len);
	for (done = 0; done < n; done += PAGE_SIZE) {
		u8 page[64];

		memset(page, fill, sizeof(page));
		KUNIT_ASSERT_EQ(test, copy_to_user(v->ubuf + done, page,
						   sizeof(page)), 0ul);
		KUNIT_ASSERT_EQ(test, clear_user(v->ubuf + done + sizeof(page),
						 PAGE_SIZE - sizeof(page)), 0ul);
	}
	return s3fs_ebpf_populate_pages(ino, off, n, v->ubuf);
}

/* Replace the entry table with one regular file under the root. */
static void s3k_one_entry(struct kunit *test, u64 ino, const char *name, u64 size)
{
	struct s3fs_entry_dyn *e = kvcalloc(1, sizeof(*e), GFP_KERNEL);

	KUNIT_ASSERT_NOT_NULL(test, e);
	s3k_dyn_set(e, ino, S3FS_ROOT_INO, name);
	e->size = size;
	KUNIT_ASSERT_EQ(test, s3fs_ebpf_entries_replace(e, 1), 0);
}

static struct s3fs_file_data *s3k_fd_of(struct file *f)
{
	return file_inode(f)->i_private;
}

static void s3k_vfs_read_bootstrap(struct kunit *test)
{
	struct file *f = s3k_open(test, "hello.txt", O_RDONLY);
	char buf[32] = {};
	loff_t pos = 0;

	KUNIT_EXPECT_EQ(test, kernel_read(f, buf, sizeof(buf), &pos), 17L);
	KUNIT_EXPECT_STREQ(test, buf, "hello, s3fs-ebpf\n");
	KUNIT_EXPECT_EQ(test, kernel_read(f, buf, sizeof(buf), &pos), 0L);
	s3k_close(f);
}

static void s3k_vfs_maxbytes(struct kunit *test)
{
	struct s3k_vfs *v = test->priv;

	/* alloc_super's default (MAX_NON_LFS) cut reads at 2 GiB */
	KUNIT_EXPECT_GT(test, v->mnt->mnt_sb->s_maxbytes, (loff_t)MAX_NON_LFS);
}

static void s3k_vfs_read_past_2g(struct kunit *test)
{
	const u64 ino = S3K_INO_BASE + 20, at = 2ULL << 30;
	struct file *f;
	char buf[64];
	loff_t pos = at;

	s3k_one_entry(test, ino, "big", 3ULL << 30);
	f = s3k_open(test, "big", O_RDONLY);
	KUNIT_ASSERT_EQ(test, i_size_read(file_inode(f)), (loff_t)(3ULL << 30));
	KUNIT_ASSERT_EQ(test, s3k_populate(test, ino, at, 'g', S3FS_CHUNK_SIZE), 0);
	KUNIT_EXPECT_EQ(test, kernel_read(f, buf, sizeof(buf), &pos), 64L);
	KUNIT_EXPECT_EQ(test, buf[0], 'g');
	KUNIT_EXPECT_EQ(test, buf[63], 'g');
	pos = at;
	KUNIT_EXPECT_EQ(test, vfs_llseek(f, at + 1, SEEK_SET), (loff_t)(at + 1));
	s3k_close(f);
}

static void s3k_vfs_populate_mid_chunk(struct kunit *test)
{
	const u64 ino = S3K_INO_BASE + 21, C = S3FS_CHUNK_SIZE;
	struct file *f;
	char buf[16];
	loff_t pos;

	s3k_one_entry(test, ino, "six", 6 * S3K_MiB);
	f = s3k_open(test, "six", O_RDONLY);
	/* [5 MiB, 6 MiB) reaches eof but starts inside chunk 1 */
	KUNIT_ASSERT_EQ(test, s3k_populate(test, ino, 5 * S3K_MiB, 't', S3K_MiB), 0);
	KUNIT_EXPECT_FALSE(test, s3k_fd_of(f)->chunks[1].populated);
	/* no daemon: the never-fetched front of the chunk is an error, not 0s
	 * (T2 serves a chunk only once it is populated as a whole) */
	pos = C;
	KUNIT_EXPECT_EQ(test, kernel_read(f, buf, sizeof(buf), &pos), (ssize_t)-EIO);
	/* a populate of the whole chunk makes it valid */
	KUNIT_ASSERT_EQ(test, s3k_populate(test, ino, C, 'u', 2 * S3K_MiB), 0);
	KUNIT_EXPECT_TRUE(test, s3k_fd_of(f)->chunks[1].populated);
	pos = C;
	KUNIT_EXPECT_EQ(test, kernel_read(f, buf, sizeof(buf), &pos), 16L);
	KUNIT_EXPECT_EQ(test, buf[0], 'u');
	pos = 5 * S3K_MiB;
	KUNIT_EXPECT_EQ(test, kernel_read(f, buf, sizeof(buf), &pos), 16L);
	KUNIT_EXPECT_EQ(test, buf[0], 'u');
	s3k_close(f);
}

static void s3k_vfs_write_read_back(struct kunit *test)
{
	struct file *f = s3k_open(test, "hello.txt", O_RDWR);
	char buf[32] = {};
	loff_t pos = 0;

	/* fill the page cache first: the write must invalidate it */
	KUNIT_ASSERT_EQ(test, kernel_read(f, buf, sizeof(buf), &pos), 17L);
	KUNIT_ASSERT_EQ(test, s3k_write(test, f, 0, "HELLO", 5), 5L);
	KUNIT_ASSERT_EQ(test, s3k_write(test, f, 17, "!!", 2), 2L);
	KUNIT_EXPECT_TRUE(test, s3k_fd_of(f)->dirty);
	KUNIT_EXPECT_EQ(test, i_size_read(file_inode(f)), (loff_t)19);
	memset(buf, 0, sizeof(buf));
	pos = 0;
	KUNIT_EXPECT_EQ(test, kernel_read(f, buf, sizeof(buf), &pos), 19L);
	KUNIT_EXPECT_STREQ(test, buf, "HELLO, s3fs-ebpf\n!!");
	s3k_close(f);
}

static void s3k_vfs_truncate(struct kunit *test)
{
	struct file *f = s3k_open(test, "hello.txt", O_RDWR);
	char buf[32];
	loff_t pos = 0;
	int i;

	KUNIT_ASSERT_EQ(test, kernel_read(f, buf, sizeof(buf), &pos), 17L);
	KUNIT_ASSERT_EQ(test, vfs_truncate(&f->f_path, 5), 0);
	KUNIT_ASSERT_EQ(test, vfs_truncate(&f->f_path, 12), 0);
	KUNIT_EXPECT_TRUE(test, s3k_fd_of(f)->dirty);
	memset(buf, 0x7f, sizeof(buf));
	pos = 0;
	KUNIT_ASSERT_EQ(test, kernel_read(f, buf, sizeof(buf), &pos), 12L);
	KUNIT_EXPECT_EQ(test, memcmp(buf, "hello", 5), 0);
	for (i = 5; i < 12; i++)
		KUNIT_EXPECT_EQ_MSG(test, buf[i], 0, "byte %d", i);
	s3k_close(f);
}

static void s3k_vfs_close_flush(struct kunit *test)
{
	struct s3k_vfs *v = test->priv;
	struct s3fs_flush_event *ev = kunit_kzalloc(test, sizeof(*ev), GFP_KERNEL);
	struct file *f = s3k_open(test, "hello.txt", O_RDWR);
	struct inode *inode = file_inode(f);
	struct s3fs_file_data *fd = inode->i_private;
	char out[32] = {};

	KUNIT_ASSERT_NOT_NULL(test, ev);
	ihold(inode);
	KUNIT_ASSERT_EQ(test, s3k_write(test, f, 0, "J", 1), 1L);
	s3k_close(f);	/* .release: fire-and-forget flush */
	KUNIT_ASSERT_EQ(test, s3k_pop_flush(ev), 0);
	KUNIT_EXPECT_EQ(test, ev->ino, (u64)inode->i_ino);
	KUNIT_EXPECT_EQ(test, ev->size, 17ull);
	KUNIT_EXPECT_EQ(test, ev->parent_ino, (u64)S3FS_ROOT_INO);
	KUNIT_EXPECT_STREQ(test, ev->name, "hello.txt");
	KUNIT_EXPECT_EQ(test, ev->flags, 0u);
	KUNIT_EXPECT_TRUE(test, fd->flushing);
	KUNIT_EXPECT_FALSE(test, fd->dirty);
	KUNIT_EXPECT_FALSE(test, list_empty(&fd->pin_link));	/* W1 pin */

	/* W1: a signal for a PUT that never read the buffer covers nothing */
	KUNIT_EXPECT_EQ(test, s3fs_ebpf_signal_flush(inode->i_ino, 0), -ENOENT);
	KUNIT_EXPECT_TRUE(test, fd->flushing);
	KUNIT_ASSERT_EQ(test, s3fs_ebpf_read_dirty(inode->i_ino, v->ubuf, 32, NULL), 17L);
	KUNIT_ASSERT_EQ(test, copy_from_user(out, v->ubuf, 17), 0ul);
	KUNIT_EXPECT_STREQ(test, out, "Jello, s3fs-ebpf\n");
	KUNIT_EXPECT_EQ(test, s3fs_ebpf_signal_flush(inode->i_ino, 0), -ENOENT);
	KUNIT_EXPECT_FALSE(test, fd->flushing);
	KUNIT_EXPECT_TRUE(test, list_empty(&fd->pin_link));
	KUNIT_EXPECT_TRUE(test, fd->chunks[0].populated);
	iput(inode);
}

struct s3k_fsync_ctx {
	struct file *f;
	int ret;
	struct completion done;
};

static int s3k_fsync_thread(void *arg)
{
	struct s3k_fsync_ctx *c = arg;

	c->ret = vfs_fsync(c->f, 0);
	complete(&c->done);
	return 0;
}

static void s3k_vfs_fsync_waits_for_its_put(struct kunit *test)
{
	struct s3k_vfs *v = test->priv;
	struct s3fs_flush_event *ev = kunit_kzalloc(test, sizeof(*ev), GFP_KERNEL);
	struct s3k_fsync_ctx *c = kunit_kzalloc(test, sizeof(*c), GFP_KERNEL);
	struct file *f = s3k_open(test, "hello.txt", O_RDWR);
	u64 ino = file_inode(f)->i_ino;
	struct task_struct *t;
	int i;

	KUNIT_ASSERT_NOT_NULL(test, ev);
	KUNIT_ASSERT_NOT_NULL(test, c);
	/* close #1: the daemon pops it and reads the buffer (PUT 1 starts) */
	KUNIT_ASSERT_EQ(test, s3k_write(test, f, 0, "1", 1), 1L);
	KUNIT_ASSERT_EQ(test, s3fs_maybe_enqueue_flush(f, false), 0);
	KUNIT_ASSERT_EQ(test, s3k_pop_flush(ev), 0);
	KUNIT_ASSERT_EQ(test, s3fs_ebpf_read_dirty(ino, v->ubuf, 32, NULL), 17L);

	/* a second write, then fsync blocks on its own flush */
	KUNIT_ASSERT_EQ(test, s3k_write(test, f, 1, "2", 1), 1L);
	c->f = f;
	init_completion(&c->done);
	t = kthread_run(s3k_fsync_thread, c, "s3k_fsync");
	KUNIT_ASSERT_FALSE(test, IS_ERR(t));
	for (i = 0; i < 100; i++) {
		if (s3fs_ebpf_flush_pop_wait(ev) == 0)
			break;
	}
	KUNIT_ASSERT_LT(test, i, 100);
	KUNIT_EXPECT_EQ(test, ev->flags, S3FS_FLUSH_FLAG_WAIT);

	/* PUT 1 finishes: it holds "1ello", not the "2" fsync is waiting for */
	KUNIT_EXPECT_EQ(test, s3fs_ebpf_signal_flush(ino, 0), 0);
	msleep(50);
	KUNIT_EXPECT_FALSE_MSG(test, completion_done(&c->done),
			       "fsync returned %d before its data was read", c->ret);

	/* the fsync's own PUT */
	KUNIT_ASSERT_EQ(test, s3fs_ebpf_read_dirty(ino, v->ubuf, 32, NULL), 17L);
	KUNIT_EXPECT_EQ(test, s3fs_ebpf_signal_flush(ino, 0), 0);
	KUNIT_ASSERT_GT(test, wait_for_completion_timeout(&c->done, 5 * HZ), 0ul);
	KUNIT_EXPECT_EQ(test, c->ret, 0);
	s3k_close(f);
}

/* AC-71: a READ_DIRTY whose buffer is smaller than the content (a stale
 * event's size: the 0-byte placeholder .create queues, read after the file
 * grew) neither copies nor claims the flush.  Before, it claimed every enqueue,
 * so the signal of its (short) PUT woke an fsync whose data had not been read
 * yet.  The fsync's own read, with a buffer of exactly the content, completes it. */
static void s3k_vfs_read_dirty_short_buffer_claims_nothing(struct kunit *test)
{
	struct s3k_vfs *v = test->priv;
	struct s3fs_flush_event *ev = kunit_kzalloc(test, sizeof(*ev), GFP_KERNEL);
	struct s3k_fsync_ctx *c = kunit_kzalloc(test, sizeof(*c), GFP_KERNEL);
	struct file *f = s3k_open(test, "hello.txt", O_RDWR);
	struct s3fs_file_data *fd = s3k_fd_of(f);
	u64 ino = file_inode(f)->i_ino;
	u64 size = 0;
	struct task_struct *t;
	int i;

	KUNIT_ASSERT_NOT_NULL(test, ev);
	KUNIT_ASSERT_NOT_NULL(test, c);
	KUNIT_ASSERT_EQ(test, s3k_write(test, f, 0, "1", 1), 1L);
	c->f = f;
	init_completion(&c->done);
	t = kthread_run(s3k_fsync_thread, c, "s3k_fsync_short");
	KUNIT_ASSERT_FALSE(test, IS_ERR(t));
	for (i = 0; i < 100; i++)
		if (s3fs_ebpf_flush_pop_wait(ev) == 0)
			break;
	KUNIT_ASSERT_LT(test, i, 100);

	/* the stale event: a buffer of the size it had (0) */
	KUNIT_EXPECT_EQ(test, s3fs_ebpf_read_dirty(ino, NULL, 0, &size), (long)-ENOSPC);
	KUNIT_EXPECT_EQ(test, size, 17ull);
	KUNIT_EXPECT_NE(test, fd->flush_read_seq, fd->flush_seq);
	KUNIT_EXPECT_EQ(test, s3fs_ebpf_signal_flush(ino, 0), 0);
	msleep(50);
	KUNIT_EXPECT_FALSE_MSG(test, completion_done(&c->done),
			       "fsync returned %d on a PUT that read no data", c->ret);

	/* the fsync's own read: exactly the content */
	KUNIT_ASSERT_EQ(test, s3fs_ebpf_read_dirty(ino, v->ubuf, 17, NULL), 17L);
	KUNIT_EXPECT_EQ(test, fd->flush_read_seq, fd->flush_seq);
	KUNIT_EXPECT_EQ(test, s3fs_ebpf_signal_flush(ino, 0), 0);
	KUNIT_ASSERT_GT(test, wait_for_completion_timeout(&c->done, 5 * HZ), 0ul);
	KUNIT_EXPECT_EQ(test, c->ret, 0);
	s3k_close(f);
}

/* fsync of a file truncated to 0: the daemon still issues READ_DIRTY (cap 0)
 * so kmod knows the PUT covers the flush; the waiter and the pin go. */
static void s3k_vfs_fsync_empty_file(struct kunit *test)
{
	struct s3fs_flush_event *ev = kunit_kzalloc(test, sizeof(*ev), GFP_KERNEL);
	struct s3k_fsync_ctx *c = kunit_kzalloc(test, sizeof(*c), GFP_KERNEL);
	struct file *f = s3k_open(test, "hello.txt", O_RDWR);
	struct s3fs_file_data *fd = s3k_fd_of(f);
	u64 ino = file_inode(f)->i_ino;
	struct task_struct *t;
	int i;

	KUNIT_ASSERT_NOT_NULL(test, ev);
	KUNIT_ASSERT_NOT_NULL(test, c);
	KUNIT_ASSERT_EQ(test, vfs_truncate(&f->f_path, 0), 0);
	c->f = f;
	init_completion(&c->done);
	t = kthread_run(s3k_fsync_thread, c, "s3k_fsync0");
	KUNIT_ASSERT_FALSE(test, IS_ERR(t));
	for (i = 0; i < 100; i++)
		if (s3fs_ebpf_flush_pop_wait(ev) == 0)
			break;
	KUNIT_ASSERT_LT(test, i, 100);
	KUNIT_EXPECT_EQ(test, ev->size, 0ull);
	KUNIT_EXPECT_EQ(test, s3fs_ebpf_read_dirty(ino, NULL, 0, NULL), 0L);
	KUNIT_EXPECT_EQ(test, s3fs_ebpf_signal_flush(ino, 0), 0);
	KUNIT_ASSERT_GT(test, wait_for_completion_timeout(&c->done, 5 * HZ), 0ul);
	KUNIT_EXPECT_EQ(test, c->ret, 0);
	KUNIT_EXPECT_FALSE(test, fd->flushing);
	KUNIT_EXPECT_TRUE(test, list_empty(&fd->pin_link));
	s3k_close(f);
}

/* A folio past the end of a dirty fd (a truncate that ran after read_folio
 * looked at i_size) reads as zeros instead of failing the read. */
static void s3k_vfs_fill_past_dirty_eof(struct kunit *test)
{
	struct file *f = s3k_open(test, "hello.txt", O_RDWR);
	struct s3fs_file_data *fd = s3k_fd_of(f);
	struct folio *folio = folio_alloc(GFP_KERNEL, 0);
	u8 *p;

	KUNIT_ASSERT_NOT_NULL(test, folio);
	KUNIT_ASSERT_EQ(test, vfs_truncate(&f->f_path, 5), 0);
	KUNIT_ASSERT_TRUE(test, fd->dirty);
	p = kmap_local_folio(folio, 0);
	memset(p, 0xee, PAGE_SIZE);
	kunmap_local(p);
	KUNIT_EXPECT_TRUE(test, s3fs_folio_fill_from_kmalloc(folio, PAGE_SIZE,
							     PAGE_SIZE, fd, false));
	p = kmap_local_folio(folio, 0);
	KUNIT_EXPECT_EQ(test, p[0], 0);
	KUNIT_EXPECT_EQ(test, p[PAGE_SIZE - 1], 0);
	kunmap_local(p);
	/* and inside the file, the data */
	KUNIT_EXPECT_TRUE(test, s3fs_folio_fill_from_kmalloc(folio, PAGE_SIZE, 0,
							     fd, false));
	p = kmap_local_folio(folio, 0);
	KUNIT_EXPECT_EQ(test, memcmp(p, "hello\0", 6), 0);
	kunmap_local(p);
	folio_put(folio);
	s3k_close(f);
}

static void s3k_vfs_remount_fsync_queue(struct kunit *test)
{
	struct s3k_vfs *v = test->priv;
	struct file_system_type *type = get_fs_type("s3fs_ebpf");
	struct s3fs_flush_event *ev = kunit_kzalloc(test, sizeof(*ev), GFP_KERNEL);

	KUNIT_ASSERT_NOT_NULL(test, type);
	KUNIT_ASSERT_NOT_NULL(test, ev);
	kern_unmount(v->mnt);
	v->mnt = kern_mount(type);
	module_put(type->owner);	/* put_filesystem() is not exported */
	KUNIT_ASSERT_FALSE(test, IS_ERR(v->mnt));
	/* the fsync queue of the new mount is open: empty, not shut down */
	KUNIT_EXPECT_EQ(test, s3fs_ebpf_flush_pop_wait(ev), -EAGAIN);
}

/* -- concurrency ----------------------------------------------------------- */

#define S3K_STRESS_SPAN	(256 * 1024)	/* file offsets the threads touch */
#define S3K_STRESS_MS	300

struct s3k_stress {
	struct file *f;
	struct mm_struct *mm;
	char __user *ubuf;	/* this thread's slice of the test's user buffer */
	unsigned long deadline;
	atomic_t *errors;
	int kind;		/* 0 write, 1 read, 2 truncate */
	struct completion done;
};

static int s3k_stress_thread(void *arg)
{
	struct s3k_stress *s = arg;
	char *kbuf = kmalloc(8192, GFP_KERNEL);
	unsigned int iter = 0;

	if (s->mm)
		kthread_use_mm(s->mm);
	while (kbuf && time_before(jiffies, s->deadline)) {
		loff_t pos = get_random_u32_below(S3K_STRESS_SPAN);
		size_t n = 1 + get_random_u32_below(8192);
		long r = 0;

		switch (s->kind) {
		case 0:
			memset(kbuf, 'a' + iter % 26, n);
			if (copy_to_user(s->ubuf, kbuf, n))
				r = -EFAULT;
			else
				r = s3fs_ebpf_file_write(s->f, s->ubuf, n, &pos);
			break;
		case 1:
			r = kernel_read(s->f, kbuf, n, &pos);
			break;
		case 2:
			r = vfs_truncate(&s->f->f_path, pos);
			break;
		}
		if (r < 0) {
			pr_err("s3k_stress: kind %d iter %u: %ld\n", s->kind, iter, r);
			atomic_inc(s->errors);
		}
		iter++;
		cond_resched();
	}
	if (s->mm)
		kthread_unuse_mm(s->mm);
	kfree(kbuf);
	complete(&s->done);
	return 0;
}

/* Writers, readers and truncates race on one file; afterwards the page
 * cache must agree with the fd and the chunk invariants must hold. */
static void s3k_vfs_concurrent_modify(struct kunit *test)
{
	static const int kinds[] = { 0, 0, 1, 1, 2 };
	struct s3k_vfs *v = test->priv;
	struct file *f = s3k_open(test, "hello.txt", O_RDWR);
	struct s3fs_file_data *fd = s3k_fd_of(f);
	struct s3k_stress *st;
	atomic_t errors = ATOMIC_INIT(0);
	char *a, *b;
	loff_t pos, size;
	int i, slice = 0;

	st = kunit_kcalloc(test, ARRAY_SIZE(kinds), sizeof(*st), GFP_KERNEL);
	a = kunit_kzalloc(test, S3K_STRESS_SPAN + 8192, GFP_KERNEL);
	b = kunit_kzalloc(test, S3K_STRESS_SPAN + 8192, GFP_KERNEL);
	KUNIT_ASSERT_NOT_NULL(test, st);
	KUNIT_ASSERT_NOT_NULL(test, a);
	KUNIT_ASSERT_NOT_NULL(test, b);
	for (i = 0; i < ARRAY_SIZE(kinds); i++) {
		struct task_struct *t;

		st[i].f = f;
		st[i].kind = kinds[i];
		st[i].errors = &errors;
		st[i].deadline = jiffies + msecs_to_jiffies(S3K_STRESS_MS);
		if (kinds[i] == 0) {
			mmget(current->mm);
			st[i].mm = current->mm;
			st[i].ubuf = v->ubuf + slice++ * 8192;
		}
		init_completion(&st[i].done);
		t = kthread_run(s3k_stress_thread, &st[i], "s3k_stress%d", i);
		KUNIT_ASSERT_FALSE(test, IS_ERR(t));
	}
	for (i = 0; i < ARRAY_SIZE(kinds); i++) {
		wait_for_completion(&st[i].done);
		if (st[i].mm)
			mmput(st[i].mm);
	}
	KUNIT_EXPECT_EQ(test, atomic_read(&errors), 0);

	mutex_lock(&fd->lock);
	s3k_fd_check(test, fd);
	size = fd->size;
	KUNIT_EXPECT_EQ(test, i_size_read(file_inode(f)), size);
	KUNIT_ASSERT_LE(test, size, (loff_t)S3K_STRESS_SPAN + 8192);
	KUNIT_ASSERT_EQ(test, s3fs_fd_copy_out(fd, v->ubuf, size), (long)size);
	mutex_unlock(&fd->lock);
	KUNIT_ASSERT_EQ(test, copy_from_user(a, v->ubuf, size), 0ul);
	pos = 0;
	KUNIT_EXPECT_EQ(test, kernel_read(f, b, size + 1, &pos), (ssize_t)size);
	for (i = 0; i < size; i++)
		if (a[i] != b[i]) {
			KUNIT_FAIL(test, "page cache differs from fd at %d: %#x vs %#x",
				   i, b[i], a[i]);
			break;
		}
	s3k_close(f);
}

struct s3k_pend_stress {
	u64 ino;
	unsigned long deadline;
	bool signaler;
	struct completion done;
};

static int s3k_pend_thread(void *arg)
{
	struct s3k_pend_stress *s = arg;

	while (time_before(jiffies, s->deadline)) {
		u64 co = (u64)get_random_u32_below(4) << S3FS_PCACHE_CHUNK_SHIFT;

		if (s->signaler) {
			s3fs_ebpf_signal_admit(s->ino, co, 0, 0);
		} else {
			struct s3fs_pending *p = pending_get_or_create(s->ino, co);

			if (p) {
				wait_for_completion_timeout(&p->done, 1);
				pending_put(p);
			}
		}
		cond_resched();
	}
	complete(&s->done);
	return 0;
}

/* Readers come and go on four chunks of one ino while signals complete them:
 * the two-reference scheme (H-06) must neither leak nor free early (KASAN /
 * refcount warnings), and nothing may stay hashed once all are signalled. */
static void s3k_pending_concurrent(struct kunit *test)
{
	const u64 ino = S3K_INO_BASE + 30;
	struct s3k_pend_stress *st;
	int i, n = 6;

	st = kunit_kcalloc(test, n, sizeof(*st), GFP_KERNEL);
	KUNIT_ASSERT_NOT_NULL(test, st);
	for (i = 0; i < n; i++) {
		struct task_struct *t;

		st[i].ino = ino;
		st[i].signaler = i < 2;
		st[i].deadline = jiffies + msecs_to_jiffies(S3K_STRESS_MS);
		init_completion(&st[i].done);
		t = kthread_run(s3k_pend_thread, &st[i], "s3k_pend%d", i);
		KUNIT_ASSERT_FALSE(test, IS_ERR(t));
	}
	for (i = 0; i < n; i++)
		wait_for_completion(&st[i].done);
	for (i = 0; i < 4; i++)
		s3fs_ebpf_signal_admit(ino, (u64)i << S3FS_PCACHE_CHUNK_SHIFT, 0, 0);
	KUNIT_EXPECT_FALSE(test, pending_exists(ino));
}

/* -- Issue #22: hit accumulation ------------------------------------------ */

static u64 s3k_hit_order[8];
static u32 s3k_hit_n;

static void s3k_hit_rec_on_hit(u64 ino)
{
	if (s3k_hit_n < ARRAY_SIZE(s3k_hit_order))
		s3k_hit_order[s3k_hit_n++] = ino;
}

static void s3k_hit_bump(struct s3fs_file_data *fd, u64 n, u64 tick)
{
	struct s3fs_hit_acc *acc = get_cpu_ptr(fd->hit);

	acc->count += n;
	acc->last_tick = tick;
	put_cpu_ptr(fd->hit);
}

/* Phase 3d: the resident index holds an fd exactly while fd->cap > 0, whatever
 * the dirty flag (dirty fds are linked but never returned as candidates), and
 * s3fs_collect_resident returns each clean one once with its fd->cap. */
static bool s3k_index_has(u64 ino, u64 *bytes)
{
	struct s3fs_resident *res;
	bool found = false;
	int n, i;

	n = s3fs_collect_resident(&res);
	for (i = 0; i < n; i++)
		if (res[i].ino == ino) {
			found = true;
			if (bytes)
				*bytes = res[i].bytes;
		}
	kfree(res);
	return found;
}

static void s3k_resident_index_follows_cap(struct kunit *test)
{
	const u64 ino = S3K_INO_BASE + 90, C = S3FS_CHUNK_SIZE;
	struct s3fs_file_data *fd = s3fs_file_data_new(ino, NULL, 0);
	int nodes0 = atomic_read(&s3fs_resident_nodes);
	char *buf = kunit_kzalloc(test, 8192, GFP_KERNEL);	/* the source of every store below */
	u64 bytes = 0;
	char x = 1;

	KUNIT_ASSERT_NOT_NULL(test, fd);
	KUNIT_ASSERT_NOT_NULL(test, buf);
	KUNIT_EXPECT_TRUE(test, list_empty(&fd->res_link));	/* cap == 0: not indexed */
	KUNIT_EXPECT_FALSE(test, s3k_index_has(ino, NULL));

	KUNIT_ASSERT_EQ(test, s3fs_fd_store(fd, 0, &x, NULL, 1, 0), 0);
	KUNIT_ASSERT_EQ(test, s3fs_fd_store(fd, C, &x, NULL, 1, 0), 0);
	KUNIT_EXPECT_FALSE(test, list_empty(&fd->res_link));
	KUNIT_EXPECT_EQ(test, atomic_read(&s3fs_resident_nodes), nodes0 + 1);	/* one node per fd, not per chunk */
	KUNIT_EXPECT_TRUE(test, s3k_index_has(ino, &bytes));
	KUNIT_EXPECT_EQ(test, bytes, (u64)fd->cap);

	/* growing an allocated chunk keeps the single node and updates the bytes */
	KUNIT_ASSERT_EQ(test, s3fs_fd_store(fd, 1, buf, NULL, 8192, 0), 0);
	KUNIT_EXPECT_EQ(test, atomic_read(&s3fs_resident_nodes), nodes0 + 1);
	KUNIT_EXPECT_TRUE(test, s3k_index_has(ino, &bytes));
	KUNIT_EXPECT_EQ(test, bytes, (u64)fd->cap);

	/* dirty / flushing: still linked (it holds data) but not a candidate */
	fd->dirty = true;
	KUNIT_EXPECT_FALSE(test, list_empty(&fd->res_link));
	KUNIT_EXPECT_FALSE(test, s3k_index_has(ino, NULL));
	fd->dirty = false;
	fd->flushing = true;
	KUNIT_EXPECT_FALSE(test, s3k_index_has(ino, NULL));
	fd->flushing = false;
	KUNIT_EXPECT_TRUE(test, s3k_index_has(ino, NULL));

	/* freeing one chunk of two keeps it; freeing the last one unlinks it */
	s3fs_fd_chunk_free(fd, 1);
	KUNIT_EXPECT_FALSE(test, list_empty(&fd->res_link));
	KUNIT_EXPECT_TRUE(test, s3k_index_has(ino, NULL));
	s3fs_fd_chunk_free(fd, 0);
	KUNIT_EXPECT_EQ(test, (u64)fd->cap, 0ull);
	KUNIT_EXPECT_TRUE(test, list_empty(&fd->res_link));
	KUNIT_EXPECT_EQ(test, atomic_read(&s3fs_resident_nodes), nodes0);
	KUNIT_EXPECT_FALSE(test, s3k_index_has(ino, NULL));

	/* populate again, then free the whole fd: no node is left behind */
	KUNIT_ASSERT_EQ(test, s3fs_fd_store(fd, 0, &x, NULL, 1, 0), 0);
	KUNIT_EXPECT_EQ(test, atomic_read(&s3fs_resident_nodes), nodes0 + 1);
	s3fs_file_data_free(fd);
	KUNIT_EXPECT_EQ(test, atomic_read(&s3fs_resident_nodes), nodes0);
	KUNIT_EXPECT_FALSE(test, s3k_index_has(ino, NULL));
}

static void s3k_hit_delta_once(struct kunit *test)
{
	struct s3fs_file_data *fd = s3fs_file_data_new(S3K_INO_BASE, NULL, 0);
	struct s3fs_hit_rec rec = {};

	KUNIT_ASSERT_NOT_NULL(test, fd);
	/* First look, no populate base (arena mode): everything so far is "before". */
	s3k_hit_bump(fd, 5, 100);
	KUNIT_EXPECT_TRUE(test, s3fs_hit_collect_fd(fd, S3K_INO_BASE, &rec));
	KUNIT_EXPECT_TRUE(test, rec.first);
	KUNIT_EXPECT_EQ(test, rec.d_pre, 5ull);
	KUNIT_EXPECT_EQ(test, rec.d, 0ull);
	KUNIT_EXPECT_TRUE(test, fd->policy_seen);
	KUNIT_EXPECT_EQ(test, fd->hit_flushed, 5ull);
	/* Nothing new: nothing to replay. */
	KUNIT_EXPECT_FALSE(test, s3fs_hit_collect_fd(fd, S3K_INO_BASE, &rec));
	/* Counts between two looks are handed over once, as a difference. */
	s3k_hit_bump(fd, 2, 110);
	s3k_hit_bump(fd, 1, 120);
	KUNIT_EXPECT_TRUE(test, s3fs_hit_collect_fd(fd, S3K_INO_BASE, &rec));
	KUNIT_EXPECT_FALSE(test, rec.first);
	KUNIT_EXPECT_EQ(test, rec.d, 3ull);
	KUNIT_EXPECT_EQ(test, rec.last_tick, 120ull);
	KUNIT_EXPECT_FALSE(test, s3fs_hit_collect_fd(fd, S3K_INO_BASE, &rec));
	/* Evicted and forgotten: the accesses while it was out (the one that missed
	 * and the populate base) are "before"; the reads after the populate are hits. */
	fd->policy_seen = false;		/* s3fs_hit_forget */
	s3k_hit_bump(fd, 3, 125);		/* misses while not resident, the last one populates */
	fd->hit_base = READ_ONCE(fd->hit_flushed) + 3;
	fd->hit_base_set = true;		/* s3fs_hit_set_base at the populate */
	s3k_hit_bump(fd, 2, 126);		/* hits after the populate */
	KUNIT_EXPECT_TRUE(test, s3fs_hit_collect_fd(fd, S3K_INO_BASE, &rec));
	KUNIT_EXPECT_TRUE(test, rec.first);
	KUNIT_EXPECT_EQ(test, rec.d_pre, 3ull);
	KUNIT_EXPECT_EQ(test, rec.d, 2ull);
	KUNIT_EXPECT_FALSE(test, fd->hit_base_set);
	KUNIT_EXPECT_EQ(test, fd->hit_flushed, 13ull);	/* 5 + 3 (replayed before) + 3 + 2 */
	s3fs_file_data_free(fd);
}

static void s3k_hit_replay_order(struct kunit *test)
{
	struct s3fs_hit_rec recs[4] = {
		{ .ino = S3K_INO_BASE + 3, .d = 1, .last_tick = 20 },
		{ .ino = S3K_INO_BASE + 2, .d = 20, .last_tick = 10 },
		{ .ino = S3K_INO_BASE + 1, .d = 1, .last_tick = 20 },
		/* a new ino: 3 accesses before its populate, 2 hits after */
		{ .ino = S3K_INO_BASE + 4, .d = 2, .d_pre = 3, .last_tick = 30, .first = true },
	};
	const unsigned int old_max = s3fs_hit_replay_max;
	long before = atomic_long_read(&s3fs_hits_replayed_total);

	s3k_hit_n = 0;
	s3fs_hit_replay_max = 2;
	s3fs_hit_replay(recs, 4, S3FS_HIT_BEFORE_INSERT, s3k_hit_rec_on_hit);

	/* oldest last_tick first (capped at hit_replay_max calls), then the
	 * tied ticks by ino; the new ino's d_pre (3, cut to 2) last. */
	KUNIT_ASSERT_EQ(test, s3k_hit_n, 6u);
	KUNIT_EXPECT_EQ(test, s3k_hit_order[0], S3K_INO_BASE + 2);
	KUNIT_EXPECT_EQ(test, s3k_hit_order[1], S3K_INO_BASE + 2);
	KUNIT_EXPECT_EQ(test, s3k_hit_order[2], S3K_INO_BASE + 1);
	KUNIT_EXPECT_EQ(test, s3k_hit_order[3], S3K_INO_BASE + 3);
	KUNIT_EXPECT_EQ(test, s3k_hit_order[4], S3K_INO_BASE + 4);
	KUNIT_EXPECT_EQ(test, s3k_hit_order[5], S3K_INO_BASE + 4);

	/* after on_insert: only the new ino's d (2) */
	s3k_hit_n = 0;
	s3fs_hit_replay(recs, 4, S3FS_HIT_AFTER_INSERT, s3k_hit_rec_on_hit);
	s3fs_hit_replay_max = old_max;
	KUNIT_ASSERT_EQ(test, s3k_hit_n, 2u);
	KUNIT_EXPECT_EQ(test, s3k_hit_order[0], S3K_INO_BASE + 4);
	/* the counter is the delta before the cut: 1 + 20 + 1 + 3 (before) + 2 (after) */
	KUNIT_EXPECT_EQ(test, atomic_long_read(&s3fs_hits_replayed_total) - before, 27l);
}

/* -- entries_replace: an unchanged listing must not shrink the dcache ------- */

static void s3k_dyn_entries_equal(struct kunit *test)
{
	/* struct s3fs_entry_dyn carries a full name buffer: keep the arrays off the stack */
	struct s3fs_entry_dyn *a = kunit_kcalloc(test, 4, sizeof(*a), GFP_KERNEL);
	struct s3fs_entry_dyn *b = a + 2;

	KUNIT_ASSERT_NOT_NULL(test, a);
	a[0] = (struct s3fs_entry_dyn){ .ino = 10, .size = 46, .mode = S_IFREG | 0644, .name_len = 5 };
	memcpy(a[0].name, "a.txt", 5);
	a[1] = (struct s3fs_entry_dyn){ .ino = 11, .parent_ino = 7, .size = 9, .mode = S_IFREG | 0644, .name_len = 5 };
	memcpy(a[1].name, "b.txt", 5);
	memcpy(b, a, 2 * sizeof(*a));

	KUNIT_EXPECT_TRUE(test, dyn_entries_equal(a, 2, b, 2));
	KUNIT_EXPECT_FALSE(test, dyn_entries_equal(a, 2, b, 1));	/* a key removed */
	b[1].size = 10;							/* an object changed size */
	KUNIT_EXPECT_FALSE(test, dyn_entries_equal(a, 2, b, 2));
	b[1].size = 9;
	b[0].name[0] = 'x';						/* renamed */
	KUNIT_EXPECT_FALSE(test, dyn_entries_equal(a, 2, b, 2));
	b[0].name[0] = 'a';
	b[1].parent_ino = 8;						/* moved to another directory */
	KUNIT_EXPECT_FALSE(test, dyn_entries_equal(a, 2, b, 2));
	/* bytes past name_len do not count */
	b[1].parent_ino = 7;
	b[0].name[20] = 'z';
	KUNIT_EXPECT_TRUE(test, dyn_entries_equal(a, 2, b, 2));
}

/* -- policy bookkeeping: a miss in flight is not tracked yet ---------------- */

static void s3k_policy_trackable(struct kunit *test)
{
	const u64 ino = S3K_INO_BASE + 33;
	struct s3fs_pending *p;

	KUNIT_EXPECT_TRUE(test, s3fs_policy_trackable(ino));
	p = pending_get_or_create(ino, 0);
	KUNIT_ASSERT_NOT_NULL(test, p);
	/* A pending miss keeps the kmod from evicting the ino; a policy that picked it
	 * would already have moved it to its ghost / hand.  Phase 1 skips on_insert,
	 * on_size and the hit collect for it until the miss completes. */
	KUNIT_EXPECT_FALSE(test, s3fs_policy_trackable(ino));
	KUNIT_EXPECT_TRUE(test, s3fs_policy_trackable(ino + 1));	/* other inos unaffected */
	KUNIT_EXPECT_EQ(test, s3fs_ebpf_signal_admit(ino, 0, 0, 0), 0);
	KUNIT_EXPECT_TRUE(test, s3fs_policy_trackable(ino));
	pending_put(p);
}

static struct kunit_case s3k_data_cases[] = {
	KUNIT_CASE(s3k_populate_fills_chunk),
	KUNIT_CASE(s3k_chunk_populate_sizes),
	KUNIT_CASE(s3k_chunk_tail_populate),
	KUNIT_CASE(s3k_chunk_write_growth),
	KUNIT_CASE(s3k_chunk_sparse_write_zeroes_gap),
	KUNIT_CASE(s3k_chunk_truncate),
	KUNIT_CASE(s3k_chunk_limits_and_pins),
	KUNIT_CASE(s3k_chunk_initial_content),
	KUNIT_CASE(s3k_resident_index_follows_cap),
	KUNIT_CASE(s3k_mop_range),
	KUNIT_CASE(s3k_mop_needs_data),
	KUNIT_CASE(s3k_mpin_balanced),
	KUNIT_CASE(s3k_modify_ready_and_remote),
	KUNIT_CASE(s3k_dyn_entries_equal),
	KUNIT_CASE(s3k_policy_trackable),
	KUNIT_CASE(s3k_hit_delta_once),
	KUNIT_CASE(s3k_hit_replay_order),
	{}
};

static struct kunit_suite s3k_data_suite = {
	.name = "s3fs_ebpf_chunks",
	.test_cases = s3k_data_cases,
};

/* Opening /dev/s3fs_ebpf arms the read-miss upcall path (AC-43) and umount
 * disarms it; the gate is a plain flag now, not the daemon's mm. */
static void s3k_daemon_arm(struct kunit *test)
{
	const bool old = READ_ONCE(s3fs_daemon_armed);

	WRITE_ONCE(s3fs_daemon_armed, false);
	KUNIT_EXPECT_FALSE(test, READ_ONCE(s3fs_daemon_armed));
	s3fs_ebpf_daemon_arm();
	KUNIT_EXPECT_TRUE(test, READ_ONCE(s3fs_daemon_armed));
	s3fs_ebpf_daemon_arm();		/* idempotent */
	KUNIT_EXPECT_TRUE(test, READ_ONCE(s3fs_daemon_armed));
	WRITE_ONCE(s3fs_daemon_armed, old);
}

static struct kunit_case s3k_table_cases[] = {
	KUNIT_CASE(s3k_dyn_index),
	KUNIT_CASE(s3k_dyn_index_many),
	KUNIT_CASE(s3k_daemon_arm),
	KUNIT_CASE(s3k_hidden_entry_lifecycle),
	KUNIT_CASE(s3k_hidden_entry_revived),
	KUNIT_CASE(s3k_local_entry_promotion_needs_applied),
	KUNIT_CASE(s3k_event_seq_is_shared_and_increasing),
	KUNIT_CASE(s3k_pending_per_chunk),
	KUNIT_CASE(s3k_pending_verify_populated),
	KUNIT_CASE(s3k_miss_queue_fifo),
	KUNIT_CASE(s3k_flush_queue_f1),
	KUNIT_CASE(s3k_flush_wait_reopen),
	KUNIT_CASE(s3k_pending_concurrent),
	{}
};

static struct kunit_suite s3k_table_suite = {
	.name = "s3fs_ebpf_tables",
	.test_cases = s3k_table_cases,
};

/* sum of the per-CPU access counts of an fd */
static u64 s3k_hit_sum(struct s3fs_file_data *fd)
{
	u64 sum = 0;
	int cpu;

	for_each_possible_cpu(cpu)
		sum += per_cpu_ptr(fd->hit, cpu)->count;
	return sum;
}

/* .read_iter counts one access per open file (hit_rearm_jiffies = 0). */
static void s3k_vfs_hit_count(struct kunit *test)
{
	const u64 ino = S3K_INO_BASE + 31;
	const unsigned int old_rearm = s3fs_hit_rearm_jiffies;
	struct file *f, *g;
	struct s3fs_file_data *fd;
	char buf[8];
	loff_t pos;

	s3k_one_entry(test, ino, "hc", 64);
	f = s3k_open(test, "hc", O_RDONLY);
	fd = s3k_fd_of(f);
	s3fs_hit_rearm_jiffies = 0;

	/* read at EOF: no upcall, but .read_iter still counts */
	pos = 64;
	kernel_read(f, buf, sizeof(buf), &pos);
	KUNIT_EXPECT_EQ(test, s3k_hit_sum(fd), 1ull);
	pos = 64;
	kernel_read(f, buf, sizeof(buf), &pos);	/* the same open file: once */
	KUNIT_EXPECT_EQ(test, s3k_hit_sum(fd), 1ull);

	g = s3k_open(test, "hc", O_RDONLY);	/* a new open: another access */
	pos = 64;
	kernel_read(g, buf, sizeof(buf), &pos);
	KUNIT_EXPECT_EQ(test, s3k_hit_sum(fd), 2ull);

	s3fs_hit_rearm_jiffies = old_rearm;
	s3k_close(g);
	s3k_close(f);
}

/* AC-77: with a NO_ON_HIT policy the hit signal is not counted, but the read
 * pattern (miss events, IOC_STATS) still is. */
static void s3k_vfs_hit_count_skipped_no_on_hit(struct kunit *test)
{
	const u64 ino = S3K_INO_BASE + 32;
	const unsigned int old_rearm = s3fs_hit_rearm_jiffies;
	struct file *f;
	struct s3fs_file_data *fd;
	u64 calls0;
	char buf[8];
	loff_t pos = 64;
	u32 seq, rnd;

	s3k_one_entry(test, ino, "hn", 64);
	f = s3k_open(test, "hn", O_RDONLY);
	fd = s3k_fd_of(f);
	s3fs_hit_rearm_jiffies = 0;
	calls0 = s3fs_read_iter_calls();

	WRITE_ONCE(s3fs_policy_skip_hit, true);
	kernel_read(f, buf, sizeof(buf), &pos);
	WRITE_ONCE(s3fs_policy_skip_hit, false);
	KUNIT_EXPECT_EQ(test, s3k_hit_sum(fd), 0ull);
	KUNIT_EXPECT_EQ(test, s3fs_read_iter_calls() - calls0, 1ull);
	s3fs_fd_read_pattern(fd, &seq, &rnd);
	KUNIT_EXPECT_EQ(test, seq + rnd, 1u);

	pos = 64;			/* policy swapped to one with on_hit: counted again */
	kernel_read(f, buf, sizeof(buf), &pos);
	KUNIT_EXPECT_EQ(test, s3k_hit_sum(fd), 1ull);

	s3fs_hit_rearm_jiffies = old_rearm;
	s3k_close(f);
}

/* Every whole-ino eviction goes through admit_evict_ino (it needs idle chunks to free), which resets the
 * per-ino hit state (policy_seen) - the policy forgets the ino, so its next
 * populate is a first look again.  A populate while the policy still knows the
 * ino must not move the base. */
static void s3k_vfs_forget_on_evict(struct kunit *test)
{
	const u64 ino = S3K_INO_BASE + 32;
	struct s3fs_file_data *fd;
	struct file *f;

	s3k_one_entry(test, ino, "fe", 64);
	f = s3k_open(test, "fe", O_RDONLY);
	fd = s3k_fd_of(f);

	/* first look not yet taken: the populate records its base */
	KUNIT_ASSERT_EQ(test, s3k_populate(test, ino, 0, 'a', 64), 0);
	KUNIT_EXPECT_TRUE(test, fd->hit_base_set);

	/* the policy knows the ino: a further populate leaves the base alone */
	fd->hit_base_set = false;
	fd->policy_seen = true;
	KUNIT_ASSERT_EQ(test, s3k_populate(test, ino, 0, 'b', 64), 0);
	KUNIT_EXPECT_FALSE(test, fd->hit_base_set);

	/* evicted whole: forgotten */
	KUNIT_EXPECT_TRUE(test, admit_evict_ino(ino, NULL));
	KUNIT_EXPECT_FALSE(test, fd->policy_seen);

	s3k_close(f);
}


/* -- v2 (Phase 2 / 3): flush keeps its chunks, read pattern, capacity pass -- */

/* AC-82: after a successful flush the clean fd keeps what it just PUT, chunk by
 * chunk: the allocated, non-remote chunks are `populated` (a hole stays a hole)
 * and the inode is a capacity candidate again, so the next read is served from
 * the fd instead of upcalling the daemon (which no longer re-admits the PUT
 * bytes into an arena).  A failed flush keeps the data dirty and out of the
 * candidates. */
static bool s3k_is_resident(u64 ino, u64 *bytes)
{
	struct s3fs_resident *res;
	bool found = false;
	int n, i;

	n = s3fs_collect_resident(&res);
	for (i = 0; i < n; i++)
		if (res[i].ino == ino) {
			found = true;
			if (bytes)
				*bytes = res[i].bytes;
		}
	kfree(res);
	return found;
}

static void s3k_vfs_signal_flush_keeps_chunks(struct kunit *test)
{
	const u64 C = S3FS_CHUNK_SIZE;
	struct s3fs_flush_event *ev = kunit_kzalloc(test, sizeof(*ev), GFP_KERNEL);
	struct file *f = s3k_open(test, "hello.txt", O_RDWR);
	u64 ino = file_inode(f)->i_ino, bytes = 0;
	struct s3fs_file_data *fd = s3k_fd_of(f);

	KUNIT_ASSERT_NOT_NULL(test, ev);

	/* chunk 0 (the bootstrap content, populated), a hole at chunk 1, and a
	 * new chunk 2 */
	KUNIT_ASSERT_EQ(test, s3k_write(test, f, 2 * C, "x", 1), 1L);
	KUNIT_EXPECT_FALSE(test, s3k_is_resident(ino, NULL));	/* dirty: not a candidate */
	KUNIT_ASSERT_EQ(test, s3fs_maybe_enqueue_flush(f, false), 0);
	KUNIT_ASSERT_EQ(test, s3k_pop_flush(ev), 0);
	mutex_lock(&fd->lock);
	fd->flush_read_seq = fd->flush_seq;	/* the daemon read the buffer */
	mutex_unlock(&fd->lock);
	KUNIT_EXPECT_FALSE(test, fd->chunks[2].populated);

	/* PUT failed: the daemon marks it dirty again; nothing is kept as clean */
	KUNIT_EXPECT_EQ(test, s3fs_ebpf_signal_flush(ino, -EIO), -ENOENT);
	KUNIT_ASSERT_EQ(test, s3fs_ebpf_mark_dirty(ino), 0);
	KUNIT_EXPECT_FALSE(test, fd->chunks[2].populated);
	KUNIT_EXPECT_FALSE(test, s3k_is_resident(ino, NULL));

	/* the retry's PUT succeeded */
	KUNIT_ASSERT_EQ(test, s3fs_maybe_enqueue_flush(f, false), 0);
	KUNIT_ASSERT_EQ(test, s3k_pop_flush(ev), 0);
	mutex_lock(&fd->lock);
	fd->flush_read_seq = fd->flush_seq;
	mutex_unlock(&fd->lock);
	KUNIT_EXPECT_EQ(test, s3fs_ebpf_signal_flush(ino, 0), -ENOENT);

	KUNIT_EXPECT_TRUE(test, fd->chunks[0].populated);
	KUNIT_EXPECT_NULL(test, fd->chunks[1].p);		/* the hole */
	KUNIT_EXPECT_FALSE(test, fd->chunks[1].populated);
	KUNIT_EXPECT_NOT_NULL(test, fd->chunks[2].p);
	KUNIT_EXPECT_TRUE(test, fd->chunks[2].populated);
	KUNIT_EXPECT_TRUE(test, s3k_is_resident(ino, &bytes));	/* a candidate again */
	KUNIT_EXPECT_EQ(test, bytes, (u64)fd->cap);
	KUNIT_EXPECT_GT(test, bytes, 0ull);

	s3k_close(f);
}

/* .read_iter counts the access pattern per fd (sequential when a read starts
 * within 4 KiB of the previous end, else random), hands it to the daemon in
 * the miss event, and keeps the .read_iter call counters (IOC_STATS 3 / 4). */
static void s3k_vfs_read_pattern(struct kunit *test)
{
	static const loff_t at[] = { 0, 4096, 12288, 49152, 0 };
	const u64 ino = S3K_INO_BASE + 41, n = 64 * 1024;
	struct s3fs_read_miss_event ev;
	struct s3fs_file_data *fd;
	u64 calls0 = s3fs_read_iter_calls(), pos0_0 = s3fs_read_iter_pos0();
	char *buf = kunit_kzalloc(test, 4096, GFP_KERNEL);
	struct file *f;
	u32 seq, rnd;
	loff_t pos;
	int i;

	KUNIT_ASSERT_NOT_NULL(test, buf);
	s3k_one_entry(test, ino, "rp", n);
	f = s3k_open(test, "rp", O_RDONLY);
	fd = s3k_fd_of(f);
	KUNIT_ASSERT_EQ(test, s3k_populate(test, ino, 0, 'r', n), 0);

	/* the pattern state is per CPU: stay on one */
	migrate_disable();
	for (i = 0; i < ARRAY_SIZE(at); i++) {
		pos = at[i];
		KUNIT_EXPECT_EQ(test, kernel_read(f, buf, 4096, &pos),
				(ssize_t)4096);
	}
	migrate_enable();

	/* 0 (first read), 4096 (contiguous), 12288 (a 4 KiB gap is within the
	 * slack) are sequential; 49152 (a jump) and 0 (backwards) are random */
	s3fs_fd_read_pattern(fd, &seq, &rnd);
	KUNIT_EXPECT_EQ(test, seq, 3u);
	KUNIT_EXPECT_EQ(test, rnd, 2u);
	KUNIT_EXPECT_EQ(test, s3fs_read_iter_calls() - calls0, (u64)ARRAY_SIZE(at));
	KUNIT_EXPECT_EQ(test, s3fs_read_iter_pos0() - pos0_0, 2ull);	/* the two reads at 0 */

	/* the miss event of a read carries it; one that is not a read does not */
	while (s3fs_read_miss_dequeue(&ev, true) > 0)
		;
	KUNIT_ASSERT_EQ(test, miss_enqueue(ino, 0, 4096, 0, fd), 0);
	KUNIT_ASSERT_EQ(test, s3fs_read_miss_dequeue(&ev, true), (ssize_t)sizeof(ev));
	KUNIT_EXPECT_EQ(test, ev.ino, ino);
	KUNIT_EXPECT_EQ(test, ev.seq_count, 3u);
	KUNIT_EXPECT_EQ(test, ev.rand_count, 2u);
	KUNIT_ASSERT_EQ(test, miss_enqueue(ino, 0, 0, S3FS_MISS_FLAG_OPEN_CHECK, NULL), 0);
	KUNIT_ASSERT_EQ(test, s3fs_read_miss_dequeue(&ev, true), (ssize_t)sizeof(ev));
	KUNIT_EXPECT_EQ(test, ev.seq_count, 0u);
	KUNIT_EXPECT_EQ(test, ev.rand_count, 0u);

	s3k_close(f);
}

/* The capacity pass (s3fs_ebpf_capacity_pass, the EVAL ioctl): nothing is
 * evicted without a capacity (and without a policy vote), and with a capacity
 * for one of two equally sized files it evicts exactly one - chunks freed,
 * evictions_total + 1. */
static void s3k_vfs_capacity_pass_evicts_to_capacity(struct kunit *test)
{
	const u64 a = S3K_INO_BASE + 51, b = S3K_INO_BASE + 52, n = 64 * 1024;
	const u64 old_cap = s3fs_capacity_bytes;
	struct s3fs_entry_dyn *e = kvcalloc(2, sizeof(*e), GFP_KERNEL);
	long ev0 = atomic_long_read(&s3fs_evictions_total);
	struct file *fa, *fb;

	KUNIT_ASSERT_NOT_NULL(test, e);
	s3k_dyn_set(&e[0], a, S3FS_ROOT_INO, "ca");
	e[0].size = n;
	s3k_dyn_set(&e[1], b, S3FS_ROOT_INO, "cb");
	e[1].size = n;
	KUNIT_ASSERT_EQ(test, s3fs_ebpf_entries_replace(e, 2), 0);
	fa = s3k_open(test, "ca", O_RDONLY);
	fb = s3k_open(test, "cb", O_RDONLY);
	KUNIT_ASSERT_EQ(test, s3k_populate(test, a, 0, 'a', n), 0);
	KUNIT_ASSERT_EQ(test, s3k_populate(test, b, 0, 'b', n), 0);
	KUNIT_ASSERT_TRUE(test, s3k_is_resident(a, NULL) && s3k_is_resident(b, NULL));

	s3fs_capacity_bytes = 0;
	KUNIT_EXPECT_EQ(test, s3fs_ebpf_capacity_pass(), 0);
	KUNIT_EXPECT_TRUE(test, s3k_is_resident(a, NULL) && s3k_is_resident(b, NULL));
	KUNIT_EXPECT_EQ(test, atomic_long_read(&s3fs_evictions_total), ev0);

	s3fs_capacity_bytes = n;	/* room for one of the two */
	KUNIT_EXPECT_EQ(test, s3fs_ebpf_capacity_pass(), 0);
	s3fs_capacity_bytes = old_cap;
	KUNIT_EXPECT_NE(test, s3k_is_resident(a, NULL), s3k_is_resident(b, NULL));	/* exactly one left */
	KUNIT_EXPECT_EQ(test, atomic_long_read(&s3fs_evictions_total), ev0 + 1);
	KUNIT_EXPECT_EQ(test, s3fs_ino_buf_bytes(a) + s3fs_ino_buf_bytes(b), n);

	s3k_close(fb);
	s3k_close(fa);
}

/* v2 Phase 3e: the capacity pass runs at the end of POPULATE_PAGES (the daemon no
 * longer asks for it with an extra ioctl).  In capacity mode the populate that
 * takes the cache over capacity evicts down to it before it returns; without a
 * capacity nothing is evicted; and while a pass is already running a populate only
 * asks for one more round (coalescing) instead of running a second pass. */
static void s3k_vfs_populate_runs_capacity_pass(struct kunit *test)
{
	const u64 a = S3K_INO_BASE + 71, b = S3K_INO_BASE + 72, c = S3K_INO_BASE + 73;
	const u64 n = 64 * 1024, old_cap = s3fs_capacity_bytes;
	struct s3fs_entry_dyn *e = kvcalloc(3, sizeof(*e), GFP_KERNEL);
	long ev0 = atomic_long_read(&s3fs_evictions_total);
	struct file *fa, *fb, *fc;

	KUNIT_ASSERT_NOT_NULL(test, e);
	s3k_dyn_set(&e[0], a, S3FS_ROOT_INO, "pa");
	e[0].size = n;
	s3k_dyn_set(&e[1], b, S3FS_ROOT_INO, "pb");
	e[1].size = n;
	s3k_dyn_set(&e[2], c, S3FS_ROOT_INO, "pc");
	e[2].size = n;
	KUNIT_ASSERT_EQ(test, s3fs_ebpf_entries_replace(e, 3), 0);
	fa = s3k_open(test, "pa", O_RDONLY);
	fb = s3k_open(test, "pb", O_RDONLY);
	fc = s3k_open(test, "pc", O_RDONLY);

	/* no capacity: both populates stay */
	s3fs_capacity_bytes = 0;
	KUNIT_ASSERT_EQ(test, s3k_populate(test, a, 0, 'a', n), 0);
	KUNIT_ASSERT_EQ(test, s3k_populate(test, b, 0, 'b', n), 0);
	KUNIT_EXPECT_TRUE(test, s3k_is_resident(a, NULL) && s3k_is_resident(b, NULL));
	KUNIT_EXPECT_EQ(test, atomic_long_read(&s3fs_evictions_total), ev0);

	/* room for two: the third populate evicts exactly one before it returns */
	s3fs_capacity_bytes = 2 * n;
	KUNIT_ASSERT_EQ(test, s3k_populate(test, c, 0, 'c', n), 0);
	KUNIT_EXPECT_EQ(test, atomic_long_read(&s3fs_evictions_total), ev0 + 1);
	KUNIT_EXPECT_EQ(test, s3fs_ino_buf_bytes(a) + s3fs_ino_buf_bytes(b) + s3fs_ino_buf_bytes(c), 2 * n);

	/* a pass is running: the populate only asks for another round */
	s3fs_capacity_bytes = n;
	atomic_set(&s3fs_pass_running, 1);
	atomic_set(&s3fs_pass_again, 0);
	s3fs_ebpf_capacity_pass_after_populate();
	KUNIT_EXPECT_EQ(test, atomic_read(&s3fs_pass_again), 1);
	KUNIT_EXPECT_EQ(test, atomic_long_read(&s3fs_evictions_total), ev0 + 1);	/* no pass ran */
	atomic_set(&s3fs_pass_running, 0);
	atomic_set(&s3fs_pass_again, 0);
	/* and once nothing runs, it runs the pass and leaves the flags clear */
	s3fs_ebpf_capacity_pass_after_populate();
	KUNIT_EXPECT_EQ(test, atomic_long_read(&s3fs_evictions_total), ev0 + 2);
	KUNIT_EXPECT_EQ(test, atomic_read(&s3fs_pass_running), 0);
	KUNIT_EXPECT_EQ(test, atomic_read(&s3fs_pass_again), 0);
	s3fs_capacity_bytes = old_cap;

	s3k_close(fc);
	s3k_close(fb);
	s3k_close(fa);
}

/* U8: a reader pins its chunk BEFORE it creates the pending - possibly before
 * the chunk is allocated.  s3fs_file_data_reset (a whole-ino eviction) must
 * count that pin: it frees the idle chunks around it, keeps the table, and
 * reports nothing freed when the pinned chunk was the only candidate.  Dropping
 * the table lost the pin, and the chunk the daemon then populated was evicted
 * between its wake-up and the reader's re-check (evict-race read_err). */
static void s3k_vfs_reset_counts_unallocated_pin(struct kunit *test)
{
	const u64 ino = S3K_INO_BASE + 61, n = 64 * 1024;
	struct s3fs_file_data *fd;
	struct file *f;
	long skipped0 = atomic_long_read(&s3fs_reset_skipped);

	s3k_one_entry(test, ino, "rp2", n);
	f = s3k_open(test, "rp2", O_RDONLY);
	fd = s3k_fd_of(f);
	KUNIT_ASSERT_EQ(test, s3k_populate(test, ino, 0, 'q', n), 0);	/* chunk 0: idle */

	/* a reader pinned chunk 2, which is not allocated yet */
	mutex_lock(&fd->lock);
	KUNIT_ASSERT_TRUE(test, s3fs_fd_chunk_pin(fd, 2));
	KUNIT_ASSERT_NULL(test, fd->chunks[2].p);
	mutex_unlock(&fd->lock);

	KUNIT_EXPECT_TRUE(test, s3fs_file_data_reset(ino));	/* chunk 0 went */

	mutex_lock(&fd->lock);
	KUNIT_EXPECT_NULL(test, fd->chunks[0].p);
	KUNIT_EXPECT_GT(test, fd->nr_chunks, 2u);		/* the table is kept ... */
	KUNIT_EXPECT_EQ(test, fd->chunks[2].readers, 1u);	/* ... with the pin */
	mutex_unlock(&fd->lock);
	KUNIT_EXPECT_EQ(test, atomic_long_read(&s3fs_reset_skipped), skipped0 + 1);

	/* nothing idle left: nothing freed, the pin still stands */
	KUNIT_EXPECT_FALSE(test, s3fs_file_data_reset(ino));
	mutex_lock(&fd->lock);
	KUNIT_EXPECT_EQ(test, fd->chunks[2].readers, 1u);
	s3fs_fd_chunk_unpin(fd, 2);
	mutex_unlock(&fd->lock);

	/* unpinned, an empty fd frees its table */
	KUNIT_EXPECT_FALSE(test, s3fs_file_data_reset(ino));
	mutex_lock(&fd->lock);
	KUNIT_EXPECT_EQ(test, fd->nr_chunks, 0u);
	mutex_unlock(&fd->lock);

	s3k_close(f);
}

static struct kunit_case s3k_vfs_cases[] = {
	KUNIT_CASE(s3k_vfs_read_bootstrap),
	KUNIT_CASE(s3k_vfs_maxbytes),
	KUNIT_CASE(s3k_vfs_read_past_2g),
	KUNIT_CASE(s3k_vfs_populate_mid_chunk),
	KUNIT_CASE(s3k_vfs_hit_count),
	KUNIT_CASE(s3k_vfs_hit_count_skipped_no_on_hit),
	KUNIT_CASE(s3k_vfs_read_pattern),
	KUNIT_CASE(s3k_vfs_reset_counts_unallocated_pin),
	KUNIT_CASE(s3k_vfs_signal_flush_keeps_chunks),
	KUNIT_CASE(s3k_vfs_capacity_pass_evicts_to_capacity),
	KUNIT_CASE(s3k_vfs_populate_runs_capacity_pass),
	KUNIT_CASE(s3k_vfs_forget_on_evict),
	KUNIT_CASE(s3k_vfs_write_read_back),
	KUNIT_CASE(s3k_vfs_truncate),
	KUNIT_CASE(s3k_vfs_close_flush),
	KUNIT_CASE(s3k_vfs_fsync_waits_for_its_put),
	KUNIT_CASE(s3k_vfs_fsync_empty_file),
	KUNIT_CASE(s3k_vfs_read_dirty_short_buffer_claims_nothing),
	KUNIT_CASE(s3k_vfs_fill_past_dirty_eof),
	KUNIT_CASE(s3k_vfs_remount_fsync_queue),
	KUNIT_CASE(s3k_vfs_concurrent_modify),
	{}
};

static struct kunit_suite s3k_vfs_suite = {
	.name = "s3fs_ebpf_vfs",
	.init = s3k_vfs_init,
	.exit = s3k_vfs_exit,
	.test_cases = s3k_vfs_cases,
};

kunit_test_suites(&s3k_data_suite, &s3k_table_suite, &s3k_vfs_suite);
