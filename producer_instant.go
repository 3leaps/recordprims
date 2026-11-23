package recordprims

import (
	"fmt"
	"time"
)

// NormalizeProducerInstant converts a UTC instant observed by a producer into
// the canonical spelling, for use in new records before their digest is
// computed. It accepts exactly three spellings:
//
//	2026-11-23T12:00:00Z         whole seconds
//	2026-11-23T12:00:00.123Z     exactly three fractional digits
//	2026-11-23T12:00:00.123456Z  exactly six fractional digits (returned unchanged)
//
// The result always has six fractional digits; the conversion only appends
// zeros, so it is lossless. Every other spelling is refused, never rounded or
// repaired: other fraction lengths, an offset, a lowercase t or z, a comma
// fraction, an invalid calendar date, a leap second and year 0000.
//
// This is a producer helper. Stored and received instants are verified with
// [ParseInstant], which accepts only the canonical spelling.
func NormalizeProducerInstant(s string) (string, error) {
	var canonical string
	switch {
	case len(s) == 20 && s[19] == 'Z':
		canonical = s[:19] + ".000000Z"
	case len(s) == 24 && s[19] == '.' && s[23] == 'Z':
		canonical = s[:23] + "000Z"
	case len(s) == 27:
		canonical = s
	default:
		return "", fmt.Errorf("%w: producer instant spelling", ErrSchema)
	}
	if _, err := ParseInstant(canonical); err != nil {
		return "", err
	}
	return canonical, nil
}

// ProducerInstant returns the canonical spelling of t for use in a new record.
// It refuses a value with a sub-microsecond remainder instead of truncating
// it, and a value outside years 0001 to 9999. (The store's own clock is
// formatted by [FormatInstant], which truncates.)
func ProducerInstant(t time.Time) (string, error) {
	if t.Nanosecond()%int(time.Microsecond) != 0 {
		return "", fmt.Errorf("%w: producer instant has a sub-microsecond remainder", ErrSchema)
	}
	u := t.UTC()
	if u.Year() < 1 || u.Year() > 9999 {
		return "", fmt.Errorf("%w: producer instant outside years 0001 to 9999", ErrSchema)
	}
	return u.Format(instantLayout), nil
}
