// SPDX-License-Identifier: GPL-2.0
/*
 * KUnit suite for policy_struct_ops.c (#included under S3FS_KUNIT): the
 * vtable dispatch and the reg / unreg / update publication rules, driven
 * with a C vtable in place of a BPF struct_ops map.
 */

#include <kunit/test.h>

static u64 s3k_pol_victim;
static u64 s3k_pol_last_insert, s3k_pol_last_remove;

static int s3k_pol_admit(u64 c, u64 h)    { return (int)(c + h); }
static int s3k_pol_evict(u64 ino)         { return ino == 7; }
static int s3k_pol_pressure(u64 c, u64 t) { return c > t; }
static int s3k_pol_hit(u64 ino)           { return 1; }
static int s3k_pol_insert(u64 ino)        { s3k_pol_last_insert = ino; return 0; }
static int s3k_pol_remove(u64 ino)        { s3k_pol_last_remove = ino; return 0; }
static u64 s3k_pol_pick(void)             { return s3k_pol_victim; }

static struct s3fs_policy_ops s3k_pol_full = {
	.on_admit = s3k_pol_admit, .on_evict = s3k_pol_evict,
	.on_pressure = s3k_pol_pressure, .on_hit = s3k_pol_hit,
	.on_insert = s3k_pol_insert, .on_remove = s3k_pol_remove,
	.pick_victim = s3k_pol_pick,
};

/* A 4-member vtable from before AC-58. */
static struct s3fs_policy_ops s3k_pol_legacy = {
	.on_evict = s3k_pol_evict,
};

static struct s3fs_policy_ops *s3k_pol_active(void)
{
	return rcu_access_pointer(s3fs_policy_active);
}

static int s3k_pol_init(struct kunit *test)
{
	/* never run next to a loaded policy */
	return s3k_pol_active() ? -EBUSY : 0;
}

static void s3k_pol_exit(struct kunit *test)
{
	RCU_INIT_POINTER(s3fs_policy_active, NULL);
	synchronize_rcu();
}

static void s3k_pol_defaults(struct kunit *test)
{
	KUNIT_EXPECT_EQ(test, s3fs_policy_ops_eval_admit(3, 4), 0);
	KUNIT_EXPECT_EQ(test, s3fs_policy_ops_eval_evict(7), 0);
	KUNIT_EXPECT_EQ(test, s3fs_policy_ops_eval_pressure(9, 1), 0);
	KUNIT_EXPECT_EQ(test, s3fs_policy_ops_eval_hit(7), 0);
	KUNIT_EXPECT_FALSE(test, s3fs_policy_has_pick_victim());
	KUNIT_EXPECT_EQ(test, s3fs_policy_ops_pick_victim(), 0ull);
	s3fs_policy_ops_insert(1);	/* no-ops without a policy */
	s3fs_policy_ops_remove(1);
}

static void s3k_pol_dispatch(struct kunit *test)
{
	KUNIT_ASSERT_EQ(test, s3fs_policy_reg(&s3k_pol_full, NULL), 0);
	KUNIT_EXPECT_EQ(test, s3fs_policy_ops_eval_admit(3, 4), 7);
	KUNIT_EXPECT_EQ(test, s3fs_policy_ops_eval_evict(7), 1);
	KUNIT_EXPECT_EQ(test, s3fs_policy_ops_eval_evict(8), 0);
	KUNIT_EXPECT_EQ(test, s3fs_policy_ops_eval_pressure(9, 1), 1);
	KUNIT_EXPECT_EQ(test, s3fs_policy_ops_eval_hit(7), 1);
	KUNIT_EXPECT_TRUE(test, s3fs_policy_has_pick_victim());
	s3k_pol_victim = 42;
	s3fs_policy_evict_lock();
	s3fs_policy_ops_insert(11);
	KUNIT_EXPECT_EQ(test, s3fs_policy_ops_pick_victim(), 42ull);
	s3fs_policy_ops_remove(42);
	s3fs_policy_evict_unlock();
	KUNIT_EXPECT_EQ(test, s3k_pol_last_insert, 11ull);
	KUNIT_EXPECT_EQ(test, s3k_pol_last_remove, 42ull);
	s3fs_policy_unreg(&s3k_pol_full, NULL);
	KUNIT_EXPECT_NULL(test, s3k_pol_active());
}

