// Package admit holds the pure planning logic for the daemon-side admit
// table (what the kmod currently has admitted): which entries a new admit
// replaces, which are pruned, and which arena chunks that frees.  The ioctl
// push, locking and arena free-list calls stay in cmd/s3fs-ebpf.
package admit

import "github.com/bbsakura/s3fs-ebpf/daemon/internal/kmodio"

// ChunkPair is dropAdmits's per-eviction bookkeeping — a
// tiny value type so the "compute-then-free" split doesn't need to
// hold admitMu while calling into progs.FreeArenaChunk.
// T-05: uint64 so offsets > 4 GiB are handled correctly if the arena
// is ever extended; current arena is 128 MiB so this is defensive.
type ChunkPair struct {
	Offset uint64
	Size   uint64
}

// PlanReplace is the pure-function core of replaceAdmit: it
// filters any prior entry sharing entry.Ino out of `admits`, appends
// the fresh entry, and reports all evicted entries' arena ranges so
// the caller can return them to the free-list. Split out so the dedup
// invariant can be unit-tested without an eBPF loader or the
// /dev/s3fs_ebpf ioctl.
//
// T-04: returns freed []ChunkPair (all matches) rather than a
// single oldChunk. With D-01 range-list, the same ino can have
// multiple entries in admits; the prior single-value return discarded
// all but the last match's chunk, leaking arena bytes.
//
// arenaBase==0 or e.Uaddr < arenaBase suppresses the chunk report —
// used in offline mode and as a defensive guard against garbage
// uaddrs. The returned slice shares backing storage with `admits`
// (in-place filter), matching the previous implementation.
func PlanReplace(admits []kmodio.AdmitEntry, entry kmodio.AdmitEntry, arenaBase uint64) (
	next []kmodio.AdmitEntry, freed []ChunkPair,
) {
	filtered := admits[:0]
	for _, e := range admits {
		if e.Ino == entry.Ino {
			if arenaBase != 0 && e.Uaddr >= arenaBase {
				freed = append(freed, ChunkPair{
					Offset: e.Uaddr - arenaBase,
					Size:   e.Size,
				})
			}
			continue
		}
		filtered = append(filtered, e)
	}
	next = append(filtered, entry)
	return next, freed
}

// PlanReplaceRange is the pure core of replaceAdmitRange: entries of
// entry.Ino whose file range [Offset, Offset+Size) overlaps the new one are
// removed (and reported in freed, like PlanReplace), everything else is
// kept, and entry is appended.  A whole-object entry overlaps every range of
// its ino, so it behaves exactly like PlanReplace.
func PlanReplaceRange(admits []kmodio.AdmitEntry, entry kmodio.AdmitEntry, arenaBase uint64) (
	next []kmodio.AdmitEntry, freed []ChunkPair,
) {
	filtered := admits[:0]
	for _, e := range admits {
		if e.Ino == entry.Ino && RangesOverlap(e.Offset, e.Size, entry.Offset, entry.Size) {
			if arenaBase != 0 && e.Uaddr >= arenaBase {
				freed = append(freed, ChunkPair{
					Offset: e.Uaddr - arenaBase,
					Size:   e.Size,
				})
			}
			continue
		}
		filtered = append(filtered, e)
	}
	next = append(filtered, entry)
	return next, freed
}

// RangesOverlap reports whether [aOff, aOff+aSize) and [bOff, bOff+bSize)
// share a byte.  A zero-length range overlaps nothing.
func RangesOverlap(aOff, aSize, bOff, bSize uint64) bool {
	return aSize > 0 && bSize > 0 && aOff < bOff+bSize && bOff < aOff+aSize
}

// PlanPrune is the pure-function core of pruneAdmitsTo: given the
// current snapshot, it returns the filtered slice (keeping only inos in
// keepInos) and the arena ranges of the dropped entries so the caller can
// return them to the D-02 free-list. arenaBase==0 suppresses chunk
// reporting (offline mode). Split out so the arena-free invariant can be
// unit-tested without an eBPF loader.
func PlanPrune(
	snapshot []kmodio.AdmitEntry,
	keepInos map[uint64]struct{},
	arenaBase uint64,
) (pruned []kmodio.AdmitEntry, freed []ChunkPair) {
	pruned = snapshot[:0]
	for _, e := range snapshot {
		if _, ok := keepInos[e.Ino]; ok {
			pruned = append(pruned, e)
			continue
		}
		if arenaBase != 0 && e.Uaddr >= arenaBase {
			freed = append(freed, ChunkPair{
				Offset: e.Uaddr - arenaBase,
				Size:   e.Size,
			})
		}
	}
	return pruned, freed
}
