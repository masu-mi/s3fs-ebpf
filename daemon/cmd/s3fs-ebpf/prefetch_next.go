package main

import (
	"fmt"
	"sort"

	fpebpf "github.com/bbsakura/s3fs-ebpf/daemon/internal/ebpf"
	"github.com/bbsakura/s3fs-ebpf/daemon/internal/policy"
)

// AC-68: sibling read-ahead. When a read misses on the first chunk of a
// file, the CEL hook `prefetch_next: <int>` (evaluated on that file) says
// how many of the following files in the same directory, by name, to read
// ahead. They go to the pcache prewarm worker (prewarm_pcache.go), which
// reads them through the mount, so the next shard / log segment is already
// in the page cache when the reader opens it. pcache mode only.

// prefetchPolicy is set once by main when the policy declares
// prefetch_next and pcache mode is on; nil disables prefetch.
var prefetchPolicy *policy.Engine

// maybePrefetchSiblings is called by handleReadMiss for a read (not a load
// for write) miss on the file's first chunk.
func maybePrefetchSiblings(ino uint64) {
	if prefetchPolicy == nil || !pcacheWorkerUp.Load() || isWarming(ino) {
		return // disabled, no reader, or the worker's own read
	}
	entries := fpebpf.Snapshot()
	var cur *fpebpf.Entry
	for i := range entries {
		if entries[i].Ino == ino {
			cur = &entries[i]
			break
		}
	}
	if cur == nil {
		return
	}
	n := prefetchPolicy.EvalPrefetchNext(policy.EntryInput{
		Ino: cur.Ino, Name: cur.Name, S3Key: cur.S3Key,
		Size: cur.Fallback, Mode: cur.Mode,
	})
	if n == 0 {
		return
	}
	sibs := nextSiblings(entries, *cur, n)
	items := make([]pcachePrewarmItem, 0, len(sibs))
	for _, e := range sibs {
		items = append(items, pcachePrewarmItem{Ino: e.Ino, Size: e.Fallback, Prefetch: true})
	}
	if q := queuePcacheItems(items); q > 0 {
		fmt.Printf("[prefetch-next] ino=%d %s: queued %d of the next %d siblings\n",
			cur.Ino, cur.Name, q, n)
	}
}

// nextSiblings returns up to n regular files that follow cur in its
// directory, ordered by name. Parent 0 and 1 are both the root (G-01).
func nextSiblings(entries []fpebpf.Entry, cur fpebpf.Entry, n int) []fpebpf.Entry {
	parent := normParent(cur.ParentIno)
	var sibs []fpebpf.Entry
	for _, e := range entries {
		if normParent(e.ParentIno) != parent || e.Mode&fileTypeMask == 0o040000 {
			continue
		}
		if e.Name > cur.Name {
			sibs = append(sibs, e)
		}
	}
	sort.Slice(sibs, func(i, j int) bool { return sibs[i].Name < sibs[j].Name })
	if len(sibs) > n {
		sibs = sibs[:n]
	}
	return sibs
}

func normParent(p uint64) uint64 {
	if p == 1 {
		return 0
	}
	return p
}
