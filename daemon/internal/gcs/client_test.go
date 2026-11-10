package gcs

import "testing"

func TestParseGenTag(t *testing.T) {
	for _, tc := range []struct {
		in string
		n  int64
		ok bool
	}{
		{"gen:1699999999", 1699999999, true},
		{"gen:0", 0, false},
		{"gen:", 0, false},
		{"\"abc\"", 0, false},
	} {
		n, ok := parseGenTag(tc.in)
		if n != tc.n || ok != tc.ok {
			t.Errorf("parseGenTag(%q) = %d,%v want %d,%v", tc.in, n, ok, tc.n, tc.ok)
		}
	}
}
