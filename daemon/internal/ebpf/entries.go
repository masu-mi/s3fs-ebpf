package ebpf

// Entry mirrors one row of kmod/vfs_ops.c's `s3fs_entries[]` — the
// compile-time table of objects that the mount point exposes. The
// daemon keeps its own copy so it can prewarm the arena without a
// runtime channel back to the module. Update in lockstep with the
// C-side table until Week 4+ adds a bucket-sync ioctl.
//
// Entries with Prewarm=false are known to the daemon (so it can
// resolve an ino → S3Key when a read-miss upcall arrives) but not
// admitted at startup — exercising the Week 4 (a) C on-demand fetch
// path.
type Entry struct {
	Ino      uint64 // matches kmod's assigned inode number
	Name     string // matches kmod's dentry name
	S3Key    string // maps the local dentry to an S3 object key
	Mode     uint32 // file mode as passed to inode->i_mode
	Fallback uint64 // size in bytes to use when S3 stat is unavailable
	Prewarm  bool   // if false, wait for a read-miss upcall before fetching
}

// Entries is the daemon's canonical table. It mirrors kmod's
// s3fs_entries[] and adds a `S3Key` field so the daemon knows which
// bucket object each ino maps to. Ordering matches kmod's for
// clarity (not for correctness — lookup is by ino, not slot index).
var Entries = []Entry{
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

// LookupByIno returns the entry with the matching ino, or nil.
func LookupByIno(ino uint64) *Entry {
	for i := range Entries {
		if Entries[i].Ino == ino {
			return &Entries[i]
		}
	}
	return nil
}
