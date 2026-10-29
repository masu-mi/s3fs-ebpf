package ebpf

import (
	"errors"
	"fmt"
	"sort"

	"github.com/cilium/ebpf"
)

// arenaChunk is one entry in the free-list — a range of bytes in
// arena that used to hold admit content and is now available for
// reuse. offset is the arena byte offset (matches AdmitContent's
// return value); size is the chunk length.
// T-05: both fields are uint64 to match AdmitContent / FreeArenaChunk.
type arenaChunk struct {
	offset uint64
	size   uint64
}

// ErrCacheFull signals the arena has no more slots for InodeStubs.
var ErrCacheFull = errors.New("arena cache full")

// AdmitContent writes `data` into the arena and returns its byte
// offset. Preferred allocation order (D-02):
//
//  1. First-fit match in the free-list: any evicted chunk large
//     enough to hold `data` is reused (smallest-of-fits picked for
//     minimal internal waste).
//  2. Otherwise extend the bump allocator (cacheNextOff) if arena
//     still has room.
//
// Chunks larger than `len(data)` are split — the used prefix goes to
// the caller, the tail returns to the free-list. This lets 100 MiB+
// blobs evict-and-refit at chunk granularity without arena bloat.
func (p *Programs) AdmitContent(data []byte) (uint64, error) {
	p.cacheMu.Lock()
	defer p.cacheMu.Unlock()

	arenaSize := p.arena.Size()
	need := uint64(len(data))

	// (1) Best-fit in free-list. arenaFree is sorted ascending by size
	// (V-04), so sort.Search finds the first (= smallest) fitting chunk
	// in O(log n) instead of the prior O(n) linear scan.
	i := sort.Search(len(p.arenaFree), func(j int) bool {
		return p.arenaFree[j].size >= need
	})
	if i < len(p.arenaFree) {
		chunk := p.arenaFree[i]
		// Remove by shifting; preserves the size-sorted invariant.
		p.arenaFree = append(p.arenaFree[:i], p.arenaFree[i+1:]...)
		if len(data) > 0 {
			if _, err := p.arena.WriteAt(data, int64(chunk.offset)); err != nil {
				return 0, fmt.Errorf("arena write reuse: %w", err)
			}
		}
		// Return leftover to the free-list. Split tail may abut an
		// existing chunk (unlikely but not disallowed by the allocator
		// order); H-03 coalesce catches it.
		if chunk.size > need {
			p.arenaFree = insertArenaFreeBySize(p.arenaFree, arenaChunk{
				offset: chunk.offset + need,
				size:   chunk.size - need,
			})
			p.arenaFree, p.cacheNextOff = coalesceArenaFree(p.arenaFree, p.cacheNextOff)
		}
		return chunk.offset, nil
	}

	// (2) Bump allocator.
	nextOff := p.cacheNextOff
	if nextOff+need > arenaSize {
		return 0, ErrCacheFull
	}
	if need > 0 {
		if _, err := p.arena.WriteAt(data, int64(nextOff)); err != nil {
			return 0, fmt.Errorf("arena write content: %w", err)
		}
	}
	off := nextOff
	p.cacheNextOff = nextOff + need
	return off, nil
}

// FreeArenaChunk returns a (offset, size) range to the free-list so
// a subsequent AdmitContent can reuse it. Idempotent for zero-size.
// D-02: called by the daemon's drain loop when kmod-side eviction
// removes an admit whose arena range is no longer referenced.
//
// H-03: after append, coalesceArenaFreeLocked merges adjacent
// chunks and returns any trailing run that abuts the bump allocator
// back to cacheNextOff. This keeps long-running daemons from
// fragmenting into hundreds of tiny chunks even under sustained
// evict/admit churn.
func (p *Programs) FreeArenaChunk(offset, size uint64) {
	if size == 0 {
		return
	}
	p.cacheMu.Lock()
	defer p.cacheMu.Unlock()
	// V-05: binary-insert maintains size-sorted order without a full
	// sort.Slice call (previously: append + coalesceArenaFree sorted).
	p.arenaFree = insertArenaFreeBySize(p.arenaFree, arenaChunk{offset: offset, size: size})
	p.arenaFree, p.cacheNextOff = coalesceArenaFree(p.arenaFree, p.cacheNextOff)
}

