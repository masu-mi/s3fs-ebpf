package main

import (
	"testing"
)

func TestS3fifoGhostCapFor(t *testing.T) {
	for _, tt := range []struct {
		softcap int
		want    uint32
	}{
		{-1, 8192}, // leave the kmod default (8): the minimum
		{0, 8192},  // capacity mode: no softcap to scale from
		{1, 8192},  // 1*1024 is below the minimum
		{8, 8192},  // exactly the minimum
		{9, 9216},  // scales as softcap*1024 above it
		{32, 32768},
		{64, 65536},   // exactly the cap
		{1000, 65536}, // capped (about 1 MB of map)
	} {
		if got := s3fifoGhostCapFor(tt.softcap); got != tt.want {
			t.Errorf("s3fifoGhostCapFor(%d) = %d, want %d", tt.softcap, got, tt.want)
		}
	}
}
