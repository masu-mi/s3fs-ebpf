// Package inostate is the S3 key -> ino mapping the daemon hands to the
// kmod, with optional on-disk persistence so inos survive a restart.
package inostate

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	fpebpf "github.com/bbsakura/s3fs-ebpf/daemon/internal/ebpf"
)

const (
	// RootReserved is the first ino handed out; ino 1 is the mount root.
	RootReserved = 100
	rootIno      = uint64(1)
)

// Allocator is the S3Key → ino mapping. Previously spread across
// package globals; collected here (W-02) so the state is named,
// testable, and can be zero'd cleanly between test cases.
//
// Convention for pathByIno: dirIno → "sub1/nested" (no trailing '/'),
// fileIno → "sub1/nested/foo.txt". Root is not stored (empty prefix).
// Synthetic dir keys use the "dir:" prefix in byKey; pathByIno drops
// it so callers see the actual S3-side path.
type Allocator struct {
	mu        sync.Mutex
	byKey     map[string]uint64 // S3Key → ino, populated first-seen
	pathByIno map[uint64]string // G-01 reverse map
	nextIno   uint64
	seeded    bool

	// H-02: persistence path. Non-empty → flush on every registerIno and
	// once per sync round (V-02 debounce via dirty flag).
	statePath string
	dirty     atomic.Uint32
}

// New returns an empty in-memory allocator.
func New() *Allocator {
	return &Allocator{
		byKey:     map[string]uint64{},
		pathByIno: map[uint64]string{},
		nextIno:   uint64(RootReserved),
	}
}

// Seed maps bootstrap S3 keys to their well-known inos so that a fresh
// sync against a bucket that happens to contain those keys stays
// compatible with existing docs / kmod bootstrap.
func (a *Allocator) Seed() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.seeded {
		return
	}
	for _, e := range fpebpf.Bootstrap() {
		a.byKey[e.S3Key] = e.Ino
		if e.Ino >= a.nextIno {
			a.nextIno = e.Ino + 1
		}
	}
	a.seeded = true
}

// AssignIno returns the ino for s3Key, allocating a new one on first
// sight. Deterministic within a daemon process; when H-02 persistence
// is enabled (SetStatePath), also stable across restarts.
func (a *Allocator) AssignIno(s3Key string) uint64 {
	a.mu.Lock()
	if ino, ok := a.byKey[s3Key]; ok {
		a.mu.Unlock()
		return ino
	}
	ino := a.nextIno
	a.nextIno++
	a.byKey[s3Key] = ino
	a.pathByIno[ino] = trimDirPrefix(s3Key)
	a.mu.Unlock()
	// Mark dirty; the sync round that called us will flush once at the
	// end via flush rather than writing on every new key (V-02).
	a.dirty.Store(1)
	return ino
}

// RegisterIno pins a specific ino to s3Key, overriding whatever the
// allocator would have picked. Called by the flush loop after a PUT for
// a locally-created file (kmod-picked ino) so the next sync round
// emits the same ino and iterate_shared unifies the two views.
func (a *Allocator) RegisterIno(s3Key string, ino uint64) {
	a.mu.Lock()
	prev, hadPrev := a.byKey[s3Key]
	a.byKey[s3Key] = ino
	a.pathByIno[ino] = trimDirPrefix(s3Key)
	if ino >= a.nextIno {
		a.nextIno = ino + 1
	}
	a.mu.Unlock()
	if !hadPrev || prev != ino {
		a.Save()
	}
}

// ResolveKeyPath rebuilds a full S3 key from a parent ino + basename.
// Returns basename alone when parentIno resolves to root (or is 0).
func (a *Allocator) ResolveKeyPath(parentIno uint64, name string) string {
	if parentIno == 0 || parentIno == rootIno {
		return name
	}
	a.mu.Lock()
	prefix, ok := a.pathByIno[parentIno]
	a.mu.Unlock()
	if !ok || prefix == "" {
		return name
	}
	return prefix + "/" + name
}

// SetStatePath enables H-02 persistence. If p names an existing file it
// is loaded into the ino table (byKey merges, nextIno advances past any
// observed ino) before the sync loop starts. Passing "" is a no-op and
// leaves the allocator in-memory only.
func (a *Allocator) SetStatePath(p string) error {
	a.statePath = p
	if p == "" {
		return nil
	}
	dir := filepath.Dir(p)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("ino state mkdir %s: %w", dir, err)
	}
	return a.load()
}

// CountMappings returns the current number of S3Key → ino entries and
// the next ino that would be allocated. Used by the startup log line.
func (a *Allocator) CountMappings() (n int, next uint64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.byKey), a.nextIno
}

type inoStateFile struct {
	InoByKey map[string]uint64 `json:"ino_by_key"`
}

func (a *Allocator) load() error {
	data, err := os.ReadFile(a.statePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("ino state read %s: %w", a.statePath, err)
	}
	var st inoStateFile
	if err := json.Unmarshal(data, &st); err != nil {
		return fmt.Errorf("ino state parse %s: %w", a.statePath, err)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for k, v := range st.InoByKey {
		a.byKey[k] = v
		a.pathByIno[v] = trimDirPrefix(k)
		if v >= a.nextIno {
			a.nextIno = v + 1
		}
	}
	return nil
}

// Save writes statePath atomically (tmp + rename). No-op when persistence
// is disabled. Failure is logged and swallowed.
func (a *Allocator) Save() {
	if a.statePath == "" {
		return
	}
	a.mu.Lock()
	snap := inoStateFile{InoByKey: make(map[string]uint64, len(a.byKey))}
	for k, v := range a.byKey {
		snap.InoByKey[k] = v
	}
	a.mu.Unlock()
	data, err := json.Marshal(&snap)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[ino-state] marshal: %v\n", err)
		return
	}
	tmp := a.statePath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "[ino-state] write %s: %v\n", tmp, err)
		return
	}
	if err := os.Rename(tmp, a.statePath); err != nil {
		fmt.Fprintf(os.Stderr, "[ino-state] rename %s: %v\n", tmp, err)
	}
}

// Flush writes the ino table to disk if the dirty flag is set. Called
// once per sync round so that a burst of assignIno calls results in
// exactly one disk write instead of N (V-02).
func (a *Allocator) Flush() {
	if a.dirty.CompareAndSwap(1, 0) {
		a.Save()
	}
}

// trimDirPrefix strips the "dir:" tag assignIno applies to synthetic
// directory keys so callers of pathByIno see the real S3-side path.
func trimDirPrefix(s3Key string) string {
	if len(s3Key) > 4 && s3Key[:4] == "dir:" {
		return s3Key[4:]
	}
	return s3Key
}
