package main

import "testing"

func TestReadaheadWindow(t *testing.T) {
	const mib = 1 << 20
	for _, c := range []struct {
		cap  int64
		want int
	}{{0, 0}, {-1, 0}, {8 * mib, 0}, {12 * mib, 0}, {16 * mib, 1}, {32 * mib, 3}, {1024 * mib, readaheadMaxChunks}} {
		if got := readaheadWindow(c.cap); got != c.want {
			t.Errorf("readaheadWindow(%d) = %d, want %d", c.cap, got, c.want)
		}
	}
}

func TestReadaheadAhead(t *testing.T) {
	const mib = 1 << 20
	for _, c := range []struct {
		cap  int64
		want int
	}{{0, 0}, {12 * mib, 0}, {16 * mib, 2}, {32 * mib, 6}, {64 * mib, readaheadMaxAhead}, {1024 * mib, readaheadMaxAhead}} {
		if got := readaheadAhead(c.cap); got != c.want {
			t.Errorf("readaheadAhead(%d) = %d, want %d", c.cap, got, c.want)
		}
	}
}

func TestReadaheadRestart(t *testing.T) {
	const ch = partialAdmitChunk
	for _, c := range []struct {
		name             string
		next, miss, want int64
		ahead            int
	}{
		{"reader inside the requested range", 7 * ch, 3 * ch, 7 * ch, 6},
		{"reader caught up with the range", 4 * ch, 4 * ch, 5 * ch, 6},
		{"reader jumped past it", 4 * ch, 40 * ch, 41 * ch, 6},
		{"reader went back by more than ahead", 50 * ch, 10 * ch, 11 * ch, 6},
		{"reader went back within ahead", 12 * ch, 6 * ch, 12 * ch, 6},
	} {
		if got := readaheadRestart(c.next, c.miss, c.ahead); got != c.want {
			t.Errorf("%s: next = %d chunks, want %d", c.name, got/ch, c.want/ch)
		}
	}
}
