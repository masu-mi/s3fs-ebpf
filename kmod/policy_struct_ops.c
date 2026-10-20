// SPDX-License-Identifier: GPL-2.0
/*
 * policy_struct_ops.c — Layer 2 struct_ops implementation.
 *
 * Alternative to the fmod_ret path in tree_traversal.c / vfs_ops.c.
 * Instead of registering hooks with register_btf_fmodret_id_set() and
 * calling `s3fs_policy_on_*()` directly, we expose `struct
 * s3fs_policy_ops` as a BPF struct_ops type. BPF programs implement
 * this vtable as a single atomic unit; the whole policy hot-swaps by
 * one BPF map update.
 *
 * Compared with fmod_ret:
 *   fmod_ret: per-hook attach; tracing links have no BPF_LINK_UPDATE,
 *             so a swap is N attaches + N detaches (both policies run
 *             in between)
 *   struct_ops: 1 vtable, 1 BPF_LINK_UPDATE -> .update below swaps the
 *             RCU pointer once (atomic policy swap)
 *   fmod_ret: fentry-style, retval override
 *   struct_ops: function-pointer dispatch, retval is natural
 *
 * See docs/design/policy-engine-design.md §4.5 for the design context.
 */

#include <linux/bpf.h>
#include <linux/btf.h>
#include <linux/btf_ids.h>
#include <linux/filter.h>
#include <linux/module.h>
#include <linux/rcupdate.h>

#include "policy_struct_ops.h"

/* Currently-active policy vtable. RCU-protected: readers on the hot
 * path do rcu_dereference(); the reg/unreg callbacks (invoked at
 * bpf_link create/destroy) publish under rcu_assign_pointer +
 * synchronize_rcu(). */
static struct s3fs_policy_ops __rcu *s3fs_policy_active;

/*
 * Public entry point for kmod call sites. Returns the vtable's
 * decision when a policy is loaded, else the passed default. Kept
 * short and inline-friendly so the fast paths stay cheap.
 */
int s3fs_policy_ops_eval_admit(u64 count, u64 arena_used_hint)
{
	struct s3fs_policy_ops *ops;
	int ret = 0;

	rcu_read_lock();
	ops = rcu_dereference(s3fs_policy_active);
	if (ops && ops->on_admit)
		ret = ops->on_admit(count, arena_used_hint);
	rcu_read_unlock();
	return ret;
}

int s3fs_policy_ops_eval_evict(u64 ino)
{
	struct s3fs_policy_ops *ops;
	int ret = 0;

	rcu_read_lock();
	ops = rcu_dereference(s3fs_policy_active);
	if (ops && ops->on_evict)
		ret = ops->on_evict(ino);
	rcu_read_unlock();
	return ret;
}

int s3fs_policy_ops_eval_pressure(u64 count, u64 threshold)
{
	struct s3fs_policy_ops *ops;
	int ret = 0;

	rcu_read_lock();
	ops = rcu_dereference(s3fs_policy_active);
	if (ops && ops->on_pressure)
		ret = ops->on_pressure(count, threshold);
	rcu_read_unlock();
	return ret;
}

/*
 * on_hit fires on every read-path admit lookup that returned present
 * (see s3fs_ebpf_read_folio). Called per folio, so it's the hottest
 * of the four hooks — keep the wrapper lean and don't consume the
 * return value; hit signals are advisory (recency / freq updates).
 */
int s3fs_policy_ops_eval_hit(u64 ino)
{
	struct s3fs_policy_ops *ops;
	int ret = 0;

	rcu_read_lock();
	ops = rcu_dereference(s3fs_policy_active);
	if (ops && ops->on_hit)
		ret = ops->on_hit(ino);
	rcu_read_unlock();
	return ret;
}

/* AC-58: on_insert / on_remove / pick_victim mutate BPF-side policy state
 * that has no lock of its own, so one eviction round holds this mutex. */
static DEFINE_MUTEX(s3fs_policy_evict_mutex);

void s3fs_policy_evict_lock(void)   { mutex_lock(&s3fs_policy_evict_mutex); }
void s3fs_policy_evict_unlock(void) { mutex_unlock(&s3fs_policy_evict_mutex); }

bool s3fs_policy_has_pick_victim(void)
{
	struct s3fs_policy_ops *ops;
	bool ret;

	rcu_read_lock();
	ops = rcu_dereference(s3fs_policy_active);
	ret = ops && ops->pick_victim;
	rcu_read_unlock();
	return ret;
}

void s3fs_policy_ops_insert(u64 ino)
{
	struct s3fs_policy_ops *ops;

	rcu_read_lock();
	ops = rcu_dereference(s3fs_policy_active);
	if (ops && ops->on_insert)
		ops->on_insert(ino);
	rcu_read_unlock();
}

void s3fs_policy_ops_remove(u64 ino)
{
	struct s3fs_policy_ops *ops;

	rcu_read_lock();
	ops = rcu_dereference(s3fs_policy_active);
	if (ops && ops->on_remove)
		ops->on_remove(ino);
	rcu_read_unlock();
}

u64 s3fs_policy_ops_pick_victim(void)
{
	struct s3fs_policy_ops *ops;
	u64 ret = 0;

	rcu_read_lock();
	ops = rcu_dereference(s3fs_policy_active);
	if (ops && ops->pick_victim)
		ret = ops->pick_victim();
	rcu_read_unlock();
	return ret;
}

