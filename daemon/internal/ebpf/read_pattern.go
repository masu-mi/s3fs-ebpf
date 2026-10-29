package ebpf

// ReadPattern holds the per-ino access pattern snapshot read from the
// per-CPU slots of s3fs_ino_slots, merged across CPUs (see
// mergeReadPatterns).  SeqRatio returns the fraction of sequential reads
// (0.0–1.0).
type ReadPattern struct {
	LastEndPos uint64
	SeqCount   uint64
	RandCount  uint64
	TotalBytes uint64
}

// SeqRatio returns the fraction of sequential reads [0.0, 1.0].
// Returns 0.0 when no reads have been recorded.
func (rp ReadPattern) SeqRatio() float64 {
	total := rp.SeqCount + rp.RandCount
	if total == 0 {
		return 0.0
	}
	return float64(rp.SeqCount) / float64(total)
}

// slotBits and slotIndex mirror S3FS_SLOT_BITS / s3fs_slot_index in
// include/s3fs_shared_types.h: the fentry keeps an inode's read pattern in
// slot slotIndex(ino) of a per-CPU array, so a lookup is one array read.
const slotBits = 10

func slotIndex(ino uint64) uint32 {
	return uint32((ino * 0x9E3779B97F4A7C15) >> (64 - slotBits))
}

// ReadAccessPattern looks up the access pattern for ino from s3fs_ino_slots.
// Returns (zero, false) when no CPU's slot is currently owned by ino — it was
// not seen by fentry/generic_file_read_iter yet, or another inode took the
// slot over.
func (p *Programs) ReadAccessPattern(ino uint64) (ReadPattern, bool) {
	var vs []s3fsfpS3fsInoSlot
	key := slotIndex(ino)
	if err := p.Objs.S3fsInoSlots.Lookup(&key, &vs); err != nil {
		return ReadPattern{}, false
	}
	return mergeReadPatterns(ino, vs)
}

// mergeReadPatterns folds the per-CPU slots (AC-47: s3fs_ino_slots, a
// direct-mapped PERCPU_ARRAY) that belong to ino into one pattern: counters
// are summed (in 64 bits, so many CPUs' u32 counts cannot wrap), and
// LastEndPos is the furthest end any CPU has read — the sequential frontier
// the prefetch loop extends.  ok is false when no slot is owned by ino.
func mergeReadPatterns(ino uint64, vs []s3fsfpS3fsInoSlot) (rp ReadPattern, ok bool) {
	for _, v := range vs {
		if v.Ino != ino {
			continue
		}
		ok = true
		rp.SeqCount += uint64(v.SeqCount)
		rp.RandCount += uint64(v.RandCount)
		rp.TotalBytes += v.TotalBytes
		if v.LastEndPos > rp.LastEndPos {
			rp.LastEndPos = v.LastEndPos
		}
	}
	return rp, ok
}
