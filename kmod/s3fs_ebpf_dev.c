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
#include <linux/mm.h>
#include <linux/mm_types.h>
#include <linux/module.h>
#include <linux/poll.h>
#include <linux/sched/mm.h>
#include <linux/uaccess.h>
#include <linux/version.h>
#include <linux/vmalloc.h>

#include "s3fs_ebpf_dev.h"
#include "s3fs_ebpf_uapi.h"
#include "vfs_ops.h"

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

/*
 * S3FS_IOC_ADMIT_ARENA handler.
 *
 * count==0 clears kmod's admit table (dropping the previously held
 * mm reference). Otherwise: grab a reference on the caller's mm
 * (daemon's mm — the caller is the ioctl syscall in daemon context),
 * copy the entry array from userspace, hand both off to the vfs-side
 * admit table. Reference lifecycle: on success the mm reference
 * passes to the admit table (dropped by a future count==0 admit or by
 * module unload). On failure we drop it here.
 */
static long s3fs_dev_admit_arena(void __user *uarg)
{
	struct s3fs_admit_arena hdr;
	struct s3fs_admit_entry *entries = NULL;
	struct mm_struct *mm = NULL;
	int err;

	if (copy_from_user(&hdr, uarg, sizeof(hdr)))
		return -EFAULT;

	if (hdr.count == 0) {
		err = s3fs_ebpf_admit_replace(NULL, NULL, 0);
		if (!err)
			pr_info("s3fs_ebpf: admit cleared\n");
		return err;
	}

	if (hdr.count > 4096)
		return -E2BIG;

	mm = get_task_mm(current);
	if (!mm)
		return -ESRCH;

	entries = kcalloc(hdr.count, sizeof(*entries), GFP_KERNEL);
	if (!entries) {
		err = -ENOMEM;
		goto out_put_mm;
	}
	if (copy_from_user(entries, (void __user *)(uintptr_t)hdr.entries,
			   hdr.count * sizeof(*entries))) {
		err = -EFAULT;
		goto out_free_entries;
	}

	/* Hand mm ownership over on success. */
	err = s3fs_ebpf_admit_replace(mm, entries, hdr.count);
	if (err)
		goto out_free_entries;

	pr_info("s3fs_ebpf: admitted %u entries mm=%p (daemon)\n",
		hdr.count, mm);
	kfree(entries);
	return 0;

out_free_entries:
	kfree(entries);
out_put_mm:
	mmput(mm);
	return err;
}

/*
 * S3FS_IOC_SIGNAL_ADMIT — daemon → kmod completion signal for a
 * previously enqueued read-miss. `err` is written into the pending
 * entry before completion; 0 means "admit table now has this ino,
 * please retry".
 */
static long s3fs_dev_signal_admit(void __user *uarg)
{
	struct s3fs_signal_admit sig;

	if (copy_from_user(&sig, uarg, sizeof(sig)))
		return -EFAULT;
	return s3fs_ebpf_signal_admit(sig.ino, sig.err);
}

/*
 * S3FS_IOC_SIGNAL_FLUSH — daemon → kmod completion signal for a
 * previously enqueued flush event (E-01). Reuses the signal_admit
 * struct because the payload shape is identical {ino, err}. Returns
 * -ENOENT when no fsync() caller is waiting on this ino, which is
 * a normal outcome for the fire-and-forget .release path — daemon
 * signals unconditionally after every PUT.
 */
static long s3fs_dev_signal_flush(void __user *uarg)
{
	struct s3fs_signal_admit sig;

	if (copy_from_user(&sig, uarg, sizeof(sig)))
		return -EFAULT;
	return s3fs_ebpf_signal_flush(sig.ino, sig.err);
}

/*
 * S3FS_IOC_SYNC_ENTRIES — replace the dynamic entry table.
 *
 * count==0 restores the bootstrap set. Otherwise we allocate the
 * internal s3fs_entry_dyn array here, copy each UAPI entry over (with
 * name_len clamped to S3FS_SYNC_NAME_MAX), and hand ownership to the
 * vfs side. The vfs side kfree's the previous dynamic array under
 * its own mutex and shrinks the dcache so stale dentries clear.
 */
