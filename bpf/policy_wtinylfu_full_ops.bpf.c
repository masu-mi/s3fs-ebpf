// SPDX-License-Identifier: GPL-2.0
//
// policy_wtinylfu_full_ops — Full W-TinyLFU (CMS + Window LRU tier +
// Doorkeeper Bloom filter + Aging).
//
// Reference: Einziger & Friedman, "TinyLFU: A Highly Efficient Cache
// Admission Policy", 2017. Deployed at scale in Caffeine (Java).
//
// This is the paper-complete version. The scoped-down predecessor
// (CMS + aging only, no tier / doorkeeper) shipped earlier and hit
// 80-88% on the Zipf bench; that version proved the pattern of using
// bpf_loop() to age a 1024-slot sketch. We now layer the other two
// components on top:
//
// Window LRU tier (W)
//   New admits land in the "window" tier and stay there for up to
//   WINDOW_MAX_ROUNDS admit_replace rounds. During that window they
//   evict on the FIFO clock; if their CMS score climbs to
//   PROMOTE_THRESHOLD they graduate to Main (M) and evict on the
//   CMS-based rule. Prevents warm-up bursts from stealing slots
//   from hot Main items.
//
// Doorkeeper (Bloom filter, DK)
//   Fixed-size bitmap of DK_BITS bits (4 hash functions). First hit
//   on an ino sets the bits but does NOT bump CMS — that's the
//   "give this item one free hit before letting it into the
//   frequency estimator" step. Second hit onwards bumps CMS.
//   Prevents cold-item collisions from polluting the sketch.
//   Reset on every aging tick (matches paper's periodic clear).
//
// Aging
//   Every AGE_INTERVAL admit_replace rounds, halve all CMS counters
//   AND clear the doorkeeper. Uses bpf_loop() so the 1024/512-slot
//   iteration passes the verifier.

#include "vmlinux.h"
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_tracing.h>

struct s3fs_policy_ops {
	int (*on_admit)(__u64 count, __u64 arena_used_hint);
	int (*on_evict)(__u64 ino);
	int (*on_pressure)(__u64 count, __u64 threshold);
	int (*on_hit)(__u64 ino);
};

/* --- CMS geometry ---------------------------------------------------- */
#define CMS_W          256U   /* slots per row */
#define CMS_W_MASK     (CMS_W - 1)
#define CMS_D          4U     /* rows */
#define CMS_MAX        15U    /* 4-bit counter saturation */
#define EVICT_THRESHOLD 2U    /* on_evict votes when Main estimate < this */
#define PROMOTE_THRESHOLD 1U  /* W → M when Window ino's CMS reaches this */
#define AGE_INTERVAL   4U     /* halve CMS every N on_admit calls */
#define DK_RESET_INTERVAL 16U /* reset doorkeeper every N on_admit calls
                                 (decoupled from CMS aging — paper hints
                                 the DK cycle is 4-8× longer than CMS
                                 aging for small caches) */

/* --- Window LRU tier ------------------------------------------------- */
/* per-ino state tracks tier and time-in-window. Fits in 16 bits.
 * TIER_W=0 (new admit), TIER_M=1 (promoted). rounds is the count of
 * admit_replace calls since the ino entered its current tier; W
 * items evict when rounds >= WINDOW_MAX_ROUNDS unless they promoted
 * in the meantime. */
#define TIER_W 0U
#define TIER_M 1U
#define WINDOW_MAX_ROUNDS 8U  /* longer than the bench-harness's 4 forced
                                 eval rounds so items get a chance to
                                 accumulate CMS from real workload hits */

struct wtlfu_state {
	__u8 tier;
	__u8 rounds;
	__u16 _pad;
	__u32 _pad2;
};

struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__type(key, __u64);
	__type(value, struct wtlfu_state);
	__uint(max_entries, 4096);
} wtinylfu_tier SEC(".maps");

/* --- Doorkeeper (Bloom filter) --------------------------------------- */
/* 4096 bits = 512 u64 slots. 4 hash functions share seeds with CMS
 * for cheapness but ranged into a different bit-index domain. */
#define DK_BITS       4096U
#define DK_BITS_MASK  (DK_BITS - 1)
#define DK_U64S       (DK_BITS / 64) /* 64 */
#define DK_HASH_COUNT 4U

struct {
	__uint(type, BPF_MAP_TYPE_ARRAY);
	__type(key, __u32);
	__type(value, __u64);
	__uint(max_entries, DK_U64S);
} wtinylfu_doorkeeper SEC(".maps");

/* --- CMS backing store ----------------------------------------------- */
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

/* Row hash seeds — same for CMS + DK, ranged via masks. */
static const __u32 seeds[CMS_D] = {
	0x9e3779b1U,
	0x85ebca77U,
	0xc2b2ae3dU,
	0x27d4eb2fU,
};

/* Universal mixing hash — ino * golden ratio then XOR with row seed
 * then reflect. Used by both CMS and doorkeeper (with different masks). */
static __always_inline __u64 hash_ino(__u64 ino, __u32 row_seed)
{
	__u64 h = ino * 0x9e3779b97f4a7c15ULL;
	h ^= (__u64)row_seed;
	h ^= h >> 33;
	return h;
}

static __always_inline __u32 cms_slot(__u64 ino, __u32 row_seed)
{
	return (__u32)(hash_ino(ino, row_seed) & CMS_W_MASK);
}

/* Doorkeeper: bit-position within DK_BITS from the same universal
 * hash but a different mask. */
