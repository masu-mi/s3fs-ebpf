package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"sync"
	"time"
	"unsafe"

	fpebpf "github.com/bbsakura/s3fs-ebpf/daemon/internal/ebpf"
	"github.com/bbsakura/s3fs-ebpf/daemon/internal/metrics"
	"github.com/bbsakura/s3fs-ebpf/daemon/internal/objstore"
	"golang.org/x/sys/unix"
)

// syncNameMax mirrors S3FS_SYNC_NAME_MAX in include/s3fs_ebpf_uapi.h.
const syncNameMax = 255

// syncEntry mirrors `struct s3fs_sync_entry` (G-01 layout: parent_ino added).
type syncEntry struct {
	Ino       uint64
	ParentIno uint64 // G-01: 0 == root == S3FS_ROOT_INO
	Size      uint64
	Mode      uint32
	NameLen   uint32
	Name      [syncNameMax + 1]byte
}

// syncEntriesArg mirrors `struct s3fs_sync_entries`.
type syncEntriesArg struct {
	Count   uint32
	_pad    uint32
	Entries uint64
}

// s3fsIocSyncEntries encodes _IOW(0xE3, 0x04, struct s3fs_sync_entries).
const s3fsIocSyncEntries = (1 << 30) | (uint32(unsafe.Sizeof(syncEntriesArg{})) << 16) | (0xE3 << 8) | 0x04

// syncEntriesIoctl pushes `entries` to kmod via S3FS_IOC_SYNC_ENTRIES.
// Passing an empty slice tells kmod to restore its bootstrap table.
func syncEntriesIoctl(devPath string, entries []syncEntry) error {
	f, err := os.OpenFile(devPath, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("open %s: %w", devPath, err)
	}
	defer f.Close()

	arg := syncEntriesArg{Count: uint32(len(entries))}
	if len(entries) > 0 {
		arg.Entries = uint64(uintptr(unsafe.Pointer(&entries[0])))
	}
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL,
		f.Fd(), uintptr(s3fsIocSyncEntries),
		uintptr(unsafe.Pointer(&arg))); errno != 0 {
		return fmt.Errorf("ioctl SYNC_ENTRIES: %w", errno)
	}
	return nil
}

// -- ino allocator: process-local, first-seen order -----------------------

const (
	inoRootReserved = 100 // ino 1 is the mount root, keep 100+ for entries
)

var (
	inoMu     sync.Mutex
	inoByKey  = map[string]uint64{} // S3Key → ino, populated first-seen
	nextIno   = uint64(inoRootReserved)
	seededIno bool

	// G-01: reverse map so write-path handlers can reconstruct the
	// full S3 key from a flush/delete/rename event's parent_ino +
	// basename. Populated alongside assignIno / registerIno.
	// Convention: pathByIno[dirIno] = "sub1/nested" (no trailing '/'),
	// pathByIno[fileIno] = "sub1/nested/foo.txt". Root is not stored
	// (empty prefix). Synthetic dir keys use the "dir:" allocator
	// prefix in inoByKey; pathByIno drops the "dir:" so callers get
	// the actual S3 path.
	pathByIno = map[uint64]string{}

	// H-02: persistence for the S3Key → ino mapping. When
	// inoStatePath is non-empty, assignIno / registerIno flush the
	// full map atomically after every mutation so an application
	// caching (pid, ino) across daemon restarts still resolves the
	// same file. Empty path disables persistence (default — matches
	// legacy behavior).
	inoStatePath string
)

// seedInoAllocator maps bootstrap S3 keys to their well-known inos so
// that a fresh sync against a bucket that happens to contain those
// keys stays compatible with existing docs / kmod bootstrap.
func seedInoAllocator() {
	inoMu.Lock()
	defer inoMu.Unlock()
	if seededIno {
		return
	}
	for _, e := range fpebpf.Bootstrap() {
		inoByKey[e.S3Key] = e.Ino
		if e.Ino >= nextIno {
			nextIno = e.Ino + 1
		}
	}
	seededIno = true
}

