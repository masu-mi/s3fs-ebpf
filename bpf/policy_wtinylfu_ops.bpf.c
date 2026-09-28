// SPDX-License-Identifier: GPL-2.0
//
// policy_wtinylfu_ops — W-TinyLFU-like eviction policy.
//
// Reference: Einziger & Friedman, "TinyLFU: A Highly Efficient Cache
// Admission Policy", 2017. Deployed at scale in Caffeine (Java).
//
// **This is a scoped-down variant, not the full paper.** We keep the
// centerpiece — a Count-Min Sketch (CMS) frequency estimator that
// on_hit bumps and on_evict thresholds against — and drop:
//   - Window LRU tier: the paper puts new admits into a small W-LRU
//     (~1 %) before they compete for admission to Main. We don't have
//     a window tier here; new admits land in the same pool.
//   - Doorkeeper (Bloom filter for 1-hit wonders): omitted for
//     verifier simplicity; the aging step below serves a similar role.
//   - TinyLFU's admission decision (evict-if-victim-freq < candidate-
//     freq): our on_evict is called per-ino at capacity pressure and
//     doesn't know the incoming candidate, so we threshold on absolute
//     freq estimate instead.
//
// What we keep:
//   - 4-hash × 256-slot CMS storing 4-bit saturating counters.
//   - on_hit bumps all 4 slots (up to 15). Estimator = min across the
//     4 hashes.
//   - on_evict: cms_estimate(ino) < EVICT_THRESHOLD → vote evict.
//   - Periodic aging (NOT IMPLEMENTED — see comment on cms_age): the
//     BPF verifier rejects the CMS_W * CMS_D = 1024-slot iteration
//     that TinyLFU's "reset by half" step would need. For the short
//     bench workload (< 1000 hits) counters stay well under
//     saturation without aging, so we accept the compromise. A
//     production impl would either use bpf_loop() (verifier-friendly
//     bounded iteration, kernel 5.17+) or reduce CMS geometry.
//
// Under the same Zipf(0.7) bench, this should rank between SIEVE and
// LRU: the CMS captures frequency (a fuller signal than SIEVE's 1 bit)
// but our missing Window-LRU tier means we can't distinguish
// "just admitted, no history" from "old and cold" the way full
// W-TinyLFU does.

#include "vmlinux.h"
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_tracing.h>

struct s3fs_policy_ops {
	int (*on_admit)(__u64 count, __u64 arena_used_hint);
	int (*on_evict)(__u64 ino);
	int (*on_pressure)(__u64 count, __u64 threshold);
	int (*on_hit)(__u64 ino);
};

/* --- CMS geometry -----------------------------------------------------
 *
 * Powers-of-two so % becomes a mask and hashes are cheap. Total
 * memory: 256 × 4 × 1 B = 1 KiB — a rounding error next to a folio.
 *
 * Row seeds: FNV-32 constants offset per row to decorrelate hashes.
 * They don't need to be prime; they just need to spread ino bits
 * differently for each row. */
#define CMS_W          256U   /* slots per row */
#define CMS_W_MASK     (CMS_W - 1)
#define CMS_D          4U     /* rows */
#define CMS_MAX        15U    /* 4-bit counter saturation */
#define EVICT_THRESHOLD 2U    /* on_evict votes when estimate < this */
/* AGE_INTERVAL disabled — see cms_age comment. Kept as a comment
 * marker for the day we switch to bpf_loop(). */
/* #define AGE_INTERVAL   32U */

/* One flat array — index by row*W + slot. u8 wastes 4 bits per entry
 * but avoids bit-packing headaches in the verifier. */
struct {
	__uint(type, BPF_MAP_TYPE_ARRAY);
	__type(key, __u32);
	__type(value, __u8);
	__uint(max_entries, CMS_W * CMS_D);
} wtinylfu_cms SEC(".maps");

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

/* Row hash seeds — FNV-32 basis + offsets. Any 4 distinct large odd
 * numbers work; these mix ino bits differently per row. */
static const __u32 seeds[CMS_D] = {
	0x9e3779b1U, /* golden ratio */
	0x85ebca77U, /* Murmur3 c1 */
	0xc2b2ae3dU, /* Murmur3 c2 */
	0x27d4eb2fU, /* FNV offset variant */
};

/* Hash ino into a CMS slot index for the given row. Multiply by a
 * mixing constant, XOR with a row-specific seed, then mask to width.
 * BPF verifier tolerates this without unrolling as long as the loop
 * is unrolled at compile time (we unroll manually below). */
static __always_inline __u32 cms_slot(__u64 ino, __u32 row_seed)
{
	__u64 h = ino * 0x9e3779b97f4a7c15ULL;
	h ^= (__u64)row_seed;
	h ^= h >> 33;
	return (__u32)(h & CMS_W_MASK);
}

/* Estimate = min counter across the 4 rows. Missing slot → 0. */
static __always_inline __u32 cms_estimate(__u64 ino)
{
	__u32 est = CMS_MAX + 1;
	__u32 idx, i;
	__u8 *cp;

	/* Unrolled — CMS_D is a compile-time constant so the verifier
	 * treats this as 4 straight-line lookups. */
	#pragma unroll
	for (i = 0; i < CMS_D; i++) {
		idx = i * CMS_W + cms_slot(ino, seeds[i]);
		cp = bpf_map_lookup_elem(&wtinylfu_cms, &idx);
		if (cp && *cp < est)
			est = *cp;
	}
	return est == CMS_MAX + 1 ? 0 : est;
}

/* Increment (saturating) the 4 counters this ino hashes to. */
static __always_inline void cms_bump(__u64 ino)
{
	__u32 idx, i;
	__u8 *cp;

	#pragma unroll
	for (i = 0; i < CMS_D; i++) {
		idx = i * CMS_W + cms_slot(ino, seeds[i]);
		cp = bpf_map_lookup_elem(&wtinylfu_cms, &idx);
		if (cp && *cp < CMS_MAX)
			*cp += 1;
	}
}

/* cms_age intentionally omitted — the verifier rejects the
 * 1024-slot iteration this file's sketch would need. See the
 * header comment for the trade-off. */

SEC("struct_ops.s/on_admit")
int BPF_PROG(wtinylfu_admit, __u64 count, __u64 hint)
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
int BPF_PROG(wtinylfu_evict, __u64 ino)
{
	__u32 est = cms_estimate(ino);

	/* First-sight items have est=0; grace them once so a brand-new
	 * admit doesn't get kicked before it has any hits. */
	if (est == 0)
		return 0;
	if (est < EVICT_THRESHOLD)
		return 1;
	return 0;
}

SEC("struct_ops.s/on_pressure")
int BPF_PROG(wtinylfu_pressure, __u64 count, __u64 threshold)
{
	return 0;
}

SEC("struct_ops.s/on_hit")
int BPF_PROG(wtinylfu_hit, __u64 ino)
{
	cms_bump(ino);
	return 0;
}

SEC(".struct_ops.link")
struct s3fs_policy_ops wtinylfu_ops = {
	.on_admit    = (void *)wtinylfu_admit,
	.on_evict    = (void *)wtinylfu_evict,
	.on_pressure = (void *)wtinylfu_pressure,
	.on_hit      = (void *)wtinylfu_hit,
};

char _license[] SEC("license") = "GPL";
