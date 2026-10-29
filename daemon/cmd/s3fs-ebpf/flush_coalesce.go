package main

import (
	"context"
	"time"

	"github.com/bbsakura/s3fs-ebpf/daemon/internal/coalesce"
	fpebpf "github.com/bbsakura/s3fs-ebpf/daemon/internal/ebpf"
	"github.com/bbsakura/s3fs-ebpf/daemon/internal/objstore"
)

// coalescer debounces flush events per ino (internal/coalesce); each one that
// survives its window is flushed with handleFlush.
type coalescer = coalesce.Coalescer

func newCoalescer(devFd int, devPath string,
	progs *fpebpf.Programs, s3c objstore.Backend, window time.Duration) *coalescer {
	return coalesce.New(window, func(ctx context.Context, ev *flushEvent) {
		handleFlush(ctx, devFd, devPath, progs, s3c, ev)
	})
}
