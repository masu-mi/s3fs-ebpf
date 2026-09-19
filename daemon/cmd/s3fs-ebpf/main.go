// s3fs-ebpf CLI entry.
//
// Subcommands:
//   mount       - load kmod, load eBPF, create arena, mount FS
//   unmount     - reverse of mount
//   stats       - print fast/slow path counters
//   policy      - hot-reload the Lua/WASM policy blob
//   test-arena  - Week 1 round-trip probe: verify bpf_arena creation,
//                 daemon<->BPF shared memory, and KFunc call from BPF
//
// mount/unmount/stats/policy remain stubbed until Week 2+.
package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"os"

	"github.com/bbsakura/s3fs-ebpf/daemon/internal/arena"
	"github.com/bbsakura/s3fs-ebpf/daemon/internal/ebpf"
)

const usage = `Usage: s3fs-ebpf <command> [options]

Commands:
  mount        Mount an S3 bucket via the s3fs_ebpf VFS
  unmount      Unmount a previously mounted target
  stats        Print fast/slow path counters
  policy       Hot-reload the cache policy blob
  test-arena   Week 1 probe: bpf_arena + KFunc round-trip (requires 6.9+ kernel
               and the s3fs_kfunc module loaded)

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
	case "test-arena":
		os.Exit(runTestArena(args))
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
	fmt.Fprintln(os.Stderr, "mount: skeleton only — not implemented yet (see docs/motivation.md Week 2)")
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

// -- Week 1: test-arena -------------------------------------------------------

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
	fmt.Println("== subtest B+C: mmap shared array + KFunc call ================")
	progs, err := ebpf.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "[FAIL B] ebpf.Load: %v\n", err)
		return 1
	}
	defer progs.Close()

	// Populate the shared array with dummy inodes: ino = base + i.
	shared := progs.SharedMap()
	for i := uint32(0); i < testInodes; i++ {
		key := i
		val := make([]byte, inodeStubSize)
		binary.LittleEndian.PutUint64(val[0:8], testInoBase+uint64(i))
		binary.LittleEndian.PutUint64(val[8:16], 4096)               // size
		binary.LittleEndian.PutUint64(val[16:24], 0)                 // mtime_ns
		binary.LittleEndian.PutUint32(val[24:28], 0o100644)          // mode
		if err := shared.Update(&key, val, 0); err != nil {
			fmt.Fprintf(os.Stderr, "[FAIL B] populate shared[%d]: %v\n", i, err)
			return 1
		}
	}
	fmt.Printf("[B] wrote %d dummy inodes to shared map\n", testInodes)

	in := ebpf.TestArgs{Idx: idx, KfuncArg: kfuncArg}
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
