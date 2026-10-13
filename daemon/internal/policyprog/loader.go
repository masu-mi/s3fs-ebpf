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
	"runtime"
	"strings"
	"unsafe"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"golang.org/x/sys/unix"
)

// Slot indices — must match bpf/policy_observe.bpf.c PSTAT_*.
// pressure_ops-specific slots (Z-04) are only meaningful when that
// policy is loaded; other policies have PSTAT_MAX≤5 so reads of
// slots 5/6 return an out-of-range error and are silently skipped.
const (
	StatInvocations          = 0
	StatLastCount            = 1
	StatLastHint             = 2
	StatPressureStrictTotal  = 5 // Z-04: pressure_ops strict-branch (visited=1, high-pressure → evict) fires
	StatPressureLenientTotal = 6 // Z-04: pressure_ops lenient-branch (visited=1, normal → keep) fires
)

// Program bundles a loaded policy program set. Two shapes are
// supported and auto-detected from the .bpf.o:
//
//   - fentry / fmod_ret programs: individual attach per hook,
//     N Link objects. Tracing links do not implement BPF_LINK_UPDATE
//     (cilium/ebpf returns ErrNotSupported), so hot swap = attach the
//     N new links, then close the N old ones; in between both policies
//     are attached and run.
//   - struct_ops program: single vtable map, 1 Link object,
//     hot swap = 1 BPF_LINK_UPDATE that retargets the existing link at
//     the new vtable map (kmod .update callback = 1 RCU pointer swap).
//     See SwapFrom.
//
// See docs/design/policy-engine-design.md §4.5 for the trade-off analysis.
type Program struct {
	Coll     *ebpf.Collection
	Progs    map[string]*ebpf.Program // ebpf-section-name → program
	Stats    *ebpf.Map                // shared policy_stats map (may be nil)
	Links    []link.Link              // one per attached hook or 1 for struct_ops
	Path     string                   // source path for logs
	IsStruct bool                     // true when a struct_ops map was found
	VTable   *ebpf.Map                // the attached struct_ops map (struct_ops only)
}

// LoadOpts carries optional per-load tuning knobs. Zero value is valid
// and behaves identically to the no-opts path.
type LoadOpts struct {
	// MapMaxEntries overrides max_entries for the named maps before the
	// collection is loaded. Useful for Z-03: scaling the S3-FIFO ghost
	// map proportionally to the workload's admit capacity so promotion
	// fires correctly under large N_FILES workloads. Unknown map names
	// are silently ignored (safe to pass opts that don't apply to the
	// currently loaded policy).
	MapMaxEntries map[string]uint32
}

// Load reads a .bpf.o and returns a ready-to-attach Program. The
// object may declare 1..N fentry programs (all attached on Attach())
// and should expose a "policy_stats" ARRAY map for daemon-side
// metric mirroring (optional but conventional).
//
// Missing map is a warning; missing programs is an error.
func Load(path string, opts ...LoadOpts) (*Program, error) {
	spec, err := ebpf.LoadCollectionSpec(path)
	if err != nil {
		return nil, fmt.Errorf("policyprog: parse %s: %w", path, err)
	}
	if len(opts) > 0 {
		for mapName, maxEntries := range opts[0].MapMaxEntries {
			if ms, ok := spec.Maps[mapName]; ok {
				ms.MaxEntries = maxEntries
			}
		}
	}
	coll, err := ebpf.NewCollection(spec)
	if err != nil {
		return nil, fmt.Errorf("policyprog: load %s: %w", path, err)
	}
	progs := make(map[string]*ebpf.Program)
	isStruct := false
	for name, p := range coll.Programs {
		switch p.Type() {
		case ebpf.Tracing:
			progs[name] = p
		case ebpf.StructOps:
			progs[name] = p
			isStruct = true
		}
	}
	if len(progs) == 0 {
		coll.Close()
		return nil, fmt.Errorf("policyprog: %s has no fentry or struct_ops programs", path)
	}
	stats := coll.Maps["policy_stats"] // may be nil (Layer 3 programs may skip)
	return &Program{Coll: coll, Progs: progs, Stats: stats, Path: path, IsStruct: isStruct}, nil
}

// Attach binds the program(s) to their kmod-side targets. For
// fentry/fmod_ret each program attaches independently; for struct_ops
// the single vtable map attaches once via bpf(BPF_LINK_CREATE) with
// attach_type BPF_STRUCT_OPS. cilium/ebpf abstracts both under
// link.AttachStructOps / link.AttachTracing but you have to pick.
func (p *Program) Attach() error {
	if p.IsStruct {
		return p.attachStructOps()
	}
	return p.attachTracing()
}

