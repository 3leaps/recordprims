package recordprims

import (
	"fmt"
	"time"

	"github.com/3leaps/recordprims/internal/journal"
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

// ParseInstant parses an instant in the canonical spelling
// YYYY-MM-DDTHH:MM:SS.ffffffZ: UTC, a literal Z, and exactly six fractional
// digits. Any other spelling (an offset, a lowercase t or z, fewer or more
// fractional digits) is refused, never normalized, and so is a canonical-looking
// instant that is not a valid date and time (February 30, hour 24, a leap
// second, year 0000).
func ParseInstant(s string) (time.Time, error) {
	if !journal.CanonicalSpelling(s) {
		return time.Time{}, fmt.Errorf("%w: instant spelling", ErrSchema)
	}
	t, ok := journal.ParseInstant(s)
	if !ok {
		return time.Time{}, fmt.Errorf("%w: instant is not a valid date and time", ErrSchema)
	}
	return t, nil
}
