// Package policyprog is the Layer 2 / Layer 3 eBPF program loader
// for Week 4 (d). Layer 2 = engine-shipped .bpf.o under bpf/, Layer 3
// = user-supplied .bpf.o loaded via `--policy-bpf file:<path>`.
//
// Both paths converge on the same shape: a collection with a
// SEC("fentry/s3fs_policy_on_admit") program and a
// BPF_MAP_TYPE_ARRAY named policy_stats indexed by
// PSTAT_INVOCATIONS/LAST_COUNT/LAST_HINT. Users writing Layer 3
// programs replicate this shape.
package policyprog

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
)

// Slot indices — must match bpf/policy_observe.bpf.c PSTAT_*.
const (
	StatInvocations = 0
	StatLastCount   = 1
	StatLastHint    = 2
)

// Program bundles a loaded policy program set (a .bpf.o may declare
// multiple hooks — on_admit / on_evict / on_pressure). All fentry
// programs are attached on Attach(); links stay alive until Close().
type Program struct {
	Coll  *ebpf.Collection
	Progs map[string]*ebpf.Program // ebpf-section-name → program
	Stats *ebpf.Map                // shared policy_stats map (may be nil)
	Links []link.Link              // one per attached fentry
	Path  string                   // source path for logs
}

// Load reads a .bpf.o and returns a ready-to-attach Program. The
// object may declare 1..N fentry programs (all attached on Attach())
// and should expose a "policy_stats" ARRAY map for daemon-side
// metric mirroring (optional but conventional).
//
// Missing map is a warning; missing programs is an error.
func Load(path string) (*Program, error) {
	spec, err := ebpf.LoadCollectionSpec(path)
	if err != nil {
		return nil, fmt.Errorf("policyprog: parse %s: %w", path, err)
	}
	coll, err := ebpf.NewCollection(spec)
	if err != nil {
		return nil, fmt.Errorf("policyprog: load %s: %w", path, err)
	}
	progs := make(map[string]*ebpf.Program)
	for name, p := range coll.Programs {
		if p.Type() == ebpf.Tracing {
			progs[name] = p
		}
	}
	if len(progs) == 0 {
		coll.Close()
		return nil, fmt.Errorf("policyprog: %s has no fentry programs", path)
	}
	stats := coll.Maps["policy_stats"] // may be nil (Layer 3 programs may skip)
	return &Program{Coll: coll, Progs: progs, Stats: stats, Path: path}, nil
}

// Attach hooks every fentry program to its target (declared via SEC in
// the .bpf.o). Programs bind by symbol at load time; attach just
// activates them.
func (p *Program) Attach() error {
	for name, prog := range p.Progs {
		l, err := link.AttachTracing(link.TracingOptions{Program: prog})
		if err != nil {
			// Partial attach: unwind the ones we already attached.
			for _, done := range p.Links {
				_ = done.Close()
			}
			p.Links = nil
			return fmt.Errorf("policyprog: attach %s program %s: %w",
				p.Path, name, err)
		}
		p.Links = append(p.Links, l)
	}
	return nil
}

// Close detaches (if attached) and frees the collection.
func (p *Program) Close() {
	if p == nil {
		return
	}
	for _, l := range p.Links {
		_ = l.Close()
	}
	p.Links = nil
	if p.Coll != nil {
		p.Coll.Close()
	}
}

// ReadStat returns the current value of one policy_stats slot.
// Returns (0, nil) when the program didn't declare the shared map.
func (p *Program) ReadStat(slot uint32) (uint64, error) {
	if p.Stats == nil {
		return 0, nil
	}
	var v uint64
	if err := p.Stats.Lookup(slot, &v); err != nil {
		return 0, err
	}
	return v, nil
}

// AttachedCount returns how many fentry links are live (useful for
// smoke-verifying multi-hook programs like S3-FIFO).
func (p *Program) AttachedCount() int { return len(p.Links) }

// Resolve turns a --policy-bpf argument into a concrete .bpf.o path.
//
// Supported forms:
//   - "builtin:<name>"  → repo's bpf/<name>.bpf.o (Layer 2)
//   - "file:<path>"     → arbitrary path (Layer 3, user-supplied)
//   - "<path>"          → treated as file: shortcut
//   - "" (empty)        → no program, returns ("", nil)
//
// `repoRoot` is the project root (usually TOPDIR) so builtin lookup
// finds bpf/*.bpf.o without hard-coding an absolute path.
func Resolve(arg, repoRoot string) (string, error) {
	if arg == "" {
		return "", nil
	}
	const (
		builtinPrefix = "builtin:"
		filePrefix    = "file:"
	)
	switch {
	case len(arg) > len(builtinPrefix) && arg[:len(builtinPrefix)] == builtinPrefix:
		name := arg[len(builtinPrefix):]
		path := filepath.Join(repoRoot, "bpf", name+".bpf.o")
		if _, err := os.Stat(path); err != nil {
			return "", fmt.Errorf("policyprog: builtin %q not found at %s", name, path)
		}
		return path, nil
	case len(arg) > len(filePrefix) && arg[:len(filePrefix)] == filePrefix:
		return arg[len(filePrefix):], nil
	default:
		return arg, nil
	}
}

// Sentinel: no-op program (for tests / offline mode).
var ErrNoProgram = errors.New("no policy program specified")
