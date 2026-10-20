// SPDX-License-Identifier: GPL-2.0
//
// policy_wtinylfu_pick_ops — W-TinyLFU (Einziger et al., TOS 2017; Caffeine's
// shape) on the AC-58 vtable.  Design: docs/design/wtinylfu-pick-plan.md
// (AC-66).  Reference simulator: daemon/internal/trace/sim_wtinylfu.go
// (`wtinylfu-clock` follows this file step for step).
//
//   W (window, 1 %)        new inos enter here
//   P (probation, main)    the window's overflow: candidates for main
//   T (protected, 80 % of main)  probation inos that were hit again
//   X (rejected)           lost the admission duel, came back (pending shed)
//   L (limbo)              picked, but the kmod has not removed it yet
//
//   on_hit      sketch++ (any read of a known ino, hit or miss); hit = 1
//   on_insert   L -> back where it was (X if it lost the duel); new -> W.
//               W over budget: W's oldest unhit ino moves to P and becomes
//               the candidate
//   pick_victim X's oldest first.  T over budget: demote T's oldest unhit
//               ino to P.  P's oldest hit ino -> T.  Then the duel: the
//               candidate against P's oldest unhit ino, the lower sketch
//               estimate is the victim (a tie rejects the candidate).
//               P empty: T's, then W's oldest unhit ino
//   on_remove   forget the ino (the sketch keeps its frequency)
//   on_size     the bytes the kmod charges this ino; budgets are in bytes
//
// Segments are FIFOs with a hit bit (CLOCK), not exact LRUs: on_hit runs
// concurrently and lockless, so it can only set a bit; all movement happens
// in on_insert / pick_victim, which the kmod serializes
// (s3fs_policy_evict_lock).  Each queue is a ring of (seq << 32 | slot); an
// entry whose seq no longer matches its item is a tombstone, as in
// policy_s3fifo_pick_ops.bpf.c.

#include "vmlinux.h"
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_tracing.h>

#define N 4096 /* slots (resident + limbo) */
#define R N    /* ring capacity per queue */

enum { Q_W = 0, Q_P = 1, Q_T = 2, Q_X = 3, Q_L = 4, NQ = 5 };
#define QM 7 /* index mask for the per-queue arrays (8 > NQ) */

#define WINDOW_PCT    1
#define PROTECTED_PCT 80
#define SAMPLE_FACTOR 10 /* sketch reset after 10 × resident increments */
#define SAMPLE_MIN    16
#define LIMBO_MAX     1024

#define SK_W      1024U
#define SK_ROWS   4U
#define SK_MAX    15U

struct wt_item {
	__u64 ino;
	__u64 weight; /* bytes from on_size, 1 until then */
	__u32 seq;    /* seq of the latest push; a ring entry must match */
	__u8  valid;
	__u8  tier;
	__u8  hit;
	__u8  rej;    /* lost the duel: comes back from L into X */
	__u8  prev;   /* tier before L */
	__u8  pad[7];
};

struct {
	__uint(type, BPF_MAP_TYPE_ARRAY);
	__type(key, __u32);
	__type(value, struct wt_item);
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

/* Count-Min sketch, SK_ROWS rows of SK_W saturating 4-bit counters (u8). */
struct {
	__uint(type, BPF_MAP_TYPE_ARRAY);
	__type(key, __u32);
	__type(value, __u8);
	__uint(max_entries, SK_ROWS * SK_W);
} sketch SEC(".maps");

struct wt_state {
	__u32 clock;
	__u32 hi;
	__u32 nfree;
	__u32 cand;      /* slot of the latest candidate (valid if cand_seq) */
	__u32 cand_seq;  /* 0 = none */
	__u32 pad;
	__u32 head[8];
	__u32 tail[8];
	__u32 live[8];
	__u64 wsum[8];   /* weight per queue */
};

struct {
	__uint(type, BPF_MAP_TYPE_ARRAY);
	__type(key, __u32);
	__type(value, struct wt_state);
	__uint(max_entries, 1);
} st SEC(".maps");

/* Sketch increments since the last reset, and the reset threshold.  Apart
 * from wt_state because on_hit touches them without the kmod's lock. */
enum { SK_ADDS = 0, SK_SAMPLE = 1, SK_MAXSTAT };

struct {
	__uint(type, BPF_MAP_TYPE_ARRAY);
	__type(key, __u32);
	__type(value, __u64);
	__uint(max_entries, SK_MAXSTAT);
} sk_ctl SEC(".maps");

enum {
	PSTAT_INVOCATIONS = 0,
	PSTAT_ADMITTED    = 1, /* duel won by the candidate */
	PSTAT_REJECTED    = 2, /* duel lost by the candidate */
	PSTAT_RESTORED    = 3, /* came back from limbo */
	PSTAT_RESETS      = 4, /* sketch halvings */
	PSTAT_MAX,
};

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
	int (*on_size)(__u64 ino, __u64 bytes);
};