static long s3fs_dev_sync_entries(void __user *uarg)
{
	struct s3fs_sync_entries hdr;
	struct s3fs_sync_entry *ubuf = NULL;
	struct s3fs_entry_dyn *kbuf = NULL;
	int err;
	u32 i;

	if (copy_from_user(&hdr, uarg, sizeof(hdr)))
		return -EFAULT;

	if (hdr.count == 0)
		return s3fs_ebpf_entries_replace(NULL, 0);

	if (hdr.count > 4096)
		return -E2BIG;

	ubuf = kvcalloc(hdr.count, sizeof(*ubuf), GFP_KERNEL);
	if (!ubuf)
		return -ENOMEM;
	if (copy_from_user(ubuf, (void __user *)(uintptr_t)hdr.entries,
			   hdr.count * sizeof(*ubuf))) {
		err = -EFAULT;
		goto out_free_ubuf;
	}

	kbuf = kvcalloc(hdr.count, sizeof(*kbuf), GFP_KERNEL);
	if (!kbuf) {
		err = -ENOMEM;
		goto out_free_ubuf;
	}
	for (i = 0; i < hdr.count; i++) {
		u32 nlen = ubuf[i].name_len;

		if (nlen == 0 || nlen > S3FS_SYNC_NAME_MAX) {
			err = -EINVAL;
			goto out_free_kbuf;
		}
		kbuf[i].ino      = ubuf[i].ino;
		kbuf[i].size     = ubuf[i].size;
		kbuf[i].mode     = ubuf[i].mode;
		kbuf[i].name_len = nlen;
		memcpy(kbuf[i].name, ubuf[i].name, nlen);
		kbuf[i].name[nlen] = '\0';
	}

	/* Hand ownership over on success. */
	err = s3fs_ebpf_entries_replace(kbuf, hdr.count);
	if (err)
		goto out_free_kbuf;

	/* pr_debug so periodic no-op syncs don't spam dmesg. Enable with
	 * `echo -n 'module s3fs_kfunc +p' > /sys/kernel/debug/dynamic_debug/control`. */
	pr_debug("s3fs_ebpf: synced %u entries\n", hdr.count);
	kvfree(ubuf);
	return 0;

out_free_kbuf:
	kvfree(kbuf);
out_free_ubuf:
	kvfree(ubuf);
	return err;
}

/*
 * S3FS_IOC_POP_FLUSH — daemon pulls one write-back event.
 *
 * Returns -EAGAIN if the queue is empty. Non-blocking; the daemon
 * polls this on a ticker (typically 100ms) and drains all pending
 * events per tick.
 */
static long s3fs_dev_pop_flush(void __user *uarg)
{
	struct s3fs_flush_event ev;
	int err;

	err = s3fs_ebpf_flush_pop(&ev);
	if (err)
		return err;
	if (copy_to_user(uarg, &ev, sizeof(ev)))
		return -EFAULT;
	return 0;
}

/*
 * S3FS_IOC_POP_DELETE — daemon drains queued delete events (E-02).
 * Symmetric with POP_FLUSH: -EAGAIN on empty, 0 with event copied
 * out on success.
 */
static long s3fs_dev_pop_delete(void __user *uarg)
{
	struct s3fs_delete_event ev;
	int err;

	err = s3fs_ebpf_delete_pop(&ev);
	if (err)
		return err;
	if (copy_to_user(uarg, &ev, sizeof(ev)))
		return -EFAULT;
	return 0;
}

/*
 * S3FS_IOC_READ_DIRTY — daemon reads the current scratch bytes for
 * `ino` into its own buffer. Returns 0 on success and writes
 * `out_bytes` back; returns -EAGAIN when the ino is unknown (raced
 * with an evict) so the daemon can drop the flush event silently.
 */
static long s3fs_dev_read_dirty(void __user *uarg)
{
	struct s3fs_read_dirty req;
	long copied;

	if (copy_from_user(&req, uarg, sizeof(req)))
		return -EFAULT;
	copied = s3fs_ebpf_read_dirty(req.ino,
				      (void __user *)(uintptr_t)req.uaddr,
				      req.size);
	if (copied < 0)
		return copied;
	req.out_bytes = (u64)copied;
	if (copy_to_user(uarg, &req, sizeof(req)))
		return -EFAULT;
	return 0;
}

