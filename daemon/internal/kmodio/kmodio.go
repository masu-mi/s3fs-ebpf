// Package kmodio holds the wire format shared with the s3fs_kfunc kernel
// module: the ioctl argument structs and the ioctl numbers for
// /dev/s3fs_ebpf.  They mirror include/s3fs_ebpf_uapi.h; kmodio_test.go checks
// every size and number against values taken from that header.
package kmodio

import "unsafe"

const (
	// SyncNameMax mirrors S3FS_SYNC_NAME_MAX.
	SyncNameMax = 255
	// StatsMax is the number of counters S3FS_IOC_STATS returns.
	StatsMax = 16
)

// AB-04: PopulatePagesArg mirrors `struct s3fs_populate_pages`.
// The daemon hands S3 data to kmod via this ioctl so kmod can store it
// in fd->buf (kernel memory) and insert a PCACHE-mode admit entry.
// Reads then use s3fs_folio_fill_from_kmalloc (no GUP).
type PopulatePagesArg struct {
	Ino     uint64
	Offset  uint64
	Size    uint64
	DataPtr uint64 // user pointer to data bytes (unsafe.Pointer value)
}

// DoneArg mirrors `struct s3fs_done`: the daemon is done with a request (v2
// Phase 4). Type is ReqMiss (wake the waiters of the chunk record, Seq names the
// request answered, 0 = the plain-read record) or ReqFlush (the PUT of Ino is
// finished; Err != 0 makes kmod mark the file dirty again). Flags: DoneVerifyPopulated.
type DoneArg struct {
	Type        uint32
	Flags       uint32
	Ino         uint64
	ChunkOffset uint64
	Seq         uint64
	Err         int32
	_pad        uint32
}

// FlushEvent is the flush form of a ReqEvent, as the handlers take it.
// Flags carry S3FS_FLUSH_FLAG_* — currently only FLUSH_WAIT (fsync
// path) which tells the coalescer to skip debounce.
type FlushEvent struct {
	Ino       uint64
	ParentIno uint64 // G-01: dir the file lives under, for full-key reconstruction
	Size      uint64
	Seq       uint64 // AC-84: kmod-wide event order (flush / rename / delete)
	NameLen   uint32
	Flags     uint32
	Name      [SyncNameMax + 1]byte
}

// ReadDirtyArg mirrors `struct s3fs_read_dirty`.
type ReadDirtyArg struct {
	Ino      uint64
	Uaddr    uint64
	Size     uint64
	OutBytes uint64
}

type StatsArg struct {
	Count  uint32
	Filled uint32
	Values [StatsMax]uint64
}

// ReqEvent mirrors `struct s3fs_req_event`: what read(2) of /dev/s3fs_ebpf
// returns, one request per read, in the order kmod queued them. Type is
// ReqMiss / ReqFlush / ReqDelete / ReqRename; unused fields are 0.
type ReqEvent struct {
	Type         uint32
	Flags        uint32 // MISS: S3FS_MISS_FLAG_*, FLUSH: FlushFlag*
	Seq          uint64 // flush / delete / rename: kmod-wide order (AC-84); 0 for a miss
	Ino          uint64
	ParentIno    uint64 // rename: source dir
	NewParentIno uint64 // rename: destination dir
	Size         uint64 // flush
	Offset       int64  // miss
	Count        uint32 // miss
	SeqCount     uint32 // miss: sequential reads so far
	RandCount    uint32 // miss: random reads so far
	NameLen      uint32
	NewNameLen   uint32
	_pad         uint32
	Name         [SyncNameMax + 1]byte
	NewName      [SyncNameMax + 1]byte
}

const (
	ReqMiss   = 1
	ReqFlush  = 2
	ReqDelete = 3
	ReqRename = 4
)

// DeleteEvent is the delete form of a ReqEvent, as the handlers take it.
type DeleteEvent struct {
	Ino       uint64
	ParentIno uint64 // G-01
	Seq       uint64 // AC-84
	NameLen   uint32
	_pad      uint32
	Name      [SyncNameMax + 1]byte
}

// RenameEvent is the rename form of a ReqEvent, as the handlers take it.
// Carries both names + their parent inos in one payload so daemon
// resolves full S3 keys for a cross-dir CopyObject + Delete.
type RenameEvent struct {
	Ino          uint64
	OldParentIno uint64 // G-01
	NewParentIno uint64 // G-01
	Seq          uint64 // AC-84
	OldNameLen   uint32
	NewNameLen   uint32
	OldName      [SyncNameMax + 1]byte
	NewName      [SyncNameMax + 1]byte
}

type DirtyLayoutArg struct {
	Ino         uint64
	StatesUaddr uint64
	Cap         uint32
	NrChunks    uint32
	Size        uint64
	ChunkSize   uint64
}

type ReadDirtyRangeArg struct {
	Ino      uint64
	Uaddr    uint64
	Offset   uint64
	Size     uint64
	OutBytes uint64
}

// SyncEntry mirrors `struct s3fs_sync_entry` (G-01 layout: parent_ino added).
type SyncEntry struct {
	Ino       uint64
	ParentIno uint64 // G-01: 0 == root == S3FS_ROOT_INO
	Size      uint64
	Mode      uint32
	NameLen   uint32
	Name      [SyncNameMax + 1]byte
}

// SyncEntriesArg mirrors `struct s3fs_sync_entries`.
type SyncEntriesArg struct {
	Count      uint32
	_pad       uint32
	Entries    uint64
	AppliedSeq uint64 // AC-85: events with seq <= AppliedSeq are applied to S3
}

// ioctl numbers: _IO{R,W,WR}(0xE3, nr, type).
const (
	IocPopulatePages  = (1 << 30) | (uint32(unsafe.Sizeof(PopulatePagesArg{})) << 16) | (0xE3 << 8) | 0x0E
	IocEval           = (0 << 30) | (0 << 16) | (0xE3 << 8) | 0x02 // _IO(0xE3, 0x02)
	IocDone           = (1 << 30) | (uint32(unsafe.Sizeof(DoneArg{})) << 16) | (0xE3 << 8) | 0x12
	IocReadDirty      = (3 << 30) | (uint32(unsafe.Sizeof(ReadDirtyArg{})) << 16) | (0xE3 << 8) | 0x06
	IocStats          = (3 << 30) | (uint32(unsafe.Sizeof(StatsArg{})) << 16) | (0xE3 << 8) | 0x08
	IocDirtyLayout    = (3 << 30) | (uint32(unsafe.Sizeof(DirtyLayoutArg{})) << 16) | (0xE3 << 8) | 0x0F
	IocReadDirtyRange = (3 << 30) | (uint32(unsafe.Sizeof(ReadDirtyRangeArg{})) << 16) | (0xE3 << 8) | 0x10
	IocSyncEntries    = (1 << 30) | (uint32(unsafe.Sizeof(SyncEntriesArg{})) << 16) | (0xE3 << 8) | 0x04
)
