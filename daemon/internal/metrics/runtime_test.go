package metrics

import (
	"bytes"
	"strings"
	"testing"
)

// TestWriteRuntime_EmitsExpectedSeries pins J-02: the /metrics
// endpoint must expose the standard go_* series names so any
// vanilla client_golang Grafana dashboard binds to them without
// dashboard edits. Also asserts the format shape (# HELP + # TYPE +
// value) so a stray Fprint would fail loudly.
func TestWriteRuntime_EmitsExpectedSeries(t *testing.T) {
	var buf bytes.Buffer
	writeRuntime(&buf)
	body := buf.String()

	required := []string{
		"go_goroutines",
		"go_threads",
		"go_memstats_alloc_bytes",
		"go_memstats_sys_bytes",
		"go_memstats_heap_objects",
		"go_memstats_next_gc_bytes",
		"go_gc_runs_total",
		"go_gc_pause_seconds_total",
	}
	for _, name := range required {
		if !strings.Contains(body, "# HELP "+name+" ") {
			t.Errorf("missing HELP line for %s", name)
		}
		if !strings.Contains(body, "# TYPE "+name+" ") {
			t.Errorf("missing TYPE line for %s", name)
		}
		if !strings.Contains(body, "\n"+name+" ") {
			t.Errorf("missing sample line for %s", name)
		}
	}
}
