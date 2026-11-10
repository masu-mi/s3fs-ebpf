package main

import "github.com/bbsakura/s3fs-ebpf/daemon/internal/kmodio"

// The wire types live in internal/kmodio; these aliases keep the package-local names.
type (
	populatePagesArg  = kmodio.PopulatePagesArg
	doneArg           = kmodio.DoneArg
	flushEvent        = kmodio.FlushEvent
	readDirtyArg      = kmodio.ReadDirtyArg
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
	s3fsIocDone           = kmodio.IocDone
	s3fsIocReadDirty      = kmodio.IocReadDirty
	s3fsIocStats          = kmodio.IocStats
	s3fsIocDirtyLayout    = kmodio.IocDirtyLayout
	s3fsIocReadDirtyRange = kmodio.IocReadDirtyRange
	s3fsIocSyncEntries    = kmodio.IocSyncEntries
	syncNameMax           = kmodio.SyncNameMax
	statsMax              = kmodio.StatsMax
)
