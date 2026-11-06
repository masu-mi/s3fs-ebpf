package main

import (
	"fmt"
	"os"
	"sync"
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

// AC-45 C2: capacity enforcement after a pcache populate.
//
// kmod only evicts by capacity inside its capacity pass, and a pcache miss
// (populate ioctls + signal_admit) does not run it, so fd->buf kept growing
// until a policy eval or sync happened to come along.  After the last
// populate of a miss, and before signal_admit, handleReadMiss asks kmod to
// run its capacity pass (the EVAL ioctl).  The chunk just populated is
// still pending, so the pass cannot take it from the reader that waits for
// it; older files are evicted by policy score.  Concurrent misses are
// coalesced: one pass runs, and misses that arrive meanwhile share one
// follow-up pass.

// capacityEnforceFn is the pass itself; a var so tests can count calls.
var capacityEnforceFn = evalPass

// capacityBytesFn reads the kmod's capacity_bytes parameter (0 = capacity
// mode off, legacy softcap voting).  A var so tests need no sysfs.
var capacityBytesFn = func() (int64, bool) { return getKmodParamInt64("capacity_bytes") }

var (
	capEnforceMu      sync.Mutex
	capEnforceRunning bool
	capEnforceAgain   bool
)

// enforceCapacityAfterPopulate runs (or joins) a capacity pass when kmod is
// in capacity mode.  Errors are logged only: the miss itself succeeded and
// the periodic loops remain the fallback.
func enforceCapacityAfterPopulate(devPath string) {
	if c, ok := capacityBytesFn(); !ok || c <= 0 {
		return
	}
	capEnforceMu.Lock()
	if capEnforceRunning {
		capEnforceAgain = true
		capEnforceMu.Unlock()
		return
	}
	capEnforceRunning = true
	capEnforceMu.Unlock()

	for {
		if _, err := capacityEnforceFn(devPath); err != nil {
			fmt.Fprintf(os.Stderr, "[capacity] post-populate pass: %v\n", err)
		}
		capEnforceMu.Lock()
		if !capEnforceAgain {
			capEnforceRunning = false
			capEnforceMu.Unlock()
			return
		}
		capEnforceAgain = false
		capEnforceMu.Unlock()
	}
}
