package shape

import (
	"errors"
	"strings"
	"testing"

	"github.com/3leaps/recordprims/internal/wire"
)

// compileTest admits a synthetic schema whose root is body (a JSON object
// without the braces) and returns a validator for it.
func compileTest(t *testing.T, body string) func(instance string) error {
	t.Helper()
	raw := `{"$schema":"` + draft202012 + `"` + body + `}`
	d, err := compileDoc("test.schema.json", []byte(raw))
	if err != nil {
		t.Fatalf("compile %s: %v", body, err)
	}
	return func(instance string) error {
		n, err := wire.ParseJSON([]byte(instance))
		if err != nil {
			t.Fatalf("instance %s: %v", instance, err)
		}
		st := &state{d: d}
		if f := st.check(d.root, n); f != nil {
			return &wire.Reject{Reason: ReasonShape, Detail: f.path + " " + f.keyword}
		}
		if st.limit {
			return &wire.Reject{Reason: wire.ReasonLocalLimit}
		}
		return nil
	}
}

func expect(t *testing.T, v func(string) error, ok []string, bad []string) {
	t.Helper()
	for _, s := range ok {
		if err := v(s); err != nil {
			t.Errorf("%s: %v, want valid", s, err)
		}
	}
	for _, s := range bad {
		var r *wire.Reject
		if err := v(s); !errors.As(err, &r) || r.Reason != ReasonShape {
			t.Errorf("%s: %v, want shape refusal", s, err)
		}
	}
}

func TestTypeKeyword(t *testing.T) {
	v := compileTest(t, `,"type":"integer"`)
	expect(t, v, []string{`1`, `-1`, `12.0`, `1.2e1`, `0`, `-0.0`, `1e300`},
		[]string{`1.5`, `"1"`, `true`, `null`, `[]`, `{}`, `1e-5`})
	for typ, ok := range map[string]string{
		"string": `"x"`, "boolean": `false`, "null": `null`, "array": `[]`, "object": `{}`,
	} {
		v := compileTest(t, `,"type":"`+typ+`"`)
		expect(t, v, []string{ok}, []string{`1`})
	}
}

// const, enum and uniqueItems use type-sensitive structural equality:
// true is not 1, 1 equals 1.0, objects compare as member sets.
func TestEquality(t *testing.T) {
	expect(t, compileTest(t, `,"const":1`), []string{`1`, `1.0`, `1e0`, `10e-1`}, []string{`true`, `"1"`, `2`, `1.0000000000000000001`})
	expect(t, compileTest(t, `,"const":true`), []string{`true`}, []string{`1`, `false`})
	expect(t, compileTest(t, `,"const":{"a":1,"b":[1,"x"]}`),
		[]string{`{"b":[1.0,"x"],"a":1}`}, []string{`{"a":1}`, `{"a":1,"b":["x",1]}`, `{"a":1,"b":[1,"x"],"c":0}`})
	expect(t, compileTest(t, `,"enum":["a",2,null]`), []string{`"a"`, `2.0`, `null`}, []string{`"b"`, `3`, `false`})
	expect(t, compileTest(t, `,"uniqueItems":true`),
		[]string{`[1,"1",true]`, `[{"a":1},{"a":2}]`, `[]`},
		[]string{`[1,1.0]`, `[{"a":1,"b":2},{"b":2,"a":1}]`, `[null,null]`})
}

// Lengths count Unicode code points, not bytes.
func TestStringLengthCountsCodePoints(t *testing.T) {
	v := compileTest(t, `,"minLength":2,"maxLength":3`)
	expect(t, v, []string{`"ab"`, `"abc"`, "\"\U0001F600\U0001F600\"", "\"a\U0001F600b\""},
		[]string{`"a"`, `"abcd"`, "\"\U0001F600\"", "\"\U0001F600\U0001F600\U0001F600\U0001F600\""})
}

func TestNumericBounds(t *testing.T) {
	v := compileTest(t, `,"minimum":1,"maximum":9007199254740991`)
	expect(t, v, []string{`1`, `1.0`, `9007199254740991`, `"x"`},
		[]string{`0`, `0.9999999999999999999`, `9007199254740992`, `9007199254740991.5`, `-1`})
	// Huge exponents compare without expansion.
	expect(t, compileTest(t, `,"maximum":10`), []string{`1e-99999999`, `0e99999999`}, []string{`1e99999999`})
}

func TestObjectKeywords(t *testing.T) {
	v := compileTest(t, `,"type":"object","required":["a"],"properties":{"a":{"type":"string"},"b":{"type":"integer"}},"additionalProperties":false`)
	expect(t, v, []string{`{"a":"x"}`, `{"a":"x","b":2}`},
		[]string{`{}`, `{"a":1}`, `{"a":"x","b":"y"}`, `{"a":"x","c":1}`})
	// additionalProperties sees only its own schema object's properties.
	w := compileTest(t, `,"type":"object","properties":{"a":{}},"additionalProperties":false,"allOf":[{"properties":{"b":{}}}]`)
	expect(t, w, []string{`{"a":1}`}, []string{`{"b":1}`})
}

func TestArrayKeywords(t *testing.T) {
	v := compileTest(t, `,"type":"array","items":{"type":"integer"},"minItems":1,"maxItems":2,"contains":{"const":1}`)
	expect(t, v, []string{`[1]`, `[2,1]`}, []string{`[]`, `[2]`, `[1,1,1]`, `[1,"x"]`})
}