static void s3k_pol_legacy_vtable(struct kunit *test)
{
	/* missing members fall back to the defaults instead of a NULL call */
	KUNIT_ASSERT_EQ(test, s3fs_policy_reg(&s3k_pol_legacy, NULL), 0);
	KUNIT_EXPECT_EQ(test, s3fs_policy_ops_eval_evict(7), 1);
	KUNIT_EXPECT_EQ(test, s3fs_policy_ops_eval_admit(3, 4), 0);
	KUNIT_EXPECT_EQ(test, s3fs_policy_ops_eval_hit(7), 0);
	KUNIT_EXPECT_FALSE(test, s3fs_policy_has_pick_victim());
	KUNIT_EXPECT_EQ(test, s3fs_policy_ops_pick_victim(), 0ull);
	s3fs_policy_ops_insert(5);
	s3fs_policy_unreg(&s3k_pol_legacy, NULL);
}

static void s3k_pol_hot_reload(struct kunit *test)
{
	/* attach-new-then-close-old: the old unreg keeps the new vtable */
	KUNIT_ASSERT_EQ(test, s3fs_policy_reg(&s3k_pol_legacy, NULL), 0);
	KUNIT_ASSERT_EQ(test, s3fs_policy_reg(&s3k_pol_full, NULL), 0);
	s3fs_policy_unreg(&s3k_pol_legacy, NULL);
	KUNIT_EXPECT_PTR_EQ(test, s3k_pol_active(), &s3k_pol_full);
	/* BPF_LINK_UPDATE swaps in place */
	KUNIT_ASSERT_EQ(test, s3fs_policy_update(&s3k_pol_legacy, &s3k_pol_full,
						 NULL), 0);
	KUNIT_EXPECT_PTR_EQ(test, s3k_pol_active(), &s3k_pol_legacy);
	s3fs_policy_unreg(&s3k_pol_legacy, NULL);
	KUNIT_EXPECT_NULL(test, s3k_pol_active());
}


/* -- Data members (flags / name): init_member and validate ------------------
 * s3fs_policy_init_member reads one thing from the BTF the kernel hands it: the
 * member's bit offset.  A struct btf_type with kind_flag clear and one btf_member
 * stand in for it, so the rules run without a BPF struct_ops map. */

struct s3k_fake_btf {
	struct btf_type   t;
	struct btf_member m;
};

static int s3k_init_member(size_t byte_off, const struct s3fs_policy_ops *u,
			   struct s3fs_policy_ops *k)
{
	struct s3k_fake_btf f = {};

	f.t.info = (u32)BTF_KIND_STRUCT << 24 | 1;	/* kind_flag clear, vlen 1 */
	f.m.offset = byte_off * 8;
	return s3fs_policy_init_member(&f.t, &f.m, k, u);
}

static void s3k_pol_data_members_taken(struct kunit *test)
{
	struct s3fs_policy_ops u = {}, k = {};

	u.flags = S3FS_POLICY_F_NO_ON_HIT;
	KUNIT_EXPECT_EQ(test, s3k_init_member(offsetof(struct s3fs_policy_ops, flags), &u, &k), 1);
	KUNIT_EXPECT_EQ(test, k.flags, (u32)S3FS_POLICY_F_NO_ON_HIT);

	strscpy(u.name, "unit.test_1", sizeof(u.name));
	KUNIT_EXPECT_EQ(test, s3k_init_member(offsetof(struct s3fs_policy_ops, name), &u, &k), 1);
	KUNIT_EXPECT_STREQ(test, k.name, "unit.test_1");
	/* the rest of the array is zero, not stale bytes */
	KUNIT_EXPECT_EQ(test, k.name[sizeof(k.name) - 1], 0);

	/* a policy built before the name member existed leaves it empty: allowed */
	memset(&u, 0, sizeof(u));
	memset(k.name, 'x', sizeof(k.name) - 1);
	KUNIT_EXPECT_EQ(test, s3k_init_member(offsetof(struct s3fs_policy_ops, name), &u, &k), 1);
	KUNIT_EXPECT_STREQ(test, k.name, "");
}

