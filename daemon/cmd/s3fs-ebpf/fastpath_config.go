package main

// s3fifoGhostMap is the BPF map the S3-FIFO policy keeps its ghost queue in.
const s3fifoGhostMap = "s3fifo3_state"

const (
	s3fifoGhostMinCap = uint32(8192)
	s3fifoGhostMaxCap = uint32(65536)
)

// s3fifoGhostCapFor sizes the S3-FIFO ghost map from the --softcap flag (Z-03).
// Ghost entries are about 4x the expected number of admitted files so that
// promotion still fires after large working-set eviction cycles:
// max(8192, softcap*1024), capped at 65536 (about 1 MB of map).  softcap <= 0
// ("leave the kmod default of 8") gives the minimum.
func s3fifoGhostCapFor(softcap int) uint32 {
	ghost := s3fifoGhostMinCap
	if softcap > 0 {
		if est := uint32(softcap) * 1024; est > ghost {
			ghost = est
		}
	}
	if ghost > s3fifoGhostMaxCap {
		ghost = s3fifoGhostMaxCap
	}
	return ghost
}
