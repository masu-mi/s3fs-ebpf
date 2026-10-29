package mountinfo

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const fsType = "s3fs_ebpf"

func TestFind(t *testing.T) {
	p := filepath.Join(t.TempDir(), "mounts")
	mounts := "proc /proc proc rw 0 0\n" +
		"none /mnt/s3\\040demo s3fs_ebpf rw,relatime 0 0\n" +
		"none /mnt/other s3fs_ebpf rw 0 0\n"
	if err := os.WriteFile(p, []byte(mounts), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := Find(p, fsType)
	if err != nil || got != "/mnt/s3 demo" {
		t.Fatalf("got %q, %v; want first mount, unescaped", got, err)
	}

	if err := os.WriteFile(p, []byte("proc /proc proc rw 0 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := Find(p, fsType); err != nil || got != "" {
		t.Fatalf("no mount: got %q, %v", got, err)
	}
}

func TestWait(t *testing.T) {
	ctx := context.Background()
	if got, err := Wait(ctx, "/explicit", "/nonexistent", fsType, 0); err != nil || got != "/explicit" {
		t.Fatalf("explicit: got %q, %v", got, err)
	}
	p := filepath.Join(t.TempDir(), "mounts")
	if err := os.WriteFile(p, []byte("proc /proc proc rw 0 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Wait(ctx, "", p, fsType, 10*time.Millisecond); err == nil {
		t.Fatal("want an error when no mount appears")
	}
}

func TestUnescape(t *testing.T) {
	for in, want := range map[string]string{
		`/mnt/plain`:       "/mnt/plain",
		`/mnt/a\040b`:      "/mnt/a b",
		`/mnt/tab\011x`:    "/mnt/tab\tx",
		`/mnt/back\134sl`:  `/mnt/back\sl`,
		`/mnt/short\04`:    `/mnt/short\04`,
		`/mnt/notoctal\09`: `/mnt/notoctal\09`,
	} {
		if got := Unescape(in); got != want {
			t.Errorf("%q: got %q, want %q", in, got, want)
		}
	}
}
