// SPDX-License-Identifier: GPL-2.0
/*
 * s3fs-ebpf: /dev/s3fs_ebpf miscdevice.
 *
 * Week 4 (a) control channel between the daemon and the kernel module.
 * The daemon owns the BPF arena (creates it via cilium/ebpf, holds the
 * fd) and needs to hand kmod the arena's kernel-VM base so subsequent
 * VFS reads can copy_to_user() directly out of arena pages without a
 * userspace bounce.
 *
 * The kernel exports bpf_map_get(fd) but neither
 * bpf_arena_get_kern_vm_start() nor struct bpf_arena itself. To keep the
 * PoC on a stock 6.12 kernel we mirror the arena layout (recovered from
 * vmlinux BTF) and container_of() back to it. This is fragile across
 * kernel bumps and is called out as such in the Meetup slides.
 */

#include <linux/bpf.h>
#include <linux/fs.h>
#include <linux/miscdevice.h>
#include <linux/module.h>
#include <linux/uaccess.h>
#include <linux/version.h>
#include <linux/vmalloc.h>

#include "s3fs_ebpf_dev.h"
#include "s3fs_ebpf_uapi.h"

/*
 * BTF-derived layout of struct bpf_arena on Linux 6.12. The upstream
 * definition lives in kernel/bpf/arena.c and is not exported via any
 * public header; if this struct drifts we lose the container_of() path
 * and readers will silently observe garbage. Guard against the kernel
 * version we actually built against.
 */
#if LINUX_VERSION_CODE < KERNEL_VERSION(6, 12, 0)
#error "s3fs_ebpf_dev.c: struct bpf_arena layout is only verified for 6.12+"
#endif

struct bpf_arena_layout_6_12 {
	struct bpf_map map;
	u64 user_vm_start;
	u64 user_vm_end;
	struct vm_struct *kern_vm;
	/* trailing fields (maple_tree, list_head, mutex) omitted — we
	 * never touch them. sizeof mismatch is fine as long as we only
	 * read fields at their fixed offsets. */
};

static struct bpf_map *s3fs_dev_get_arena_map(u32 fd)
{
	struct bpf_map *map = bpf_map_get(fd);

	if (IS_ERR(map))
		return map;
	if (map->map_type != BPF_MAP_TYPE_ARENA) {
		bpf_map_put(map);
		return ERR_PTR(-EINVAL);
	}
	return map;
}

static long s3fs_dev_probe_kvbase(void __user *uarg)
{
	struct s3fs_probe_kvbase arg;
	struct bpf_map *map;
	struct bpf_arena_layout_6_12 *arena;
	u64 kbase;
	u64 size;

	if (copy_from_user(&arg, uarg, sizeof(arg)))
		return -EFAULT;

	map = s3fs_dev_get_arena_map(arg.arena_fd);
	if (IS_ERR(map))
		return PTR_ERR(map);

	arena = container_of(map, struct bpf_arena_layout_6_12, map);
	if (!arena->kern_vm) {
		bpf_map_put(map);
		return -ENODATA;
	}
	kbase = (u64)(unsigned long)arena->kern_vm->addr;
	size  = arena->kern_vm->size;

	bpf_map_put(map);

	arg.out_kvbase = kbase;
	arg.out_size   = size;
	if (copy_to_user(uarg, &arg, sizeof(arg)))
		return -EFAULT;

	pr_info("s3fs_ebpf: probe_kvbase fd=%u user_vm_start=0x%llx kern_vm=0x%llx size=%llu\n",
		arg.arena_fd, arena->user_vm_start, kbase, size);
	return 0;
}

static long s3fs_dev_ioctl(struct file *filp, unsigned int cmd, unsigned long arg)
{
	void __user *uarg = (void __user *)arg;

	switch (cmd) {
	case S3FS_IOC_PROBE_KVBASE:
		return s3fs_dev_probe_kvbase(uarg);
	default:
		return -ENOTTY;
	}
}

static const struct file_operations s3fs_ebpf_dev_fops = {
	.owner		= THIS_MODULE,
	.unlocked_ioctl	= s3fs_dev_ioctl,
	.compat_ioctl	= s3fs_dev_ioctl,
	.llseek		= noop_llseek,
};

static struct miscdevice s3fs_ebpf_misc = {
	.minor	= MISC_DYNAMIC_MINOR,
	.name	= S3FS_EBPF_DEV_NAME,
	.fops	= &s3fs_ebpf_dev_fops,
	.mode	= 0600,
};

int s3fs_ebpf_dev_register(void)
{
	int ret = misc_register(&s3fs_ebpf_misc);

	if (ret)
		pr_err("s3fs_ebpf: misc_register failed: %d\n", ret);
	return ret;
}

void s3fs_ebpf_dev_unregister(void)
{
	misc_deregister(&s3fs_ebpf_misc);
}
