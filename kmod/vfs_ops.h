#ifndef _S3FS_EBPF_VFS_OPS_H
#define _S3FS_EBPF_VFS_OPS_H

#include <linux/fs.h>
#include <linux/fs_context.h>

int s3fs_ebpf_fill_super(struct super_block *sb, struct fs_context *fc);

#endif /* _S3FS_EBPF_VFS_OPS_H */
