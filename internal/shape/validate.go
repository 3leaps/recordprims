package shape

import (
	"fmt"
	"io/fs"
	"strings"
	"unicode/utf8"

	"github.com/3leaps/recordprims/internal/wire"
)

// ReasonShape is the contract refusal code for a schema-invalid document.
const ReasonShape = "shape"

// Bounds on one validation. Exceeding them is a local limit, never a shape
// refusal and never a pass.
const (
	maxSteps       = 1 << 20 // schema-node visits per document
	maxUniqueItems = 4096    // array length for a uniqueItems pairwise check
)

var docs = map[string]*doc{}

// The schemas are compiled when the package loads; an inadmissible schema is
// a build failure, never a runtime pass.
func init() {
	for _, fam := range []string{"v0", "v1"} {
		entries, err := fs.ReadDir(schemaFS, "schemas/"+fam)
		if err != nil {
			panic(err)
		}
		for _, e := range entries {
			name := fam + "/" + e.Name()
			raw, err := schemaFS.ReadFile("schemas/" + name)
			if err != nil {
				panic(err)
			}
			d, err := compileDoc(name, raw)
			if err != nil {
				panic(err)
			}
			docs[name] = d
		}
	}
}

// Schemas lists the compiled schema documents, for tests.
func Schemas() []string {
	out := make([]string, 0, len(docs))
	for n := range docs {
		out = append(out, n)
	}
	return out
}

type state struct {
	d     *doc
	steps int
	limit bool
}

// failure is one shape refusal: the schema path and keyword, never document
// content (freeze 4.4).
type failure struct{ path, keyword string }

// Validate checks a document that passed wire.Ingest against the named
// schema ("v1/journal-entry.schema.json"). It returns nil, a *wire.Reject with
// reason "shape" naming the first failing schema path and keyword, or a
// *wire.Reject with reason "local_limit" when the bounds are exceeded.
func Validate(schema string, n *wire.Node) error {
	d, ok := docs[schema]
	if !ok {
		return fmt.Errorf("shape: unknown schema %q", schema)
	}
	st := &state{d: d}
	f := st.check(d.root, n)
	if st.limit {
		return &wire.Reject{Reason: wire.ReasonLocalLimit, Detail: "shape: validation work exceeds the local bound"}
	}
	if f != nil {
		return &wire.Reject{Reason: ReasonShape, Detail: schema + " " + f.path + " " + f.keyword}
	}
	return nil
}

func fail(n *node, keyword string) *failure { return &failure{path: n.path, keyword: keyword} }

