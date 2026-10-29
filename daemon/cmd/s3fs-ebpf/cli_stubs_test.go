package main

import (
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/bbsakura/s3fs-ebpf/daemon/internal/objstore"
)

func TestEnvHelpers(t *testing.T) {
	t.Setenv("S3FS_TEST_STR", "from-env")
	if got := envOr("S3FS_TEST_STR", "fb"); got != "from-env" {
		t.Errorf("envOr(set) = %q", got)
	}
	if got := envOr("S3FS_TEST_UNSET", "fb"); got != "fb" {
		t.Errorf("envOr(unset) = %q, want the fallback", got)
	}
	t.Setenv("S3FS_TEST_EMPTY", "")
	if got := envOr("S3FS_TEST_EMPTY", "fb"); got != "fb" {
		t.Errorf("envOr(empty) = %q, want the fallback", got)
	}

	for val, want := range map[string]int64{"4096": 4096, "-1": -1, "junk": 7, "": 7} {
		t.Setenv("S3FS_TEST_INT", val)
		if got := envInt64("S3FS_TEST_INT", 7); got != want {
			t.Errorf("envInt64(%q) = %d, want %d", val, got, want)
		}
	}
}

func TestGetKmodParamInt64(t *testing.T) {
	dir := t.TempDir()
	orig := kmodParamDir
	kmodParamDir = dir
	defer func() { kmodParamDir = orig }()

	for name, tt := range map[string]struct {
		content string
		want    int64
		ok      bool
	}{
		"plain":    {"352\n", 352, true},
		"negative": {"-1\n", -1, true},
		"padded":   {"  8 \n", 8, true},
		"garbage":  {"abc\n", 0, false},
		"empty":    {"", 0, false},
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(tt.content), 0o644); err != nil {
			t.Fatal(err)
		}
		if got, ok := getKmodParamInt64(name); got != tt.want || ok != tt.ok {
			t.Errorf("%s: got (%d, %v), want (%d, %v)", name, got, ok, tt.want, tt.ok)
		}
	}
	if _, ok := getKmodParamInt64("missing"); ok {
		t.Error("a parameter the module does not have reported ok")
	}
}

func TestBackendFlagsRejectsUnknownBackend(t *testing.T) {
	fs := newTestFlagSet()
	build := backendFlags(fs)
	if err := fs.Parse([]string{"--backend", "nonsense"}); err != nil {
		t.Fatal(err)
	}
	if _, err := build(); err == nil {
		t.Error("an unknown --backend was accepted")
	}
}

func TestBackendFlagsDefaultsFromEnv(t *testing.T) {
	t.Setenv("S3FS_BACKEND", objstore.KindGCS)
	fs := newTestFlagSet()
	backendFlags(fs)
	if f := fs.Lookup("backend"); f == nil || f.DefValue != objstore.KindGCS {
		t.Errorf("--backend default = %v, want %q from S3FS_BACKEND", f, objstore.KindGCS)
	}
	if f := fs.Lookup("bucket"); f == nil || f.DefValue != "s3fs-test" {
		t.Errorf("--bucket default = %v, want s3fs-test", f)
	}
}

// The skeleton commands only print; they must exit 0 and accept their flags.
func TestSkeletonCommandsExitZero(t *testing.T) {
	for name, run := range map[string]func([]string) int{
		"mount":   runMount,
		"unmount": runUnmount,
		"stats":   runStats,
		"policy":  runPolicy,
	} {
		if code := run(nil); code != 0 {
			t.Errorf("%s with no args exited %d", name, code)
		}
	}
	if code := runMount([]string{"-bucket", "b", "--mount-point", "/m"}); code != 0 {
		t.Errorf("mount with flags exited %d", code)
	}
	if code := runPolicy([]string{"-action", "reload", "-plugin", "p"}); code != 0 {
		t.Errorf("policy with flags exited %d", code)
	}
}

func newTestFlagSet() *flag.FlagSet { return flag.NewFlagSet("t", flag.ContinueOnError) }
