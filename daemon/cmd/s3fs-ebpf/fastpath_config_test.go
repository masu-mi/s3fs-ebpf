package main

import (
	"strings"
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

func TestFastpathModesValidate(t *testing.T) {
	for _, tt := range []struct {
		name string
		m    fastpathModes
		want string // substring of the error; "" = valid
	}{
		{"defaults", fastpathModes{patternAdmit: true}, ""},
		{"nothing set", fastpathModes{}, ""},
		{"pattern admit without the hook", fastpathModes{patternAdmit: true, noReadIterHook: true}, "--pattern-aware-admit requires"},
		{"prefetch-seq without the hook", fastpathModes{prefetchSeq: true, noReadIterHook: true}, "--prefetch-seq-admit requires"},
		{"hook off alone is fine", fastpathModes{noReadIterHook: true}, ""},
		{"pcache-prewarm needs pcache mode", fastpathModes{pcachePrewarm: true}, "--pcache-prewarm requires --arena-metadata-only"},
		{"pcache-prewarm with pcache mode", fastpathModes{pcachePrewarm: true, arenaMetadataOnly: true}, ""},
	} {
		err := tt.m.validate()
		switch {
		case tt.want == "" && err != nil:
			t.Errorf("%s: unexpected error %v", tt.name, err)
		case tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)):
			t.Errorf("%s: err = %v, want one containing %q", tt.name, err, tt.want)
		}
	}
}