// assignIno returns the ino for `s3Key`, allocating a new one on
// first sight. Deterministic within a daemon process; when H-02
// persistence is enabled (SetInoStatePath), also stable across
// restarts. G-01: pathByIno picks up the reverse mapping, stripping
// the "dir:" prefix so synthetic dir entries surface their real
// S3-side path.
func assignIno(s3Key string) uint64 {
	inoMu.Lock()
	if ino, ok := inoByKey[s3Key]; ok {
		inoMu.Unlock()
		return ino
	}
	ino := nextIno
	nextIno++
	inoByKey[s3Key] = ino
	pathByIno[ino] = trimDirPrefix(s3Key)
	inoMu.Unlock()
	saveInoState()
	return ino
}

// registerIno pins a specific `ino` to `s3Key`, overriding whatever
// the allocator would have picked. Called by the flush loop after a
// PUT for a locally-created file (kmod-picked ino) so the next sync
// round emits the same ino and iterate_shared unifies the two views.
func registerIno(s3Key string, ino uint64) {
	inoMu.Lock()
	prev, hadPrev := inoByKey[s3Key]
	inoByKey[s3Key] = ino
	pathByIno[ino] = trimDirPrefix(s3Key)
	if ino >= nextIno {
		nextIno = ino + 1
	}
	inoMu.Unlock()
	if !hadPrev || prev != ino {
		saveInoState()
	}
}

// trimDirPrefix strips the "dir:" tag assignIno applies to
// synthetic directory keys so callers of pathByIno see the real
// S3-side path.
func trimDirPrefix(s3Key string) string {
	if len(s3Key) > 4 && s3Key[:4] == "dir:" {
		return s3Key[4:]
	}
	return s3Key
}

// resolveKeyPath rebuilds a full S3 key from a parent ino + basename.
// Returns basename alone when parentIno resolves to root (or is 0).
// Used by flush / delete / rename handlers to translate kmod-side
// events into S3 keys.
func resolveKeyPath(parentIno uint64, name string) string {
	if parentIno == 0 || parentIno == kmodRootIno {
		return name
	}
	inoMu.Lock()
	prefix, ok := pathByIno[parentIno]
	inoMu.Unlock()
	if !ok || prefix == "" {
		return name
	}
	return prefix + "/" + name
}

// SetInoStatePath enables H-02 persistence. If `p` names an existing
// file it is loaded into the ino table (inoByKey merges, nextIno
// advances past any observed ino) before the sync loop starts.
// Subsequent mutations flush the whole table atomically via tmp+rename.
// Called from main.go once during startup; passing "" is a no-op and
// leaves the allocator in-memory only.
func SetInoStatePath(p string) error {
	inoStatePath = p
	if p == "" {
		return nil
	}
	dir := filepath.Dir(p)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("ino state mkdir %s: %w", dir, err)
	}
	return loadInoState()
}

type inoStateFile struct {
	InoByKey map[string]uint64 `json:"ino_by_key"`
}

// loadInoState reads inoStatePath (best-effort — missing file is not
// an error, it's the first-run case) and merges its contents into the
// live allocator. Advances nextIno past any observed ino.
func loadInoState() error {
	data, err := os.ReadFile(inoStatePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("ino state read %s: %w", inoStatePath, err)
	}
	var st inoStateFile
	if err := json.Unmarshal(data, &st); err != nil {
		return fmt.Errorf("ino state parse %s: %w", inoStatePath, err)
	}
	inoMu.Lock()
	defer inoMu.Unlock()
	for k, v := range st.InoByKey {
		inoByKey[k] = v
		pathByIno[v] = trimDirPrefix(k) // G-01: rehydrate reverse map too
		if v >= nextIno {
			nextIno = v + 1
		}
	}
	return nil
}

