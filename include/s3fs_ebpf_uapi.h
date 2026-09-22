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

#endif /* _S3FS_EBPF_UAPI_H */
