package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSetKmodParam(t *testing.T) {
	dir := t.TempDir()
	orig := kmodParamDir
	kmodParamDir = dir
	defer func() { kmodParamDir = orig }()

	p := filepath.Join(dir, "capacity_bytes")
	if err := os.WriteFile(p, []byte("0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := setKmodParam("capacity_bytes", "352"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); string(b) != "352" {
		t.Fatalf("capacity_bytes = %q, want 352", b)
	}
	if err := setKmodParam("missing", "1"); err == nil {
		t.Fatal("want error for a parameter the module does not have")
	}
}
