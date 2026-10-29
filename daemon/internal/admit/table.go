package admit

import (
	"sort"
	"sync"

	"github.com/bbsakura/s3fs-ebpf/daemon/internal/kmodio"
	"github.com/bbsakura/s3fs-ebpf/daemon/internal/metrics"
)

// Arena is the part of the eBPF loader the table needs: where the arena is
// mapped, and giving a range back to its free-list (D-02).
type Arena interface {
	BaseAddr() uint64
	FreeArenaChunk(offset, size uint64)
}

// PushFunc replaces the kmod's admit table with the given snapshot
// (S3FS_IOC_ADMIT_ARENA).
type PushFunc func(devPath string, entries []kmodio.AdmitEntry) error

// DrainFunc pulls the inos the kmod's policy has evicted since the last call
// (S3FS_IOC_DRAIN_EVICTIONS).
type DrainFunc func(devPath string) (evicted []uint64, overflow uint32, err error)

// Table is the daemon-side source of truth for what the kmod currently has
// admitted.  The kmod ioctl semantics are "replace the whole table", so every
// mutation sends the full snapshot.
//
// H-04 invariant: mu is held from the moment the slice is mutated through the
// push.  Two concurrent mutators used to take their snapshots under the lock,
// release it and then issue the ioctls in an arbitrary order, so the kmod
// ended up with whichever snapshot won the syscall race and silently lost the
// other's mutation.  Holding mu across the push serializes the push order and
// keeps the kmod's table equal to the latest committed daemon-side state.
type Table struct {
	mu      sync.Mutex
	entries []kmodio.AdmitEntry
	arena   Arena // default arena for eviction feedback; nil offline

	push  PushFunc
	drain DrainFunc
}

// NewTable returns an empty table that pushes snapshots with push and learns
// about kmod-side evictions from drain.
func NewTable(push PushFunc, drain DrainFunc) *Table {
	return &Table{push: push, drain: drain}
}

// SetArena sets the arena that evicted entries are returned to when a push
// path reconciles the kmod's evict log.  nil disables the free-list update.
func (t *Table) SetArena(a Arena) {
	t.mu.Lock()
	t.arena = a
	t.mu.Unlock()
}

// Snapshot returns a copy of the current entries.
func (t *Table) Snapshot() []kmodio.AdmitEntry {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]kmodio.AdmitEntry(nil), t.entries...)
}

func (t *Table) setLocked(es []kmodio.AdmitEntry) {
	t.entries = es
	metrics.Global.AdmitsCurrent.Store(uint64(len(es)))
}

func arenaBase(a Arena) uint64 {
	if a == nil {
		return 0
	}
	return a.BaseAddr()
}

func free(a Arena, freed []ChunkPair) {
	if a == nil {
		return
	}
	for _, r := range freed {
		a.FreeArenaChunk(r.Offset, r.Size)
	}
}

// reconcileLocked pulls the kmod's evict log and drops those inos from the
// snapshot before the caller pushes it (S-04).  The kmod only evicts inside an
// admit_replace that the daemon itself issued under mu, so draining right
// before every push closes the window in which the periodic drain had not run
// yet and the next push re-sent, and so resurrected, evicted inos.  Errors are
// ignored: the periodic loop is still the fallback.
func (t *Table) reconcileLocked(devPath string) {
	evicted, _, err := t.drain(devPath)
	if err != nil || len(evicted) == 0 {
		return
	}
	t.dropLocked(evicted, t.arena)
}

// Set stores the initial (prewarm) list and pushes it.  Any prior state is
// dropped.
//
// X-04: sorted by Ino ascending so the kmod receives entries in assignment
// order (lower ino ~ earlier assigned ~ oldest).  The kmod breaks score ties
// by index (smallest = oldest); without the sort the prewarm list arrives in
// Go map iteration order and eviction is not deterministic across runs.
func (t *Table) Set(devPath string, initial []kmodio.AdmitEntry) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	sort.Slice(initial, func(i, j int) bool { return initial[i].Ino < initial[j].Ino })
	t.setLocked(append(t.entries[:0], initial...))
	return t.push(devPath, t.entries)
}

// Replace inserts entry and pushes the whole snapshot, dropping every prior
// admit of the same ino and returning those arena bytes to the free-list.  It
// serves both the first admission (no prior entry) and the refresh path (flush
// or re-admit after eviction).
//
// Without the dedup, a flush after prewarm would push [old, new] for one ino:
// the kmod's hash accepts both, the older one becomes dead weight, and its
// arena bytes leak because no eviction feedback removes them.  arena may be nil
// (offline mode); the free-list update is then skipped.
func (t *Table) Replace(devPath string, entry kmodio.AdmitEntry, arena Arena) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	// Before planning: if entry.Ino itself was evicted, its old range is freed
	// here and the fresh entry is then simply appended.
	t.reconcileLocked(devPath)
	next, freed := PlanReplace(t.entries, entry, arenaBase(arena))
	t.setLocked(next)
	free(arena, freed)
	return t.push(devPath, t.entries)
}

