// SPDX-License-Identifier: GPL-2.0
//
// policy_time_decay_ops — time-decay LRU. Second Layer 0 KFunc demo
// after policy_pressure_ops. Where pressure_ops reads live admit
// count, this one reads live monotonic time and evicts anything
// whose last on_hit was more than DECAY_NS ago.
//
// The point isn't to compete with SIEVE/W-TinyLFU on hit rate — it
// won't, because the paper-worthy policies capture reuse pattern
// with far less state. The point is that a policy CAN pull live
// kernel-side kmod state via KFuncs, which the pre-P-08 vtable
// couldn't do at all. Under this shape any user policy can also
// implement things like "evict when eviction_total > my threshold"
// or "grace new admits for 30 s before considering them for evict".

#include "vmlinux.h"
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_tracing.h>

/* Layer 0 KFuncs — resolve at load time against
 * kmod/tree_traversal.c BTF_KFUNCS_START(s3fs_kfunc_ids_struct_ops). */
extern __u32 s3fs_kfunc_admit_count(void)     __ksym;
extern __u32 s3fs_kfunc_softcap(void)         __ksym;
extern __u32 s3fs_kfunc_evictions_total(void) __ksym;
extern __u64 s3fs_kfunc_time_ns(void)         __ksym;

struct s3fs_policy_ops {
	int (*on_admit)(__u64 count, __u64 arena_used_hint);
	int (*on_evict)(__u64 ino);
	int (*on_pressure)(__u64 count, __u64 threshold);
	int (*on_hit)(__u64 ino);
};

/* Per-ino last-hit timestamp (ns since boot). Missing key = never
 * hit; on_evict grants those a grace round to warm up. */
struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__type(key, __u64);
	__type(value, __u64);
	__uint(max_entries, 4096);
} time_decay_last_hit SEC(".maps");

enum {
	PSTAT_INVOCATIONS   = 0,
	PSTAT_LAST_COUNT    = 1,
	PSTAT_LAST_HINT     = 2,
	/* Layer 0 read-throughs — smoke-verify the KFuncs are alive. */
	PSTAT_LAST_ADMITS   = 3,
	PSTAT_LAST_EVICTS   = 4,
	PSTAT_LAST_TIME_MS  = 5,
	PSTAT_MAX,
};

struct {
	__uint(type, BPF_MAP_TYPE_ARRAY);
	__type(key, __u32);
	__type(value, __u64);
	__uint(max_entries, PSTAT_MAX);
} policy_stats SEC(".maps");

/* Time an item is allowed to sit unaccessed before it's a candidate
 * for eviction. 5 s is the default; overridable at startup via
 * time_decay_config[0] (Z-02). Set to e.g. 1 s for short benches. */
#define DECAY_NS_DEFAULT  (5ULL * 1000ULL * 1000ULL * 1000ULL)

/* Z-02: runtime-configurable decay window.
 * Slot 0 = decay_ns. 0 (BPF zero-init) → fall back to DECAY_NS_DEFAULT. */
enum { TDCFG_DECAY_NS = 0, TDCFG_MAX };

struct {
	__uint(type, BPF_MAP_TYPE_ARRAY);
	__type(key, __u32);
	__type(value, __u64);
	__uint(max_entries, TDCFG_MAX);
} time_decay_config SEC(".maps");

SEC("struct_ops/on_admit")
int BPF_PROG(time_decay_admit, __u64 count, __u64 hint)
{
	__u32 k;
	__u64 *sp;

	k = PSTAT_INVOCATIONS;
	sp = bpf_map_lookup_elem(&policy_stats, &k);
	if (sp)
		__sync_fetch_and_add(sp, 1);
	k = PSTAT_LAST_COUNT;
	sp = bpf_map_lookup_elem(&policy_stats, &k);
	if (sp)
		*sp = count;
	k = PSTAT_LAST_HINT;
	sp = bpf_map_lookup_elem(&policy_stats, &k);
	if (sp)
		*sp = hint;

	/* Read Layer 0 state and stash for /metrics visibility. This
	 * proves the KFuncs work even when the eviction path doesn't
	 * enforce anything time-related. */
	k = PSTAT_LAST_ADMITS;
	sp = bpf_map_lookup_elem(&policy_stats, &k);
	if (sp)
		*sp = s3fs_kfunc_admit_count();
	k = PSTAT_LAST_EVICTS;
	sp = bpf_map_lookup_elem(&policy_stats, &k);
	if (sp)
		*sp = s3fs_kfunc_evictions_total();
	k = PSTAT_LAST_TIME_MS;
	sp = bpf_map_lookup_elem(&policy_stats, &k);
	if (sp)
		*sp = s3fs_kfunc_time_ns() / 1000000ULL;

	return 0;
}

SEC("struct_ops/on_evict")
int BPF_PROG(time_decay_evict, __u64 ino)
{
	__u64 *last = bpf_map_lookup_elem(&time_decay_last_hit, &ino);
	__u64 now = s3fs_kfunc_time_ns();
	__u32 k;
	__u64 *cp, decay_ns;

	/* Z-02: read configured decay window; fall back to default. */
	k = TDCFG_DECAY_NS;
	cp = bpf_map_lookup_elem(&time_decay_config, &k);
	decay_ns = (cp && *cp) ? *cp : DECAY_NS_DEFAULT;
	if (!decay_ns)
		decay_ns = DECAY_NS_DEFAULT;

	if (!last) {
		/* First sight — store sentinel 0 meaning "no on_hit yet".
		 * on_hit will overwrite with a real timestamp (always > 0 since
		 * the clock is already running at boot). This prevents the bench
		 * convergence loop from evicting all items before the workload. */
		__u64 zero = 0;
		bpf_map_update_elem(&time_decay_last_hit, &ino, &zero, BPF_ANY);
		return 0;
	}
	/* Sentinel 0 = on_hit has never fired → infinite grace. */
	if (!*last)
		return 0;
	if (now - *last > decay_ns)
		return 1;
	return 0;
}

SEC("struct_ops/on_pressure")
int BPF_PROG(time_decay_pressure, __u64 count, __u64 threshold)
{
	return 0;
}

SEC("struct_ops/on_hit")
int BPF_PROG(time_decay_hit, __u64 ino)
{
	__u64 now = s3fs_kfunc_time_ns();

	bpf_map_update_elem(&time_decay_last_hit, &ino, &now, BPF_ANY);
	return 0;
}

SEC(".struct_ops.link")
struct s3fs_policy_ops time_decay_ops = {
	.on_admit    = (void *)time_decay_admit,
	.on_evict    = (void *)time_decay_evict,
	.on_pressure = (void *)time_decay_pressure,
	.on_hit      = (void *)time_decay_hit,
};

char _license[] SEC("license") = "GPL";
