package main

import "testing"

func TestReadaheadWindow(t *testing.T) {
	const mib = 1 << 20
	for _, c := range []struct {
		cap  int64
		want int
	}{{0, 0}, {-1, 0}, {8 * mib, 0}, {16 * mib, 1}, {32 * mib, 2}, {1024 * mib, readaheadMaxChunks}} {
		if got := readaheadWindow(c.cap); got != c.want {
			t.Errorf("readaheadWindow(%d) = %d, want %d", c.cap, got, c.want)
		}
	}
}

func TestReadaheadRange(t *testing.T) {
	const ch = partialAdmitChunk
	cases := []struct {
		name             string
		miss, next, size int64
		window           int
		wantFrom, wantTo int64
	}{
		{"first miss", 0, 0, 100 * ch, 2, 1 * ch, 3 * ch},
		{"reader caught up with the window", 1 * ch, 3 * ch, 100 * ch, 2, 3 * ch, 4 * ch},
		{"nothing new", 2 * ch, 4 * ch, 100 * ch, 1, 0, 0},
		{"clipped at eof", 98 * ch, 0, 100 * ch, 2, 99 * ch, 100 * ch},
		{"short last chunk", 98 * ch, 0, 99*ch + 1, 2, 99 * ch, 99*ch + 1},
		{"at eof", 99 * ch, 0, 100 * ch, 2, 0, 0},
		{"reader went back", 0, 50 * ch, 100 * ch, 2, 1 * ch, 3 * ch},
	}
	for _, c := range cases {
		f, to := readaheadRange(c.miss, c.next, c.size, c.window)
		if f != c.wantFrom || to != c.wantTo {
			t.Errorf("%s: got [%d,%d), want [%d,%d)", c.name, f, to, c.wantFrom, c.wantTo)
		}
	}
}
