package main

import (
	"errors"
	"io"
	"os"
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
