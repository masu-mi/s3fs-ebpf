// SPDX-License-Identifier: GPL-2.0
//
// policy_s3fifo_ops — S3-FIFO implemented as a BPF struct_ops vtable.
//
// Same algorithm as policy_s3fifo.bpf.c but exposed via SEC(".struct_ops.link")
// so the whole policy is 1 vtable, hot-swapped by a single link.Update.
// Contrast with the fmod_ret version which needs N per-hook links.
//
// See kmod/policy_struct_ops.h for the vtable definition.

#include "vmlinux.h"
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_tracing.h>

/* Vtable layout — must mirror kmod/policy_struct_ops.h. libbpf
 * relocates via BTF against the module's actual definition at load
 * time, so field order and types matter but names are string-linked. */
struct s3fs_policy_ops {
	int (*on_admit)(__u64 count, __u64 arena_used_hint);
	int (*on_evict)(__u64 ino);
	int (*on_pressure)(__u64 count, __u64 threshold);
};

/* Per-ino 2-bit saturating frequency counter, same shape as the
 * fmod_ret variant. Missing key = never admitted. */
struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__type(key, __u64);
	__type(value, __u8);
	__uint(max_entries, 4096);
} s3fifo_freq SEC(".maps");

/* Aggregate stats mirrored to daemon via periodic read. */
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

/* struct_ops function bodies. Signatures MUST match the kmod-side
 * `struct s3fs_policy_ops` (kmod/policy_struct_ops.h). Verifier
 * validates against the CFI stubs registered by the kmod. */

SEC("struct_ops.s/on_admit")
int BPF_PROG(s3fifo_admit, __u64 count, __u64 hint)
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
int BPF_PROG(s3fifo_evict, __u64 ino)
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

SEC("struct_ops.s/on_pressure")
int BPF_PROG(s3fifo_pressure, __u64 count, __u64 threshold)
{
	return 0;
}

/* The struct_ops vtable. Field names must match kmod's `struct
 * s3fs_policy_ops` — verifier binds function pointers via BTF. */
SEC(".struct_ops.link")
struct s3fs_policy_ops s3fifo_ops = {
	.on_admit    = (void *)s3fifo_admit,
	.on_evict    = (void *)s3fifo_evict,
	.on_pressure = (void *)s3fifo_pressure,
};

char _license[] SEC("license") = "GPL";