/* --- struct_ops framework glue --- */

/* CFI stubs. Modern kernels validate every indirect BPF call against
 * a stub whose signature matches. These are never actually invoked
 * (real dispatch goes through the vtable), so bodies return safe
 * defaults. */
static int __s3fs_stub_admit(u64 c, u64 h)     { return 0; }
static int __s3fs_stub_evict(u64 ino)          { return 0; }
static int __s3fs_stub_pressure(u64 c, u64 t)  { return 0; }
static int __s3fs_stub_hit(u64 ino)            { return 0; }
static int __s3fs_stub_insert(u64 ino)         { return 0; }
static int __s3fs_stub_remove(u64 ino)         { return 0; }
static u64 __s3fs_stub_pick(void)              { return 0; }

static struct s3fs_policy_ops s3fs_policy_cfi_stubs = {
	.on_admit    = __s3fs_stub_admit,
	.on_evict    = __s3fs_stub_evict,
	.on_pressure = __s3fs_stub_pressure,
	.on_hit      = __s3fs_stub_hit,
	.on_insert   = __s3fs_stub_insert,
	.on_remove   = __s3fs_stub_remove,
	.pick_victim = __s3fs_stub_pick,
};

/* verifier_ops: minimal — allow the base helper set. */
static const struct bpf_func_proto *
s3fs_policy_get_func_proto(enum bpf_func_id func_id,
			   const struct bpf_prog *prog)
{
	return bpf_base_func_proto(func_id, prog);
}

static bool s3fs_policy_is_valid_access(int off, int size,
					enum bpf_access_type type,
					const struct bpf_prog *prog,
					struct bpf_insn_access_aux *info)
{
	return bpf_tracing_btf_ctx_access(off, size, type, prog, info);
}

static const struct bpf_verifier_ops s3fs_policy_verifier_ops = {
	.get_func_proto  = s3fs_policy_get_func_proto,
	.is_valid_access = s3fs_policy_is_valid_access,
};

/* init_member: run per struct_ops slot when the BPF map is created.
 * The framework populates function-pointer members automatically
 * from the loaded BPF program; we just accept them. */
static int s3fs_policy_init_member(const struct btf_type *t,
				   const struct btf_member *member,
				   void *kdata, const void *udata)
{
	return 0;
}

/* reg: publish the vtable to the RCU pointer. Callable while another
 * vtable is already active — hot reload flow attaches the new one
 * before closing the old, so reg replaces the active pointer and the
 * subsequent unreg on the old vtable must NOT clear the new one. */
static int s3fs_policy_reg(void *kdata, struct bpf_link *link)
{
	struct s3fs_policy_ops *ops = kdata;

	rcu_assign_pointer(s3fs_policy_active, ops);
	pr_info("s3fs_ebpf: policy struct_ops registered (ops=%p)\n", ops);
	return 0;
}

/* unreg: clear the vtable only if the pointer still points at THIS
 * kdata. During hot reload the new reg has already overwritten
 * s3fs_policy_active before the old link's unreg fires; clearing
 * unconditionally would strand kmod without an active policy while
 * the new vtable is still attached. */
static void s3fs_policy_unreg(void *kdata, struct bpf_link *link)
{
	struct s3fs_policy_ops *ops = kdata;
	struct s3fs_policy_ops *cur;

	cur = rcu_dereference_protected(s3fs_policy_active, 1);
	if (cur == ops) {
		rcu_assign_pointer(s3fs_policy_active, NULL);
		synchronize_rcu();
		pr_info("s3fs_ebpf: policy struct_ops unregistered (was active)\n");
	} else {
		pr_info("s3fs_ebpf: policy struct_ops unregistered (superseded, keeping %p)\n",
			cur);
	}
}

/* update: hot-swap the vtable atomically (single pointer swap).
 * Invoked by BPF_LINK_UPDATE on a SEC(".struct_ops.link") link — the
 * daemon's POST /policy/reload path (policyprog.SwapFrom). */
static int s3fs_policy_update(void *kdata, void *old_kdata,
			      struct bpf_link *link)
{
	struct s3fs_policy_ops *ops = kdata;

	rcu_assign_pointer(s3fs_policy_active, ops);
	synchronize_rcu();
	pr_info("s3fs_ebpf: policy struct_ops updated (hot swap)\n");
	return 0;
}

static int s3fs_policy_init(struct btf *btf)
{
	return 0;
}

static struct bpf_struct_ops bpf_s3fs_policy_ops = {
	.verifier_ops = &s3fs_policy_verifier_ops,
	.init         = s3fs_policy_init,
	.init_member  = s3fs_policy_init_member,
	.reg          = s3fs_policy_reg,
	.unreg        = s3fs_policy_unreg,
	.update       = s3fs_policy_update,
	.cfi_stubs    = &s3fs_policy_cfi_stubs,
	.owner        = THIS_MODULE,
	.name         = "s3fs_policy_ops",
};

int s3fs_policy_struct_ops_register(void)
{
	return register_bpf_struct_ops(&bpf_s3fs_policy_ops, s3fs_policy_ops);
}

void s3fs_policy_struct_ops_unregister(void)
{
	/* struct_ops types are torn down automatically when THIS_MODULE
	 * unloads; no explicit deregister API is exposed. */
}

#ifdef S3FS_KUNIT
#include "policy_struct_ops_kunit.c"
#endif
