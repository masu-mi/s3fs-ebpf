// Package ebpf wraps the bpf2go-generated objects (s3fsfp_*) with a
// small Load/Run/Close lifecycle for the daemon.
package ebpf

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/bbsakura/s3fs-ebpf/daemon/internal/arena"
	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/rlimit"
)

// StatSlot indices mirror `enum s3fs_stat_slot` in
// include/s3fs_shared_types.h. Keep in sync.
const (
	StatFastHits            uint32 = 0
	StatSlowMisses          uint32 = 1
	StatUnknownFS           uint32 = 2
	StatReadIterHits        uint32 = 3
	StatReadIterMisses      uint32 = 4
	StatReadIterHitsPos0    uint32 = 5 // Y-06: read_iter hit where ki_pos==0 (first read per open)
	StatSlotMax             uint32 = 6
)

// MissOp mirrors `enum s3fs_miss_op`.
const (
	OpGetattr uint32 = 0
	OpLookup  uint32 = 1
	OpRead    uint32 = 2
)

// MissEvent mirrors `struct s3fs_miss_event`.
type MissEvent struct {
	Ino uint64
	Op  uint32
	Pad uint32
}

// TestArgs mirrors `struct s3fs_test_args` in include/s3fs_shared_types.h.
// Layout MUST stay in sync: Week 2 grew a leading arena_base_uaddr so the
// BPF program can reconstruct __arena pointers from the daemon's mmap.
type TestArgs struct {
	ArenaBaseUaddr uint64
	Idx            uint32
	KfuncArg       uint32
	OutIno         uint64
	OutKfunc       uint32
	Pad            uint32
}

// runtimeObjs is the subset of bpf2go objects loaded at daemon startup.
//
// The generated s3fsfpObjects also includes S3fsDeepWalkReject — a
// deliberately verifier-rejected program used only by the verifier-demo
// CLI. Referencing it here would drag it into every LoadAndAssign and
// break runtime paths, so we mirror only the healthy programs and let
// the demo path load its own program by name.
type runtimeObjs struct {
	// Programs.
	S3fsFpGetattr     *ebpf.Program `ebpf:"s3fs_fp_getattr"`
	S3fsFpReadIter    *ebpf.Program `ebpf:"s3fs_fp_read_iter"`
	S3fsTestShared    *ebpf.Program `ebpf:"s3fs_test_shared"`
	S3fsArenaPrealloc *ebpf.Program `ebpf:"s3fs_arena_prealloc"`

	// Maps.
	S3fsArena          *ebpf.Map `ebpf:"s3fs_arena"`
	S3fsCounters       *ebpf.Map `ebpf:"s3fs_counters"`
	S3fsEvents         *ebpf.Map `ebpf:"s3fs_events"`
	S3fsInodeIndex     *ebpf.Map `ebpf:"s3fs_inode_index"`
	S3fsInoSlots *ebpf.Map `ebpf:"s3fs_ino_slots"`
}

func (o *runtimeObjs) Close() error {
	return _S3fsfpClose(
		o.S3fsFpGetattr, o.S3fsFpReadIter, o.S3fsTestShared, o.S3fsArenaPrealloc,
		o.S3fsArena, o.S3fsCounters, o.S3fsEvents, o.S3fsInodeIndex, o.S3fsInoSlots,
	)
}

// Programs holds loaded bpf2go objects together with the pre-mmap'd arena.
//
// Load order matters: the kernel refuses bpf_addr_space_cast unless the
// arena's user_vm_start is already set, so we create the arena map, mmap
// it, and only then load the programs (with MapReplacements pointing at
// the pre-created arena).
type Programs struct {
	Objs  runtimeObjs
	arena *arena.Arena

	// cache admission state for WarmCache. cacheKnown deduplicates
	// concurrent misses on the same ino; cacheNextOff is a bump
	// allocator into the arena.
	//
	// D-02: arenaFree is a free-list of {offset, size} chunks that
	// eviction returns to. AdmitContent searches this list first
	// (first-fit) before extending cacheNextOff. Turns the arena
	// from a monotonic high-water buffer into a reusable pool.
	//
	// T-05: offsets and sizes use uint64 so the API is safe for arenas
	// larger than 4 GiB. Current arena is 128 MiB, but keeping the
	// arithmetic in uint64 prevents silent truncation if the limit grows.
	cacheMu      sync.Mutex
	cacheKnown   map[uint64]uint64
	cacheNextOff uint64
	arenaFree    []arenaChunk
}

// arenaChunk is one entry in the free-list — a range of bytes in
// arena that used to hold admit content and is now available for
// reuse. offset is the arena byte offset (matches AdmitContent's
// return value); size is the chunk length.
// T-05: both fields are uint64 to match AdmitContent / FreeArenaChunk.
type arenaChunk struct {
	offset uint64
	size   uint64
}

