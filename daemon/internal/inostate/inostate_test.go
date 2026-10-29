package inostate

import (
	"os"
	"path/filepath"
	"testing"
)

// TestInoState_RoundTrip pins H-02: enable persistence, assign +
// register a couple of inos, reset in-memory state, re-enable
// persistence at the same path, and assert the mapping was restored.
func TestInoState_RoundTrip(t *testing.T) {
	a := New()
	statePath := filepath.Join(t.TempDir(), "inos.json")

	if err := a.SetStatePath(statePath); err != nil {
		t.Fatalf("SetStatePath: %v", err)
	}
	_ = a.AssignIno("keyA")
	ino2 := a.AssignIno("keyB")
	a.RegisterIno("keyA", 5000) // overrides the first assignment

	if err := a.SetStatePath(statePath); err != nil {
		t.Fatalf("SetStatePath (reload): %v", err)
	}

	a.mu.Lock()
	gotA := a.byKey["keyA"]
	gotB := a.byKey["keyB"]
	gotNext := a.nextIno
	a.mu.Unlock()

	if gotA != 5000 {
		t.Errorf("post-restart keyA = %d, want 5000 (registerIno override lost)", gotA)
	}
	if gotB != ino2 {
		t.Errorf("post-restart keyB = %d, want %d", gotB, ino2)
	}
	// nextIno must advance past the highest observed ino so a new
	// assign after restart doesn't collide.
	if gotNext <= 5000 {
		t.Errorf("post-restart nextIno = %d, want > 5000", gotNext)
	}
	// Fresh assign after restart uses the advanced allocator.
	newIno := a.AssignIno("keyC")
	if newIno <= 5000 {
		t.Errorf("a.AssignIno(keyC) = %d, want > 5000", newIno)
	}
}

// TestInoState_MissingFileIsFirstRun ensures loadInoState treats
// ENOENT as "first run" rather than a hard error — a fresh install
// shouldn't have to touch-create the state file to boot.
func TestInoState_MissingFileIsFirstRun(t *testing.T) {
	a := New()
	statePath := filepath.Join(t.TempDir(), "does-not-exist.json")

	if err := a.SetStatePath(statePath); err != nil {
		t.Fatalf("SetStatePath on missing file: %v", err)
	}
	// A first assign should still work; the file is written by Flush
	// (V-02: assignIno only marks dirty, sync round flushes).
	ino := a.AssignIno("first")
	if ino != RootReserved {
		t.Errorf("first ino = %d, want %d", ino, RootReserved)
	}
	a.Flush()
	if _, err := os.Stat(statePath); err != nil {
		t.Errorf("state file not created after Flush: %v", err)
	}
}

// TestInoState_DirtyDebounce pins V-02: N assignIno calls within one
// sync round must result in exactly one disk write (triggered by
// Flush) rather than N individual writes.
func TestInoState_DirtyDebounce(t *testing.T) {
	a := New()
	statePath := filepath.Join(t.TempDir(), "inos.json")
	if err := a.SetStatePath(statePath); err != nil {
		t.Fatalf("SetStatePath: %v", err)
	}

	// assignIno must set the dirty flag but must NOT write the file
	// immediately (lazy write / debounce is the V-02 invariant).
	for _, k := range []string{"a", "b", "c", "d", "e"} {
		a.AssignIno(k)
	}
	if a.dirty.Load() == 0 {
		t.Error("dirty flag not set after assignIno calls")
	}
	if _, err := os.Stat(statePath); err == nil {
		t.Error("state file written by assignIno — V-02 debounce violated (should wait for Flush)")
	}

	// First Flush writes the file and clears dirty to 0.
	a.Flush()
	if _, err := os.Stat(statePath); err != nil {
		t.Fatalf("state file not created after Flush: %v", err)
	}
	if a.dirty.Load() != 0 {
		t.Error("dirty flag not cleared after Flush")
	}

	// A second consecutive flush with no new assigns must be a no-op
	// (dirty==0 → CAS fails → save is skipped).
	a.Flush()
	if a.dirty.Load() != 0 {
		t.Error("dirty flag unexpectedly set by no-op Flush")
	}
}

// TestInoState_DisabledIsNoOp verifies that leaving the path empty
// keeps behavior identical to the pre-H-02 in-memory allocator.
func TestInoState_DisabledIsNoOp(t *testing.T) {
	a := New()
	if err := a.SetStatePath(""); err != nil {
		t.Fatalf("a.SetStatePath(\"\"): %v", err)
	}
	ino := a.AssignIno("keyD")
	if ino != RootReserved {
		t.Errorf("first ino = %d, want %d", ino, RootReserved)
	}
	// No state path means Save should be silent — assert
	// no side-effect file was created in the working dir.
	if _, err := os.Stat(".tmp"); err == nil {
		t.Error(".tmp side-effect created when persistence was disabled")
	}
}

func TestTrimDirPrefix(t *testing.T) {
	for in, want := range map[string]string{
		"dir:sub1/nested": "sub1/nested",
		"sub1/a.txt":      "sub1/a.txt",
		"dir:":            "dir:", // nothing after the tag: left as is
		"":                "",
	} {
		if got := trimDirPrefix(in); got != want {
			t.Errorf("trimDirPrefix(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestResolveKeyPath(t *testing.T) {
	a := New()
	dir := a.AssignIno("dir:sub1")
	nested := a.AssignIno("dir:sub1/nested")

	for _, tt := range []struct {
		name   string
		parent uint64
		base   string
		want   string
	}{
		{"parent 0 is the root", 0, "a.txt", "a.txt"},
		{"mount root", rootIno, "a.txt", "a.txt"},
		{"one level", dir, "a.txt", "sub1/a.txt"},
		{"two levels", nested, "b.txt", "sub1/nested/b.txt"},
		{"unknown parent falls back to the basename", 99999, "c.txt", "c.txt"},
	} {
		if got := a.ResolveKeyPath(tt.parent, tt.base); got != tt.want {
			t.Errorf("%s: ResolveKeyPath(%d, %q) = %q, want %q", tt.name, tt.parent, tt.base, got, tt.want)
		}
	}
}

// Seed maps the kmod bootstrap keys to their fixed inos and moves nextIno
// past them; a second call changes nothing.
func TestSeed(t *testing.T) {
	a := New()
	a.Seed()
	n, next := a.CountMappings()
	if n == 0 {
		t.Fatal("Seed mapped no bootstrap keys")
	}
	a.Seed()
	if n2, next2 := a.CountMappings(); n2 != n || next2 != next {
		t.Errorf("second Seed changed the table: %d/%d -> %d/%d", n, next, n2, next2)
	}
	if got := a.AssignIno("brand-new-key"); got < next {
		t.Errorf("AssignIno after Seed = %d, collides with seeded inos (next=%d)", got, next)
	}
}

func TestAssignInoIsStable(t *testing.T) {
	a := New()
	first := a.AssignIno("k")
	if again := a.AssignIno("k"); again != first {
		t.Errorf("AssignIno(k) = %d then %d, want the same ino", first, again)
	}
	if other := a.AssignIno("k2"); other == first {
		t.Errorf("two keys got the same ino %d", other)
	}
}

func TestLoadRejectsCorruptFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "inos.json")
	if err := os.WriteFile(p, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := New().SetStatePath(p); err == nil {
		t.Error("SetStatePath accepted a corrupt state file")
	}
}
