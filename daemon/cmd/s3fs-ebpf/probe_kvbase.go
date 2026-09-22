package main

import (
	"flag"
	"fmt"
	"os"
	"unsafe"

	fpebpf "github.com/bbsakura/s3fs-ebpf/daemon/internal/ebpf"
	"golang.org/x/sys/unix"
)

// probeKvbaseArg mirrors `struct s3fs_probe_kvbase` in
// include/s3fs_ebpf_uapi.h. Layout is fixed by the kmod's ioctl handler.
type probeKvbaseArg struct {
	ArenaFD   uint32
	_pad      uint32
	OutKvbase uint64
	OutSize   uint64
}

// s3fsIocProbeKvbase encodes _IOWR(0xE3, 0x01, struct s3fs_probe_kvbase).
// _IOWR direction is 3; size = sizeof(probeKvbaseArg) = 24 bytes.
const s3fsIocProbeKvbase = (3 << 30) | (uint32(unsafe.Sizeof(probeKvbaseArg{})) << 16) | (0xE3 << 8) | 0x01

// runProbeKvbase is the Week 4 (a) spike: it verifies that the
// container_of-based extraction of struct bpf_arena.kern_vm->addr
// works on the running kernel by comparing kmod's ioctl output against
// the daemon's mmap base.
func runProbeKvbase(args []string) int {
	fs := flag.NewFlagSet("probe-kvbase", flag.ExitOnError)
	dev := fs.String("dev", "/dev/s3fs_ebpf", "kmod control device")
	fs.Parse(args)

	progs, err := fpebpf.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "load bpf collection: %v\n", err)
		return 1
	}
	defer progs.Close()

	arenaFD := progs.Arena().Map.FD()
	userVM := progs.Arena().BaseAddr()
	userVMSize := progs.Arena().Size()
	fmt.Printf("[probe] arena fd=%d user_vm_base=0x%x user_vm_size=%d\n",
		arenaFD, userVM, userVMSize)

	f, err := os.OpenFile(*dev, os.O_RDWR, 0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open %s: %v (is s3fs_kfunc loaded?)\n", *dev, err)
		return 1
	}
	defer f.Close()

	arg := probeKvbaseArg{ArenaFD: uint32(arenaFD)}
	// Raw syscall: unix.IoctlSetInt only handles integer args, and the
	// generic Ioctl* wrappers don't cover struct-out ioctls with mixed
	// in/out fields. This is a two-line workaround.
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL,
		f.Fd(), uintptr(s3fsIocProbeKvbase),
		uintptr(unsafe.Pointer(&arg))); errno != 0 {
		fmt.Fprintf(os.Stderr, "ioctl PROBE_KVBASE: %v\n", errno)
		return 1
	}

	fmt.Printf("[probe] kern_vm_base=0x%x kern_vm_size=%d\n",
		arg.OutKvbase, arg.OutSize)
	if arg.OutKvbase == 0 {
		fmt.Fprintln(os.Stderr, "[probe] FAIL: kern_vm_base is zero")
		return 2
	}
	if arg.OutKvbase == userVM {
		fmt.Fprintln(os.Stderr, "[probe] FAIL: kern_vm_base == user_vm_base (no cast happened)")
		return 3
	}
	fmt.Println("[probe] OK: kern_vm and user_vm are distinct — Path A′ is viable")
	return 0
}
