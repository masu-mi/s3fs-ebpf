// SPDX-License-Identifier: GPL-2.0
/*
 * s3fs-ebpf: KFunc registrations.
 *
 * Week 0: `s3fs_kfunc_traverse_stub` – placeholder returned 0.
 * Week 1: adds `s3fs_kfunc_echo` (registered against BPF_PROG_TYPE_SYSCALL)
 *         to prove the eBPF → KFunc call path independent of arena access.
 *
 * Week 2 will grow this file into the arena-dereferencing traversal that
 * the fentry-hooked BPF program will invoke.
 */

#include <linux/bpf.h>
#include <linux/btf.h>
#include <linux/btf_ids.h>
#include <linux/module.h>

#include "tree_traversal.h"
#include "vfs_ops.h" /* s3fs_policy_on_admit/evict/pressure prototypes */

__bpf_kfunc_start_defs();

/*
 * s3fs_kfunc_traverse_stub - placeholder for the Week 2 arena walker.
 * Returns 0. Registered against tracing programs so the fentry stub can
 * link against it once we wire the real fast path.
 */
__bpf_kfunc int s3fs_kfunc_traverse_stub(void *arena_root)
{
	return 0;
}

/*
 * s3fs_kfunc_echo - Week 1 round-trip probe.
 *
 * Deterministic transform (2v + 1) that the SEC("syscall") test harness
 * asserts against. The point is to demonstrate that a custom KFunc
 * registered from a module is callable from an eBPF program without
 * hitting the Verifier's complexity limit.
 */
__bpf_kfunc u32 s3fs_kfunc_echo(u32 v)
{
	return v * 2u + 1u;
}

__bpf_kfunc_end_defs();

/*
 * 6.11+ requires BTF_KFUNCS_START (sets BTF_SET8_KFUNCS in .flags) —
 * register_btf_kfunc_id_set rejects sets without it. Fall back to
 * BTF_SET8_START on older kernels that predate BTF_KFUNCS_START.
 */
#ifndef BTF_KFUNCS_START
#define BTF_KFUNCS_START(name) BTF_SET8_START(name)
#define BTF_KFUNCS_END(name)   BTF_SET8_END(name)
#endif

/* KFuncs callable from BPF_PROG_TYPE_TRACING (fentry / kprobe programs). */
BTF_KFUNCS_START(s3fs_kfunc_ids_tracing)
BTF_ID_FLAGS(func, s3fs_kfunc_traverse_stub, KF_TRUSTED_ARGS)
BTF_KFUNCS_END(s3fs_kfunc_ids_tracing)

/* KFuncs callable from BPF_PROG_TYPE_SYSCALL (BPF_PROG_TEST_RUN harness). */
BTF_KFUNCS_START(s3fs_kfunc_ids_syscall)
BTF_ID_FLAGS(func, s3fs_kfunc_echo)
BTF_KFUNCS_END(s3fs_kfunc_ids_syscall)

/*
 * Policy hooks registered for BPF_MODIFY_RETURN (fmod_ret programs).
 * Attaching a BPF program with SEC("fmod_ret/<name>") to these hooks
 * lets the BPF program override the return value directly, so
 * eviction votes / admission decisions flow through as function
 * return values rather than side-channel maps. See
 * docs/policy-engine-design.md §4.5 and P-10 in docs/backlog.md.
 */
BTF_SET8_START(s3fs_policy_fmodret_ids)
BTF_ID_FLAGS(func, s3fs_policy_on_admit)
BTF_ID_FLAGS(func, s3fs_policy_on_evict)
BTF_ID_FLAGS(func, s3fs_policy_on_pressure)
BTF_SET8_END(s3fs_policy_fmodret_ids)

static const struct btf_kfunc_id_set s3fs_policy_fmodret_set = {
	.owner = THIS_MODULE,
	.set   = &s3fs_policy_fmodret_ids,
};

static const struct btf_kfunc_id_set s3fs_kfunc_set_tracing = {
	.owner = THIS_MODULE,
	.set   = &s3fs_kfunc_ids_tracing,
};

static const struct btf_kfunc_id_set s3fs_kfunc_set_syscall = {
	.owner = THIS_MODULE,
	.set   = &s3fs_kfunc_ids_syscall,
};

int s3fs_kfunc_register(void)
{
	int ret;

	ret = register_btf_kfunc_id_set(BPF_PROG_TYPE_TRACING,
					&s3fs_kfunc_set_tracing);
	if (ret)
		return ret;

	ret = register_btf_kfunc_id_set(BPF_PROG_TYPE_SYSCALL,
					&s3fs_kfunc_set_syscall);
	if (ret)
		return ret;

	/* Enable SEC("fmod_ret/s3fs_policy_on_*") programs to override
	 * the hooks' return values (used for eviction votes etc.). */
	ret = register_btf_fmodret_id_set(&s3fs_policy_fmodret_set);
	if (ret)
		return ret;

	return 0;
}

void s3fs_kfunc_unregister(void)
{
	/*
	 * Kfunc sets are torn down automatically when THIS_MODULE unloads.
	 */
}