static __always_inline void stat_inc(__u32 k)
{
	__u64 *v = bpf_map_lookup_elem(&policy_stats, &k);

	if (v)
		__sync_fetch_and_add(v, 1);
}

static __always_inline struct wt_state *get_state(void)
{
	__u32 z = 0;

	return bpf_map_lookup_elem(&st, &z);
}

static __always_inline struct wt_item *get_item(__u32 slot)
{
	return bpf_map_lookup_elem(&items, &slot);
}

/* --- sketch (same hash as sim_wtinylfu.go) ---------------------------- */

static const __u32 seeds[SK_ROWS] = {
	0x9e3779b1U, 0x85ebca77U, 0xc2b2ae3dU, 0x27d4eb2fU,
};

static __always_inline __u32 sk_key(__u64 ino, __u32 row)
{
	__u64 h = ino * 0x9e3779b97f4a7c15ULL;

	h ^= (__u64)seeds[row & 3];
	h ^= h >> 33;
	return (row & 3) * SK_W + (__u32)(h & (SK_W - 1));
}

static __always_inline void sk_increment(__u64 ino)
{
	__u32 r, k;
	__u8 *c;

	#pragma unroll
	for (r = 0; r < SK_ROWS; r++) {
		k = sk_key(ino, r);
		c = bpf_map_lookup_elem(&sketch, &k);
		if (c && *c < SK_MAX)
			*c += 1; /* racy; a lost increment is harmless */
	}
}

static __always_inline __u32 sk_estimate(__u64 ino)
{
	__u32 r, k, est = SK_MAX;
	__u8 *c;

	#pragma unroll
	for (r = 0; r < SK_ROWS; r++) {
		k = sk_key(ino, r);
		c = bpf_map_lookup_elem(&sketch, &k);
		if (c && *c < est)
			est = *c;
	}
	return est;
}

static long sk_halve_cb(__u32 i, void *ctx)
{
	__u8 *c = bpf_map_lookup_elem(&sketch, &i);

	if (c)
		*c >>= 1;
	return 0;
}

/* Called under the kmod's lock: the reset itself does not run on the read
 * path, on_hit only counts. */
static __always_inline void sk_maybe_reset(void)
{
	__u32 a = SK_ADDS, b = SK_SAMPLE;
	__u64 *adds = bpf_map_lookup_elem(&sk_ctl, &a);
	__u64 *sample = bpf_map_lookup_elem(&sk_ctl, &b);

	if (!adds || !sample || !*sample || *adds < *sample)
		return;
	bpf_loop(SK_ROWS * SK_W, sk_halve_cb, NULL, 0);
	*adds = 0;
	stat_inc(PSTAT_RESETS);
}

/* --- slots and rings ---------------------------------------------------- */

static __always_inline int alloc_slot(struct wt_state *s)
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

static __always_inline void free_slot(struct wt_state *s, __u32 slot)
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

/* The item leaves its queue's accounting (its ring entry becomes a
 * tombstone once it is pushed elsewhere or freed). */
static __always_inline void leave(struct wt_state *s, struct wt_item *it)
{
	__u32 q = it->tier & QM;

	if (s->live[q])
		s->live[q]--;
	s->wsum[q] -= it->weight < s->wsum[q] ? it->weight : s->wsum[q];
}

static __always_inline void forget(struct wt_state *s, __u32 slot,
				   struct wt_item *it)
{
	bpf_map_delete_elem(&idx, &it->ino);
	it->valid = 0;
	free_slot(s, slot);
}

static __always_inline __u64 *ring_at(__u32 q, __u32 pos)
{
	__u32 key = q * R + (pos % R);

	return bpf_map_lookup_elem(&rings, &key);
}

/* Is ring entry e of queue q the item's current position? */
static __always_inline struct wt_item *entry_item(__u64 e, __u32 q, __u32 *slot)
{
	struct wt_item *it;

	*slot = (__u32)e;
	it = get_item(*slot);
	if (!it || !it->valid || it->tier != q || it->seq != (__u32)(e >> 32))
		return NULL;
	return it;
}

