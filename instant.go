package recordprims

import (
	"fmt"
	"time"
)

// instantLayout is the canonical emitted spelling for every instant: UTC, a
// literal Z, and exactly six fractional digits.
const instantLayout = "2006-01-02T15:04:05.000000Z"

// FormatInstant renders t in the canonical spelling. Sub-microsecond precision
// is truncated only because t is produced by the store's own clock; inputs are
// validated by ParseInstant, which never rounds.
func FormatInstant(t time.Time) string {
	return t.UTC().Truncate(time.Microsecond).Format(instantLayout)
}

// ParseInstant parses an RFC 3339 instant. An instant with precision finer than
// a microsecond is rejected unless the extra digits are all zero; it is never
// rounded.
func ParseInstant(s string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: invalid instant", ErrSchema)
	}
	if t.Nanosecond()%1000 != 0 {
		return time.Time{}, fmt.Errorf("%w: instant precision finer than a microsecond", ErrSchema)
	}
	return t.UTC(), nil
}
