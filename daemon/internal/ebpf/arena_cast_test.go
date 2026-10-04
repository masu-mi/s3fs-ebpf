package ebpf

import (
	"testing"

	"github.com/cilium/ebpf/asm"
)

// TestEmbeddedObjectHasArenaCasts guards the committed bpf2go objects
// (s3fsfp_bpf{el,eb}.o) against being regenerated with clang < 19.
//
// Older clang does not define __BPF_FEATURE_ADDR_SPACE_CAST, so
// bpf/arena_types.h silently falls back to plain pointers: the object
// still builds, but every __arena dereference becomes a scalar load and
// the verifier rejects the program at load time ("R2 invalid mem access
// 'scalar'" in `make test-arena`). With clang 19+ each arena deref is
// preceded by bpf_addr_space_cast (ALU64 MOV|X with Offset=1).
//
// Regenerate with `make daemon-gen` (it refuses clang < 19).
func TestEmbeddedObjectHasArenaCasts(t *testing.T) {
	spec, err := loadS3fsfp()
	if err != nil {
		t.Fatalf("loadS3fsfp: %v", err)
	}

	casts := 0
	for _, prog := range spec.Programs {
		for _, ins := range prog.Instructions {
			if ins.OpCode.Class() == asm.ALU64Class &&
				ins.OpCode.ALUOp() == asm.Mov &&
				ins.OpCode.Source() == asm.RegSource &&
				ins.Offset == 1 {
				casts++
			}
		}
	}
	if casts == 0 {
		t.Fatal("no bpf_addr_space_cast in the embedded BPF object: it was " +
			"generated with clang < 19 (__arena fell back to plain pointers). " +
			"Run `make daemon-gen` with clang >= 19 (see `make doctor`, C-1).")
	}

	// s3fs_test_shared (make test-arena subtest B) dereferences an __arena
	// pointer, so it must carry its own cast.
	p := spec.Programs["s3fs_test_shared"]
	if p == nil {
		t.Fatal("program s3fs_test_shared missing from embedded object")
	}
	found := false
	for _, ins := range p.Instructions {
		if ins.OpCode.ALUOp() == asm.Mov && ins.OpCode.Source() == asm.RegSource && ins.Offset == 1 {
			found = true
			break
		}
	}
	if !found {
		t.Error("s3fs_test_shared has no bpf_addr_space_cast")
	}
}
