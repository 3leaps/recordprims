//go:build recordprims_conformance

package conformance

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/3leaps/recordprims/internal/shape"
	"github.com/3leaps/recordprims/internal/wire"
)

// The schemas internal/shape embeds are byte-identical copies of the
// vendored contract, and the two sets are equal.
func TestEmbeddedSchemasMatchContract(t *testing.T) {
	root := repoRoot(t)
	for _, fam := range []string{"v0", "v1"} {
		emb, _ := filepath.Glob(filepath.Join(root, "internal", "shape", "schemas", fam, "*.schema.json"))
		ven, _ := filepath.Glob(filepath.Join(root, "contract", "crucible", "schemas", "durable-record", fam, "*.schema.json"))
		names := func(fs []string) []string {
			out := make([]string, len(fs))
			for i, f := range fs {
				out[i] = filepath.Base(f)
			}
			sort.Strings(out)
			return out
		}
		if a, b := strings.Join(names(emb), ","), strings.Join(names(ven), ","); a != b || a == "" {
			t.Fatalf("%s: embedded %s, vendored %s", fam, a, b)
		}
		for _, f := range ven {
			want, _ := os.ReadFile(f)
			got, err := os.ReadFile(filepath.Join(root, "internal", "shape", "schemas", fam, filepath.Base(f)))
			if err != nil || !bytes.Equal(got, want) {
				t.Errorf("%s/%s: embedded copy differs from the vendored contract", fam, filepath.Base(f))
			}
		}
	}
}

