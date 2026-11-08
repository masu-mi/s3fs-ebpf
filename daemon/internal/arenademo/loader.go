// Package arenademo wraps the bpf2go-generated objects (arenademo_*) with a
// small Load/Run/Close lifecycle for the bpf_arena + KFunc demos
// (dev test-arena, verifier demo). Not part of the production data path.
package arenademo

import (
	"errors"
	"fmt"

	"github.com/bbsakura/s3fs-ebpf/daemon/internal/arena"
	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/rlimit"
)

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
// The generated arenademoObjects also includes S3fsDeepWalkReject — a
// deliberately verifier-rejected program used only by the verifier-demo
// CLI. Referencing it here would drag it into every LoadAndAssign and
// break runtime paths, so we mirror only the healthy programs and let
// the demo path load its own program by name.
type runtimeObjs struct {
	// Programs.
	S3fsTestShared    *ebpf.Program `ebpf:"s3fs_test_shared"`
	S3fsArenaPrealloc *ebpf.Program `ebpf:"s3fs_arena_prealloc"`

	// Maps.
	S3fsArena      *ebpf.Map `ebpf:"s3fs_arena"`
}

func (o *runtimeObjs) Close() error {
	return _ArenademoClose(
		o.S3fsTestShared, o.S3fsArenaPrealloc,
		o.S3fsArena,
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
}

// Load pulls the bpf2go collection into the kernel and returns a handle.
// Removes the memlock rlimit before load (required on older kernels).
func Load() (*Programs, error) {
	if err := rlimit.RemoveMemlock(); err != nil {
		return nil, fmt.Errorf("remove memlock: %w", err)
	}

	spec, err := loadArenademo()
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

	p := &Programs{arena: a}
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

// ArenaFD returns the file descriptor of the underlying arena map.
// Used by the daemon to hand kmod an arena reference over the
// /dev/s3fs_ebpf admit-arena ioctl.
func (p *Programs) ArenaFD() int {
	return p.arena.Map.FD()
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
