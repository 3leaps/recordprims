package projectwriter

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/3leaps/recordprims"
	"github.com/3leaps/recordprims/internal/shape"
	"github.com/3leaps/recordprims/internal/wire"
)

func validate(t *testing.T, name string, schema []byte, doc []byte) error {
	t.Helper()
	inst, err := wire.ParseJSON(doc)
	if err != nil {
		t.Fatalf("%s: parse: %v", name, err)
	}
	return shape.ValidateDocument(name, schema, inst)
}

// A stored record is checked starting from its own payload_contract: the
// payload against the wrapper schema that token names, then the nested entry
// against the schema its descriptor pins.
func TestEmittedRecordValidatesFromItsOuterContract(t *testing.T) {
	f := newFixture(t, "168h")
	w := f.open(t, "rowan")
	res := mustRecord(t, w, decisionRequest(t, "dec-wrap", "op-1"), OutcomeAccepted)

	c, err := w.serve(ctx, w.streams["decision"])
	if err != nil {
		t.Fatal(err)
	}
	raw, ok := c.records[*res.Pin]
	if !ok {
		t.Fatal("the accepted record was not served")
	}
	var rec struct {
		PayloadContract string          `json:"payload_contract"`
		Payload         json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatal(err)
	}
	reg := w.Registry()
	if rec.PayloadContract != RecordPayloadContract || reg.Record.PayloadContract != rec.PayloadContract {
		t.Fatalf("payload_contract = %q, registry record contract = %q", rec.PayloadContract, reg.Record.PayloadContract)
	}
	if err := validate(t, "record", reg.Record.SchemaJSON(), rec.Payload); err != nil {
		t.Fatalf("payload does not conform to %s: %v", rec.PayloadContract, err)
	}

	var payload struct {
		Descriptor    DescriptorRef   `json:"descriptor"`
		EntryContract string          `json:"entry_contract"`
		Entry         json.RawMessage `json:"entry"`
	}
	if err := json.Unmarshal(rec.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	d, ok := reg.Lookup(payload.Descriptor.Kind, payload.Descriptor.Revision, payload.Descriptor.Digest)
	if !ok || d.EntryContract != payload.EntryContract || payload.EntryContract != ContractDecision {
		t.Fatalf("descriptor %+v (found %v), entry_contract %q", payload.Descriptor, ok, payload.EntryContract)
	}
	if err := validate(t, "entry", d.SchemaJSON(), payload.Entry); err != nil {
		t.Fatalf("entry does not conform to %s: %v", d.EntryContract, err)
	}
	// The entry schema describes the entry only, not the stored payload.
	if err := validate(t, "entry-schema-on-payload", d.SchemaJSON(), rec.Payload); err == nil {
		t.Fatal("the entry schema admits the whole payload")
	}
}

// Lookup gives one opaque outcome for every pin it does not serve, and the
// serving path delivers nothing for any of them.
func TestLookupGivesOneOutcomeForEveryUnservedPin(t *testing.T) {
	f := newFixture(t, "168h")
	w := f.open(t, "rowan")
	held := mustRecord(t, w, decisionRequest(t, "dec-held", "op-1"), OutcomeAccepted)
	retired := mustRecord(t, w, decisionRequest(t, "dec-retired", "op-2"), OutcomeAccepted)
	served := mustRecord(t, w, decisionRequest(t, "dec-served", "op-3"), OutcomeAccepted)

	f.clk.advance(time.Second)
	hr, err := w.Hold(ctx, HoldRequest{Pin: *held.Pin, OperationKey: "op-hold", EvidenceRef: "case-1",
		EvidenceDigest: "sha256:" + strings.Repeat("e", 64), ObservedAt: "2026-11-23T12:00:00Z"})
	if err != nil || hr.Outcome != "tombstoned" {
		t.Fatalf("hold = %+v, %v", hr, err)
	}
	f.clk.advance(time.Second)
	if _, err := w.streams["decision"].Retire(ctx, "dec-retired", recordprims.RetireWithdrawn); err != nil {
		t.Fatal(err)
	}

	absent := *served.Pin
	absent.RecordID = "dec-never-written"
	wrongDigest := *served.Pin
	wrongDigest.Digest = "sha256:" + strings.Repeat("0", 64)
	laterRevision := *served.Pin
	laterRevision.Revision = 2
	otherStream := *served.Pin
	otherStream.Stream = "harbor-demo.elsewhere"

	cases := map[string]recordprims.Pin{
		"held": *held.Pin, "retired": *retired.Pin, "absent": absent,
		"wrong digest": wrongDigest, "later revision": laterRevision, "other stream": otherStream,
	}
	var text string
	var cut uint64
	for name, pin := range cases {
		e, err := w.Lookup(ctx, pin)
		if !errors.Is(err, ErrNotServed) || !reflect.DeepEqual(e, Entry{}) {
			t.Fatalf("%s: Lookup = %+v, %v", name, e, err)
		}
		if text == "" {
			text = err.Error()
		} else if err.Error() != text {
			t.Fatalf("%s: error text %q differs from %q", name, err.Error(), text)
		}
		if page, err := w.Render(ctx, pin); page != "" || !errors.Is(err, ErrNotServed) || err.Error() != text {
			t.Fatalf("%s: Render = %q, %v", name, page, err)
		}
		if pin.Stream != w.project.Kinds["decision"].Stream {
			continue
		}
		c := &collect{records: map[recordprims.Pin][]byte{}}
		res, err := w.streams["decision"].Release(ctx, recordprims.UseRequest{
			Purpose: recordprims.PurposeServe, Closure: recordprims.ClosureMetadataOnly,
			Selection: []recordprims.Pin{pin},
		}, c)
		if err != nil || res.Outcome != recordprims.ReleaseRefused || len(c.order) != 0 {
			t.Fatalf("%s: release = %+v, %v, %d frames", name, res, err, len(c.order))
		}
		if cut == 0 {
			cut = res.Cut
		} else if res.Cut != cut {
			t.Fatalf("%s: cut %d differs from %d", name, res.Cut, cut)
		}
	}
	if text != "not_served" {
		t.Fatalf("error text = %q", text)
	}

	if e, err := w.Lookup(ctx, *served.Pin); err != nil || e.Pin != *served.Pin {
		t.Fatalf("served pin: %+v, %v", e.Pin, err)
	}
}

// After a correction, each exact pin reads its own revision.
func TestLookupReadsEachRevisionAfterACorrection(t *testing.T) {
	f := newFixture(t, "168h")
	w := f.open(t, "rowan")
	orig := mustRecord(t, w, decisionRequest(t, "dec-fix", "op-1"), OutcomeAccepted)
	fix := decisionRequest(t, "dec-fix", "op-2")
	fix.Predecessor = orig.Pin
	fix.Entry = json.RawMessage(strings.Replace(string(fix.Entry), "October trial", "October 2026 trial", 1))
	corrected := mustRecord(t, w, fix, OutcomeAccepted)

	first, err := w.Lookup(ctx, *orig.Pin)
	if err != nil || first.Pin != *orig.Pin || first.Supersedes != nil || strings.Contains(string(first.Entry), "October 2026 trial") {
		t.Fatalf("original = %+v, %v", first, err)
	}
	second, err := w.Lookup(ctx, *corrected.Pin)
	if err != nil || second.Pin != *corrected.Pin || second.Supersedes == nil || *second.Supersedes != *orig.Pin ||
		!strings.Contains(string(second.Entry), "October 2026 trial") {
		t.Fatalf("correction = %+v, %v", second, err)
	}
	if first.EntryContract != ContractDecision || second.PayloadContract != RecordPayloadContract {
		t.Fatalf("contracts = %q, %q", first.EntryContract, second.PayloadContract)
	}
}

// A wrapper schema that differs from its pin refuses the whole registry.
func TestTamperedRecordSchemaRefusesTheRegistry(t *testing.T) {
	f := newFixture(t, "168h")
	path := filepath.Join(f.root, "registry", "record", "schema.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(raw, ' '), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(f.project, f.opts("rowan")); err == nil || !strings.Contains(err.Error(), "record schema") {
		t.Fatalf("Open = %v", err)
	}
}

// repinDecision rewrites the decision descriptor in the fixture's registry,
// re-pins the registry in the project config, and opens the project.
func repinDecision(t *testing.T, f *fixture, owner string, revision float64) (*Writer, error) {
	t.Helper()
	regPath := filepath.Join(f.root, "registry", "registry.json")
	raw, err := os.ReadFile(regPath)
	if err != nil {
		t.Fatal(err)
	}
	var reg map[string]any
	if err := json.Unmarshal(raw, &reg); err != nil {
		t.Fatal(err)
	}
	for _, d := range reg["descriptors"].([]any) {
		if dm := d.(map[string]any); dm["kind"] == "decision" {
			dm["owner"], dm["descriptor_revision"] = owner, revision
		}
	}
	out, _ := json.MarshalIndent(reg, "", "  ")
	if err := os.WriteFile(regPath, out, 0o600); err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	cfgRaw, _ := os.ReadFile(f.project)
	json.Unmarshal(cfgRaw, &cfg)
	cfg["registry"].(map[string]any)["digest"] = fileDigest(out)
	f.writeConfig(t, cfg)
	w, err := Open(f.project, f.opts("rowan"))
	if err == nil {
		t.Cleanup(func() { w.Close() })
	}
	return w, err
}

// The registry admits exactly the descriptor owners and revisions the record
// wrapper schema admits: at the bound the emitted payload conforms, one past
// it the registry is refused, so no accepted record can fail its wrapper.
func TestDescriptorBoundsMatchTheWrapperSchema(t *testing.T) {
	long := strings.Repeat("a", 127)
	for _, c := range []struct {
		name     string
		owner    string
		revision float64
		ok       bool
	}{
		{"owner components at 127", long + "/" + long, 1, true},
		{"revision at 2^53-1", "example.org/project-records", 1<<53 - 1, true},
		{"first owner component at 128", long + "a/x", 1, false},
		{"second owner component at 128", "x/" + long + "a", 1, false},
		{"revision past 2^53-1", "example.org/project-records", 1 << 53, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t, "168h")
			w, err := repinDecision(t, f, c.owner, c.revision)
			if !c.ok {
				if err == nil || !strings.Contains(err.Error(), "malformed") {
					t.Fatalf("registry opened: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			res := mustRecord(t, w, decisionRequest(t, "dec-bound", "op-bound"), OutcomeAccepted)
			cs, err := w.serve(ctx, w.streams["decision"])
			if err != nil {
				t.Fatal(err)
			}
			var rec struct {
				Payload json.RawMessage `json:"payload"`
			}
			if err := json.Unmarshal(cs.records[*res.Pin], &rec); err != nil {
				t.Fatal(err)
			}
			if err := validate(t, "record", w.Registry().Record.SchemaJSON(), rec.Payload); err != nil {
				t.Fatalf("payload at the bound fails the wrapper schema: %v", err)
			}
		})
	}
}
