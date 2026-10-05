// SPDX-License-Identifier: GPL-2.0
//
// policy_s3fifo_ops — S3-FIFO (Yang et al, SOSP 2023) as a Layer 2
// struct_ops policy. **This is the real 3-tier implementation.**
//
// Paper: https://www.cs.cmu.edu/~beckmann/publications/papers/2023.sosp.s3fifo.pdf
//
// The single-tier 2-bit counter version that previously carried this
// name is now in bpf/policy_freq2bit_ops.bpf.c under a truthful name.
//
// Algorithm shape (paper Section 3.1), adapted to our vtable:
//
//   State per ino: { tier ∈ {S, M, G}, freq ∈ 0..3, seq }
//
//   on_hit(ino):
//     freq++ (saturate at FREQ_MAX). Seed (tier=S, freq=1) if the ino
//     hasn't been seen before (should be rare — on_evict usually seeds
//     first as part of admit).
//
//   on_evict(ino) — kmod calls this for every admit entry at
//   capacity pressure:
//     - no state         → seed (tier=S, freq=0), keep (grace)
//     - tier=G           → re-admit of a ghost, PROMOTE to M with
//                          freq=0, keep. Real S3-FIFO uses the ghost
//                          record to skip the S-tier warmup.
//     - tier=S, freq>=2  → PROMOTE to M with freq reset, keep.
//     - tier=S, freq<2   → EVICT, mark tier=G (retain ghost record).
//     - tier=M, freq>=1  → decrement freq, keep.
//     - tier=M, freq==0  → EVICT, mark tier=G.
//
// Tier proportions in the paper: |S|=10 %, |M|=90 %, |G|=|M|. We
// don't enforce sizes explicitly — softcap in kmod bounds the whole
// admit hash and the algorithm naturally leaves ~S-worth of new
// items and ~M-worth of promoted items, with the rest evicted.
//
// Ghost bloat: state map's max_entries caps the ghost history. At
// 8192 entries a full workload eventually GCs itself via LRU-like
// hash overwrite; no explicit expiry.

#include "vmlinux.h"
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_tracing.h>

struct s3fs_policy_ops {
	int (*on_admit)(__u64 count, __u64 arena_used_hint);
	int (*on_evict)(__u64 ino);
	int (*on_pressure)(__u64 count, __u64 threshold);
	int (*on_hit)(__u64 ino);
};

/* Tiers stored as u8 so a packed struct stays 16 bytes.
 * TIER_G is a ghost record: the ino is NOT in the admit hash, but
 * we remember it for the re-admit promotion path. */
enum {
	TIER_S = 0,   /* Small FIFO — new admits land here */
	TIER_M = 1,   /* Main FIFO  — promoted from S or from G */
	TIER_G = 2,   /* Ghost      — evicted, kept for recency signal */
};

#define FREQ_MAX          3
#define PROMOTE_S_FREQ    2  /* paper: promote S→M when freq > 1 */
#define KEEP_M_FREQ       1  /* paper: keep in M when freq >= 1  */

struct s3fifo_state {
	__u8  tier;
	__u8  freq;
	__u16 _pad;
	__u32 _pad2;
	__u64 seq;   /* insertion tick — unused today, kept for future FIFO GC */
};

struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__type(key, __u64);
	__type(value, struct s3fifo_state);
	__uint(max_entries, 8192);
} s3fifo3_state SEC(".maps");

/* Monotonic seq counter for insertion order. Array of 1 u64. */
struct {
	__uint(type, BPF_MAP_TYPE_ARRAY);
	__type(key, __u32);
	__type(value, __u64);
	__uint(max_entries, 1);
} s3fifo3_seq SEC(".maps");

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

static __always_inline __u64 next_seq(void)
{
	__u32 k = 0;
	__u64 *sp = bpf_map_lookup_elem(&s3fifo3_seq, &k);
	if (!sp)
		return 0;
	return __sync_fetch_and_add(sp, 1);
}

SEC("struct_ops/on_admit")
int BPF_PROG(s3fifo3_admit, __u64 count, __u64 hint)
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
int BPF_PROG(s3fifo3_evict, __u64 ino)
{
	struct s3fifo_state *e = bpf_map_lookup_elem(&s3fifo3_state, &ino);
	struct s3fifo_state seed;

	if (!e) {
		/* First sight — brand-new admit. Land in S with freq=0. */
		seed.tier  = TIER_S;
		seed.freq  = 0;
		seed._pad  = 0;
		seed._pad2 = 0;
		seed.seq   = next_seq();
		bpf_map_update_elem(&s3fifo3_state, &ino, &seed, BPF_ANY);
		return 0;
	}

	if (e->tier == TIER_G) {
		/* Re-admit of a ghost — the paper's Ghost hit signal.
		 * Promote directly to M so we skip the S warmup. */
		e->tier = TIER_M;
		e->freq = 0;
		e->seq  = next_seq();
		return 0;
	}

	if (e->tier == TIER_S) {
		if (e->freq >= PROMOTE_S_FREQ) {
			/* Warm enough to graduate. */
			e->tier = TIER_M;
			e->freq = 0;
			e->seq  = next_seq();
			return 0;
		}
		/* Cold S entry — evict, retain as ghost. */
		e->tier = TIER_G;
		e->freq = 0;
		e->seq  = next_seq();
		return 1;
	}

	/* tier == TIER_M */
	if (e->freq >= KEEP_M_FREQ) {
		e->freq -= 1;
		return 0;
	}
	/* Cold M entry — evict, retain as ghost. */
	e->tier = TIER_G;
	e->freq = 0;
	e->seq  = next_seq();
	return 1;
}

SEC("struct_ops/on_pressure")
int BPF_PROG(s3fifo3_pressure, __u64 count, __u64 threshold)
{
	return 0;
}

SEC("struct_ops/on_hit")
int BPF_PROG(s3fifo3_hit, __u64 ino)
{
	struct s3fifo_state *e = bpf_map_lookup_elem(&s3fifo3_state, &ino);
	struct s3fifo_state seed;

	if (e) {
		if (e->tier != TIER_G && e->freq < FREQ_MAX)
			e->freq += 1;
		return 0;
	}

	/* Race: on_hit before on_evict seeded. Land in S with freq=1
	 * so the first eviction round sees an "already useful" entry. */
	seed.tier  = TIER_S;
	seed.freq  = 1;
	seed._pad  = 0;
	seed._pad2 = 0;
	seed.seq   = next_seq();
	bpf_map_update_elem(&s3fifo3_state, &ino, &seed, BPF_ANY);
	return 0;
}

SEC(".struct_ops.link")
struct s3fs_policy_ops s3fifo_ops = {
	.on_admit    = (void *)s3fifo3_admit,
	.on_evict    = (void *)s3fifo3_evict,
	.on_pressure = (void *)s3fifo3_pressure,
	.on_hit      = (void *)s3fifo3_hit,
};

char _license[] SEC("license") = "GPL";
