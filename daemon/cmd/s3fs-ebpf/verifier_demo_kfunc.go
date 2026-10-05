package main

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/cilium/ebpf"

	"github.com/bbsakura/s3fs-ebpf/daemon/internal/arena"
)

// walkArgs mirrors struct s3fs_walk_args in bpf/kfunc_walk.bpf.c.
type walkArgs struct {
	HeadUaddr uint64
	OutSum    uint64
	OutRet    int64
	MaxNodes  uint32
	_         uint32
}

// walkNodeSize is sizeof({u64 next; u64 payload;}) — the node layout
// shared by s3fs_deep_walk_reject, bpf/kfunc_walk.bpf.c and
// s3fs_kfunc_arena_list_walk.
const walkNodeSize = 16

// defaultKfuncWalkObj finds bpf/kfunc_walk.bpf.o relative to the cwd
// (repo root or bin/), same lookup the --policy-bpf builtin: prefix uses.
func defaultKfuncWalkObj() string {
	cwd, _ := os.Getwd()
	for _, root := range []string{cwd, filepath.Dir(cwd)} {
		p := filepath.Join(root, "bpf", "kfunc_walk.bpf.o")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return filepath.Join(cwd, "bpf", "kfunc_walk.bpf.o")
}

// runKfuncWalkDemo is the second half of verifier-demo: build the same
// arena linked list the rejected program would walk, then walk it
// through s3fs_kfunc_arena_list_walk from a SEC("syscall") program.
//
// Returns 0 on a correct walk, 1 on a wrong result, and skipped=true
// (rc 0) when a prerequisite is missing (object not built, kmod not
// loaded) so the reject half of the demo still works standalone.
func runKfuncWalkDemo(objPath string, nodes int) (rc int, skipped bool) {
	fmt.Println("== KFunc counterpart ==========================================")
	if nodes < 1 {
		nodes = 1
	}
	if _, err := os.Stat(objPath); err != nil {
		fmt.Printf("SKIP: %s not found (run `make bpf`).\n", objPath)
		return 0, true
	}

	pages := uint32((nodes*walkNodeSize + 4095) / 4096)
	a, err := arena.New(pages)
	if err != nil {
		fmt.Fprintf(os.Stderr, "verifier-demo: %v\n", err)
		return 1, false
	}
	defer a.Close()

	// node[i].next = &node[i+1] (arena user_vm address), last = 0.
	base := a.BaseAddr()
	buf := make([]byte, nodes*walkNodeSize)
	var want uint64
	for i := 0; i < nodes; i++ {
		var next uint64
		if i+1 < nodes {
			next = base + uint64(i+1)*walkNodeSize
		}
		payload := uint64(i + 1)
		want += payload
		binary.LittleEndian.PutUint64(buf[i*walkNodeSize:], next)
		binary.LittleEndian.PutUint64(buf[i*walkNodeSize+8:], payload)
	}
	if _, err := a.WriteAt(buf, 0); err != nil {
		fmt.Fprintf(os.Stderr, "verifier-demo: arena write: %v\n", err)
		return 1, false
	}
	fmt.Printf("built a %d-node linked list in a %d-page arena (head=0x%x)\n",
		nodes, pages, base)

	spec, err := ebpf.LoadCollectionSpec(objPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "verifier-demo: parse %s: %v\n", objPath, err)
		return 1, false
	}
	coll, err := ebpf.NewCollection(spec)
	if err != nil {
		// The __ksym only resolves while s3fs_kfunc.ko is loaded.
		fmt.Printf("SKIP: load %s failed (is s3fs_kfunc loaded? `sudo insmod kmod/s3fs_kfunc.ko`): %v\n",
			filepath.Base(objPath), err)
		return 0, true
	}
	defer coll.Close()
	prog := coll.Programs["s3fs_deep_walk_kfunc"]
	if prog == nil {
		fmt.Fprintf(os.Stderr, "verifier-demo: s3fs_deep_walk_kfunc missing from %s\n", objPath)
		return 1, false
	}
	fmt.Println("s3fs_deep_walk_kfunc loaded: the verifier accepted it (the loop lives in the KFunc).")

	in := walkArgs{HeadUaddr: base, MaxNodes: uint32(nodes) + 1}
	var out walkArgs
	start := time.Now()
	if _, err := prog.Run(&ebpf.RunOptions{Context: in, ContextOut: &out}); err != nil {
		fmt.Fprintf(os.Stderr, "verifier-demo: BPF_PROG_TEST_RUN: %v\n", err)
		return 1, false
	}
	elapsed := time.Since(start)

	if out.OutRet < 0 {
		fmt.Fprintf(os.Stderr, "verifier-demo: kfunc returned %d (%v)\n",
			out.OutRet, syscall.Errno(-out.OutRet))
		return 1, false
	}
	fmt.Printf("walked %d nodes in %s, sum=%d (want %d)\n", out.OutRet, elapsed, out.OutSum, want)
	if out.OutRet != int64(nodes) || out.OutSum != want {
		fmt.Fprintln(os.Stderr, "verifier-demo: KFunc walk result mismatch")
		return 1, false
	}
	return 0, false
}
