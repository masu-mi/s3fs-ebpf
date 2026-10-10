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
