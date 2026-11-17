package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveShareRoot(t *testing.T) {
	t.Run("env wins", func(t *testing.T) {
		t.Setenv("S3FS_EBPF_ROOT", "/opt/x")
		if got := resolveShareRoot(); got != "/opt/x" {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("cwd with bpf", func(t *testing.T) {
		t.Setenv("S3FS_EBPF_ROOT", "")
		d := t.TempDir()
		if err := os.Mkdir(filepath.Join(d, "bpf"), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Chdir(d)
		want, _ := filepath.EvalSymlinks(d)
		if got, _ := filepath.EvalSymlinks(resolveShareRoot()); got != want {
			t.Fatalf("got %q want %q", got, want)
		}
	})
	t.Run("parent of bin/", func(t *testing.T) {
		t.Setenv("S3FS_EBPF_ROOT", "")
		d := t.TempDir()
		_ = os.Mkdir(filepath.Join(d, "bpf"), 0o755)
		_ = os.Mkdir(filepath.Join(d, "bin"), 0o755)
		t.Chdir(filepath.Join(d, "bin"))
		want, _ := filepath.EvalSymlinks(d)
		if got, _ := filepath.EvalSymlinks(resolveShareRoot()); got != want {
			t.Fatalf("got %q want %q", got, want)
		}
	})
}
