package kmodio

import (
	"testing"
	"unsafe"
)

// The expected sizes and numbers below were produced by compiling
// include/s3fs_ebpf_uapi.h with gcc (sizeof(struct ...) and the S3FS_IOC_*
// macros).  If the header changes, regenerate them; a mismatch here means the
// daemon would send the kmod an ioctl it does not recognise.

func TestStructSizesMatchUAPI(t *testing.T) {
	for _, tt := range []struct {
		name string
		got  uintptr
		want uintptr
	}{
		{"s3fs_signal_admit", unsafe.Sizeof(SignalAdmitArg{}), 24},
		{"s3fs_sync_entry", unsafe.Sizeof(SyncEntry{}), 288},
		{"s3fs_sync_entries", unsafe.Sizeof(SyncEntriesArg{}), 24},
		{"s3fs_flush_event", unsafe.Sizeof(FlushEvent{}), 296},
		{"s3fs_read_dirty", unsafe.Sizeof(ReadDirtyArg{}), 32},
		{"s3fs_mark_dirty", unsafe.Sizeof(MarkDirtyArg{}), 8},
		{"s3fs_stats", unsafe.Sizeof(StatsArg{}), 136},
		{"s3fs_delete_event", unsafe.Sizeof(DeleteEvent{}), 288},
		{"s3fs_rename_event", unsafe.Sizeof(RenameEvent{}), 552},
		{"s3fs_populate_pages", unsafe.Sizeof(PopulatePagesArg{}), 32},
		{"s3fs_dirty_layout", unsafe.Sizeof(DirtyLayoutArg{}), 40},
		{"s3fs_read_dirty_range", unsafe.Sizeof(ReadDirtyRangeArg{}), 40},
	} {
		if tt.got != tt.want {
			t.Errorf("sizeof(%s) = %d, want %d", tt.name, tt.got, tt.want)
		}
	}
}

func TestIoctlNumbersMatchUAPI(t *testing.T) {
	for _, tt := range []struct {
		name string
		got  uint32
		want uint32
	}{
		{"EVAL", IocEval, 0xe302},
		{"SIGNAL_ADMIT", IocSignalAdmit, 0x4018e303},
		{"SYNC_ENTRIES", IocSyncEntries, 0x4018e304},
		{"POP_FLUSH", IocPopFlush, 0x8128e305},
		{"READ_DIRTY", IocReadDirty, 0xc020e306},
		{"MARK_DIRTY", IocMarkDirty, 0x4008e307},
		{"STATS", IocStats, 0xc088e308},
		{"SIGNAL_FLUSH", IocSignalFlush, 0x4018e30a},
		{"POP_DELETE", IocPopDelete, 0x8120e30b},
		{"POP_RENAME", IocPopRename, 0x8228e30c},
		{"POP_FLUSH_WAIT", IocPopFlushWait, 0x8128e30d},
		{"POPULATE_PAGES", IocPopulatePages, 0x4020e30e},
		{"DIRTY_LAYOUT", IocDirtyLayout, 0xc028e30f},
		{"READ_DIRTY_RANGE", IocReadDirtyRange, 0xc028e310},
	} {
		if tt.got != tt.want {
			t.Errorf("S3FS_IOC_%s = %#x, want %#x", tt.name, tt.got, tt.want)
		}
	}
}
