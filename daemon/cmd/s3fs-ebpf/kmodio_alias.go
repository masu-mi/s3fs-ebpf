package main

import "github.com/bbsakura/s3fs-ebpf/daemon/internal/kmodio"

// The wire types live in internal/kmodio; these aliases keep the package-local names.
type (
	populatePagesArg  = kmodio.PopulatePagesArg
	signalAdmitArg    = kmodio.SignalAdmitArg
	flushEvent        = kmodio.FlushEvent
	readDirtyArg      = kmodio.ReadDirtyArg
	markDirtyArg      = kmodio.MarkDirtyArg
	statsArg          = kmodio.StatsArg
	reqEvent          = kmodio.ReqEvent
	deleteEvent       = kmodio.DeleteEvent
	renameEvent       = kmodio.RenameEvent
	dirtyLayoutArg    = kmodio.DirtyLayoutArg
	readDirtyRangeArg = kmodio.ReadDirtyRangeArg
	syncEntry         = kmodio.SyncEntry
	syncEntriesArg    = kmodio.SyncEntriesArg
)

const (
	s3fsIocPopulatePages  = kmodio.IocPopulatePages
	s3fsIocEval           = kmodio.IocEval
	s3fsIocSignalAdmit    = kmodio.IocSignalAdmit
	s3fsIocReadDirty      = kmodio.IocReadDirty
	s3fsIocMarkDirty      = kmodio.IocMarkDirty
	s3fsIocStats          = kmodio.IocStats
	s3fsIocSignalFlush    = kmodio.IocSignalFlush
	s3fsIocDirtyLayout    = kmodio.IocDirtyLayout
	s3fsIocReadDirtyRange = kmodio.IocReadDirtyRange
	s3fsIocSyncEntries    = kmodio.IocSyncEntries
	syncNameMax           = kmodio.SyncNameMax
	statsMax              = kmodio.StatsMax
)
