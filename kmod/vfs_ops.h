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

#endif /* _S3FS_EBPF_VFS_OPS_H */