// ReplaceRange is Replace for one range of an ino (AC-49): it drops only the
// ino's entries that overlap entry and frees their arena bytes, and keeps its
// other ranges.  Replace drops every entry of the ino, which is right when the
// new entry is the whole object (prewarm, flush) but made a chunk admit (D-01)
// evict the ino's other chunks: reading chunk 0 threw away chunk 1, and the
// next read fetched chunk 1 from S3 again.
func (t *Table) ReplaceRange(devPath string, entry kmodio.AdmitEntry, arena Arena) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.reconcileLocked(devPath)
	next, freed := PlanReplaceRange(t.entries, entry, arenaBase(arena))
	t.setLocked(next)
	free(arena, freed)
	return t.push(devPath, t.entries)
}

// AddRange appends entry and pushes the full snapshot WITHOUT evicting the
// ino's other entries.  Used by the sequential prefetch path to stage the next
// chunk next to the current one (Replace would evict the current chunk first).
// Idempotent: returns nil without a push if the exact (ino, offset) pair is
// already admitted.
func (t *Table) AddRange(devPath string, entry kmodio.AdmitEntry) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.reconcileLocked(devPath)
	for _, e := range t.entries {
		if e.Ino == entry.Ino && e.Offset == entry.Offset {
			return nil
		}
	}
	t.setLocked(append(t.entries, entry))
	return t.push(devPath, t.entries)
}

// Refresh re-pushes the current snapshot without any mutation and returns its
// size.  Used to force a policy round when steady state (no admissions, no
// evictions) would otherwise leave the vtable idle.
func (t *Table) Refresh(devPath string) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.reconcileLocked(devPath)
	if err := t.push(devPath, t.entries); err != nil {
		return 0, err
	}
	return len(t.entries), nil
}

// Clear empties the snapshot and tells the kmod to drop its table.  Called on
// daemon shutdown.
func (t *Table) Clear(devPath string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.setLocked(t.entries[:0])
	return t.push(devPath, nil)
}

// Drop removes the listed inos from the snapshot without pushing (the kmod
// already dropped them via the policy vote; pushing would re-add them), and
// returns their arena ranges to the free-list (D-02).  It returns the number of
// entries removed.
func (t *Table) Drop(evicted []uint64, arena Arena) int {
	if len(evicted) == 0 {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.dropLocked(evicted, arena)
}

// DropIno removes every admit of ino and pushes the table.
func (t *Table) DropIno(devPath string, ino uint64, arena Arena) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.dropLocked([]uint64{ino}, arena) > 0 {
		_ = t.push(devPath, t.entries)
	}
}

// dropLocked returns the number of entries removed, not the number of freed
// arena chunks (which is smaller when arena is nil or Uaddr lies outside the
// arena) - T-02: returning the chunk count made the "[evict-drain] dropped N
// admits" log under-report without a wired arena.
func (t *Table) dropLocked(evicted []uint64, arena Arena) int {
	skip := make(map[uint64]struct{}, len(evicted))
	for _, ino := range evicted {
		skip[ino] = struct{}{}
	}
	base := arenaBase(arena)
	filtered := t.entries[:0]
	var freed []ChunkPair
	dropped := 0
	for _, e := range t.entries {
		if _, ok := skip[e.Ino]; ok {
			dropped++
			if arena != nil && base != 0 && e.Uaddr >= base {
				freed = append(freed, ChunkPair{Offset: e.Uaddr - base, Size: e.Size})
			}
			continue
		}
		filtered = append(filtered, e)
	}
	t.setLocked(filtered)
	free(arena, freed)
	return dropped
}

// Prune drops every admit whose ino is not in keep and pushes the result.
// Called after a bucket sync when a key disappeared from S3: the kmod's dentry
// is already gone but its admit hash keeps the orphan ino, so without this a
// long-running daemon leaks arena buffer and admit entries (T-01: the dropped
// entries' arena ranges also go back to the free-list).  The push is skipped
// when nothing was dropped.  The arena is the one set by SetArena.
func (t *Table) Prune(devPath string, keep map[uint64]struct{}) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.reconcileLocked(devPath)
	pruned, freed := PlanPrune(t.entries, keep, arenaBase(t.arena))
	dropped := len(t.entries) - len(pruned)
	t.setLocked(pruned)
	free(t.arena, freed)
	if dropped == 0 {
		return nil
	}
	return t.push(devPath, t.entries)
}
