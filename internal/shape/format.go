package shape

import "time"

// dateTimeLayouts are the layouts the contract's reference shape validator
// (goneat v0.6.7, through gojsonschema's DateTimeFormatChecker) accepts for
// format "date-time", tried in this order with Go's time.Parse.
//
// Scope: this is restricted compatibility with the pinned schemas and that
// pinned reference validator, not general draft 2020-12 format validation and
// not a statement of what date-time means in the contract. The reference is
// more lenient than RFC 3339 date-time: a bare date, a bare time, a comma
// fraction and a one-digit hour pass. In v1 every date-time position also
// carries the canonical instant pattern, and the instant_spelling semantics
// still apply, so v1 behaviour is unaffected. The leniency reaches only v0
// documents, and recordprims does not qualify v0-source ingestion on this
// parity: the contract's intended date-time semantics are an open upstream
// question.
var dateTimeLayouts = []string{
	"15:04:05",
	"15:04:05Z07:00",
	"2006-01-02",
	time.RFC3339,
	time.RFC3339Nano,
}

func isDateTime(s string) bool {
	for _, l := range dateTimeLayouts {
		if _, err := time.Parse(l, s); err == nil {
			return true
		}
	}
	return false
}
