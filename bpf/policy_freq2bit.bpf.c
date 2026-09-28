// SPDX-License-Identifier: GPL-2.0
//
// policy_freq2bit — Layer 2 built-in policy: per-ino 2-bit reference
// counter with decrement-on-evict.
//
// **Naming honesty**: this file was previously labelled "S3-FIFO"
// but the algorithm implemented here is NOT the S3-FIFO of Yang et
// al (SOSP 2023). Real S3-FIFO relies on the {Small, Main, Ghost}
// FIFO split for its performance; this file collapses all three
// into a single per-ino counter. The result is essentially a
// Generalized CLOCK / aging-LFU variant with a 2-bit reference
// counter. See bpf/policy_s3fifo_ops.bpf.c for a real 3-tier
// S3-FIFO implementation.
//
// Semantics:
//   on_hit  : freq++ (saturate at FREQ_MAX=3), seed freq=1 on first
//             sight
//   on_evict: freq==0 → vote evict (return 1)
//             freq> 0 → decrement, vote keep (return 0)
//             missing → seed freq=0, vote keep (grace one round)
//
// This was our first-cut policy back when on_hit did not exist —
// preserving it lets P-07 bench compare "single-tier freq counter"
// vs "3-tier S3-FIFO" head-to-head and shows how much the tier
// split contributes to hit-rate.

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
} freq2bit_counter SEC(".maps");

#define FREQ_MAX 3

SEC("fmod_ret/s3fs_policy_on_admit")
int BPF_PROG(freq2bit_on_admit, __u64 count, __u64 hint)
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

SEC("fmod_ret/s3fs_policy_on_evict")
int BPF_PROG(freq2bit_on_evict, __u64 ino)
{
	__u8 *fp = bpf_map_lookup_elem(&freq2bit_counter, &ino);
	__u8 zero = 0;

	if (!fp) {
		bpf_map_update_elem(&freq2bit_counter, &ino, &zero, BPF_ANY);
		return 0;
	}
	if (*fp == 0)
		return 1;
	*fp = *fp - 1;
	return 0;
}

SEC("fmod_ret/s3fs_policy_on_pressure")
int BPF_PROG(freq2bit_on_pressure, __u64 count, __u64 threshold)
{
	return 0;
}

SEC("fmod_ret/s3fs_policy_on_hit")
int BPF_PROG(freq2bit_on_hit, __u64 ino)
{
	__u8 *fp = bpf_map_lookup_elem(&freq2bit_counter, &ino);
	__u8 seed = 1;

	if (!fp) {
		bpf_map_update_elem(&freq2bit_counter, &ino, &seed, BPF_ANY);
		return 0;
	}
	if (*fp < FREQ_MAX)
		*fp = *fp + 1;
	return 0;
}

char _license[] SEC("license") = "GPL";
