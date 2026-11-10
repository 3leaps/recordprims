package journal

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

const instantLayout = "2006-01-02T15:04:05.000000Z"

// canonicalInstant is the only spelling a v1 instant may have.
var canonicalInstant = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}\.[0-9]{6}Z$`)

var rfc3339Instant = regexp.MustCompile(`^([0-9]{4})-([0-9]{2})-([0-9]{2})[Tt]([0-9]{2}):([0-9]{2}):([0-9]{2})(?:\.([0-9]+))?(?:([Zz])|([+-])([0-9]{2}):([0-9]{2}))$`)

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

// parseV0Instant parses a v0 RFC 3339 timestamp with
// reject_submicrosecond_nonzero=True, matching legacy.instant in the
// reference validator.
func parseV0Instant(s string) (time.Time, bool) {
	m := rfc3339Instant.FindStringSubmatch(s)
	if m == nil {
		return time.Time{}, false
	}
	year, _ := strconv.Atoi(m[1])
	month, _ := strconv.Atoi(m[2])
	day, _ := strconv.Atoi(m[3])
	hour, _ := strconv.Atoi(m[4])
	minute, _ := strconv.Atoi(m[5])
	second, _ := strconv.Atoi(m[6])
	if year < 1 || year > 9999 || month < 1 || month > 12 || day < 1 || day > 31 || hour > 23 || minute > 59 || second >= 60 {
		return time.Time{}, false
	}
	micro := 0
	if frac := m[7]; frac != "" {
		if len(frac) > 6 && strings.TrimRight(frac[6:], "0") != "" {
			return time.Time{}, false
		}
		padded := (frac + "000000")[:6]
		micro, _ = strconv.Atoi(padded)
	}
	loc := time.UTC
	if m[8] == "" {
		offH, _ := strconv.Atoi(m[10])
		offM, _ := strconv.Atoi(m[11])
		if offH > 23 || offM > 59 {
			return time.Time{}, false
		}
		secOffset := (offH*60 + offM) * 60
		if m[9] == "-" {
			secOffset = -secOffset
		}
		loc = time.FixedZone("", secOffset)
	}
	t := time.Date(year, time.Month(month), day, hour, minute, second, micro*1000, loc)
	if t.Year() != year || int(t.Month()) != month || t.Day() != day {
		return time.Time{}, false
	}
	utc := t.UTC()
	if utc.Year() < 1 || utc.Year() > 9999 {
		return time.Time{}, false
	}
	return utc, true
}
