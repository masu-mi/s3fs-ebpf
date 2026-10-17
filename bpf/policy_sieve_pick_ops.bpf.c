// SPDX-License-Identifier: GPL-2.0
//
// policy_sieve_pick_ops — SIEVE (NSDI 2024, Zhang et al) with a real FIFO
// and hand, on the AC-58 vtable (docs/design/ac58-pick-victim.md).
//
//   on_insert   append ino at the head of the FIFO (visited = 0); idempotent
//   on_hit      visited = 1
//   pick_victim move the hand from the oldest item toward the newest; a
//               visited item is cleared and spared, the first unvisited one
//               is the victim (removed from the FIFO).  At the newest end
//               the hand wraps to the oldest.  New items never move the hand
//               and there is no promotion.
//   on_remove   the kmod evicted ino by another route
//
// The FIFO is a ring of N slots addressed by a monotonically growing seq
// (slot = seq % N).  on_insert / on_remove / pick_victim are serialized by
// the kmod (s3fs_policy_evict_lock), so they take no lock; on_hit runs
// concurrently and only stores a 1 into a slot (a lost update is harmless).

#include "vmlinux.h"
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_tracing.h>

#define N 8192

struct sv_slot {
	__u64 ino;
	__u32 visited;
	__u32 valid;
};

struct {
	__uint(type, BPF_MAP_TYPE_ARRAY);
	__type(key, __u32);
	__type(value, struct sv_slot);
	__uint(max_entries, N);
} slots SEC(".maps");

/* ino -> seq */
struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__type(key, __u64);
	__type(value, __u64);
	__uint(max_entries, N);
} idx SEC(".maps");

struct sv_state {
	__u64 head; /* next seq to assign */
	__u64 tail; /* oldest seq that may still be valid */
	__u64 hand; /* next seq the hand examines */
};

struct {
	__uint(type, BPF_MAP_TYPE_ARRAY);
	__type(key, __u32);
	__type(value, struct sv_state);
	__uint(max_entries, 1);
} st SEC(".maps");

/* Same slots as sibling policies so the daemon's Prometheus mirror works. */
enum { PSTAT_INVOCATIONS = 0, PSTAT_LAST_COUNT = 1, PSTAT_LAST_HINT = 2,
       PSTAT_MAX };

struct {
	__uint(type, BPF_MAP_TYPE_ARRAY);
	__type(key, __u32);
	__type(value, __u64);
	__uint(max_entries, PSTAT_MAX);
} policy_stats SEC(".maps");

/* Mirror kmod/policy_struct_ops.h. Field names bind by BTF at load. */
struct s3fs_policy_ops {
	int (*on_admit)(__u64 count, __u64 arena_used_hint);
	int (*on_evict)(__u64 ino);
	int (*on_pressure)(__u64 count, __u64 threshold);
	int (*on_hit)(__u64 ino);
	int (*on_insert)(__u64 ino);
	int (*on_remove)(__u64 ino);
	__u64 (*pick_victim)(void);
};

static __always_inline struct sv_state *get_state(void)
{
	__u32 z = 0;

	return bpf_map_lookup_elem(&st, &z);
}

static __always_inline struct sv_slot *get_slot(__u64 seq)
{
	__u32 k = (__u32)(seq % N);

	return bpf_map_lookup_elem(&slots, &k);
}

/* Move tail past invalid slots (bounded). */
struct sv_skip_ctx {
	struct sv_state *s;
};

static long skip_invalid(__u64 i, void *data)
{
	struct sv_skip_ctx *c = data;
	struct sv_slot *sl;

	if (c->s->tail >= c->s->head)
		return 1;
	sl = get_slot(c->s->tail);
	if (!sl || sl->valid)
		return 1;
	c->s->tail++;
	return 0;
}

static __always_inline void advance_tail(struct sv_state *s)
{
	struct sv_skip_ctx c = { .s = s };

	bpf_loop(N, skip_invalid, &c, 0);
}

