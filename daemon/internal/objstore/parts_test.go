package objstore

import (
	"errors"
	"math/rand"
	"testing"
)

const mib = 1 << 20
const chunk = 4 * mib

func kindsOf(s string) []ChunkKind {
	k := make([]ChunkKind, len(s))
	for i, c := range s {
		switch c {
		case 'D':
			k[i] = ChunkData
		case 'H':
			k[i] = ChunkHole
		case 'R':
			k[i] = ChunkRemote
		}
	}
	return k
}

// checkPlan asserts the invariants every plan must satisfy.
func checkPlan(t *testing.T, kinds []ChunkKind, size int64, parts []PartSpec) {
	t.Helper()
	var off int64
	for i, p := range parts {
		if p.Off != off || p.Len <= 0 {
			t.Fatalf("part %d = %+v, want Off=%d Len>0 (parts %v)", i, p, off, parts)
		}
		if i < len(parts)-1 && p.Len < MinPartSize {
			t.Fatalf("part %d is %d bytes, below the S3 minimum (parts %v)", i, p.Len, parts)
		}
		if p.Kind == PartCopy {
			if p.Len > MaxCopyPart {
				t.Fatalf("copy part %d too large", i)
			}
			// a copy may only cover remote bytes
			for ci := p.Off / chunk; ci*chunk < p.Off+p.Len; ci++ {
				if kinds[ci] != ChunkRemote {
					t.Fatalf("copy part %d covers non-remote chunk %d (parts %v)", i, ci, parts)
				}
			}
		} else if p.Len > MaxUploadPart {
			t.Fatalf("upload part %d too large", i)
		}
		off += p.Len
	}
	if off != size {
		t.Fatalf("parts cover %d bytes, want %d", off, size)
	}
	if len(parts) > MaxParts {
		t.Fatalf("%d parts", len(parts))
	}
}

func TestPlanParts_MidWrite100MiB(t *testing.T) {
	// 25 chunks; chunk 12 holds the write.
	k := kindsOf("RRRRRRRRRRRRDRRRRRRRRRRRR")
	parts, err := PlanParts(k, chunk, 100*mib)
	if err != nil {
		t.Fatal(err)
	}
	checkPlan(t, k, 100*mib, parts)
	if len(parts) != 3 || parts[0].Kind != PartCopy || parts[1].Kind != PartUpload || parts[2].Kind != PartCopy {
		t.Fatalf("parts = %v, want copy / upload / copy", parts)
	}
	if parts[1].Len != 5*mib {
		t.Fatalf("upload = %d bytes, want the 4 MiB chunk padded to 5 MiB", parts[1].Len)
	}
}

func TestPlanParts_NoRemoteIsPlainPut(t *testing.T) {
	if _, err := PlanParts(kindsOf("DDHD"), chunk, 16*mib); !errors.Is(err, ErrNoCopy) {
		t.Fatalf("err = %v, want ErrNoCopy", err)
	}
}

func TestPlanParts_ShortRemoteBetweenWrites(t *testing.T) {
	k := kindsOf("DRD") // the 4 MiB remote chunk is below the part minimum
	if _, err := PlanParts(k, chunk, 12*mib); !errors.Is(err, ErrNoCopy) {
		t.Fatalf("err = %v, want ErrNoCopy (all bytes end up uploaded)", err)
	}
}

func TestPlanParts_TailChunkShorter(t *testing.T) {
	size := int64(10*chunk + 123)
	k := kindsOf("RRRRRDRRRRR")
	parts, err := PlanParts(k, chunk, size)
	if err != nil {
		t.Fatal(err)
	}
	checkPlan(t, k, size, parts)
}

func TestPlanParts_Random(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for iter := 0; iter < 20000; iter++ {
		n := 1 + rng.Intn(60)
		k := make([]ChunkKind, n)
		for i := range k {
			switch r := rng.Intn(10); {
			case r < 6:
				k[i] = ChunkRemote
			case r < 9:
				k[i] = ChunkData
			default:
				k[i] = ChunkHole
			}
		}
		size := int64(n-1)*chunk + 1 + rng.Int63n(chunk)
		parts, err := PlanParts(k, chunk, size)
		if errors.Is(err, ErrNoCopy) {
			continue
		}
		if err != nil {
			t.Fatalf("iter %d: %v", iter, err)
		}
		checkPlan(t, k, size, parts)
	}
}

func TestPlanParts_HugeUploadIsSplit(t *testing.T) {
	n := 200 // 800 MiB of data chunks after one remote run
	k := make([]ChunkKind, n)
	for i := range k {
		k[i] = ChunkData
	}
	for i := 0; i < 5; i++ {
		k[i] = ChunkRemote
	}
	parts, err := PlanParts(k, chunk, int64(n)*chunk)
	if err != nil {
		t.Fatal(err)
	}
	checkPlan(t, k, int64(n)*chunk, parts)
}
