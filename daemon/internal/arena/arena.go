// Package arena wraps a BPF_MAP_TYPE_ARENA map: create, mmap, and hand
// out a byte-addressable view. Requires Linux 6.9+.
package arena

import (
	"fmt"

	"github.com/cilium/ebpf"
)

const (
	// PageSize is the arena's page size (matches PAGE_SIZE on the host).
	PageSize = 4096
)

// Arena is a handle to a bpf_arena map together with its userspace mapping.
type Arena struct {
	Map    *ebpf.Map
	Memory *ebpf.Memory
}

// New allocates a bpf_arena with `pages` 4 KiB pages and mmaps it.
func New(pages uint32) (*Arena, error) {
	spec := &ebpf.MapSpec{
		Name:       "s3fs_arena",
		Type:       ebpf.Arena,
		MaxEntries: pages,
		Flags:      0x400, /* BPF_F_MMAPABLE */
	}
	m, err := ebpf.NewMap(spec)
	if err != nil {
		return nil, fmt.Errorf("create arena map (%d pages): %w", pages, err)
	}

	mem, err := m.Memory()
	if err != nil {
		m.Close()
		return nil, fmt.Errorf("mmap arena: %w", err)
	}
	return &Arena{Map: m, Memory: mem}, nil
}

// WriteAt writes p into the arena at the given byte offset.
func (a *Arena) WriteAt(p []byte, off int64) (int, error) {
	if a == nil || a.Memory == nil {
		return 0, fmt.Errorf("arena not initialised")
	}
	return a.Memory.WriteAt(p, off)
}

// ReadAt reads len(p) bytes from the arena at the given byte offset.
func (a *Arena) ReadAt(p []byte, off int64) (int, error) {
	if a == nil || a.Memory == nil {
		return 0, fmt.Errorf("arena not initialised")
	}
	return a.Memory.ReadAt(p, off)
}

// Size returns the mmap'd region size in bytes.
func (a *Arena) Size() uint32 {
	if a == nil || a.Memory == nil {
		return 0
	}
	return a.Memory.Size()
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

// Close unmaps and releases the arena.
func (a *Arena) Close() error {
	if a == nil {
		return nil
	}
	var err error
	if a.Map != nil {
		err = a.Map.Close()
	}
	return err
}
