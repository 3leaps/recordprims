package shape

import (
	"math/big"
	"regexp"
	"strings"
)

// decimal is the exact value of a JSON number token, normalised without
// expanding its exponent: value = (-1)^neg * 0.D1D2...Dn * 10^pointExp, with
// the digits D free of leading and trailing zeros. Zero has no digits. Huge
// exponents ("1e-99999999", "0e99999999") cost nothing to represent or compare.
type decimal struct {
	neg      bool
	digits   string   // significant digits, no leading or trailing zeros
	pointExp *big.Int // position of the decimal point relative to the digits
}

var numberToken = regexp.MustCompile(`^(-?)(0|[1-9][0-9]*)(?:\.([0-9]+))?(?:[eE]([+-]?[0-9]+))?$`)

func parseDecimal(token string) (decimal, bool) {
	m := numberToken.FindStringSubmatch(token)
	if m == nil {
		return decimal{}, false
	}
	intPart, frac, exp := m[2], m[3], m[4]
	all := intPart + frac
	lead := len(all) - len(strings.TrimLeft(all, "0"))
	digits := strings.TrimRight(all[lead:], "0")
	if digits == "" {
		return decimal{pointExp: new(big.Int)}, true // zero, whatever its sign
	}
	// value = 0.digits * 10^(len(intPart) - lead + e)
	e := new(big.Int)
	if exp != "" {
		if _, ok := e.SetString(exp, 10); !ok {
			return decimal{}, false
		}
	}
	e.Add(e, big.NewInt(int64(len(intPart)-lead)))
	return decimal{neg: m[1] == "-", digits: digits, pointExp: e}, true
}

func (d decimal) isZero() bool { return d.digits == "" }

// isInt reports whether the value is an integer: every significant digit sits
// left of the decimal point.
func (d decimal) isInt() bool {
	if d.isZero() {
		return true
	}
	return d.pointExp.Cmp(big.NewInt(int64(len(d.digits)))) >= 0
}

// cmp compares two exact values without expanding either exponent.
func (d decimal) cmp(o decimal) int {
	switch {
	case d.isZero() && o.isZero():
		return 0
	case d.isZero():
		if o.neg {
			return 1
		}
		return -1
	case o.isZero():
		if d.neg {
			return -1
		}
		return 1
	case d.neg != o.neg:
		if d.neg {
			return -1
		}
		return 1
	}
	mag := d.pointExp.Cmp(o.pointExp)
	if mag == 0 {
		// Same magnitude: compare digit strings padded to equal length.
		a, b := d.digits, o.digits
		if len(a) < len(b) {
			a += strings.Repeat("0", len(b)-len(a))
		} else if len(b) < len(a) {
			b += strings.Repeat("0", len(a)-len(b))
		}
		mag = strings.Compare(a, b)
	}
	if d.neg {
		return -mag
	}
	return mag
}
