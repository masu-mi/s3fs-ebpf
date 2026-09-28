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

// Program bundles a loaded policy program with the map it exports.
// The link is kept alive on Attached; Close() destroys everything.
type Program struct {
	Coll  *ebpf.Collection
	Prog  *ebpf.Program
	Stats *ebpf.Map
	Link  link.Link
	Path  string // source path for logs
}

// Load reads a .bpf.o and returns a ready-to-attach Program. The
// object must have:
//   - one program under SEC("fentry/s3fs_policy_on_admit")
//   - one map named "policy_stats" of type ARRAY with 8-byte values
//
// Missing pieces are surfaced as clear errors so authors of Layer 3
// programs see them at load time (not at attach).
func Load(path string) (*Program, error) {
	spec, err := ebpf.LoadCollectionSpec(path)
	if err != nil {
		return nil, fmt.Errorf("policyprog: parse %s: %w", path, err)
	}
	coll, err := ebpf.NewCollection(spec)
	if err != nil {
		return nil, fmt.Errorf("policyprog: load %s: %w", path, err)
	}
	// Look up by SEC name (bpf2go generates Go names; here we take
	// the raw .bpf.o so program discovery is symbol-driven).
	var prog *ebpf.Program
	for _, p := range coll.Programs {
		if p.Type() == ebpf.Tracing {
			prog = p
			break
		}
	}
	if prog == nil {
		coll.Close()
		return nil, fmt.Errorf("policyprog: %s has no fentry program", path)
	}
	stats, ok := coll.Maps["policy_stats"]
	if !ok {
		coll.Close()
		return nil, fmt.Errorf("policyprog: %s missing 'policy_stats' map", path)
	}
	return &Program{Coll: coll, Prog: prog, Stats: stats, Path: path}, nil
}

// Attach hooks the program to its target (declared via SEC in the
// .bpf.o). fentry programs bind by symbol at load time, so this is
// essentially "start firing".
func (p *Program) Attach() error {
	l, err := link.AttachTracing(link.TracingOptions{Program: p.Prog})
	if err != nil {
		return fmt.Errorf("policyprog: attach %s: %w", p.Path, err)
	}
	p.Link = l
	return nil
}

// Close detaches (if attached) and frees the collection.
func (p *Program) Close() {
	if p == nil {
		return
	}
	if p.Link != nil {
		_ = p.Link.Close()
	}
	if p.Coll != nil {
		p.Coll.Close()
	}
}

// ReadStat returns the current value of one policy_stats slot.
func (p *Program) ReadStat(slot uint32) (uint64, error) {
	var v uint64
	if err := p.Stats.Lookup(slot, &v); err != nil {
		return 0, err
	}
	return v, nil
}

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
