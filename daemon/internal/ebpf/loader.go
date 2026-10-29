// Package ebpf wraps the bpf2go-generated objects (s3fsfp_*) with a
// small Load/Run/Close lifecycle for the daemon.
package ebpf

import (
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
	StatFastHits         uint32 = 0
	StatSlowMisses       uint32 = 1
	StatUnknownFS        uint32 = 2
	StatReadIterHits     uint32 = 3
	StatReadIterMisses   uint32 = 4
	StatReadIterHitsPos0 uint32 = 5 // Y-06: read_iter hit where ki_pos==0 (first read per open)
	StatSlotMax          uint32 = 6
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
	S3fsArena      *ebpf.Map `ebpf:"s3fs_arena"`
	S3fsCounters   *ebpf.Map `ebpf:"s3fs_counters"`
	S3fsEvents     *ebpf.Map `ebpf:"s3fs_events"`
	S3fsInodeIndex *ebpf.Map `ebpf:"s3fs_inode_index"`
	S3fsInoSlots   *ebpf.Map `ebpf:"s3fs_ino_slots"`
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

// ArenaFD returns the file descriptor of the underlying arena map.
// Used by the daemon to hand kmod an arena reference over the
// /dev/s3fs_ebpf admit-arena ioctl.
func (p *Programs) ArenaFD() int {
	return p.arena.Map.FD()
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
