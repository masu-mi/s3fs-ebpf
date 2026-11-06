// Startup / sync prewarm: queue the files the prewarm policy selects for
// --pcache-prewarm.  File bodies are not fetched here: they reach the kmod's chunk
// table on the first read (upcall + POPULATE_PAGES), or through the mount with
// --pcache-prewarm.  (Until v2 Phase 3e this also wrote an inode stub into the
// arena and the BPF inode_index for every entry, for the getattr fentry; both are gone.)

package main

import (
	"context"
	"fmt"
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

// prewarm queues the files the prewarm policy selects for --pcache-prewarm.
//
// V-03: entries are examined in parallel (up to prewarmWorkers) to overlap
// S3 round-trips.
func prewarm(ctx context.Context, s3c objstore.Backend, pol *policy.Engine) {
	timedCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	live := fpebpf.Snapshot()
	g, gCtx := errgroup.WithContext(timedCtx)
	g.SetLimit(prewarmWorkers)
	for _, entry := range live {
		entry := entry
		g.Go(func() error {
			prewarmEntry(gCtx, s3c, pol, entry)
			return nil
		})
	}
	_ = g.Wait()
}

// prewarmEntry decides whether one entry's content should be prewarmed and, if
// so (and --pcache-prewarm is on), queues the file to be read through the mount
// once the upcall loop runs (AC-67); POPULATE_PAGES cannot be used here because
// the inode does not exist yet.  The size comes from S3 (Stat, when reachable),
// else the bootstrap size.
func prewarmEntry(ctx context.Context, s3c objstore.Backend,
	pol *policy.Engine, entry fpebpf.Entry) {
	// G-01: synthetic directory entries have no S3 backing key.
	if entry.Mode&fileTypeMask == 0o040000 {
		return
	}
	if !pcachePrewarmEnabled {
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
	if !wantPrewarm {
		return
	}
	size := entry.Fallback
	if s3c != nil {
		meta, err := s3c.Stat(ctx, entry.S3Key)
		if err != nil {
			fmt.Printf("[prewarm] ino=%d s3://%s stat: %v (using fallback)\n",
				entry.Ino, entry.S3Key, err)
		} else {
			size = uint64(meta.Size)
		}
	}
	queuePcachePrewarm(entry.Ino, size)
}
