package wire

import (
	"fmt"
	"math"
	"math/big"
	"regexp"
	"strconv"
	"strings"
)

// MaxSafeInteger is the largest protocol integer: 2^53 - 1.
const MaxSafeInteger = 1<<53 - 1

var jsonNumber = regexp.MustCompile(`^(-?)(0|[1-9][0-9]*)(?:\.([0-9]+))?(?:[eE]([+-]?[0-9]+))?$`)

func numericf(format string, a ...any) error {
	return &Reject{Reason: ReasonNumericDomain, Detail: fmt.Sprintf(format, a...)}
}

// SafeInt is the numeric_domain check for one protocol-integer token: the
// token's exact value must be an integer of magnitude at most MaxSafeInteger.
// Spellings such as 12.0, 1.2e1 and -0.0 are integers; a huge exponent is
// refused without being expanded.
func SafeInt(token string) (int64, error) {
	m := jsonNumber.FindStringSubmatch(token)
	if m == nil {
		return 0, ingestf("invalid JSON numeric token %q", token)
	}
	sign, intPart, frac, exp := m[1], m[2], m[3], m[4]
	digits := strings.TrimLeft(intPart+frac, "0")
	if digits == "" {
		return 0, nil
	}
	trimmed := strings.TrimRight(digits, "0")
	baseScale := (len(digits) - len(trimmed)) - len(frac)
	expVal := 0
	if exp != "" {
		neg := strings.HasPrefix(exp, "-")
		expDigits := strings.TrimLeft(strings.TrimLeft(exp, "+-"), "0")
		if expDigits == "" {
			expDigits = "0"
		}
		bound := abs(baseScale) + 16
		if len(expDigits) > len(strconv.Itoa(bound)) {
			return 0, numericf("exponent outside safe integer domain (%q)", token)
		}
		e, _ := strconv.Atoi(expDigits)
		if e > bound {
			return 0, numericf("exponent outside safe integer domain (%q)", token)
		}
		if neg {
			e = -e
		}
		expVal = e
	}
	scale := baseScale + expVal
	if scale < 0 {
		return 0, numericf("non-integral numeric token %q", token)
	}
	if len(trimmed) > 16 || len(trimmed)+scale > 16 {
		return 0, numericf("%q exceeds safe integer ceiling %d", token, MaxSafeInteger)
	}
	mag, err := strconv.ParseInt(trimmed+strings.Repeat("0", scale), 10, 64)
	if err != nil || mag > MaxSafeInteger {
		return 0, numericf("%q exceeds safe integer ceiling %d", token, MaxSafeInteger)
	}
	if sign == "-" {
		return -mag, nil
	}
	return mag, nil
}

// checkOpaque is stage 1's check on a number outside a protocol-integer
// position (a record payload, for example): it must be a finite IEEE 754
// binary64 value, and an integer spelling must be exactly representable.
func checkOpaque(token string) error {
	m := jsonNumber.FindStringSubmatch(token)
	if m == nil {
		return ingestf("invalid JSON numeric token %q", token)
	}
	intPart, frac, exp := m[2], m[3], m[4]
	isFloat := frac != "" || exp != ""
	if strings.TrimLeft(intPart+frac, "0") == "" {
		return nil
	}
	if !isFloat {
		if len(intPart) > 315 {
			return ingestf("integer overflows IEEE 754 binary64")
		}
		v, ok := new(big.Int).SetString(token, 10)
		if !ok {
			return ingestf("invalid JSON numeric token %q", token)
		}
		// SetInt at precision 0 keeps the exact value, so the accuracy below
		// is that of the binary64 conversion itself.
		f, acc := new(big.Float).SetInt(v).Float64()
		if acc != big.Exact || math.IsInf(f, 0) {
			return ingestf("integer is outside the I-JSON IEEE 754 binary64 domain")
		}
		return nil
	}
	if exp != "" {
		neg := strings.HasPrefix(exp, "-")
		expDigits := strings.TrimLeft(strings.TrimLeft(exp, "+-"), "0")
		if expDigits == "" {
			expDigits = "0"
		}
		limit := len(token) + 400
		if e, err := strconv.Atoi(expDigits); len(expDigits) > len(strconv.Itoa(limit)) || err != nil || e > limit {
			if neg {
				return nil // underflows to zero
			}
			return ingestf("non-finite IEEE 754 number %q", token)
		}
	}
	f, err := strconv.ParseFloat(token, 64)
	if math.IsInf(f, 0) || math.IsNaN(f) {
		return ingestf("non-finite IEEE 754 number %q", token)
	}
	_ = err // an underflow to zero is finite and accepted
	return nil
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}
