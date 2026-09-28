// SPDX-License-Identifier: GPL-2.0
//
// policy_size_aware_ops — SIEVE with size-biased eviction. Demo for
// the per-ino KFunc `s3fs_kfunc_ino_size()` (P-08 extension).
//
// Motivation: on our current arch, eviction removes the admit-hash
// entry but does NOT free arena bytes (arena high-water is
// monotonic — see backlog H-03). So "big" and "small" cost the same
// to keep. But eviction pushes the ino back into the miss path,
// which for a large file means a large S3 GET on next access. If
// bandwidth (S3 → daemon) is the bottleneck rather than admit-hash
// capacity, preferring to evict SMALL files (cheap to re-fetch)
// makes tail latency better; preferring to evict LARGE files
// (cheap to keep in the arena, but tail-costly if re-fetched) makes
// warm-set hit rate better on skewed workloads.
//
// This policy chooses the second: at high pressure, big files stay,
// small ones evict eagerly. It's a demo, not a recommendation — the
// right bias depends heavily on workload shape.
//
// Compared with plain SIEVE:
//   - Small file (< SMALL_THRESHOLD): SIEVE eviction as usual
//   - Large file (>= SMALL_THRESHOLD): grant an extra chance —
//     visited=1 → clear, visited=0 → clear (not evict), require
//     TWO consecutive on_evict rounds without a hit before evicting.
//
// KFunc usage smoke:
//   /metrics gets `policy_last_admit_count` from on_admit (normal),
//   plus we stash `policy_last_ino_size` on evict decisions for a
//   visible KFunc-call proof.

#include "vmlinux.h"
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_tracing.h>

extern __u32 s3fs_kfunc_admit_count(void) __ksym;
extern __u32 s3fs_kfunc_softcap(void)     __ksym;
extern __u64 s3fs_kfunc_ino_size(__u64)   __ksym;

struct s3fs_policy_ops {
	int (*on_admit)(__u64 count, __u64 arena_used_hint);
	int (*on_evict)(__u64 ino);
	int (*on_pressure)(__u64 count, __u64 threshold);
	int (*on_hit)(__u64 ino);
};

/* SIEVE-shaped visited state + a "big miss" counter tracked per ino.
 * Two rounds without a hit are required to evict a big file. */
struct size_aware_state {
	__u8 visited;    /* SIEVE bit: set on hit, cleared on evict-poll */
	__u8 big_misses; /* # consecutive evict-polls without hit, for
	                  * files >= SMALL_THRESHOLD */
	__u16 _pad;
	__u32 _pad2;
};

struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__type(key, __u64);
	__type(value, struct size_aware_state);
	__uint(max_entries, 4096);
} size_aware_state SEC(".maps");

enum {
	PSTAT_INVOCATIONS  = 0,
	PSTAT_LAST_COUNT   = 1,
	PSTAT_LAST_HINT    = 2,
	PSTAT_LAST_INO_SZ  = 3, /* last non-zero ino_size returned */
	PSTAT_MAX,
};

struct {
	__uint(type, BPF_MAP_TYPE_ARRAY);
	__type(key, __u32);
	__type(value, __u64);
	__uint(max_entries, PSTAT_MAX);
} policy_stats SEC(".maps");

/* 4 KiB. Files below this evict on the first SIEVE-clean pass; files
 * above get an extra grace round. Tune per-workload. Small enough
 * that our 44-byte bench files land solidly in the "small" bucket. */
#define SMALL_THRESHOLD 4096ULL
#define BIG_MISS_CAP    2U

SEC("struct_ops.s/on_admit")
int BPF_PROG(size_aware_admit, __u64 count, __u64 hint)
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
int BPF_PROG(size_aware_evict, __u64 ino)
{
	struct size_aware_state *s = bpf_map_lookup_elem(&size_aware_state, &ino);
	__u64 size;
	__u32 k;
	__u64 *sp;
	struct size_aware_state seed;

	if (!s) {
		/* First sight — seed visited=1, keep. */
		seed.visited = 1;
		seed.big_misses = 0;
		seed._pad = 0;
		seed._pad2 = 0;
		bpf_map_update_elem(&size_aware_state, &ino, &seed, BPF_ANY);
		return 0;
	}

	/* Per-ino size via KFunc. 0 means "not admitted", which
	 * shouldn't happen inside on_evict (kmod iterates admits) but
	 * defend anyway. */
	size = s3fs_kfunc_ino_size(ino);
	if (size > 0) {
		k = PSTAT_LAST_INO_SZ;
		sp = bpf_map_lookup_elem(&policy_stats, &k);
		if (sp)
			*sp = size;
	}

	if (s->visited) {
		/* Regardless of size: visited=1 → clear (SIEVE
		 * second-chance shape). Reset big-miss counter — a hit
		 * "restores confidence". */
		s->visited = 0;
		s->big_misses = 0;
		return 0;
	}

	/* visited=0 branch */
	if (size >= SMALL_THRESHOLD) {
		/* Big file: burn a big-miss slot, only evict after
		 * BIG_MISS_CAP consecutive uneventful passes. */
		if (s->big_misses < BIG_MISS_CAP) {
			s->big_misses += 1;
			return 0;
		}
	}
	/* Evict — small file, or big file that exhausted its extra
	 * chances. Delete state so a re-admit restarts the grace. */
	bpf_map_delete_elem(&size_aware_state, &ino);
	return 1;
}

SEC("struct_ops.s/on_pressure")
int BPF_PROG(size_aware_pressure, __u64 count, __u64 threshold)
{
	return 0;
}

SEC("struct_ops.s/on_hit")
int BPF_PROG(size_aware_hit, __u64 ino)
{
	struct size_aware_state *s = bpf_map_lookup_elem(&size_aware_state, &ino);
	struct size_aware_state seed;

	if (s) {
		s->visited = 1;
		s->big_misses = 0;
		return 0;
	}
	seed.visited = 1;
	seed.big_misses = 0;
	seed._pad = 0;
	seed._pad2 = 0;
	bpf_map_update_elem(&size_aware_state, &ino, &seed, BPF_ANY);
	return 0;
}

SEC(".struct_ops.link")
struct s3fs_policy_ops size_aware_ops = {
	.on_admit    = (void *)size_aware_admit,
	.on_evict    = (void *)size_aware_evict,
	.on_pressure = (void *)size_aware_pressure,
	.on_hit      = (void *)size_aware_hit,
};

char _license[] SEC("license") = "GPL";
