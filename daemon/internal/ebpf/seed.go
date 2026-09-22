package ebpf

// SeedEntry mirrors kmod/vfs_ops.c's `s3fs_entries[]` — the compile-time
// list of "objects" that the mount point exposes. Update in lockstep
// with the C-side table (there's no better source of truth until the
// module grows a bucket-sync ioctl in Week 4+).
type SeedEntry struct {
	Ino     uint64 // matches kmod's assigned inode number
	Name    string // matches kmod's dentry name
	S3Key   string // maps the local dentry to an S3 object key
	Mode    uint32 // file mode as passed to inode->i_mode
	Fallback uint64 // seed size in bytes; used when S3 stat is unavailable
}

// Seeds is the daemon's canonical list. It mirrors kmod's
// s3fs_entries[] table and adds a `S3Key` field so the daemon knows
// which bucket object each ino maps to. Ordering matches kmod's for
// clarity (not for correctness — lookup is by ino, not slot index).
var Seeds = []SeedEntry{
	{Ino: 100, Name: "hello.txt", S3Key: "hello.txt",
		Mode: 0o100644, Fallback: 17},
	{Ino: 102, Name: "models_weights.bin", S3Key: "models/weights.bin",
		Mode: 0o100644, Fallback: 23},
	{Ino: 101, Name: "world.txt", S3Key: "world.txt",
		Mode: 0o100644, Fallback: 13},
}
