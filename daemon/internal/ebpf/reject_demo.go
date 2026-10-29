package ebpf

import (
	"errors"
	"fmt"

	"github.com/bbsakura/s3fs-ebpf/daemon/internal/arena"
	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/rlimit"
)

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
