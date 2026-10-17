// SPDX-License-Identifier: GPL-2.0
//
// policy_s3fifo_pick_ops — S3-FIFO (Yang et al, SOSP 2023) on the AC-58
// vtable (docs/design/ac58-pick-victim.md), with the three real queues.
//
//   S (small, ~10 %)  new candidates enter here
//   M (main)          promoted from S, or re-admitted from the ghost queue
//   G (ghost)         inos recently evicted (no data, just the name)
//
//   on_insert   a ghost -> M (freq 0); a new ino -> tail of S (freq 0)
//   on_hit      freq = min(freq + 1, 3)
//   pick_victim if S holds >= 10 % of S+M (or M is empty) look at S's oldest:
//                 freq > 1  -> move to M (freq 0), look again
//                 else      -> victim, goes to G
//               otherwise look at M's oldest:
//                 freq > 0  -> freq--, back to M's tail, look again
//                 else      -> victim, goes to G
//   on_remove   the kmod evicted ino by another route: goes to G
//
// Each queue is a ring of (seq << 32 | slot) entries.  An item carries the seq
// of its latest push, so a ring entry whose seq no longer matches is a
// tombstone and is skipped when popped (that is how a ghost hit or a removal
// leaves a queue in O(1)).  on_insert / on_remove / pick_victim are
// serialized by the kmod (s3fs_policy_evict_lock); on_hit only bumps freq.

#include "vmlinux.h"
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_tracing.h>

#define N 4096 /* slots (live items + ghosts) */
#define R N    /* ring capacity per queue */

enum { Q_S = 0, Q_M = 1, Q_G = 2, NQ = 3 };
#define FREQ_MAX 3

struct sf_item {
	__u64 ino;
	__u32 seq;   /* seq of the latest push; a ring entry must match */
	__u8  valid;
	__u8  tier;
	__u8  freq;
	__u8  pad;
};

struct {
	__uint(type, BPF_MAP_TYPE_ARRAY);
	__type(key, __u32);
	__type(value, struct sf_item);
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

/* NQ rings of R entries: key = q * R + (pos % R) */
struct {
	__uint(type, BPF_MAP_TYPE_ARRAY);
	__type(key, __u32);
	__type(value, __u64);
	__uint(max_entries, NQ * R);
} rings SEC(".maps");

struct sf_state {
	__u32 clock;
	__u32 hi;
	__u32 nfree;
	__u32 head[4]; /* NQ used; 4 so an index can be masked with & 3 */
	__u32 tail[4];
	__u32 live[4];
};

struct {
	__uint(type, BPF_MAP_TYPE_ARRAY);
	__type(key, __u32);
	__type(value, struct sf_state);
	__uint(max_entries, 1);
} st SEC(".maps");

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

static __always_inline struct sf_state *get_state(void)
{
	__u32 z = 0;

	return bpf_map_lookup_elem(&st, &z);
}

static __always_inline struct sf_item *get_item(__u32 slot)
{
	return bpf_map_lookup_elem(&items, &slot);
}

static __always_inline int alloc_slot(struct sf_state *s)
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

static __always_inline void free_slot(struct sf_state *s, __u32 slot)
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

static __always_inline void forget(struct sf_state *s, __u32 slot,
				   struct sf_item *it)
{
	bpf_map_delete_elem(&idx, &it->ino);
	it->valid = 0;
	free_slot(s, slot);
}

/* Pop the oldest ring entry of queue q.  Returns 1 and the entry, 0 if empty. */
static __always_inline int ring_pop(struct sf_state *s, __u32 q, __u64 *e)
{
	__u32 key;
	__u64 *v;

	if (q >= NQ || s->tail[q & 3] == s->head[q & 3])
		return 0;
	key = q * R + (s->tail[q & 3] % R);
	v = bpf_map_lookup_elem(&rings, &key);
	if (!v)
		return 0;
	*e = *v;
	s->tail[q & 3]++;
	return 1;
}

/* Is ring entry e (popped from queue q) the item's current position? */
static __always_inline struct sf_item *entry_item(__u64 e, __u32 q, __u32 *slot)
{
	struct sf_item *it;

	*slot = (__u32)e;
	it = get_item(*slot);
	if (!it || !it->valid || it->tier != q || it->seq != (__u32)(e >> 32))
		return NULL;
	return it;
}

/* Append slot at the tail of queue q.  A full ring drops its oldest entry; if
 * that entry was a live item, the item is forgotten. */
static __always_inline void push(struct sf_state *s, __u32 q, __u32 slot,
				 struct sf_item *it)
{
	__u32 key;
	__u64 *v;

