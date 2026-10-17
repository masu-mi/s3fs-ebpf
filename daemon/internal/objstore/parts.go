package objstore

import (
	"context"
	"errors"
	"fmt"
)

// AC-51 (docs/design/ac51-partial-modify.md §7): a flush of a file whose
// unmodified chunks are still "remote" (identical to the object in S3) is
// a multipart upload where those chunks are server-side copies.  PlanParts
// turns the per-chunk layout kmod reports into the part list.

// ChunkKind is the state of one chunk of a dirty file.
type ChunkKind uint8

const (
	ChunkData   ChunkKind = iota // kmod holds the bytes
	ChunkHole                    // beyond the original object, reads as zeros
	ChunkRemote                  // unchanged: same bytes as the object in S3
)

// PartKind says how a part reaches S3.
type PartKind uint8

const (
	PartUpload PartKind = iota // UploadPart with bytes the daemon supplies
	PartCopy                   // UploadPartCopy of [Off, Off+Len) of the source
)

// PartSpec is a byte range [Off, Off+Len) of the NEW object.  For PartCopy
// the same range of the source object is copied (a remote chunk sits at the
// same offset in the old and the new object).
type PartSpec struct {
	Kind PartKind
	Off  int64
	Len  int64
}

const (
	// MinPartSize is S3's minimum size of every part but the last.
	MinPartSize = 5 << 20
	// MaxCopyPart bounds one UploadPartCopy (S3 allows 5 GiB).
	MaxCopyPart = 4 << 30
	// MaxUploadPart bounds the bytes the daemon holds for one UploadPart.
	MaxUploadPart = 64 << 20
	// MaxParts is S3's limit on the part count.
	MaxParts = 10000
)

var (
	// ErrNoCopy: nothing is remote, a plain Put does the job.
	ErrNoCopy = errors.New("objstore: no remote chunk, use a plain put")
	// ErrTooManyParts: the layout would need more than MaxParts parts.
	ErrTooManyParts = errors.New("objstore: too many parts")
)

// PlanParts builds the part list for a new object of `size` bytes whose
// chunk i (chunkSize bytes, the last one possibly shorter) has kinds[i].
// The parts are contiguous, cover [0, size) exactly, and every part but the
// last is at least MinPartSize: a remote run shorter than that, and an
// upload shorter than that, are fixed by moving bytes between neighbours
// (the executor fills an upload's bytes from kmod or from a range GET of
// the source, so it does not matter which side they come from).
func PlanParts(kinds []ChunkKind, chunkSize, size int64) ([]PartSpec, error) {
	if size <= 0 || chunkSize <= 0 {
		return nil, ErrNoCopy
	}
	n := int((size + chunkSize - 1) / chunkSize)
	if len(kinds) < n {
		return nil, fmt.Errorf("objstore: layout has %d chunks, size %d needs %d", len(kinds), size, n)
	}
	// Runs of remote / not-remote chunks.
	var segs []PartSpec
	for i := 0; i < n; i++ {
		kind := PartUpload
		if kinds[i] == ChunkRemote {
			kind = PartCopy
		}
		off := int64(i) * chunkSize
		end := min(off+chunkSize, size)
		if k := len(segs) - 1; k >= 0 && segs[k].Kind == kind {
			segs[k].Len = end - segs[k].Off
		} else {
			segs = append(segs, PartSpec{Kind: kind, Off: off, Len: end - off})
		}
	}
	hasCopy := false
	for _, s := range segs {
		hasCopy = hasCopy || s.Kind == PartCopy
	}
	if !hasCopy {
		return nil, ErrNoCopy
	}
	// A copy shorter than a part cannot stand alone (unless it is last).
	for i := range segs[:len(segs)-1] {
		if segs[i].Kind == PartCopy && segs[i].Len < MinPartSize {
			segs[i].Kind = PartUpload
		}
	}
	segs = mergeUploads(segs)
	// An upload that is too short borrows from the copy after it.
	for i := 0; i < len(segs)-1; i++ {
		if segs[i].Kind != PartUpload || segs[i].Len >= MinPartSize {
			continue
		}
		next := &segs[i+1] // a copy: uploads are merged
		deficit := MinPartSize - segs[i].Len
		if left := next.Len - deficit; left >= MinPartSize || (i+1 == len(segs)-1 && left > 0) {
			segs[i].Len += deficit
			next.Off += deficit
			next.Len = left
			continue
		}
		segs[i].Len += next.Len // take all of it
		segs = append(segs[:i+1], segs[i+2:]...)
		segs = mergeUploads(segs)
		i-- // re-check the grown upload
	}
	segs, err := splitLarge(segs)
	if err != nil {
		return nil, err
	}
	copies := false
	for _, s := range segs {
		copies = copies || s.Kind == PartCopy
	}
	if !copies {
		return nil, ErrNoCopy
	}
	return segs, nil
}

