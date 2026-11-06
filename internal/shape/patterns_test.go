package shape

import (
	"strings"
	"testing"
)

// patternSamples holds one value each pinned pattern must match. The tests
// below then check that $ does not match before a trailing newline, that the
// patterns are anchored (no prefix or suffix slips through an unanchored
// search), and that non-ASCII lookalikes are refused.
var patternSamples = map[string]string{
	`^(?:[0-9a-f]{2})*$`: "00ff",
	`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}\.[0-9]{6}Z$`:  "2026-10-20T00:00:01.000000Z",
	`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`:                                 "Rec:1.a-b_c",
	`^[A-Za-z0-9][A-Za-z0-9._:-]{0,255}$`:                                 "Key:1.a-b_c",
	`^[A-Za-z0-9][A-Za-z0-9:._-]{0,255}$`:                                 "Key:1.a-b_c",
	`^[a-z0-9][a-z0-9!#$&^_.+-]{0,126}/[a-z0-9][a-z0-9!#$&^_.+-]{0,126}$`: "text/plain",
	`^[a-z0-9][a-z0-9.-]{0,63}$`:                                          "records.example",
	`^[a-z0-9][a-z0-9._-]{0,127}$`:                                        "records.example_1",
	`^[a-z0-9][a-z0-9._-]{0,63}$`:                                         "records.example_1",
	`^[a-z0-9][a-z0-9._:-]{0,63}$`:                                        "store:bodies.1",
	`^[a-z0-9][a-z0-9_-]{0,63}$`:                                          "standard_1",
	`^[a-z][a-z0-9_]{0,31}$`:                                              "verified",
	`^contract: [a-z0-9][a-z0-9-]*/v[0-9]+$`:                              "contract: example/v0",
	`^sha256:[0-9a-f]{64}$`:                                               "sha256:" + strings.Repeat("ab", 32),
}

// Every pinned pattern has a sample, and every pattern the schemas use is
// pinned (admission refuses unpinned ones; this keeps the sets equal).
func TestPatternSetIsPinned(t *testing.T) {
	used := map[string]bool{}
	for _, d := range docs {
		var walk func(n *node)
		seen := map[*node]bool{}
		walk = func(n *node) {
			if n == nil || seen[n] {
				return
			}
			seen[n] = true
			if n.pattern != nil {
				used[n.pattern.String()] = true
			}
			for _, p := range n.properties {
				walk(p)
			}
			for _, g := range [][]*node{n.allOf, n.anyOf, n.oneOf} {
				for _, s := range g {
					walk(s)
				}
			}
			for _, s := range []*node{n.items, n.contains, n.not, n.ifN, n.thenN, n.elseN} {
				walk(s)
			}
		}
		walk(d.root)
		for _, n := range d.defs {
			walk(n)
		}
	}
	for p := range admittedPatterns {
		if !used[p] {
			t.Errorf("pinned pattern %q is not used by any schema", p)
		}
		if _, ok := patternSamples[p]; !ok {
			t.Errorf("pinned pattern %q has no semantics sample", p)
		}
	}
	for p := range used {
		if !admittedPatterns[p] {
			t.Errorf("schema pattern %q is not pinned", p)
		}
	}
}

func TestPatternSemantics(t *testing.T) {
	for p, sample := range patternSamples {
		v := compileTest(t, `,"type":"string","pattern":`+jsonString(p))
		bad := []string{
			sample + "\n",     // $ must not match before a trailing newline
			"\n" + sample,     // ^ must not match after a leading newline
			" " + sample,      // anchored: no unanchored-search prefix
			sample + " ",      // anchored: no suffix
			sample + "\u00a0", // a non-ASCII trailing space
		}
		// A non-ASCII lookalike of the first character.
		bad = append(bad, "\uff10"+sample[1:])
		ok := []string{sample}
		for _, b := range bad {
			if err := v(jsonString(b)); err == nil {
				t.Errorf("pattern %s accepted %q", p, b)
			}
		}
		for _, s := range ok {
			if err := v(jsonString(s)); err != nil {
				t.Errorf("pattern %s refused its sample %q: %v", p, s, err)
			}
		}
	}
}

// jsonString renders s as a JSON string literal.
func jsonString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"' || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r == '\n':
			b.WriteString(`\n`)
		case r < 0x20:
			b.WriteString(`\u00`)
			b.WriteString("0123456789abcdef"[r>>4 : r>>4+1])
			b.WriteString("0123456789abcdef"[r&15 : r&15+1])
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