SEC("struct_ops/on_admit")
int BPF_PROG(sp_admit, __u64 count, __u64 hint)
{
	__u32 k = PSTAT_INVOCATIONS;
	__u64 *sp = bpf_map_lookup_elem(&policy_stats, &k);

	if (sp)
		__sync_fetch_and_add(sp, 1);
	return 0;
}

/* The score path is unused when pick_victim answers; keep it neutral. */
SEC("struct_ops/on_evict")
int BPF_PROG(sp_evict, __u64 ino)
{
	return 0;
}

SEC("struct_ops/on_pressure")
int BPF_PROG(sp_pressure, __u64 count, __u64 threshold)
{
	return 0;
}

SEC("struct_ops/on_hit")
int BPF_PROG(sp_hit, __u64 ino)
{
	__u64 *seq = bpf_map_lookup_elem(&idx, &ino);
	struct sv_slot *sl;

	if (!seq)
		return 0;
	sl = get_slot(*seq);
	if (sl && sl->ino == ino && sl->valid && !sl->visited)
		sl->visited = 1;
	return 0;
}

SEC("struct_ops/on_insert")
int BPF_PROG(sp_insert, __u64 ino)
{
	struct sv_state *s = get_state();
	struct sv_slot *sl;
	__u64 seq;

	if (!s || bpf_map_lookup_elem(&idx, &ino))
		return 0;
	if (s->head - s->tail >= N) {
		/* Full: drop the oldest. */
		sl = get_slot(s->tail);
		if (sl && sl->valid) {
			bpf_map_delete_elem(&idx, &sl->ino);
			sl->valid = 0;
		}
		s->tail++;
	}
	seq = s->head++;
	sl = get_slot(seq);
	if (!sl)
		return 0;
	sl->ino = ino;
	sl->visited = 0;
	sl->valid = 1;
	bpf_map_update_elem(&idx, &ino, &seq, BPF_ANY);
	return 0;
}

SEC("struct_ops/on_remove")
int BPF_PROG(sp_remove, __u64 ino)
{
	__u64 *seq = bpf_map_lookup_elem(&idx, &ino);
	struct sv_slot *sl;

	if (!seq)
		return 0;
	sl = get_slot(*seq);
	if (sl && sl->ino == ino)
		sl->valid = 0;
	bpf_map_delete_elem(&idx, &ino);
	return 0;
}

struct sv_pick_ctx {
	struct sv_state *s;
	__u64 victim;
};

static long pick_step(__u64 i, void *data)
{
	struct sv_pick_ctx *c = data;
	struct sv_state *s = c->s;
	struct sv_slot *sl;

	if (s->hand < s->tail)
		s->hand = s->tail;
	if (s->hand >= s->head) {
		/* Reached the newest end: wrap to the oldest. */
		s->hand = s->tail;
		if (s->hand >= s->head)
			return 1; /* empty */
	}
	sl = get_slot(s->hand);
	if (!sl || !sl->valid) {
		s->hand++;
		return 0;
	}
	if (sl->visited) {
		sl->visited = 0;
		s->hand++;
		return 0;
	}
	c->victim = sl->ino;
	sl->valid = 0;
	bpf_map_delete_elem(&idx, &c->victim);
	s->hand++;
	return 1;
}

SEC("struct_ops/pick_victim")
int BPF_PROG(sp_pick)
{
	struct sv_state *s = get_state();
	struct sv_pick_ctx c = { .s = s, .victim = 0 };

	if (!s)
		return 0;
	/* At most two sweeps: one to clear visited bits, one to find a victim. */
	bpf_loop(2 * N + 1, pick_step, &c, 0);
	advance_tail(s);
	return c.victim;
}

SEC(".struct_ops.link")
struct s3fs_policy_ops sieve_pick_ops = {
	.on_admit    = (void *)sp_admit,
	.on_evict    = (void *)sp_evict,
	.on_pressure = (void *)sp_pressure,
	.on_hit      = (void *)sp_hit,
	.on_insert   = (void *)sp_insert,
	.on_remove   = (void *)sp_remove,
	.pick_victim = (void *)sp_pick,
};

char _license[] SEC("license") = "GPL";
