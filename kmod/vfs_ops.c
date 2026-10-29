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
#include <linux/uio.h>
#include <linux/version.h>
#include <linux/wait.h>

#include "vfs_ops.h"
#include "policy_struct_ops.h"

/* S3FS_EBPF_MAGIC lives in the shared header so the BPF fentry hook can
 * filter by super_block::s_magic without a second source of truth. */
#include "s3fs_shared_types.h"

/*
 * The implementation is split by concern under kmod/vfs/.  The parts share
 * file-static state and are included in dependency order, so this stays one
 * translation unit: reordering them changes forward-declaration needs.
 */
#include "vfs/entries.inc.c"
#include "vfs/admit_table.inc.c"
#include "vfs/capacity_evict.inc.c"
#include "vfs/read_miss.inc.c"
#include "vfs/attr_override.inc.c"
#include "vfs/fd_chunks.inc.c"
#include "vfs/hit_accum.inc.c"
#include "vfs/fd_reset.inc.c"
#include "vfs/flush_queue.inc.c"
#include "vfs/delete_rename_queue.inc.c"
#include "vfs/dirty_populate.inc.c"
#include "vfs/file_ops.inc.c"
#include "vfs/aops_read.inc.c"
#include "vfs/dir_super_ops.inc.c"


#ifdef S3FS_KUNIT
#include "vfs_ops_kunit.c"
#endif