// Load pulls the bpf2go collection into the kernel and returns a handle.
// Removes the memlock rlimit before load (required on older kernels).
func Load() (*Programs, error) {
	if err := rlimit.RemoveMemlock(); err != nil {
		return nil, fmt.Errorf("remove memlock: %w", err)
	}

	spec, err := loadS3fsfp()
	if err != nil {
		return nil, fmt.Errorf("parse bpf spec: %w", err)
	}

	arenaSpec, ok := spec.Maps["s3fs_arena"]
	if !ok {
		return nil, fmt.Errorf("s3fs_arena map missing from spec")
	}
	arenaMap, err := ebpf.NewMap(arenaSpec)
	if err != nil {
		return nil, fmt.Errorf("create arena map: %w", err)
	}
	// Mmap NOW: this sets arena->user_vm_start in the kernel, without
	// which the verifier rejects bpf_addr_space_cast at prog load.
	a, err := arena.Wrap(arenaMap)
	if err != nil {
		arenaMap.Close()
		return nil, fmt.Errorf("mmap arena: %w", err)
	}

	p := &Programs{arena: a, cacheKnown: make(map[uint64]uint64)}
	opts := &ebpf.CollectionOptions{
		MapReplacements: map[string]*ebpf.Map{
			"s3fs_arena": arenaMap,
		},
	}
	if err := spec.LoadAndAssign(&p.Objs, opts); err != nil {
		a.Close()
		// cilium/ebpf truncates VerifierError.Error() to a single line.
		// The full log (which cilium/ebpf auto-refetches on load failure)
		// is only surfaced with %+v — critical when debugging arena or
		// addr_space_cast rejections. Keep it on the error path only.
		var ve *ebpf.VerifierError
		if errors.As(err, &ve) {
			return nil, fmt.Errorf("load bpf objects: %w\n=== verifier log ===\n%+v", err, ve)
		}
		return nil, fmt.Errorf("load bpf objects: %w", err)
	}
	return p, nil
}

// Arena returns the pre-mmap'd arena handle. The daemon writes stub data
// via WriteAt and reads the mmap base via BaseAddr() to hand to BPF.
func (p *Programs) Arena() *arena.Arena {
	return p.arena
}

// EventsMap returns the ringbuf carrying slow-path miss events.
func (p *Programs) EventsMap() *ebpf.Map { return p.Objs.S3fsEvents }

// CountersMap returns the per-CPU counter array indexed by StatSlot*.
func (p *Programs) CountersMap() *ebpf.Map { return p.Objs.S3fsCounters }

// InodeIndexMap returns the ino → arena-offset hash the fentry hook uses
// to decide hit/miss. Daemon populates this as it caches S3 metadata.
func (p *Programs) InodeIndexMap() *ebpf.Map { return p.Objs.S3fsInodeIndex }

// AttachFentry attaches the fentry/vfs_getattr program. The returned
// link.Link must be Close()d during teardown or the program stays
// attached beyond the process lifetime.
func (p *Programs) AttachFentry() (link.Link, error) {
	l, err := link.AttachTracing(link.TracingOptions{
		Program: p.Objs.S3fsFpGetattr,
	})
	if err != nil {
		return nil, fmt.Errorf("attach fentry/vfs_getattr: %w", err)
	}
	return l, nil
}

// AttachFentryReadIter attaches the fentry/generic_file_read_iter observer
// (R-01). The returned link.Link must be Close()d during teardown.
func (p *Programs) AttachFentryReadIter() (link.Link, error) {
	l, err := link.AttachTracing(link.TracingOptions{
		Program: p.Objs.S3fsFpReadIter,
	})
	if err != nil {
		return nil, fmt.Errorf("attach fentry/generic_file_read_iter: %w", err)
	}
	return l, nil
}

// ReadPattern holds the per-ino access pattern snapshot read from the
// per-CPU slots of s3fs_ino_slots, merged across CPUs (see
// mergeReadPatterns).  SeqRatio returns the fraction of sequential reads
// (0.0–1.0).
type ReadPattern struct {
	LastEndPos uint64
	SeqCount   uint64
	RandCount  uint64
	TotalBytes uint64
}

// SeqRatio returns the fraction of sequential reads [0.0, 1.0].
// Returns 0.0 when no reads have been recorded.
func (rp ReadPattern) SeqRatio() float64 {
	total := rp.SeqCount + rp.RandCount
	if total == 0 {
		return 0.0
	}
	return float64(rp.SeqCount) / float64(total)
}

// slotBits and slotIndex mirror S3FS_SLOT_BITS / s3fs_slot_index in
// include/s3fs_shared_types.h: the fentry keeps an inode's read pattern in
// slot slotIndex(ino) of a per-CPU array, so a lookup is one array read.
const slotBits = 10