/* Append slot at the tail of queue q (the item has already left its old
 * queue).  A full ring drops its oldest entry; a live item there is
 * forgotten. */
static __always_inline void push(struct wt_state *s, __u32 q, __u32 slot,
				 struct wt_item *it)
{
	__u64 *v;

	if (q >= NQ)
		return;
	if (s->head[q & QM] - s->tail[q & QM] >= R) {
		__u32 os;
		struct wt_item *oi;

		v = ring_at(q, s->tail[q & QM]);
		s->tail[q & QM]++;
		if (v) {
			oi = entry_item(*v, q, &os);
			if (oi) {
				leave(s, oi);
				forget(s, os, oi);
			}
		}
	}
	it->seq = ++s->clock;
	if (!it->seq)
		it->seq = ++s->clock; /* 0 means "no candidate" */
	it->tier = q;
	v = ring_at(q, s->head[q & QM]);
	if (!v)
		return;
	*v = ((__u64)it->seq << 32) | slot;
	s->head[q & QM]++;
	s->live[q & QM]++;
	s->wsum[q & QM] += it->weight;
}

/* Oldest live entry of queue q, left in place (tombstones in front of it
 * are dropped).  -1 if the queue is empty. */
struct wt_peek {
	struct wt_state *s;
	__u32 q;
	int slot;
};

static long peek_step(__u64 i, void *data)
{
	struct wt_peek *c = data;
	struct wt_state *s = c->s;
	__u32 q = c->q & QM;
	__u32 slot;
	__u64 *v;

	if (s->tail[q] == s->head[q])
		return 1;
	v = ring_at(c->q, s->tail[q]);
	if (!v)
		return 1;
	if (entry_item(*v, c->q, &slot)) {
		c->slot = (int)slot;
		return 1;
	}
	s->tail[q]++;
	return 0;
}

static __always_inline int peek_oldest(struct wt_state *s, __u32 q)
{
	struct wt_peek c = { .s = s, .q = q, .slot = -1 };

	if (q >= NQ)
		return -1;
	bpf_loop(R, peek_step, &c, 0);
	if (c.slot < 0)
		s->live[q & QM] = 0, s->wsum[q & QM] = 0; /* resync */
	return c.slot;
}

static __always_inline void to_limbo(struct wt_state *s, __u32 slot,
				     struct wt_item *it, __u8 rej)
{
	leave(s, it);
	it->prev = it->tier;
	it->rej = rej;
	it->hit = 0;
	push(s, Q_L, slot, it);
	if (s->live[Q_L] > LIMBO_MAX) {
		int os = peek_oldest(s, Q_L);
		struct wt_item *oi = os >= 0 ? get_item((__u32)os) : NULL;

		if (oi) {
			leave(s, oi);
			forget(s, (__u32)os, oi);
		}
	}
}

static __always_inline __u64 resident(struct wt_state *s)
{
	return s->wsum[Q_W] + s->wsum[Q_P] + s->wsum[Q_T];
}

static __always_inline __u64 window_budget(struct wt_state *s)
{
	__u64 b = resident(s) * WINDOW_PCT / 100;

	return b ? b : 1;
}

static __always_inline __u64 protected_budget(struct wt_state *s)
{
	__u64 r = resident(s), w = window_budget(s);

	return r > w ? (r - w) * PROTECTED_PCT / 100 : 0;
}

/* W over budget: its oldest unhit ino moves to P as the candidate. */
struct wt_ctx {
	struct wt_state *s;
	__u64 victim;
};

static long balance_window_step(__u64 i, void *data)
{
	struct wt_ctx *c = data;
	struct wt_state *s = c->s;
	struct wt_item *it;
	int slot;

	if (s->wsum[Q_W] <= window_budget(s) || !s->live[Q_W])
		return 1;
	slot = peek_oldest(s, Q_W);
	if (slot < 0)
		return 1;
	it = get_item((__u32)slot);
	if (!it)
		return 1;
	leave(s, it);
	if (it->hit) {
		it->hit = 0;
		push(s, Q_W, (__u32)slot, it);
		return 0;
	}
	push(s, Q_P, (__u32)slot, it);
	s->cand = (__u32)slot;
	s->cand_seq = it->seq;
	return 1;
}

