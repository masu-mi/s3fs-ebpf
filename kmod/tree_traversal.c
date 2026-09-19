// SPDX-License-Identifier: GPL-2.0
/*
 * s3fs-ebpf: KFunc registration stub.
 *
 * Week 0 skeleton: registers one KFunc that returns 0. Week 1 will grow
 * this into the `bpf_arena` radix-tree walker that bypasses the Verifier
 * complexity limit.
 */

#include <linux/bpf.h>
#include <linux/btf.h>
#include <linux/btf_ids.h>
#include <linux/module.h>

#include "tree_traversal.h"

__bpf_kfunc_start_defs();

/*
 * s3fs_kfunc_traverse_stub - placeholder KFunc.
 *
 * @arena_root: opaque pointer into a bpf_arena mapping. Unused for now.
 *
 * Returns 0. In future sprints this will walk a radix tree inside the
 * arena and return the resolved inode number.
 */
__bpf_kfunc int s3fs_kfunc_traverse_stub(void *arena_root)
{
	return 0;
}

__bpf_kfunc_end_defs();

/*
 * BTF_SET8_START / BTF_SET8_END are the pre-6.11 spelling; kernels 6.11+
 * added BTF_KFUNCS_START / BTF_KFUNCS_END as aliases. Use SET8 so we
 * compile against the entire 6.8 → mainline range.
 */
BTF_SET8_START(s3fs_kfunc_ids)
BTF_ID_FLAGS(func, s3fs_kfunc_traverse_stub, KF_TRUSTED_ARGS)
BTF_SET8_END(s3fs_kfunc_ids)

static const struct btf_kfunc_id_set s3fs_kfunc_set = {
	.owner = THIS_MODULE,
	.set   = &s3fs_kfunc_ids,
};

int s3fs_kfunc_register(void)
{
	return register_btf_kfunc_id_set(BPF_PROG_TYPE_TRACING, &s3fs_kfunc_set);
}

void s3fs_kfunc_unregister(void)
{
	/*
	 * Nothing to do: the kfunc set is tied to THIS_MODULE and is torn
	 * down automatically when the module is unloaded.
	 */
}