	if (q >= NQ)
		return;
	if (s->head[q & 3] - s->tail[q & 3] >= R) {
		__u64 old;
		__u32 os;
		struct sf_item *oi;

		if (ring_pop(s, q, &old)) {
			oi = entry_item(old, q, &os);
			if (oi) {
				if (s->live[q & 3])
					s->live[q & 3]--;
				forget(s, os, oi);
			}
		}
	}
	it->seq = ++s->clock;
	it->tier = q;
	key = q * R + (s->head[q & 3] % R);
	v = bpf_map_lookup_elem(&rings, &key);
	if (!v)
		return;
	*v = ((__u64)it->seq << 32) | slot;
	s->head[q & 3]++;
	s->live[q & 3]++;
}

/* Pop the oldest live item of queue q (tombstones skipped); the caller owns
 * the item afterwards (live[q & 3] already decremented).  -1 if none. */
struct sf_pop {
	struct sf_state *s;
	__u32 q;
	int slot;
};

static long pop_step(__u64 i, void *data)
{
	struct sf_pop *c = data;
	struct sf_item *it;
	__u64 e;
	__u32 slot;

	if (!ring_pop(c->s, c->q, &e))
		return 1;
	it = entry_item(e, c->q, &slot);
	if (!it)
		return 0;
	if (c->s->live[c->q & 3])
		c->s->live[c->q & 3]--;
	c->slot = (int)slot;
	return 1;
}

static __always_inline int pop_live(struct sf_state *s, __u32 q)
{
	struct sf_pop c = { .s = s, .q = q, .slot = -1 };

	bpf_loop(R, pop_step, &c, 0);
	return c.slot;
}

/* Drop the oldest ghost, freeing its slot. */
static __always_inline void reclaim_ghost(struct sf_state *s)
{
	int slot = pop_live(s, Q_G);
	struct sf_item *it;

	if (slot < 0)
		return;
	it = get_item((__u32)slot);
	if (it)
		forget(s, (__u32)slot, it);
}

/* Item (already out of its queue) becomes a ghost; |G| is kept <= |M|. */
static __always_inline void to_ghost(struct sf_state *s, __u32 slot,
				     struct sf_item *it)
{
	__u32 cap = s->live[Q_M] > 16 ? s->live[Q_M] : 16;

