// s3fs-ebpf CLI entry.
//
// Subcommands:
//   mount    - load kmod, load eBPF, create arena, mount FS
//   unmount  - reverse of mount
//   stats    - print fast/slow path counters
//   policy   - hot-reload the Lua/WASM policy blob
//
// Week 0 skeleton: subcommands parse flags and exit; each returns
// ErrNotImplemented from the corresponding internal package. Wired so
// `s3fs-ebpf --help` and `s3fs-ebpf mount --help` produce real output.
package main

import (
	"flag"
	"fmt"
	"os"
)

const usage = `Usage: s3fs-ebpf <command> [options]

Commands:
  mount    Mount an S3 bucket via the s3fs_ebpf VFS
  unmount  Unmount a previously mounted target
  stats    Print fast/slow path counters
  policy   Hot-reload the cache policy blob

Run "s3fs-ebpf <command> --help" for command-specific flags.
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
	case "stats":
		os.Exit(runStats(args))
	case "policy":
		os.Exit(runPolicy(args))
	case "-h", "--help", "help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n%s", sub, usage)
		os.Exit(2)
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
	fmt.Fprintln(os.Stderr, "mount: skeleton only — not implemented yet (see docs/motivation.md Week 1)")
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
	fmt.Fprintln(os.Stderr, "policy: skeleton only — not implemented yet (see docs/motivation.md Week 3)")
	return 0
}
