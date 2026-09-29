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
	inoMu.Lock()
	inoByKey = map[string]uint64{}
	nextIno = uint64(inoRootReserved)
	inoStatePath = ""
	seededIno = false
	inoMu.Unlock()
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

	inoMu.Lock()
	gotA := inoByKey["keyA"]
	gotB := inoByKey["keyB"]
	gotNext := nextIno
	inoMu.Unlock()

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
	// A first assign should still work and populate the file.
	ino := assignIno("first")
	if ino != inoRootReserved {
		t.Errorf("first ino = %d, want %d", ino, inoRootReserved)
	}
	if _, err := os.Stat(statePath); err != nil {
		t.Errorf("state file not created after first assignIno: %v", err)
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