func TestComposition(t *testing.T) {
	one := compileTest(t, `,"oneOf":[{"type":"integer"},{"minimum":5}]`)
	expect(t, one, []string{`1`, `5.5`, `"x"`}, []string{`7`}) // "x" matches the minimum branch only
	any := compileTest(t, `,"anyOf":[{"type":"integer"},{"type":"string"}]`)
	expect(t, any, []string{`1`, `"x"`}, []string{`true`})
	all := compileTest(t, `,"allOf":[{"type":"integer"},{"minimum":2}]`)
	expect(t, all, []string{`2`}, []string{`1`, `"x"`})
	not := compileTest(t, `,"not":{"type":"string"}`)
	expect(t, not, []string{`1`}, []string{`"x"`})
	cond := compileTest(t, `,"if":{"properties":{"k":{"const":"a"}}},"then":{"required":["x"]},"else":{"required":["y"]}`)
	// An absent k satisfies the if vacuously, so then applies: {"y":1} is invalid.
	expect(t, cond, []string{`{"k":"a","x":1}`, `{"k":"b","y":1}`, `{"x":1}`}, []string{`{"k":"a","y":1}`, `{"k":"b","x":1}`, `{"y":1}`})
	noop := compileTest(t, `,"if":{"type":"string"}`)
	expect(t, noop, []string{`1`, `"x"`}, nil)
}

func TestRefResolvesAgainstRoot(t *testing.T) {
	v := compileTest(t, `,"$ref":"#/$defs/r","$defs":{"r":{"type":"object","required":["a"]}}`)
	expect(t, v, []string{`{"a":1}`}, []string{`{}`, `1`})
}

// Diagnostics name the schema path and keyword and never quote the document.
func TestDiagnosticCarriesNoContent(t *testing.T) {
	v := compileTest(t, `,"type":"object","properties":{"secret":{"maxLength":1}}`)
	err := v(`{"secret":"hunter2-value"}`)
	if err == nil || strings.Contains(err.Error(), "hunter2") || !strings.Contains(err.Error(), "maxLength") {
		t.Fatalf("diagnostic %q", err)
	}
}

func TestBoundsAreLocalLimits(t *testing.T) {
	v := compileTest(t, `,"uniqueItems":true`)
	big := "[" + strings.Repeat("0,", maxUniqueItems) + "0]"
	var r *wire.Reject
	if err := v(big); !errors.As(err, &r) || r.Reason != wire.ReasonLocalLimit {
		t.Fatalf("%v, want local_limit", err)
	}
}

// Admission refuses anything outside the subset, anywhere in the document.
func TestAdmissionRefusals(t *testing.T) {
	for name, body := range map[string]string{
		"unknown keyword":               `,"multipleOf":2`,
		"unknown keyword in $defs":      `,"$defs":{"x":{"propertyNames":{}}}`,
		"unknown keyword in else":       `,"if":{},"then":{},"else":{"dependentRequired":{}}`,
		"type number":                   `,"type":"number"`,
		"type list":                     `,"type":["string","null"]`,
		"non-local ref":                 `,"$ref":"https://example.com/s.json"`,
		"unresolved ref":                `,"$ref":"#/$defs/missing"`,
		"ref with assertion sibling":    `,"$ref":"#/$defs/a","type":"object","$defs":{"a":{}}`,
		"ref-only cycle":                `,"$ref":"#/$defs/a","$defs":{"a":{"$ref":"#/$defs/b"},"b":{"allOf":[{"$ref":"#/$defs/a"}]}}`,
		"boolean schema":                `,"not":true`,
		"object additionalProperties":   `,"additionalProperties":{"type":"string"}`,
		"format other than date-time":   `,"format":"email"`,
		"pattern not in the pinned set": `,"pattern":"^a$"`,
		"$defs below the root":          `,"properties":{"a":{"$defs":{}}}`,
		"negative maxLength":            `,"maxLength":-1`,
		"empty anyOf":                   `,"anyOf":[]`,
	} {
		raw := `{"$schema":"` + draft202012 + `"` + body + `}`
		if _, err := compileDoc("test.schema.json", []byte(raw)); err == nil {
			t.Errorf("%s: admitted", name)
		}
	}
	if _, err := compileDoc("t", []byte(`{"$schema":"http://json-schema.org/draft-07/schema#"}`)); err == nil {
		t.Error("draft-07 $schema admitted")
	}
}

func TestDecimal(t *testing.T) {
	for _, c := range []struct {
		a, b string
		cmp  int
	}{
		{"1", "1.0", 0}, {"1.2e1", "12", 0}, {"-0", "0", 0}, {"0.05", "5e-2", 0},
		{"1e-99999999", "0", 1}, {"-1e-99999999", "0", -1}, {"1e99999999", "1e99999998", 1},
		{"9007199254740991", "9007199254740992", -1}, {"-2", "-1", -1}, {"0.1", "0.10000000000000001", -1},
	} {
		da, _ := parseDecimal(c.a)
		db, _ := parseDecimal(c.b)
		if got := da.cmp(db); got != c.cmp {
			t.Errorf("cmp(%s, %s) = %d, want %d", c.a, c.b, got, c.cmp)
		}
	}
	for tok, isInt := range map[string]bool{"12": true, "12.0": true, "1.2e1": true, "1.25e1": false, "0e5": true, "1e-1": false, "120e-1": true} {
		d, _ := parseDecimal(tok)
		if d.isInt() != isInt {
			t.Errorf("isInt(%s) = %v", tok, !isInt)
		}
	}
}
