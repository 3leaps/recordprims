//go:build recordprims_conformance

package conformance

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/3leaps/recordprims/internal/wire"
)

// schemaKinds maps each vendored schema file to the wire document kind used
// to ingest it.
var schemaKinds = map[string]string{
	"journal-entry.schema.json":               wire.KindJournalEntry,
	"record.schema.json":                      wire.KindRecord,
	"write-receipt.schema.json":               wire.KindWriteReceipt,
	"stratum-profile.schema.json":             wire.KindStratumProfile,
	"protection-request.schema.json":          wire.KindProtectionRequest,
	"protection-receipt.schema.json":          wire.KindProtectionReceipt,
	"export-request.schema.json":              wire.KindExportRequest,
	"export-result.schema.json":               wire.KindExportResult,
	"body-state-request.schema.json":          wire.KindBodyStateRequest,
	"stream-seal.schema.json":                 wire.KindStreamSeal,
	"removal-eligibility-request.schema.json": wire.KindRemovalEligibility,
}

// TestProtocolIntsCoverSchemas walks every vendored v0 and v1 schema,
// resolving local $refs and descending through properties, items and the
// combinators, and checks that every integer-typed member (type "integer", or
// an integer const or enum) is a protocol-integer position in the wire path
// map. A pin bump that adds an integer field the map does not know fails here
// instead of letting the field be read as an opaque number.
func TestProtocolIntsCoverSchemas(t *testing.T) {
	root := repoRoot(t)
	var problems []string
	files := 0
	for _, fam := range []string{"v0", "v1"} {
		dir := filepath.Join(root, "contract", "crucible", "schemas", "durable-record", fam)
		matches, err := filepath.Glob(filepath.Join(dir, "*.schema.json"))
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range matches {
			base := filepath.Base(f)
			kind, ok := schemaKinds[base]
			if !ok {
				problems = append(problems, fmt.Sprintf("%s/%s: no wire kind mapped", fam, base))
				continue
			}
			b, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			var doc map[string]any
			if err := json.Unmarshal(b, &doc); err != nil {
				t.Fatalf("%s: %v", f, err)
			}
			w := &walker{doc: doc, where: fam + "/" + base}
			w.object(doc, kind, "$", 0)
			problems = append(problems, w.problems...)
			files++
		}
	}
	if files < 13 {
		t.Fatalf("walked %d schema files, expected at least 13", files)
	}
	sort.Strings(problems)
	for _, p := range problems {
		t.Error(p)
	}
}

type walker struct {
	doc      map[string]any
	where    string
	problems []string
}

func (w *walker) fail(path, format string, a ...any) {
	w.problems = append(w.problems, w.where+" "+path+": "+fmt.Sprintf(format, a...))
}

func (w *walker) resolve(s map[string]any, path string) map[string]any {
	for i := 0; i < 32; i++ {
		ref, ok := s["$ref"].(string)
		if !ok {
			return s
		}
		if !strings.HasPrefix(ref, "#/$defs/") {
			w.fail(path, "non-local $ref %q", ref)
			return s
		}
		defs, _ := w.doc["$defs"].(map[string]any)
		next, ok := defs[strings.TrimPrefix(ref, "#/$defs/")].(map[string]any)
		if !ok {
			w.fail(path, "unresolved $ref %q", ref)
			return s
		}
		s = next
	}
	w.fail(path, "$ref chain too deep")
	return s
}

// isIntLeaf reports whether s constrains a value to integers.
func isIntLeaf(s map[string]any) bool {
	if t, ok := s["type"].(string); ok && t == "integer" {
		return true
	}
	if c, ok := s["const"].(float64); ok && c == math.Trunc(c) {
		return true
	}
	if e, ok := s["enum"].([]any); ok && len(e) > 0 {
		for _, v := range e {
			f, ok := v.(float64)
			if !ok || f != math.Trunc(f) {
				return false
			}
		}
		return true
	}
	return false
}

// object walks a schema s describing a value in a position of the given wire
// kind (an object kind, "list:<kind>", "int" or "opaque").
func (w *walker) object(s map[string]any, kind, path string, depth int) {
	if depth > 64 {
		w.fail(path, "schema nesting too deep")
		return
	}
	s = w.resolve(s, path)
	if tl, ok := s["type"].([]any); ok {
		w.fail(path, "type list %v is not supported by the walker", tl)
	}
	if t, ok := s["type"].(string); ok && t == "number" {
		w.fail(path, "type number: decide whether this is a protocol integer")
	}
	if isIntLeaf(s) && kind != "int" {
		w.fail(path, "integer-typed, but the wire map treats it as %q", kind)
	}
	for _, k := range []string{"allOf", "anyOf", "oneOf"} {
		if list, ok := s[k].([]any); ok {
			for i, sub := range list {
				if m, ok := sub.(map[string]any); ok {
					w.object(m, kind, fmt.Sprintf("%s/%s[%d]", path, k, i), depth+1)
				}
			}
		}
	}
	for _, k := range []string{"not", "if", "then", "else"} {
		if m, ok := s[k].(map[string]any); ok {
			w.object(m, kind, path+"/"+k, depth+1)
		}
	}
	if props, ok := s["properties"].(map[string]any); ok {
		for name, sub := range props {
			m, ok := sub.(map[string]any)
			if !ok {
				continue
			}
			child := "opaque"
			if wire.IsObjectKind(kind) {
				child = wire.MemberKind(kind, name)
			}
			w.object(m, child, path+"."+name, depth+1)
		}
	}
	if items, ok := s["items"].(map[string]any); ok {
		elem := "opaque"
		if strings.HasPrefix(kind, "list:") {
			elem = strings.TrimPrefix(kind, "list:")
		}
		w.object(items, elem, path+"[]", depth+1)
	}
	for _, k := range []string{"additionalProperties", "patternProperties", "prefixItems"} {
		if _, ok := s[k].(map[string]any); ok {
			w.fail(path, "%s schema is not supported by the walker", k)
		}
	}
}