static __always_inline struct wt_item *candidate(struct wt_state *s)
{
	struct wt_item *it;

	if (!s->cand_seq)
		return NULL;
	it = get_item(s->cand);
	if (!it || !it->valid || it->tier != Q_P || it->seq != s->cand_seq) {
		s->cand_seq = 0;
		return NULL;
	}
	return it;
}

SEC("struct_ops/on_admit")
int BPF_PROG(wt_admit, __u64 count, __u64 hint)
{
	stat_inc(PSTAT_INVOCATIONS);
	return 0;
}

/* Score order fallback (kmod sorts high first): rarest first. */
SEC("struct_ops/on_evict")
int BPF_PROG(wt_evict, __u64 ino)
{
	return (int)(SK_MAX - sk_estimate(ino));
}

SEC("struct_ops/on_pressure")
int BPF_PROG(wt_pressure, __u64 count, __u64 threshold)
{
	return 0;
}

static __always_inline int do_hit(__u64 ino)
{
	__u32 k = SK_ADDS, *slot;
	__u64 *adds = bpf_map_lookup_elem(&sk_ctl, &k);
	struct wt_item *it;

	sk_increment(ino);
	if (adds)
		__sync_fetch_and_add(adds, 1);
	slot = bpf_map_lookup_elem(&idx, &ino);
	if (!slot)
		return 0;
	it = get_item(*slot);
	if (it && it->valid && it->ino == ino)
		it->hit = 1;
	return 0;
}

SEC("struct_ops/on_hit")
int BPF_PROG(wt_hit, __u64 ino)
{
	return do_hit(ino);
}

static __always_inline int do_insert(__u64 ino)
{
	struct wt_state *s = get_state();
	struct wt_ctx c = { .s = s };
	struct wt_item *it;
	__u32 *sp, u, k = SK_SAMPLE;
	__u64 *sample, n;
	int slot;

	if (!s || !ino)
		return 0;
	sk_maybe_reset();
	sp = bpf_map_lookup_elem(&idx, &ino);
	if (sp) {
		u = *sp;
		it = get_item(u);
		if (!it || !it->valid || it->tier != Q_L)
			return 0; /* already resident */
		/* Shed rather than evicted (a miss was in flight): back where
		 * it was, or first in line if it lost the duel. */
		leave(s, it);
		push(s, it->rej ? Q_X : (it->prev & QM) < Q_X ? it->prev : Q_P,
		     u, it);
		stat_inc(PSTAT_RESTORED);
		return 0;
	}
	slot = alloc_slot(s);
	if (slot < 0) {
		int os = peek_oldest(s, Q_L);
		struct wt_item *oi = os >= 0 ? get_item((__u32)os) : NULL;

		if (!oi)
			return 0;
		leave(s, oi);
		forget(s, (__u32)os, oi);
		slot = alloc_slot(s);
		if (slot < 0)
			return 0;
	}
	u = (__u32)slot;
	it = get_item(u);
	if (!it)
		return 0;
	it->ino = ino;
	it->weight = 1;
	it->valid = 1;
	it->hit = 0;
	it->rej = 0;
	bpf_map_update_elem(&idx, &ino, &u, BPF_ANY);
	push(s, Q_W, u, it);
	bpf_loop(R, balance_window_step, &c, 0);

	n = (__u64)(s->live[Q_W] + s->live[Q_P] + s->live[Q_T] + s->live[Q_X]) *
	    SAMPLE_FACTOR;
	sample = bpf_map_lookup_elem(&sk_ctl, &k);
	if (sample)
		*sample = n > SAMPLE_MIN ? n : SAMPLE_MIN;
	return 0;
}

SEC("struct_ops/on_insert")
int BPF_PROG(wt_insert, __u64 ino)
{
	return do_insert(ino);
}

static __always_inline int do_size(__u64 ino, __u64 bytes)
{
	struct wt_state *s = get_state();
	__u32 *sp = bpf_map_lookup_elem(&idx, &ino);
	struct wt_item *it;
	__u32 q;

	if (!s || !sp)
		return 0;
	it = get_item(*sp);
	if (!it || !it->valid)
		return 0;
	if (!bytes)
		bytes = 1;
	q = it->tier & QM;
	s->wsum[q] -= it->weight < s->wsum[q] ? it->weight : s->wsum[q];
	s->wsum[q] += bytes;
	it->weight = bytes;
	/* A window ino grown past the budget is the candidate now. */
	if (q == Q_W) {
		struct wt_ctx c = { .s = s };

		bpf_loop(R, balance_window_step, &c, 0);
	}
	return 0;
}

