// Package arena wraps a BPF_MAP_TYPE_ARENA map: create (or attach to an
// existing one), mmap, and hand out both a byte-addressable view and the
// user-space base address so BPF programs can reconstruct arena pointers.
// Requires Linux 6.9+.
package arena

import (
	"fmt"
	"os"
	"unsafe"

	"github.com/cilium/ebpf"
	"golang.org/x/sys/unix"
)

const (
	// PageSize is the arena's page size (matches PAGE_SIZE on the host).
	PageSize = 4096

	// BPF_F_MMAPABLE — mirrors the flag from <uapi/linux/bpf.h>. Kept
	// local to avoid pulling the whole sys package.
	flagMmapable = 0x400
)

// Arena is a handle to a bpf_arena map together with its userspace mapping.
//
// The mmap is owned here (not delegated to cilium/ebpf's Map.Memory) so we
// can expose the base user-VM address to BPF programs that need to cast
// (base + offset) into an __arena pointer.
type Arena struct {
	Map    *ebpf.Map
	mem    []byte
	base   uint64
	owned  bool // true when New() created the map (so Close() releases it)
}

// New allocates a bpf_arena with `pages` 4 KiB pages and mmaps it.
func New(pages uint32) (*Arena, error) {
	spec := &ebpf.MapSpec{
		Name:       "s3fs_arena",
		Type:       ebpf.Arena,
		MaxEntries: pages,
		Flags:      flagMmapable,
	}
	m, err := ebpf.NewMap(spec)
	if err != nil {
		return nil, fmt.Errorf("create arena map (%d pages): %w", pages, err)
	}

	a, err := wrap(m, true)
	if err != nil {
		m.Close()
		return nil, err
	}
	return a, nil
}

// Wrap takes an existing bpf_arena map and mmaps it. Ownership of the
// map transfers to the Arena: Close() releases both the mmap and the map
// fd. When the caller also hands the map to cilium/ebpf via
// CollectionOptions.MapReplacements, cilium/ebpf clones the map first, so
// the two lifecycles remain independent.
func Wrap(m *ebpf.Map) (*Arena, error) {
	return wrap(m, true)
}

func wrap(m *ebpf.Map, owned bool) (*Arena, error) {
	if m == nil {
		return nil, fmt.Errorf("nil map")
	}
	if m.Type() != ebpf.Arena {
		return nil, fmt.Errorf("map type %s is not Arena", m.Type())
	}

	info, err := m.Info()
	if err != nil {
		return nil, fmt.Errorf("map info: %w", err)
	}
	size := int(info.MaxEntries) * os.Getpagesize()
	if size == 0 {
		return nil, fmt.Errorf("arena has zero pages")
	}

	b, err := unix.Mmap(m.FD(), 0, size,
		unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		return nil, fmt.Errorf("mmap arena (%d bytes): %w", size, err)
	}

	return &Arena{
		Map:   m,
		mem:   b,
		base:  uint64(uintptr(unsafe.Pointer(&b[0]))),
		owned: owned,
	}, nil
}

// WriteAt writes p into the arena at the given byte offset. The first
// access to each page fault-allocates it in the arena.
func (a *Arena) WriteAt(p []byte, off int64) (int, error) {
	if a == nil || a.mem == nil {
		return 0, fmt.Errorf("arena not initialised")
	}
	if off < 0 || off >= int64(len(a.mem)) {
		return 0, fmt.Errorf("arena write offset %d out of range [0,%d)", off, len(a.mem))
	}
	n := copy(a.mem[off:], p)
	if n < len(p) {
		return n, fmt.Errorf("arena short write: %d/%d", n, len(p))
	}
	return n, nil
}

// ReadAt reads len(p) bytes from the arena at the given byte offset.
func (a *Arena) ReadAt(p []byte, off int64) (int, error) {
	if a == nil || a.mem == nil {
		return 0, fmt.Errorf("arena not initialised")
	}
	if off < 0 || off >= int64(len(a.mem)) {
		return 0, fmt.Errorf("arena read offset %d out of range [0,%d)", off, len(a.mem))
	}
	n := copy(p, a.mem[off:])
	if n < len(p) {
		return n, fmt.Errorf("arena short read: %d/%d", n, len(p))
	}
	return n, nil
}

// Size returns the mmap'd region size in bytes.
func (a *Arena) Size() uint64 {
	if a == nil {
		return 0
	}
	return uint64(len(a.mem))
}

// BaseAddr returns the user-VM start address of the mmap'd arena. BPF
// programs use this to reconstruct __arena pointers as (base + offset).
func (a *Arena) BaseAddr() uint64 {
	if a == nil {
		return 0
	}
	return a.base
}

// MapID returns the kernel's numeric ID for the arena map.
func (a *Arena) MapID() (uint32, error) {
	info, err := a.Map.Info()
	if err != nil {
		return 0, err
	}
	id, ok := info.ID()
	if !ok {
		return 0, fmt.Errorf("map ID unavailable")
	}
	return uint32(id), nil
}

// Close unmaps the arena. If Arena.owned is true (i.e. New() created the
// map), it also releases the map fd.
func (a *Arena) Close() error {
	if a == nil {
		return nil
	}
	var errs []error
	if a.mem != nil {
		if err := unix.Munmap(a.mem); err != nil {
			errs = append(errs, fmt.Errorf("munmap arena: %w", err))
		}
		a.mem = nil
	}
	if a.owned && a.Map != nil {
		if err := a.Map.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close arena map: %w", err))
		}
	}
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("arena close: %v", errs)
}