// check returns the first failure of instance x against schema node s, or nil.
// All keywords of s apply, including those beside $ref (draft 2020-12).
func (st *state) check(s *node, x *wire.Node) *failure {
	if s == nil || st.limit {
		return nil
	}
	st.steps++
	if st.steps > maxSteps {
		st.limit = true
		return nil
	}
	if s.ref != "" {
		if f := st.check(st.d.defs[strings.TrimPrefix(s.ref, "#/$defs/")], x); f != nil {
			return f
		}
	}
	if s.typ != "" && !hasType(x, s.typ) {
		return fail(s, "type")
	}
	if s.isConst && !equal(x, s.consts[0]) {
		return fail(s, "const")
	}
	if s.hasEnum {
		ok := false
		for _, c := range s.consts {
			if equal(x, c) {
				ok = true
				break
			}
		}
		if !ok {
			return fail(s, "enum")
		}
	}
	switch x.Kind {
	case wire.String:
		if s.minLength >= 0 || s.maxLength >= 0 {
			n := utf8.RuneCountInString(x.Str)
			if s.minLength >= 0 && n < s.minLength {
				return fail(s, "minLength")
			}
			if s.maxLength >= 0 && n > s.maxLength {
				return fail(s, "maxLength")
			}
		}
		if s.pattern != nil && !s.pattern.MatchString(x.Str) {
			return fail(s, "pattern")
		}
		if s.dateTime && !isDateTime(x.Str) {
			return fail(s, "format")
		}
	case wire.Number:
		if s.minimum != nil || s.maximum != nil {
			r, ok := parseDecimal(x.Token)
			if !ok {
				return fail(s, "type")
			}
			if s.minimum != nil && r.cmp(*s.minimum) < 0 {
				return fail(s, "minimum")
			}
			if s.maximum != nil && r.cmp(*s.maximum) > 0 {
				return fail(s, "maximum")
			}
		}
	case wire.Object:
		for _, name := range s.required {
			if x.Get(name) == nil {
				return fail(s, "required")
			}
		}
		for _, m := range x.Fields {
			if p, ok := s.properties[m.Name]; ok {
				if f := st.check(p, m.Value); f != nil {
					return f
				}
			} else if s.additionalFalse {
				return fail(s, "additionalProperties")
			}
		}
	case wire.Array:
		if s.minItems >= 0 && len(x.Elems) < s.minItems {
			return fail(s, "minItems")
		}
		if s.maxItems >= 0 && len(x.Elems) > s.maxItems {
			return fail(s, "maxItems")
		}
		if s.items != nil {
			for _, e := range x.Elems {
				if f := st.check(s.items, e); f != nil {
					return f
				}
			}
		}
		if s.contains != nil {
			found := false
			for _, e := range x.Elems {
				if st.check(s.contains, e) == nil {
					found = true
					break
				}
			}
			if st.limit {
				return nil
			}
			if !found {
				return fail(s, "contains")
			}
		}
		if s.uniqueItems {
			if len(x.Elems) > maxUniqueItems {
				st.limit = true
				return nil
			}
			for i := 0; i < len(x.Elems); i++ {
				for j := i + 1; j < len(x.Elems); j++ {
					if equal(x.Elems[i], x.Elems[j]) {
						return fail(s, "uniqueItems")
					}
				}
			}
		}
	}
	for _, sub := range s.allOf {
		if f := st.check(sub, x); f != nil {
			return f
		}
	}
	if len(s.anyOf) > 0 {
		ok := false
		for _, sub := range s.anyOf {
			if st.check(sub, x) == nil {
				ok = true
				break
			}
		}
		if st.limit {
			return nil
		}
		if !ok {
			return fail(s, "anyOf")
		}
	}
	if len(s.oneOf) > 0 {
		matches := 0
		for _, sub := range s.oneOf {
			if st.check(sub, x) == nil {
				matches++
			}
		}
		if st.limit {
			return nil
		}
		if matches != 1 {
			return fail(s, "oneOf")
		}
	}
	if s.not != nil {
		if st.check(s.not, x) == nil {
			if st.limit {
				return nil
			}
			return fail(s, "not")
		}
	}
	if s.ifN != nil {
		if st.check(s.ifN, x) == nil {
			if f := st.check(s.thenN, x); f != nil {
				return f
			}
		} else if f := st.check(s.elseN, x); f != nil {
			return f
		}
	}
	return nil
}

// hasType is the JSON Schema type test. "integer" accepts any number whose
// exact value is an integer, whatever its spelling (12.0, 1.2e1).
func hasType(x *wire.Node, typ string) bool {
	switch typ {
	case "object":
		return x.Kind == wire.Object
	case "array":
		return x.Kind == wire.Array
	case "string":
		return x.Kind == wire.String
	case "boolean":
		return x.Kind == wire.Bool
	case "null":
		return x.Kind == wire.Null
	case "integer":
		if x.Kind != wire.Number {
			return false
		}
		r, ok := parseDecimal(x.Token)
		return ok && r.isInt()
	}
	return false
}

// equal is JSON Schema structural equality: type-sensitive (true is not 1),
// numbers compared by exact value (1 equals 1.0), objects compared as member
// sets regardless of order, arrays elementwise.
func equal(a, b *wire.Node) bool {
	if a.Kind != b.Kind {
		return false
	}
	switch a.Kind {
	case wire.Null:
		return true
	case wire.Bool:
		return a.Bool == b.Bool
	case wire.String:
		return a.Str == b.Str
	case wire.Number:
		ra, oka := parseDecimal(a.Token)
		rb, okb := parseDecimal(b.Token)
		return oka && okb && ra.cmp(rb) == 0
	case wire.Array:
		if len(a.Elems) != len(b.Elems) {
			return false
		}
		for i := range a.Elems {
			if !equal(a.Elems[i], b.Elems[i]) {
				return false
			}
		}
		return true
	case wire.Object:
		if len(a.Fields) != len(b.Fields) {
			return false
		}
		for _, m := range a.Fields {
			o := b.Get(m.Name)
			if o == nil || !equal(m.Value, o) {
				return false
			}
		}
		return true
	}
	return false
}
