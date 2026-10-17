// SPDX-License-Identifier: GPL-2.0
//
// policy_lru_pick_ops — exact LRU on the AC-58 vtable
// (docs/design/ac58-pick-victim.md).  The older policy_lru_ops is an
// epoch-aged approximation voted per ino; this one keeps a last-access
// stamp per candidate and pick_victim returns the smallest.
//
//   on_insert   new candidate, stamp = ++clock; idempotent
//   on_hit      stamp = ++clock (atomic: on_hit runs concurrently)
//   pick_victim the valid slot with the smallest stamp, removed
//   on_remove   the kmod evicted ino by another route
//
// pick_victim scans the slots below the high-water mark (O(candidates));
// the candidate set is bounded by the cache capacity, and a round evicts a
// handful of inos.  on_insert / on_remove / pick_victim are serialized by the
// kmod (s3fs_policy_evict_lock); on_hit only stores a stamp.

#include "vmlinux.h"
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_tracing.h>

#define N 4096

struct lp_item {
	__u64 ino;
	__u64 stamp;
	__u32 valid;
	__u32 pad;
};

struct {
	__uint(type, BPF_MAP_TYPE_ARRAY);
	__type(key, __u32);
	__type(value, struct lp_item);
	__uint(max_entries, N);
} items SEC(".maps");

/* ino -> slot */
struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__type(key, __u64);
	__type(value, __u32);
	__uint(max_entries, N);
} idx SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_ARRAY);
	__type(key, __u32);
	__type(value, __u32);
	__uint(max_entries, N);
} freelist SEC(".maps");

struct lp_state {
	__u64 clock;
	__u32 hi;    /* slots [0, hi) have been handed out at least once */
	__u32 nfree; /* entries in freelist */
};

struct {
	__uint(type, BPF_MAP_TYPE_ARRAY);
	__type(key, __u32);
	__type(value, struct lp_state);
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

static __always_inline struct lp_state *get_state(void)
{
	__u32 z = 0;

	return bpf_map_lookup_elem(&st, &z);
}

static __always_inline struct lp_item *get_item(__u32 slot)
{
	return bpf_map_lookup_elem(&items, &slot);
}

static __always_inline __u64 tick(struct lp_state *s)
{
	return __sync_fetch_and_add(&s->clock, 1) + 1;
}

/* Take a slot: a freed one first, else the next never-used one; -1 if full. */
static __always_inline int alloc_slot(struct lp_state *s)
{
	__u32 k, *v;

	if (s->nfree > 0) {
		k = --s->nfree;
		v = bpf_map_lookup_elem(&freelist, &k);
		return v ? (int)*v : -1;
	}
	if (s->hi < N)
		return (int)s->hi++;
	return -1;
}

static __always_inline void free_slot(struct lp_state *s, __u32 slot)
{
	__u32 k = s->nfree, *v;

	if (k >= N)
		return;
	v = bpf_map_lookup_elem(&freelist, &k);
	if (!v)
		return;
	*v = slot;
	s->nfree++;
}

static __always_inline void drop(struct lp_state *s, __u64 ino, __u32 slot)
{
	struct lp_item *it = get_item(slot);

	if (it)
		it->valid = 0;
	bpf_map_delete_elem(&idx, &ino);
	free_slot(s, slot);
}

SEC("struct_ops/on_admit")
int BPF_PROG(lp_admit, __u64 count, __u64 hint)
{
	__u32 k = PSTAT_INVOCATIONS;
	__u64 *sp = bpf_map_lookup_elem(&policy_stats, &k);

	if (sp)
		__sync_fetch_and_add(sp, 1);
	return 0;
}

/* Unused when pick_victim answers; neutral for the score fallback. */
SEC("struct_ops/on_evict")
int BPF_PROG(lp_evict, __u64 ino)
{
	return 0;
}

SEC("struct_ops/on_pressure")
int BPF_PROG(lp_pressure, __u64 count, __u64 threshold)
{
	return 0;
}

SEC("struct_ops/on_hit")
int BPF_PROG(lp_hit, __u64 ino)
{
	__u32 *slot = bpf_map_lookup_elem(&idx, &ino);
	struct lp_state *s = get_state();
	struct lp_item *it;

	if (!slot || !s)
		return 0;
	it = get_item(*slot);
	if (it && it->valid && it->ino == ino)
		it->stamp = tick(s);
	return 0;
}

SEC("struct_ops/on_insert")
int BPF_PROG(lp_insert, __u64 ino)
{
	struct lp_state *s = get_state();
	struct lp_item *it;
	int slot;
	__u32 u;

	if (!s || !ino || bpf_map_lookup_elem(&idx, &ino))
		return 0;
	slot = alloc_slot(s);
	if (slot < 0)
		return 0;
	u = (__u32)slot;
	it = get_item(u);
	if (!it)
		return 0;
	it->ino = ino;
	it->stamp = tick(s);
	it->valid = 1;
	bpf_map_update_elem(&idx, &ino, &u, BPF_ANY);
	return 0;
}

SEC("struct_ops/on_remove")
int BPF_PROG(lp_remove, __u64 ino)
{
	__u32 *slot = bpf_map_lookup_elem(&idx, &ino);
	struct lp_state *s = get_state();

	if (slot && s)
		drop(s, ino, *slot);
	return 0;
}

struct lp_scan {
	__u64 min;
	__u32 best;
	__u32 hi;
	int found;
};

static long scan_step(__u64 i, void *data)
{
	struct lp_scan *c = data;
	struct lp_item *it;

	if (i >= c->hi)
		return 1;
	it = get_item((__u32)i);
	if (it && it->valid && it->stamp < c->min) {
		c->min = it->stamp;
		c->best = (__u32)i;
		c->found = 1;
	}
	return 0;
}

SEC("struct_ops/pick_victim")
__u64 BPF_PROG(lp_pick)
{
	struct lp_state *s = get_state();
	struct lp_scan c = { .min = ~0ULL, .best = 0, .found = 0 };
	struct lp_item *it;
	__u64 ino;

	if (!s)
		return 0;
	c.hi = s->hi;
	bpf_loop(N, scan_step, &c, 0);
	if (!c.found)
		return 0;
	it = get_item(c.best);
	if (!it)
		return 0;
	ino = it->ino;
	drop(s, ino, c.best);
	return ino;
}

SEC(".struct_ops.link")
struct s3fs_policy_ops lru_pick_ops = {
	.on_admit    = (void *)lp_admit,
	.on_evict    = (void *)lp_evict,
	.on_pressure = (void *)lp_pressure,
	.on_hit      = (void *)lp_hit,
	.on_insert   = (void *)lp_insert,
	.on_remove   = (void *)lp_remove,
	.pick_victim = (void *)lp_pick,
};

char _license[] SEC("license") = "GPL";
