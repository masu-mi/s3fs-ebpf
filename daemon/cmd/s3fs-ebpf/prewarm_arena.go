// Startup / sync prewarm: metadata warm (inode_index stubs) from the kmod entry table.
// File bodies are not fetched here: they reach the kmod's chunk table on the first
// read (upcall + POPULATE_PAGES), or through the mount with --pcache-prewarm.

package main

import (
	"context"
	"fmt"
	"os"
	"time"

	fpebpf "github.com/bbsakura/s3fs-ebpf/daemon/internal/ebpf"

	"github.com/bbsakura/s3fs-ebpf/daemon/internal/objstore"
	"github.com/bbsakura/s3fs-ebpf/daemon/internal/policy"
	"golang.org/x/sync/errgroup"
)

// prewarmWorkers is the concurrency limit for parallel S3 fetches during
// prewarm. 8 matches typical S3 client concurrency limits and avoids
// overwhelming small MinIO test instances (V-03).
const prewarmWorkers = 8

// prewarm warms the inode_index stub of every entry in the kmod entry table
// so the first stat hits the fentry fast_hits counter, and queues the files
// the prewarm policy selects for --pcache-prewarm.
//
// V-03: entries are prewarmed in parallel (up to prewarmWorkers) to overlap
// S3 round-trips.
func prewarm(ctx context.Context, progs *fpebpf.Programs, s3c objstore.Backend, pol *policy.Engine) {
	timedCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	live := fpebpf.Snapshot()
	g, gCtx := errgroup.WithContext(timedCtx)
	g.SetLimit(prewarmWorkers)
	for _, entry := range live {
		entry := entry
		g.Go(func() error {
			prewarmEntry(gCtx, progs, s3c, pol, entry)
			return nil
		})
	}
	_ = g.Wait()
}

// prewarmEntry warms the metadata of one entry: S3 Stat (when reachable, for
// the authoritative size / mtime, else the bootstrap values) -> progs.WarmCache.
// It runs regardless of the prewarm policy.  When the policy wants the content
// and --pcache-prewarm is on, the file is queued to be read through the mount
// once the upcall loop runs (AC-67); POPULATE_PAGES cannot be used here
// because the inode does not exist yet.
func prewarmEntry(ctx context.Context, progs *fpebpf.Programs, s3c objstore.Backend,
	pol *policy.Engine, entry fpebpf.Entry) {
	// G-01: synthetic directory entries have no S3 backing key; WarmCache
	// would try to stat "", which S3 answers with a nonsense error.
	if entry.Mode&fileTypeMask == 0o040000 {
		return
	}
	// Entry.Prewarm is the static bootstrap default; the policy engine, when
	// loaded, overrides per-entry (P-01 spike).
	wantPrewarm := entry.Prewarm
	if pol != nil {
		wantPrewarm = pol.EvalPrewarm(policy.EntryInput{
			Ino: entry.Ino, Name: entry.Name, S3Key: entry.S3Key,
			Size: entry.Fallback, Mode: entry.Mode,
		})
	}

	stub := fpebpf.InodeStub{
		Ino:     entry.Ino,
		Size:    entry.Fallback,
		MtimeNs: uint64(time.Now().UnixNano()),
		Mode:    entry.Mode,
	}
	source := "fallback"
	if s3c != nil {
		meta, err := s3c.Stat(ctx, entry.S3Key)
		if err != nil {
			fmt.Printf("[prewarm] ino=%d s3://%s stat: %v (using fallback)\n",
				entry.Ino, entry.S3Key, err)
		} else {
			stub.Size = uint64(meta.Size)
			stub.MtimeNs = uint64(meta.LastModified.UnixNano())
			source = fmt.Sprintf("s3://%s", entry.S3Key)
		}
	}
	off, err := progs.WarmCache(stub)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[prewarm] ino=%d warm: %v\n", entry.Ino, err)
		return
	}
	fmt.Printf("[prewarm] ino=%d name=%s size=%d src=%s → stub=%d\n",
		entry.Ino, entry.Name, stub.Size, source, off)

	if wantPrewarm && pcachePrewarmEnabled {
		queuePcachePrewarm(entry.Ino, stub.Size)
	}
}
