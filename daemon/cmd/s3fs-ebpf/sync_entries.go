package main

import (
	"context"
	"fmt"
	"os"
	"time"
	"unsafe"

	fpebpf "github.com/bbsakura/s3fs-ebpf/daemon/internal/ebpf"
	"github.com/bbsakura/s3fs-ebpf/daemon/internal/inostate"
	"github.com/bbsakura/s3fs-ebpf/daemon/internal/syncplan"
	"github.com/bbsakura/s3fs-ebpf/daemon/internal/metrics"
	"github.com/bbsakura/s3fs-ebpf/daemon/internal/objstore"
	"golang.org/x/sys/unix"
)

// syncNameMax mirrors S3FS_SYNC_NAME_MAX in include/s3fs_ebpf_uapi.h.

// s3fsIocSyncEntries encodes _IOW(0xE3, 0x04, struct s3fs_sync_entries).

// syncEntriesIoctl pushes `entries` to kmod via S3FS_IOC_SYNC_ENTRIES.
// Passing an empty slice tells kmod to restore its bootstrap table.
func syncEntriesIoctl(devPath string, entries []syncEntry) error {
	f, err := os.OpenFile(devPath, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("open %s: %w", devPath, err)
	}
	defer f.Close()

	arg := syncEntriesArg{Count: uint32(len(entries))}
	if len(entries) > 0 {
		arg.Entries = uint64(uintptr(unsafe.Pointer(&entries[0])))
	}
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL,
		f.Fd(), uintptr(s3fsIocSyncEntries),
		uintptr(unsafe.Pointer(&arg))); errno != 0 {
		return fmt.Errorf("ioctl SYNC_ENTRIES: %w", errno)
	}
	return nil
}

// -- package-level wrappers around the process-wide allocator ---------------

// inoAlloc is the process-wide allocator singleton.
var inoAlloc = inostate.New()

const inoRootReserved = inostate.RootReserved

func seedInoAllocator()                    { inoAlloc.Seed() }
func assignIno(s3Key string) uint64        { return inoAlloc.AssignIno(s3Key) }
func registerIno(s3Key string, ino uint64) { inoAlloc.RegisterIno(s3Key, ino) }
func resolveKeyPath(parentIno uint64, name string) string {
	return inoAlloc.ResolveKeyPath(parentIno, name)
}
func SetInoStatePath(p string) error { return inoAlloc.SetStatePath(p) }
func flushInoState()                 { inoAlloc.Flush() }
func saveInoState()                  { inoAlloc.Save() }

// The entry building lives in internal/syncplan; these keep the local names.
const (
	kmodRootIno  = syncplan.RootIno
	fileTypeMask = syncplan.FileTypeMask
)

func buildEntriesFromS3(ctx context.Context, s3c objstore.Backend) ([]fpebpf.Entry, error) {
	return syncplan.BuildEntries(ctx, s3c, inoAlloc)
}
func entriesToSyncPayload(es []fpebpf.Entry) []syncEntry { return syncplan.ToPayload(es) }

// -- sync loop --------------------------------------------------------

// resolveEventS3Key returns the S3 key for a VFS event (flush / delete /
// rename). Priority order:
//  1. The dyn-table entry's S3Key when it is already known (sync-promoted
//     file with a full prefix path).
//  2. resolveKeyPath(parentIno, name) — reconstructed from the parent ino
//     reverse map (G-01). Falls back to just name for root-level files.
//
// Called identically from handleFlush, handleDelete, and handleRename so
// that all three callers share one definition of "how an event becomes a key."
func resolveEventS3Key(ino, parentIno uint64, name string) string {
	if e := fpebpf.LookupByIno(ino); e != nil && e.S3Key != "" {
		return e.S3Key
	}
	return resolveKeyPath(parentIno, name)
}

// syncEntriesOnce runs one sync round: List → assign inos → push to
// kmod → update daemon mirror. Returns the fresh entry set on
// success. syncEntries is a no-op when s3c is nil (offline mode uses
// the bootstrap mirror kmod already has).
func syncEntriesOnce(ctx context.Context, devPath string, s3c objstore.Backend) ([]fpebpf.Entry, error) {
	if s3c == nil {
		return fpebpf.Snapshot(), nil
	}
	seedInoAllocator()
	timedCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	fresh, err := buildEntriesFromS3(timedCtx, s3c)
	if err != nil {
		metrics.Global.SyncErrors.Add(1)
		return nil, fmt.Errorf("list: %w", err)
	}
	payload := entriesToSyncPayload(fresh)
	if err := syncEntriesIoctl(devPath, payload); err != nil {
		metrics.Global.SyncErrors.Add(1)
		return nil, err
	}
	fpebpf.Replace(fresh)
	metrics.Global.SyncRounds.Add(1)
	metrics.Global.SyncEntriesCurrent.Store(uint64(len(fresh)))
	// V-02: flush the ino state once per sync round instead of on every
	// assignIno call. Batches N new keys from a single List into 1 write.
	flushInoState()
	return fresh, nil
}

// runSyncLoop invokes syncEntriesOnce on `interval` until ctx is
// done. A zero interval disables periodic sync (startup-only mode).
func runSyncLoop(ctx context.Context, devPath string, s3c objstore.Backend,
	interval time.Duration, onSync func([]fpebpf.Entry)) {
	if interval <= 0 {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			fresh, err := syncEntriesOnce(ctx, devPath, s3c)
			if err != nil {
				fmt.Fprintf(os.Stderr, "[sync] %v\n", err)
				continue
			}
			if onSync != nil {
				onSync(fresh)
			}
		}
	}
}
