package main

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	fpebpf "github.com/bbsakura/s3fs-ebpf/daemon/internal/ebpf"
	"github.com/bbsakura/s3fs-ebpf/daemon/internal/metrics"
	"github.com/bbsakura/s3fs-ebpf/daemon/internal/objstore"
	"golang.org/x/sys/unix"
)

// readMissEvent mirrors `struct s3fs_read_miss_event` in
// include/s3fs_ebpf_uapi.h. Layout is fixed by kmod's .read handler.
type readMissEvent struct {
	Ino    uint64
	Offset int64
	Count  uint32
	Flags  uint32
	// SeqCount / RandCount: the file's read pattern so far, counted by the
	// kmod in .read_iter (what the read_iter fentry used to keep in a BPF map).
	SeqCount  uint32
	RandCount uint32
}

// readMissEventSize is sizeof(struct s3fs_read_miss_event): ino, offset,
// count, flags, seq_count, rand_count.
const readMissEventSize = 32

// decodeReadMissEvent parses the kmod's event (little endian, as read from
// /dev/s3fs_ebpf).  A buffer shorter than the struct (a pre-v2 kmod sent 24
// bytes) is refused.
func decodeReadMissEvent(buf []byte) (readMissEvent, bool) {
	if len(buf) < readMissEventSize {
		return readMissEvent{}, false
	}
	return readMissEvent{
		Ino:       binary.LittleEndian.Uint64(buf[0:8]),
		Offset:    int64(binary.LittleEndian.Uint64(buf[8:16])),
		Count:     binary.LittleEndian.Uint32(buf[16:20]),
		Flags:     binary.LittleEndian.Uint32(buf[20:24]),
		SeqCount:  binary.LittleEndian.Uint32(buf[24:28]),
		RandCount: binary.LittleEndian.Uint32(buf[28:32]),
	}, true
}

// seqRatio returns the fraction of sequential reads [0, 1]; 0 when no read
// has been counted.
func (ev readMissEvent) seqRatio() float64 {
	total := uint64(ev.SeqCount) + uint64(ev.RandCount)
	if total == 0 {
		return 0
	}
	return float64(ev.SeqCount) / float64(total)
}

// missFlagForWrite mirrors S3FS_MISS_FLAG_FOR_WRITE (AC-50): kmod is about
// to modify the file and needs the chunk at Offset loaded into its fd.
const missFlagForWrite = 0x1

// missFlagPinned mirrors S3FS_MISS_FLAG_PINNED (AC-51): the fd depends on the
// pinned ETag, so a changed object is a conflict, not a stale pin.
const missFlagPinned = 0x2

// missFlagOpenCheck mirrors S3FS_MISS_FLAG_OPEN_CHECK: kmod's open(2) (module
// param open_check=1) asks whether the object still exists.
const missFlagOpenCheck = 0x4

// openCheckKey mirrors S3FS_OPEN_CHECK_KEY, the pending chunk_offset of an open
// check (all ones as a u64; a read's chunk offsets are 4 MiB multiples).
const openCheckKey int64 = -1 // uint64(openCheckKey) is all ones

// handleOpenCheck HEADs the object behind ev.Ino and signals the open's
// waiter: 0 if it exists, -ENOENT if S3 says 404 (or the ino is unknown),
// -EIO for any other failure. It is not a read miss, so it is neither counted
// as an upcall nor observed in the miss latency histogram.
func handleOpenCheck(ctx context.Context, devPath string, s3c objstore.Backend, ev readMissEvent) {
	metrics.Global.OpenChecks.Add(1)
	signal := func(code int32) { _ = signalAdmitFn(devPath, ev.Ino, openCheckKey, code) }
	entry := fpebpf.LookupByIno(ev.Ino)
	if entry == nil {
		metrics.Global.OpenCheckMissing.Add(1)
		signal(-2 /* -ENOENT */)
		return
	}
	if s3c == nil {
		signal(-61 /* -ENODATA */)
		return
	}
	if _, err := s3c.Stat(ctx, entry.S3Key); err != nil {
		if errors.Is(err, objstore.ErrNotFound) {
			metrics.Global.OpenCheckMissing.Add(1)
			fmt.Fprintf(os.Stderr, "[upcall] ino=%d %s: open check: not in S3 — ENOENT\n", ev.Ino, entry.Name)
			signal(-2)
			return
		}
		fmt.Fprintf(os.Stderr, "[upcall] ino=%d %s: open check: %v\n", ev.Ino, entry.Name, err)
		metrics.Global.ReadMissErrors.Add(1)
		signal(-5 /* -EIO */)
		return
	}
	signal(0)
}

