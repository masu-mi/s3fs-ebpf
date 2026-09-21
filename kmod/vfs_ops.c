// SPDX-License-Identifier: GPL-2.0
/*
 * s3fs-ebpf: minimal VFS operations for the skeleton mount.
 * Populates only what is required so that `mount -t s3fs_ebpf` returns a
 * root inode we can `ls` (empty directory). Real read/lookup logic lands
 * in Week 1+ alongside the KFunc traversal path.
 */

#include <linux/fs.h>
#include <linux/fs_context.h>
#include <linux/pagemap.h>
#include <linux/module.h>
#include <linux/statfs.h>
#include <linux/version.h>

#include "vfs_ops.h"

/* S3FS_EBPF_MAGIC lives in the shared header so the BPF fentry hook can
 * filter by super_block::s_magic without a second source of truth. */
#include "s3fs_shared_types.h"

static const struct super_operations s3fs_ebpf_super_ops = {
	.statfs		= simple_statfs,
	.drop_inode	= generic_delete_inode,
};

static const struct inode_operations s3fs_ebpf_dir_iops = {
	.lookup		= simple_lookup,
};

static const struct file_operations s3fs_ebpf_dir_fops = {
	.open		= dcache_dir_open,
	.release	= dcache_dir_close,
	.llseek		= dcache_dir_lseek,
	.read		= generic_read_dir,
	.iterate_shared	= dcache_readdir,
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

	root->i_ino = 1;
	root->i_mode = S_IFDIR | 0755;
#if LINUX_VERSION_CODE >= KERNEL_VERSION(6, 7, 0)
	simple_inode_init_ts(root);
#else
	root->i_atime = root->i_mtime = root->i_ctime = current_time(root);
#endif
	root->i_op = &s3fs_ebpf_dir_iops;
	root->i_fop = &s3fs_ebpf_dir_fops;
	set_nlink(root, 2);

	sb->s_root = d_make_root(root);
	if (!sb->s_root)
		return -ENOMEM;

	return 0;
}