static void s3k_pol_data_members_refused(struct kunit *test)
{
	struct s3fs_policy_ops u = {}, k = {};

	/* a flag bit the kmod does not know; kdata is left alone */
	u.flags = S3FS_POLICY_F_ALL << 1;
	KUNIT_EXPECT_EQ(test, s3k_init_member(offsetof(struct s3fs_policy_ops, flags), &u, &k), -EINVAL);
	KUNIT_EXPECT_EQ(test, k.flags, 0u);

	/* a character outside [A-Za-z0-9_.] */
	memset(&u, 0, sizeof(u));
	strscpy(u.name, "bad name", sizeof(u.name));
	KUNIT_EXPECT_EQ(test, s3k_init_member(offsetof(struct s3fs_policy_ops, name), &u, &k), -EINVAL);
	strscpy(u.name, "dash-ed", sizeof(u.name));
	KUNIT_EXPECT_EQ(test, s3k_init_member(offsetof(struct s3fs_policy_ops, name), &u, &k), -EINVAL);

	/* no NUL inside the array */
	memset(u.name, 'a', sizeof(u.name));
	KUNIT_EXPECT_EQ(test, s3k_init_member(offsetof(struct s3fs_policy_ops, name), &u, &k), -EINVAL);

	/* a function pointer is not this callback's: 0 hands it to the framework */
	KUNIT_EXPECT_EQ(test, s3k_init_member(offsetof(struct s3fs_policy_ops, on_hit), &u, &k), 0);
	KUNIT_EXPECT_EQ(test, s3k_init_member(offsetof(struct s3fs_policy_ops, pick_victim), &u, &k), 0);
}

static void s3k_pol_validate(struct kunit *test)
{
	struct s3fs_policy_ops o = {};

	KUNIT_EXPECT_EQ(test, s3fs_policy_validate(&o), 0);		/* empty */
	KUNIT_EXPECT_EQ(test, s3fs_policy_validate(&s3k_pol_legacy), 0);
	KUNIT_EXPECT_EQ(test, s3fs_policy_validate(&s3k_pol_full), 0);

	/* pick_victim needs on_insert and on_remove (AC-58, AC-64) */
	o.pick_victim = s3k_pol_pick;
	KUNIT_EXPECT_EQ(test, s3fs_policy_validate(&o), -EINVAL);
	o.on_insert = s3k_pol_insert;
	KUNIT_EXPECT_EQ(test, s3fs_policy_validate(&o), -EINVAL);
	o.on_remove = s3k_pol_remove;
	KUNIT_EXPECT_EQ(test, s3fs_policy_validate(&o), 0);

	/* NO_ON_HIT must agree with the vtable */
	memset(&o, 0, sizeof(o));
	o.flags = S3FS_POLICY_F_NO_ON_HIT;
	KUNIT_EXPECT_EQ(test, s3fs_policy_validate(&o), 0);
	o.on_hit = s3k_pol_hit;
	KUNIT_EXPECT_EQ(test, s3fs_policy_validate(&o), -EINVAL);
}

/* The name and flags are in the vtable that reg publishes: what a BPF_LINK_UPDATE
 * swaps in comes with its own data. */
static void s3k_pol_data_swap_with_vtable(struct kunit *test)
{
	struct s3fs_policy_ops a = s3k_pol_legacy, b = s3k_pol_legacy;

	strscpy(a.name, "first", sizeof(a.name));
	strscpy(b.name, "second", sizeof(b.name));
	b.flags = S3FS_POLICY_F_NO_ON_HIT;
	KUNIT_ASSERT_EQ(test, s3fs_policy_reg(&a, NULL), 0);
	KUNIT_EXPECT_STREQ(test, s3k_pol_active()->name, "first");
	KUNIT_ASSERT_EQ(test, s3fs_policy_update(&b, &a, NULL), 0);
	KUNIT_EXPECT_STREQ(test, s3k_pol_active()->name, "second");
	KUNIT_EXPECT_EQ(test, s3k_pol_active()->flags, (u32)S3FS_POLICY_F_NO_ON_HIT);
	s3fs_policy_unreg(&b, NULL);
	KUNIT_EXPECT_NULL(test, s3k_pol_active());
}

static struct kunit_case s3k_pol_cases[] = {
	KUNIT_CASE(s3k_pol_defaults),
	KUNIT_CASE(s3k_pol_dispatch),
	KUNIT_CASE(s3k_pol_legacy_vtable),
	KUNIT_CASE(s3k_pol_hot_reload),
	KUNIT_CASE(s3k_pol_data_members_taken),
	KUNIT_CASE(s3k_pol_data_members_refused),
	KUNIT_CASE(s3k_pol_validate),
	KUNIT_CASE(s3k_pol_data_swap_with_vtable),
	{}
};

static struct kunit_suite s3k_pol_suite = {
	.name = "s3fs_ebpf_policy_ops",
	.init = s3k_pol_init,
	.exit = s3k_pol_exit,
	.test_cases = s3k_pol_cases,
};

kunit_test_suites(&s3k_pol_suite);
