// SPDX-License-Identifier: GPL-2.0
//
// policy_observe_ops — Layer 2 built-in policy (struct_ops).
//
// The observability baseline: every hook only counts and never votes to
// evict. It is the policy the tests and benchmarks run under when they
// need the policy path to be live but the eviction decision to be
// neutral. Real algorithms (SIEVE / LRU / S3-FIFO / freq2bit) live in
// their sibling policy_*_ops files.
//
// SEC(".struct_ops.link") so it can be the source or target of a
// BPF_LINK_UPDATE (POST /policy/reload).

#include "vmlinux.h"
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_tracing.h>

/* Slot indices for policy_stats[] — mirrored by the daemon reader in
 * daemon/internal/policyprog/. Additive-only (do not renumber). */
enum {
	PSTAT_INVOCATIONS = 0,
	PSTAT_LAST_COUNT  = 1,
	PSTAT_LAST_HINT   = 2,
	PSTAT_MAX,
};

struct s3fs_policy_ops {
	int (*on_admit)(__u64 count, __u64 arena_used_hint);
	int (*on_evict)(__u64 ino);
	int (*on_pressure)(__u64 count, __u64 threshold);
	int (*on_hit)(__u64 ino);
};

struct {
	__uint(type, BPF_MAP_TYPE_ARRAY);
	__type(key, __u32);
	__type(value, __u64);
	__uint(max_entries, PSTAT_MAX);
} policy_stats SEC(".maps");

SEC("struct_ops/on_admit")
int BPF_PROG(observe_admit, __u64 count, __u64 hint)
{
	__u32 k;
	__u64 *p;

	k = PSTAT_INVOCATIONS;
	p = bpf_map_lookup_elem(&policy_stats, &k);
	if (p)
		__sync_fetch_and_add(p, 1);

	k = PSTAT_LAST_COUNT;
	p = bpf_map_lookup_elem(&policy_stats, &k);
	if (p)
		*p = count;

	k = PSTAT_LAST_HINT;
	p = bpf_map_lookup_elem(&policy_stats, &k);
	if (p)
		*p = hint;

	return 0;
}

SEC("struct_ops/on_evict")
int BPF_PROG(observe_evict, __u64 ino)
{
	return 0; /* never vote to evict */
}

SEC("struct_ops/on_pressure")
int BPF_PROG(observe_pressure, __u64 count, __u64 threshold)
{
	return 0;
}

/* Bumps the invocations counter per hit so /metrics shows the hit rate
 * of the hook itself. No decision made. */
SEC("struct_ops/on_hit")
int BPF_PROG(observe_hit, __u64 ino)
{
	__u32 k = PSTAT_INVOCATIONS;
	__u64 *p = bpf_map_lookup_elem(&policy_stats, &k);

	if (p)
		__sync_fetch_and_add(p, 1);
	return 0;
}

SEC(".struct_ops.link")
struct s3fs_policy_ops observe_ops = {
	.on_admit    = (void *)observe_admit,
	.on_evict    = (void *)observe_evict,
	.on_pressure = (void *)observe_pressure,
	.on_hit      = (void *)observe_hit,
};

char _license[] SEC("license") = "GPL";