// saveInoState writes inoStatePath atomically (tmp + rename) after
// every ino table mutation. No-op when persistence is disabled.
// Failure is logged and swallowed — a lost persist round beats a
// crashing daemon.
func saveInoState() {
	if inoStatePath == "" {
		return
	}
	inoMu.Lock()
	snap := inoStateFile{InoByKey: make(map[string]uint64, len(inoByKey))}
	for k, v := range inoByKey {
		snap.InoByKey[k] = v
	}
	inoMu.Unlock()
	data, err := json.Marshal(&snap)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[ino-state] marshal: %v\n", err)
		return
	}
	tmp := inoStatePath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "[ino-state] write %s: %v\n", tmp, err)
		return
	}
	if err := os.Rename(tmp, inoStatePath); err != nil {
		fmt.Fprintf(os.Stderr, "[ino-state] rename %s: %v\n", tmp, err)
	}
}

// -- sync loop --------------------------------------------------------

// buildEntriesFromS3 lists the bucket and turns each object into an
// Entry. G-01: S3 keys with "/" separators are split into a chain of
// synthetic directory entries plus the leaf file, so `ls
// /mnt/s3demo/sub1` walks only into the sub1/ prefix. F-02 also
// falls out: two S3 keys with the same basename but different
// prefixes get distinct (parent_ino, name) tuples and can coexist.
//
// Placeholder keys (trailing '/') are still filtered — they only
// serve as directory markers on the S3 side and their names collide
// with our synthesized dir entries.
func buildEntriesFromS3(ctx context.Context, s3c objstore.Backend) ([]fpebpf.Entry, error) {
	objs, err := s3c.List(ctx, "")
	if err != nil {
		return nil, err
	}
	out := make([]fpebpf.Entry, 0, len(objs))
	// dirInoByPath: prefix ("sub1/", "sub1/nested/") → assigned ino
	// for the synthetic dir entry. Populated on first sighting so
	// two files under the same dir share the same parent ino.
	dirInoByPath := map[string]uint64{"": kmodRootIno}
	for _, o := range objs {
		if len(o.Key) == 0 || o.Key[len(o.Key)-1] == '/' {
			continue
		}
		leafName := path.Base(o.Key)
		if len(leafName) == 0 || len(leafName) > syncNameMax {
			fmt.Fprintf(os.Stderr, "[sync] skip %q: invalid basename\n", o.Key)
			continue
		}
		// Walk the path prefix, minting a dir entry per component.
		parent := kmodRootIno
		prefix := ""
		dir := path.Dir(o.Key)
		if dir != "." && dir != "/" && dir != "" {
			components := splitPath(dir)
			for _, comp := range components {
				if len(comp) == 0 || len(comp) > syncNameMax {
					fmt.Fprintf(os.Stderr, "[sync] skip %q: bad component %q\n",
						o.Key, comp)
					parent = 0
					break
				}
				if prefix == "" {
					prefix = comp
				} else {
					prefix = prefix + "/" + comp
				}
				dino, seen := dirInoByPath[prefix]
				if !seen {
					dino = assignIno("dir:" + prefix)
					dirInoByPath[prefix] = dino
					out = append(out, fpebpf.Entry{
						Ino:       dino,
						ParentIno: parent,
						Name:      comp,
						S3Key:     "", // synthetic — no backing object
						Mode:      0o040755, // S_IFDIR | 0755
						Fallback:  0,
						Prewarm:   false, // dirs have no content to admit
					})
				}
				parent = dino
			}
			if parent == 0 {
				continue // skipped due to bad component
			}
		}
		out = append(out, fpebpf.Entry{
			Ino:       assignIno(o.Key),
			ParentIno: parent,
			Name:      leafName,
			S3Key:     o.Key,
			Mode:      0o100644, // S_IFREG | 0644 (kmod fills S_IFREG bit)
			Fallback:  uint64(o.Size),
			Prewarm:   true, // Option 1: prewarm-all, policy engine overrides later
		})
	}
	// Sort by ino so ioctl output is stable across List() runs.
	sort.Slice(out, func(i, j int) bool { return out[i].Ino < out[j].Ino })
	return out, nil
}

