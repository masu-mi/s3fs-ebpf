package main

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	fpebpf "github.com/bbsakura/s3fs-ebpf/daemon/internal/ebpf"
)

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
	items := []pcachePrewarmItem{{Ino: 501, Size: 3 << 20}, {Ino: 503, Size: 5}, {Ino: 504, Size: 1}, {Ino: 505, Size: 1}}
	if got := prewarmThroughMount(context.Background(), mnt, items); got != 3<<20+5 {
		t.Fatalf("bytes read: got %d, want %d", got, 3<<20+5)
	}
}
