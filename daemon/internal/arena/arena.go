// Package arena manages the bpf_arena map lifecycle: create the arena,
// mmap it into the daemon, expose the map id we pass to the kernel
// module via `mount -o map_id=...`.
package arena

import "errors"

// Arena is a handle to the bpf_arena mapping shared with the kernel.
type Arena struct {
	MapID uint32
	Size  uint64
}

// ErrNotImplemented is returned until Week 1 wires cilium/ebpf.
var ErrNotImplemented = errors.New("bpf_arena not implemented in Week 0 skeleton")

// New creates a bpf_arena of `sizePages` pages and mmap()s it.
func New(sizePages uint32) (*Arena, error) {
	return nil, ErrNotImplemented
}

// Close unmaps and releases the arena.
func (a *Arena) Close() error {
	return nil
}
