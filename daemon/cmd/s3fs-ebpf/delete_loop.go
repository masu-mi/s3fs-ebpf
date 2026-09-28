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