	it->freq = 0;
	push(s, Q_G, slot, it);
	if (s->live[Q_G] > cap)
		reclaim_ghost(s);
}

SEC("struct_ops/on_admit")
int BPF_PROG(sf_admit, __u64 count, __u64 hint)
{
	__u32 k = PSTAT_INVOCATIONS;
	__u64 *sp = bpf_map_lookup_elem(&policy_stats, &k);

	if (sp)
		__sync_fetch_and_add(sp, 1);
	return 0;
}

SEC("struct_ops/on_evict")
int BPF_PROG(sf_evict, __u64 ino)
{
	return 0;
}

SEC("struct_ops/on_pressure")
int BPF_PROG(sf_pressure, __u64 count, __u64 threshold)
{
	return 0;
}

SEC("struct_ops/on_hit")
int BPF_PROG(sf_hit, __u64 ino)
{
	__u32 *slot = bpf_map_lookup_elem(&idx, &ino);
	struct sf_item *it;

	if (!slot)
		return 0;
	it = get_item(*slot);
	if (it && it->valid && it->ino == ino && it->tier != Q_G &&
	    it->freq < FREQ_MAX)
		it->freq++;
	return 0;
}

SEC("struct_ops/on_insert")
int BPF_PROG(sf_insert, __u64 ino)
{
	struct sf_state *s = get_state();
	struct sf_item *it;
	__u32 *sp, u;
	int slot;

	if (!s || !ino)
		return 0;
	sp = bpf_map_lookup_elem(&idx, &ino);
	if (sp) {
		it = get_item(*sp);
		if (!it || !it->valid || it->tier != Q_G)
			return 0; /* already a candidate */
		/* Ghost hit: straight to M.  Its G ring entry is now a tombstone
		 * (push bumps seq). */
		if (s->live[Q_G])
			s->live[Q_G]--;
		it->freq = 0;
		push(s, Q_M, *sp, it);
		return 0;
	}
	slot = alloc_slot(s);
	if (slot < 0) {
		reclaim_ghost(s);
		slot = alloc_slot(s);
		if (slot < 0)
			return 0;
	}
	u = (__u32)slot;
	it = get_item(u);
	if (!it)
		return 0;
	it->ino = ino;
	it->valid = 1;
	it->freq = 0;
	bpf_map_update_elem(&idx, &ino, &u, BPF_ANY);
	push(s, Q_S, u, it);
	return 0;
}

SEC("struct_ops/on_remove")
int BPF_PROG(sf_remove, __u64 ino)
{
	struct sf_state *s = get_state();
	__u32 *sp = bpf_map_lookup_elem(&idx, &ino);
	struct sf_item *it;
	__u32 slot;

	if (!s || !sp)
		return 0;
	slot = *sp;
	it = get_item(slot);
	if (!it || !it->valid || it->tier == Q_G)
		return 0;
	/* Leaves S/M by another route: its old ring entry becomes a tombstone. */
	if (s->live[it->tier & 3])
		s->live[it->tier & 3]--;
	to_ghost(s, slot, it);
	return 0;
}

struct sf_pick {
	struct sf_state *s;
	__u64 victim;
};

static long pick_step(__u64 i, void *data)
{
	struct sf_pick *c = data;
	struct sf_state *s = c->s;
	struct sf_item *it;
	__u32 ls = s->live[Q_S], lm = s->live[Q_M];
	int slot;
	bool from_s;

	if (!ls && !lm)
		return 1;
	from_s = lm == 0 || (ls && ls * 10 >= ls + lm);
	slot = pop_live(s, from_s ? Q_S : Q_M);
	if (slot < 0) {
		/* Counter out of step with the ring: resync and retry. */
		s->live[from_s ? Q_S : Q_M] = 0;
		return 0;
	}
	it = get_item((__u32)slot);
	if (!it)
		return 0;
	if (from_s) {
		if (it->freq > 1) {
			it->freq = 0;
			push(s, Q_M, (__u32)slot, it);
			return 0;
		}
	} else if (it->freq > 0) {
		it->freq--;
		push(s, Q_M, (__u32)slot, it);
		return 0;
	}
	c->victim = it->ino;
	to_ghost(s, (__u32)slot, it);
	return 1;
}

SEC("struct_ops/pick_victim")
__u64 BPF_PROG(sf_pick)
{
	struct sf_state *s = get_state();
	struct sf_pick c = { .s = s, .victim = 0 };

	if (!s)
		return 0;
	/* Each step either returns a victim or ages one item (freq <= 3). */
	bpf_loop(4 * N + 8, pick_step, &c, 0);
	return c.victim;
}

SEC(".struct_ops.link")
struct s3fs_policy_ops s3fifo_pick_ops = {
	.on_admit    = (void *)sf_admit,
	.on_evict    = (void *)sf_evict,
	.on_pressure = (void *)sf_pressure,
	.on_hit      = (void *)sf_hit,
	.on_insert   = (void *)sf_insert,
	.on_remove   = (void *)sf_remove,
	.pick_victim = (void *)sf_pick,
};

char _license[] SEC("license") = "GPL";
