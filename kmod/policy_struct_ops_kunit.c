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

static struct kunit_case s3k_pol_cases[] = {
	KUNIT_CASE(s3k_pol_defaults),
	KUNIT_CASE(s3k_pol_dispatch),
	KUNIT_CASE(s3k_pol_legacy_vtable),
	KUNIT_CASE(s3k_pol_hot_reload),
	{}
};

static struct kunit_suite s3k_pol_suite = {
	.name = "s3fs_ebpf_policy_ops",
	.init = s3k_pol_init,
	.exit = s3k_pol_exit,
	.test_cases = s3k_pol_cases,
};

kunit_test_suites(&s3k_pol_suite);
