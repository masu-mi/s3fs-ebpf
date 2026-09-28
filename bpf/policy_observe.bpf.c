// SPDX-License-Identifier: GPL-2.0
//
// policy_observe — Layer 2 built-in policy program (Week 4 (d)).
//
// Attaches to kmod's s3fs_policy_on_admit fentry anchor. Increments
// a per-invocation counter and stamps the last-seen (count, hint)
// tuple. Real algorithms (SIEVE / LRU / S3-FIFO / freq2bit) live in
// their sibling files: SEC("fentry/s3fs_policy_on_admit") + BPF_PROG(...)
// reading kernel args, writing decisions to a shared BPF map.
//
// This one intentionally does *no* eviction — it's the observability
// baseline that the whole Layer 2/3 wiring is verified against.

#include "vmlinux.h"
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_tracing.h>

/* Slot indices for policy_stats[] — mirrored by the daemon reader
 * in daemon/internal/policyprog/. Additive-only (do not renumber). */
enum {
	PSTAT_INVOCATIONS = 0,
	PSTAT_LAST_COUNT  = 1,
	PSTAT_LAST_HINT   = 2,
	PSTAT_MAX,
};

struct {
	__uint(type, BPF_MAP_TYPE_ARRAY);
	__type(key, __u32);
	__type(value, __u64);
	__uint(max_entries, PSTAT_MAX);
} policy_stats SEC(".maps");

SEC("fmod_ret/s3fs_policy_on_admit")
int BPF_PROG(observe_on_admit, __u64 count, __u64 arena_used_hint)
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
		*p = arena_used_hint;

	return 0;
}

/* Baseline observer for the per-folio hit hook. Rebuys the invocations
 * counter so /metrics increments per hit — useful for smoke-checking
 * that on_hit fires at the expected rate before switching to a real
 * algorithm (SIEVE/LRU/S3-FIFO/freq2bit). No decision made. */
SEC("fmod_ret/s3fs_policy_on_hit")
int BPF_PROG(observe_on_hit, __u64 ino)
{
	__u32 k = PSTAT_INVOCATIONS;
	__u64 *p = bpf_map_lookup_elem(&policy_stats, &k);

	if (p)
		__sync_fetch_and_add(p, 1);
	return 0;
}

char _license[] SEC("license") = "GPL";
