package journal

import (
	"regexp"
	"time"
)

const instantLayout = "2006-01-02T15:04:05.000000Z"

// canonicalInstant is the only spelling a v1 instant may have.
var canonicalInstant = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}\.[0-9]{6}Z$`)

// CanonicalSpelling reports whether s has the v1 instant spelling, whatever
// its calendar validity.
func CanonicalSpelling(s string) bool { return canonicalInstant.MatchString(s) }

// ParseInstant parses a v1 instant: YYYY-MM-DDTHH:MM:SS.ffffffZ with ASCII
// digits, a year from 0001 to 9999, a valid proleptic Gregorian date, hour
// 00-23, minute and second 00-59 (a leap second is refused) and exactly six
// fractional digits. Any other spelling is refused, never normalized.
func ParseInstant(s string) (time.Time, bool) {
	if !canonicalInstant.MatchString(s) {
		return time.Time{}, false
	}
	t, err := time.Parse(instantLayout, s)
	if err != nil || t.Year() < 1 {
		return time.Time{}, false
	}
	return t, true
}

func instantOf(s string, where string) (time.Time, error) {
	t, ok := ParseInstant(s)
	if !ok {
		return time.Time{}, reject(ReasonInstantSpelling, "%s: %q is not a canonical v1 instant", where, s)
	}
	return t, nil
}
