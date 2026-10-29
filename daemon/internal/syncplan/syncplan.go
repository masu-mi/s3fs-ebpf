// Package syncplan turns a bucket listing into the entry table the daemon
// mirrors and pushes to the kmod (S3FS_IOC_SYNC_ENTRIES): synthetic directory
// entries for key prefixes, one file entry per object, and the UAPI payload.
package syncplan

import (
	"context"
	"fmt"
	"os"
	"path"
	"sort"

	fpebpf "github.com/bbsakura/s3fs-ebpf/daemon/internal/ebpf"
	"github.com/bbsakura/s3fs-ebpf/daemon/internal/inostate"
	"github.com/bbsakura/s3fs-ebpf/daemon/internal/kmodio"
	"github.com/bbsakura/s3fs-ebpf/daemon/internal/objstore"
)

// RootIno mirrors S3FS_ROOT_INO in kmod/vfs_ops.h.  The daemon writes it
// explicitly rather than relying on the "parent_ino==0 -> root" fallback (which
// stays in kmod as a safety net but is a code smell to depend on).
const RootIno = uint64(1)

// FileTypeMask is the S_IFMT bits (0170000) — file-type portion of a
// POSIX mode. Distinguishes S_IFDIR (0040000) from S_IFREG (0100000).
const FileTypeMask uint32 = 0o170000

// BuildEntries lists the bucket and turns each object into an
// Entry. G-01: S3 keys with "/" separators are split into a chain of
// synthetic directory entries plus the leaf file, so `ls
// /mnt/s3demo/sub1` walks only into the sub1/ prefix. F-02 also
// falls out: two S3 keys with the same basename but different
// prefixes get distinct (parent_ino, name) tuples and can coexist.
//
// Placeholder keys (trailing '/') are still filtered — they only
// serve as directory markers on the S3 side and their names collide
// with our synthesized dir entries.
func BuildEntries(ctx context.Context, s3c objstore.Backend, alloc *inostate.Allocator) ([]fpebpf.Entry, error) {
	objs, err := s3c.List(ctx, "")
	if err != nil {
		return nil, err
	}
	out := make([]fpebpf.Entry, 0, len(objs))
	// dirInoByPath: prefix ("sub1/", "sub1/nested/") → assigned ino
	// for the synthetic dir entry. Populated on first sighting so
	// two files under the same dir share the same parent ino.
	dirInoByPath := map[string]uint64{"": RootIno}
	for _, o := range objs {
		if len(o.Key) == 0 || o.Key[len(o.Key)-1] == '/' {
			continue
		}
		leafName := path.Base(o.Key)
		if len(leafName) == 0 || len(leafName) > kmodio.SyncNameMax {
			fmt.Fprintf(os.Stderr, "[sync] skip %q: invalid basename\n", o.Key)
			continue
		}
		// Walk the path prefix, minting a dir entry per component.
		parent := RootIno
		prefix := ""
		dir := path.Dir(o.Key)
		if dir != "." && dir != "/" && dir != "" {
			components := SplitPath(dir)
			for _, comp := range components {
				if len(comp) == 0 || len(comp) > kmodio.SyncNameMax {
					fmt.Fprintf(os.Stderr, "[sync] skip %q: bad component %q\n",
						o.Key, comp)
					parent = 0
					break
				}
				if prefix == "" {
					prefix = comp
				} else {
					prefix = prefix + "/" + comp
				}
				dino, seen := dirInoByPath[prefix]
				if !seen {
					dino = alloc.AssignIno("dir:" + prefix)
					dirInoByPath[prefix] = dino
					out = append(out, fpebpf.Entry{
						Ino:       dino,
						ParentIno: parent,
						Name:      comp,
						S3Key:     "",       // synthetic — no backing object
						Mode:      0o040755, // S_IFDIR | 0755
						Fallback:  0,
						Prewarm:   false, // dirs have no content to admit
					})
				}
				parent = dino
			}
			if parent == 0 {
				continue // skipped due to bad component
			}
		}
		out = append(out, fpebpf.Entry{
			Ino:       alloc.AssignIno(o.Key),
			ParentIno: parent,
			Name:      leafName,
			S3Key:     o.Key,
			Mode:      0o100644, // S_IFREG | 0644 (kmod fills S_IFREG bit)
			Fallback:  uint64(o.Size),
			Prewarm:   true, // Option 1: prewarm-all, policy engine overrides later
		})
	}
	// Sort by ino so ioctl output is stable across List() runs.
	sort.Slice(out, func(i, j int) bool { return out[i].Ino < out[j].Ino })
	return out, nil
}

// SplitPath breaks a POSIX-style path into non-empty components.
// path.Split only peels off the last element; we want every segment.
// A leading '/' or repeated separators are silently absorbed.
func SplitPath(p string) []string {
	out := make([]string, 0, 4)
	start := 0
	for i := 0; i <= len(p); i++ {
		if i == len(p) || p[i] == '/' {
			if i > start {
				out = append(out, p[start:i])
			}
			start = i + 1
		}
	}
	return out
}

// ToPayload marshals Go-side Entries into the UAPI struct
// array kmod expects. G-01: directory entries are passed through with
// their S_IFDIR mode bit intact; file entries have S_IFREG (0100000)
// stamped on to defend against a caller that only supplied the perm
// bits.
func ToPayload(es []fpebpf.Entry) []kmodio.SyncEntry {
	out := make([]kmodio.SyncEntry, 0, len(es))
	for _, e := range es {
		nlen := len(e.Name)
		if nlen == 0 || nlen > kmodio.SyncNameMax {
			fmt.Fprintf(os.Stderr, "[sync] skip ino=%d name=%q: bad length\n",
				e.Ino, e.Name)
			continue
		}
		mode := e.Mode
		if mode&FileTypeMask == 0 {
			mode |= 0o100000 // default to S_IFREG when caller left type bits blank
		}
		se := kmodio.SyncEntry{
			Ino:       e.Ino,
			ParentIno: e.ParentIno,
			Size:      e.Fallback, // Fallback holds the S3-stat size in Week 4(b)
			Mode:      mode,
			NameLen:   uint32(nlen),
		}
		copy(se.Name[:], e.Name)
		out = append(out, se)
	}
	return out
}
