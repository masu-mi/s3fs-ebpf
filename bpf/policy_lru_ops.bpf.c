// SPDX-License-Identifier: GPL-2.0
//
// policy_lru_ops — approximate LRU (admit-order FIFO) as a Layer 2
// struct_ops policy. Second built-in beside policy_s3fifo_ops so
// `--policy-bpf` / CEL selection has a real alternative to swap to.
//
// "Approximate" because we don't have an on_hit hook (that's the
// super-hot, per-read path handled by the kernel's built-in
// LRU_HASH map — see docs/policy-engine-design.md §2.2, §5.6).
// Without on_hit we can't distinguish frequently-vs-rarely-accessed
// entries, so this reduces to FIFO by admission order:
//   - on_admit: bump a global epoch counter
//   - on_evict per ino: first sight → seed with current epoch, keep;
//                        subsequent → if (epoch - seed) > cap_age, evict
//
// Effectively: entries admitted more than cap_age epochs ago are
// candidates for eviction. Compared with S3-FIFO's 2-bit freq
// counter, LRU here degrades linearly with age rather than reserving
// "recently reheated" entries. Both algorithms are visible via the
// same policy_stats map for daemon-side reads.

#include "vmlinux.h"
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_tracing.h>

/* Same shape as policy_s3fifo_ops so the daemon's policyprog reader
 * works unchanged (Stats, hook signatures, license). */
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

/* Per-ino admit-epoch (seq at which the ino was first observed here).
 * Age = current_epoch - seed_epoch. Evict when age > CAP_AGE. */
struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__type(key, __u64);
	__type(value, __u64);
	__uint(max_entries, 4096);
} lru_seed SEC(".maps");

/* Single-slot global epoch counter. Incremented once per on_admit
 * call (i.e. per admit_replace batch), so "one epoch" ≈ "one round of
 * admit changes". */
struct {
	__uint(type, BPF_MAP_TYPE_ARRAY);
	__type(key, __u32);
	__type(value, __u64);
	__uint(max_entries, 1);
} lru_epoch SEC(".maps");

/* Age threshold in epochs. 3 means "keep an entry for up to 3
 * admit_replace rounds before it becomes eligible for eviction".
 * Small on purpose so the softcap-driven eviction actually fires in
 * the smoke tests. */
#define LRU_CAP_AGE 3ULL

/* Mirror kmod/policy_struct_ops.h. See policy_s3fifo_ops.bpf.c for
 * the shape rationale (BTF relocation binds by member name). */
struct s3fs_policy_ops {
	int (*on_admit)(__u64 count, __u64 arena_used_hint);
	int (*on_evict)(__u64 ino);
	int (*on_pressure)(__u64 count, __u64 threshold);
	int (*on_hit)(__u64 ino);
};

SEC("struct_ops.s/on_admit")
int BPF_PROG(lru_admit, __u64 count, __u64 hint)
{
	__u32 k = 0;
	__u64 *ep, *sp;

	/* Advance epoch — all inos seen after this call are "one older". */
	ep = bpf_map_lookup_elem(&lru_epoch, &k);
	if (ep)
		__sync_fetch_and_add(ep, 1);

	/* Mirror stats slots so daemon panels light up the same way. */
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
int BPF_PROG(lru_evict, __u64 ino)
{
	__u32 k0 = 0;
	__u64 *ep = bpf_map_lookup_elem(&lru_epoch, &k0);
	__u64 epoch = ep ? *ep : 0;
	__u64 *seed = bpf_map_lookup_elem(&lru_seed, &ino);

	if (!seed) {
		bpf_map_update_elem(&lru_seed, &ino, &epoch, BPF_ANY);
		return 0; /* first sight, grace this round */
	}
	if (epoch >= *seed + LRU_CAP_AGE)
		return 1; /* aged out */
	return 0;
}

SEC("struct_ops.s/on_pressure")
int BPF_PROG(lru_pressure, __u64 count, __u64 threshold)
{
	return 0;
}

/* on_hit: refresh the seed to the current epoch — "reset age". This is
 * the whole reason a hit matters to LRU: an entry that keeps getting
 * accessed keeps looking young to on_evict's `epoch - seed >= CAP_AGE`
 * comparison. Fires per folio in the read hot path. */
SEC("struct_ops.s/on_hit")
int BPF_PROG(lru_hit, __u64 ino)
{
	__u32 k = 0;
	__u64 *ep = bpf_map_lookup_elem(&lru_epoch, &k);
	__u64 now = ep ? *ep : 0;

	bpf_map_update_elem(&lru_seed, &ino, &now, BPF_ANY);
	return 0;
}

SEC(".struct_ops.link")
struct s3fs_policy_ops lru_ops = {
	.on_admit    = (void *)lru_admit,
	.on_evict    = (void *)lru_evict,
	.on_pressure = (void *)lru_pressure,
	.on_hit      = (void *)lru_hit,
};

char _license[] SEC("license") = "GPL";
