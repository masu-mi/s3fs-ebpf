package ebpf

import (
	"encoding/binary"
	"fmt"
)

// InodeStub mirrors `struct s3fs_inode_stub` in bpf/arena_types.h.
// Layout is packed to match the BPF-side struct byte-for-byte.
type InodeStub struct {
	Ino     uint64
	Size    uint64
	MtimeNs uint64
	Mode    uint32
	Pad     uint32
}

// stubSize matches sizeof(struct s3fs_inode_stub).
const stubSize = 32

// WarmCache writes stub into the arena at the next free slot and
// installs an ino → offset mapping in s3fs_inode_index so subsequent
// fentry/vfs_getattr_nosec hits on that ino short-circuit as fast_hits
// instead of firing another miss.
//
// Returns the arena offset the stub was written at. Returns
// ErrCacheFull if the arena has no more slots. Callers that receive
// ErrCacheFull can drop the entry — the fentry hook keeps emitting
// misses and counters keep incrementing, which is the honest signal
// that the cache is undersized.
func (p *Programs) WarmCache(stub InodeStub) (uint64, error) {
	p.cacheMu.Lock()
	defer p.cacheMu.Unlock()

	if existing, ok := p.cacheKnown[stub.Ino]; ok {
		return existing, nil
	}

	arenaSize := p.arena.Size()
	nextOff := p.cacheNextOff
	if nextOff+stubSize > arenaSize {
		return 0, ErrCacheFull
	}

	buf := make([]byte, stubSize)
	binary.LittleEndian.PutUint64(buf[0:8], stub.Ino)
	binary.LittleEndian.PutUint64(buf[8:16], stub.Size)
	binary.LittleEndian.PutUint64(buf[16:24], stub.MtimeNs)
	binary.LittleEndian.PutUint32(buf[24:28], stub.Mode)
	binary.LittleEndian.PutUint32(buf[28:32], stub.Pad)
	if _, err := p.arena.WriteAt(buf, int64(nextOff)); err != nil {
		return 0, fmt.Errorf("arena write ino=%d: %w", stub.Ino, err)
	}

	// S3fsInodeIndex is a __u32 BPF map; cast is safe since the arena is
	// bounded at 128 MiB (well within uint32 range).
	off32 := uint32(nextOff)
	if err := p.Objs.S3fsInodeIndex.Update(&stub.Ino, &off32, 0); err != nil {
		return 0, fmt.Errorf("inode_index update ino=%d: %w", stub.Ino, err)
	}

	p.cacheKnown[stub.Ino] = nextOff
	p.cacheNextOff = nextOff + stubSize
	return nextOff, nil
}

// IsWarmed reports whether ino has already been admitted to the cache.
// Callers use this to short-circuit an S3 round-trip when a burst of
// queued miss events all reference the same ino (the ringbuf drains
// after the fentry hook fires several times before the daemon catches
// up).
func (p *Programs) IsWarmed(ino uint64) bool {
	p.cacheMu.Lock()
	defer p.cacheMu.Unlock()
	_, ok := p.cacheKnown[ino]
	return ok
}

// KnownInos returns a snapshot of all inos currently admitted to the
// arena cache. Ordering is undefined. Used by CLI subcommands that
// want to report cache state.
func (p *Programs) KnownInos() []uint64 {
	p.cacheMu.Lock()
	defer p.cacheMu.Unlock()
	out := make([]uint64, 0, len(p.cacheKnown))
	for ino := range p.cacheKnown {
		out = append(out, ino)
	}
	return out
}
