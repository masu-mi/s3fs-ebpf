package main

import (
	"errors"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestOSFileCloseWakesBlockedRead pins the OS/runtime behavior
// runReadMissLoop's shutdown relies on (H-05): when a *os.File is
// Close()d while another goroutine is blocked in Read, Read must
// return promptly with io.EOF or os.ErrClosed rather than hang.
//
// This is generic Go / kernel behavior and should always hold, but
// runReadMissLoop is our only reader that depends on it, so a
// regression here would be silent in prod (the daemon would linger
// past SIGINT). Belt-and-suspenders: the production path also has a
// 5s shutdown-warn log (see readMissShutdownWarn) that would surface
// a hang, but we'd rather fail a test than wait on a bench.
func TestOSFileCloseWakesBlockedRead(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	defer w.Close()

	readErr := make(chan error, 1)
	go func() {
		var buf [8]byte
		_, err := r.Read(buf[:])
		readErr <- err
	}()

	// Give Read a chance to actually block before we close.
	time.Sleep(20 * time.Millisecond)
	if err := r.Close(); err != nil {
		t.Fatalf("r.Close: %v", err)
	}

	select {
	case err := <-readErr:
		if !errors.Is(err, io.EOF) && !errors.Is(err, os.ErrClosed) {
			t.Fatalf("Read after Close returned %v, want io.EOF or os.ErrClosed", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Read did not wake within 500ms of Close — runReadMissLoop shutdown would hang")
	}
}

// TestEnforceCapacityAfterPopulate pins the AC-45 C2 policy: no pass when
// kmod is not in capacity mode, and concurrent misses share a single
// follow-up pass instead of one pass each.
func TestEnforceCapacityAfterPopulate(t *testing.T) {
	origFn, origCap := capacityEnforceFn, capacityBytesFn
	defer func() { capacityEnforceFn, capacityBytesFn = origFn, origCap }()

	var calls int32
	capacityEnforceFn = func(string) (int, error) {
		atomic.AddInt32(&calls, 1)
		return 0, nil
	}

	for name, fn := range map[string]func() (int64, bool){
		"capacity off":    func() (int64, bool) { return 0, true },
		"param missing":   func() (int64, bool) { return 0, false },
		"legacy (-1 val)": func() (int64, bool) { return -1, true },
	} {
		capacityBytesFn = fn
		enforceCapacityAfterPopulate("/dev/null")
		if got := atomic.LoadInt32(&calls); got != 0 {
			t.Fatalf("%s: %d passes, want 0", name, got)
		}
	}

	capacityBytesFn = func() (int64, bool) { return 4 << 20, true }
	enforceCapacityAfterPopulate("/dev/null")
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("capacity mode: %d passes, want 1", got)
	}

	// While one pass is running, N more misses must collapse into one
	// follow-up pass (2 passes total), not N.
	atomic.StoreInt32(&calls, 0)
	started := make(chan struct{})
	release := make(chan struct{})
	capacityEnforceFn = func(string) (int, error) {
		if atomic.AddInt32(&calls, 1) == 1 {
			close(started)
			<-release
		}
		return 0, nil
	}
	done := make(chan struct{})
	go func() { enforceCapacityAfterPopulate("/dev/null"); close(done) }()
	<-started
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); enforceCapacityAfterPopulate("/dev/null") }()
	}
	wg.Wait() // joiners return immediately
	close(release)
	<-done
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("coalesced: %d passes, want 2", got)
	}
}
