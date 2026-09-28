package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"
	"unsafe"

	fpebpf "github.com/bbsakura/s3fs-ebpf/daemon/internal/ebpf"
	"github.com/bbsakura/s3fs-ebpf/daemon/internal/s3"
	"golang.org/x/sys/unix"
)

// deleteEvent mirrors `struct s3fs_delete_event`. Layout identical
// to flushEvent minus the Size field — kept as a separate type so
// the ioctl size encoding matches kmod's expected sizeof.
type deleteEvent struct {
	Ino     uint64
	NameLen uint32
	_pad    uint32
	Name    [syncNameMax + 1]byte
}

const s3fsIocPopDelete = (2 << 30) | (uint32(unsafe.Sizeof(deleteEvent{})) << 16) | (0xE3 << 8) | 0x0B

func popDelete(devFd int, out *deleteEvent) error {
	_, _, errno := unix.Syscall(unix.SYS_IOCTL,
		uintptr(devFd), uintptr(s3fsIocPopDelete),
		uintptr(unsafe.Pointer(out)))
	if errno != 0 {
		return errno
	}
	return nil
}

// runDeleteLoop drains kmod's delete queue on `interval` and issues
// S3 DELETE for each event. Symmetric with runFlushLoop; both run
// on the same /dev/s3fs_ebpf fd if the caller chooses. Best-effort:
// a failed DELETE logs to stderr; the entry stays on S3 until the
// next .unlink (which the user has to reissue). Persistent
// delete-retry queue is backlog H-01 territory.
func runDeleteLoop(ctx context.Context, devPath string, s3c *s3.Client,
	interval time.Duration) {
	f, err := os.OpenFile(devPath, os.O_RDWR, 0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[delete] open %s: %v — delete-drain disabled\n",
			devPath, err)
		return
	}
	defer f.Close()

	devFd := int(f.Fd())
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			drainDeletes(ctx, devFd, s3c)
		}
	}
}

func drainDeletes(ctx context.Context, devFd int, s3c *s3.Client) {
	for {
		var ev deleteEvent
		if err := popDelete(devFd, &ev); err != nil {
			if errors.Is(err, unix.EAGAIN) {
				return
			}
			fmt.Fprintf(os.Stderr, "[delete] pop: %v\n", err)
			return
		}
		handleDelete(ctx, s3c, &ev)
	}
}

func handleDelete(ctx context.Context, s3c *s3.Client, ev *deleteEvent) {
	name := string(ev.Name[:ev.NameLen])
	// Resolve S3 key from the sync mirror; fall back to name (flat
	// bucket at PoC scope). Same shape as handleFlush.
	s3key := name
	if e := fpebpf.LookupByIno(ev.Ino); e != nil {
		s3key = e.S3Key
	}
	if s3c == nil {
		fmt.Printf("[delete] drop ino=%d name=%s (no S3 backend)\n",
			ev.Ino, name)
		return
	}
	putCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := s3c.Delete(putCtx, s3key); err != nil {
		fmt.Fprintf(os.Stderr, "[delete] ino=%d s3://%s delete: %v\n",
			ev.Ino, s3key, err)
		return
	}
	fmt.Printf("[delete] ino=%d s3://%s deleted\n", ev.Ino, s3key)
}

// renameEvent mirrors `struct s3fs_rename_event`. Carries both
// names in one payload for CopyObject + Delete.
type renameEvent struct {
	Ino        uint64
	OldNameLen uint32
	NewNameLen uint32
	OldName    [syncNameMax + 1]byte
	NewName    [syncNameMax + 1]byte
}

const s3fsIocPopRename = (2 << 30) | (uint32(unsafe.Sizeof(renameEvent{})) << 16) | (0xE3 << 8) | 0x0C

func popRename(devFd int, out *renameEvent) error {
	_, _, errno := unix.Syscall(unix.SYS_IOCTL,
		uintptr(devFd), uintptr(s3fsIocPopRename),
		uintptr(unsafe.Pointer(out)))
	if errno != 0 {
		return errno
	}
	return nil
}

// runRenameLoop drains kmod's rename queue and issues
// s3c.Rename (server-side CopyObject + Delete). Same ticker cadence
// as delete/flush.
func runRenameLoop(ctx context.Context, devPath string, s3c *s3.Client,
	interval time.Duration) {
	f, err := os.OpenFile(devPath, os.O_RDWR, 0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[rename] open %s: %v — rename-drain disabled\n",
			devPath, err)
		return
	}
	defer f.Close()

	devFd := int(f.Fd())
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			drainRenames(ctx, devFd, s3c)
		}
	}
}

func drainRenames(ctx context.Context, devFd int, s3c *s3.Client) {
	for {
		var ev renameEvent
		if err := popRename(devFd, &ev); err != nil {
			if errors.Is(err, unix.EAGAIN) {
				return
			}
			fmt.Fprintf(os.Stderr, "[rename] pop: %v\n", err)
			return
		}
		handleRename(ctx, s3c, &ev)
	}
}

func handleRename(ctx context.Context, s3c *s3.Client, ev *renameEvent) {
	oldName := string(ev.OldName[:ev.OldNameLen])
	newName := string(ev.NewName[:ev.NewNameLen])
	oldKey := oldName
	if e := fpebpf.LookupByIno(ev.Ino); e != nil {
		oldKey = e.S3Key
	}
	newKey := newName
	if s3c == nil {
		fmt.Printf("[rename] drop ino=%d %s→%s (no S3 backend)\n",
			ev.Ino, oldName, newName)
		return
	}
	putCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := s3c.Rename(putCtx, oldKey, newKey); err != nil {
		fmt.Fprintf(os.Stderr, "[rename] ino=%d %s→%s: %v\n",
			ev.Ino, oldKey, newKey, err)
		return
	}
	// Rewire the ino → S3Key map so subsequent flushes / deletes
	// use the new key.
	registerIno(newKey, ev.Ino)
	fmt.Printf("[rename] ino=%d s3://%s → s3://%s\n", ev.Ino, oldKey, newKey)
}
