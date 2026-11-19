package fixedpoint

import (
	"errors"
	"math/big"
	"strconv"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in    string
		scale int
		want  int64
		err   error
	}{
		{"90500.01000000", 2, 9050001, nil},
		{"0.00001000", 5, 1, nil},
		{"0", 8, 0, nil},
		{"0.00000000", 0, 0, nil},
		{"007.50", 1, 75, nil},
		{"5", 0, 5, nil},
		{"0.001", 2, 0, ErrScale},
		{"9223372036854775807", 0, 9223372036854775807, nil},
		{"9223372036854775808", 0, 0, ErrOverflow},
		{"92233720368.54775807", 8, 9223372036854775807, nil},
		{"92233720368.54775808", 8, 0, ErrOverflow},
		{"100000000000", 8, 0, ErrOverflow},
		{"", 2, 0, ErrSyntax},
		{"-1", 2, 0, ErrSyntax},
		{"1e3", 2, 0, ErrSyntax},
		{".5", 2, 0, ErrSyntax},
		{"5.", 2, 0, ErrSyntax},
		{"1.2.3", 2, 0, ErrSyntax},
		{" 1", 2, 0, ErrSyntax},
		{strings.Repeat("1", 65), 0, 0, ErrSyntax},
	}
	for _, tc := range tests {
		got, err := Parse(tc.in, tc.scale)
		if !errors.Is(err, tc.err) || (err == nil && got != tc.want) {
			t.Errorf("Parse(%q, %d) = %d, %v; want %d, %v", tc.in, tc.scale, got, err, tc.want, tc.err)
		}
	}
	if _, err := Parse("1", 19); err == nil {
		t.Error("scale 19 must be rejected")
	}
	if _, err := Parse("1", -1); err == nil {
		t.Error("negative scale must be rejected")
	}
}

func TestDecimalsAndScaleOf(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]int{"0.01000000": 2, "1": 0, "1.0": 0, "0.00001": 5} {
		if got, err := Decimals(in); err != nil || got != want {
			t.Errorf("Decimals(%q) = %d, %v", in, got, err)
		}
		r, _ := new(big.Rat).SetString(in)
		if got, err := ScaleOf(r); err != nil || got != want {
			t.Errorf("ScaleOf(%s) = %d, %v", in, got, err)
		}
	}
	if _, err := ScaleOf(big.NewRat(1, 3)); !errors.Is(err, ErrScale) {
		t.Errorf("1/3 must not have a finite scale: %v", err)
	}
	if _, err := ScaleOf(big.NewRat(-1, 1)); err == nil {
		t.Error("negative must be rejected")
	}
}

func TestRatRoundTrip(t *testing.T) {
	t.Parallel()
	r, _ := new(big.Rat).SetString("90500.01")
	v, err := FromRat(r, 2)
	if err != nil || v != 9050001 {
		t.Fatalf("FromRat: %d %v", v, err)
	}
	if ToRat(v, 2).Cmp(r) != 0 {
		t.Fatal("round trip")
	}
	if _, err := FromRat(r, 1); !errors.Is(err, ErrScale) {
		t.Fatalf("want ErrScale, got %v", err)
	}
	big1, _ := new(big.Rat).SetString("100000000000")
	if _, err := FromRat(big1, 8); !errors.Is(err, ErrOverflow) {
		t.Fatalf("want ErrOverflow, got %v", err)
	}
}

func FuzzParse(f *testing.F) {
	for _, s := range []string{"0", "1.5", "90500.01000000", "0.00000001", "9223372036854775807"} {
		f.Add(s, 8)
	}
	f.Fuzz(func(t *testing.T, s string, scale int) {
		if scale < 0 || scale > MaxScale {
			return
		}
		v, err := Parse(s, scale)
		if err != nil {
			return
		}
		// A successful parse must round-trip exactly through big.Rat.
		r, ok := new(big.Rat).SetString(s)
		if !ok || ToRat(v, scale).Cmp(r) != 0 {
			t.Fatalf("Parse(%q, %d) = %d does not equal the input", s, scale, v)
		}
		if strconv.FormatInt(v, 10) == "" {
			t.Fatal("unreachable")
		}
	})
}
