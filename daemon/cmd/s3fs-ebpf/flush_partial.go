package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"
	"unsafe"

	"github.com/bbsakura/s3fs-ebpf/daemon/internal/objstore"
	"golang.org/x/sys/unix"
)

// AC-51: flush of a dirty file whose unchanged ("remote") chunks are still
// the object in S3.  kmod reports the chunk states (S3FS_IOC_DIRTY_LAYOUT);
// the remote runs are copied server-side (UploadPartCopy, pinned by ETag) and
// only the other chunks travel.  See docs/design/ac51-partial-modify.md §7-8.

type dirtyLayoutArg struct {
	Ino         uint64
	StatesUaddr uint64
	Cap         uint32
	NrChunks    uint32
	Size        uint64
	ChunkSize   uint64
}

type readDirtyRangeArg struct {
	Ino      uint64
	Uaddr    uint64
	Offset   uint64
	Size     uint64
	OutBytes uint64
}

const (
	s3fsIocDirtyLayout    = (3 << 30) | (uint32(unsafe.Sizeof(dirtyLayoutArg{})) << 16) | (0xE3 << 8) | 0x0F
	s3fsIocReadDirtyRange = (3 << 30) | (uint32(unsafe.Sizeof(readDirtyRangeArg{})) << 16) | (0xE3 << 8) | 0x10
)

// dirtyLayout asks kmod for the chunk states of ino.
func dirtyLayout(devFd int, ino uint64) (kinds []objstore.ChunkKind, size, chunk int64, err error) {
	states := make([]byte, 256)
	for {
		arg := dirtyLayoutArg{Ino: ino, StatesUaddr: uint64(uintptr(unsafe.Pointer(&states[0]))), Cap: uint32(len(states))}
		_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(devFd), uintptr(s3fsIocDirtyLayout), uintptr(unsafe.Pointer(&arg)))
		if errno == unix.ENOSPC && int(arg.NrChunks) > len(states) {
			states = make([]byte, arg.NrChunks)
			continue
		}
		if errno != 0 {
			return nil, 0, 0, errno
		}
		kinds = make([]objstore.ChunkKind, arg.NrChunks)
		for i := range kinds {
			kinds[i] = objstore.ChunkKind(states[i])
		}
		return kinds, int64(arg.Size), int64(arg.ChunkSize), nil
	}
}

// readDirtyRange copies the file bytes at [off, off+len(buf)) from kmod.
func readDirtyRange(devFd int, ino uint64, off int64, buf []byte) error {
	if len(buf) == 0 {
		return nil
	}
	arg := readDirtyRangeArg{Ino: ino, Uaddr: uint64(uintptr(unsafe.Pointer(&buf[0]))), Offset: uint64(off), Size: uint64(len(buf))}
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(devFd), uintptr(s3fsIocReadDirtyRange), uintptr(unsafe.Pointer(&arg))); errno != 0 {
		return errno
	}
	if arg.OutBytes != uint64(len(buf)) {
		return fmt.Errorf("read dirty range [%d,+%d): got %d bytes", off, len(buf), arg.OutBytes)
	}
	return nil
}

// objETags remembers, per ino, the ETag of the object the unchanged chunks
// of the current modify cycle come from.  It is set by the first load (or at
// flush time when no chunk was ever loaded) and dropped when the flush
// succeeds, so the next cycle starts from the then-current object.
var objETags sync.Map // ino -> string

// pinETag returns the ETag of ino's object for this cycle, recording the
// current one if there is none yet.
func pinETag(ctx context.Context, s3c objstore.Backend, ino uint64, key string) (string, error) {
	if v, ok := objETags.Load(ino); ok {
		return v.(string), nil
	}
	m, err := s3c.Stat(ctx, key)
	if err != nil {
		return "", err
	}
	v, _ := objETags.LoadOrStore(ino, m.ETag)
	return v.(string), nil
}

