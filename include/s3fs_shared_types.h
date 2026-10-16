/* SPDX-License-Identifier: (LGPL-2.1 OR BSD-2-Clause) */
/*
 * s3fs-ebpf: type definitions shared across kernel module, eBPF program,
 * and Go daemon.
 *
 * The layout MUST stay stable — Go mirrors these via cgo or hand-rolled
 * binary.Read. Bump S3FS_SHARED_ABI_VERSION on any layout change.
 */

#ifndef _S3FS_EBPF_SHARED_TYPES_H
#define _S3FS_EBPF_SHARED_TYPES_H

#define S3FS_SHARED_ABI_VERSION 1

#define S3FS_MAX_PATH 4096
#define S3FS_MAX_NAME 255

#if defined(__KERNEL__)
#include <linux/types.h>
#elif defined(__bpf__) || defined(__BPF__)
/* Types come from vmlinux.h in the BPF translation unit. */
#else
#include <stdint.h>
typedef uint64_t __u64;
typedef uint32_t __u32;
typedef uint16_t __u16;
#endif

/* Mirror of struct stat we cache in arena. */
struct s3fs_stat {
	__u64 ino;
	__u64 size;
	__u64 blocks;
	__u64 atime_ns;
	__u64 mtime_ns;
	__u64 ctime_ns;
	__u32 mode;
	__u32 nlink;
	__u32 uid;
	__u32 gid;
};

/* Slow-path event pushed via BPF_MAP_TYPE_RINGBUF to the Go daemon.
 *
 * Week 2 intentionally omits the resolved path: reconstructing it in a
 * BPF program requires a bounded d_path()-style walk, which we defer to
 * Week 3 once the kernel module owns the inode table and can hand us a
 * stable key. For now the daemon looks up ino against its own S3-key
 * index.
 */
struct s3fs_miss_event {
	__u64 ino;
	__u32 op;               /* see enum s3fs_miss_op */
	__u32 _pad;
};

enum s3fs_miss_op {
	S3FS_OP_GETATTR = 0,
	S3FS_OP_LOOKUP  = 1,
	S3FS_OP_READ    = 2,
};

/* Per-CPU counter slots (indices into s3fs_counters map). */
enum s3fs_stat_slot {
	S3FS_STAT_FAST_HITS             = 0,
	S3FS_STAT_SLOW_MISSES           = 1,
	S3FS_STAT_UNKNOWN_FS            = 2,
	S3FS_STAT_READ_ITER_HITS        = 3, /* generic_file_read_iter: ino in index (all reads) */
	S3FS_STAT_READ_ITER_MISSES      = 4, /* generic_file_read_iter: ino not in index */
	S3FS_STAT_READ_ITER_HITS_POS0   = 5, /* read_iter hit where ki_pos==0 (first read per open); Y-06 */
	S3FS_STAT_SLOT_MAX,
};

/*
 * AC-47: per-CPU, direct-mapped slot of what the read_iter fentry knows
 * about one inode.  The hook runs on every read of the file system (and
 * the s_magic filter on every generic_file_read_iter in the system), so
 * its per-read cost is the main gap to FUSE in the 64-job read bench; one
 * array index replaces the two hash lookups (access pattern, inode_index)
 * and a kfunc call per read.
 *
 * Slot = s3fs_slot_index(ino).  A slot belongs to `ino`; a read of another
 * inode that maps to it takes the slot over and starts from zero (the
 * pattern of the evicted inode is lost — it is an approximate signal).
 * The daemon reads the slots of all CPUs and merges those owned by the
 * inode (ReadAccessPattern).
 */
#define S3FS_SLOT_BITS	10
#define S3FS_SLOT_COUNT	(1u << S3FS_SLOT_BITS)

/* The slot holds a cached "ino is in s3fs_inode_index" answer. */
#define S3FS_SLOT_IN_INDEX	0x1u

struct s3fs_ino_slot {
	__u64 ino;           /* owner; 0 = empty */
	__u64 last_end_pos;  /* end offset of the previous read (pos + len) */
	__u64 total_bytes;   /* cumulative bytes requested via read_iter */
	__u64 last_policy_tick;/* jiffies when on_hit was last forwarded to the policy */
	__u32 seq_count;     /* number of sequential reads observed */
	__u32 rand_count;    /* number of random reads observed */
	__u32 reads;         /* read_iter calls since the slot was claimed */
	__u32 flags;         /* S3FS_SLOT_* */
};

/* Slot of an inode.  Must stay identical to slotIndex in
 * daemon/internal/ebpf/loader.go. */
static inline __u32 s3fs_slot_index(__u64 ino)
{
	return (__u32)((ino * 0x9E3779B97F4A7C15ULL) >> (64 - S3FS_SLOT_BITS));
}

/* Magic identifying an s3fs_ebpf super_block. Kept in sync with
 * kmod/vfs_ops.c so the BPF fentry hook can filter by it. */
#define S3FS_EBPF_MAGIC 0x53334642UL  /* "S3FB" */

/*
 * Context passed via BPF_PROG_TEST_RUN to the SEC("syscall") test harness.
 * Week 1 laid this out for an mmap-array; Week 2 promotes it to a real
 * bpf_arena: the daemon mmaps the arena, writes stubs at byte offsets, and
 * passes `arena_base_uaddr` = mmap base so the BPF program can reconstruct
 * arena pointers (`__arena *`) via a scalar-to-address_space cast.
 *
 * Layout is padded to 8-byte boundaries; `_pad` keeps sizeof stable at 32.
 */
struct s3fs_test_args {
	__u64 arena_base_uaddr;
	__u32 idx;
	__u32 kfunc_arg;
	__u64 out_ino;
	__u32 out_kfunc;
	__u32 _pad;
};

#endif /* _S3FS_EBPF_SHARED_TYPES_H */