func slotIndex(ino uint64) uint32 {
	return uint32((ino * 0x9E3779B97F4A7C15) >> (64 - slotBits))
}

// ReadAccessPattern looks up the access pattern for ino from s3fs_ino_slots.
// Returns (zero, false) when no CPU's slot is currently owned by ino — it was
// not seen by fentry/generic_file_read_iter yet, or another inode took the
// slot over.
func (p *Programs) ReadAccessPattern(ino uint64) (ReadPattern, bool) {
	var vs []s3fsfpS3fsInoSlot
	key := slotIndex(ino)
	if err := p.Objs.S3fsInoSlots.Lookup(&key, &vs); err != nil {
		return ReadPattern{}, false
	}
	return mergeReadPatterns(ino, vs)
}

// mergeReadPatterns folds the per-CPU slots (AC-47: s3fs_ino_slots, a
// direct-mapped PERCPU_ARRAY) that belong to ino into one pattern: counters
// are summed (in 64 bits, so many CPUs' u32 counts cannot wrap), and
// LastEndPos is the furthest end any CPU has read — the sequential frontier
// the prefetch loop extends.  ok is false when no slot is owned by ino.
func mergeReadPatterns(ino uint64, vs []s3fsfpS3fsInoSlot) (rp ReadPattern, ok bool) {
	for _, v := range vs {
		if v.Ino != ino {
			continue
		}
		ok = true
		rp.SeqCount += uint64(v.SeqCount)
		rp.RandCount += uint64(v.RandCount)
		rp.TotalBytes += v.TotalBytes
		if v.LastEndPos > rp.LastEndPos {
			rp.LastEndPos = v.LastEndPos
		}
	}
	return rp, ok
}

// rejectDemoObjs targets exactly the reject demo program and its arena
// map dependency. LoadAndAssign will load both (and any maps the
// program references) without dragging in the healthy programs.
type rejectDemoObjs struct {
	S3fsDeepWalkReject *ebpf.Program `ebpf:"s3fs_deep_walk_reject"`
	S3fsArena          *ebpf.Map     `ebpf:"s3fs_arena"`
}

func (o *rejectDemoObjs) Close() error {
	return _S3fsfpClose(o.S3fsDeepWalkReject, o.S3fsArena)
}

// LoadRejectDemo instantiates ONLY the intentionally verifier-rejected
// s3fs_deep_walk_reject program and returns its VerifierError with the
// full kernel log attached. Returns (nil, nil) if the program somehow
// loaded — a future kernel might learn to accept it, in which case the
// demo is obsolete and the caller should say so.
//
// This is deliberately separate from Load(): the reject program is
// designed to fail, so folding it into the runtime load path would
// break every mount. We must mmap the arena before load (same reason
// as Load(): the verifier refuses bpf_addr_space_cast until the
// arena's user_vm_start is set).
func LoadRejectDemo() (*ebpf.VerifierError, error) {
	if err := rlimit.RemoveMemlock(); err != nil {
		return nil, fmt.Errorf("remove memlock: %w", err)
	}
	spec, err := loadS3fsfp()
	if err != nil {
		return nil, fmt.Errorf("parse bpf spec: %w", err)
	}
	arenaSpec, ok := spec.Maps["s3fs_arena"]
	if !ok {
		return nil, fmt.Errorf("s3fs_arena map missing from spec")
	}
	arenaMap, err := ebpf.NewMap(arenaSpec)
	if err != nil {
		return nil, fmt.Errorf("create arena map: %w", err)
	}
	defer arenaMap.Close()
	a, err := arena.Wrap(arenaMap)
	if err != nil {
		return nil, fmt.Errorf("mmap arena: %w", err)
	}
	defer a.Close()

	opts := &ebpf.CollectionOptions{
		MapReplacements: map[string]*ebpf.Map{"s3fs_arena": arenaMap},
	}
	var objs rejectDemoObjs
	loadErr := spec.LoadAndAssign(&objs, opts)
	if loadErr == nil {
		objs.Close()
		return nil, nil
	}
	var ve *ebpf.VerifierError
	if errors.As(loadErr, &ve) {
		return ve, nil
	}
	return nil, fmt.Errorf("load reject demo (non-verifier error): %w", loadErr)
}

// InodeStub mirrors `struct s3fs_inode_stub` in bpf/arena_types.h.
// Layout is packed to match the BPF-side struct byte-for-byte.
type InodeStub struct {
	Ino     uint64
	Size    uint64
	MtimeNs uint64
	Mode    uint32
	Pad     uint32
}

// stubSize matches sizeof(struct s3fs_inode_stub).
const stubSize = 32

