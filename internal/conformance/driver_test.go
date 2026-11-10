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
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/3leaps/recordprims/internal/journal"
	"github.com/3leaps/recordprims/internal/wire"
)

const (
	statusPass             = "pass"
	statusPendingSemantics = "pending:semantics"
	// statusPassPartial: the vector's refusal matches, but an earlier entry
	// needed semantics not implemented yet, so an unported check could in
	// principle have refused first. Tracked exactly, never counted as a pass.
	statusPassPartial = "pass:partial"
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
			// The reference interpreter's order (raw_request, raw_receipt,
			// raw_entry, raw_body_state, raw_use), fixed so that the first
			// refusal decides deterministically.
			for _, fk := range [...]struct{ field, kind string }{
				{"raw_request", wire.KindProtectionRequest},
				{"raw_entry", wire.KindJournalEntry},
				{"raw_body_state", wire.KindBodyStateRequest},
				{"raw_use", wire.KindExportRequest},
			} {
				if raw := s.Get(fk.field); raw != nil {
					if r := first(raw.Str, fk.kind); r != "" {
						return r, nil
					}
				}
			}
			if raw := s.Get("raw_receipt"); raw != nil {
				if _, err := wire.IngestReceipt([]byte(raw.Str)); err != nil {
					var r *wire.Reject
					if errors.As(err, &r) {
						return r.Reason, nil
					}
					return "", err
				}
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

// typed runs the wire stages over a document the vector gives as JSON (not
// as text), so its protocol integers are typed as the reference's are.
func typed(n *wire.Node, kind string) (*wire.Node, error) {
	return wire.Ingest(wire.Encode(n), kind)
}

func reasonOf(err error) string {
	var r *wire.Reject
	if errors.As(err, &r) {
		return r.Reason
	}
	return "error: " + err.Error()
}

// semanticResult replays a vector whose raw inputs passed the wire stages, in
// the reference interpreter's order (evaluate): the idempotency window, the
// profile, the journal as a whole, each entry, then the steps. It returns
// the first refusal, and whether the replay up to that point was complete: a
// vector that observes a source journal, reaches a step other than a journal
// entry, or has ingested an entry whose semantics are not implemented yet is
// incomplete, and an incomplete replay never passes a vector, since an
// unported check might have refused first (a matching refusal is recorded as
// pass:partial).
//
// One known ordering difference: a journal given as JSON (not text) is typed
// entry by entry before CheckJournal runs, while the reference checks each
// entry's numeric domain inside check_journal, interleaved with the seq and
// stream checks. A vector with an early seq_gap and a later numeric_domain
// entry would differ; none exists.
func semanticResult(v *wire.Node) (refusal string, results []*wire.Node, journalSize int, complete bool) {
	st, err := journal.New(v.Get("window_ms"))
	if err != nil {
		return reasonOf(err), nil, 0, true
	}
	var prof *wire.Node
	if p := v.Get("raw_profile"); p != nil {
		prof, err = wire.Ingest([]byte(p.Str), wire.KindStratumProfile)
	} else if p := v.Get("profile"); p != nil {
		prof, err = typed(p, wire.KindStratumProfile)
	}
	if err != nil {
		return reasonOf(err), nil, 0, true
	}
	if prof != nil {
		if err := journal.CheckProfile(prof); err != nil {
			return reasonOf(err), nil, 0, true
		}
		if rs := prof.Get("runtime").Get("body_removal_stream"); rs != nil && rs.Kind == wire.String {
			st.SetRemovalStream(rs.Str)
		}
	}
	if v.Get("source_journal") != nil || v.Get("raw_source_journal") != nil {
		return "", nil, 0, false
	}
	var entries []*wire.Node
	if j := v.Get("raw_journal"); j != nil {
		for _, line := range j.Elems {
			n, err := wire.Ingest([]byte(line.Str), wire.KindJournalEntry)
			if err != nil {
				return reasonOf(err), nil, 0, true
			}
			entries = append(entries, n)
		}
	} else if j := v.Get("journal"); j != nil {
		for _, e := range j.Elems {
			n, err := typed(e, wire.KindJournalEntry)
			if err != nil {
				return reasonOf(err), nil, 0, true
			}
			entries = append(entries, n)
		}
	}
	if err := journal.CheckJournal(entries); err != nil {
		return reasonOf(err), nil, 0, true
	}
	for _, e := range entries {
		if err := st.Ingest(e); err != nil {
			return reasonOf(err), nil, 0, !st.Partial()
		}
	}
	if steps := v.Get("steps"); steps != nil {
		for _, s := range steps.Elems {
			switch {
			case s.Get("raw_entry") != nil || s.Get("entry") != nil:
				var e *wire.Node
				if raw := s.Get("raw_entry"); raw != nil {
					e, err = wire.Ingest([]byte(raw.Str), wire.KindJournalEntry)
				} else {
					e, err = typed(s.Get("entry"), wire.KindJournalEntry)
				}
				if err != nil {
					return reasonOf(err), nil, 0, !st.Partial()
				}
				if err := st.Ingest(e); err != nil {
					return reasonOf(err), nil, 0, !st.Partial()
				}
				results = append(results, &wire.Node{
					Kind:   wire.Object,
					Fields: []wire.Member{{Name: "outcome", Value: &wire.Node{Kind: wire.String, Str: "journaled"}}},
				})
			case s.Get("raw_receipt") != nil || s.Get("receipt") != nil:
				var rcpt *wire.Node
				if raw := s.Get("raw_receipt"); raw != nil {
					rcpt, err = wire.IngestReceipt([]byte(raw.Str))
				} else {
					r := s.Get("receipt")
					rcpt, err = typed(r, wire.ReceiptKind(r))
				}
				if err != nil {
					return reasonOf(err), nil, 0, !st.Partial()
				}
				res, err := st.VerifyReceipt(rcpt)
				if err != nil {
					return reasonOf(err), nil, 0, !st.Partial()
				}
				results = append(results, res)
			case s.Get("reconcile") != nil:
				rec := s.Get("reconcile")
				res, err := st.Reconcile(
					rec.Get("key").Str,
					rec.Get("at").Str,
					rec.Get("attempted_at").Str,
				)
				if err != nil {
					return reasonOf(err), nil, 0, !st.Partial()
				}
				results = append(results, res)
			default:
				return "", nil, 0, false
			}
		}
	}
	if err := journal.CheckJournal(st.Journal()); err != nil {
		return reasonOf(err), nil, 0, !st.Partial()
	}
	return "", results, st.Len(), !st.Partial()
}

// acceptedAs reports whether a vector's expected result matches the step
// results, has no active holds, and matches the final journal size.
func acceptedAs(exp *wire.Node, results []*wire.Node, size int) bool {
	res, holds, js := exp.Get("results"), exp.Get("active_holds"), exp.Get("journal_size")
	if res == nil || holds == nil || js == nil || len(exp.Fields) != 3 || len(holds.Elems) != 0 {
		return false
	}
	if len(res.Elems) != len(results) {
		return false
	}
	for i, want := range res.Elems {
		wantTyped, err := wire.ParseJSON(wire.Encode(want))
		if err != nil {
			return false
		}
		gotTyped, err := wire.ParseJSON(wire.Encode(results[i]))
		if err != nil {
			return false
		}
		if !journal.Equal(wantTyped, gotTyped) {
			return false
		}
	}
	n, err := wire.SafeInt(js.Token)
	return err == nil && n == int64(size)
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
		if stage == "" {
			// The wire stages pass: the semantic stage decides, as far as it
			// is implemented. A refusal that matches the vector passes; one
			// that contradicts it is an error only when nothing unimplemented
			// could have refused first.
			sem, results, size, complete := semanticResult(v)
			switch {
			case strings.HasPrefix(sem, "error"):
				t.Errorf("%s: %s", name, sem)
			case !complete && sem != "" && sem == want:
				got[name] = statusPassPartial
			case !complete:
				got[name] = statusPendingSemantics
			case sem != "" && sem == want:
				got[name] = statusPass
			case sem != "":
				t.Errorf("%s: semantic stage refuses %q, vector expects %q", name, sem, orAccept(want))
			case want != "":
				t.Errorf("%s: semantic stage accepts a complete replay, vector expects %q", name, want)
			case !acceptedAs(v.Get("expected"), results, size):
				t.Errorf("%s: complete replay of %d entries does not match the expected result", name, size)
			default:
				got[name] = statusPass
			}
			continue
		}
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
		// Update mode records progress only: it never lowers a pass.
		for n, s := range ledger {
			if s == statusPass && got[n] != statusPass {
				t.Fatalf("%s: ledger says pass, run gives %q; update mode never lowers a pass", n, got[n])
			}
		}
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
	pass, partial := 0, 0
	for _, s := range got {
		switch s {
		case statusPass:
			pass++
		case statusPassPartial:
			partial++
		}
	}
	t.Logf("%d vectors: %d pass, %d pass:partial, %d pending", len(got), pass, partial, len(got)-pass-partial)
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
