//go:build recordprims_conformance

// Package conformance drives the vendored durable-record/v1 vectors through
// recordprims' implementation and keeps a ledger of which vectors the
// implemented stages already decide.
//
// The ledger (conformance/v1-ledger.json) lists every vector as "pass" or
// "pending:<stage>". A vector is never skipped silently: one the implemented
// stages contradict fails the test, a "pass" can never regress, and a pending
// vector that starts passing fails the test until the ledger is updated
// (RECORDPRIMS_UPDATE_LEDGER=1), so the ledger is always exact.
//
// The driver reads the vendored contract, which the standalone Go module does
// not carry, so it builds only under the recordprims_conformance tag and runs
// from `make conformance` (part of `make check`).
package conformance

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/3leaps/recordprims/internal/wire"
)

const (
	statusPass             = "pass"
	statusPendingSemantics = "pending:semantics"
)

func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

// stageResult runs the implemented stages (ingest, numeric_domain) over a
// vector's raw inputs in the reference interpreter's order: profile, journal
// lines, then raw step fields; the first refusal decides. A source journal
// that fails these stages is refused as "continuation". It returns "" when
// every raw input passes.
func stageResult(v *wire.Node) (string, error) {
	first := func(raw string, kind string) string {
		_, err := wire.Ingest([]byte(raw), kind)
		var r *wire.Reject
		if errors.As(err, &r) {
			return r.Reason
		}
		return ""
	}
	if p := v.Get("raw_profile"); p != nil {
		if r := first(p.Str, wire.KindStratumProfile); r != "" {
			return r, nil
		}
	}
	if j := v.Get("raw_journal"); j != nil {
		for _, line := range j.Elems {
			if r := first(line.Str, wire.KindJournalEntry); r != "" {
				return r, nil
			}
		}
	}
	if steps := v.Get("steps"); steps != nil {
		for _, s := range steps.Elems {
			for field, kind := range map[string]string{
				"raw_request":    wire.KindProtectionRequest,
				"raw_entry":      wire.KindJournalEntry,
				"raw_body_state": wire.KindBodyStateRequest,
				"raw_use":        wire.KindExportRequest,
			} {
				if raw := s.Get(field); raw != nil {
					if r := first(raw.Str, kind); r != "" {
						return r, nil
					}
				}
			}
			if s.Get("raw_receipt") != nil {
				return "", fmt.Errorf("raw_receipt steps are not driven yet")
			}
		}
	}
	if src := v.Get("raw_source_journal"); src != nil {
		for _, line := range src.Elems {
			if r := first(line.Str, wire.KindJournalEntry); r != "" {
				return "continuation", nil
			}
		}
	}
	return "", nil
}

func TestV1Vectors(t *testing.T) {
	root := repoRoot(t)
	files, err := filepath.Glob(filepath.Join(root, "contract", "crucible", "schemas", "durable-record", "v1", "vectors", "*.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no vectors found: %v", err)
	}
	ledgerPath := filepath.Join(root, "conformance", "v1-ledger.json")
	ledger := map[string]string{}
	if b, err := os.ReadFile(ledgerPath); err == nil {
		if err := json.Unmarshal(b, &ledger); err != nil {
			t.Fatalf("ledger: %v", err)
		}
	}
	got := map[string]string{}
	for _, f := range files {
		name := strings.TrimSuffix(filepath.Base(f), ".json")
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		v, err := wire.ParseJSON(raw)
		if err != nil {
			t.Fatalf("%s: vector file is not strict JSON: %v", name, err)
		}
		want := ""
		if exp := v.Get("expected"); exp != nil && exp.Get("refusal") != nil {
			want = exp.Get("refusal").Str
		}
		stage, err := stageResult(v)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		earlyRefusal := want == wire.ReasonIngest || want == wire.ReasonNumericDomain ||
			(want == "continuation" && v.Get("raw_source_journal") != nil && stage == "continuation")
		switch {
		case stage != "" && stage != want:
			t.Errorf("%s: implemented stages refuse %q, vector expects %q", name, stage, orAccept(want))
		case stage != "" && earlyRefusal:
			got[name] = statusPass
		case stage == "" && hasRaw(v) && (want == wire.ReasonIngest || want == wire.ReasonNumericDomain):
			// A raw-input vector expecting an early refusal must get it here.
			// Typed and semantic numeric_domain refusals (no raw input) are
			// later stages and stay pending.
			t.Errorf("%s: vector expects %q, implemented stages accept", name, want)
		default:
			got[name] = statusPendingSemantics
		}
	}
	if os.Getenv("RECORDPRIMS_UPDATE_LEDGER") == "1" {
		writeLedger(t, ledgerPath, got)
		return
	}
	names := make([]string, 0, len(got))
	for n := range got {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if ledger[n] != got[n] {
			t.Errorf("%s: ledger says %q, run gives %q (set RECORDPRIMS_UPDATE_LEDGER=1 if this is progress)", n, ledger[n], got[n])
		}
	}
	for n := range ledger {
		if _, ok := got[n]; !ok {
			t.Errorf("%s: in the ledger but not among the vectors", n)
		}
	}
	pass := 0
	for _, s := range got {
		if s == statusPass {
			pass++
		}
	}
	t.Logf("%d vectors: %d pass, %d pending", len(got), pass, len(got)-pass)
}

func hasRaw(v *wire.Node) bool {
	for _, k := range []string{"raw_profile", "raw_journal", "raw_source_journal"} {
		if v.Get(k) != nil {
			return true
		}
	}
	if steps := v.Get("steps"); steps != nil {
		for _, s := range steps.Elems {
			for _, k := range []string{"raw_request", "raw_entry", "raw_body_state", "raw_use", "raw_receipt"} {
				if s.Get(k) != nil {
					return true
				}
			}
		}
	}
	return false
}

func orAccept(s string) string {
	if s == "" {
		return "acceptance"
	}
	return s
}

func writeLedger(t *testing.T, path string, got map[string]string) {
	t.Helper()
	b, err := json.MarshalIndent(got, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("ledger written: %s", path)
}