SEC("struct_ops/on_size")
int BPF_PROG(wt_size, __u64 ino, __u64 bytes)
{
	return do_size(ino, bytes);
}

static __always_inline int do_remove(__u64 ino)
{
	struct wt_state *s = get_state();
	__u32 *sp = bpf_map_lookup_elem(&idx, &ino);
	struct wt_item *it;
	__u32 slot;

	if (!s || !sp)
		return 0;
	slot = *sp;
	it = get_item(slot);
	if (!it || !it->valid)
		return 0;
	leave(s, it);
	forget(s, slot, it);
	return 0;
}

SEC("struct_ops/on_remove")
int BPF_PROG(wt_remove, __u64 ino)
{
	return do_remove(ino);
}

/* One step of pick_victim: either names the victim (returns 1) or moves one
 * ino (a CLOCK second chance, a promotion or a demotion) and goes again. */
static long pick_step(__u64 i, void *data)
{
	struct wt_ctx *c = data;
	struct wt_state *s = c->s;
	struct wt_item *it, *ci;
	int slot;
	__u32 q;

	if (s->live[Q_X]) {
		slot = peek_oldest(s, Q_X);
		if (slot >= 0 && (it = get_item((__u32)slot))) {
			c->victim = it->ino;
			to_limbo(s, (__u32)slot, it, 1);
			return 1;
		}
		return 0;
	}
	if (s->live[Q_T] && s->wsum[Q_T] > protected_budget(s)) {
		slot = peek_oldest(s, Q_T);
		if (slot >= 0 && (it = get_item((__u32)slot))) {
			leave(s, it);
			if (it->hit) {
				it->hit = 0;
				push(s, Q_T, (__u32)slot, it);
				return 0;
			}
			push(s, Q_P, (__u32)slot, it);
		}
		return 0;
	}
	if (s->live[Q_P]) {
		slot = peek_oldest(s, Q_P);
		if (slot < 0)
			return 0;
		it = get_item((__u32)slot);
		if (!it)
			return 1;
		if (it->hit) {
			leave(s, it);
			it->hit = 0;
			push(s, Q_T, (__u32)slot, it);
			return 0;
		}
		ci = candidate(s);
		if (ci && s->cand != (__u32)slot) {
			s->cand_seq = 0;
			if (sk_estimate(ci->ino) > sk_estimate(it->ino)) {
				stat_inc(PSTAT_ADMITTED);
				c->victim = it->ino;
				to_limbo(s, (__u32)slot, it, 0);
			} else {
				stat_inc(PSTAT_REJECTED);
				c->victim = ci->ino;
				to_limbo(s, s->cand, ci, 1);
			}
			return 1;
		}
		c->victim = it->ino;
		to_limbo(s, (__u32)slot, it, 0);
		return 1;
	}
	/* P is empty: protected, then the window. */
	q = s->live[Q_T] ? Q_T : Q_W;
	if (!s->live[q])
		return 1; /* nothing resident */
	slot = peek_oldest(s, q);
	if (slot < 0)
		return 0;
	it = get_item((__u32)slot);
	if (!it)
		return 1;
	if (it->hit) {
		leave(s, it);
		it->hit = 0;
		push(s, q, (__u32)slot, it);
		return 0;
	}
	c->victim = it->ino;
	to_limbo(s, (__u32)slot, it, 0);
	return 1;
}

static __always_inline __u64 do_pick(void)
{
	struct wt_state *s = get_state();
	struct wt_ctx c = { .s = s, .victim = 0 };

	if (!s)
		return 0;
	sk_maybe_reset();
	/* Each step names the victim or clears one hit bit / moves one ino;
	 * hit bits are cleared at most once per ino per call. */
	bpf_loop(4 * N + 8, pick_step, &c, 0);
	return c.victim;
}

SEC("struct_ops/pick_victim")
__u64 BPF_PROG(wt_pick)
{
	return do_pick();
}

SEC(".struct_ops.link")
struct s3fs_policy_ops wtinylfu_pick_ops = {
	.on_admit    = (void *)wt_admit,
	.on_evict    = (void *)wt_evict,
	.on_pressure = (void *)wt_pressure,
	.on_hit      = (void *)wt_hit,
	.on_insert   = (void *)wt_insert,
	.on_remove   = (void *)wt_remove,
	.pick_victim = (void *)wt_pick,
	.on_size     = (void *)wt_size,
};

char _license[] SEC("license") = "GPL";
