package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"
	"unsafe"

	fpebpf "github.com/bbsakura/s3fs-ebpf/daemon/internal/ebpf"
	"github.com/bbsakura/s3fs-ebpf/daemon/internal/objstore"
	"golang.org/x/sys/unix"
)

func popDelete(devFd int, out *deleteEvent) error {
	_, _, errno := unix.Syscall(unix.SYS_IOCTL,
		uintptr(devFd), uintptr(s3fsIocPopDelete),
		uintptr(unsafe.Pointer(out)))
	if errno != 0 {
		return errno
	}
	return nil
}

func drainDeletes(ctx context.Context, devFd int, s3c objstore.Backend) {
	drainDeletesWith(ctx, func(ev *deleteEvent) error { return popDelete(devFd, ev) }, s3c)
}

// drainDeletesWith services delete events from pop until the queue is empty
// (EAGAIN) or pop fails.
func drainDeletesWith(ctx context.Context, pop func(*deleteEvent) error, s3c objstore.Backend) {
	for {
		var ev deleteEvent
		if err := pop(&ev); err != nil {
			if errors.Is(err, unix.EAGAIN) {
				return
			}
			fmt.Fprintf(os.Stderr, "[delete] pop: %v\n", err)
			return
		}
		handleDelete(ctx, s3c, &ev)
	}
}

func handleDelete(ctx context.Context, s3c objstore.Backend, ev *deleteEvent) {
	name := string(ev.Name[:ev.NameLen])
	s3key := resolveEventS3Key(ev.Ino, ev.ParentIno, name)
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

func popRename(devFd int, out *renameEvent) error {
	_, _, errno := unix.Syscall(unix.SYS_IOCTL,
		uintptr(devFd), uintptr(s3fsIocPopRename),
		uintptr(unsafe.Pointer(out)))
	if errno != 0 {
		return errno
	}
	return nil
}

func drainRenames(ctx context.Context, devFd int, s3c objstore.Backend) {
	drainRenamesWith(ctx, func(ev *renameEvent) error { return popRename(devFd, ev) }, s3c)
}

// drainRenamesWith services rename events from pop until the queue is empty
// (EAGAIN) or pop fails.
func drainRenamesWith(ctx context.Context, pop func(*renameEvent) error, s3c objstore.Backend) {
	for {
		var ev renameEvent
		if err := pop(&ev); err != nil {
			if errors.Is(err, unix.EAGAIN) {
				return
			}
			fmt.Fprintf(os.Stderr, "[rename] pop: %v\n", err)
			return
		}
		handleRename(ctx, s3c, &ev)
	}
}

func handleRename(ctx context.Context, s3c objstore.Backend, ev *renameEvent) {
	oldName := string(ev.OldName[:ev.OldNameLen])
	newName := string(ev.NewName[:ev.NewNameLen])
	oldKey := resolveEventS3Key(ev.Ino, ev.OldParentIno, oldName)
	newKey := resolveKeyPath(ev.NewParentIno, newName)
	if s3c == nil {
		fmt.Printf("[rename] drop ino=%d %s→%s (no S3 backend)\n",
			ev.Ino, oldKey, newKey)
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
	fpebpf.RecordKey(ev.Ino, ev.NewParentIno, newName, newKey, 0) // AC-82
	fmt.Printf("[rename] ino=%d s3://%s → s3://%s\n", ev.Ino, oldKey, newKey)
}