// getPinned reads [off, off+n) of ino's object for a load-for-write, pinned
// to the cycle's ETag.  An ETag mismatch has two causes:
//   - a flush finished meanwhile and pinned the new object (its remote
//     chunks have the same bytes): read the pin again and retry;
//   - the pin is stale.  If the fd depends on it (bound: dirty, flushing, or
//     remote chunks) that is a real conflict and the error is returned.
//     Otherwise nothing depends on it (an abandoned cycle): drop it, Stat
//     again and retry.
func getPinned(ctx context.Context, s3c objstore.Backend, mc objstore.MultipartCopier,
	ino uint64, key string, off, n int64, bound bool) ([]byte, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		etag, err := pinETag(ctx, s3c, ino, key)
		if err != nil {
			return nil, err
		}
		body, err := mc.GetRangeMatch(ctx, key, off, n, etag)
		if !errors.Is(err, objstore.ErrSourceChanged) {
			return body, err
		}
		lastErr = err
		if cur, ok := objETags.Load(ino); ok && cur.(string) != etag {
			continue // a flush re-pinned
		}
		if bound {
			return nil, err
		}
		objETags.CompareAndDelete(ino, etag)
	}
	return nil, lastErr
}

// partialIO is what the part executor needs; tests replace it.
type partialIO struct {
	readDirty func(off int64, buf []byte) error
	getMatch  func(ctx context.Context, off, n int64) ([]byte, error) // pinned
	getNow    func(ctx context.Context, off, n int64) ([]byte, error) // current object
}

// getNowClamped reads [off, off+n) of the object as it is now, cut at its end:
// a range wholly past the end is empty (S3 would answer 416) and one that
// crosses it is short.
func getNowClamped(ctx context.Context, s3c objstore.Backend, key string, off, n int64) ([]byte, error) {
	m, err := s3c.Stat(ctx, key)
	if err != nil {
		return nil, err
	}
	if off >= m.Size {
		return nil, nil
	}
	if off+n > m.Size {
		n = m.Size - off
	}
	return s3c.GetRange(ctx, key, off, n)
}

// fillRange returns the bytes [off, off+n) of the new object: kmod's for the
// data / hole chunks, the pinned object's for remote ones.
func fillRange(ctx context.Context, io partialIO, kinds []objstore.ChunkKind, chunk, off, n int64, pinned bool) ([]byte, error) {
	buf := make([]byte, n)
	end := off + n
	for pos := off; pos < end; {
		ci := int(pos / chunk)
		next := min(end, int64(ci+1)*chunk)
		if kinds[ci] == objstore.ChunkRemote {
			get := io.getNow
			if pinned {
				get = io.getMatch
			}
			b, err := get(ctx, pos, next-pos)
			if err != nil {
				return nil, err
			}
			if int64(len(b)) != next-pos {
				if pinned {
					return nil, fmt.Errorf("remote range [%d,%d): got %d bytes", pos, next, len(b))
				}
				// The current object is shorter than this remote chunk
				// (truncated by someone else since we pinned it).  The bytes
				// are gone: what is missing reads as zeros, as if the file had
				// been truncated and then extended.  The fallback is last
				// writer wins anyway (the rest of the range already comes from
				// the object as it is now).
				fmt.Fprintf(os.Stderr, "[flush] remote range [%d,%d): the object now has only %d of its bytes; the rest is written as zeros\n",
					pos, next, len(b))
			}
			copy(buf[pos-off:], b)
		} else {
			// extend over the following non-remote chunks in one call
			for next < end && kinds[int(next/chunk)] != objstore.ChunkRemote {
				next = min(end, next+chunk)
			}
			if err := io.readDirty(pos, buf[pos-off:next-off]); err != nil {
				return nil, err
			}
		}
		pos = next
	}
	return buf, nil
}

