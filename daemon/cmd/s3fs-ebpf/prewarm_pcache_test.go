package main

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	fpebpf "github.com/bbsakura/s3fs-ebpf/daemon/internal/ebpf"
)

func TestFindS3fsMount(t *testing.T) {
	p := filepath.Join(t.TempDir(), "mounts")
	mounts := "proc /proc proc rw 0 0\n" +
		"none /mnt/s3\\040demo s3fs_ebpf rw,relatime 0 0\n" +
		"none /mnt/other s3fs_ebpf rw 0 0\n"
	if err := os.WriteFile(p, []byte(mounts), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := findS3fsMount(p)
	if err != nil || got != "/mnt/s3 demo" {
		t.Fatalf("got %q, %v; want first mount, unescaped", got, err)
	}

	if err := os.WriteFile(p, []byte("proc /proc proc rw 0 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := findS3fsMount(p); err != nil || got != "" {
		t.Fatalf("no mount: got %q, %v", got, err)
	}
}

func TestWaitForS3fsMount(t *testing.T) {
	ctx := context.Background()
	if got, err := waitForS3fsMount(ctx, "/explicit", "/nonexistent", 0); err != nil || got != "/explicit" {
		t.Fatalf("explicit: got %q, %v", got, err)
	}
	p := filepath.Join(t.TempDir(), "mounts")
	if err := os.WriteFile(p, []byte("proc /proc proc rw 0 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := waitForS3fsMount(ctx, "", p, 10*time.Millisecond); err == nil {
		t.Fatal("want an error when no mount appears")
	}
}

func TestUnescapeMountField(t *testing.T) {
	for in, want := range map[string]string{
		`/mnt/plain`:       "/mnt/plain",
		`/mnt/a\040b`:      "/mnt/a b",
		`/mnt/tab\011x`:    "/mnt/tab\tx",
		`/mnt/back\134sl`:  `/mnt/back\sl`,
		`/mnt/short\04`:    `/mnt/short\04`,
		`/mnt/notoctal\09`: `/mnt/notoctal\09`,
	} {
		if got := unescapeMountField(in); got != want {
			t.Errorf("%q: got %q, want %q", in, got, want)
		}
	}
}

func TestEntryPaths(t *testing.T) {
	entries := []fpebpf.Entry{
		{Ino: 10, ParentIno: 0, Name: "top.txt", Mode: 0o100644},
		{Ino: 20, ParentIno: 0, Name: "datasets", Mode: 0o040755},
		{Ino: 21, ParentIno: 20, Name: "train", Mode: 0o040755},
		{Ino: 22, ParentIno: 21, Name: "shard.parquet", Mode: 0o100644},
		{Ino: 30, ParentIno: 99, Name: "orphan.txt", Mode: 0o100644}, // parent missing
		{Ino: 40, ParentIno: 1, Name: "root1.txt", Mode: 0o100644},   // 1 == root ino
	}
	want := map[uint64]string{
		10: "top.txt",
		22: "datasets/train/shard.parquet",
		40: "root1.txt",
	}
	if got := entryPaths(entries); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}

	// A parent cycle must not hang.
	cyc := []fpebpf.Entry{
		{Ino: 1001, ParentIno: 1002, Name: "a", Mode: 0o040755},
		{Ino: 1002, ParentIno: 1001, Name: "b", Mode: 0o040755},
		{Ino: 1003, ParentIno: 1001, Name: "f", Mode: 0o100644},
	}
	if got := entryPaths(cyc); len(got) != 0 {
		t.Fatalf("cycle: got %v, want none", got)
	}
}

func TestSelectWithinCapacity(t *testing.T) {
	items := []pcachePrewarmItem{{ino: 1, size: 40}, {ino: 2, size: 50}, {ino: 3, size: 10}, {ino: 4, size: 5}}
	if got, skipped := selectWithinCapacity(items, 0, 0); len(got) != 4 || skipped != 0 {
		t.Fatalf("unlimited: got %v skipped %d", got, skipped)
	}
	got, skipped := selectWithinCapacity(items, 30, 100)
	want := []pcachePrewarmItem{{ino: 1, size: 40}, {ino: 3, size: 10}, {ino: 4, size: 5}}
	if !reflect.DeepEqual(got, want) || skipped != 1 {
		t.Fatalf("capped: got %v skipped %d, want %v skipped 1", got, skipped, want)
	}
}

func TestPrewarmThroughMount(t *testing.T) {
	mnt := t.TempDir()
	if err := os.MkdirAll(filepath.Join(mnt, "d"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mnt, "a.bin"), make([]byte, 3<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mnt, "d", "b.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	saved := fpebpf.Snapshot()
	t.Cleanup(func() { fpebpf.Replace(saved) })
	fpebpf.Replace([]fpebpf.Entry{
		{Ino: 501, Name: "a.bin", Mode: 0o100644},
		{Ino: 502, Name: "d", Mode: 0o040755},
		{Ino: 503, ParentIno: 502, Name: "b.txt", Mode: 0o100644},
		{Ino: 504, Name: "missing.bin", Mode: 0o100644}, // read error is skipped
	})
	items := []pcachePrewarmItem{{ino: 501, size: 3 << 20}, {ino: 503, size: 5}, {ino: 504, size: 1}, {ino: 505, size: 1}}
	if got := prewarmThroughMount(context.Background(), mnt, items); got != 3<<20+5 {
		t.Fatalf("bytes read: got %d, want %d", got, 3<<20+5)
	}
}

func TestPcachePrewarmQueue(t *testing.T) {
	doneWarming(takePcachePrewarm())
	queuePcachePrewarm(7, 70)
	queuePcachePrewarm(8, 80)
	queuePcachePrewarm(7, 70) // already queued: dropped
	if !isWarming(7) || isWarming(9) {
		t.Fatal("warming set wrong")
	}
	got := takePcachePrewarm()
	defer doneWarming(got)
	want := []pcachePrewarmItem{{ino: 7, size: 70}, {ino: 8, size: 80}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if rest := takePcachePrewarm(); len(rest) != 0 {
		t.Fatalf("queue not drained: %v", rest)
	}
	select {
	case <-pcachePrewarmQ.kick:
	default:
		t.Fatal("queue did not kick the worker")
	}
}

func TestSelectWithinCapacityPrefetch(t *testing.T) {
	items := []pcachePrewarmItem{
		{ino: 1, size: 90},                 // startup: fills the budget
		{ino: 2, size: 20, prefetch: true}, // prefetch: not summed, ≤ cap/4
		{ino: 3, size: 30, prefetch: true}, // prefetch: > cap/4, skipped
		{ino: 4, size: 20},                 // startup: over budget, skipped
	}
	got, skipped := selectWithinCapacity(items, 0, 100)
	want := []pcachePrewarmItem{items[0], items[1]}
	if !reflect.DeepEqual(got, want) || skipped != 2 {
		t.Fatalf("got %v skipped %d, want %v skipped 2", got, skipped, want)
	}
}

func TestPrewarmDoneClearsWarming(t *testing.T) {
	doneWarming(takePcachePrewarm())
	queuePcacheItems([]pcachePrewarmItem{{ino: 11, size: 1}, {ino: 12, size: 1}})
	items := takePcachePrewarm()
	if !isWarming(11) {
		t.Fatal("taken but unread item must stay warming")
	}
	doneWarming(items)
	if isWarming(11) || isWarming(12) {
		t.Fatal("doneWarming left items")
	}
	<-pcachePrewarmQ.kick
}
