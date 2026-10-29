package metrics

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

func resetCustom(t *testing.T) {
	t.Helper()
	customGaugesMu.Lock()
	customGauges = make(map[string]float64)
	customGaugesMu.Unlock()
	t.Cleanup(func() {
		customGaugesMu.Lock()
		customGauges = make(map[string]float64)
		customGaugesMu.Unlock()
	})
}

func TestRecordCustomCap(t *testing.T) {
	resetCustom(t)
	for i := 0; i < CustomGaugeCap; i++ {
		if !RecordCustom(fmt.Sprintf("m%d", i), float64(i)) {
			t.Fatalf("name %d rejected below the cap", i)
		}
	}
	if RecordCustom("one-too-many", 1) {
		t.Error("a new name was accepted past the cap")
	}
	if !RecordCustom("m0", 42) {
		t.Error("updating an existing name was rejected at the cap")
	}
}

func TestWriteCustom(t *testing.T) {
	resetCustom(t)
	var b bytes.Buffer
	writeCustom(&b)
	if b.Len() != 0 {
		t.Errorf("no gauges should print nothing, got %q", b.String())
	}
	RecordCustom("zeta", 2)
	RecordCustom("alpha", 1.5)
	b.Reset()
	writeCustom(&b)
	out := b.String()
	a, z := strings.Index(out, `name="alpha"} 1.5`), strings.Index(out, `name="zeta"} 2`)
	if a < 0 || z < 0 || a > z {
		t.Errorf("gauges missing or not sorted by name:\n%s", out)
	}
	if !strings.Contains(out, "# TYPE s3fs_ebpf_policy_metric gauge") {
		t.Errorf("no TYPE line:\n%s", out)
	}
}

func TestEscapeLabel(t *testing.T) {
	for in, want := range map[string]string{
		"plain":     "plain",
		`a"b`:       `a\"b`,
		`a\b`:       `a\\b`,
		"a\nb":      `a\nb`,
		`"\` + "\n": `\"\\\n`,
	} {
		if got := escapeLabel(in); got != want {
			t.Errorf("escapeLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFormatFloat(t *testing.T) {
	for in, want := range map[float64]string{
		0: "0", 3: "3", -7: "-7", 1.5: "1.5", 1e16: "1e+16", 0.001: "0.001",
	} {
		if got := formatFloat(in); got != want {
			t.Errorf("formatFloat(%v) = %q, want %q", in, got, want)
		}
	}
}

// The label value is escaped once (escapeLabel); a second pass through %q
// would turn a quote into \\\" and break the exposition format.
func TestWriteCustomEscapesLabelOnce(t *testing.T) {
	resetCustom(t)
	RecordCustom(`say "hi"`+"\n"+`back\slash`, 1)
	var b bytes.Buffer
	writeCustom(&b)
	want := `s3fs_ebpf_policy_metric{name="say \"hi\"\nback\\slash"} 1` + "\n"
	if !strings.Contains(b.String(), want) {
		t.Errorf("output lacks %q:\n%s", want, b.String())
	}
}
