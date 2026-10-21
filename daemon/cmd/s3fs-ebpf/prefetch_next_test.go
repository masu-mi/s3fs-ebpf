package main

import (
	"os"
	"path/filepath"
	"testing"

	fpebpf "github.com/bbsakura/s3fs-ebpf/daemon/internal/ebpf"
	"github.com/bbsakura/s3fs-ebpf/daemon/internal/policy"
)

func prefetchTestEntries() []fpebpf.Entry {
	return []fpebpf.Entry{
		{Ino: 600, Name: "train", Mode: 0o040755},
		{Ino: 603, ParentIno: 600, Name: "shard-0003", S3Key: "train/shard-0003", Mode: 0o100644, Fallback: 30},
		{Ino: 601, ParentIno: 600, Name: "shard-0001", S3Key: "train/shard-0001", Mode: 0o100644, Fallback: 10},
		{Ino: 602, ParentIno: 600, Name: "shard-0002", S3Key: "train/shard-0002", Mode: 0o100644, Fallback: 20},
		{Ino: 604, ParentIno: 600, Name: "shard-0004", S3Key: "train/shard-0004", Mode: 0o100644, Fallback: 40},
		{Ino: 605, ParentIno: 600, Name: "zz-sub", Mode: 0o040755},
		{Ino: 610, Name: "root-a", S3Key: "root-a", Mode: 0o100644},
		{Ino: 611, ParentIno: 1, Name: "root-b", S3Key: "root-b", Mode: 0o100644},
	}
}

func inos(es []fpebpf.Entry) []uint64 {
	out := make([]uint64, len(es))
	for i, e := range es {
		out[i] = e.Ino
	}
	return out
}

func TestNextSiblings(t *testing.T) {
	es := prefetchTestEntries()
	byIno := map[uint64]fpebpf.Entry{}
	for _, e := range es {
		byIno[e.Ino] = e
	}
	cases := []struct {
		cur  uint64
		n    int
		want []uint64
	}{
		{601, 2, []uint64{602, 603}},
		{602, 5, []uint64{603, 604}}, // directories are skipped
		{604, 3, []uint64{}},
		{610, 1, []uint64{611}}, // parent 0 and 1 are both the root
	}
	for _, c := range cases {
		got := inos(nextSiblings(es, byIno[c.cur], c.n))
		if len(got) != len(c.want) {
			t.Errorf("cur=%d n=%d: got %v, want %v", c.cur, c.n, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("cur=%d n=%d: got %v, want %v", c.cur, c.n, got, c.want)
				break
			}
		}
	}
}

func TestMaybePrefetchSiblings(t *testing.T) {
	saved := fpebpf.Snapshot()
	t.Cleanup(func() { fpebpf.Replace(saved) })
	fpebpf.Replace(prefetchTestEntries())

	p := filepath.Join(t.TempDir(), "pf.cel")
	if err := os.WriteFile(p, []byte("prefetch_next: entry.s3_key.startsWith(\"train/\") ? 2 : 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	eng, err := policy.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	savedPol := prefetchPolicy
	t.Cleanup(func() { prefetchPolicy = savedPol; pcacheWorkerUp.Store(false) })
	doneWarming(takePcachePrewarm())

	prefetchPolicy = eng
	maybePrefetchSiblings(601) // worker not up: nothing queued
	if got := takePcachePrewarm(); len(got) != 0 {
		t.Fatalf("queued without a worker: %v", got)
	}

	pcacheWorkerUp.Store(true)
	maybePrefetchSiblings(601)
	got := takePcachePrewarm()
	want := []pcachePrewarmItem{{ino: 602, size: 20, prefetch: true}, {ino: 603, size: 30, prefetch: true}}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("got %v, want %v", got, want)
	}

	// The worker's own read of 602 misses: it must not chain to 603/604.
	maybePrefetchSiblings(602)
	if rest := takePcachePrewarm(); len(rest) != 0 {
		t.Fatalf("worker read triggered prefetch: %v", rest)
	}
	doneWarming(got)

	// A user read of 603 after the worker finished prefetches 604 only.
	maybePrefetchSiblings(603)
	got = takePcachePrewarm()
	if len(got) != 1 || got[0].ino != 604 {
		t.Fatalf("got %v, want [604]", got)
	}
	doneWarming(got)

	// Outside train/ the policy says 0.
	maybePrefetchSiblings(610)
	if rest := takePcachePrewarm(); len(rest) != 0 {
		t.Fatalf("root file prefetched: %v", rest)
	}
	select {
	case <-pcachePrewarmQ.kick:
	default:
	}
}
