/* SPDX-License-Identifier: GPL-2.0 */
/*
 * s3fs-ebpf UAPI: shared ioctl definitions between kmod, daemon, and BPF.
 *
 * All commands are issued against /dev/s3fs_ebpf, exposed by the module as
 * a miscdevice once loaded.
 */
#ifndef _S3FS_EBPF_UAPI_H
#define _S3FS_EBPF_UAPI_H

#ifdef __KERNEL__
#include <linux/ioctl.h>
#include <linux/types.h>
#else
#include <sys/ioctl.h>
#include <stdint.h>
typedef uint32_t __u32;
typedef uint64_t __u64;
#endif

#define S3FS_EBPF_DEV_NAME "s3fs_ebpf"
#define S3FS_IOC_MAGIC     0xE3

/*
 * S3FS_IOC_PROBE_KVBASE — Week 4 (a) spike.
 *
 * Given a BPF arena map fd, return the kernel-VM base address that the
 * arena's pages are mapped at (arena->kern_vm->addr). Meant only for
 * verifying that the container_of-based extraction works on the running
 * kernel; production paths will admit ino→backing tuples via a separate
 * ioctl once the theory is validated.
 */
struct s3fs_probe_kvbase {
	__u32 arena_fd;   /* in:  fd of BPF_MAP_TYPE_ARENA held by daemon */
	__u32 _pad;
	__u64 out_kvbase; /* out: kernel-VM start of the arena */
	__u64 out_size;   /* out: mapped size in bytes (pages * PAGE_SIZE) */
};

#define S3FS_IOC_PROBE_KVBASE \
	_IOWR(S3FS_IOC_MAGIC, 0x01, struct s3fs_probe_kvbase)

/*
 * S3FS_IOC_ADMIT_ARENA — register per-ino userspace ranges that back
 * file content.
 *
 * The daemon writes each object's bytes into its arena mmap, then
 * submits this ioctl to tell kmod: "ino N's content lives at user_vm
 * address `uaddr` for `size` bytes". kmod stores (ino → mm, uaddr,
 * size) in a hash keyed by ino; .read then uses
 * pin_user_pages_remote + kmap_local_page to reach the same physical
 * pages the arena mmap holds and copy_to_user out of them.
 *
 * We use the daemon's user_vm view (not the arena's kernel-VM base)
 * because 6.12's arena kern_vm range has no generic-context fault
 * handler; only BPF programs can service the fault path. GUP over
 * user_vm sidesteps that limitation by going through the daemon's
 * process mm, which does have arena_vm_fault registered.
 *
 * Passing count=0 clears kmod's admission table (used on daemon exit
 * to drop the mm reference cleanly).
 */
struct s3fs_admit_entry {
	__u64 ino;
	__u64 uaddr; /* daemon-side user_vm address of the object bytes */
	__u64 size;
};

struct s3fs_admit_arena {
	__u32 count;   /* in: number of entries; 0 clears the table */
	__u32 _pad;
	__u64 entries; /* in: user pointer to array of s3fs_admit_entry */
};

#define S3FS_IOC_ADMIT_ARENA \
	_IOW(S3FS_IOC_MAGIC, 0x02, struct s3fs_admit_arena)

#endif /* _S3FS_EBPF_UAPI_H */
