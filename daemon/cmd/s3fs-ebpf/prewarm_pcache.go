package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	fpebpf "github.com/bbsakura/s3fs-ebpf/daemon/internal/ebpf"
	"github.com/bbsakura/s3fs-ebpf/daemon/internal/mountinfo"
	"github.com/bbsakura/s3fs-ebpf/daemon/internal/prewarmq"
)

// AC-67: content prewarm in pcache mode (--arena-metadata-only).
//
// AC-44 turned prewarm into metadata only in pcache mode: at prewarm time
// the inode does not exist yet, so POPULATE_PAGES returns -ENOENT, and an
// inode made without a dentry would be evicted (generic_delete_inode) with
// its chunks on the last iput. Instead, after the read-miss loop is up, the
// daemon reads each selected file once through the mount. The read takes
// the ordinary miss path (lookup → read_folio → upcall → POPULATE_PAGES →
// signal), so every pcache invariant (chunk pin, R1, capacity pass) holds
// without kmod changes, and the dentry keeps the inode and its chunks alive.

// pcachePrewarmEnabled is set once by main from --pcache-prewarm.
var pcachePrewarmEnabled bool

// pcacheWorkerUp is true while runPcachePrewarm is serving the queue.
// Prefetch (AC-68) queues nothing otherwise, so the queue cannot grow
// without a reader.
var pcacheWorkerUp atomic.Bool

// pcachePrewarmMount is set once by main from --pcache-prewarm-mount.
// Empty means find the s3fs_ebpf mount in /proc/self/mounts.
var pcachePrewarmMount string

const (
	// pcachePrewarmReaders bounds concurrent prewarm reads. Kept well
	// under readMissWorkers so prewarm never starves real readers of
	// upcall handlers.
	pcachePrewarmReaders = 4
	// pcachePrewarmMountWait is how long the worker waits for the
	// mount to appear (scripts mount before starting the daemon, but
	// the order is not enforced).
	pcachePrewarmMountWait = 30 * time.Second
	s3fsFSType             = "s3fs_ebpf"
)

// pcacheQ is the process-wide queue of files the worker reads (internal/prewarmq).
var pcacheQ = prewarmq.New()

type pcachePrewarmItem = prewarmq.Item

// queuePcachePrewarm records a file whose content prewarm the policy asked
// for but pcache mode skipped at prewarm time. Safe from any goroutine.
func queuePcachePrewarm(ino, size uint64) {
	pcacheQ.Add([]pcachePrewarmItem{{Ino: ino, Size: size}})
}

func queuePcacheItems(items []pcachePrewarmItem) int { return pcacheQ.Add(items) }
func isWarming(ino uint64) bool                      { return pcacheQ.IsWarming(ino) }
func doneWarming(items []pcachePrewarmItem)          { pcacheQ.Done(items) }
func takePcachePrewarm() []pcachePrewarmItem         { return pcacheQ.Take() }

// waitForS3fsMount returns explicit when set, else polls mountsPath for an
// s3fs_ebpf mount until it appears or wait elapses.
func waitForS3fsMount(ctx context.Context, explicit, mountsPath string, wait time.Duration) (string, error) {
	return mountinfo.Wait(ctx, explicit, mountsPath, s3fsFSType, wait)
}

// runPcachePrewarm reads queued files through the mount until ctx is done.
// Start it after runReadMissLoop: the reads block on upcalls it serves.
func runPcachePrewarm(ctx context.Context) {
	mnt, err := waitForS3fsMount(ctx, pcachePrewarmMount, "/proc/self/mounts", pcachePrewarmMountWait)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[pcache-prewarm] %v — content prewarm disabled\n", err)
		return
	}
	fmt.Printf("[pcache-prewarm] reading through %s\n", mnt)
	pcacheWorkerUp.Store(true)
	defer pcacheWorkerUp.Store(false)
	var used uint64 // startup prewarm bytes so far (prefetch is not summed)
	for {
		items := takePcachePrewarm()
		if len(items) > 0 {
			capBytes, _ := capacityBytesFn()
			sel, skipped := prewarmq.SelectWithinCapacity(items, used, capBytes)
			if skipped > 0 {
				fmt.Printf("[pcache-prewarm] %d files skipped: capacity_bytes=%d would be exceeded\n",
					skipped, capBytes)
			}
			prewarmThroughMount(ctx, mnt, sel)
			for _, it := range sel {
				if !it.Prefetch {
					used += it.Size
				}
			}
			doneWarming(items)
		}
		select {
		case <-ctx.Done():
			return
		case <-pcacheQ.Kick():
		}
	}
}

// prewarmThroughMount reads each item's file to EOF and returns the bytes
// read. Errors are logged and skipped: prewarm is best effort.
func prewarmThroughMount(ctx context.Context, mnt string, items []pcachePrewarmItem) uint64 {
	paths := entryPaths(fpebpf.Snapshot())
	var (
		mu    sync.Mutex
		total uint64
		wg    sync.WaitGroup
	)
	sem := make(chan struct{}, pcachePrewarmReaders)
	start := time.Now()
	for _, it := range items {
		rel, ok := paths[it.Ino]
		if !ok {
			continue // removed by a sync round since it was queued
		}
		select {
		case <-ctx.Done():
			wg.Wait()
			return total
		case sem <- struct{}{}:
		}
		wg.Add(1)
		go func(ino uint64, p string) {
			defer wg.Done()
			defer func() { <-sem }()
			n, err := readToEOF(p)
			if err != nil {
				fmt.Fprintf(os.Stderr, "[pcache-prewarm] ino=%d %s: %v\n", ino, p, err)
			}
			mu.Lock()
			total += uint64(n)
			mu.Unlock()
		}(it.Ino, filepath.Join(mnt, rel))
	}
	wg.Wait()
	fmt.Printf("[pcache-prewarm] %d files, %d bytes in %s\n",
		len(items), total, time.Since(start).Round(time.Millisecond))
	return total
}

func readToEOF(path string) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	return io.CopyBuffer(io.Discard, f, make([]byte, 1<<20))
}

// entryPaths maps each regular file's ino to its path under the mount,
// built from the ParentIno / Name chain the kmod lookup uses (G-01).
func entryPaths(entries []fpebpf.Entry) map[uint64]string {
	byIno := make(map[uint64]fpebpf.Entry, len(entries))
	for _, e := range entries {
		byIno[e.Ino] = e
	}
	out := make(map[uint64]string, len(entries))
	for _, e := range entries {
		if e.Mode&fileTypeMask == 0o040000 {
			continue
		}
		parts := []string{e.Name}
		ok := true
		for p, depth := e.ParentIno, 0; p != 0 && p != 1; depth++ {
			d, found := byIno[p]
			if !found || depth > len(entries) { // dangling parent or a cycle
				ok = false
				break
			}
			parts = append(parts, d.Name)
			p = d.ParentIno
		}
		if !ok {
			continue
		}
		for i, j := 0, len(parts)-1; i < j; i, j = i+1, j-1 {
			parts[i], parts[j] = parts[j], parts[i]
		}
		out[e.Ino] = filepath.Join(parts...)
	}
	return out
}
