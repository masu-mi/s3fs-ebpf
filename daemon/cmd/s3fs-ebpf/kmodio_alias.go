package main

import "github.com/bbsakura/s3fs-ebpf/daemon/internal/kmodio"

// The wire types live in internal/kmodio; these aliases keep the package-local names.
type (
	admitEntry        = kmodio.AdmitEntry
	populatePagesArg  = kmodio.PopulatePagesArg
	admitArenaArg     = kmodio.AdmitArenaArg
	signalAdmitArg    = kmodio.SignalAdmitArg
	flushEvent        = kmodio.FlushEvent
	readDirtyArg      = kmodio.ReadDirtyArg
	markDirtyArg      = kmodio.MarkDirtyArg
	statsArg          = kmodio.StatsArg
	drainEvictionsArg = kmodio.DrainEvictionsArg
	deleteEvent       = kmodio.DeleteEvent
	renameEvent       = kmodio.RenameEvent
	dirtyLayoutArg    = kmodio.DirtyLayoutArg
	readDirtyRangeArg = kmodio.ReadDirtyRangeArg
	syncEntry         = kmodio.SyncEntry
	syncEntriesArg    = kmodio.SyncEntriesArg
	probeKvbaseArg    = kmodio.ProbeKvbaseArg
)

const (
	s3fsIocPopulatePages  = kmodio.IocPopulatePages
	s3fsIocAdmitArena     = kmodio.IocAdmitArena
	s3fsIocSignalAdmit    = kmodio.IocSignalAdmit
	s3fsIocPopFlush       = kmodio.IocPopFlush
	s3fsIocReadDirty      = kmodio.IocReadDirty
	s3fsIocMarkDirty      = kmodio.IocMarkDirty
	s3fsIocStats          = kmodio.IocStats
	s3fsIocDrainEvictions = kmodio.IocDrainEvictions
	s3fsIocSignalFlush    = kmodio.IocSignalFlush
	s3fsIocPopFlushWait   = kmodio.IocPopFlushWait
	s3fsIocPopDelete      = kmodio.IocPopDelete
	s3fsIocPopRename      = kmodio.IocPopRename
	s3fsIocDirtyLayout    = kmodio.IocDirtyLayout
	s3fsIocReadDirtyRange = kmodio.IocReadDirtyRange
	s3fsIocSyncEntries    = kmodio.IocSyncEntries
	s3fsIocProbeKvbase    = kmodio.IocProbeKvbase
	syncNameMax           = kmodio.SyncNameMax
	statsMax              = kmodio.StatsMax
	evictLogMax           = kmodio.EvictLogMax
)
