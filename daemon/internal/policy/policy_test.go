package policy

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoadRepoPolicies loads every policies/*.cel in the repo so a
// sample that no longer parses or compiles fails here, not at mount.
func TestLoadRepoPolicies(t *testing.T) {
	paths, err := filepath.Glob("../../../policies/*.cel")
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) == 0 {
		t.Fatal("no policies/*.cel found")
	}
	for _, p := range paths {
		if _, err := Load(p); err != nil {
			t.Errorf("%s: %v", filepath.Base(p), err)
		}
	}
}

func TestContinuationLines(t *testing.T) {
	p := filepath.Join(t.TempDir(), "multi.cel")
	src := "# comment\n" +
		"prewarm: entry.size <= 10u\n" +
		"    && entry.name.endsWith(\".bin\")\n" +
		"\n" +
		"  # indented comment inside the expression is skipped\n" +
		"\t|| entry.name == \"keep\"\n" +
		"policy: \"builtin:policy_lru_ops\"\n"
	if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	eng, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		in   EntryInput
		want bool
	}{
		{EntryInput{Name: "a.bin", Size: 5}, true},
		{EntryInput{Name: "a.bin", Size: 50}, false},
		{EntryInput{Name: "a.txt", Size: 5}, false},
		{EntryInput{Name: "keep", Size: 50}, true},
	}
	for _, c := range cases {
		if got := eng.EvalPrewarm(c.in); got != c.want {
			t.Errorf("%+v: got %v, want %v", c.in, got, c.want)
		}
	}
	if got := eng.EvalPolicyName(); got != "builtin:policy_lru_ops" {
		t.Errorf("policy name: got %q", got)
	}
}

func TestContinuationBeforeHook(t *testing.T) {
	p := filepath.Join(t.TempDir(), "bad.cel")
	if err := os.WriteFile(p, []byte("  && true\nprewarm: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err == nil {
		t.Fatal("want error for a continuation line before any hook")
	}
}

func TestPrewarmDatasetPolicy(t *testing.T) {
	eng, err := Load("../../../policies/prewarm-dataset.cel")
	if err != nil {
		t.Fatal(err)
	}
	const mib = 1 << 20
	cases := []struct {
		key  string
		size uint64
		want bool
	}{
		{"datasets/imagenet/train/shard-0001.parquet", 6 * mib, true},
		{"datasets/imagenet/val/shard-0001.tfrecord", 1 * mib, true},
		{"datasets/imagenet/train/shard-0002.parquet", 64 * mib, false}, // too large
		{"datasets/imagenet/test/shard-0001.parquet", 1 * mib, false},   // not train/val
		{"datasets/imagenet/train/raw.csv", 1 * mib, false},             // not a shard format
		{"datasets/imagenet/_SUCCESS", 0, true},
		{"datasets/imagenet/manifest.json", 200 * 1024, true},
		{"models/resnet/latest/weights.pt", 1500 * mib, true},
		{"models/resnet/v1/weights.pt", 1500 * mib, false},
		{"models/resnet/latest/weights.pt", 3 * 1024 * mib, false},
		{"configs/train.yaml", 900, true},
		{"logs/run1/stdout.log", 100, false},
		{"datasets/imagenet/train/.shard.parquet", 1 * mib, false},
		{"datasets/imagenet/train/shard-0003.parquet.part", 1 * mib, false},
		{"datasets/_staging/train/shard-0001.parquet", 1 * mib, false},
		{"archive/models/resnet/latest/weights.pt", 100, false},
	}
	for _, c := range cases {
		in := EntryInput{Name: filepath.Base(c.key), S3Key: c.key, Size: c.size}
		if got := eng.EvalPrewarm(in); got != c.want {
			t.Errorf("%s size=%d: got %v, want %v", c.key, c.size, got, c.want)
		}
	}
	if got := eng.EvalPolicyName(); got != "builtin:policy_s3fifo_pick_ops" {
		t.Errorf("policy name: got %q", got)
	}
	for key, want := range map[string]int{
		"datasets/imagenet/train/shard-0001.parquet": 2,
		"datasets/imagenet/test/shard-0001.parquet":  0,
		"models/resnet/latest/weights.pt":            0,
	} {
		if got := eng.EvalPrefetchNext(EntryInput{S3Key: key}); got != want {
			t.Errorf("prefetch_next %s: got %d, want %d", key, got, want)
		}
	}
}

func TestPrefetchNext(t *testing.T) {
	p := filepath.Join(t.TempDir(), "pf.cel")
	src := "prefetch_next: entry.s3_key.startsWith(\"datasets/\") ? 3 : 0\n"
	if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	eng, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if !eng.HasPrefetchNext() {
		t.Fatal("HasPrefetchNext = false")
	}
	if got := eng.EvalPrefetchNext(EntryInput{S3Key: "datasets/a/x"}); got != 3 {
		t.Errorf("datasets: got %d, want 3", got)
	}
	if got := eng.EvalPrefetchNext(EntryInput{S3Key: "logs/x"}); got != 0 {
		t.Errorf("logs: got %d, want 0", got)
	}

	for expr, want := range map[string]int{
		"-5":        0,
		"1000":      MaxPrefetchNext,
		"7u":        7,
		"100000u":   MaxPrefetchNext,
		"\"three\"": 0, // non-int
	} {
		if err := os.WriteFile(p, []byte("prefetch_next: "+expr+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		eng, err := Load(p)
		if err != nil {
			t.Fatalf("%s: %v", expr, err)
		}
		if got := eng.EvalPrefetchNext(EntryInput{}); got != want {
			t.Errorf("%s: got %d, want %d", expr, got, want)
		}
	}

	empty, _ := Load("")
	if empty.HasPrefetchNext() || empty.EvalPrefetchNext(EntryInput{}) != 0 {
		t.Error("no policy: want no prefetch")
	}
}