// handleLoadForWrite serves a miss that kmod flagged S3FS_MISS_FLAG_FOR_WRITE
// (AC-50): load the 4 MiB chunk containing ev.Offset into the ino's fd with
// POPULATE_PAGES — whatever the admit mode, since the bytes are for the
// write buffer, not the arena — and signal the waiter once kmod confirms
// the chunk is cached.
func handleLoadForWrite(ctx context.Context, devPath string, s3c objstore.Backend, ev readMissEvent) {
	missChunkOffset := (ev.Offset / partialAdmitChunk) * partialAdmitChunk
	fail := func(code int32, format string, a ...any) {
		fmt.Fprintf(os.Stderr, "[upcall] ino=%d load-for-write: "+format+"\n",
			append([]any{ev.Ino}, a...)...)
		metrics.Global.ReadMissErrors.Add(1)
		_ = signalAdmitFn(devPath, ev.Ino, missChunkOffset, code)
	}
	entry := fpebpf.LookupByIno(ev.Ino)
	if entry == nil {
		fail(-2 /* -ENOENT */, "unknown ino")
		return
	}
	if s3c == nil {
		fail(-61 /* -ENODATA */, "no S3 backend")
		return
	}
	size := int64(partialAdmitChunk)
	if rest := int64(entry.Fallback) - missChunkOffset; rest < size {
		size = rest
	}
	// kmod sends the length the chunk must hold up to its i_size. The
	// entry's size is older when the file was appended and flushed since the
	// last sync; a chunk cut at the old size never becomes populated and the
	// write fails with EIO.
	if want := int64(ev.Count); want > size && want <= partialAdmitChunk {
		size = want
	}
	if size <= 0 {
		fail(-5 /* -EIO */, "chunk at %d is past the object (%d B)", missChunkOffset, entry.Fallback)
		return
	}
	var body []byte
	var err error
	if mc, ok := s3c.(objstore.MultipartCopier); ok {
		// AC-51: the chunks loaded in one modify cycle must come from the
		// same object the flush copies the rest from.
		body, err = getPinned(ctx, s3c, mc, ev.Ino, entry.S3Key, missChunkOffset, size,
			ev.Flags&missFlagPinned != 0)
	} else {
		body, err = s3c.GetRange(ctx, entry.S3Key, missChunkOffset, size)
	}
	if err != nil {
		fail(-5, "s3://%s get: %v", entry.S3Key, err)
		return
	}
	for attempt := 0; ; attempt++ {
		if err := populatePagesFn(devPath, ev.Ino, missChunkOffset, body); err != nil {
			fail(-5, "populate pages: %v", err)
			return
		}
		fmt.Printf("[upcall] ino=%d %s load-for-write [%d,%d)\n",
			ev.Ino, entry.Name, missChunkOffset, missChunkOffset+int64(len(body)))
		err := signalAdmitFlagsFn(devPath, ev.Ino, missChunkOffset, 0, s3fsSignalVerifyPopulated)
		if !errors.Is(err, unix.EAGAIN) {
			return // woken, or nobody is waiting (ENOENT)
		}
		if attempt >= signalVerifyRetries {
			_ = signalAdmitFn(devPath, ev.Ino, missChunkOffset, 0)
			return
		}
		metrics.Global.ReadMissRepopulates.Add(1)
	}
}

