//go:build recordprims_conformance

package conformance

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/3leaps/recordprims/internal/shape"
	"github.com/3leaps/recordprims/internal/wire"
)

// schemaKind maps a schema stem to the wire kind used to ingest documents of
// that schema.
var schemaKind = map[string]string{
	"journal-entry": wire.KindJournalEntry, "record": wire.KindRecord,
	"write-receipt": wire.KindWriteReceipt, "stratum-profile": wire.KindStratumProfile,
	"protection-request": wire.KindProtectionRequest, "protection-receipt": wire.KindProtectionReceipt,
	"export-request": wire.KindExportRequest, "export-result": wire.KindExportResult,
	"body-state-request": wire.KindBodyStateRequest,
}

// TestShapeMatchesReferenceVerdicts replays conformance/shape-verdicts.json.gz:
// documents derived from the vendored fixtures and vectors, with the verdict
// the contract's reference shape validator (goneat, through the vendored
// shape_suite) gave each. Regenerate it with scripts/gen-shape-corpus.py on a
// contract pin bump; CI never runs goneat.
func TestShapeMatchesReferenceVerdicts(t *testing.T) {
	f, err := os.Open(filepath.Join(repoRoot(t), "conformance", "shape-verdicts.json.gz"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Schema string `json:"schema"`
		Raw    string `json:"raw"`
		Valid  bool   `json:"valid"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) < 5000 {
		t.Fatalf("corpus has %d cases, expected at least 5000", len(cases))
	}
	valid, mismatches := 0, 0
	for _, c := range cases {
		stem := strings.TrimSuffix(c.Schema[strings.Index(c.Schema, "/")+1:], ".schema.json")
		kind, ok := schemaKind[stem]
		if !ok {
			t.Fatalf("unknown schema %q", c.Schema)
		}
		n, err := wire.Ingest([]byte(c.Raw), kind)
		if err != nil {
			t.Fatalf("corpus document refused before shape: %v", err)
		}
		err = shape.Validate(c.Schema, n)
		var r *wire.Reject
		if err != nil && (!errors.As(err, &r) || r.Reason != shape.ReasonShape) {
			t.Fatalf("%s: non-shape error %v", c.Schema, err)
		}
		if c.Valid {
			valid++
		}
		if (err == nil) != c.Valid {
			mismatches++
			if mismatches <= 10 {
				t.Errorf("%s: recordprims valid=%v, reference valid=%v (%v)", c.Schema, err == nil, c.Valid, err)
			}
		}
	}
	if valid == 0 || valid == len(cases) {
		t.Fatalf("corpus is one-sided: %d valid of %d", valid, len(cases))
	}
	t.Logf("%d cases (%d valid), %d mismatches", len(cases), valid, mismatches)
}
