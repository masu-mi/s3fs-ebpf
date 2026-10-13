// s3fs-ebpf CLI entry.
//
// Operational subcommands (top level):
//   mount        - load kmod, load eBPF, create arena, mount FS
//   unmount      - reverse of mount
//   fastpath     - run the fastpath daemon (fentry attach, S3 sync, arena)
//   stats        - print fast/slow path counters
//   policy       - hot-reload the cache policy blob
//   hash-policy  - print SHA256 for a policy .bpf.o (allowlist entry helper)
//   s3-list      - list objects in a bucket
//   s3-stat      - stat a single object
//
// Development / probe / demo subcommands (under "dev"):
//   dev test-arena    - Week 1 bpf_arena + KFunc round-trip probe
//   dev verifier-demo - Week 2 verifier rejection demo
//   dev probe-kvbase  - Week 4 arena ioctl kvbase probe
//   dev microbench    - tight-loop stat/read latency bench
//   dev trace-replay  - offline policy hit-rate simulator
//
// mount/unmount/stats/policy remain stubbed until Week 2+.
package main

import (
	"context"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/bbsakura/s3fs-ebpf/daemon/internal/arena"
	fpebpf "github.com/bbsakura/s3fs-ebpf/daemon/internal/ebpf"
	"github.com/bbsakura/s3fs-ebpf/daemon/internal/gcs"
	"github.com/bbsakura/s3fs-ebpf/daemon/internal/metrics"
	"github.com/bbsakura/s3fs-ebpf/daemon/internal/objstore"
	"github.com/bbsakura/s3fs-ebpf/daemon/internal/policy"
	"github.com/bbsakura/s3fs-ebpf/daemon/internal/policyprog"
	"github.com/bbsakura/s3fs-ebpf/daemon/internal/s3"
	"github.com/cilium/ebpf/ringbuf"
)

const usage = `Usage: s3fs-ebpf <command> [options]

Commands:
  mount        Mount an S3 bucket via the s3fs_ebpf VFS
  unmount      Unmount a previously mounted target
  fastpath     Run the fastpath daemon: attach fentry/vfs_getattr, stream
               ringbuf miss events, sync S3, serve arena content
  stats        Print fast/slow path counters
  policy       Hot-reload the cache policy blob
  hash-policy  Print SHA256 for a user policy .bpf.o (for allowlist entries).
               See --policy-allowlist and policies/allowlist.example.txt.
  s3-list      List objects in a bucket. Backend: --backend s3|gcs.
  s3-stat      Stat a single object.     Backend: --backend s3|gcs.
  dev          Development / probe / demo commands.
               Run "s3fs-ebpf dev" for the full list.

Run "s3fs-ebpf <command> --help" for command-specific flags.
`

