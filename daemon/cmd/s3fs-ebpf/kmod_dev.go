package main

import (
	"fmt"
	"os"
)

// sharedDevFile holds the single /dev/s3fs_ebpf fd opened by openSharedDev.
// All ioctls reuse this fd instead of opening per-call (V-01).
// Nil until openSharedDev is called; fallback to per-call open when nil
// so tests without a real device still compile and run.
var sharedDevFile *os.File

// openSharedDev opens devPath once and stores it for all ioctl helpers.
// The caller (runFastpath) is responsible for closing it on exit.
func openSharedDev(devPath string) (*os.File, error) {
	f, err := os.OpenFile(devPath, os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", devPath, err)
	}
	sharedDevFile = f
	return f, nil
}

// devFileOrOpen returns sharedDevFile when available, otherwise opens
// devPath for a single call (offline mode / test fallback). The returned
// close func must be called; it is a no-op when the shared fd was used.
func devFileOrOpen(devPath string) (f *os.File, close func(), err error) {
	if sharedDevFile != nil {
		return sharedDevFile, func() {}, nil
	}
	f, err = os.OpenFile(devPath, os.O_RDWR, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("open %s: %w", devPath, err)
	}
	return f, func() { f.Close() }, nil
}

// capacityBytesFn reads the kmod's capacity_bytes parameter (0 = capacity
// mode off, legacy softcap voting).  A var so tests need no sysfs.
var capacityBytesFn = func() (int64, bool) { return getKmodParamInt64("capacity_bytes") }
