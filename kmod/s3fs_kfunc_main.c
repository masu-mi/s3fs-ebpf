// SPDX-License-Identifier: GPL-2.0
/*
 * s3fs-ebpf: skeleton VFS + KFunc kernel module.
 *
 * Registers a `file_system_type` and a placeholder KFunc so the userspace
 * daemon can `mount -t s3fs_ebpf none <mp>` and later push a bpf_arena
 * map id to us. Read/lookup logic is intentionally empty for Week 0.
 */

#include <linux/fs.h>
#include <linux/fs_context.h>
#include <linux/init.h>
#include <linux/module.h>
#include <linux/version.h>

#include "policy_struct_ops.h"
#include "s3fs_ebpf_dev.h"
#include "tree_traversal.h"
#include "vfs_ops.h"

static int s3fs_ebpf_get_tree(struct fs_context *fc)
{
	return get_tree_nodev(fc, s3fs_ebpf_fill_super);
}

static const struct fs_context_operations s3fs_ebpf_ctx_ops = {
	.get_tree = s3fs_ebpf_get_tree,
};

static int s3fs_ebpf_init_fs_context(struct fs_context *fc)
{
	fc->ops = &s3fs_ebpf_ctx_ops;
	return 0;
}

static struct file_system_type s3fs_ebpf_fs_type = {
	.owner			= THIS_MODULE,
	.name			= "s3fs_ebpf",
	.init_fs_context	= s3fs_ebpf_init_fs_context,
	.kill_sb		= kill_anon_super,
	.fs_flags		= 0,
};

static int __init s3fs_ebpf_init(void)
{
	int ret;

	ret = s3fs_kfunc_register();
	if (ret) {
		pr_err("s3fs_ebpf: failed to register kfunc set: %d\n", ret);
		return ret;
	}

	ret = register_filesystem(&s3fs_ebpf_fs_type);
	if (ret) {
		pr_err("s3fs_ebpf: register_filesystem failed: %d\n", ret);
		s3fs_kfunc_unregister();
		return ret;
	}

	ret = s3fs_ebpf_dev_register();
	if (ret) {
		unregister_filesystem(&s3fs_ebpf_fs_type);
		s3fs_kfunc_unregister();
		return ret;
	}

	ret = s3fs_policy_struct_ops_register();
	if (ret) {
		pr_warn("s3fs_ebpf: struct_ops register failed: %d (fmod_ret path still works)\n", ret);
		/* Non-fatal — fmod_ret path is available independently. */
	}

	pr_info("s3fs_ebpf: loaded (skeleton)\n");
	return 0;
}

static void __exit s3fs_ebpf_exit(void)
{
	s3fs_ebpf_dev_unregister();
	unregister_filesystem(&s3fs_ebpf_fs_type);
	s3fs_kfunc_unregister();
	pr_info("s3fs_ebpf: unloaded\n");
}

module_init(s3fs_ebpf_init);
module_exit(s3fs_ebpf_exit);

MODULE_LICENSE("GPL");
MODULE_AUTHOR("bbsakura");
MODULE_DESCRIPTION("s3fs-ebpf: VFS + KFunc skeleton for eBPF-driven S3 filesystem");
MODULE_VERSION("0.0.1");
/*
 * bpf_map_get() lives in the BPF_INTERNAL namespace on 6.12; opt in.
 * 6.13 turned the namespace argument into a string literal (commit
 * cdd30ebb1b9f), so the bare identifier no longer compiles there.
 */
#if LINUX_VERSION_CODE >= KERNEL_VERSION(6, 13, 0)
MODULE_IMPORT_NS("BPF_INTERNAL");
#else
MODULE_IMPORT_NS(BPF_INTERNAL);
#endif
MODULE_ALIAS_FS("s3fs_ebpf");