// signalVerifyRetries bounds how often handleReadMiss re-populates when kmod
// reports the chunk was evicted before the success signal (AC-45).
const signalVerifyRetries = 3

// readMissWorkers is the maximum number of concurrent handleReadMiss
// goroutines. Bounded to prevent unbounded goroutine growth under
// bursty kmod .read events (V-06). 16 workers overlap S3 round-trips
// without overwhelming the S3 client or the arena allocator.
const readMissWorkers = 16

// runReadMissLoop reads /dev/s3fs_ebpf and services each event by
// fetching the object from S3, admitting it, and signaling the
// blocked kmod .read. Terminates when the device read returns EOF or
// ctx is done.
//
// V-06: goroutine spawning is bounded by a semaphore channel with
// readMissWorkers slots. A full semaphore back-pressures new events;
// kmod's per-ino wait queue serializes them so no event is lost.
func runReadMissLoop(ctx context.Context, devPath string,
	progs *fpebpf.Programs, s3c objstore.Backend) {
	f, err := os.OpenFile(devPath, os.O_RDWR, 0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[upcall] open %s: %v\n", devPath, err)
		return
	}
	defer f.Close()

	// Cancel-triggered close so the blocked kmod-side .read wakes
	// with EOF. The 5s deadline is an H-05 observability net: on any
	// modern kernel .release drops the wait queue, but a stuck kmod
	// (buggy hook, stale mm ref) could leave Read hanging forever.
	// Emit an audible warning so bench harnesses and operators can
	// tell whether shutdown actually completed.
	loopExit := make(chan struct{})
	go func() {
		<-ctx.Done()
		f.Close()
		select {
		case <-loopExit:
		case <-time.After(readMissShutdownWarn):
			fmt.Fprintf(os.Stderr,
				"[upcall] SHUTDOWN WARN: read loop still blocked %s after close — check kmod .release path\n",
				readMissShutdownWarn)
		}
	}()
	defer close(loopExit)

	sem := make(chan struct{}, readMissWorkers)

	var buf [readMissEventSize]byte
	for {
		n, err := f.Read(buf[:])
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, os.ErrClosed) {
				fmt.Fprintf(os.Stderr, "[upcall] read: %v\n", err)
			}
			return
		}
		ev, ok := decodeReadMissEvent(buf[:n])
		if !ok {
			fmt.Fprintf(os.Stderr, "[upcall] short read: %d bytes\n", n)
			continue
		}
		fmt.Printf("[upcall] miss ino=%d offset=%d count=%d\n",
			ev.Ino, ev.Offset, ev.Count)
		sem <- struct{}{}
		go func(ev readMissEvent) {
			defer func() { <-sem }()
			handleReadMiss(ctx, devPath, progs, s3c, ev)
		}(ev)
	}
}

// PartialAdmitThreshold — files above this size are admitted as a
// single chunk covering just the miss offset (D-01). Below, we
// fetch the whole object (fewer requests, keeps small-file behavior
// identical to pre-D-01). 4 MiB matches S3-tier defaults for small
// PoC deployments; production would set larger.
const (
	partialAdmitThreshold = 4 * 1024 * 1024
	partialAdmitChunk     = 4 * 1024 * 1024

	// seqAdmitRatioThreshold — minimum sequential-read ratio (from the miss event)
	// above which a large-file miss is promoted to a full-file GET.
	// R-01: kmod's .read_iter counts the reads before .read_folio blocks, so
	// the ratio in the miss event already reflects the access character.
	seqAdmitRatioThreshold = 0.70
)