// kmodRootIno mirrors S3FS_ROOT_INO in kmod/vfs_ops.h. Kept private
// to this file so the daemon writes it explicitly rather than
// relying on the "parent_ino==0 → root" fallback (which stays as a
// safety net in kmod but is a code smell if the daemon depends on it).
const kmodRootIno = uint64(1)

// splitPath breaks a POSIX-style path into non-empty components.
// path.Split only peels off the last element; we want every segment.
// A leading '/' or repeated separators are silently absorbed.
func splitPath(p string) []string {
	out := make([]string, 0, 4)
	start := 0
	for i := 0; i <= len(p); i++ {
		if i == len(p) || p[i] == '/' {
			if i > start {
				out = append(out, p[start:i])
			}
			start = i + 1
		}
	}
	return out
}

// entriesToSyncPayload marshals Go-side Entries into the UAPI struct
// array kmod expects. G-01: directory entries are passed through with
// their S_IFDIR mode bit intact; file entries have S_IFREG (0100000)
// stamped on to defend against a caller that only supplied the perm
// bits.
func entriesToSyncPayload(es []fpebpf.Entry) []syncEntry {
	out := make([]syncEntry, 0, len(es))
	for _, e := range es {
		nlen := len(e.Name)
		if nlen == 0 || nlen > syncNameMax {
			fmt.Fprintf(os.Stderr, "[sync] skip ino=%d name=%q: bad length\n",
				e.Ino, e.Name)
			continue
		}
		mode := e.Mode
		if mode&fileTypeMask == 0 {
			mode |= 0o100000 // default to S_IFREG when caller left type bits blank
		}
		se := syncEntry{
			Ino:       e.Ino,
			ParentIno: e.ParentIno,
			Size:      e.Fallback, // Fallback holds the S3-stat size in Week 4(b)
			Mode:      mode,
			NameLen:   uint32(nlen),
		}
		copy(se.Name[:], e.Name)
		out = append(out, se)
	}
	return out
}

// fileTypeMask is the S_IFMT bits (0170000) — file-type portion of a
// POSIX mode. Distinguishes S_IFDIR (0040000) from S_IFREG (0100000).
const fileTypeMask uint32 = 0o170000

// syncEntriesOnce runs one sync round: List → assign inos → push to
// kmod → update daemon mirror. Returns the fresh entry set on
// success. syncEntries is a no-op when s3c is nil (offline mode uses
// the bootstrap mirror kmod already has).
func syncEntriesOnce(ctx context.Context, devPath string, s3c objstore.Backend) ([]fpebpf.Entry, error) {
	if s3c == nil {
		return fpebpf.Snapshot(), nil
	}
	seedInoAllocator()
	timedCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	fresh, err := buildEntriesFromS3(timedCtx, s3c)
	if err != nil {
		metrics.Global.SyncErrors.Add(1)
		return nil, fmt.Errorf("list: %w", err)
	}
	payload := entriesToSyncPayload(fresh)
	if err := syncEntriesIoctl(devPath, payload); err != nil {
		metrics.Global.SyncErrors.Add(1)
		return nil, err
	}
	fpebpf.Replace(fresh)
	metrics.Global.SyncRounds.Add(1)
	metrics.Global.SyncEntriesCurrent.Store(uint64(len(fresh)))
	return fresh, nil
}

// runSyncLoop invokes syncEntriesOnce on `interval` until ctx is
// done. A zero interval disables periodic sync (startup-only mode).
func runSyncLoop(ctx context.Context, devPath string, s3c objstore.Backend,
	interval time.Duration, onSync func([]fpebpf.Entry)) {
	if interval <= 0 {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			fresh, err := syncEntriesOnce(ctx, devPath, s3c)
			if err != nil {
				fmt.Fprintf(os.Stderr, "[sync] %v\n", err)
				continue
			}
			if onSync != nil {
				onSync(fresh)
			}
		}
	}
}