// insertArenaFreeBySize inserts c into free at the position that maintains
// ascending size order. Called by FreeArenaChunk and AdmitContent (split
// tail) so arenaFree stays sorted for O(log n) search in AdmitContent
// without a full sort.Slice on every insertion (V-04/V-05).
func insertArenaFreeBySize(free []arenaChunk, c arenaChunk) []arenaChunk {
	i := sort.Search(len(free), func(j int) bool { return free[j].size >= c.size })
	free = append(free, arenaChunk{})
	copy(free[i+1:], free[i:])
	free[i] = c
	return free
}

// coalesceArenaFree sorts `free` by offset, merges adjacent chunks,
// and — if the trailing run abuts `nextOff` — folds it back into the
// bump allocator (returning the reduced list and the new nextOff).
// Pure function so the merge invariant can be unit-tested without a
// live arena. Called under Programs.cacheMu.
//
// The free-list is normally maintained sorted by size (V-04/V-05), so
// this function sorts by offset for the merge pass, then restores size
// order before returning. The extra sort is acceptable because coalesce
// is called infrequently (only on eviction or split) and n is small.
func coalesceArenaFree(free []arenaChunk, nextOff uint64) ([]arenaChunk, uint64) {
	if len(free) == 0 {
		return free, nextOff
	}
	sort.Slice(free, func(i, j int) bool {
		return free[i].offset < free[j].offset
	})
	merged := free[:0]
	for _, c := range free {
		if len(merged) > 0 {
			last := &merged[len(merged)-1]
			if last.offset+last.size == c.offset {
				last.size += c.size
				continue
			}
		}
		merged = append(merged, c)
	}
	if len(merged) > 0 {
		tail := merged[len(merged)-1]
		if tail.offset+tail.size == nextOff {
			nextOff = tail.offset
			merged = merged[:len(merged)-1]
		}
	}
	// Restore size order so AdmitContent's sort.Search invariant holds.
	sort.Slice(merged, func(i, j int) bool { return merged[i].size < merged[j].size })
	return merged, nextOff
}

// ArenaFreeStats reports the current free-list state — total bytes
// available and the number of chunks. Used by /metrics for the
// arena_free_bytes gauge (D-02) and by CLI stats commands.
func (p *Programs) ArenaFreeStats() (bytes uint64, chunks int) {
	p.cacheMu.Lock()
	defer p.cacheMu.Unlock()
	for _, c := range p.arenaFree {
		bytes += c.size
	}
	return bytes, len(p.arenaFree)
}

// PreallocArenaPages runs the SEC("syscall") wrapper around
// bpf_arena_alloc_pages so both user_vm and kern_vm PTEs get
// populated for `pages` pages pinned at user_vm_start. Returning
// without error means kmod's kern_vm reads on those pages will no
// longer EFAULT.
func (p *Programs) PreallocArenaPages(pages uint32) error {
	if pages == 0 {
		return nil
	}
	in := TestArgs{
		ArenaBaseUaddr: p.arena.BaseAddr(),
		Idx:            pages,
	}
	out := TestArgs{}
	ret, err := p.Objs.S3fsArenaPrealloc.Run(&ebpf.RunOptions{
		Context:    in,
		ContextOut: &out,
	})
	if err != nil {
		return fmt.Errorf("arena prealloc %d pages: %w", pages, err)
	}
	if int32(ret) != 0 {
		return fmt.Errorf("arena prealloc returned %d (out_ino=0x%x)", int32(ret), out.OutIno)
	}
	return nil
}