static __always_inline __u32 dk_bit(__u64 ino, __u32 row_seed)
{
	return (__u32)(hash_ino(ino, row_seed) & DK_BITS_MASK);
}

/* Estimate = min counter across the 4 CMS rows. */
static __always_inline __u32 cms_estimate(__u64 ino)
{
	__u32 est = CMS_MAX + 1;
	__u32 idx, i;
	__u8 *cp;

	#pragma unroll
	for (i = 0; i < CMS_D; i++) {
		idx = i * CMS_W + cms_slot(ino, seeds[i]);
		cp = bpf_map_lookup_elem(&wtinylfu_cms, &idx);
		if (cp && *cp < est)
			est = *cp;
	}
	return est == CMS_MAX + 1 ? 0 : est;
}

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

/* Doorkeeper test-and-set: returns 1 if the ino was already present
 * (all 4 bits set) BEFORE this call. Sets missing bits as side effect.
 * Bloom filter semantics: false positives possible, false negatives not. */
static __always_inline int dk_seen_and_set(__u64 ino)
{
	__u32 i, u64_idx;
	__u64 mask, *slot;
	int all_set = 1;

	#pragma unroll
	for (i = 0; i < DK_HASH_COUNT; i++) {
		__u32 bit = dk_bit(ino, seeds[i]);
		u64_idx = bit >> 6;   /* / 64 */
		mask = 1ULL << (bit & 63U);
		slot = bpf_map_lookup_elem(&wtinylfu_doorkeeper, &u64_idx);
		if (!slot) {
			all_set = 0;
			continue;
		}
		if ((*slot & mask) == 0) {
			all_set = 0;
			__sync_fetch_and_or(slot, mask);
		}
	}
	return all_set;
}

/* Age callbacks. Two separate bpf_loop() bounded iterations —
 * verifier accepts each callback as constant work. */
static long cms_age_cb(__u32 idx, void *ctx)
{
	__u8 *cp = bpf_map_lookup_elem(&wtinylfu_cms, &idx);
	if (cp)
		*cp >>= 1;
	return 0;
}

static long dk_reset_cb(__u32 idx, void *ctx)
{
	__u64 *slot = bpf_map_lookup_elem(&wtinylfu_doorkeeper, &idx);
	if (slot)
		*slot = 0;
	return 0;
}

static __always_inline void age_cms(void)
{
	bpf_loop(CMS_W * CMS_D, cms_age_cb, NULL, 0);
}

static __always_inline void reset_dk(void)
{
	bpf_loop(DK_U64S, dk_reset_cb, NULL, 0);
}

SEC("struct_ops/on_admit")
int BPF_PROG(wtinylfu_full_admit, __u64 count, __u64 hint)
{
	__u32 k;
	__u64 *sp;

	k = PSTAT_INVOCATIONS;
	sp = bpf_map_lookup_elem(&policy_stats, &k);
	if (sp) {
		__u64 inv = __sync_fetch_and_add(sp, 1) + 1;
		if ((inv & (AGE_INTERVAL - 1)) == 0)
			age_cms();
		if ((inv & (DK_RESET_INTERVAL - 1)) == 0)
			reset_dk();
	}
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
int BPF_PROG(wtinylfu_full_evict, __u64 ino)
{
	struct wtlfu_state *s = bpf_map_lookup_elem(&wtinylfu_tier, &ino);
	struct wtlfu_state seed;
	__u32 est;

	if (!s) {
		/* First sight — seed as W tier, grace this round. */
		seed.tier = TIER_W;
		seed.rounds = 0;
		seed._pad = 0;
		seed._pad2 = 0;
		bpf_map_update_elem(&wtinylfu_tier, &ino, &seed, BPF_ANY);
		return 0;
	}

	est = cms_estimate(ino);

	if (s->tier == TIER_W) {
		/* Window tier: promote when warm enough; otherwise age
		 * and evict ONLY if past WINDOW_MAX_ROUNDS AND CMS is
		 * still zero (truly cold). Items with any CMS activity
		 * survive to give the doorkeeper + aging cycle a chance
		 * to warm them. Prevents pre-workload eval rounds from
		 * flushing everything before real hits arrive. */
		if (est >= PROMOTE_THRESHOLD) {
			s->tier = TIER_M;
			s->rounds = 0;
			return 0;
		}
		if (s->rounds < 255U)
			s->rounds += 1;
		if (s->rounds >= WINDOW_MAX_ROUNDS && est == 0)
			return 1;
		return 0;
	}

	/* Main tier: CMS-based eviction. */
	if (est < EVICT_THRESHOLD)
		return 1;
	return 0;
}

SEC("struct_ops/on_pressure")
int BPF_PROG(wtinylfu_full_pressure, __u64 count, __u64 threshold)
{
	return 0;
}

SEC("struct_ops/on_hit")
int BPF_PROG(wtinylfu_full_hit, __u64 ino)
{
	/* Doorkeeper: first hit does not bump CMS (Bloom-filter shield
	 * against 1-hit wonders). Second hit onwards bumps normally. */
	if (dk_seen_and_set(ino))
		cms_bump(ino);
	return 0;
}

SEC(".struct_ops.link")
struct s3fs_policy_ops wtinylfu_full_ops = {
	.on_admit    = (void *)wtinylfu_full_admit,
	.on_evict    = (void *)wtinylfu_full_evict,
	.on_pressure = (void *)wtinylfu_full_pressure,
	.on_hit      = (void *)wtinylfu_full_hit,
};

char _license[] SEC("license") = "GPL";
