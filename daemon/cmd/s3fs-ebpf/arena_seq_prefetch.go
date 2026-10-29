package main

import (
	"context"
	"fmt"
	"sync"
	"time"

	fpebpf "github.com/bbsakura/s3fs-ebpf/daemon/internal/ebpf"
	"github.com/bbsakura/s3fs-ebpf/daemon/internal/objstore"
)

// prefetchSeqEnabled gates runSeqPrefetchLoop. Set once by main before any
// goroutine reads it.
var prefetchSeqEnabled bool

const (
	// prefetchInterval is how often the prefetch goroutine scans admitted inos.
	prefetchInterval = 200 * time.Millisecond
	// prefetchTriggerWindow is the number of bytes before the end of an
	// admitted chunk at which we start fetching the next chunk.
	// partialAdmitChunk / 2 = 2 MiB lookahead.
	prefetchTriggerWindow = partialAdmitChunk / 2
)

// seqPrefetchNext returns the offset of the next chunk to pre-admit for an
// ino, given the ino's current admit ranges and the last read position from
// the pattern map. Returns -1 when no prefetch is needed (file fully
// covered, trigger threshold not reached, or next chunk already admitted).
func seqPrefetchNext(inoAdmits []admitEntry, lastEnd uint64, fullSize uint64) int64 {
	if fullSize == 0 || lastEnd == 0 {
		return -1
	}
	for _, e := range inoAdmits {
		end := e.Offset + e.Size
		if lastEnd < e.Offset || lastEnd > end {
			continue
		}
		// lastEnd is within [e.Offset, end].
		// Only trigger when within prefetchTriggerWindow of the chunk boundary.
		if end < prefetchTriggerWindow || lastEnd < end-prefetchTriggerWindow {
			continue
		}
		nextStart := int64(end)
		if nextStart >= int64(fullSize) {
			return -1 // already at or past EOF
		}
		// Check if the next chunk is already admitted.
		for _, e2 := range inoAdmits {
			if int64(e2.Offset) == nextStart {
				return -1
			}
		}
		return nextStart
	}
	return -1
}

// runSeqPrefetchLoop periodically inspects admitted inos with a strong
// sequential signal and pre-admits the next chunk before the miss fires.
// Exits when ctx is cancelled. No-op when prefetchSeqEnabled is false.
func runSeqPrefetchLoop(ctx context.Context, devPath string,
	progs *fpebpf.Programs, s3c objstore.Backend) {

	if !prefetchSeqEnabled || progs == nil || s3c == nil {
		return
	}

	ticker := time.NewTicker(prefetchInterval)
	defer ticker.Stop()

	// inflight tracks in-flight prefetches so we don't double-fetch.
	// Key: (ino<<20 | chunkIndex) — chunkIndex = chunkOffset/partialAdmitChunk.
	var inflight sync.Map

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		// Snapshot admits under the lock to avoid holding it during S3 calls.
		admitMu.Lock()
		snapshot := append([]admitEntry(nil), admits...)
		admitMu.Unlock()

		// Group entries by ino.
		byIno := make(map[uint64][]admitEntry, len(snapshot))
		for _, e := range snapshot {
			byIno[e.Ino] = append(byIno[e.Ino], e)
		}

		for ino, inoAdmits := range byIno {
			pat, ok := progs.ReadAccessPattern(ino)
			if !ok || pat.SeqRatio() < seqAdmitRatioThreshold {
				continue
			}
			entry := fpebpf.LookupByIno(ino)
			if entry == nil || entry.Fallback == 0 {
				continue
			}
			nextOff := seqPrefetchNext(inoAdmits, pat.LastEndPos, entry.Fallback)
			if nextOff < 0 {
				continue
			}
			chunkIdx := uint64(nextOff) / partialAdmitChunk
			inflightKey := ino<<20 | chunkIdx
			if _, loaded := inflight.LoadOrStore(inflightKey, struct{}{}); loaded {
				continue
			}
			go func(ino uint64, chunkOffset int64, key uint64) {
				defer inflight.Delete(key)
				e := fpebpf.LookupByIno(ino)
				if e == nil {
					return
				}
				fullSize := int64(e.Fallback)
				chunkSize := int64(partialAdmitChunk)
				if chunkOffset+chunkSize > fullSize {
					chunkSize = fullSize - chunkOffset
				}
				body, err := s3c.GetRange(ctx, e.S3Key, chunkOffset, chunkSize)
				if err != nil {
					fmt.Printf("[prefetch] ino=%d %s offset=%d: %v\n",
						ino, e.Name, chunkOffset, err)
					return
				}
				off, err := progs.AdmitContent(body)
				if err != nil {
					fmt.Printf("[prefetch] ino=%d %s offset=%d admit: %v\n",
						ino, e.Name, chunkOffset, err)
					return
				}
				uaddr := progs.Arena().BaseAddr() + off
				updateArenaHighWater(off, len(body))
				if err := addAdmitRange(devPath, admitEntry{
					Ino:    ino,
					Uaddr:  uaddr,
					Size:   uint64(len(body)),
					Offset: uint64(chunkOffset),
				}, progs); err != nil {
					fmt.Printf("[prefetch] ino=%d %s offset=%d push: %v\n",
						ino, e.Name, chunkOffset, err)
					return
				}
				fmt.Printf("[prefetch] ino=%d %s range [%d,%d) pre-admitted\n",
					ino, e.Name, chunkOffset, chunkOffset+chunkSize)
			}(ino, nextOff, inflightKey)
		}
	}
}
