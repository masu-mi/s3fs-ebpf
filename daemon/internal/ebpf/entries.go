package ebpf

// Entry mirrors one row of kmod/vfs_ops.c's `s3fs_entries[]` — the
// compile-time table of objects that the mount point exposes. The
// daemon keeps its own copy so it can prewarm the arena without a
// runtime channel back to the module. Update in lockstep with the
// C-side table until Week 4+ adds a bucket-sync ioctl.
type Entry struct {
	Ino      uint64 // matches kmod's assigned inode number
	Name     string // matches kmod's dentry name
	S3Key    string // maps the local dentry to an S3 object key
	Mode     uint32 // file mode as passed to inode->i_mode
	Fallback uint64 // size in bytes to use when S3 stat is unavailable
}

// Entries is the daemon's canonical table. It mirrors kmod's
// s3fs_entries[] and adds a `S3Key` field so the daemon knows which
// bucket object each ino maps to. Ordering matches kmod's for
// clarity (not for correctness — lookup is by ino, not slot index).
var Entries = []Entry{
	{Ino: 100, Name: "hello.txt", S3Key: "hello.txt",
		Mode: 0o100644, Fallback: 17},
	{Ino: 102, Name: "models_weights.bin", S3Key: "models/weights.bin",
		Mode: 0o100644, Fallback: 23},
	{Ino: 101, Name: "world.txt", S3Key: "world.txt",
		Mode: 0o100644, Fallback: 13},
}
