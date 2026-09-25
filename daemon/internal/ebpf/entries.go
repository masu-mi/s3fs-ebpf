package ebpf

import "sync"

// Entry mirrors one row of kmod's live entry table — the set of
// objects the mount currently exposes. Sync is one-way: daemon
// derives Entries from an S3 List() and pushes the snapshot to kmod
// via S3FS_IOC_SYNC_ENTRIES. Older builds kept two hand-written
// copies (Go + C) that had to be edited in lockstep; kmod still ships
// a bootstrap table for the daemon-not-yet-connected case.
type Entry struct {
	Ino      uint64 // daemon-assigned, in-memory persistence only
	Name     string // dentry name; must equal path.Base(S3Key)
	S3Key    string // S3 object key backing this entry
	Mode     uint32 // file mode as passed to inode->i_mode
	Fallback uint64 // size in bytes when S3 stat is unavailable
	Prewarm  bool   // if false, wait for a read-miss upcall before fetching
}

// bootstrap mirrors kmod's compile-time s3fs_bootstrap_entries so the
// daemon can operate (and prewarm) without S3 — useful for smoke
// tests against a stubbed backend and as the starting mirror before
// the first Sync round.
var bootstrap = []Entry{
	{Ino: 100, Name: "hello.txt", S3Key: "hello.txt",
		Mode: 0o100644, Fallback: 17, Prewarm: true},
	{Ino: 102, Name: "models_weights.bin", S3Key: "models/weights.bin",
		Mode: 0o100644, Fallback: 23, Prewarm: true},
	{Ino: 101, Name: "world.txt", S3Key: "world.txt",
		Mode: 0o100644, Fallback: 13, Prewarm: true},
	{Ino: 103, Name: "lazy.txt", S3Key: "lazy.txt",
		Mode: 0o100644, Fallback: 18, Prewarm: false},
	{Ino: 104, Name: "big.bin", S3Key: "big.bin",
		Mode: 0o100644, Fallback: 45, Prewarm: true},
}

var (
	entriesMu sync.RWMutex
	entries   = append([]Entry(nil), bootstrap...) // start with bootstrap so
	                                               // startup smoke works even
	                                               // without a sync round
)

// Bootstrap returns a copy of the compile-time entry table.
func Bootstrap() []Entry {
	out := make([]Entry, len(bootstrap))
	copy(out, bootstrap)
	return out
}

// Snapshot returns a copy of the current live table, safe to iterate
// without holding the lock.
func Snapshot() []Entry {
	entriesMu.RLock()
	defer entriesMu.RUnlock()
	out := make([]Entry, len(entries))
	copy(out, entries)
	return out
}

// Replace atomically swaps the live table. Callers own `newEntries`;
// this function keeps its own copy.
func Replace(newEntries []Entry) {
	dup := make([]Entry, len(newEntries))
	copy(dup, newEntries)
	entriesMu.Lock()
	entries = dup
	entriesMu.Unlock()
}

// LookupByIno returns a copy of the entry with the matching ino, or
// nil. Copy semantics avoid handing out pointers into the live slice.
func LookupByIno(ino uint64) *Entry {
	entriesMu.RLock()
	defer entriesMu.RUnlock()
	for i := range entries {
		if entries[i].Ino == ino {
			e := entries[i]
			return &e
		}
	}
	return nil
}
