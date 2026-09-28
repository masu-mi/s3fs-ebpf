// Package policy is the CEL-based policy engine spike (Week 4 (d)
// P-01). Minimum viable path: load one .cel file at daemon start,
// evaluate the `prewarm` expression per Entry, return a bool. The
// wider architecture (`docs/policy-engine-design.md §4.5`: DSL Layer 1
// + built-in eBPF Layer 2 + user eBPF Layer 3 + KFunc Layer 0) grows
// from here — additional hooks reuse the same `cel.Env` shape.
//
// Contract: policy authors write CEL expressions that take an `entry`
// variable (fields listed in EntryInput below) and return `bool`. A
// missing file, empty policy, or an eval error falls back to true
// (prewarm) so a broken policy doesn't strand the mount cold.
package policy

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"cel.dev/cel-go/cel"
	"cel.dev/cel-go/common/types"
	"cel.dev/cel-go/common/types/ref"

	"github.com/bbsakura/s3fs-ebpf/daemon/internal/metrics"
)

// EntryInput is the shape the CEL expression sees as `entry`. Kept
// small and stable — adding a field is a UAPI-shaped change for
// policy authors. Fields mirror daemon/internal/ebpf.Entry.
type EntryInput struct {
	Ino     uint64 // stable within a daemon run
	Name    string // dentry basename
	S3Key   string // full S3 key (may contain '/')
	Size    uint64 // S3 object size in bytes (Fallback field)
	Mode    uint32 // Unix mode bits, S_IFREG | 0644 etc.
}

// Engine holds compiled programs for each declared hook. Currently
// `prewarm` (per-entry bool) and `policy` (global string returning a
// Layer 2 program name / builtin:/file: URI) are wired.
type Engine struct {
	mu           sync.RWMutex
	source       string      // originating file path, for logs
	prewarm      cel.Program // may be nil (policy did not declare it)
	policyName   cel.Program // returns string; global scope, no entry
}

// Load reads a CEL policy from `path`. Format: newline-separated
// hook declarations, each of the form:
//
//	prewarm: <cel expression returning bool>
//
// Comments start with `#`. Blank lines allowed. Unknown hook names
// are ignored with a log line (forward-compat when older engines
// meet newer policies).
//
// Returns a working Engine with prewarm=nil when the file exists
// but declares nothing — the daemon's default (prewarm=true) then
// applies. Returns error only on parse/compile failure.
func Load(path string) (*Engine, error) {
	if path == "" {
		return &Engine{}, nil // no policy → default behavior everywhere
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("policy: read %s: %w", path, err)
	}
	env, err := buildEnv()
	if err != nil {
		return nil, fmt.Errorf("policy: build env: %w", err)
	}
	eng := &Engine{source: filepath.Base(path)}
	// tiny hand-rolled parser — full YAML/HCL is overkill for one hook.
	for lineno, line := range splitLines(raw) {
		trim := trimSpace(line)
		if trim == "" || trim[0] == '#' {
			continue
		}
		name, expr, ok := cutColon(trim)
		if !ok {
			return nil, fmt.Errorf("policy: %s:%d syntax: expected 'hook: expr'", path, lineno+1)
		}
		prog, err := compileHook(env, expr)
		if err != nil {
			return nil, fmt.Errorf("policy: %s:%d compile %s: %w", path, lineno+1, name, err)
		}
		switch name {
		case "prewarm":
			eng.prewarm = prog
		case "policy":
			eng.policyName = prog
		default:
			fmt.Fprintf(os.Stderr, "[policy] %s:%d ignoring unknown hook %q\n",
				path, lineno+1, name)
		}
	}
	return eng, nil
}

