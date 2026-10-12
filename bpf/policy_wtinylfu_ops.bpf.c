// SPDX-License-Identifier: GPL-2.0
//
// policy_wtinylfu_ops — W-TinyLFU-like eviction policy (scoped).
//
// Reference: Einziger & Friedman, "TinyLFU: A Highly Efficient Cache
// Admission Policy", 2017. Deployed at scale in Caffeine (Java).
//
// **Paper-complete impl lives in policy_wtinylfu_full_ops.bpf.c**
// (Window LRU tier + Doorkeeper + Aging). This "scoped" variant
// keeps only the centerpiece (CMS + aging) which turns out to be
// the strongest choice on our hit-rate bench (docs/journal/hit-rate-bench.md:
// scoped 80-88 % vs full 49 %). The full variant honors the paper
// but its Window tier + doorkeeper filter useful signal on
// small-cache workloads with no one-hit wonders — see the bench
// caveats section.
//
// We drop (all captured in the full variant):
//
//   - Window LRU tier: the paper puts new admits into a small W-LRU
//     (~1 %) before they compete for admission to Main. We don't have
//     a window tier here; new admits land in the same pool.
//   - Doorkeeper (Bloom filter for 1-hit wonders): omitted; aging
//     picks up part of the same slack by preventing counter
//     saturation on cold-item collisions.
//   - TinyLFU's admission decision (evict-if-victim-freq < candidate-
//     freq): our on_evict is called per-ino at capacity pressure and
//     doesn't know the incoming candidate, so we threshold on
//     absolute freq estimate instead.
//
// What we keep:
//   - 4-hash × 256-slot CMS storing 4-bit saturating counters.
//   - on_hit bumps all 4 slots (up to 15). Estimator = min across the
//     4 hashes.
//   - on_evict: cms_estimate(ino) < EVICT_THRESHOLD → vote evict.
//   - Periodic aging (X-03): every AGE_INTERVAL_HITS on_hit calls,
//     halve every CMS counter via bpf_loop(). Driven by actual hit
//     count rather than admit rounds so bench forced evals and daemon
//     sync cycles do not trigger spurious aging on cold-cache workloads.
//     Mimics TinyLFU's "reset by half" step.

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
#define EVICT_THRESHOLD 1U    /* evict only est==0 (never hit); est≥1 = keep */
#define AGE_INTERVAL_HITS_DEFAULT 512U /* default; overridable via wtinylfu_config[0] (Z-01) */

/* One flat array — index by row*W + slot. u8 wastes 4 bits per entry
 * but avoids bit-packing headaches in the verifier. */
struct {
	__uint(type, BPF_MAP_TYPE_ARRAY);
	__type(key, __u32);
	__type(value, __u8);
	__uint(max_entries, CMS_W * CMS_D);
} wtinylfu_cms SEC(".maps");

enum {
	PSTAT_INVOCATIONS = 0,  /* on_admit call count */
	PSTAT_LAST_COUNT  = 1,
	PSTAT_LAST_HINT   = 2,
	PSTAT_HITS        = 3,  /* cumulative on_hit calls; drives aging (X-03) */
	PSTAT_MAX,
};

struct {
	__uint(type, BPF_MAP_TYPE_ARRAY);
	__type(key, __u32);
	__type(value, __u64);
	__uint(max_entries, PSTAT_MAX);
} policy_stats SEC(".maps");

/* Z-01: runtime-configurable age interval. Slot 0 holds age_interval_hits.
 * Default (0 or unset) → fall back to AGE_INTERVAL_HITS_DEFAULT.
 * Daemon writes via policyprog.SetWTinyLFUAgeInterval() after load. */
enum { WCFG_AGE_INTERVAL = 0, WCFG_MAX };

struct {
	__uint(type, BPF_MAP_TYPE_ARRAY);
	__type(key, __u32);
	__type(value, __u64);
	__uint(max_entries, WCFG_MAX);
} wtinylfu_config SEC(".maps");

/* Row hash seeds — FNV-32 basis + offsets. Any 4 distinct large odd
 * numbers work; these mix ino bits differently per row. */
static const __u32 seeds[CMS_D] = {
	0x9e3779b1U, /* golden ratio */
	0x85ebca77U, /* Murmur3 c1 */
	0xc2b2ae3dU, /* Murmur3 c2 */
	0x27d4eb2fU, /* FNV offset variant */
};

/* Hash ino into a CMS slot index for the given row. Multiply by a
 * mixing constant, XOR with a row-specific seed, then mask to width. */
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

/* bpf_loop() callback for cms_age. Halves the counter at `idx`.
 * Returning 0 tells the helper to continue; non-zero would terminate
 * early. Signature matches the bpf_loop_callback_t typedef. */
static long age_slot_cb(__u32 idx, void *ctx)
{
	__u8 *cp = bpf_map_lookup_elem(&wtinylfu_cms, &idx);

	if (cp)
		*cp >>= 1;
	return 0;
}

/* Halve every CMS counter. Uses bpf_loop() to sidestep the verifier's
 * per-program instruction budget: manually unrolling 1024 iterations
 * exceeds the 1M limit, whereas bpf_loop delegates iteration to the
 * kernel with a fixed bound at runtime. Requires kernel 5.17+. */
static __always_inline void cms_age(void)
{
	bpf_loop(CMS_W * CMS_D, age_slot_cb, NULL, 0);
}

SEC("struct_ops/on_admit")
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

SEC("struct_ops/on_evict")
int BPF_PROG(wtinylfu_evict, __u64 ino)
{
	__u32 est = cms_estimate(ino);

	/* Evict only items with est==0 (never accessed since admitted).
	 * Items with est≥1 (at least one on_hit) are kept. This eliminates
	 * the est=1 eviction valley present when EVICT_THRESHOLD was 2. */
	if (est < EVICT_THRESHOLD)
		return 1;
	return 0;
}

SEC("struct_ops/on_pressure")
int BPF_PROG(wtinylfu_pressure, __u64 count, __u64 threshold)
{
	return 0;
}

SEC("struct_ops/on_hit")
int BPF_PROG(wtinylfu_hit, __u64 ino)
{
	__u32 k;
	__u64 *sp;
	__u64 age_interval;

	cms_bump(ino);

	/* Z-01: read age_interval from config map; fall back to default if
	 * not set (0 is the BPF ARRAY zero-value before daemon init).
	 * Explicit non-zero guard satisfies the BPF verifier for division. */
	k = WCFG_AGE_INTERVAL;
	sp = bpf_map_lookup_elem(&wtinylfu_config, &k);
	age_interval = (sp && *sp) ? *sp : AGE_INTERVAL_HITS_DEFAULT;
	if (!age_interval)
		age_interval = AGE_INTERVAL_HITS_DEFAULT;

	/* X-03: age CMS every age_interval total hits so aging is
	 * proportional to access frequency, not admit-round count. */
	k = PSTAT_HITS;
	sp = bpf_map_lookup_elem(&policy_stats, &k);
	if (sp) {
		__u64 hits = __sync_fetch_and_add(sp, 1) + 1;
		if (hits % age_interval == 0)
			cms_age();
	}
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