func (p *Program) attachTracing() error {
	for name, prog := range p.Progs {
		l, err := link.AttachTracing(link.TracingOptions{Program: prog})
		if err != nil {
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

func (p *Program) attachStructOps() error {
	// struct_ops maps carry the vtable. Find the one named after our
	// kmod-registered type. Instance names come from the BPF source
	// (`struct s3fs_policy_ops <name>_ops = { ... }`, e.g. sieve_ops).
	for name, m := range p.Coll.Maps {
		if m.Type() != ebpf.StructOpsMap {
			continue
		}
		l, err := link.AttachStructOps(link.StructOpsOptions{Map: m})
		if err != nil {
			return fmt.Errorf("policyprog: attach struct_ops map %s: %w",
				name, err)
		}
		p.Links = append(p.Links, l)
		p.VTable = m
		return nil // exactly one vtable per policy
	}
	return fmt.Errorf("policyprog: no struct_ops map in %s", p.Path)
}

// structOpsVTable returns the first struct_ops map in the collection.
func (p *Program) structOpsVTable() *ebpf.Map {
	for _, m := range p.Coll.Maps {
		if m.Type() == ebpf.StructOpsMap {
			return m
		}
	}
	return nil
}

// SwapFrom makes p the active policy in place of old, which must be
// attached. p must be loaded but not yet attached. On success p owns
// the live link(s) and old has none left, so old.Close() only frees
// its collection.
//
// struct_ops → struct_ops: one BPF_LINK_UPDATE on old's link with
// p's vtable map (flags=BPF_F_REPLACE, old map as the expected
// current value). The kernel calls the kmod's .update callback, which
// swaps the RCU pointer once — readers see either the whole old vtable
// or the whole new one. Requires both objects to use
// SEC(".struct_ops.link") (BPF_F_LINK) and kernel 6.4+.
//
// Any other combination (fentry/fmod_ret on either side), or a kernel
// that refuses the update, falls back to attach-new-then-close-old:
// the kmod reg callback publishes the new vtable and the old link's
// unreg sees it was superseded. Returns updated=true when the atomic
// link update path was used.
func (p *Program) SwapFrom(old *Program) (updated bool, err error) {
	if p.IsStruct && old != nil && old.IsStruct && old.VTable != nil && len(old.Links) == 1 {
		if nm := p.structOpsVTable(); nm != nil {
			uerr := updateStructOpsLink(old.Links[0], nm, old.VTable)
			if uerr == nil {
				p.Links, old.Links = old.Links, nil
				p.VTable, old.VTable = nm, nil
				return true, nil
			}
			fmt.Fprintf(os.Stderr,
				"policyprog: BPF_LINK_UPDATE %s → %s failed (%v); falling back to attach + detach\n",
				old.Path, p.Path, uerr)
		}
	}
	if err := p.Attach(); err != nil {
		return false, err
	}
	if old != nil {
		for _, l := range old.Links {
			_ = l.Close()
		}
		old.Links = nil
	}
	return false, nil
}

// updateStructOpsLink issues BPF_LINK_UPDATE for a struct_ops link.
// cilium/ebpf's structOpsLink.Update only accepts *ebpf.Program and
// always returns ErrNotSupported, so the map-flavoured update
// (link_update.new_map_fd / old_map_fd share the prog fd slots) is
// done with a raw bpf(2) call.
func updateStructOpsLink(l link.Link, newMap, oldMap *ebpf.Map) error {
	fl, ok := l.(interface{ FD() int })
	if !ok {
		return fmt.Errorf("link %T exposes no fd", l)
	}
	attr := struct {
		LinkFd   uint32
		NewMapFd uint32
		Flags    uint32
		OldMapFd uint32
	}{
		LinkFd:   uint32(fl.FD()),
		NewMapFd: uint32(newMap.FD()),
		Flags:    unix.BPF_F_REPLACE,
		OldMapFd: uint32(oldMap.FD()),
	}
	_, _, errno := unix.Syscall(unix.SYS_BPF, unix.BPF_LINK_UPDATE,
		uintptr(unsafe.Pointer(&attr)), unsafe.Sizeof(attr))
	runtime.KeepAlive(newMap)
	runtime.KeepAlive(oldMap)
	if errno != 0 {
		return errno
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
	p.VTable = nil
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

// SetConfigU64 writes a u64 value to slot 0 of a named BPF ARRAY map in the
// policy collection. No-op when the map doesn't exist (policy doesn't declare it).
// Used for Z-01 (wtinylfu_config) and similar per-policy tuning knobs.
func (p *Program) SetConfigU64(mapName string, slot uint32, val uint64) error {
	if p.Coll == nil {
		return nil
	}
	m, ok := p.Coll.Maps[mapName]
	if !ok {
		return nil // policy doesn't expose this map — silently ignore
	}
	return m.Update(slot, val, 0 /* BPF_ANY */)
}

// AttachedCount returns how many links are live (useful for
// smoke-verifying multi-hook programs like the freq2bit fmod_ret set).
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
		// Reject path-traversal attempts (e.g. "builtin:../../etc/evil").
		// filepath.Join cleans ".." components, so without this check a
		// crafted name could escape the bpf/ directory entirely.
		if strings.ContainsAny(name, "/\\") || strings.Contains(name, "..") {
			return "", fmt.Errorf("policyprog: invalid builtin name %q (path separators not allowed)", name)
		}
		path := filepath.Clean(filepath.Join(repoRoot, "bpf", name+".bpf.o"))
		// Belt-and-suspenders containment check — the name guard above
		// should catch all cases, but verify the resolved path stays under
		// bpf/ before handing it to the loader.
		bpfDir := filepath.Clean(filepath.Join(repoRoot, "bpf"))
		if !strings.HasPrefix(path, bpfDir+string(filepath.Separator)) {
			return "", fmt.Errorf("policyprog: builtin %q resolves outside bpf/ (%s)", name, path)
		}
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
