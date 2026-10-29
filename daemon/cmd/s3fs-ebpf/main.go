// s3fs-ebpf CLI entry.
//
// Operational subcommands (top level):
//
//	mount        - load kmod, load eBPF, create arena, mount FS
//	unmount      - reverse of mount
//	fastpath     - run the fastpath daemon (fentry attach, S3 sync, arena)
//	stats        - print fast/slow path counters
//	policy       - hot-reload the cache policy blob
//	hash-policy  - print SHA256 for a policy .bpf.o (allowlist entry helper)
//	s3-list      - list objects in a bucket
//	s3-stat      - stat a single object
//
// Development / probe / demo subcommands (under "dev"):
//
//	dev test-arena    - Week 1 bpf_arena + KFunc round-trip probe
//	dev verifier-demo - Week 2 verifier rejection demo
//	dev probe-kvbase  - Week 4 arena ioctl kvbase probe
//	dev microbench    - tight-loop stat/read latency bench
//	dev trace-replay  - offline policy hit-rate simulator
//
// mount/unmount/stats/policy remain stubbed until Week 2+.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/bbsakura/s3fs-ebpf/daemon/internal/policyprog"
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
