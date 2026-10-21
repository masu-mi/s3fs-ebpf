package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	fpebpf "github.com/bbsakura/s3fs-ebpf/daemon/internal/ebpf"
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

type pcachePrewarmItem struct {
	ino      uint64
	size     uint64
	prefetch bool // AC-68: queued by a read miss, not by the prewarm policy
}

var pcachePrewarmQ struct {
	mu    sync.Mutex
	items []pcachePrewarmItem
	// warming holds the inos queued or being read by the worker. A miss
	// on one of them is the worker's own read, so it must not trigger
	// prefetch_next (that would chain through the whole directory).
	warming map[uint64]struct{}
	kick    chan struct{}
}

func init() {
	pcachePrewarmQ.kick = make(chan struct{}, 1)
	pcachePrewarmQ.warming = make(map[uint64]struct{})
}

// queuePcachePrewarm records a file whose content prewarm the policy asked
// for but pcache mode skipped at prewarm time. Safe from any goroutine.
func queuePcachePrewarm(ino, size uint64) {
	queuePcacheItems([]pcachePrewarmItem{{ino: ino, size: size}})
}

// queuePcacheItems queues items whose ino is not already queued or being
// read, and returns how many it queued.
func queuePcacheItems(items []pcachePrewarmItem) int {
	n := 0
	pcachePrewarmQ.mu.Lock()
	for _, it := range items {
		if _, busy := pcachePrewarmQ.warming[it.ino]; busy {
			continue
		}
		pcachePrewarmQ.warming[it.ino] = struct{}{}
		pcachePrewarmQ.items = append(pcachePrewarmQ.items, it)
		n++
	}
	pcachePrewarmQ.mu.Unlock()
	if n > 0 {
		select {
		case pcachePrewarmQ.kick <- struct{}{}:
		default:
		}
	}
	return n
}

// isWarming reports whether ino is queued or being read by the worker.
func isWarming(ino uint64) bool {
	pcachePrewarmQ.mu.Lock()
	defer pcachePrewarmQ.mu.Unlock()
	_, ok := pcachePrewarmQ.warming[ino]
	return ok
}

func doneWarming(items []pcachePrewarmItem) {
	pcachePrewarmQ.mu.Lock()
	for _, it := range items {
		delete(pcachePrewarmQ.warming, it.ino)
	}
	pcachePrewarmQ.mu.Unlock()
}

func takePcachePrewarm() []pcachePrewarmItem {
	pcachePrewarmQ.mu.Lock()
	defer pcachePrewarmQ.mu.Unlock()
	out := pcachePrewarmQ.items
	pcachePrewarmQ.items = nil
	return out
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
			sel, skipped := selectWithinCapacity(items, used, capBytes)
			if skipped > 0 {
				fmt.Printf("[pcache-prewarm] %d files skipped: capacity_bytes=%d would be exceeded\n",
					skipped, capBytes)
			}
			prewarmThroughMount(ctx, mnt, sel)
			for _, it := range sel {
				if !it.prefetch {
					used += it.size
				}
			}
			doneWarming(items)
		}
		select {
		case <-ctx.Done():
			return
		case <-pcachePrewarmQ.kick:
		}
	}
}

// selectWithinCapacity keeps startup prewarm items, in order, while the
// running total (starting at used) stays within capBytes: prewarming past
// the capacity would only evict what was just prewarmed. Prefetch items
// (AC-68) replace older content as the reader moves on, so they are not
// summed; each is kept unless it alone exceeds a quarter of the capacity.
// capBytes <= 0 means no limit.
func selectWithinCapacity(items []pcachePrewarmItem, used uint64, capBytes int64) ([]pcachePrewarmItem, int) {
	if capBytes <= 0 {
		return items, 0
	}
	var out []pcachePrewarmItem
	for _, it := range items {
		if it.prefetch {
			if it.size > uint64(capBytes)/4 {
				continue
			}
		} else {
			if used+it.size > uint64(capBytes) {
				continue
			}
			used += it.size
		}
		out = append(out, it)
	}
	return out, len(items) - len(out)
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
		rel, ok := paths[it.ino]
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
		}(it.ino, filepath.Join(mnt, rel))
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

// waitForS3fsMount returns explicit when set, else polls mountsPath for an
// s3fs_ebpf mount until it appears or wait elapses.
func waitForS3fsMount(ctx context.Context, explicit, mountsPath string, wait time.Duration) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	deadline := time.Now().Add(wait)
	for {
		mnt, err := findS3fsMount(mountsPath)
		if err == nil && mnt != "" {
			return mnt, nil
		}
		if time.Now().After(deadline) {
			if err == nil {
				err = fmt.Errorf("no %s mount in %s after %s", s3fsFSType, mountsPath, wait)
			}
			return "", err
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// findS3fsMount returns the first s3fs_ebpf mount point in a
// /proc/self/mounts formatted file, or "" when there is none.
func findS3fsMount(mountsPath string) (string, error) {
	f, err := os.Open(mountsPath)
	if err != nil {
		return "", err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) >= 3 && fields[2] == s3fsFSType {
			return unescapeMountField(fields[1]), nil
		}
	}
	return "", sc.Err()
}

// unescapeMountField undoes the octal escapes (\040 for space, etc.) the
// kernel applies to paths in /proc/self/mounts.
func unescapeMountField(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) && isOctal(s[i+1]) && isOctal(s[i+2]) && isOctal(s[i+3]) {
			b.WriteByte((s[i+1]-'0')<<6 | (s[i+2]-'0')<<3 | (s[i+3] - '0'))
			i += 3
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func isOctal(c byte) bool { return c >= '0' && c <= '7' }
