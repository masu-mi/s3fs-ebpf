package main

import (
	"os"
	"path/filepath"
	"testing"
)

// resetInoStateForTest returns the allocator to its zero state so
// concurrent test cases don't leak mutations. Callers should run this
// via t.Cleanup at the start of each test.
func resetInoStateForTest(t *testing.T) {
	t.Helper()
	inoAlloc = newInoAllocator()
}

// TestInoState_RoundTrip pins H-02: enable persistence, assign +
// register a couple of inos, reset in-memory state, re-enable
// persistence at the same path, and assert the mapping was restored.
func TestInoState_RoundTrip(t *testing.T) {
	resetInoStateForTest(t)
	statePath := filepath.Join(t.TempDir(), "inos.json")

	if err := SetInoStatePath(statePath); err != nil {
		t.Fatalf("SetInoStatePath: %v", err)
	}
	_ = assignIno("keyA")
	ino2 := assignIno("keyB")
	registerIno("keyA", 5000) // overrides the first assignment

	// Simulate daemon restart: wipe in-memory state, then reload.
	resetInoStateForTest(t)
	if err := SetInoStatePath(statePath); err != nil {
		t.Fatalf("SetInoStatePath (reload): %v", err)
	}

	inoAlloc.mu.Lock()
	gotA := inoAlloc.byKey["keyA"]
	gotB := inoAlloc.byKey["keyB"]
	gotNext := inoAlloc.nextIno
	inoAlloc.mu.Unlock()

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
	newIno := assignIno("keyC")
	if newIno <= 5000 {
		t.Errorf("assignIno(keyC) = %d, want > 5000", newIno)
	}
}

// TestInoState_MissingFileIsFirstRun ensures loadInoState treats
// ENOENT as "first run" rather than a hard error — a fresh install
// shouldn't have to touch-create the state file to boot.
func TestInoState_MissingFileIsFirstRun(t *testing.T) {
	resetInoStateForTest(t)
	statePath := filepath.Join(t.TempDir(), "does-not-exist.json")

	if err := SetInoStatePath(statePath); err != nil {
		t.Fatalf("SetInoStatePath on missing file: %v", err)
	}
	// A first assign should still work; the file is written by flushInoState
	// (V-02: assignIno only marks dirty, sync round flushes).
	ino := assignIno("first")
	if ino != inoRootReserved {
		t.Errorf("first ino = %d, want %d", ino, inoRootReserved)
	}
	flushInoState()
	if _, err := os.Stat(statePath); err != nil {
		t.Errorf("state file not created after flushInoState: %v", err)
	}
}

// TestInoState_DirtyDebounce pins V-02: N assignIno calls within one
// sync round must result in exactly one disk write (triggered by
// flushInoState) rather than N individual writes.
func TestInoState_DirtyDebounce(t *testing.T) {
	resetInoStateForTest(t)
	statePath := filepath.Join(t.TempDir(), "inos.json")
	if err := SetInoStatePath(statePath); err != nil {
		t.Fatalf("SetInoStatePath: %v", err)
	}

	// assignIno must set the dirty flag but must NOT write the file
	// immediately (lazy write / debounce is the V-02 invariant).
	for _, k := range []string{"a", "b", "c", "d", "e"} {
		assignIno(k)
	}
	if inoAlloc.dirty.Load() == 0 {
		t.Error("dirty flag not set after assignIno calls")
	}
	if _, err := os.Stat(statePath); err == nil {
		t.Error("state file written by assignIno — V-02 debounce violated (should wait for flushInoState)")
	}

	// First flushInoState writes the file and clears dirty to 0.
	flushInoState()
	if _, err := os.Stat(statePath); err != nil {
		t.Fatalf("state file not created after flushInoState: %v", err)
	}
	if inoAlloc.dirty.Load() != 0 {
		t.Error("dirty flag not cleared after flushInoState")
	}

	// A second consecutive flush with no new assigns must be a no-op
	// (dirty==0 → CAS fails → save is skipped).
	flushInoState()
	if inoAlloc.dirty.Load() != 0 {
		t.Error("dirty flag unexpectedly set by no-op flushInoState")
	}
}

// TestInoState_DisabledIsNoOp verifies that leaving the path empty
// keeps behavior identical to the pre-H-02 in-memory allocator.
func TestInoState_DisabledIsNoOp(t *testing.T) {
	resetInoStateForTest(t)
	if err := SetInoStatePath(""); err != nil {
		t.Fatalf("SetInoStatePath(\"\"): %v", err)
	}
	ino := assignIno("keyD")
	if ino != inoRootReserved {
		t.Errorf("first ino = %d, want %d", ino, inoRootReserved)
	}
	// No state path means saveInoState should be silent — assert
	// no side-effect file was created in the working dir.
	if _, err := os.Stat(".tmp"); err == nil {
		t.Error(".tmp side-effect created when persistence was disabled")
	}
}
