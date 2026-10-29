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
	// EvictLogMax is the capacity of one S3FS_IOC_DRAIN_EVICTIONS reply.
	EvictLogMax = 256
)

// AdmitEntry mirrors `struct s3fs_admit_entry` in
// include/s3fs_ebpf_uapi.h. `Uaddr` is the daemon-side user_vm
// address of the range's bytes. `Offset` is the file offset where
// the range starts (D-01); Offset=0 & Size=full_file is the
// whole-file admit (backward-compat default).
type AdmitEntry struct {
	Ino    uint64
	Uaddr  uint64
	Size   uint64
	Offset uint64
}

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

// AdmitArenaArg mirrors `struct s3fs_admit_arena`.
type AdmitArenaArg struct {
	Count   uint32
	_pad    uint32
	Entries uint64
}

// SignalAdmitArg mirrors `struct s3fs_signal_admit` (U13: added ChunkOffset).
type SignalAdmitArg struct {
	Ino         uint64
	ChunkOffset uint64
	Err         int32
	Flags       uint32
}

// FlushEvent mirrors `struct s3fs_flush_event` (G-01 layout).
// Flags carry S3FS_FLUSH_FLAG_* — currently only FLUSH_WAIT (fsync
// path) which tells the coalescer to skip debounce.
type FlushEvent struct {
	Ino       uint64
	ParentIno uint64 // G-01: dir the file lives under, for full-key reconstruction
	Size      uint64
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

// MarkDirtyArg mirrors `struct s3fs_mark_dirty`.
type MarkDirtyArg struct {
	Ino uint64
}

type StatsArg struct {
	Count  uint32
	Filled uint32
	Values [StatsMax]uint64
}

type DrainEvictionsArg struct {
	Cap      uint32
	Filled   uint32
	Overflow uint32
	_pad     uint32
	Inos     [EvictLogMax]uint64
}

// DeleteEvent mirrors `struct s3fs_delete_event` (G-01 layout).
// Kept as a separate type from FlushEvent so the ioctl size encoding
// matches kmod's expected sizeof exactly.
type DeleteEvent struct {
	Ino       uint64
	ParentIno uint64 // G-01
	NameLen   uint32
	_pad      uint32
	Name      [SyncNameMax + 1]byte
}

// RenameEvent mirrors `struct s3fs_rename_event` (G-01 layout).
// Carries both names + their parent inos in one payload so daemon
// resolves full S3 keys for a cross-dir CopyObject + Delete.
type RenameEvent struct {
	Ino          uint64
	OldParentIno uint64 // G-01
	NewParentIno uint64 // G-01
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
	Count   uint32
	_pad    uint32
	Entries uint64
}

// ProbeKvbaseArg mirrors `struct s3fs_probe_kvbase` in
// include/s3fs_ebpf_uapi.h. Layout is fixed by the kmod's ioctl handler.
type ProbeKvbaseArg struct {
	ArenaFD   uint32
	_pad      uint32
	OutKvbase uint64
	OutSize   uint64
}

// ioctl numbers: _IO{R,W,WR}(0xE3, nr, type).
const (
	IocPopulatePages  = (1 << 30) | (uint32(unsafe.Sizeof(PopulatePagesArg{})) << 16) | (0xE3 << 8) | 0x0E
	IocAdmitArena     = (1 << 30) | (uint32(unsafe.Sizeof(AdmitArenaArg{})) << 16) | (0xE3 << 8) | 0x02
	IocSignalAdmit    = (1 << 30) | (uint32(unsafe.Sizeof(SignalAdmitArg{})) << 16) | (0xE3 << 8) | 0x03
	IocPopFlush       = (2 << 30) | (uint32(unsafe.Sizeof(FlushEvent{})) << 16) | (0xE3 << 8) | 0x05
	IocReadDirty      = (3 << 30) | (uint32(unsafe.Sizeof(ReadDirtyArg{})) << 16) | (0xE3 << 8) | 0x06
	IocMarkDirty      = (1 << 30) | (uint32(unsafe.Sizeof(MarkDirtyArg{})) << 16) | (0xE3 << 8) | 0x07
	IocStats          = (3 << 30) | (uint32(unsafe.Sizeof(StatsArg{})) << 16) | (0xE3 << 8) | 0x08
	IocDrainEvictions = (3 << 30) | (uint32(unsafe.Sizeof(DrainEvictionsArg{})) << 16) | (0xE3 << 8) | 0x09
	IocSignalFlush    = (1 << 30) | (uint32(unsafe.Sizeof(SignalAdmitArg{})) << 16) | (0xE3 << 8) | 0x0A
	IocPopFlushWait   = (2 << 30) | (uint32(unsafe.Sizeof(FlushEvent{})) << 16) | (0xE3 << 8) | 0x0D
	IocPopDelete      = (2 << 30) | (uint32(unsafe.Sizeof(DeleteEvent{})) << 16) | (0xE3 << 8) | 0x0B
	IocPopRename      = (2 << 30) | (uint32(unsafe.Sizeof(RenameEvent{})) << 16) | (0xE3 << 8) | 0x0C
	IocDirtyLayout    = (3 << 30) | (uint32(unsafe.Sizeof(DirtyLayoutArg{})) << 16) | (0xE3 << 8) | 0x0F
	IocReadDirtyRange = (3 << 30) | (uint32(unsafe.Sizeof(ReadDirtyRangeArg{})) << 16) | (0xE3 << 8) | 0x10
	IocSyncEntries    = (1 << 30) | (uint32(unsafe.Sizeof(SyncEntriesArg{})) << 16) | (0xE3 << 8) | 0x04
	IocProbeKvbase    = (3 << 30) | (uint32(unsafe.Sizeof(ProbeKvbaseArg{})) << 16) | (0xE3 << 8) | 0x01
)