// WarmCache writes stub into the arena at the next free slot and
// installs an ino → offset mapping in s3fs_inode_index so subsequent
// fentry/vfs_getattr_nosec hits on that ino short-circuit as fast_hits
// instead of firing another miss.
//
// Returns the arena offset the stub was written at. Returns
// ErrCacheFull if the arena has no more slots. Callers that receive
// ErrCacheFull can drop the entry — the fentry hook keeps emitting
// misses and counters keep incrementing, which is the honest signal
// that the cache is undersized.
func (p *Programs) WarmCache(stub InodeStub) (uint64, error) {
	p.cacheMu.Lock()
	defer p.cacheMu.Unlock()

	if existing, ok := p.cacheKnown[stub.Ino]; ok {
		return existing, nil
	}

	arenaSize := p.arena.Size()
	nextOff := p.cacheNextOff
	if nextOff+stubSize > arenaSize {
		return 0, ErrCacheFull
	}

	buf := make([]byte, stubSize)
	binary.LittleEndian.PutUint64(buf[0:8], stub.Ino)
	binary.LittleEndian.PutUint64(buf[8:16], stub.Size)
	binary.LittleEndian.PutUint64(buf[16:24], stub.MtimeNs)
	binary.LittleEndian.PutUint32(buf[24:28], stub.Mode)
	binary.LittleEndian.PutUint32(buf[28:32], stub.Pad)
	if _, err := p.arena.WriteAt(buf, int64(nextOff)); err != nil {
		return 0, fmt.Errorf("arena write ino=%d: %w", stub.Ino, err)
	}

	// S3fsInodeIndex is a __u32 BPF map; cast is safe since the arena is
	// bounded at 128 MiB (well within uint32 range).
	off32 := uint32(nextOff)
	if err := p.Objs.S3fsInodeIndex.Update(&stub.Ino, &off32, 0); err != nil {
		return 0, fmt.Errorf("inode_index update ino=%d: %w", stub.Ino, err)
	}

	p.cacheKnown[stub.Ino] = nextOff
	p.cacheNextOff = nextOff + stubSize
	return nextOff, nil
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

// ArenaFD returns the file descriptor of the underlying arena map.
// Used by the daemon to hand kmod an arena reference over the
// /dev/s3fs_ebpf admit-arena ioctl.
func (p *Programs) ArenaFD() int {
	return p.arena.Map.FD()
}

// IsWarmed reports whether ino has already been admitted to the cache.
// Callers use this to short-circuit an S3 round-trip when a burst of
// queued miss events all reference the same ino (the ringbuf drains
// after the fentry hook fires several times before the daemon catches
// up).
func (p *Programs) IsWarmed(ino uint64) bool {
	p.cacheMu.Lock()
	defer p.cacheMu.Unlock()
	_, ok := p.cacheKnown[ino]
	return ok
}

// KnownInos returns a snapshot of all inos currently admitted to the
// arena cache. Ordering is undefined. Used by CLI subcommands that
// want to report cache state.
func (p *Programs) KnownInos() []uint64 {
	p.cacheMu.Lock()
	defer p.cacheMu.Unlock()
	out := make([]uint64, 0, len(p.cacheKnown))
	for ino := range p.cacheKnown {
		out = append(out, ino)
	}
	return out
}

// ReadCounters returns the aggregated (summed over CPUs) counter value
// for each StatSlot* index. The returned slice has length StatSlotMax.
func (p *Programs) ReadCounters() ([]uint64, error) {
	out := make([]uint64, StatSlotMax)
	perCPU := make([]uint64, 0)
	for slot := uint32(0); slot < StatSlotMax; slot++ {
		if err := p.Objs.S3fsCounters.Lookup(&slot, &perCPU); err != nil {
			return nil, fmt.Errorf("read counter slot %d: %w", slot, err)
		}
		var sum uint64
		for _, v := range perCPU {
			sum += v
		}
		out[slot] = sum
	}
	return out, nil
}

// RunTestShared invokes the SEC("syscall") test harness with the given
// ctx and returns the populated result.
func (p *Programs) RunTestShared(in TestArgs) (TestArgs, uint32, error) {
	out := TestArgs{}
	ret, err := p.Objs.S3fsTestShared.Run(&ebpf.RunOptions{
		Context:    in,
		ContextOut: &out,
	})
	if err != nil {
		return TestArgs{}, 0, fmt.Errorf("BPF_PROG_TEST_RUN: %w", err)
	}
	return out, ret, nil
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

// Close releases all loaded programs, maps, and the arena mmap.
func (p *Programs) Close() error {
	if p == nil {
		return nil
	}
	var errs []error
	if err := p.Objs.Close(); err != nil {
		errs = append(errs, err)
	}
	if p.arena != nil {
		if err := p.arena.Close(); err != nil {
			errs = append(errs, err)
		}
		p.arena = nil
	}
	if len(errs) > 0 {
		return fmt.Errorf("close: %v", errs)
	}
	return nil
}

