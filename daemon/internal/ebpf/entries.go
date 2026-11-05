package ebpf

import "sync"

// Entry mirrors one row of kmod's live entry table — the set of
// objects the mount currently exposes. Sync is one-way: daemon
// derives Entries from an S3 List() and pushes the snapshot to kmod
// via S3FS_IOC_SYNC_ENTRIES. Older builds kept two hand-written
// copies (Go + C) that had to be edited in lockstep; kmod still ships
// a bootstrap table for the daemon-not-yet-connected case.
type Entry struct {
	Ino       uint64 // daemon-assigned, in-memory persistence only
	ParentIno uint64 // G-01: 0 == root == kmod S3FS_ROOT_INO(=1). Directories carry their own ino.
	Name      string // basename only; parent path is reconstructed by walking ParentIno.
	S3Key     string // full S3 object key (with any subdir prefix). Empty for synthetic directories.
	Mode      uint32 // file mode as passed to inode->i_mode. S_IFDIR bit distinguishes subdirs.
	Fallback  uint64 // size in bytes when S3 stat is unavailable
	Prewarm   bool   // if false, wait for a read-miss upcall before fetching
	RecSeq    uint64 // AC-86/85: seq of the flush / rename event RecordKey recorded this key for (0 = from a listing)
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

// SetSizeByIno records the size of ino after the daemon itself wrote the
// object (a flush may grow or shrink it between two syncs).
func SetSizeByIno(ino, size uint64) {
	entriesMu.Lock()
	defer entriesMu.Unlock()
	for i := range entries {
		if entries[i].Ino == ino {
			entries[i].Fallback = size
			return
		}
	}
}

// RecordKey makes the mirror agree with a key the daemon just wrote or moved:
// a flush of a file created since the last sync, or a rename. The mirror is
// otherwise only refreshed by a sync, so until then a read miss (which looks
// the ino up here) found no entry, or the key from before a rename (AC-86).
// An ino the mirror does not know becomes a regular-file entry with the given
// parent, name, key and size; a known one keeps its mode and flags and gets the
// new parent, name and key. `seq` is the kmod event it comes from: ReplaceApplied
// keeps the recorded values while a listing may predate that event.
func RecordKey(ino, parentIno uint64, name, key string, size, seq uint64) {
	entriesMu.Lock()
	defer entriesMu.Unlock()
	for i := range entries {
		if entries[i].Ino == ino {
			entries[i].ParentIno = parentIno
			entries[i].Name = name
			entries[i].S3Key = key
			if seq > entries[i].RecSeq {
				entries[i].RecSeq = seq
			}
			return
		}
	}
	entries = append(entries, Entry{Ino: ino, ParentIno: parentIno, Name: name,
		S3Key: key, Mode: 0o100644, Fallback: size, RecSeq: seq})
}

// DropRecorded forgets the entries RecordKey made for ino (the file was
// deleted): a later listing will not have them, and ReplaceApplied would keep
// them until the daemon restarts.
func DropRecorded(ino uint64) {
	entriesMu.Lock()
	defer entriesMu.Unlock()
	out := entries[:0]
	for _, e := range entries {
		if e.Ino == ino && e.RecSeq > 0 {
			continue
		}
		out = append(out, e)
	}
	entries = out
}

// ReplaceApplied swaps in a fresh listing like Replace, except for entries that
// RecordKey recorded for an event newer than `applied` (every event with
// seq <= applied was in S3 before the listing was taken): the listing may
// predate those, so their recorded parent / name / key win, and a recorded file
// the listing does not have yet stays. Without this a sync that overlaps a
// flush or rename throws the new key away (open_check then fails ENOENT on a
// file that exists, a read miss GETs the old key).
func ReplaceApplied(fresh []Entry, applied uint64) {
	entriesMu.Lock()
	defer entriesMu.Unlock()
	recorded := map[uint64]Entry{}
	for _, e := range entries {
		if e.RecSeq > applied {
			recorded[e.Ino] = e
		}
	}
	dup := make([]Entry, len(fresh), len(fresh)+len(recorded))
	copy(dup, fresh)
	for i := range dup {
		if r, ok := recorded[dup[i].Ino]; ok {
			dup[i].ParentIno, dup[i].Name, dup[i].S3Key, dup[i].RecSeq = r.ParentIno, r.Name, r.S3Key, r.RecSeq
			delete(recorded, dup[i].Ino)
		}
	}
	for _, r := range recorded {
		dup = append(dup, r)
	}
	entries = dup
}