// Every vector's destination documents get the shape verdict the vector
// declares: a journal entry is shape-valid exactly when it is not listed in
// invalid_journal_entries, and the profile is shape-valid. This is the
// reference interpreter's own stage 3 check (check_case_stage3_shapes).
func TestVectorShapes(t *testing.T) {
	files, _ := filepath.Glob(filepath.Join(repoRoot(t), "contract", "crucible", "schemas", "durable-record", "v1", "vectors", "*.json"))
	checked := 0
	for _, f := range files {
		name := strings.TrimSuffix(filepath.Base(f), ".json")
		raw, _ := os.ReadFile(f)
		v, err := wire.ParseJSON(raw)
		if err != nil {
			t.Fatal(err)
		}
		invalid := map[int]bool{}
		if l := v.Get("invalid_journal_entries"); l != nil {
			for _, e := range l.Elems {
				x, _ := wire.SafeInt(e.Token)
				invalid[int(x)] = true
			}
		}
		invalidReceipts := map[int]bool{}
		if l := v.Get("invalid_receipt_steps"); l != nil {
			for _, e := range l.Elems {
				x, _ := wire.SafeInt(e.Token)
				invalidReceipts[int(x)] = true
			}
		}
		invalidRequests := map[int]bool{}
		if l := v.Get("invalid_request_steps"); l != nil {
			for _, e := range l.Elems {
				x, _ := wire.SafeInt(e.Token)
				invalidRequests[int(x)] = true
			}
		}
		invalidStates := map[int]bool{}
		if l := v.Get("invalid_body_state_steps"); l != nil {
			for _, e := range l.Elems {
				x, _ := wire.SafeInt(e.Token)
				invalidStates[int(x)] = true
			}
		}
		invalidUses := map[int]bool{}
		if l := v.Get("invalid_use_steps"); l != nil {
			for _, e := range l.Elems {
				x, _ := wire.SafeInt(e.Token)
				invalidUses[int(x)] = true
			}
		}
		invalidRemovals := map[int]bool{}
		if l := v.Get("invalid_removal_steps"); l != nil {
			for _, e := range l.Elems {
				x, _ := wire.SafeInt(e.Token)
				invalidRemovals[int(x)] = true
			}
		}
		invalidSeals := map[int]bool{}
		if l := v.Get("invalid_seal_steps"); l != nil {
			for _, e := range l.Elems {
				x, _ := wire.SafeInt(e.Token)
				invalidSeals[int(x)] = true
			}
		}
		check := func(doc *wire.Node, schema string, want bool, where string) {
			checked++
			err := shape.Validate(schema, doc)
			var r *wire.Reject
			if err != nil && (!errors.As(err, &r) || r.Reason != shape.ReasonShape) {
				t.Errorf("%s %s: %v", name, where, err)
				return
			}
			if (err == nil) != want {
				t.Errorf("%s %s: shape valid=%v, vector declares %v (%v)", name, where, err == nil, want, err)
			}
		}
		// Documents given as text pass stages 1 and 2 first; a refusal there
		// ends the vector, as in the reference.
		ingest := func(text, kind string) *wire.Node {
			n, err := wire.Ingest([]byte(text), kind)
			if err != nil {
				return nil
			}
			return n
		}
		if p := v.Get("raw_profile"); p != nil {
			n := ingest(p.Str, wire.KindStratumProfile)
			if n == nil {
				continue
			}
			check(n, "v1/stratum-profile.schema.json", true, "raw_profile")
		} else if p := v.Get("profile"); p != nil {
			check(p, "v1/stratum-profile.schema.json", true, "profile")
		}
		if cs := v.Get("candidate_streams"); cs != nil {
			for _, ce := range cs.Elems {
				check(ce, "v1/journal-entry.schema.json", true, "candidate_streams entry")
			}
		}
		if ss := v.Get("sharing_streams"); ss != nil {
			for _, sj := range ss.Elems {
				for _, se := range sj.Elems {
					check(se, "v1/journal-entry.schema.json", true, "sharing_streams entry")
				}
			}
		}
		if nd := v.Get("raw_journal_ndjson"); nd != nil {
			entries, err := wire.IngestLines([]byte(nd.Str), wire.KindJournalEntry)
			if err != nil {
				continue
			}
			for i, n := range entries {
				check(n, "v1/journal-entry.schema.json", !invalid[i], "raw_journal_ndjson entry")
			}
		} else if j := v.Get("raw_journal"); j != nil {
			aborted := false
			for i, line := range j.Elems {
				n := ingest(line.Str, wire.KindJournalEntry)
				if n == nil {
					aborted = true
					break
				}
				check(n, "v1/journal-entry.schema.json", !invalid[i], "raw_journal entry")
			}
			if aborted {
				continue
			}
		} else if j := v.Get("journal"); j != nil {
			for i, e := range j.Elems {
				check(e, "v1/journal-entry.schema.json", !invalid[i], "journal entry")
			}
		}
		if steps := v.Get("steps"); steps != nil {
			for i, s := range steps.Elems {
				switch {
				case s.Get("raw_request") != nil:
					n := ingest(s.Get("raw_request").Str, wire.KindProtectionRequest)
					if n == nil {
						goto nextVector
					}
					check(n, "v1/protection-request.schema.json", !invalidRequests[i], "raw_request step")
				case s.Get("request") != nil:
					check(s.Get("request"), "v1/protection-request.schema.json", !invalidRequests[i], "request step")
				case s.Get("raw_receipt") != nil:
					n, err := wire.IngestReceipt([]byte(s.Get("raw_receipt").Str))
					if err != nil {
						goto nextVector
					}
					check(n, receiptSchema(n), !invalidReceipts[i], "raw_receipt step")
				case s.Get("receipt") != nil:
					r := s.Get("receipt")
					check(r, receiptSchema(r), !invalidReceipts[i], "receipt step")
				case s.Get("raw_entry") != nil:
					n := ingest(s.Get("raw_entry").Str, wire.KindJournalEntry)
					if n == nil {
						goto nextVector
					}
					check(n, "v1/journal-entry.schema.json", true, "raw_entry step")
				case s.Get("entry") != nil:
					check(s.Get("entry"), "v1/journal-entry.schema.json", true, "entry step")
				case s.Get("raw_body_state") != nil:
					n := ingest(s.Get("raw_body_state").Str, wire.KindBodyStateRequest)
					if n == nil {
						goto nextVector
					}
					check(n, "v1/body-state-request.schema.json", !invalidStates[i], "raw_body_state step")
				case s.Get("body_state") != nil:
					check(s.Get("body_state"), "v1/body-state-request.schema.json", !invalidStates[i], "body_state step")
				case s.Get("raw_use") != nil:
					n := ingest(s.Get("raw_use").Str, wire.KindExportRequest)
					if n == nil {
						goto nextVector
					}
					check(n, "v1/export-request.schema.json", !invalidUses[i], "raw_use step")
				case s.Get("use") != nil:
					check(s.Get("use"), "v1/export-request.schema.json", !invalidUses[i], "use step")
				case s.Get("raw_removal_eligibility") != nil:
					n := ingest(s.Get("raw_removal_eligibility").Str, wire.KindRemovalEligibility)
					if n == nil {
						goto nextVector
					}
					check(n, "v1/removal-eligibility-request.schema.json", !invalidRemovals[i], "raw_removal_eligibility step")
				case s.Get("removal_eligibility") != nil:
					check(s.Get("removal_eligibility"), "v1/removal-eligibility-request.schema.json", !invalidRemovals[i], "removal_eligibility step")
				case s.Get("raw_verify_seal") != nil:
					vctx := ingest(s.Get("raw_verify_seal").Str, "seal_context")
					if vctx == nil {
						goto nextVector
					}
					if rs := vctx.Get("raw_seal"); rs != nil && rs.Kind == wire.String {
						sn := ingest(rs.Str, wire.KindStreamSeal)
						if sn == nil {
							goto nextVector
						}
						check(sn, "v1/stream-seal.schema.json", !invalidSeals[i], "raw_verify_seal.raw_seal step")
					} else if sl := vctx.Get("seal"); sl != nil {
						check(sl, "v1/stream-seal.schema.json", !invalidSeals[i], "raw_verify_seal.seal step")
					}
				case s.Get("verify_seal") != nil:
					vctx := s.Get("verify_seal")
					if rs := vctx.Get("raw_seal"); rs != nil && rs.Kind == wire.String {
						sn := ingest(rs.Str, wire.KindStreamSeal)
						if sn == nil {
							goto nextVector
						}
						check(sn, "v1/stream-seal.schema.json", !invalidSeals[i], "verify_seal.raw_seal step")
					} else if sl := vctx.Get("seal"); sl != nil {
						check(sl, "v1/stream-seal.schema.json", !invalidSeals[i], "verify_seal.seal step")
					}
				}
			}
		}
	nextVector:
	}
	if checked < 500 {
		t.Fatalf("only %d documents checked", checked)
	}
	t.Logf("%d vector documents checked", checked)
}

func receiptSchema(n *wire.Node) string {
	if wire.ReceiptKind(n) == wire.KindWriteReceipt {
		return "v1/write-receipt.schema.json"
	}
	return "v1/protection-receipt.schema.json"
}
