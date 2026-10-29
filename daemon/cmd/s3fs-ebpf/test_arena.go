// dev test-arena: Week 1 bpf_arena + KFunc round-trip probe.

package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"os"

	fpebpf "github.com/bbsakura/s3fs-ebpf/daemon/internal/ebpf"

	"github.com/bbsakura/s3fs-ebpf/daemon/internal/arena"
)

const (
	testInodes       = 32
	inodeStubSize    = 32 // sizeof(struct s3fs_inode_stub)
	testIndex        = 3
	testKfuncArg     = 21
	expectedKfuncOut = testKfuncArg*2 + 1 // s3fs_kfunc_echo(v) = v*2 + 1
	testInoBase      = uint64(0x1000)
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
