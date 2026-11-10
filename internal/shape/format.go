package shape

import (
	"regexp"
	"strconv"
	"time"
)

// rfc3339DateTime matches RFC 3339 date-time strings under strict-rfc format
// mode and structural-leap/v1 leap-second policy (goneat v0.6.8+).
var rfc3339DateTime = regexp.MustCompile(
	`^([0-9]{4})-([0-9]{2})-([0-9]{2})[Tt]([0-9]{2}):([0-9]{2}):([0-9]{2})(?:\.[0-9]+)?(?:[Zz]|([+-])([0-9]{2}):([0-9]{2}))$`,
)

func daysInMonth(y, m int) int {
	switch m {
	case 1, 3, 5, 7, 8, 10, 12:
		return 31
	case 4, 6, 9, 11:
		return 30
	case 2:
		if y%4 == 0 && (y%100 != 0 || y%400 == 0) {
			return 29
		}
		return 28
	default:
		return 0
	}
}

func isDateTime(s string) bool {
	m := rfc3339DateTime.FindStringSubmatch(s)
	if m == nil {
		return false
	}
	year, _ := strconv.Atoi(m[1])
	month, _ := strconv.Atoi(m[2])
	day, _ := strconv.Atoi(m[3])
	hour, _ := strconv.Atoi(m[4])
	minute, _ := strconv.Atoi(m[5])
	sec, _ := strconv.Atoi(m[6])
	if month < 1 || month > 12 {
		return false
	}
	if day < 1 || day > daysInMonth(year, month) {
		return false
	}
	if hour > 23 || minute > 59 || sec > 60 {
		return false
	}
	offsetMin := 0
	if m[7] != "" {
		offH, _ := strconv.Atoi(m[8])
		offM, _ := strconv.Atoi(m[9])
		if offH > 23 || offM > 59 {
			return false
		}
		offsetMin = offH*60 + offM
		if m[7] == "-" {
			offsetMin = -offsetMin
		}
	}
	if sec == 60 {
		// structural-leap/v1: second 60 is valid iff the instant normalized to
		// UTC is 23:59:60 on the last day of its month.
		utc := time.Date(year, time.Month(month), day, hour, minute, 59, 0, time.FixedZone("", offsetMin*60)).UTC()
		if utc.Hour() != 23 || utc.Minute() != 59 || utc.Day() != daysInMonth(utc.Year(), int(utc.Month())) {
			return false
		}
	}
	return true
}
