package metrics

import (
	"bytes"
	"math"
	"strings"
	"testing"
	"time"
)

func TestHistogramBucketsAreCumulative(t *testing.T) {
	h := NewHistogram("t_seconds", "test", []float64{0.01, 0.1, 1})
	h.Observe(5 * time.Millisecond)   // le 0.01, 0.1, 1
	h.Observe(50 * time.Millisecond)  // le 0.1, 1
	h.Observe(500 * time.Millisecond) // le 1
	h.Observe(2 * time.Second)        // +Inf only
	var b bytes.Buffer
	h.writeTo(&b)
	out := b.String()
	for _, want := range []string{
		"# HELP t_seconds test\n",
		"# TYPE t_seconds histogram\n",
		`t_seconds_bucket{le="0.01"} 1` + "\n",
		`t_seconds_bucket{le="0.1"} 2` + "\n",
		`t_seconds_bucket{le="1"} 3` + "\n",
		`t_seconds_bucket{le="+Inf"} 4` + "\n",
		"t_seconds_count 4\n",
		"t_seconds_sum 2.555\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

// A value equal to a bound belongs to that bucket (le is inclusive).
func TestHistogramBoundIsInclusive(t *testing.T) {
	h := NewHistogram("t", "", []float64{1})
	h.Observe(time.Second)
	var b bytes.Buffer
	h.writeTo(&b)
	if !strings.Contains(b.String(), `t_bucket{le="1"} 1`) {
		t.Errorf("an observation equal to the bound missed its bucket:\n%s", b.String())
	}
}

func TestHistogramDoesNotAliasBounds(t *testing.T) {
	bounds := []float64{1, 2}
	h := NewHistogram("t", "", bounds)
	bounds[0] = 99
	if h.bounds[0] != 1 {
		t.Error("NewHistogram kept the caller's slice")
	}
}

func TestFormatBound(t *testing.T) {
	for in, want := range map[float64]string{
		0.005: "0.005", 1: "1", 2.5: "2.5", math.Inf(1): "+Inf",
	} {
		if got := formatBound(in); got != want {
			t.Errorf("formatBound(%v) = %q, want %q", in, got, want)
		}
	}
}
