package policyprog

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strings"
)

// Allowlist gates which user-supplied .bpf.o files daemon will load
// via `file:<path>` or bare-path arguments. Built-in programs
// (`builtin:<name>`) bypass verification — those are engine-shipped
// under repo control.
//
// Format (one entry per line):
//
//	<sha256-hex>  <description...>
//
// Comments (`#`) and blank lines are ignored. Descriptions are
// free-form and only used for log output on match/miss.
//
// The whole file is loaded into memory at startup; changes require a
// daemon restart. This is deliberate — the allowlist is meant to be
// a static supply-chain gate, not a runtime tunable.
type Allowlist struct {
	hashes map[string]string // hex-sha256 (lowercase) → description
	path   string
}

// LoadAllowlist reads the file at path. A non-existent path returns
// (nil, os.ErrNotExist); callers decide whether that means strict
// reject-all or permissive allow-all. Malformed lines are a hard
// error — the operator should notice a broken allowlist immediately.
func LoadAllowlist(path string) (*Allowlist, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	a := &Allowlist{
		hashes: make(map[string]string),
		path:   path,
	}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.SplitN(line, " ", 2)
		hash := strings.ToLower(strings.TrimSpace(fields[0]))
		if len(hash) != 64 {
			return nil, fmt.Errorf(
				"allowlist %s: invalid sha256 length in %q "+
					"(expected 64 hex chars, got %d)",
				path, fields[0], len(hash))
		}
		if _, err := hex.DecodeString(hash); err != nil {
			return nil, fmt.Errorf(
				"allowlist %s: %q is not hex: %w",
				path, fields[0], err)
		}
		desc := ""
		if len(fields) > 1 {
			desc = strings.TrimSpace(fields[1])
		}
		a.hashes[hash] = desc
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return a, nil
}

// Size returns the number of entries. Used by the daemon for the
// "N approved policies" startup log line.
func (a *Allowlist) Size() int {
	if a == nil {
		return 0
	}
	return len(a.hashes)
}

// Path returns the allowlist file path (for logs / errors). Empty
// when a is nil.
func (a *Allowlist) Path() string {
	if a == nil {
		return ""
	}
	return a.path
}

// Verify hashes the file at filePath and checks against the
// allowlist. Returns nil on match, an error naming the sha256 on
// miss. When a is nil the caller has opted out of verification
// (permissive dev mode) — Verify returns nil.
func (a *Allowlist) Verify(filePath string) error {
	if a == nil {
		return nil
	}
	h, err := HashFile(filePath)
	if err != nil {
		return fmt.Errorf("allowlist: hashing %s: %w", filePath, err)
	}
	if desc, ok := a.hashes[h]; ok {
		if desc != "" {
			fmt.Fprintf(os.Stderr,
				"[allowlist] approved: %s (%s) matched %q\n",
				filePath, h[:12]+"...", desc)
		}
		return nil
	}
	return fmt.Errorf(
		"allowlist: %s (sha256=%s) not in %s",
		filePath, h, a.path)
}

// HashFile computes the lowercase hex sha256 of the file at path.
// Exported so operators can compute hashes for allowlist entries
// via `s3fs-ebpf hash-policy <path>` or similar.
func HashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