func mergeUploads(segs []PartSpec) []PartSpec {
	out := segs[:0:0]
	for _, s := range segs {
		if k := len(out) - 1; k >= 0 && out[k].Kind == PartUpload && s.Kind == PartUpload {
			out[k].Len += s.Len
			continue
		}
		out = append(out, s)
	}
	return out
}

// splitLarge cuts parts above their cap into near-equal pieces (each at
// least MinPartSize because the caps are far above 2*MinPartSize).
func splitLarge(segs []PartSpec) ([]PartSpec, error) {
	var out []PartSpec
	for _, s := range segs {
		limit := int64(MaxUploadPart)
		if s.Kind == PartCopy {
			limit = MaxCopyPart
		}
		pieces := (s.Len + limit - 1) / limit
		if int64(len(out))+pieces > MaxParts {
			return nil, ErrTooManyParts
		}
		off := s.Off
		for p := int64(0); p < pieces; p++ {
			l := (s.Off + s.Len - off) / (pieces - p) // near-equal split
			out = append(out, PartSpec{Kind: s.Kind, Off: off, Len: l})
			off += l
		}
	}
	return out, nil
}

// ErrSourceChanged: the object no longer has the ETag the copy was pinned
// to (S3 answered 412).  The caller falls back to a plain Put of the whole
// content (docs/design/ac51-partial-modify.md §8).
var ErrSourceChanged = errors.New("objstore: source object changed (ETag mismatch)")

// Part is one part of a PutWithCopy: a PartCopy needs no data, a PartUpload
// carries the bytes of [Off, Off+Len) in Data, or produces them with Fill when
// Data is nil.  Fill is called by the executor right before it uploads the
// part (and from one of its workers), so only the parts being uploaded are in
// memory, not all of them (AC-65).  An error from Fill that wraps
// ErrSourceChanged counts as the copy source having changed.
type Part struct {
	PartSpec
	Data []byte
	Fill func() ([]byte, error)
}

// Bytes returns the data of an upload part, calling Fill if the part has none.
func (p Part) Bytes() ([]byte, error) {
	if p.Data != nil || p.Fill == nil {
		return p.Data, nil
	}
	return p.Fill()
}

// MultipartCopier is implemented by backends with a multipart upload that
// can copy a range of an existing object server-side (S3: UploadPartCopy).
// GCS does not have one; the daemon then keeps today's full put.
type MultipartCopier interface {
	// GetRangeMatch is GetRange pinned to etag; ErrSourceChanged on mismatch.
	GetRangeMatch(ctx context.Context, key string, offset, size int64, etag string) ([]byte, error)
	// PutWithCopy replaces key with the concatenation of parts.  PartCopy
	// ranges are copied from the current object, which must still have
	// ifMatchETag.  Returns the ETag of the new object.
	PutWithCopy(ctx context.Context, key, ifMatchETag string, parts []Part) (string, error)
}
