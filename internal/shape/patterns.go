package shape

// admittedPatterns are the exact pattern strings of the pinned schemas.
// Each is compiled with Go's RE2 and has semantics tests in
// patterns_test.go (unanchored search, anchors, trailing newline, Unicode).
// A pin that adds or changes a pattern fails schema admission until the new
// pattern is evaluated and listed here.
var admittedPatterns = map[string]bool{
	`^(?:[0-9a-f]{2})*$`: true,
	`^([^Tt]*|[^Tt]*[Tt][^:]*(:[^:]*)?|[^Tt]*[Tt][^:]*:[^:]*:[0-5][0-9].*)$`: true,
	`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-5][0-9]\.[0-9]{6}Z$`:   true,
	`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`:                                    true,
	`^[A-Za-z0-9][A-Za-z0-9._:-]{0,255}$`:                                    true,
	`^[A-Za-z0-9][A-Za-z0-9:._-]{0,255}$`:                                    true,
	`^[a-z0-9][a-z0-9!#$&^_.+-]{0,126}/[a-z0-9][a-z0-9!#$&^_.+-]{0,126}$`:    true,
	`^[a-z0-9][a-z0-9.-]{0,63}$`:                                             true,
	`^[a-z0-9][a-z0-9._-]{0,127}$`:                                           true,
	`^[a-z0-9][a-z0-9._-]{0,63}$`:                                            true,
	`^[a-z0-9][a-z0-9._:-]{0,63}$`:                                           true,
	`^[a-z0-9][a-z0-9_-]{0,63}$`:                                             true,
	`^[a-z][a-z0-9_]{0,31}$`:                                                 true,
	`^contract: [a-z0-9][a-z0-9-]*/v[0-9]+$`:                                 true,
	`^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}$`:                                     true,
	`^[a-z0-9][a-z0-9._:-]{0,127}$`:                                          true,
	`^sha256:[0-9a-f]{64}$`:                                                  true,
}
