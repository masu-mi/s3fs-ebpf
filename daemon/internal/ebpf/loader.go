// Package ebpf wraps the bpf2go-generated objects (s3fsfp_*) with a
// small Load/Run/Close lifecycle for the daemon.
package ebpf

import (
	"fmt"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/rlimit"
)

// TestArgs mirrors `struct s3fs_test_args` in include/s3fs_shared_types.h.
// Layout must stay in sync (`_pad` present to match the C struct).
type TestArgs struct {
	Idx      uint32
	KfuncArg uint32
	OutIno   uint64
	OutKfunc uint32
	Pad      uint32
}

// Programs holds loaded bpf2go objects.
type Programs struct {
	Objs s3fsfpObjects
}

// Load pulls the bpf2go collection into the kernel and returns a handle.
// Removes the memlock rlimit before load (required on older kernels).
func Load() (*Programs, error) {
	if err := rlimit.RemoveMemlock(); err != nil {
		return nil, fmt.Errorf("remove memlock: %w", err)
	}

	p := &Programs{}
	if err := loadS3fsfpObjects(&p.Objs, nil); err != nil {
		return nil, fmt.Errorf("load bpf objects: %w", err)
	}
	return p, nil
}

// SharedMap returns the mmap-able array shared with the daemon.
func (p *Programs) SharedMap() *ebpf.Map {
	return p.Objs.S3fsShared
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

// Close releases all loaded programs and maps.
func (p *Programs) Close() error {
	if p == nil {
		return nil
	}
	return p.Objs.Close()
}