const devUsage = `Usage: s3fs-ebpf dev <command> [options]

Dev / probe / demo commands:
  test-arena    Week 1 probe: bpf_arena create + mmap, daemon<->BPF shared
                memory, and KFunc call round-trip.
                Requires Linux 6.9+ and s3fs_kfunc module loaded.
  verifier-demo Week 2 probe: load an intentionally-rejected pure-eBPF arena
                walker and print the verifier's rejection log, then run the
                same walk through the s3fs_kfunc_arena_list_walk KFunc
                (needs bpf/kfunc_walk.bpf.o and s3fs_kfunc loaded).
  probe-kvbase  Week 4 probe: create an arena, ioctl(/dev/s3fs_ebpf,
                S3FS_IOC_PROBE_KVBASE) to read its kernel-VM base, and compare
                it against the user-VM base.
  microbench    Tight-loop stat/read latency bench on a target path.
                Reports min/p50/p95/p99/max (no fork/exec overhead).
  trace-replay  Offline policy simulator (P-04). Feeds a workload trace or a
                synthetic Zipf sequence through Go-side LRU/SIEVE sims and
                reports per-policy hit rates.

Run "s3fs-ebpf dev <command> --help" for command-specific flags.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	sub := os.Args[1]
	args := os.Args[2:]

	switch sub {
	case "mount":
		os.Exit(runMount(args))
	case "unmount", "umount":
		os.Exit(runUnmount(args))
	case "fastpath":
		os.Exit(runFastpath(args))
	case "stats":
		os.Exit(runStats(args))
	case "policy":
		os.Exit(runPolicy(args))
	case "hash-policy":
		os.Exit(runHashPolicy(args))
	case "s3-list":
		os.Exit(runS3List(args))
	case "s3-stat":
		os.Exit(runS3Stat(args))
	case "dev":
		os.Exit(runDev(args))
	case "-h", "--help", "help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n%s", sub, usage)
		os.Exit(2)
	}
}

func runDev(args []string) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, devUsage)
		return 2
	}
	sub := args[0]
	rest := args[1:]
	switch sub {
	case "test-arena":
		return runTestArena(rest)
	case "verifier-demo":
		return runVerifierDemo(rest)
	case "probe-kvbase":
		return runProbeKvbase(rest)
	case "microbench":
		return runMicrobench(rest)
	case "trace-replay":
		return runTraceReplay(rest)
	case "-h", "--help", "help":
		fmt.Print(devUsage)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "unknown dev command: %s\n\n%s", sub, devUsage)
		return 2
	}
}

func runMount(args []string) int {
	fs := flag.NewFlagSet("mount", flag.ExitOnError)
	bucket := fs.String("bucket", "", "S3 bucket name")
	endpoint := fs.String("endpoint", "http://localhost:9000", "S3-compatible endpoint")
	mountPoint := fs.String("mount-point", "/mnt/s3data", "Local mount point")
	cacheDir := fs.String("cache-dir", "/var/cache/s3fs", "Local NVMe tier cache dir")
	policy := fs.String("policy", "", "Optional Lua/WASM policy path")
	fs.Parse(args)

	fmt.Printf("[mount] bucket=%s endpoint=%s target=%s cache=%s policy=%s\n",
		*bucket, *endpoint, *mountPoint, *cacheDir, *policy)
	fmt.Fprintln(os.Stderr, "mount: skeleton only — not implemented yet (see docs/user/motivation.md Week 2)")
	return 0
}

func runUnmount(args []string) int {
	fs := flag.NewFlagSet("unmount", flag.ExitOnError)
	mountPoint := fs.String("mount-point", "/mnt/s3data", "Local mount point")
	fs.Parse(args)
	fmt.Printf("[unmount] target=%s\n", *mountPoint)
	fmt.Fprintln(os.Stderr, "unmount: skeleton only — not implemented yet")
	return 0
}

func runStats(args []string) int {
	fs := flag.NewFlagSet("stats", flag.ExitOnError)
	fs.Parse(args)
	fmt.Println("Fast Path (KFunc/eBPF) Hits: 0")
	fmt.Println("Slow Path (S3 API) Misses  : 0")
	fmt.Println("bpf_arena Memory Usage      : 0 B / 4.0 GB")
	fmt.Println("Active Policy Engine        : (none)")
	return 0
}

func runPolicy(args []string) int {
	fs := flag.NewFlagSet("policy", flag.ExitOnError)
	action := fs.String("action", "load", "load|reload|unload")
	plugin := fs.String("plugin", "", "Path to Lua/WASM policy file")
	fs.Parse(args)
	fmt.Printf("[policy] action=%s plugin=%s\n", *action, *plugin)
	fmt.Fprintln(os.Stderr, "policy: skeleton only — not implemented yet (see docs/user/motivation.md Week 3)")
	return 0
}

// -- Week 1: test-arena -------------------------------------------------------

// runHashPolicy prints the SHA256 of a user policy .bpf.o so an
// operator can copy-paste it into --policy-allowlist. Kept minimal
// (no flags) — the CLI is a convenience wrapper around
// policyprog.HashFile(). Idiomatic use:
//
//	$ s3fs-ebpf hash-policy /tmp/user.bpf.o
//	6c9d3d6cf141bd92c34f85d67f95dd3156b705404b6e29572664a9ba1704eebc  /tmp/user.bpf.o
//
// Output shape matches `sha256sum`'s so it's directly appendable to
// an allowlist file (just add a description on the same line).
func runHashPolicy(args []string) int {
	fs := flag.NewFlagSet("hash-policy", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr,
			"usage: s3fs-ebpf hash-policy <path> [<path>...]\n\n"+
				"Print SHA256 of one or more .bpf.o files in\n"+
				"allowlist-appendable form. See\n"+
				"policies/allowlist.example.txt for the entry format.\n")
	}
	fs.Parse(args)
	rest := fs.Args()
	if len(rest) == 0 {
		fs.Usage()
		return 2
	}
	code := 0
	for _, p := range rest {
		h, err := policyprog.HashFile(p)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hash-policy: %s: %v\n", p, err)
			code = 1
			continue
		}
		fmt.Printf("%s  %s\n", h, p)
	}
	return code
}

const (
	testInodes        = 32
	inodeStubSize     = 32 // sizeof(struct s3fs_inode_stub)
	testIndex         = 3
	testKfuncArg      = 21
	expectedKfuncOut  = testKfuncArg*2 + 1 // s3fs_kfunc_echo(v) = v*2 + 1
	testInoBase       = uint64(0x1000)
)

// runTestArena runs three sub-tests in sequence:
//
//	A) BPF_MAP_TYPE_ARENA create + mmap + write/read roundtrip
//	B) BPF_MAP_TYPE_ARRAY|MMAPABLE shared with a SEC("syscall") program
//	C) SEC("syscall") program calls s3fs_kfunc_echo (from kmod) and returns
//
// Failure of any sub-test prints [FAIL ...] and exits 1.
func runTestArena(args []string) int {
	fs := flag.NewFlagSet("test-arena", flag.ExitOnError)
	idx := fs.Uint("idx", testIndex, "arena index to read")
	kfuncArg := fs.Uint("kfunc-arg", testKfuncArg, "argument to s3fs_kfunc_echo")
	fs.Parse(args)

	failures := 0
	failures += subtestArena()
	failures += subtestSharedAndKfunc(uint32(*idx), uint32(*kfuncArg))

	if failures > 0 {
		fmt.Fprintf(os.Stderr, "FAIL (%d subtest failures)\n", failures)
		return 1
	}
	fmt.Println("PASS")
	return 0
}

func subtestArena() int {
	fmt.Println("== subtest A: bpf_arena create + mmap =========================")
	a, err := arena.New(1)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[FAIL A] arena.New: %v\n", err)
		return 1
	}
	defer a.Close()

	id, err := a.MapID()
	if err != nil {
		fmt.Fprintf(os.Stderr, "[FAIL A] MapID: %v\n", err)
		return 1
	}
	fmt.Printf("[A] arena map id=%d size=%d bytes\n", id, a.Size())

	want := []byte("hello arena")
	if _, err := a.WriteAt(want, 0); err != nil {
		fmt.Fprintf(os.Stderr, "[FAIL A] WriteAt: %v\n", err)
		return 1
	}
	got := make([]byte, len(want))
	if _, err := a.ReadAt(got, 0); err != nil {
		fmt.Fprintf(os.Stderr, "[FAIL A] ReadAt: %v\n", err)
		return 1
	}
	if string(got) != string(want) {
		fmt.Fprintf(os.Stderr, "[FAIL A] roundtrip: got %q want %q\n", got, want)
		return 1
	}
	fmt.Printf("[A] roundtrip OK: wrote/read %q\n", got)
	return 0
}

func subtestSharedAndKfunc(idx, kfuncArg uint32) int {
	fmt.Println("== subtest B+C: bpf_arena populate + KFunc call ===============")
	progs, err := fpebpf.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "[FAIL B] ebpf.Load: %v\n", err)
		return 1
	}
	defer progs.Close()

	// The BPF-owned arena was mmap'd inside ebpf.Load() (that ordering is
	// required so the verifier sees a valid user_vm_start when the program
	// loads). First write to each page demand-allocates it.
	a := progs.Arena()
	fmt.Printf("[B] arena mmap base=0x%x size=%d bytes\n", a.BaseAddr(), a.Size())

	// Layout: stub[i] at offset i * inodeStubSize.
	buf := make([]byte, inodeStubSize)
	for i := uint32(0); i < testInodes; i++ {
		binary.LittleEndian.PutUint64(buf[0:8], testInoBase+uint64(i))
		binary.LittleEndian.PutUint64(buf[8:16], 4096)      // size
		binary.LittleEndian.PutUint64(buf[16:24], 0)        // mtime_ns
		binary.LittleEndian.PutUint32(buf[24:28], 0o100644) // mode
		if _, err := a.WriteAt(buf, int64(i)*int64(inodeStubSize)); err != nil {
			fmt.Fprintf(os.Stderr, "[FAIL B] arena WriteAt[%d]: %v\n", i, err)
			return 1
		}
	}
	fmt.Printf("[B] wrote %d dummy inodes into arena\n", testInodes)

	in := fpebpf.TestArgs{
		ArenaBaseUaddr: a.BaseAddr(),
		Idx:            idx,
		KfuncArg:       kfuncArg,
	}
	out, ret, err := progs.RunTestShared(in)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[FAIL B/C] RunTestShared: %v\n", err)
		return 1
	}
	fmt.Printf("[B/C] prog ret=%d out_ino=0x%x out_kfunc=%d\n", ret, out.OutIno, out.OutKfunc)

	wantIno := testInoBase + uint64(idx)
	wantKfunc := uint32(kfuncArg*2 + 1)
	failed := 0
	if out.OutIno != wantIno {
		fmt.Fprintf(os.Stderr, "[FAIL B] out_ino: got 0x%x want 0x%x\n", out.OutIno, wantIno)
		failed++
	}
	if out.OutKfunc != wantKfunc {
		fmt.Fprintf(os.Stderr, "[FAIL C] out_kfunc: got %d want %d\n", out.OutKfunc, wantKfunc)
		failed++
	}
	return failed
}

// -- Week 2: fastpath ---------------------------------------------------------

// runFastpath loads the BPF collection, attaches fentry/vfs_getattr_nosec,
// and streams miss events + counter samples until SIGINT/SIGTERM. If
// --s3 is passed, each miss event triggers an S3 metadata fetch and
// arena/inode_index population, so subsequent stats on the same ino
// register as fast_hits.
func runFastpath(args []string) int {
	fs := flag.NewFlagSet("fastpath", flag.ExitOnError)
	sampleEvery := fs.Duration("sample-every", 2*time.Second,
		"how often to print aggregated counters")
	enableS3 := fs.Bool("s3", false,
		"consume miss events and warm the arena from an S3-compatible backend")
	dev := fs.String("dev", "/dev/s3fs_ebpf",
		"kmod control device for arena-backed read admission")
	syncEvery := fs.Duration("sync-every", 30*time.Second,
		"how often to re-list the bucket and push the entry table to kmod (0 disables)")
	metricsAddr := fs.String("metrics-addr", ":9600",
		"address for the Prometheus scrape endpoint (empty to disable)")
	metricsToken := fs.String("metrics-token", envOr("S3FS_METRICS_TOKEN", ""),
		"shared-secret Bearer token required for POST /policy/reload and /policy/eval. "+
			"Empty disables auth (dev default). Set S3FS_METRICS_TOKEN env to avoid shell history.")
	policyPath := fs.String("policy", "",
		"path to a CEL policy file (see policies/default.cel). Empty disables the policy engine (default prewarm=all).")
	policyBPF := fs.String("policy-bpf", "",
		"policy program: 'builtin:<name>' (repo bpf/<name>.bpf.o) or 'file:<path>' (Layer 3 user .bpf.o). Empty disables policy hook.")
	policyAllowlist := fs.String("policy-allowlist", "",
		"path to SHA256 allowlist (see policies/allowlist.example.txt). "+
			"When set, `file:` / bare-path policy loads and /policy/reload "+
			"requests must hash to an allowlisted entry. builtin: bypasses. "+
			"Empty disables verification (dev/demo default).")
	noReadIterHook := fs.Bool("no-read-iter-hook", false,
		"[R-01] disable fentry/generic_file_read_iter hook. "+
			"Use for overhead measurement only; disables read pattern tracking and "+
			"is incompatible with --pattern-aware-admit.")
	patternAdmit := fs.Bool("pattern-aware-admit", true,
		"[R-01] use s3fs_read_pattern_map sequential signal to promote large-file "+
			"read-miss upcalls to full-file GETs, reducing S3 round-trips. "+
			"Requires fentry/generic_file_read_iter (incompatible with --no-read-iter-hook).")
	prefetchSeq := fs.Bool("prefetch-seq-admit", false,
		"[AA-04] proactively pre-admit the next sequential chunk while the current "+
			"chunk is still being read, reducing cold-miss upcalls for sequential "+
			"large-file reads. Fires when s3fs_read_pattern_map shows SeqRatio ≥ 0.70 "+
			"and the read position is within 2 MiB of the admitted range boundary. "+
			"Requires --pattern-aware-admit (incompatible with --no-read-iter-hook).")
	wtinylFUAgeInterval := fs.Uint64("wtinylfu-age-interval", 512,
		"[Z-01] W-TinyLFU CMS aging interval: halve all CMS counters every N on_hit calls. "+
			"Lower values age faster (suit short bench runs); higher values age slower "+
			"(suit long live workloads). Written to wtinylfu_config[0] after policy load; "+
			"silently ignored for non-wtinylfu policies.")
	timeDecayNS := fs.Uint64("time-decay-ns", 5_000_000_000,
		"[Z-02] time_decay policy: evict items unaccessed for this many nanoseconds. "+
			"Default 5 s (5000000000). Set to e.g. 1000000000 (1 s) for short bench runs. "+
			"Written to time_decay_config[0] after policy load; silently ignored for other policies.")
	wtinylFUFullAgeInterval := fs.Uint64("wtinylfu-full-age-interval", 4,
		"[Z-05] W-TinyLFU full: halve CMS every N on_admit calls (default 4). "+
			"Written to wtinylfu_full_config[0]; silently ignored for other policies.")
	wtinylFUFullDKResetInterval := fs.Uint64("wtinylfu-full-dk-reset-interval", 16,
		"[Z-05] W-TinyLFU full: reset doorkeeper every N on_admit calls (default 16). "+
			"Written to wtinylfu_full_config[1]; silently ignored for other policies.")
	inoState := fs.String("ino-state", envOr("S3FS_INO_STATE", ""),
		"[H-02] path to persist the S3Key → ino allocator table (JSON). "+
			"When set, the map is loaded at startup and atomically re-written after every "+
			"assignIno / registerIno so ino ↔ path pairs survive daemon restarts. "+
			"Empty (default) keeps the allocator in-memory only.")
	prewarmOnSync := fs.Bool("prewarm-on-sync", true,
		"[F-01] when true (default), each sync round runs the prewarm hook on any entry "+
			"that appeared since the previous round so the first cat hits Path B/A "+
			"instead of the ~30ms Path C upcall. Off to keep sync rounds purely metadata.")
	softcapFlag := fs.Int("softcap", -1,
		"[S-02] legacy-mode eviction trigger: apply policy votes once the admit "+
			"snapshot exceeds this many entries. Writes the kmod `softcap` module "+
			"parameter; -1 (default) leaves the kmod value (8) as is.")
	cacheCapacity := fs.Int64("cache-capacity", -1,
		"[S-02] admit-set capacity in bytes. >0 switches kmod to capacity mode "+
			"(evict by policy score, then oldest first, until the admitted bytes fit); "+
			"0 restores legacy vote mode; -1 (default) leaves the kmod value as is.")
	newClient := backendFlags(fs)
	fs.Parse(args)

	if *softcapFlag >= 0 {
		if err := setKmodParam("softcap", fmt.Sprint(*softcapFlag)); err != nil {
			fmt.Fprintf(os.Stderr, "softcap: %v\n", err)
			return 1
		}
	}
	if *cacheCapacity >= 0 {
		if err := setKmodParam("capacity_bytes", fmt.Sprint(*cacheCapacity)); err != nil {
			fmt.Fprintf(os.Stderr, "cache-capacity: %v\n", err)
			return 1
		}
	}

	// Z-03: scale S3-FIFO ghost map proportionally to admit capacity.
	// Ghost entries = ~4× the expected number of admitted files so
	// promotion fires even after large working-set eviction cycles.
	// Formula: max(8192, softcap*1024), capped at 65536 (≈1 MB map).
	// softcap=-1 means "leave kmod default (8)" → ghost_cap = 8192.
	const (
		s3fifoGhostMap      = "s3fifo3_state"
		s3fifoGhostMinCap   = uint32(8192)
		s3fifoGhostMaxCap   = uint32(65536)
	)
	s3fifoGhostCap := s3fifoGhostMinCap
	if *softcapFlag > 0 {
		if est := uint32(*softcapFlag) * 1024; est > s3fifoGhostCap {
			s3fifoGhostCap = est
		}
	}
	if s3fifoGhostCap > s3fifoGhostMaxCap {
		s3fifoGhostCap = s3fifoGhostMaxCap
	}
	policyLoadOpts := policyprog.LoadOpts{
		MapMaxEntries: map[string]uint32{s3fifoGhostMap: s3fifoGhostCap},
	}

	if *patternAdmit && *noReadIterHook {
		fmt.Fprintln(os.Stderr, "--pattern-aware-admit requires the read-iter hook; cannot combine with --no-read-iter-hook")
		return 1
	}
	if *prefetchSeq && *noReadIterHook {
		fmt.Fprintln(os.Stderr, "--prefetch-seq-admit requires the read-iter hook; cannot combine with --no-read-iter-hook")
		return 1
	}
	patternAwareAdmitEnabled = *patternAdmit
	prefetchSeqEnabled = *prefetchSeq

	if err := SetInoStatePath(*inoState); err != nil {
		fmt.Fprintf(os.Stderr, "ino-state: %v\n", err)
		return 1
	}
	if *inoState != "" {
		n, next := inoAlloc.CountMappings()
		fmt.Printf("[ino-state] %s: %d mappings loaded (nextIno=%d)\n",
			*inoState, n, next)
	}

	// Load policy engine (Week 4 (d) P-01 spike: prewarm hook only).
	// Missing or empty file returns an Engine that defaults every hook
	// to the safe fallback (prewarm=true), so the mount is never
	// stranded cold when a policy file is broken.
	pol, err := policy.Load(*policyPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "policy load: %v\n", err)
		return 1
	}
	if *policyPath != "" {
		fmt.Printf("[policy] loaded %s\n", *policyPath)
	}

	// Layer 2 / Layer 3 BPF policy program (Week 4 (d)). --policy-bpf
	// takes 'builtin:<name>' (engine-shipped) or 'file:<path>' (user
	// eBPF escape hatch, §4.5 Layer 3). Same load path, same attach.
	repoRoot, _ := os.Getwd()
	// Best-effort: if run from repo root or bin/, resolve upward.
	if _, err := os.Stat(filepath.Join(repoRoot, "bpf")); err != nil {
		if p := filepath.Dir(repoRoot); p != repoRoot {
			if _, err := os.Stat(filepath.Join(p, "bpf")); err == nil {
				repoRoot = p
			}
		}
	}
	// CEL policy hook may override --policy-bpf. Precedence:
	//   1. CEL `policy: <expr>` if it evaluates to non-empty string
	//   2. --policy-bpf flag
	//   3. no BPF program attached
	// The CEL path lets a single .cel file bundle prewarm decisions +
	// which Layer 2 program to attach.
	policyBPFEffective := *policyBPF
	if celName := pol.EvalPolicyName(); celName != "" {
		if *policyBPF != "" && *policyBPF != celName {
			fmt.Printf("[policy] CEL policy=%q overrides --policy-bpf=%q\n",
				celName, *policyBPF)
		}
		policyBPFEffective = celName
	}
	// Optional allowlist for user eBPF (Layer 3). When --policy-allowlist
	// is unset the daemon runs permissive — every `file:` load is
	// accepted (dev default). When set, the file must exist and each
	// non-builtin load is SHA256-checked. Missing allowlist file is a
	// startup error to avoid silent-permissive-when-you-meant-strict.
	var allowlist *policyprog.Allowlist
	if *policyAllowlist != "" {
		allowlist, err = policyprog.LoadAllowlist(*policyAllowlist)
		if err != nil {
			fmt.Fprintf(os.Stderr, "policy-allowlist: %v\n", err)
			return 1
		}
		fmt.Printf("[allowlist] %s: %d approved user .bpf.o hashes (builtin bypasses)\n",
			allowlist.Path(), allowlist.Size())
	}

	// Warn early when a file: policy will be loaded without any SHA256
	// verification. Operators who meant to enforce the allowlist get
	// immediate feedback instead of a silent permissive-mode surprise.
	if allowlist == nil && policyBPFEffective != "" && !strings.HasPrefix(policyBPFEffective, "builtin:") {
		fmt.Fprintln(os.Stderr, "[policy-bpf] WARN: --policy-allowlist not set; file: loads are not SHA256-verified (dev mode)")
	}

	// verifyIfUser gates the SHA256 check to `file:`-style loads.
	// builtin: is engine-shipped and skips verification. Kept as a
	// closure so the reload handler and initial load share exactly
	// one policy for what "user-supplied" means.
	verifyIfUser := func(arg, path string) error {
		if strings.HasPrefix(arg, "builtin:") {
			return nil
		}
		return allowlist.Verify(path)
	}

	// polProg is swapped by POST /policy/reload (HTTP goroutine) and
	// read by samplePolicyStats; polMu serialises both.
	var (
		polMu   sync.Mutex
		polProg *policyprog.Program
	)
	if bpfPath, err := policyprog.Resolve(policyBPFEffective, repoRoot); err != nil {
		fmt.Fprintf(os.Stderr, "policy-bpf: %v\n", err)
		return 1
	} else if bpfPath != "" {
		if err := verifyIfUser(policyBPFEffective, bpfPath); err != nil {
			fmt.Fprintf(os.Stderr, "policy-bpf: %v\n", err)
			return 1
		}
		polProg, err = policyprog.Load(bpfPath, policyLoadOpts)
		if err != nil {
			fmt.Fprintf(os.Stderr, "policy-bpf load: %v\n", err)
			return 1
		}
		if err := polProg.Attach(); err != nil {
			polProg.Close()
			fmt.Fprintf(os.Stderr, "policy-bpf attach: %v\n", err)
			return 1
		}
		// Close whatever program is current at exit — after a hot
		// reload that is no longer the one loaded here, so the defer
		// must not capture polProg by value.
		defer func() {
			polMu.Lock()
			polProg.Close()
			polMu.Unlock()
		}()
		fmt.Printf("[policy-bpf] loaded %s (%d links attached, struct_ops=%v)\n",
			bpfPath, polProg.AttachedCount(), polProg.IsStruct)
		// Z-01: write the configured age interval into wtinylfu_config[0].
		// No-op for policies that don't declare the map.
		if *wtinylFUAgeInterval != 512 {
			if err := polProg.SetConfigU64("wtinylfu_config", 0, *wtinylFUAgeInterval); err != nil {
				fmt.Fprintf(os.Stderr, "[policy-bpf] wtinylfu-age-interval: %v (non-fatal)\n", err)
			} else {
				fmt.Printf("[policy-bpf] wtinylfu age_interval=%d\n", *wtinylFUAgeInterval)
			}
		}
		// Z-02: write the configured decay window into time_decay_config[0].
		// No-op for policies that don't declare the map.
		const decayNSDefault = 5_000_000_000
		if *timeDecayNS != decayNSDefault {
			if err := polProg.SetConfigU64("time_decay_config", 0, *timeDecayNS); err != nil {
				fmt.Fprintf(os.Stderr, "[policy-bpf] time-decay-ns: %v (non-fatal)\n", err)
			} else {
				fmt.Printf("[policy-bpf] time_decay decay_ns=%d (%.1f s)\n", *timeDecayNS, float64(*timeDecayNS)/1e9)
			}
		}
		// Z-05: write wtinylfu_full age/doorkeeper-reset intervals.
		// No-op for policies that don't declare wtinylfu_full_config.
		if *wtinylFUFullAgeInterval != 4 {
			if err := polProg.SetConfigU64("wtinylfu_full_config", 0, *wtinylFUFullAgeInterval); err != nil {
				fmt.Fprintf(os.Stderr, "[policy-bpf] wtinylfu-full-age-interval: %v (non-fatal)\n", err)
			}
		}
		if *wtinylFUFullDKResetInterval != 16 {
			if err := polProg.SetConfigU64("wtinylfu_full_config", 1, *wtinylFUFullDKResetInterval); err != nil {
				fmt.Fprintf(os.Stderr, "[policy-bpf] wtinylfu-full-dk-reset-interval: %v (non-fatal)\n", err)
			}
		}

		// Wire up POST /policy/reload for hot swap. struct_ops →
		// struct_ops retargets the live link with one BPF_LINK_UPDATE
		// (kmod .update = single RCU pointer swap). Otherwise the new
		// program is attached BEFORE the old one is closed so the kmod
		// side sees "new reg → old unreg (superseded)" and never goes
		// without an active vtable (guarded by kmod-side
		// s3fs_policy_unreg check). See policyprog.SwapFrom.
		metrics.ReloadHandler = func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				http.Error(w, "POST only", http.StatusMethodNotAllowed)
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
			body, _ := io.ReadAll(r.Body)
			target := string(body)
			target = strings.TrimSpace(target)
			if target == "" {
				http.Error(w, "empty body: expected builtin:<name> or file:<path>",
					http.StatusBadRequest)
				return
			}
			newPath, err := policyprog.Resolve(target, repoRoot)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			// Allowlist gate — same policy as the startup load path.
			// Reject user-supplied hashes not in the list; builtin: is
			// engine-shipped and bypasses.
			if err := verifyIfUser(target, newPath); err != nil {
				http.Error(w, err.Error(), http.StatusForbidden)
				return
			}
			newProg, err := policyprog.Load(newPath, policyLoadOpts)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			polMu.Lock()
			defer polMu.Unlock()
			old := polProg
			updated, err := newProg.SwapFrom(old)
			if err != nil {
				newProg.Close()
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			polProg = newProg
			old.Close()
			how := "attach+detach"
			if updated {
				how = "BPF_LINK_UPDATE"
			}
			fmt.Printf("[policy-bpf] hot-reloaded %s → %s (%s)\n", old.Path, newPath, how)
			fmt.Fprintf(w, "reloaded to %s (%s)\n", newPath, how)
		}
		// POST /policy/eval: force an admit_replace with the current
		// snapshot so the policy program's on_admit / on_evict fires
		// another round without needing bucket-side churn. Bench
		// harnesses use this to drive SIEVE's grace → clear → evict
		// state machine on a controllable schedule.
		metrics.EvalHandler = func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				http.Error(w, "POST only", http.StatusMethodNotAllowed)
				return
			}
			n, err := refreshAdmits(*dev)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			fmt.Fprintf(w, "re-eval fired with %d admit entries\n", n)
		}
	}

	// Bring the metrics endpoint up early so Prometheus scrapes
	// succeed even before the first admit/sync happens. Failure to
	// bind is non-fatal — logged and daemon continues.
	if *metricsAddr != "" {
		if *metricsToken == "" {
			fmt.Fprintln(os.Stderr, "[metrics] WARN: --metrics-token not set; /policy/reload and /policy/eval are unauthenticated")
		}
		go func(addr, token string) {
			fmt.Printf("[metrics] serving on %s/metrics\n", addr)
			if err := metrics.Serve(addr, token); err != nil {
				fmt.Fprintf(os.Stderr, "[metrics] serve %s: %v\n", addr, err)
			}
		}(*metricsAddr, *metricsToken)
	}

	// V-01: open /dev/s3fs_ebpf once and share the fd across all admit /
	// signal ioctls. Previously admitArenaIoctl and signalAdmit reopened
	// the device on every call, adding an open/close pair per upcall.
	devF, err := openSharedDev(*dev)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open dev: %v\n", err)
		return 1
	}
	defer func() {
		sharedDevFile = nil
		devF.Close()
	}()

	progs, err := fpebpf.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "load: %v\n", err)
		return 1
	}
	defer progs.Close()
	setAdmitProgs(progs)
	defer setAdmitProgs(nil)

	fentry, err := progs.AttachFentry()
	if err != nil {
		fmt.Fprintf(os.Stderr, "attach: %v\n", err)
		return 1
	}
	defer fentry.Close()

	if *noReadIterHook {
		fmt.Println("[fastpath] fentry/generic_file_read_iter disabled (--no-read-iter-hook); read pattern tracking off")
	} else {
		readIter, err := progs.AttachFentryReadIter()
		if err != nil {
			// Non-fatal: some kernels inline generic_file_read_iter.
			fmt.Fprintf(os.Stderr, "[warn] attach fentry/generic_file_read_iter: %v (read pattern tracking disabled)\n", err)
		} else {
			defer readIter.Close()
			fmt.Println("[fastpath] attached fentry/generic_file_read_iter; read pattern tracking enabled")
		}
	}

	reader, err := ringbuf.NewReader(progs.EventsMap())
	if err != nil {
		fmt.Fprintf(os.Stderr, "ringbuf reader: %v\n", err)
		return 1
	}
	defer reader.Close()

	var s3c objstore.Backend
	if *enableS3 {
		s3c, err = newClient()
		if err != nil {
			fmt.Fprintf(os.Stderr, "fastpath: backend: %v\n", err)
			return 1
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(),
		syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	fmt.Printf("[fastpath] attached fentry/vfs_getattr_nosec; arena size=%d\n",
		progs.Arena().Size())
	// arena capacity is fixed for the lifetime of the daemon; publish
	// once so the dashboard's usage% panel has a denominator.
	metrics.Global.ArenaCapacityBytes.Store(progs.Arena().Size())
	if s3c != nil {
		prefixStr := ""
		if p, ok := s3c.(*objstore.Prefixed); ok {
			prefixStr = " key-prefix=" + p.Prefix()
		}
		fmt.Printf("[fastpath] object backend: bucket=%s%s (will warm arena on miss)\n",
			s3c.Bucket(), prefixStr)
	} else {
		fmt.Println("[fastpath] object backend disabled — misses are reported but not cached")
	}

	// Week 4 (b): before the first prewarm, ask S3 what actually
	// exists and push that snapshot to kmod so `ls /mnt/s3demo`
	// reflects reality instead of the compile-time bootstrap. When
	// s3c==nil this is a no-op — the mirror stays on bootstrap and
	// kmod keeps its own bootstrap table.
	if fresh, err := syncEntriesOnce(ctx, *dev, s3c); err != nil {
		fmt.Fprintf(os.Stderr, "[sync] initial: %v (using bootstrap)\n", err)
	} else {
		fmt.Printf("[sync] initial: %d entries\n", len(fresh))
		// On daemon exit, revert kmod's dyn table to the compile-time
		// bootstrap so a subsequent daemon restart (or an operator
		// browsing the mount without daemon) doesn't see the stale
		// snapshot from this session. Mirrors the clearAdmits defer.
		defer func() {
			if err := syncEntriesIoctl(*dev, nil); err != nil {
				fmt.Fprintf(os.Stderr, "[sync clear] %v\n", err)
			}
		}()
	}

	// Pre-warm the arena from the entry mirror so the first stat on
	// each object hits the Fast Path instead of firing a miss. With
	// S3 we reflect real object metadata (size, mtime) and — the
	// point of Week 4 (a) — copy the object body into the arena so
	// kmod's .read reaches those bytes via GUP over daemon's user_vm.
	initialAdmits := prewarm(context.Background(), progs, s3c, pol)
	if err := setAdmits(*dev, initialAdmits); err != nil {
		fmt.Fprintf(os.Stderr, "[admit] %v — kmod fallback (kmalloc scratch) still works\n", err)
	} else {
		fmt.Printf("[admit] %d entries via %s\n", len(initialAdmits), *dev)
		defer func() {
			if err := clearAdmits(*dev); err != nil {
				fmt.Fprintf(os.Stderr, "[admit clear] %v\n", err)
			}
		}()

		// Read-miss upcall reader: services `cat` on any admitted
		// entry with Prewarm=false, or any ino kmod knows about
		// that daemon hasn't warmed yet.
		go runReadMissLoop(ctx, *dev, progs, s3c)
		// AA-04: proactive sequential prefetch — pre-admit the next
		// chunk of large files while the current chunk is still being
		// read. No-op unless --prefetch-seq-admit is set.
		go runSeqPrefetchLoop(ctx, *dev, progs, s3c)
	}

	fmt.Printf("[fastpath] stat any path to trigger events. Ctrl+C to stop.\n")

	go func() {
		<-ctx.Done()
		reader.Close()
	}()
	// W-03: one goroutine samples all three stats sources (BPF counters,
	// kmod ioctl, arena free-list) on a single ticker.
	go runStatsLoop(ctx, progs, *dev, *sampleEvery)
	go drainEvictionsLoop(ctx, *dev, progs, 2*time.Second)
	// Periodic bucket sync: keep kmod's entry table + daemon mirror
	// aligned with S3 without re-mounting. New keys become visible
	// via `ls`, deleted keys stop resolving. When admit state is
	// impacted (e.g. a size change) the next .read_folio re-fetches.
	var lastSyncCount = len(fpebpf.Snapshot())
	// F-01: track inos we've already prewarmed so each sync round
	// only warms newly-appeared entries. Seeded from the startup
	// prewarm output so the first sync round doesn't re-fetch things
	// we just admitted.
	prewarmed := make(map[uint64]struct{}, len(initialAdmits))
	for _, a := range initialAdmits {
		prewarmed[a.Ino] = struct{}{}
	}
	go runSyncLoop(ctx, *dev, s3c, *syncEvery, func(fresh []fpebpf.Entry) {
		if len(fresh) != lastSyncCount {
			fmt.Printf("[sync] delta: %d → %d entries\n",
				lastSyncCount, len(fresh))
			lastSyncCount = len(fresh)
		}
		// Drop admit entries whose ino no longer appears in the fresh
		// entry set (S3 rm made the key disappear). Without this the
		// admit hash and arena buffer leak. See A-02 in docs/journal/backlog.md.
		keep := make(map[uint64]struct{}, len(fresh))
		for _, e := range fresh {
			keep[e.Ino] = struct{}{}
		}
		if err := pruneAdmitsTo(*dev, keep); err != nil {
			fmt.Fprintf(os.Stderr, "[admit prune] %v\n", err)
		}
		// F-01: prewarm any entry we haven't seen before so the
		// first cat hits Path A/B. Skips inos gone from the fresh
		// set (they no longer exist to warm) and inos we already
		// warmed in a prior round (idempotent).
		if !*prewarmOnSync {
			return
		}
		for ino := range prewarmed {
			if _, alive := keep[ino]; !alive {
				delete(prewarmed, ino)
			}
		}
		for _, entry := range fresh {
			if _, done := prewarmed[entry.Ino]; done {
				continue
			}
			ad, ok := prewarmEntry(ctx, progs, s3c, pol, entry)
			prewarmed[entry.Ino] = struct{}{}
			if !ok {
				continue
			}
			if err := replaceAdmit(*dev, ad, progs); err != nil {
				fmt.Fprintf(os.Stderr, "[sync prewarm] admit ino=%d: %v\n",
					entry.Ino, err)
			}
		}
	})
	// Week 4 (c): drain the kmod write-back queue and PUT bytes to
	// S3. Runs even without --s3 so the queue doesn't grow unbounded
	// (in offline mode it just logs and drops each event).
	go runFlushLoop(ctx, *dev, progs, s3c, 100*time.Millisecond)
	// S-08: blocking goroutine for fsync-triggered (WAIT-flagged) flush
	// events — eliminates the up-to-100 ms ticker latency for fsync.
	go runFlushWaitLoop(ctx, *dev, progs, s3c)
	// E-02: drain kmod delete queue and issue S3 DELETE for each.
	// Runs on the same interval as flush; symmetric shape.
	go runDeleteLoop(ctx, *dev, s3c, 100*time.Millisecond)
	// E-02: drain rename queue (CopyObject + Delete server-side).
	go runRenameLoop(ctx, *dev, s3c, 100*time.Millisecond)

	// Layer 2/3 policy program stats sampler. Reads the loaded BPF
	// program's policy_stats map every N and mirrors into the
	// metrics endpoint so Grafana can chart hook activity alongside
	// the daemon-side counters. No-op when --policy-bpf is empty.
	polMu.Lock()
	havePolicy := polProg != nil
	polMu.Unlock()
	if havePolicy {
		go samplePolicyStats(ctx, &polMu, &polProg, 5*time.Second)
	}

	for {
		rec, err := reader.Read()
		if err != nil {
			if errors.Is(err, ringbuf.ErrClosed) {
				break
			}
			fmt.Fprintf(os.Stderr, "ringbuf read: %v\n", err)
			continue
		}
		if len(rec.RawSample) < 16 {
			fmt.Fprintf(os.Stderr, "short miss event: %d bytes\n", len(rec.RawSample))
			continue
		}
		ev := fpebpf.MissEvent{
			Ino: binary.LittleEndian.Uint64(rec.RawSample[0:8]),
			Op:  binary.LittleEndian.Uint32(rec.RawSample[8:12]),
			Pad: binary.LittleEndian.Uint32(rec.RawSample[12:16]),
		}
		fmt.Printf("[miss] ino=%d op=%s\n", ev.Ino, opName(ev.Op))
		if s3c != nil {
			warmFromS3(ctx, progs, s3c, ev)
		}
	}

	printCounters(progs, "final")
	return 0
}

// prewarmWorkers is the concurrency limit for parallel S3 fetches during
// prewarm. 8 matches typical S3 client concurrency limits and avoids
// overwhelming small MinIO test instances (V-03).
const prewarmWorkers = 8

// prewarm populates the arena + inode_index from the kmod entry table
// so the first stat on each demo object hits fast_hits, and when S3
// is available copies each object's body into the arena and returns
// the (ino, arena_off, size) triples the caller ioctls into kmod so
// .read serves those bytes without a userspace bounce.
//
// Without S3 we still warm the stat cache but skip content admission —
// kmod's kmalloc scratch buffer covers `cat` at reduced fidelity.
//
// V-03: entries are prewarmed in parallel (up to prewarmWorkers) to
// overlap S3 round-trips. AdmitContent is serialized inside fpebpf so
// arena offset assignment is still safe under concurrency.
func prewarm(ctx context.Context, progs *fpebpf.Programs, s3c objstore.Backend, pol *policy.Engine) []admitEntry {
	timedCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	live := fpebpf.Snapshot()

	type result struct {
		ad admitEntry
		ok bool
	}
	results := make([]result, len(live))

	g, gCtx := errgroup.WithContext(timedCtx)
	g.SetLimit(prewarmWorkers)
	for i, entry := range live {
		i, entry := i, entry
		g.Go(func() error {
			ad, ok := prewarmEntry(gCtx, progs, s3c, pol, entry)
			results[i] = result{ad, ok}
			return nil
		})
	}
	_ = g.Wait()

	admits := make([]admitEntry, 0, len(live))
	for _, r := range results {
		if r.ok {
			admits = append(admits, r.ad)
		}
	}
	return admits
}

// prewarmEntry runs the per-entry prewarm work. Split into two
// distinct steps so F-03 can guarantee metadata warm even when
// content admit is skipped:
//
//  1. **Metadata warm** (always): S3 Stat → progs.WarmCache. Populates
//     the BPF inode_index so the fentry fast_hits stub recognizes
//     the ino on the first getattr. Runs regardless of policy — a
//     policy that suppresses content admit still wants stat to hit
//     Path A rather than Path C.
//
//  2. **Content admit** (policy-gated): S3 Get → progs.AdmitContent.
//     Only fires when `wantPrewarm` (Entry.Prewarm ∨ CEL override) is
//     true. Skip returns (zero, false) — the ino stays warm for stat,
//     the first cat takes Path C.
//
// Returns (entry, true) only when content admission completed. The
// warm-only case is (zero, false) — same signal shape as before F-03,
// so existing callers keep working; the meaningful visible change is
// that fentry recognizes policy-skipped inos immediately.
func prewarmEntry(ctx context.Context, progs *fpebpf.Programs, s3c objstore.Backend,
	pol *policy.Engine, entry fpebpf.Entry) (admitEntry, bool) {
	// G-01: synthetic directory entries have no arena content and
	// no S3 backing key. WarmCache would try to stat "", which S3
	// answers with a nonsense error. Short-circuit here.
	if entry.Mode&fileTypeMask == 0o040000 {
		return admitEntry{}, false
	}
	// Entry.Prewarm is the static bootstrap default; the policy
	// engine, when loaded, overrides per-entry (P-01 spike). Both
	// paths default to prewarm=true if the policy is broken /
	// missing, so a misconfigured file doesn't strand the mount.
	wantPrewarm := entry.Prewarm
	if pol != nil {
		wantPrewarm = pol.EvalPrewarm(policy.EntryInput{
			Ino: entry.Ino, Name: entry.Name, S3Key: entry.S3Key,
			Size: entry.Fallback, Mode: entry.Mode,
		})
	}

	// Step 1 — metadata warm (unconditional). Prefer S3-authoritative
	// size/mtime when reachable; fall back to the bootstrap entry
	// otherwise. Both branches populate the BPF inode_index so the
	// fentry hook resolves getattr on Path A.
	stub := fpebpf.InodeStub{
		Ino:     entry.Ino,
		Size:    entry.Fallback,
		MtimeNs: uint64(time.Now().UnixNano()),
		Mode:    entry.Mode,
	}
	source := "fallback"
	var body []byte
	if s3c != nil {
		meta, err := s3c.Stat(ctx, entry.S3Key)
		if err != nil {
			fmt.Printf("[prewarm] ino=%d s3://%s stat: %v (using fallback)\n",
				entry.Ino, entry.S3Key, err)
		} else {
			stub.Size = uint64(meta.Size)
			stub.MtimeNs = uint64(meta.LastModified.UnixNano())
			source = fmt.Sprintf("s3://%s", entry.S3Key)
			if wantPrewarm {
				body, err = s3c.Get(ctx, entry.S3Key)
				if err != nil {
					fmt.Fprintf(os.Stderr, "[prewarm] ino=%d get: %v (skip admit)\n",
						entry.Ino, err)
					body = nil
				}
			}
		}
	}
	off, err := progs.WarmCache(stub)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[prewarm] ino=%d warm: %v\n", entry.Ino, err)
		return admitEntry{}, false
	}
	fmt.Printf("[prewarm] ino=%d name=%s size=%d src=%s → stub=%d\n",
		entry.Ino, entry.Name, stub.Size, source, off)

	// Step 2 — content admit (policy-gated).
	if !wantPrewarm {
		fmt.Printf("[prewarm] ino=%d name=%s content-skip (policy=false, stat=warm, cat=PathC)\n",
			entry.Ino, entry.Name)
		return admitEntry{}, false
	}
	if body == nil {
		return admitEntry{}, false
	}
	cOff, err := progs.AdmitContent(body)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[prewarm] ino=%d content: %v (skip admit)\n",
			entry.Ino, err)
		return admitEntry{}, false
	}
	if hi := cOff + uint64(len(body)); hi > metrics.Global.ArenaUsedBytes.Load() {
		metrics.Global.ArenaUsedBytes.Store(hi)
	}
	fmt.Printf("[prewarm] ino=%d body=%d bytes → content=%d\n",
		entry.Ino, len(body), cOff)
	return admitEntry{
		Ino:   entry.Ino,
		Uaddr: progs.Arena().BaseAddr() + cOff,
		Size:  uint64(len(body)),
	}, true
}

// warmFromS3 resolves a miss event through the S3 client and populates
// the arena + inode index so the next matching stat hits Fast Path.
//
// Week 4 (a)+ per-object inos are handled by admit_arena / handleReadMiss.
// This function now only covers the root ino (=1) case where the "content"
// is the bucket-wide listing (a summary stub). Non-root inos are silently
// no-op'd — the dyn entry table + admit path owns them.
func warmFromS3(ctx context.Context, progs *fpebpf.Programs, c objstore.Backend, ev fpebpf.MissEvent) {
	if ev.Ino != 1 {
		// Handled by handleReadMiss for per-object read misses; the
		// fentry hook fires for stat too but the sync loop already
		// pushed the entry, so nothing to do here.
		return
	}
	// Drain-race: BPF can fire many misses before we admit the first;
	// they all queue on the ringbuf. Skip the S3 round-trip when the
	// ino is already cached — the fentry hook will start hitting
	// fast_hits as soon as its dentry-cache-warm getattrs finish.
	if progs.IsWarmed(ev.Ino) {
		return
	}
	timedCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	objs, err := c.List(timedCtx, "")
	if err != nil {
		fmt.Fprintf(os.Stderr, "[warm] ino=1 list bucket: %v\n", err)
		return
	}
	// The root dir stub summarises "how many objects live under this
	// mount point" and when the listing was last refreshed. It's a
	// pragmatic placeholder for Week 3 when per-object inodes exist.
	stub := fpebpf.InodeStub{
		Ino:     ev.Ino,
		Size:    uint64(len(objs)),
		MtimeNs: uint64(time.Now().UnixNano()),
		Mode:    0o40755, // S_IFDIR | 0755
	}
	off, err := progs.WarmCache(stub)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[warm] ino=%d populate: %v\n", ev.Ino, err)
		return
	}
	fmt.Printf("[warm] ino=%d ← s3://%s (%d objects) → arena offset=%d\n",
		ev.Ino, c.Bucket(), len(objs), off)
}

// samplePolicyStats mirrors the loaded policy BPF program's counters
// into metrics.Global on a ticker. Uses lookups from the shared map
// (see bpf/policy_observe.bpf.c PSTAT_* indices).
func samplePolicyStats(ctx context.Context, mu *sync.Mutex, cur **policyprog.Program, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			// Re-resolve every tick under the reload lock: a hot
			// reload closes the old program's maps, so a captured
			// pointer would read a closed (or reused) fd.
			mu.Lock()
			if p := *cur; p != nil {
				if v, err := p.ReadStat(policyprog.StatInvocations); err == nil {
					metrics.Global.PolicyHookInvocations.Store(v)
				}
				if v, err := p.ReadStat(policyprog.StatLastCount); err == nil {
					metrics.Global.PolicyLastAdmitCount.Store(v)
				}
				// Z-04: pressure_ops strict/lenient counters.
				// Silently ignored for other policies (out-of-range → error).
				if v, err := p.ReadStat(policyprog.StatPressureStrictTotal); err == nil {
					metrics.Global.PolicyPressureStrictTotal.Store(v)
				}
				if v, err := p.ReadStat(policyprog.StatPressureLenientTotal); err == nil {
					metrics.Global.PolicyPressureLenientTotal.Store(v)
				}
			}
			mu.Unlock()
		}
	}
}

// runStatsLoop consolidates the three independent per-source sampling
// goroutines (BPF counters, kmod ioctl stats, arena free-list) into a
// single ticker loop (W-03). All three sources are sampled on every
// tick at the sampleEvery interval. Opening the device fd once
// (T-03 pattern) is handled inside the loop.
func runStatsLoop(ctx context.Context, progs *fpebpf.Programs, devPath string, every time.Duration) {
	// Open the device fd once for kmod-stats ioctls. Failure is
	// non-fatal (kmod stats will be zero in /metrics) so the daemon
	// starts even without a real device (tests / offline mode).
	var devFd int = -1
	if f, err := os.OpenFile(devPath, os.O_RDWR, 0); err == nil {
		defer f.Close()
		devFd = int(f.Fd())
	} else {
		fmt.Fprintf(os.Stderr, "[stats] open %s: %v (kmod stats disabled)\n", devPath, err)
	}

	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			// Source 1: BPF per-CPU counter map.
			printCounters(progs, "sample")

			// Source 2: kmod-side ioctl stats.
			if devFd >= 0 {
				if vals, err := readStatsOn(devFd); err == nil {
					if len(vals) >= 1 {
						metrics.Global.PolicyEvictionsTotal.Store(vals[0])
					}
					if len(vals) >= 2 {
						metrics.Global.FolioHits.Store(vals[1])
					}
					if len(vals) >= 3 {
						metrics.Global.AdmitInvalidations.Store(vals[2])
					}
				}
			}

			// Source 3: arena free-list gauge.
			if progs != nil {
				bytes, chunks := progs.ArenaFreeStats()
				metrics.Global.ArenaFreeBytes.Store(bytes)
				metrics.Global.ArenaFreeChunks.Store(uint64(chunks))
			}
		}
	}
}

// drainEvictionsLoop polls S3FS_IOC_DRAIN_EVICTIONS and prunes the
// daemon-side admit snapshot to match. Without this, daemon re-sends
// evicted inos on the next admit_replace and kmod evicts them again
// in a tight loop — visible in Grafana as admits_current staying
// high while policy_evictions_total climbs monotonically.
//
// D-02: `progs` is threaded so dropAdmits can return arena chunks
// to the free-list (fpebpf.Programs.FreeArenaChunk).
func drainEvictionsLoop(ctx context.Context, devPath string, progs *fpebpf.Programs, every time.Duration) {
	// T-03: open once and reuse the fd across ticks instead of
	// open/close on every 2s poll.
	f, err := os.OpenFile(devPath, os.O_RDWR, 0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[evict-drain] open %s: %v\n", devPath, err)
		return
	}
	defer f.Close()

	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			evicted, overflow, err := drainEvictionsOn(int(f.Fd()))
			if err != nil {
				continue
			}
			if overflow > 0 {
				fmt.Fprintf(os.Stderr, "[evict-drain] kmod queue overflow=%d (may need larger buffer)\n",
					overflow)
			}
			if n := dropAdmits(evicted, progs); n > 0 {
				fmt.Printf("[evict-drain] dropped %d admits fed back from kmod\n", n)
			}
		}
	}
}


func printCounters(progs *fpebpf.Programs, label string) {
	c, err := progs.ReadCounters()
	if err != nil {
		fmt.Fprintf(os.Stderr, "read counters: %v\n", err)
		return
	}
	// Mirror the BPF-side counters into the metrics endpoint. Store
	// (not Add) because the BPF map is the source of truth — the
	// per-CPU aggregate is what we want to expose, not the delta.
	metrics.Global.FastPathHits.Store(c[fpebpf.StatFastHits])
	metrics.Global.SlowPathMisses.Store(c[fpebpf.StatSlowMisses])
	metrics.Global.UnknownFS.Store(c[fpebpf.StatUnknownFS])
	metrics.Global.ReadIterHits.Store(c[fpebpf.StatReadIterHits])
	metrics.Global.ReadIterMisses.Store(c[fpebpf.StatReadIterMisses])
	metrics.Global.ReadIterHitsPos0.Store(c[fpebpf.StatReadIterHitsPos0])
	fmt.Printf("[counters/%s] fast_hits=%d slow_misses=%d unknown_fs=%d read_iter_hits=%d read_iter_misses=%d iter_pos0=%d\n",
		label,
		c[fpebpf.StatFastHits], c[fpebpf.StatSlowMisses], c[fpebpf.StatUnknownFS],
		c[fpebpf.StatReadIterHits], c[fpebpf.StatReadIterMisses],
		c[fpebpf.StatReadIterHitsPos0])
}

func opName(op uint32) string {
	switch op {
	case fpebpf.OpGetattr:
		return "getattr"
	case fpebpf.OpLookup:
		return "lookup"
	case fpebpf.OpRead:
		return "read"
	default:
		return fmt.Sprintf("op#%d", op)
	}
}

// -- Week 2: verifier-demo ----------------------------------------------------

// runVerifierDemo tries to load the intentionally-rejected pure-eBPF
// arena walker and prints the verifier's rejection log. Exit codes:
//
//	0 — program was rejected (the expected demo outcome, log printed) and
//	    the KFunc walk returned the right sum, or was skipped because
//	    bpf/kfunc_walk.bpf.o / s3fs_kfunc is not available
//	1 — I/O or setup failure, or the KFunc walk returned a wrong result
//	2 — program unexpectedly loaded (a future kernel accepted it —
//	    the demo is obsolete and someone should update the pitch)
func runVerifierDemo(args []string) int {
	fs := flag.NewFlagSet("verifier-demo", flag.ExitOnError)
	kfuncObj := fs.String("kfunc-obj", defaultKfuncWalkObj(),
		"BPF object holding the KFunc-based walk (bpf/kfunc_walk.bpf.o); \"\" skips that half")
	nodes := fs.Int("nodes", 100000,
		"linked-list length for the KFunc walk (the reject half fails at any length)")
	fs.Parse(args)

	fmt.Println("== verifier-demo ==============================================")
	fmt.Println("Attempting to load s3fs_deep_walk_reject (pure eBPF arena walk")
	fmt.Println("with an unbounded pointer chase). The kernel's verifier should")
	fmt.Println("reject it with a complexity-limit or unbounded-loop diagnostic.")
	fmt.Println()

	ve, err := fpebpf.LoadRejectDemo()
	if err != nil {
		fmt.Fprintf(os.Stderr, "verifier-demo: %v\n", err)
		return 1
	}
	if ve == nil {
		fmt.Fprintln(os.Stderr, "verifier-demo: program loaded unexpectedly — the demo is obsolete")
		return 2
	}
	fmt.Printf("=== verifier log ===\n%+v\n", ve)
	fmt.Println()

	rc, skipped := 0, true
	if *kfuncObj != "" {
		rc, skipped = runKfuncWalkDemo(*kfuncObj, *nodes)
		fmt.Println()
	}

	fmt.Println("== outcome ====================================================")
	fmt.Println("The verifier refused the pure-eBPF walk. The same walk handed to")
	fmt.Println("s3fs_kfunc_arena_list_walk (kmod/tree_traversal.c, called from")
	fmt.Println("bpf/kfunc_walk.bpf.c) loads and runs: the loop is ordinary kernel")
	fmt.Println("C, bounded by max_nodes + signal checks and made memory-safe by")
	fmt.Println("copy_from_user instead of by the verifier.")
	if skipped {
		fmt.Println("(KFunc half skipped — see SKIP above.)")
	}
	return rc
}

// backendFlags registers the shared flags for backend selection
// (S3-compat via internal/s3 or native GCS) and returns a callback
// that constructs the concrete [objstore.Backend]. Backend-specific
// flags are ignored under the other --backend so envvars can stay
// set for both without conflict.
func backendFlags(fs *flag.FlagSet) func() (objstore.Backend, error) {
	backend := fs.String("backend", envOr("S3FS_BACKEND", objstore.KindS3),
		"object-store backend: s3 (MinIO / AWS / any S3-compatible endpoint) or gcs (Google Cloud Storage)")
	endpoint := fs.String("endpoint", envOr("S3FS_ENDPOINT", "localhost:9000"),
		"[s3] S3-compatible endpoint (host:port, no scheme — use --ssl for https)")
	bucket := fs.String("bucket", envOr("S3FS_BUCKET", "s3fs-test"), "Bucket name (both backends)")
	access := fs.String("access-key", envOr("S3FS_ACCESS_KEY", "minioadmin"), "[s3] Access key")
	secret := fs.String("secret-key", envOr("S3FS_SECRET_KEY", "minioadmin"), "[s3] Secret key")
	ssl := fs.Bool("ssl", false, "[s3] Use HTTPS")
	cacheDir := fs.String("cache-dir", envOr("S3FS_CACHE_DIR", ""),
		"local staging directory (enables use_cache-equivalent short-circuit; both backends)")
	diskCacheCapacity := fs.Int64("disk-cache-capacity", envInt64("S3FS_DISK_CACHE_CAPACITY", 0),
		"disk cache size cap in bytes (0 = unlimited, S-07)")
	// GOOGLE_APPLICATION_CREDENTIALS is honored by the GCS SDK's ADC
	// chain — don't pre-read it here or an unset flag with a set env
	// would bypass metadata-server / gcloud login fallbacks.
	gcsCreds := fs.String("gcs-credentials", "",
		"[gcs] path to a service-account JSON key file. Empty falls back to Application Default Credentials (GOOGLE_APPLICATION_CREDENTIALS, gcloud login, GCE metadata).")
	keyPrefix := fs.String("key-prefix", envOr("S3FS_KEY_PREFIX", ""),
		"map the mount root to this bucket key prefix (e.g. 'sub/dir/'). Empty exposes the whole bucket.")
	return func() (objstore.Backend, error) {
		var (
			b   objstore.Backend
			err error
		)
		switch *backend {
		case objstore.KindS3:
			b, err = s3.New(s3.Config{
				Endpoint:      *endpoint,
				Bucket:        *bucket,
				AccessKey:     *access,
				SecretKey:     *secret,
				UseSSL:        *ssl,
				CacheDir:      *cacheDir,
				MaxCacheBytes: *diskCacheCapacity,
			})
		case objstore.KindGCS:
			b, err = gcs.New(context.Background(), gcs.Config{
				Bucket:          *bucket,
				CredentialsFile: *gcsCreds,
				CacheDir:        *cacheDir,
				MaxCacheBytes:   *diskCacheCapacity,
			})
		default:
			return nil, fmt.Errorf("unknown --backend %q (want %s or %s)",
				*backend, objstore.KindS3, objstore.KindGCS)
		}
		if err != nil {
			return nil, err
		}
		return objstore.NewPrefixed(b, *keyPrefix), nil
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt64(key string, fallback int64) int64 {
	if v := os.Getenv(key); v != "" {
		var n int64
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil {
			return n
		}
	}
	return fallback
}

// runS3List lists objects in the configured bucket (optionally under a
// prefix) and prints them. Exit 1 on connection or listing failure.
func runS3List(args []string) int {
	fs := flag.NewFlagSet("s3-list", flag.ExitOnError)
	newClient := backendFlags(fs)
	prefix := fs.String("prefix", "", "Object key prefix to enumerate")
	fs.Parse(args)

	client, err := newClient()
	if err != nil {
		fmt.Fprintf(os.Stderr, "s3-list: %v\n", err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	objs, err := client.List(ctx, *prefix)
	if err != nil {
		fmt.Fprintf(os.Stderr, "s3-list: %v\n", err)
		return 1
	}
	fmt.Printf("bucket=%s prefix=%q count=%d\n", client.Bucket(), *prefix, len(objs))
	for _, o := range objs {
		fmt.Printf("  %s\t%d\t%s\t%s\n",
			o.Key, o.Size, o.ETag, o.LastModified.UTC().Format(time.RFC3339))
	}
	return 0
}

// runS3Stat fetches a single object's HEAD metadata and prints it.
func runS3Stat(args []string) int {
	fs := flag.NewFlagSet("s3-stat", flag.ExitOnError)
	newClient := backendFlags(fs)
	fs.Parse(args)
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "s3-stat: exactly one KEY argument required")
		return 2
	}
	key := fs.Arg(0)

	client, err := newClient()
	if err != nil {
		fmt.Fprintf(os.Stderr, "s3-stat: %v\n", err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	meta, err := client.Stat(ctx, key)
	if err != nil {
		fmt.Fprintf(os.Stderr, "s3-stat: %v\n", err)
		return 1
	}
	fmt.Printf("bucket=%s key=%s\n", client.Bucket(), meta.Key)
	fmt.Printf("  size          %d\n", meta.Size)
	fmt.Printf("  etag          %s\n", meta.ETag)
	fmt.Printf("  content-type  %s\n", meta.ContentType)
	fmt.Printf("  last-modified %s\n", meta.LastModified.UTC().Format(time.RFC3339))
	return 0
}