// EvalPrewarm returns true when the entry should be prewarmed. Default
// (no policy declared, eval error, non-bool result) is true — the
// engine leans toward "prewarm to be safe" so a broken policy doesn't
// strand the mount cold.
func (e *Engine) EvalPrewarm(in EntryInput) bool {
	e.mu.RLock()
	prog := e.prewarm
	e.mu.RUnlock()
	if prog == nil {
		return true
	}
	out, _, err := prog.Eval(map[string]any{
		"entry": map[string]any{
			"ino":    in.Ino,
			"name":   in.Name,
			"s3_key": in.S3Key,
			"size":   in.Size,
			"mode":   uint64(in.Mode),
		},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "[policy] %s prewarm eval ino=%d: %v (defaulting true)\n",
			e.source, in.Ino, err)
		return true
	}
	b, ok := out.(types.Bool)
	if !ok {
		fmt.Fprintf(os.Stderr, "[policy] %s prewarm ino=%d: non-bool result %v (defaulting true)\n",
			e.source, in.Ino, out.Value())
		return true
	}
	return bool(b)
}

// Source returns the loaded policy's file basename, for log context.
func (e *Engine) Source() string { return e.source }

// EvalPolicyName returns the CEL-selected policy program name (a
// builtin:<name> / file:<path> URI, or a bare name that Resolve
// treats as builtin). Empty string means "no policy: hook declared,
// caller should fall back to CLI flag / no policy".
func (e *Engine) EvalPolicyName() string {
	e.mu.RLock()
	prog := e.policyName
	e.mu.RUnlock()
	if prog == nil {
		return ""
	}
	out, _, err := prog.Eval(map[string]any{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "[policy] %s policy eval: %v (using empty)\n",
			e.source, err)
		return ""
	}
	s, ok := out.(types.String)
	if !ok {
		fmt.Fprintf(os.Stderr, "[policy] %s policy: non-string result %v\n",
			e.source, out.Value())
		return ""
	}
	return string(s)
}

// -- internals ---------------------------------------------------------

func buildEnv() (*cel.Env, error) {
	return cel.NewEnv(
		cel.Variable("entry", cel.MapType(cel.StringType, cel.DynType)),
		// metric(name, value) — side-effect: publish `value` as a
		// gauge under `s3fs_ebpf_policy_metric{name=<name>}`. Always
		// returns true so it can chain in `&&` expressions:
		//
		//   prewarm: metric("eligible_bytes", entry.size) &&
		//            entry.size <= 33554432
		//
		// dyn value → float64: accepts int, uint, or double from CEL.
		// Names past metrics.CustomGaugeCap are silently dropped.
		cel.Function("metric",
			cel.Overload("metric_string_dyn",
				[]*cel.Type{cel.StringType, cel.DynType},
				cel.BoolType,
				cel.BinaryBinding(metricFn),
			),
		),
	)
}

// metricFn is the CEL bridge to metrics.RecordCustom. Extracts a
// float64 from any numeric CEL value (int/uint/double) and drops the
// rest with a stderr note. Returns True unconditionally so an
// error-shaped drop still lets the surrounding expression proceed.
func metricFn(nameVal, valueVal ref.Val) ref.Val {
	name, ok := nameVal.(types.String)
	if !ok {
		return types.True
	}
	var f float64
	switch v := valueVal.(type) {
	case types.Int:
		f = float64(v)
	case types.Uint:
		f = float64(v)
	case types.Double:
		f = float64(v)
	default:
		return types.True
	}
	metrics.RecordCustom(string(name), f)
	return types.True
}

func compileHook(env *cel.Env, expr string) (cel.Program, error) {
	ast, issues := env.Compile(expr)
	if issues != nil && issues.Err() != nil {
		return nil, issues.Err()
	}
	return env.Program(ast)
}

func splitLines(b []byte) []string {
	var out []string
	start := 0
	for i, c := range b {
		if c == '\n' {
			out = append(out, string(b[start:i]))
			start = i + 1
		}
	}
	if start < len(b) {
		out = append(out, string(b[start:]))
	}
	return out
}

func trimSpace(s string) string {
	i, j := 0, len(s)
	for i < j && (s[i] == ' ' || s[i] == '\t') {
		i++
	}
	for j > i && (s[j-1] == ' ' || s[j-1] == '\t' || s[j-1] == '\r') {
		j--
	}
	return s[i:j]
}

func cutColon(s string) (name, expr string, ok bool) {
	for i, c := range s {
		if c == ':' {
			return trimSpace(s[:i]), trimSpace(s[i+1:]), true
		}
	}
	return "", "", false
}
