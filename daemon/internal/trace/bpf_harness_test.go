//go:build bpfharness

// AC-66: drives bpf/test/wtinylfu_harness.bpf.c (policy_wtinylfu_pick_ops's
// operations as a SEC("syscall") program).  Needs root; run it through
// scripts/test-wtinylfu-harness.sh, which builds the object and sets
// HARNESS_OBJ.

package trace

import (
	"encoding/binary"
	"fmt"
	"os"
	"testing"

	"github.com/cilium/ebpf"
)

type drv struct{ p *ebpf.Program }

func (d drv) call(op uint32, ino, arg uint64) uint64 {
	in := make([]byte, 32)
	binary.LittleEndian.PutUint32(in[0:], op)
	binary.LittleEndian.PutUint64(in[8:], ino)
	binary.LittleEndian.PutUint64(in[16:], arg)
	out := make([]byte, 32)
	if _, err := d.p.Run(&ebpf.RunOptions{Context: in, ContextOut: out}); err != nil {
		panic(err)
	}
	return binary.LittleEndian.Uint64(out[24:])
}

func load(t *testing.T) (drv, *ebpf.Collection) {
	spec, err := ebpf.LoadCollectionSpec(os.Getenv("HARNESS_OBJ"))
	if err != nil {
		t.Fatal(err)
	}
	for n, p := range spec.Programs {
		if p.Type == ebpf.StructOps {
			delete(spec.Programs, n)
		}
	}
	for n, m := range spec.Maps {
		if m.Type == ebpf.StructOpsMap {
			delete(spec.Maps, n)
		}
	}
	coll, err := ebpf.NewCollectionWithOptions(spec, ebpf.CollectionOptions{Programs: ebpf.ProgramOptions{LogLevel: 0}})
	if err != nil {
		t.Fatalf("%+v", err)
	}
	return drv{coll.Programs["drive"]}, coll
}

func replayBPF(d drv, cap int, ev []Event, sized bool) (hits int) {
	res := map[uint64]bool{}
	for _, e := range ev {
		d.call(1, e.Ino, 0)
		if res[e.Ino] {
			hits++
			continue
		}
		for len(res) >= cap {
			v := d.call(4, 0, 0)
			if v == 0 || !res[v] {
				panic(fmt.Sprintf("pick returned %d (resident %d)", v, len(res)))
			}
			d.call(3, v, 0)
			delete(res, v)
		}
		d.call(2, e.Ino, 0)
		if sized {
			d.call(5, e.Ino, 4096)
		}
		res[e.Ino] = true
	}
	return
}

func TestBPFMatchesClockSim(t *testing.T) {
	for _, cap := range []int{2, 8, 32} {
		for _, sized := range []bool{false, true} {
			ev := zipfWithScans(200, 20000, 0.9, 2000, 4*cap)
			d, coll := load(t)
			b := replayBPF(d, cap, ev, sized)
			coll.Close()
			w := uint64(1)
			if sized {
				w = 4096
			}
			g := Replay(newWTinyLFUClock(cap, w), ev).Hits
			fmt.Printf("cap=%d sized=%v bpf=%d go=%d\n", cap, sized, b, g)
			if b != g {
				t.Errorf("cap=%d sized=%v: bpf hits %d != go clock sim %d", cap, sized, b, g)
			}
		}
	}
}

// Picked inos that are only shed (pending) stay resident and come back
// through on_insert; the policy must keep naming resident inos.
func TestBPFLimbo(t *testing.T) {
	d, coll := load(t)
	defer coll.Close()
	ev := zipfWithScans(100, 20000, 0.9, 1000, 16)
	res := map[uint64]bool{}
	cap, shed, back := 8, 0, 0
	for i, e := range ev {
		d.call(1, e.Ino, 0)
		if res[e.Ino] {
			continue
		}
		var kept []uint64
		for tries := 0; len(res)-len(kept) >= cap && tries < 64; tries++ {
			v := d.call(4, 0, 0)
			if v == 0 {
				t.Fatalf("event %d: pick returned 0 with %d resident", i, len(res))
			}
			if !res[v] {
				t.Fatalf("event %d: pick returned non-resident %d", i, v)
			}
			if i%3 == 0 && len(kept) == 0 {
				kept = append(kept, v) // shed: stays, re-inserted next round
				shed++
				continue
			}
			d.call(3, v, 0)
			delete(res, v)
		}
		d.call(2, e.Ino, 0)
		d.call(5, e.Ino, 4096)
		res[e.Ino] = true
		for _, k := range kept {
			d.call(2, k, 0) // next round's on_insert
			back++
			// The shed ino goes away later (the reader finished).
			if i%2 == 0 {
				d.call(3, k, 0)
				delete(res, k)
			}
		}
	}
	fmt.Printf("shed=%d restored=%d resident=%d\n", shed, back, len(res))
}
