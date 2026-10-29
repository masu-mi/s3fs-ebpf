// fastpath subcommand: BPF collection load, fentry attach, miss-event loop.

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

	fpebpf "github.com/bbsakura/s3fs-ebpf/daemon/internal/ebpf"

	"github.com/bbsakura/s3fs-ebpf/daemon/internal/metrics"
	"github.com/bbsakura/s3fs-ebpf/daemon/internal/objstore"
	"github.com/bbsakura/s3fs-ebpf/daemon/internal/policy"
	"github.com/bbsakura/s3fs-ebpf/daemon/internal/policyprog"
	"github.com/cilium/ebpf/ringbuf"
)

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
	partialModify := fs.Bool("partial-modify", false,
		"AC-51: a write into a large existing object loads only the chunks it touches and flushes the rest with a server-side copy (S3 only; sets the kmod partial_modify parameter)")
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
	arenaMetadataOnly := fs.Bool("arena-metadata-only", false,
		"[AB-04] store S3 object data in the kernel page cache (via fd->buf) instead of "+
			"the BPF arena. Reads are served by generic_file_read_iter from the page "+
			"cache — no GUP per page. Eliminates the 1.7µs/page GUP overhead at the "+
			"cost of one extra copy on admit. Does not require BPF arena allocation. "+
			"Prewarm warms metadata only; bodies are fetched on the first read (AC-44).")
	pcachePrewarm := fs.Bool("pcache-prewarm", false,
		"[AC-67] with --arena-metadata-only, read each file the prewarm policy selects "+
			"once through the mount after startup (and after each sync round with "+
			"--prewarm-on-sync), so its first read is a page-cache hit. Reads use the "+
			"normal miss path. Stops before capacity_bytes would be exceeded; set "+
			"--cache-capacity, since the default capacity is unlimited.")
	pcachePrewarmMnt := fs.String("pcache-prewarm-mount", "",
		"[AC-67] mount point --pcache-prewarm reads through. Empty: the first "+
			"s3fs_ebpf mount in /proc/self/mounts (waits up to 30 s for it).")
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
		s3fifoGhostMap    = "s3fifo3_state"
		s3fifoGhostMinCap = uint32(8192)
		s3fifoGhostMaxCap = uint32(65536)
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
	arenaMetadataOnlyEnabled = *arenaMetadataOnly
	if *pcachePrewarm && !*arenaMetadataOnly {
		fmt.Fprintln(os.Stderr, "--pcache-prewarm requires --arena-metadata-only (arena mode already prewarms content)")
		return 1
	}
	pcachePrewarmEnabled = *pcachePrewarm
	pcachePrewarmMount = *pcachePrewarmMnt

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
	// AC-68: prefetch_next reads siblings through the mount into the
	// pcache; arena mode has no such path.
	if pol.HasPrefetchNext() {
		if arenaMetadataOnlyEnabled {
			prefetchPolicy = pol
		} else {
			fmt.Fprintln(os.Stderr, "[policy] prefetch_next ignored: needs --arena-metadata-only")
		}
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
		if p, ok := s3c.(interface{ Prefix() string }); ok {
			prefixStr = " key-prefix=" + p.Prefix()
		}
		fmt.Printf("[fastpath] object backend: bucket=%s%s (will warm arena on miss)\n",
			s3c.Bucket(), prefixStr)
		// The module outlives the daemon, so a value left by an earlier run
		// (--partial-modify on S3) would stay on for a backend that cannot
		// copy: always write the value, "0" included.
		_, canCopy := s3c.(objstore.MultipartCopier)
		pm := "0"
		switch {
		case *partialModify && canCopy:
			pm = "1"
		case *partialModify:
			fmt.Fprintln(os.Stderr, "[kmod] --partial-modify ignored: backend has no server-side copy")
		}
		if err := setKmodParam("partial_modify", pm); err != nil {
			fmt.Fprintf(os.Stderr, "[kmod] %v\n", err)
		}
	} else {
		if err := setKmodParam("partial_modify", "0"); err != nil {
			fmt.Fprintf(os.Stderr, "[kmod] %v\n", err)
		}
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
		// AC-67: the prewarm reads block on upcalls, so start after
		// runReadMissLoop.
		if pcachePrewarmEnabled || prefetchPolicy != nil {
			go runPcachePrewarm(ctx)
		}
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
