// SPDX-License-Identifier: GPL-2.0
//
// policy_freq2bit_ops — struct_ops sibling of policy_freq2bit.
//
// **Naming honesty**: the previous name was "policy_s3fifo_ops" but
// this is not the S3-FIFO of Yang et al (SOSP 2023) — it lacks the
// {Small, Main, Ghost} FIFO split that gives real S3-FIFO its
// performance. What lives here is a per-ino 2-bit reference counter
// with decrement-on-evict — closer in shape to Generalized CLOCK.
// See bpf/policy_s3fifo_ops.bpf.c for the actual 3-tier S3-FIFO.
//
// Same behavior as policy_freq2bit.bpf.c, exposed via
// SEC(".struct_ops.link") so the whole policy hot-swaps as one
// vtable via link.AttachStructOps.

#include "vmlinux.h"
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_tracing.h>

struct s3fs_policy_ops {
	int (*on_admit)(__u64 count, __u64 arena_used_hint);
	int (*on_evict)(__u64 ino);
	int (*on_pressure)(__u64 count, __u64 threshold);
	int (*on_hit)(__u64 ino);
};

#define FREQ_MAX 3

/* Per-ino 2-bit saturating frequency counter, same shape as the
 * fmod_ret variant. Missing key = never admitted. */
struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__type(key, __u64);
	__type(value, __u8);
	__uint(max_entries, 4096);
} freq2bit_counter SEC(".maps");

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

SEC("struct_ops.s/on_admit")
int BPF_PROG(freq2bit_admit, __u64 count, __u64 hint)
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
int BPF_PROG(freq2bit_evict, __u64 ino)
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

SEC("struct_ops.s/on_pressure")
int BPF_PROG(freq2bit_pressure, __u64 count, __u64 threshold)
{
	return 0;
}

SEC("struct_ops.s/on_hit")
int BPF_PROG(freq2bit_hit, __u64 ino)
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

SEC(".struct_ops.link")
struct s3fs_policy_ops freq2bit_ops = {
	.on_admit    = (void *)freq2bit_admit,
	.on_evict    = (void *)freq2bit_evict,
	.on_pressure = (void *)freq2bit_pressure,
	.on_hit      = (void *)freq2bit_hit,
};

char _license[] SEC("license") = "GPL";
