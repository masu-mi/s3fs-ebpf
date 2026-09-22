// Package ebpf wraps the bpf2go-generated objects (s3fsfp_*) with a
// small Load/Run/Close lifecycle for the daemon.
package ebpf

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sync"

	"github.com/bbsakura/s3fs-ebpf/daemon/internal/arena"
	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/rlimit"
)

// StatSlot indices mirror `enum s3fs_stat_slot` in
// include/s3fs_shared_types.h. Keep in sync.
const (
	StatFastHits   uint32 = 0
	StatSlowMisses uint32 = 1
	StatUnknownFS  uint32 = 2
	StatSlotMax    uint32 = 3
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
	S3fsTestShared    *ebpf.Program `ebpf:"s3fs_test_shared"`
	S3fsArenaPrealloc *ebpf.Program `ebpf:"s3fs_arena_prealloc"`

	// Maps.
	S3fsArena      *ebpf.Map `ebpf:"s3fs_arena"`
	S3fsCounters   *ebpf.Map `ebpf:"s3fs_counters"`
	S3fsEvents     *ebpf.Map `ebpf:"s3fs_events"`
	S3fsInodeIndex *ebpf.Map `ebpf:"s3fs_inode_index"`
}

func (o *runtimeObjs) Close() error {
	return _S3fsfpClose(
		o.S3fsFpGetattr, o.S3fsTestShared, o.S3fsArenaPrealloc,
		o.S3fsArena, o.S3fsCounters, o.S3fsEvents, o.S3fsInodeIndex,
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
	// allocator into the arena (no eviction — bounded by arena size).
	cacheMu      sync.Mutex
	cacheKnown   map[uint64]uint32
	cacheNextOff uint32
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

	p := &Programs{arena: a, cacheKnown: make(map[uint64]uint32)}
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
func (p *Programs) WarmCache(stub InodeStub) (uint32, error) {
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

	off := nextOff
	if err := p.Objs.S3fsInodeIndex.Update(&stub.Ino, &off, 0); err != nil {
		return 0, fmt.Errorf("inode_index update ino=%d: %w", stub.Ino, err)
	}

	p.cacheKnown[stub.Ino] = off
	p.cacheNextOff = nextOff + stubSize
	return off, nil
}

// ErrCacheFull signals the arena has no more slots for InodeStubs.
var ErrCacheFull = errors.New("arena cache full")

// AdmitContent writes `data` into the arena at the next free slot and
// returns its byte offset. Intended for the Week 4 (a) admit-arena
// ioctl: kmod's .read serves these bytes directly out of the arena
// page instead of from a per-inode kmalloc buffer. Shares the same
// bump allocator as WarmCache; callers should call WarmCache first if
// they want stubs at low offsets and content behind them.
func (p *Programs) AdmitContent(data []byte) (uint32, error) {
	p.cacheMu.Lock()
	defer p.cacheMu.Unlock()

	arenaSize := p.arena.Size()
	nextOff := p.cacheNextOff
	if uint64(nextOff)+uint64(len(data)) > uint64(arenaSize) {
		return 0, ErrCacheFull
	}
	if len(data) > 0 {
		if _, err := p.arena.WriteAt(data, int64(nextOff)); err != nil {
			return 0, fmt.Errorf("arena write content: %w", err)
		}
	}
	off := nextOff
	p.cacheNextOff = nextOff + uint32(len(data))
	return off, nil
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