// fullAdmitExceedsCapacity reports whether promoting a miss to a full-file
// GET would populate more than kmod's capacity_bytes holds (AC-45).  In
// pcache mode the whole body lands in fd chunks before any eviction can
// run, so a file larger than the capacity would only be populated to be
// evicted again; such files keep the chunk-by-chunk admit.
func fullAdmitExceedsCapacity(fullSize int64) bool {
	c, ok := capacityBytesFn()
	return ok && c > 0 && fullSize > c
}

// patternAwareAdmitEnabled gates the R-01 sequential-pattern logic in
// handleReadMiss. Set once by main before any goroutine reads it.
var patternAwareAdmitEnabled bool

// readMissShutdownWarn bounds how long the shutdown watchdog waits
// for the blocking Read to wake after Close before logging. Tuned
// pessimistic — real kmod releases finish in μs.
var readMissShutdownWarn = 5 * time.Second

func handleReadMiss(ctx context.Context, devPath string,
	progs *fpebpf.Programs, s3c objstore.Backend, ev readMissEvent) {
	if ev.Flags&missFlagOpenCheck != 0 {
		handleOpenCheck(ctx, devPath, s3c, ev)
		return
	}
	metrics.Global.ReadMissUpcalls.Add(1)
	// Time the full miss-service path (S3 Get + arena write +
	// admit_replace) and observe on every return path — including
	// the ENOENT / EIO / ENODATA branches so the histogram reflects
	// real tail behavior, not just the happy path. Bench uses this
	// to compare policies by upcall latency, not just hit rate.
	miss_started := time.Now()
	defer func() {
		metrics.ReadMissLatency.Observe(time.Since(miss_started))
	}()
	if ev.Flags&missFlagForWrite != 0 {
		handleLoadForWrite(ctx, devPath, s3c, ev)
		return
	}
	// U13: compute the aligned chunk boundary that kmod used when creating
	// the pending entry for this miss (matches pending_get_or_create key).
	// ALL signalAdmit calls below must use missChunkOffset so they find
	// the correct per-chunk pending entry.
	missChunkOffset := (ev.Offset / partialAdmitChunk) * partialAdmitChunk

	entry := fpebpf.LookupByIno(ev.Ino)
	if entry == nil {
		fmt.Fprintf(os.Stderr, "[upcall] ino=%d unknown — signaling ENOENT\n", ev.Ino)
		metrics.Global.ReadMissErrors.Add(1)
		_ = signalAdmitFn(devPath, ev.Ino, missChunkOffset, -2 /* -ENOENT */)
		return
	}
	if s3c == nil {
		fmt.Fprintf(os.Stderr, "[upcall] ino=%d %s: no S3 backend — signaling ENODATA\n",
			ev.Ino, entry.Name)
		metrics.Global.ReadMissErrors.Add(1)
		_ = signalAdmitFn(devPath, ev.Ino, missChunkOffset, -61 /* -ENODATA */)
		return
	}
	// AC-68: after this miss is served, read ahead the next siblings the
	// prefetch_next policy asks for. Only the first chunk's miss counts,
	// so a large file triggers it once per load, not once per chunk.
	if missChunkOffset == 0 {
		defer maybePrefetchSiblings(ev.Ino)
	}

	// D-01 partial admit path. Small files (< threshold): fetch
	// whole object, admit at offset=0. Large files: fetch just the
	// chunk covering ev.Offset via S3 range GET, admit with the
	// range's file offset so subsequent .read_folio reads at
	// nearby offsets hit the same chunk.
	//
	// R-01 pattern-aware override: when --pattern-aware-admit is set
	// and s3fs_read_pattern_map shows a strong sequential signal, promote
	// large-file chunk admits to a full-file GET. The fentry hook fires
	// before kmod's .read_folio blocks, so by the time this goroutine runs
	// seq_count is often already ≥ 1, making SeqRatio() = 1.0 on the first
	// miss and saving the remaining chunk round-trips.
	var body []byte
	var chunkOffset int64 = 0
	fullSize := int64(entry.Fallback)
	var err error
	isLarge := fullSize > 0 && fullSize > partialAdmitThreshold
	useFullAdmit := !isLarge // small files always use full admit
	if !useFullAdmit && patternAwareAdmitEnabled &&
		!fullAdmitExceedsCapacity(fullSize) {
		if ev.SeqCount+ev.RandCount > 0 && ev.seqRatio() >= seqAdmitRatioThreshold {
			useFullAdmit = true
			fmt.Printf("[upcall] ino=%d %s seq_ratio=%.2f PROMOTE_SIGNAL=pattern-aware full admit (%d bytes)\n",
				ev.Ino, entry.Name, ev.seqRatio(), fullSize)
		}
	}
	if useFullAdmit && isLarge {
		// AC-52 step 3: parallel range GETs, populated and signalled per chunk.
		serveChunkLoad(ctx, devPath, s3c, ev, entry.S3Key, entry.Name, fullSize, missChunkOffset)
		return
	}
	if useFullAdmit {
		body, err = s3c.Get(ctx, entry.S3Key)
	} else {
		// Align to chunk boundary containing the miss.
		chunkOffset = missChunkOffset
		chunkSize := int64(partialAdmitChunk)
		if chunkOffset+chunkSize > fullSize {
			chunkSize = fullSize - chunkOffset
		}
		body, err = s3c.GetRange(ctx, entry.S3Key, chunkOffset, chunkSize)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "[upcall] ino=%d s3://%s get: %v\n",
			ev.Ino, entry.S3Key, err)
		metrics.Global.ReadMissErrors.Add(1)
		_ = signalAdmitFn(devPath, ev.Ino, missChunkOffset, -5 /* -EIO */)
		return
	}
	// Send the data to kmod's chunk table (and page cache) via the
	// POPULATE_PAGES ioctl.
	// The kmod limits each call to S3FS_POPULATE_MAX_SIZE (64 MiB), so split
	// body into partialAdmitChunk-sized pieces; full-admit bodies (seq promote)
	// can be arbitrarily large.
	for attempt := 0; ; attempt++ {
		off := chunkOffset
		rem := body
		for len(rem) > 0 {
			n := len(rem)
			if n > partialAdmitChunk {
				n = partialAdmitChunk
			}
			if err := populatePagesFn(devPath, ev.Ino, off, rem[:n]); err != nil {
				fmt.Fprintf(os.Stderr, "[upcall] ino=%d populate pages: %v\n", ev.Ino, err)
				metrics.Global.ReadMissErrors.Add(1)
				_ = signalAdmitFn(devPath, ev.Ino, missChunkOffset, -5 /* -EIO */)
				return
			}
			off += int64(n)
			rem = rem[n:]
		}
		fmt.Printf("[upcall] ino=%d %s pcache [%d,%d)\n",
			ev.Ino, entry.Name, chunkOffset, chunkOffset+int64(len(body)))
		// AC-45 C2: the kmod ran its capacity pass at the end of each populate
		// ioctl, while this miss was still pending (v2 Phase 3e).
		// AC-45: kmod wakes the waiter only if the chunk is still
		// cached.  Concurrent handlers for one ino and the capacity
		// pass can evict what we just populated; populate again
		// (bounded) instead of waking the reader with nothing to read.
		err := signalAdmitFlagsFn(devPath, ev.Ino, missChunkOffset, 0,
			s3fsSignalVerifyPopulated)
		if !errors.Is(err, unix.EAGAIN) {
			return // woken, or nobody is waiting (ENOENT)
		}
		if attempt >= signalVerifyRetries {
			// Give up verifying: wake the waiter so its own retry /
			// fallback path takes over rather than hanging.
			_ = signalAdmitFn(devPath, ev.Ino, missChunkOffset, 0)
			return
		}
		metrics.Global.ReadMissRepopulates.Add(1)
	}
}

// -- AA-04: sequential read-ahead prefetch --------------------------------
