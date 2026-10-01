// SPDX-License-Identifier: GPL-2.0
//
// policy_pressure_ops — SIEVE with pressure-aware strictness. Demo
// vehicle for the P-08 Layer 0 KFunc set (admit_count + softcap).
//
// The problem it explores: a fixed policy has to guess whether the
// admit set is barely full or catastrophically over softcap.
// SIEVE's second-chance clock is generous — great when there's
// headroom, weak when the working set doubles. Reading the actual
// pressure via KFunc lets the policy pick strictness per-eviction:
//
//   pressure_ratio = admit_count / softcap
//   pressure_ratio <  1.5x  → SIEVE (second-chance grace)
//   pressure_ratio >= 1.5x  → SKIP grace, evict on visited=0 or 1
//
// Under our bench (softcap=8, 30 admits during warmup) the ratio
// climbs above 3.5x, so the aggressive branch fires for most
// eviction rounds. This should trade a bit of hot-item survival
// for faster convergence toward softcap.
//
// The KFunc calls are the interesting bit — they're the first
// piece of kmod-side state a Layer 2/3 program can read that
// isn't plumbed through hook arguments. See P-08 in
// docs/journal/backlog.md.

#include "vmlinux.h"
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_tracing.h>

/* KFunc declarations. Symbol names resolve at load time against
 * kmod's registered set (kmod/tree_traversal.c BTF_KFUNCS_START(
 * s3fs_kfunc_ids_struct_ops)). */
extern __u32 s3fs_kfunc_admit_count(void) __ksym;
extern __u32 s3fs_kfunc_softcap(void)     __ksym;

struct s3fs_policy_ops {
	int (*on_admit)(__u64 count, __u64 arena_used_hint);
	int (*on_evict)(__u64 ino);
	int (*on_pressure)(__u64 count, __u64 threshold);
	int (*on_hit)(__u64 ino);
};

/* Same visited-bit shape as policy_sieve_ops. */
struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__type(key, __u64);
	__type(value, __u8);
	__uint(max_entries, 4096);
} pressure_visited SEC(".maps");

enum {
	PSTAT_INVOCATIONS   = 0,
	PSTAT_LAST_COUNT    = 1,
	PSTAT_LAST_HINT     = 2,
	/* Aggregate KFunc reads — smoke-verifies that the Layer 0
	 * calls actually fired. Not paper-precise semantics. */
	PSTAT_LAST_ADMITS   = 3,
	PSTAT_LAST_SOFTCAP  = 4,
	PSTAT_MAX,
};

struct {
	__uint(type, BPF_MAP_TYPE_ARRAY);
	__type(key, __u32);
	__type(value, __u64);
	__uint(max_entries, PSTAT_MAX);
} policy_stats SEC(".maps");

/* Ratio above which we switch to aggressive eviction. In fixed-point
 * to avoid BPF floats: represent 1.5x as (admits * 2 >= softcap * 3). */
#define AGGRESSIVE_MUL_ADMITS  2
#define AGGRESSIVE_MUL_SOFTCAP 3

static __always_inline int is_high_pressure(void)
{
	__u32 admits  = s3fs_kfunc_admit_count();
	__u32 softcap = s3fs_kfunc_softcap();
	__u32 k;
	__u64 *sp;

	/* Store the last-observed values so the daemon /metrics
	 * mirror can prove KFunc-calls fired. */
	k = PSTAT_LAST_ADMITS;
	sp = bpf_map_lookup_elem(&policy_stats, &k);
	if (sp)
		*sp = admits;
	k = PSTAT_LAST_SOFTCAP;
	sp = bpf_map_lookup_elem(&policy_stats, &k);
	if (sp)
		*sp = softcap;

	if (!softcap)
		return 0;
	return admits * AGGRESSIVE_MUL_ADMITS >= softcap * AGGRESSIVE_MUL_SOFTCAP;
}

SEC("struct_ops.s/on_admit")
int BPF_PROG(pressure_admit, __u64 count, __u64 hint)
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

	return 0;
}

SEC("struct_ops.s/on_evict")
int BPF_PROG(pressure_evict, __u64 ino)
{
	__u8 *vp = bpf_map_lookup_elem(&pressure_visited, &ino);
	__u8 one = 1, zero = 0;
	int aggressive = is_high_pressure();

	if (!vp) {
		bpf_map_update_elem(&pressure_visited, &ino, &one, BPF_ANY);
		return 0; /* first sight — grace unconditionally */
	}

	if (*vp) {
		if (aggressive) {
			/* High pressure: skip second chance, clear the
			 * visited bit and evict. Hot items still get one
			 * bump from on_hit before the next round, so this
			 * isn't a pure FIFO. */
			bpf_map_update_elem(&pressure_visited, &ino, &zero, BPF_ANY);
			return 1;
		}
		/* Normal SIEVE: clear + keep. */
		bpf_map_update_elem(&pressure_visited, &ino, &zero, BPF_ANY);
		return 0;
	}
	/* visited=0 → always evict, regardless of pressure. */
	bpf_map_delete_elem(&pressure_visited, &ino);
	return 1;
}

SEC("struct_ops.s/on_pressure")
int BPF_PROG(pressure_pressure, __u64 count, __u64 threshold)
{
	return 0;
}

SEC("struct_ops.s/on_hit")
int BPF_PROG(pressure_hit, __u64 ino)
{
	__u8 one = 1;

	bpf_map_update_elem(&pressure_visited, &ino, &one, BPF_ANY);
	return 0;
}

SEC(".struct_ops.link")
struct s3fs_policy_ops pressure_ops = {
	.on_admit    = (void *)pressure_admit,
	.on_evict    = (void *)pressure_evict,
	.on_pressure = (void *)pressure_pressure,
	.on_hit      = (void *)pressure_hit,
};

char _license[] SEC("license") = "GPL";
