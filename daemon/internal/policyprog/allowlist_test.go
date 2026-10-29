package policyprog

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	// sha256("hello\n")
	helloSum = "5891b5b522d5df086d0ff0b110fbd9d21bb4fc7163af34d08286a2e846f6be03"
	// sha256("")
	emptySum = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
)

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestHashFile(t *testing.T) {
	dir := t.TempDir()
	for name, tt := range map[string]struct{ content, want string }{
		"hello": {"hello\n", helloSum},
		"empty": {"", emptySum},
	} {
		got, err := HashFile(writeFile(t, dir, name, tt.content))
		if err != nil || got != tt.want {
			t.Errorf("HashFile(%s) = %q, %v; want %q", name, got, err, tt.want)
		}
	}
	if _, err := HashFile(filepath.Join(dir, "missing")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("HashFile(missing) err = %v, want ErrNotExist", err)
	}
}

func TestLoadAllowlist(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "allow.txt", "# comment\n\n"+
		strings.ToUpper(helloSum)+"  upper-case hash, with description\n"+
		emptySum+"\n")
	a, err := LoadAllowlist(p)
	if err != nil {
		t.Fatal(err)
	}
	if a.Size() != 2 || a.Path() != p {
		t.Errorf("Size=%d Path=%q, want 2 and %q", a.Size(), a.Path(), p)
	}
}

func TestLoadAllowlistErrors(t *testing.T) {
	dir := t.TempDir()
	if _, err := LoadAllowlist(filepath.Join(dir, "nope")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("missing file: err = %v, want ErrNotExist", err)
	}
	for name, content := range map[string]string{
		"short":   "abcd desc\n",
		"not-hex": strings.Repeat("z", 64) + " desc\n",
	} {
		if _, err := LoadAllowlist(writeFile(t, dir, name, content)); err == nil {
			t.Errorf("%s: malformed line accepted", name)
		}
	}
}

func TestVerify(t *testing.T) {
	dir := t.TempDir()
	obj := writeFile(t, dir, "p.bpf.o", "hello\n")
	other := writeFile(t, dir, "q.bpf.o", "something else\n")
	a, err := LoadAllowlist(writeFile(t, dir, "allow.txt", helloSum+" hello object\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Verify(obj); err != nil {
		t.Errorf("listed file rejected: %v", err)
	}
	err = a.Verify(other)
	if err == nil || !strings.Contains(err.Error(), "not in") {
		t.Errorf("unlisted file: err = %v, want a 'not in' error", err)
	}
	if err := a.Verify(filepath.Join(dir, "missing")); err == nil {
		t.Error("missing file verified")
	}
}

// A nil allowlist means the operator opted out of verification.
func TestNilAllowlistIsPermissive(t *testing.T) {
	var a *Allowlist
	if a.Size() != 0 || a.Path() != "" {
		t.Errorf("nil allowlist: Size=%d Path=%q", a.Size(), a.Path())
	}
	if err := a.Verify("/does/not/matter"); err != nil {
		t.Errorf("nil allowlist rejected a file: %v", err)
	}
}
