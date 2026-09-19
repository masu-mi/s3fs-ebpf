// Package mounter wraps the mount(2) syscall so the CLI can install the
// s3fs_ebpf filesystem at a target directory and hand the kernel module
// the bpf_arena map id via mount options.
package mounter

import (
	"fmt"
	"syscall"
)

// Options carries the mount parameters.
type Options struct {
	Target string
	MapID  uint32
}

// Mount performs `mount -t s3fs_ebpf none <target> -o map_id=<id>`.
func Mount(o Options) error {
	data := fmt.Sprintf("map_id=%d", o.MapID)
	if err := syscall.Mount("none", o.Target, "s3fs_ebpf", 0, data); err != nil {
		return fmt.Errorf("mount %s: %w", o.Target, err)
	}
	return nil
}

// Unmount detaches the filesystem.
func Unmount(target string) error {
	if err := syscall.Unmount(target, 0); err != nil {
		return fmt.Errorf("umount %s: %w", target, err)
	}
	return nil
}
