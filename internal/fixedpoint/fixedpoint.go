// Package fixedpoint converts exchange decimal strings to int64 values with a
// fixed number of decimal places (scale) and back. It is used where *big.Rat
// would be too costly, such as order books with thousands of levels.
//
// A value v at scale s represents v / 10^s. Conversions are exact: input with
// more significant decimal places than the scale, or a magnitude that does
// not fit in int64, is rejected rather than rounded.
package fixedpoint

import (
	"errors"
	"fmt"
	"math"
	"math/big"
	"strings"
)

// MaxScale is the largest supported scale. 10^18 is the largest power of ten
// that fits in int64.
const MaxScale = 18

// Errors returned by the conversions.
var (
	// ErrScale reports a value with more significant decimal places than the scale.
	ErrScale = errors.New("fixedpoint: value exceeds scale")
	// ErrOverflow reports a value whose scaled magnitude does not fit in int64.
	ErrOverflow = errors.New("fixedpoint: value overflows int64")
	// ErrSyntax reports a string that is not a plain non-negative decimal.
	ErrSyntax = errors.New("fixedpoint: invalid decimal")
)

var pow10 = func() [MaxScale + 1]int64 {
	var p [MaxScale + 1]int64
	p[0] = 1
	for i := 1; i <= MaxScale; i++ {
		p[i] = p[i-1] * 10
	}
	return p
}()

// split validates s as a plain non-negative decimal ("123", "0.0100") and
// returns its integer part and its fraction without trailing zeros.
func split(s string) (intPart, frac string, err error) {
	if s == "" || len(s) > 64 {
		return "", "", ErrSyntax
	}
	intPart, frac, hasDot := strings.Cut(s, ".")
	if intPart == "" || (hasDot && frac == "") {
		return "", "", ErrSyntax
	}
	for _, part := range [2]string{intPart, frac} {
		for i := 0; i < len(part); i++ {
			if part[i] < '0' || part[i] > '9' {
				return "", "", ErrSyntax
			}
		}
	}
	return intPart, strings.TrimRight(frac, "0"), nil
}

// Decimals returns the number of significant decimal places in s
// ("0.0100" → 2, "5" → 0).
func Decimals(s string) (int, error) {
	_, frac, err := split(s)
	if err != nil {
		return 0, err
	}
	return len(frac), nil
}

// Parse converts s to an int64 at the given scale.
func Parse(s string, scale int) (int64, error) {
	if scale < 0 || scale > MaxScale {
		return 0, fmt.Errorf("fixedpoint: scale %d not in [0, %d]", scale, MaxScale)
	}
	intPart, frac, err := split(s)
	if err != nil {
		return 0, err
	}
	if len(frac) > scale {
		return 0, ErrScale
	}
	frac += strings.Repeat("0", scale-len(frac))
	intPart = strings.TrimLeft(intPart, "0")
	if len(intPart)+scale > 19 {
		return 0, ErrOverflow
	}
	var v uint64
	for _, c := range intPart + frac {
		v = v*10 + uint64(c-'0')
		if v > math.MaxInt64 {
			return 0, ErrOverflow
		}
	}
	return int64(v), nil
}

// ScaleOf returns the smallest scale at which r is an integer, or ErrScale
// when r needs more than MaxScale decimal places. r must be non-negative.
func ScaleOf(r *big.Rat) (int, error) {
	if r == nil || r.Sign() < 0 {
		return 0, ErrSyntax
	}
	x := new(big.Rat).Set(r)
	ten := big.NewRat(10, 1)
	for s := 0; s <= MaxScale; s++ {
		if x.IsInt() {
			return s, nil
		}
		x.Mul(x, ten)
	}
	return 0, ErrScale
}

// FromRat converts a non-negative r to an int64 at the given scale.
func FromRat(r *big.Rat, scale int) (int64, error) {
	if scale < 0 || scale > MaxScale {
		return 0, fmt.Errorf("fixedpoint: scale %d not in [0, %d]", scale, MaxScale)
	}
	if r == nil || r.Sign() < 0 {
		return 0, ErrSyntax
	}
	x := new(big.Rat).Mul(r, new(big.Rat).SetInt64(pow10[scale]))
	if !x.IsInt() {
		return 0, ErrScale
	}
	if !x.Num().IsInt64() {
		return 0, ErrOverflow
	}
	return x.Num().Int64(), nil
}

// ToRat returns v / 10^scale as an exact rational.
func ToRat(v int64, scale int) *big.Rat {
	if scale < 0 || scale > MaxScale {
		panic(fmt.Sprintf("fixedpoint: scale %d not in [0, %d]", scale, MaxScale))
	}
	return new(big.Rat).SetFrac(big.NewInt(v), big.NewInt(pow10[scale]))
}
