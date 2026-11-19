package exchange

import (
	"strings"
	"testing"
)

func TestParseDecimal(t *testing.T) {
	t.Parallel()
	good := map[string]string{"0": "0/1", "0.00000100": "1/1000000", "12345.5": "24691/2", "007": "7/1"}
	for in, want := range good {
		r, err := ParseDecimal(in)
		if err != nil {
			t.Errorf("ParseDecimal(%q): %v", in, err)
			continue
		}
		if r.String() != want {
			t.Errorf("ParseDecimal(%q) = %s, want %s", in, r, want)
		}
	}
	bad := []string{"", "-1", "+1", "1e5", "1E5", "1/3", "0x10", ".5", "5.", "1.2.3", " 1", "1 ", "NaN", "Inf", "1_000",
		strings.Repeat("1", MaxDecimalLen+1)}
	for _, in := range bad {
		if _, err := ParseDecimal(in); err == nil {
			t.Errorf("ParseDecimal(%q) accepted", in)
		}
	}
	if _, err := ParseDecimal(strings.Repeat("9", MaxDecimalLen+10)); err == nil || strings.Contains(err.Error(), "999999") {
		t.Errorf("long input must be rejected without echoing it: %v", err)
	}
}
