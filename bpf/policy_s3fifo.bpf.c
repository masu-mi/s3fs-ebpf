// SPDX-License-Identifier: GPL-2.0
//
// policy_s3fifo — Layer 2 built-in policy: S3-FIFO (Yang et al SOSP 2023).
//
// S3-FIFO reference: https://blog.jasony.me/system/cache/2023/08/01/s3fifo
// PDF: https://www.cs.cmu.edu/~beckmann/publications/papers/2023.sosp.s3fifo.pdf
//
// This is the verifier-safe minimum: track per-ino 2-bit frequency
// (0..3, saturating) in a hash map. On admit, freshly-admitted inos
// enter with freq=0. On evict-poll, an ino with freq==0 votes 1
// (evict); freq > 0 decrements and votes 0 (keep — the S3-FIFO "small
// → main promotion" pattern collapsed into a single tier for spike
// simplicity).
//
// Full S3-FIFO (small/main/ghost split + main-freq threshold) is a
// followup: this file proves the shape (freq map, on_evict return
// value driving daemon-side prune) and gives a template for
// policy_lru.bpf.c / policy_sieve.bpf.c to follow.

#include "vmlinux.h"
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_tracing.h>

/* Shared stats slots — same shape as policy_observe so the daemon's
 * generic reader (daemon/internal/policyprog/) works unchanged. */
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

/* Per-ino 2-bit saturating frequency counter (stored as u8 for verifier
 * simplicity; only bottom 2 bits used). Missing key = never admitted. */
struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__type(key, __u64);
	__type(value, __u8);
	__uint(max_entries, 4096);
} s3fifo_freq SEC(".maps");

/* Prior versions kept an evict_vote map because SEC("fentry/...") is
 * required to return 0. Now that kmod registers the policy hooks with
 * register_btf_fmodret_id_set (see kmod/tree_traversal.c), we attach
 * as SEC("fmod_ret/...") instead and return the vote directly. The
 * side-channel map is gone; kmod consumes the return value. */

#define FREQ_MAX 3

/* on_admit: observation only (updates stats), always return 0. Using
 * fmod_ret keeps the shape uniform with on_evict below even though we
 * don't override the return value here. */
SEC("fmod_ret/s3fs_policy_on_admit")
int BPF_PROG(s3fifo_on_admit, __u64 count, __u64 hint)
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

/* on_evict: BPF's vote on whether `ino` should be evicted. Returned
 * directly via fmod_ret — kmod side sees this as the function's
 * actual return value (register_btf_fmodret_id_set() in kmod enables
 * this; the previous version had to side-channel via a map because
 * SEC("fentry/...") forces R0=0 at exit).
 *   freq == 0 → return 1 (evict)
 *   freq > 0  → decrement and return 0 (keep, cooldown)
 *   missing   → seed freq=0, return 0 (grace one round) */
SEC("fmod_ret/s3fs_policy_on_evict")
int BPF_PROG(s3fifo_on_evict, __u64 ino)
{
	__u8 *fp = bpf_map_lookup_elem(&s3fifo_freq, &ino);
	__u8 zero = 0;

	if (!fp) {
		bpf_map_update_elem(&s3fifo_freq, &ino, &zero, BPF_ANY);
		return 0;
	}
	if (*fp == 0)
		return 1;
	*fp = *fp - 1;
	return 0;
}

/* on_pressure: observation only for now. Return value ignored by
 * kmod side (advisory hook). */
SEC("fmod_ret/s3fs_policy_on_pressure")
int BPF_PROG(s3fifo_on_pressure, __u64 count, __u64 threshold)
{
	return 0;
}

/* on_hit: per-folio access in the read path. Increments the 2-bit
 * saturating freq (up to FREQ_MAX) so on_evict's decrement-or-vote
 * step has a meaningful signal to work with. Fires on every folio,
 * so keep it O(1). Missing key = never admitted; seed freq=1 rather
 * than 0 so a first-time hit is "known active" from the start. */
SEC("fmod_ret/s3fs_policy_on_hit")
int BPF_PROG(s3fifo_on_hit, __u64 ino)
{
	__u8 *fp = bpf_map_lookup_elem(&s3fifo_freq, &ino);
	__u8 seed = 1;

	if (!fp) {
		bpf_map_update_elem(&s3fifo_freq, &ino, &seed, BPF_ANY);
		return 0;
	}
	if (*fp < FREQ_MAX)
		*fp = *fp + 1;
	return 0;
}

char _license[] SEC("license") = "GPL";
