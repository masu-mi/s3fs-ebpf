package metrics

import (
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// CustomGaugeCap limits how many distinct metric names Policy CEL /
// user code can emit. Once reached, subsequent new names are silently
// dropped (existing names still update). Keeps /metrics cardinality
// bounded — a runaway policy that emits a fresh name per Entry would
// otherwise saturate Prometheus.
const CustomGaugeCap = 32

// customGauges holds the last-observed value for each caller-supplied
// metric name. Prometheus semantics: a gauge, one time series per
// name. Renders as:
//
//	s3fs_ebpf_policy_metric{name="<name>"} <value>
//
// The value is float64 to match Prometheus's wire format; sources
// (CEL uint/int/double) coerce on the way in.
var (
	customGaugesMu sync.RWMutex
	customGauges   = make(map[string]float64)
)

// RecordCustom stores a metric value under `name`. Names past
// CustomGaugeCap are dropped (a warning would be nice, but silent for
// the hot path since CEL calls this per Entry evaluation). Returns
// true if the value was stored, false if the name was rejected.
func RecordCustom(name string, value float64) bool {
	customGaugesMu.Lock()
	defer customGaugesMu.Unlock()
	if _, exists := customGauges[name]; !exists {
		if len(customGauges) >= CustomGaugeCap {
			return false
		}
	}
	customGauges[name] = value
	return true
}

// writeCustom emits all custom gauges in Prometheus text format.
// Sorted by name for stable output (some Prometheus scraper stacks
// care about ordering when computing rate() across scrapes).
func writeCustom(w io.Writer) {
	customGaugesMu.RLock()
	if len(customGauges) == 0 {
		customGaugesMu.RUnlock()
		return
	}
	names := make([]string, 0, len(customGauges))
	for n := range customGauges {
		names = append(names, n)
	}
	values := make(map[string]float64, len(customGauges))
	for k, v := range customGauges {
		values[k] = v
	}
	customGaugesMu.RUnlock()
	sort.Strings(names)

	fmt.Fprintln(w, "# HELP s3fs_ebpf_policy_metric Custom gauge emitted by CEL policy metric(name, value) calls.")
	fmt.Fprintln(w, "# TYPE s3fs_ebpf_policy_metric gauge")
	for _, n := range names {
		fmt.Fprintf(w, "s3fs_ebpf_policy_metric{name=%q} %s\n",
			escapeLabel(n), formatFloat(values[n]))
	}
}

// escapeLabel escapes a Prometheus label value (backslash + double
// quote + newline). Overly conservative but keeps parser happy for
// unusual policy-supplied names.
func escapeLabel(s string) string {
	if !strings.ContainsAny(s, "\\\"\n") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 4)
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteString("\\\\")
		case '"':
			b.WriteString("\\\"")
		case '\n':
			b.WriteString("\\n")
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// formatFloat renders v in the shortest form that round-trips.
// Integers print without a decimal point.
func formatFloat(v float64) string {
	// If v is a whole number and fits in int64, print as int for
	// compactness. Prometheus accepts either.
	if v == float64(int64(v)) && v >= -1e15 && v <= 1e15 {
		return strconv.FormatInt(int64(v), 10)
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}
