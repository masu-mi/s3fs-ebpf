// SPDX-License-Identifier: GPL-2.0
//
// policy_sieve_ops — SIEVE (NSDI 2024, Zhang et al) as a Layer 2
// struct_ops policy. Third built-in beside S3-FIFO and LRU so hit-rate
// bench (P-07) has a modern low-overhead algorithm to compare against.
//
// Original SIEVE: a single FIFO of admitted items + a per-item visited
// bit + a hand pointer that walks tail→head; on eviction the hand
// clears visited=1 bits (grants a second chance) and evicts the first
// visited=0 it sees; on access, visited=1. O(1) per operation, no
// promotion/demotion churn.
//
// We don't have an on_hit hook (per-read access is a super-hot path
// currently handled by the kernel's built-in fentry counters — see
// docs/policy-engine-design.md §2.2). Without on_hit, we degrade
// the "hit sets visited=1" step: instead the visited bit only ever
// gets set on first sight (grace period). The remaining second-chance
// semantics (visited=1 → clear + keep, visited=0 → evict) match SIEVE
// at the admit_replace round granularity:
//
//   round 0 (first admit → first on_evict eval): seed visited=1, keep
//   round 1 (still admitted, softcap fires): visited=1 → clear, keep
//   round 2 (still admitted, softcap fires): visited=0 → EVICT
//
// vs LRU (age-based, monotonic): a "spared" LRU entry still ages,
// so eviction is inevitable. SIEVE's clear-on-spare gives frequently
// re-referenced entries (which would set visited=1 via a future
// on_hit hook) an unbounded reprieve.
//
// When on_hit is added, this program only needs one more prog +
// map_update to be true SIEVE — the eviction logic here stays.

#include "vmlinux.h"
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_tracing.h>

/* Aggregate stats — same slots as sibling policies so the daemon
 * Prometheus mirror works unchanged. */
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

/* Per-ino visited bit. Stored as __u8 (0 or 1) for readability; a
 * bitmap would save memory but 4096 * 1 B = 4 KiB is negligible. */
struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__type(key, __u64);
	__type(value, __u8);
	__uint(max_entries, 4096);
} sieve_visited SEC(".maps");

/* Mirror kmod/policy_struct_ops.h. Field names bind by BTF at load. */
struct s3fs_policy_ops {
	int (*on_admit)(__u64 count, __u64 arena_used_hint);
	int (*on_evict)(__u64 ino);
	int (*on_pressure)(__u64 count, __u64 threshold);
	int (*on_hit)(__u64 ino);
};

SEC("struct_ops.s/on_admit")
int BPF_PROG(sieve_admit, __u64 count, __u64 hint)
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
int BPF_PROG(sieve_evict, __u64 ino)
{
	__u8 *vp = bpf_map_lookup_elem(&sieve_visited, &ino);
	__u8 one = 1, zero = 0;

	if (!vp) {
		/* First sight of this ino — seed with visited=1 (grace). */
		bpf_map_update_elem(&sieve_visited, &ino, &one, BPF_ANY);
		return 0;
	}
	if (*vp) {
		/* SIEVE hand: visited=1 → clear + keep (second chance). */
		bpf_map_update_elem(&sieve_visited, &ino, &zero, BPF_ANY);
		return 0;
	}
	/* visited=0 → evict. Delete the entry so a re-admit of the
	 * same ino restarts from the seeded grace period. */
	bpf_map_delete_elem(&sieve_visited, &ino);
	return 1;
}

SEC("struct_ops.s/on_pressure")
int BPF_PROG(sieve_pressure, __u64 count, __u64 threshold)
{
	return 0;
}

/* on_hit: the missing piece for real SIEVE. Set visited=1 so a
 * subsequent on_evict pass gives the entry the second-chance branch
 * (clear + keep) instead of the eviction branch. Fires per folio in
 * the read hot path; O(1) map update. */
SEC("struct_ops.s/on_hit")
int BPF_PROG(sieve_hit, __u64 ino)
{
	__u8 one = 1;

	bpf_map_update_elem(&sieve_visited, &ino, &one, BPF_ANY);
	return 0;
}

SEC(".struct_ops.link")
struct s3fs_policy_ops sieve_ops = {
	.on_admit    = (void *)sieve_admit,
	.on_evict    = (void *)sieve_evict,
	.on_pressure = (void *)sieve_pressure,
	.on_hit      = (void *)sieve_hit,
};

char _license[] SEC("license") = "GPL";
