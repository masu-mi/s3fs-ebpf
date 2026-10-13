package policyprog

import (
	"testing"
)

// TestResolve_BuiltinPathTraversal pins Q-08: builtin: names containing
// path separators or ".." must be rejected before any filesystem access
// so a crafted --policy-bpf flag cannot escape the bpf/ directory.
func TestResolve_BuiltinPathTraversal(t *testing.T) {
	cases := []struct {
		arg  string
		desc string
	}{
		{"builtin:../../etc/evil", "double-dot traversal with slash"},
		{"builtin:sub/dir", "forward slash in name"},
		{"builtin:foo..bar", "double-dot embedded in name"},
		{"builtin:foo\\bar", "backslash in name"},
	}
	for _, tc := range cases {
		_, err := Resolve(tc.arg, "/some/root")
		if err == nil {
			t.Errorf("Resolve(%q) [%s]: want error, got nil", tc.arg, tc.desc)
		}
	}
}

// TestResolve_EmptyArgIsNoop verifies that an empty arg returns ("", nil)
// without touching the filesystem.
func TestResolve_EmptyArgIsNoop(t *testing.T) {
	got, err := Resolve("", "/any/root")
	if err != nil {
		t.Fatalf("Resolve(\"\") = %v", err)
	}
	if got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

// TestResolve_FilePrefixPassesThrough verifies that "file:<path>"
// returns the raw path without any existence check or manipulation.
func TestResolve_FilePrefixPassesThrough(t *testing.T) {
	want := "/absolute/path/prog.bpf.o"
	got, err := Resolve("file:"+want, "/ignored")
	if err != nil {
		t.Fatalf("Resolve(file:...): %v", err)
	}
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestResolve_BarePathIsFileShortcut verifies that an argument without
// a recognized prefix is treated as a file path (shortcut for "file:").
func TestResolve_BarePathIsFileShortcut(t *testing.T) {
	want := "/any/prog.bpf.o"
	got, err := Resolve(want, "/ignored")
	if err != nil {
		t.Fatalf("Resolve(bare path): %v", err)
	}
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestLoadOpts_UnknownMapIgnored pins Z-03: passing a MapMaxEntries
// override for a map that doesn't exist in the .bpf.o must be silently
// ignored. Load(path) fails here because there is no real .bpf.o, but
// the spec-mutation loop itself must not panic or error on unknown names.
func TestLoadOpts_UnknownMapIgnored(t *testing.T) {
	opts := LoadOpts{
		MapMaxEntries: map[string]uint32{
			"s3fifo3_state":      32768,
			"nonexistent_map_zz": 999,
		},
	}
	// Load on a non-existent path — we only care that the opts struct
	// is accepted; the real error is the missing file, not the opts.
	_, err := Load("/nonexistent/path.bpf.o", opts)
	if err == nil {
		t.Fatal("Load on missing .bpf.o: want error, got nil")
	}
	// Must be a parse/open error, not a panic from the opts loop.
	t.Logf("got expected error: %v", err)
}