// flushPartial writes the new object with PutWithCopy; on ErrSourceChanged
// it rebuilds the whole content from the current object plus kmod's chunks
// and does a plain Put (last writer wins, as before AC-51).  When no run of
// unchanged chunks is long enough to be a copy part (every part but the last
// must be >= 5 MiB), PlanParts says ErrNoCopy and the whole content is put
// the plain way, the remote chunks' bytes coming from the pinned object.
// Returns the number of bytes sent and the ETag of the new object ("" when
// the plain put was used: its ETag is not known here).
func flushPartial(ctx context.Context, mc objstore.MultipartCopier, put func(ctx context.Context, data []byte) error,
	io partialIO, key, etag string, kinds []objstore.ChunkKind, chunk, size int64) (int64, string, error) {
	specs, err := objstore.PlanParts(kinds, chunk, size)
	if errors.Is(err, objstore.ErrNoCopy) {
		return fullPut(ctx, io, put, kinds, chunk, size, true)
	}
	if err != nil {
		return 0, "", err
	}
	// The bytes of an upload part are produced by the executor right before it
	// uploads the part (Part.Fill), so memory holds a few parts, not all.
	parts := make([]objstore.Part, len(specs))
	var sent int64
	t0 := time.Now()
	for i, sp := range specs {
		sp := sp
		parts[i] = objstore.Part{PartSpec: sp}
		if sp.Kind == objstore.PartUpload {
			parts[i].Fill = func() ([]byte, error) {
				return fillRange(ctx, io, kinds, chunk, sp.Off, sp.Len, true)
			}
			sent += sp.Len
		}
	}
	newETag, err := mc.PutWithCopy(ctx, key, etag, parts)
	if err == nil {
		fmt.Printf("[flush] s3://%s %d parts (%d B uploaded): %.1f ms\n", key, len(parts), sent,
			float64(time.Since(t0).Microseconds())/1000)
		return sent, newETag, nil
	}
	if !errors.Is(err, objstore.ErrSourceChanged) {
		return 0, "", err
	}
	fmt.Fprintf(os.Stderr, "[flush] s3://%s changed under the copy source; falling back to a full put\n", key)
	return fullPut(ctx, io, put, kinds, chunk, size, false)
}

// fullPut puts the whole new content with a plain Put.  With pinned, the
// remote chunks' bytes come from the pinned object (they are the bytes the
// fd was built on); if that object changed, or when not pinned, from the
// current object (last writer wins).
func fullPut(ctx context.Context, io partialIO, put func(ctx context.Context, data []byte) error,
	kinds []objstore.ChunkKind, chunk, size int64, pinned bool) (int64, string, error) {
	full, err := fillRange(ctx, io, kinds, chunk, 0, size, pinned)
	if pinned && errors.Is(err, objstore.ErrSourceChanged) {
		full, err = fillRange(ctx, io, kinds, chunk, 0, size, false)
	}
	if err != nil {
		return 0, "", err
	}
	return size, "", put(ctx, full)
}

// tryFlushPartial handles the flush of ev when the file has remote chunks.
// handled=false means "use the full put" (nothing remote, or no support).
func tryFlushPartial(ctx context.Context, devFd int, s3c objstore.Backend, key string, ino uint64) (handled bool, sent int64, size int64, err error) {
	mc, canCopy := s3c.(objstore.MultipartCopier)
	kinds, size, chunk, lerr := dirtyLayout(devFd, ino)
	if lerr != nil {
		return false, 0, 0, nil // old kmod: no layout, full put
	}
	remote := false
	for _, k := range kinds {
		remote = remote || k == objstore.ChunkRemote
	}
	if !remote || size <= 0 {
		return false, 0, 0, nil
	}
	if !canCopy {
		// kmod left partial_modify on for a backend that cannot copy: a plain
		// put of the buffer would write zeros over the remote chunks.  Refuse;
		// the caller marks the file dirty again.
		return true, 0, size, errors.New("file has remote chunks but the backend cannot copy server-side; refusing a put that would zero them")
	}
	etag, err := pinETag(ctx, s3c, ino, key)
	if err != nil {
		return true, 0, size, err
	}
	io := partialIO{
		readDirty: func(off int64, buf []byte) error { return readDirtyRange(devFd, ino, off, buf) },
		getMatch: func(ctx context.Context, off, n int64) ([]byte, error) {
			return mc.GetRangeMatch(ctx, key, off, n, etag)
		},
		getNow: func(ctx context.Context, off, n int64) ([]byte, error) { return getNowClamped(ctx, s3c, key, off, n) },
	}
	sent, newETag, err := flushPartial(ctx, mc, func(ctx context.Context, d []byte) error { return s3c.Put(ctx, key, d) },
		io, key, etag, kinds, chunk, size)
	if err == nil {
		// The new object has the same bytes the pinned one had in every
		// remote chunk, so a load that runs during or after this flush (the
		// fd may be dirty again) can pin the new ETag.  Unknown ETag (plain
		// put): drop the pin; the next load pins the current object.
		if newETag != "" {
			objETags.Store(ino, newETag)
		} else {
			objETags.Delete(ino)
		}
	}
	return true, sent, size, err
}
