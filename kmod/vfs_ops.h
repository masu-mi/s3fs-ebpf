#ifndef _S3FS_EBPF_VFS_OPS_H
#define _S3FS_EBPF_VFS_OPS_H

#include <linux/fs.h>
#include <linux/fs_context.h>
#include <linux/mm_types.h>
#include <linux/types.h>

#include "s3fs_ebpf_uapi.h"

int s3fs_ebpf_fill_super(struct super_block *sb, struct fs_context *fc);

/*
 * Admit table API — populated by the /dev/s3fs_ebpf ioctl handler,
 * consumed by the read path. `mm` is the daemon's mm_struct (with a
 * reference held by kmod); entries carry userspace addresses into
 * that mm which .read pins via pin_user_pages_remote. Passing
 * count==0 (with mm==NULL) clears the table and drops the mm ref.
 */
int  s3fs_ebpf_admit_replace(struct mm_struct *mm,
			     const struct s3fs_admit_entry *entries,
			     u32 count);
void s3fs_ebpf_admit_shutdown(void);

/*
 * Read-miss upcall API — daemon-facing side.
 * s3fs_read_miss_dequeue removes the head event (blocking or
 * non-blocking) and copies it out. s3fs_read_miss_poll bridges the
 * miscdevice .poll to the internal waitqueue. signal_admit wakes a
 * pending .read waiter.
 */
struct file;
struct poll_table_struct;
ssize_t s3fs_read_miss_dequeue(struct s3fs_read_miss_event *out,
			       bool nonblocking);
__poll_t s3fs_read_miss_poll(struct file *filp, struct poll_table_struct *pt);
int s3fs_ebpf_signal_admit(u64 ino, int err);

#endif /* _S3FS_EBPF_VFS_OPS_H */