static long s3fs_dev_ioctl(struct file *filp, unsigned int cmd, unsigned long arg)
{
	void __user *uarg = (void __user *)arg;

	switch (cmd) {
	case S3FS_IOC_PROBE_KVBASE:
		return s3fs_dev_probe_kvbase(uarg);
	case S3FS_IOC_ADMIT_ARENA:
		return s3fs_dev_admit_arena(uarg);
	case S3FS_IOC_SIGNAL_ADMIT:
		return s3fs_dev_signal_admit(uarg);
	case S3FS_IOC_SIGNAL_FLUSH:
		return s3fs_dev_signal_flush(uarg);
	case S3FS_IOC_POP_DELETE:
		return s3fs_dev_pop_delete(uarg);
	case S3FS_IOC_SYNC_ENTRIES:
		return s3fs_dev_sync_entries(uarg);
	case S3FS_IOC_POP_FLUSH:
		return s3fs_dev_pop_flush(uarg);
	case S3FS_IOC_READ_DIRTY:
		return s3fs_dev_read_dirty(uarg);
	case S3FS_IOC_MARK_DIRTY: {
		struct s3fs_mark_dirty req;

		if (copy_from_user(&req, uarg, sizeof(req)))
			return -EFAULT;
		return s3fs_ebpf_mark_dirty(req.ino);
	}
	case S3FS_IOC_STATS: {
		struct s3fs_stats st;

		if (copy_from_user(&st, uarg, sizeof(st)))
			return -EFAULT;
		if (st.count > S3FS_STATS_MAX)
			st.count = S3FS_STATS_MAX;
		memset(st.values, 0, sizeof(st.values));
		if (st.count > 0)
			st.values[0] = (u64)atomic_long_read(&s3fs_evictions_total);
		if (st.count > 1)
			st.values[1] = (u64)atomic_long_read(&s3fs_folio_hits);
		st.filled = st.count;
		if (copy_to_user(uarg, &st, sizeof(st)))
			return -EFAULT;
		return 0;
	}
	case S3FS_IOC_DRAIN_EVICTIONS: {
		struct s3fs_drain_evictions *req;
		int err = 0;

		req = kzalloc(sizeof(*req), GFP_KERNEL);
		if (!req)
			return -ENOMEM;
		if (copy_from_user(req, uarg, sizeof(*req))) {
			err = -EFAULT;
			goto out_free_req;
		}
		if (req->cap > S3FS_EVICT_LOG_MAX)
			req->cap = S3FS_EVICT_LOG_MAX;
		s3fs_ebpf_drain_evictions(req->inos, req->cap,
					  &req->filled, &req->overflow);
		if (copy_to_user(uarg, req, sizeof(*req)))
			err = -EFAULT;
out_free_req:
		kfree(req);
		return err;
	}
	default:
		return -ENOTTY;
	}
}

/*
 * .read on /dev/s3fs_ebpf drains read-miss events one at a time.
 * Each read call must provide a buffer at least sizeof(event) bytes;
 * short buffers get -EINVAL rather than partial events (event
 * atomicity is easier to reason about than framing over a stream).
 */
static ssize_t s3fs_dev_read(struct file *filp, char __user *buf,
			     size_t count, loff_t *ppos)
{
	struct s3fs_read_miss_event ev;
	ssize_t n;

	if (count < sizeof(ev))
		return -EINVAL;

	n = s3fs_read_miss_dequeue(&ev, filp->f_flags & O_NONBLOCK);
	if (n < 0)
		return n;
	if (copy_to_user(buf, &ev, sizeof(ev)))
		return -EFAULT;
	return sizeof(ev);
}

static __poll_t s3fs_dev_poll(struct file *filp, struct poll_table_struct *pt)
{
	return s3fs_read_miss_poll(filp, pt);
}

static const struct file_operations s3fs_ebpf_dev_fops = {
	.owner		= THIS_MODULE,
	.unlocked_ioctl	= s3fs_dev_ioctl,
	.compat_ioctl	= s3fs_dev_ioctl,
	.read		= s3fs_dev_read,
	.poll		= s3fs_dev_poll,
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
	/* No more ioctls can arrive; drop the admit table and release
	 * the arena reference before the module unloads. */
	s3fs_ebpf_admit_shutdown();
	/* Free any dynamic entry table left over; readers can no longer
	 * fire because the mount has been torn down before rmmod. */
	s3fs_ebpf_entries_shutdown();
}
