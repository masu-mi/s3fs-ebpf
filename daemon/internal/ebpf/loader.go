// Package ebpf wraps the bpf2go-generated objects and exposes a
// Load/Attach lifecycle to the CLI. Week 0: no-op stub that panics if
// invoked, so callers can wire the plumbing without pulling cilium/ebpf
// into the skeleton build.
package ebpf

import "errors"

// Programs is the handle the daemon holds while eBPF is attached.
type Programs struct{}

// ErrNotImplemented is returned until Week 2 wires the real loader.
var ErrNotImplemented = errors.New("ebpf loader not implemented in Week 0 skeleton")

// Load pins the eBPF objects. Skeleton returns ErrNotImplemented.
func Load() (*Programs, error) {
	return nil, ErrNotImplemented
}

// Attach hooks the programs into their targets (fentry/vfs_getattr, ...).
func (p *Programs) Attach() error {
	return ErrNotImplemented
}

// Close detaches programs and closes maps.
func (p *Programs) Close() error {
	return nil
}
