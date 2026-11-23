package projectwriter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var ctx = context.Background()

type stepClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *stepClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *stepClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// fixture copies the example registry and project into a temp directory,
// with the idempotency window overridden, and initializes the store.
type fixture struct {
	root, project string
	clk           *stepClock
}

func newFixture(t *testing.T, window string) *fixture {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	copyTree(t, filepath.Join("..", "registry"), filepath.Join(root, "registry"))
	raw, err := os.ReadFile(filepath.Join("..", "project", "harbor-demo.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	cfg["idempotency_window"] = window
	f := &fixture{root: root, project: filepath.Join(root, "project", "harbor-demo.json"),
		clk: &stepClock{t: time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)}}
	f.writeConfig(t, cfg)
	if err := Init(f.project, f.opts("")); err != nil {
		t.Fatalf("Init: %v", err)
	}
	return f
}

func (f *fixture) writeConfig(t *testing.T, cfg map[string]any) {
	t.Helper()
	out, _ := json.MarshalIndent(cfg, "", "  ")
	if err := os.MkdirAll(filepath.Dir(f.project), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.project, out, 0o600); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) opts(caller string) Options {
	return Options{Caller: caller, Clock: f.clk, LockTimeout: 5 * time.Second, AllowTmpfs: true, AllowOverlay: true}
}

func (f *fixture) open(t *testing.T, caller string) *Writer {
	t.Helper()
	w, err := Open(f.project, f.opts(caller))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { w.Close() })
	return w
}

func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, 0o600)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func example(t *testing.T, rel string) json.RawMessage {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "registry", rel))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func decisionRequest(t *testing.T, id, key string) Request {
	return Request{Project: "harbor-demo", Kind: "decision", RecordID: id, OperationKey: key,
		Entry: example(t, "decision/valid-adopt-queue.json")}
}

func mustRecord(t *testing.T, w *Writer, req Request, want string) Result {
	t.Helper()
	res, err := w.Record(ctx, req)
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	if res.Outcome != want {
		t.Fatalf("Record outcome = %s (%s: %s), want %s", res.Outcome, res.Reason, res.Detail, want)
	}
	return res
}

// E4: discover, inspect, validate, write, reopen and resolve the same revision.
func TestCleanProjectWalkThrough(t *testing.T) {
	f := newFixture(t, "168h")
	w := f.open(t, "rowan")

	kinds := w.Kinds()
	var names []string
	for _, k := range kinds {
		names = append(names, k.Kind+":"+string(k.Authority))
	}
	if got := strings.Join(names, ","); got != "decision:recordprims,project-update:recordprims,risk:recordprims,runbook-decision:git" {
		t.Fatalf("kinds = %s", got)
	}
	info, ok := w.Inspect("decision")
	if !ok || len(info.Example) == 0 || !info.Trusted || info.DescriptorRevision != 1 {
		t.Fatalf("inspect = %+v", info)
	}
	req := decisionRequest(t, "dec-intake-queue", "op-1")
	req.Entry = info.Example
	if v := w.Validate(req); v.Outcome != "valid" {
		t.Fatalf("validate = %+v", v)
	}
	res := mustRecord(t, w, req, OutcomeAccepted)
	if res.Pin == nil || res.Pin.Revision != 1 || res.Authority != AuthorityStore || res.Receipt == nil {
		t.Fatalf("result = %+v", res)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	r := f.open(t, "")
	e, err := r.Lookup(ctx, *res.Pin)
	if err != nil {
		t.Fatal(err)
	}
	if e.Project != "harbor-demo" || e.Kind != "decision" || e.Descriptor.Revision != 1 || !e.DescriptorFound {
		t.Fatalf("entry = %+v", e)
	}
	var d Decision
	if err := json.Unmarshal(e.Entry, &d); err != nil || d.Title != "Use one intake queue for berth requests" {
		t.Fatalf("entry content = %s, %v", e.Entry, err)
	}
	page, err := r.Render(ctx, *res.Pin)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(page, "# Decision: Use one intake queue") || !strings.Contains(page, res.Pin.Digest) {
		t.Fatalf("page does not carry the title and pin:\n%s", page)
	}
	hist, err := r.History("decision", "dec-intake-queue")
	if err != nil || len(hist) != 1 || hist[0].Pin != *res.Pin {
		t.Fatalf("history = %+v, %v", hist, err)
	}
}

// E5: lost reply, key reuse, the reconciliation window and concurrent revisions.
func TestLostReplyConflictAndConcurrency(t *testing.T) {
	f := newFixture(t, "1h")
	w := f.open(t, "status-bot")
	upd := Request{Project: "harbor-demo", Kind: "project-update", RecordID: "update-2026-11-23",
		OperationKey: "op-upd-1", Entry: example(t, "project-update/valid-weekly.json")}
	sentAt := f.clk.Now()
	first := mustRecord(t, w, upd, OutcomeAccepted)

	// The reply is lost. Retrying the same request with the same key returns
	// the original receipt and appends nothing.
	f.clk.advance(time.Minute)
	retry := mustRecord(t, w, upd, OutcomeAccepted)
	if *retry.Pin != *first.Pin || retry.Receipt.Seq != first.Receipt.Seq {
		t.Fatalf("retry = %+v, want the original %+v", retry.Receipt, first.Receipt)
	}
	rec, err := w.Reconcile("project-update", "op-upd-1", sentAt)
	if err != nil || rec.Outcome != OutcomeAccepted || *rec.Pin != *first.Pin {
		t.Fatalf("reconcile inside the window = %+v, %v", rec, err)
	}
	if h, _ := w.History("project-update", "update-2026-11-23"); len(h) != 1 {
		t.Fatalf("history after retry has %d revisions", len(h))
	}

	// The same key with different content is a conflict.
	other := upd
	other.Entry = json.RawMessage(strings.Replace(string(upd.Entry), "on_track", "at_risk", 1))
	if res := mustRecord(t, w, other, OutcomeConflict); res.Reason != ReasonIdempotencyConflict || res.Next == "" {
		t.Fatalf("key reuse = %+v", res)
	}

	// Outside the window, acceptance cannot be established.
	f.clk.advance(2 * time.Hour)
	rec, err = w.Reconcile("project-update", "op-upd-1", sentAt)
	if err != nil || rec.Outcome != OutcomeIndeterminate || rec.Reason != ReasonUnprovable || rec.Next == "" {
		t.Fatalf("reconcile outside the window = %+v, %v", rec, err)
	}

	// Two writers revise the same predecessor at once: one wins.
	a, b := f.open(t, "status-bot"), f.open(t, "status-bot")
	rev := func(w *Writer, key, status string) Request {
		r := upd
		r.OperationKey = key
		r.Predecessor = first.Pin
		r.Entry = json.RawMessage(strings.Replace(string(upd.Entry), "on_track", status, 1))
		return r
	}
	var wg sync.WaitGroup
	results := make([]Result, 2)
	for i, c := range []struct {
		w      *Writer
		key, s string
	}{{a, "op-rev-a", "at_risk"}, {b, "op-rev-b", "blocked"}} {
		wg.Add(1)
		go func(i int, w *Writer, key, s string) {
			defer wg.Done()
			res, err := w.Record(ctx, rev(w, key, s))
			if err != nil {
				t.Error(err)
			}
			results[i] = res
		}(i, c.w, c.key, c.s)
	}
	wg.Wait()
	var won, lost *Result
	for i := range results {
		switch results[i].Outcome {
		case OutcomeAccepted:
			won = &results[i]
		case OutcomeConflict:
			lost = &results[i]
		}
	}
	if won == nil || lost == nil || lost.Reason != ReasonRevisionConflict || lost.Current == nil || *lost.Current != *won.Pin {
		t.Fatalf("concurrent revisions = %+v", results)
	}
	if won.Pin.Revision != 2 {
		t.Fatalf("winner = %+v", won.Pin)
	}
}

// E6: refusals, a registry change and holds on the read path.
func TestRefusalsRegistryChangeAndHolds(t *testing.T) {
	f := newFixture(t, "168h")
	w := f.open(t, "rowan")
	refused := func(req Request, reason string) {
		t.Helper()
		res, err := w.Record(ctx, req)
		if err != nil || res.Outcome != OutcomeRefused || res.Reason != reason {
			t.Fatalf("got %+v, %v; want refused %s", res, err, reason)
		}
	}
	req := decisionRequest(t, "dec-1", "op-1")
	unknown := req
	unknown.Kind = "incident"
	refused(unknown, ReasonUnknownKind)
	bad := req
	bad.Entry = example(t, "decision/refused-missing-rationale.json")
	refused(bad, ReasonInvalidPayload)
	both := req
	both.Entry = example(t, "decision/refused-evidence-and-absence.json")
	refused(both, ReasonInvalidPayload)
	low := req
	low.Classification = RecordClassification{Sensitivity: "0-public"}
	refused(low, ReasonBelowFloor)
	git := req
	git.Kind = "runbook-decision"
	refused(git, ReasonGitAuthority)
	other := req
	other.Project = "another-project"
	refused(other, ReasonWrongProject)

	vendor := f.open(t, "vendor-liaison")
	if res, _ := vendor.Record(ctx, req); res.Reason != ReasonUnauthorizedWriter {
		t.Fatalf("external writer = %+v", res)
	}
	stranger := f.open(t, "nobody")
	if res, _ := stranger.Record(ctx, req); res.Reason != ReasonUnauthorizedWriter {
		t.Fatalf("unknown caller = %+v", res)
	}
	bot := f.open(t, "status-bot")
	if res, _ := bot.Record(ctx, req); res.Reason != ReasonUnauthorizedWriter {
		t.Fatalf("agent writing a maintainer kind = %+v", res)
	}

	// A producer-time spelling in the entry is normalized before the digest.
	lenient := req
	lenient.Entry = example(t, "decision/refused-lenient-instant.json")
	first := mustRecord(t, w, lenient, OutcomeAccepted)
	e, err := w.Lookup(ctx, *first.Pin)
	if err != nil || !strings.Contains(string(e.Entry), `"decided_at":"2026-11-20T15:00:00.000000Z"`) {
		t.Fatalf("normalized entry = %s, %v", e.Entry, err)
	}
	w.Close()

	// A tampered schema makes its descriptor untrusted; a tampered registry
	// is refused as a whole.
	schema := filepath.Join(f.root, "registry", "risk", "schema.json")
	orig, _ := os.ReadFile(schema)
	os.WriteFile(schema, append(orig, ' '), 0o600)
	t2 := f.open(t, "rowan")
	risk := Request{Project: "harbor-demo", Kind: "risk", RecordID: "risk-1", OperationKey: "op-r",
		Entry: example(t, "risk/valid-supplier.json")}
	if res, _ := t2.Record(ctx, risk); res.Reason != ReasonUntrustedDescriptor {
		t.Fatalf("tampered schema = %+v", res)
	}
	t2.Close()
	os.WriteFile(schema, orig, 0o600)
	regPath := filepath.Join(f.root, "registry", "registry.json")
	regRaw, _ := os.ReadFile(regPath)
	os.WriteFile(regPath, append(regRaw, '\n'), 0o600)
	if _, err := Open(f.project, f.opts("rowan")); err == nil || !strings.Contains(err.Error(), "untrusted registry") {
		t.Fatalf("tampered registry opened: %v", err)
	}

	// A changed schema that is correctly pinned is still refused: this
	// build's checks implement only the schema revisions they name.
	changed := append(orig[:len(orig):len(orig)], '\n')
	os.WriteFile(schema, changed, 0o600)
	var regDoc map[string]any
	json.Unmarshal(regRaw, &regDoc)
	for _, d := range regDoc["descriptors"].([]any) {
		if dm := d.(map[string]any); dm["kind"] == "risk" {
			dm["schema"].(map[string]any)["digest"] = fileDigest(changed)
		}
	}
	repinned, _ := json.MarshalIndent(regDoc, "", "  ")
	os.WriteFile(regPath, repinned, 0o600)
	var pinCfg map[string]any
	pinRaw, _ := os.ReadFile(f.project)
	json.Unmarshal(pinRaw, &pinCfg)
	pinCfg["registry"].(map[string]any)["digest"] = fileDigest(repinned)
	f.writeConfig(t, pinCfg)
	t4 := f.open(t, "rowan")
	if res, _ := t4.Record(ctx, risk); res.Reason != ReasonUnsupportedSchema {
		t.Fatalf("changed, pinned schema = %+v", res)
	}
	t4.Close()
	os.WriteFile(schema, orig, 0o600)

	// Change the registry: decision gains descriptor revision 2 with a higher
	// floor. The project re-pins the registry. The original revision still
	// resolves under descriptor revision 1.
	var reg map[string]any
	json.Unmarshal(regRaw, &reg)
	descs := reg["descriptors"].([]any)
	d2 := map[string]any{}
	for k, v := range descs[0].(map[string]any) {
		d2[k] = v
	}
	d2["descriptor_revision"] = 2
	d2["classification"] = map[string]any{"sensitivity": "3-proprietary", "retention-lifecycle": "long"}
	reg["descriptors"] = append(descs, d2)
	newReg, _ := json.MarshalIndent(reg, "", "  ")
	os.WriteFile(regPath, newReg, 0o600)
	var cfg map[string]any
	cfgRaw, _ := os.ReadFile(f.project)
	json.Unmarshal(cfgRaw, &cfg)
	cfg["registry"].(map[string]any)["digest"] = fileDigest(newReg)
	f.writeConfig(t, cfg)

	w3 := f.open(t, "rowan")
	old, err := w3.Lookup(ctx, *first.Pin)
	if err != nil || old.Descriptor.Revision != 1 || !old.DescriptorFound {
		t.Fatalf("original revision after the registry change = %+v, %v", old, err)
	}
	if info, _ := w3.Inspect("decision"); info.DescriptorRevision != 2 {
		t.Fatalf("current descriptor = %+v", info)
	}
	next := decisionRequest(t, "dec-2", "op-2")
	next.Classification = RecordClassification{Sensitivity: "1-confidential"}
	refused2, _ := w3.Record(ctx, next)
	if refused2.Reason != ReasonBelowFloor {
		t.Fatalf("write under the new floor = %+v", refused2)
	}
	next.Classification = RecordClassification{}
	res2 := mustRecord(t, w3, next, OutcomeAccepted)
	if res2.DescriptorRevision != 2 {
		t.Fatalf("new write = %+v", res2)
	}
	if e2, err := w3.Lookup(ctx, *res2.Pin); err != nil || e2.Classification.Sensitivity != "3-proprietary" {
		t.Fatalf("new write classification = %+v, %v", e2.Classification, err)
	}

	// A held revision is not served: lookup, render and the search view
	// all go through the store's serving path.
	f.clk.advance(time.Second)
	hold := HoldRequest{Pin: *first.Pin, OperationKey: "op-hold-1", EvidenceRef: "case-7",
		EvidenceDigest: "sha256:" + strings.Repeat("e", 64), ObservedAt: "2026-11-23T12:00:00Z"}
	hr, err := w3.Hold(ctx, hold)
	if err != nil || hr.Outcome != "tombstoned" {
		t.Fatalf("hold = %+v (%s), %v", hr, hr.Raw, err)
	}
	// A retry of the same hold later returns the original receipt.
	f.clk.advance(time.Second)
	if again, err := w3.Hold(ctx, hold); err != nil || again.Outcome != "tombstoned" || again.Seq != hr.Seq {
		t.Fatalf("hold retry = %+v, %v; want the original %+v", again, err, hr)
	}
	if _, err := w3.Lookup(ctx, *first.Pin); !errors.Is(err, ErrNotServed) {
		t.Fatalf("lookup of a held revision = %v", err)
	}
	if _, err := w3.Render(ctx, *first.Pin); !errors.Is(err, ErrNotServed) {
		t.Fatalf("render of a held revision = %v", err)
	}
	view, err := w3.BuildSearchView(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range view.Items {
		if it.Pin == *first.Pin {
			t.Fatal("the search view lists a held revision")
		}
	}
	hold.OperationKey = "op-hold-2"
	if _, err := bot.Hold(ctx, hold); err == nil {
		t.Fatal("an agent placed a hold")
	}
}

// A correction supersedes its exact predecessor; a reversal is a new
// decision naming the decision it reverses and is found from the view.
// The project-defined risk kind is written without changing the store.
func TestCorrectionReversalCustomKindAndRebuild(t *testing.T) {
	f := newFixture(t, "168h")
	w := f.open(t, "rowan")
	orig := mustRecord(t, w, decisionRequest(t, "dec-night", "op-1"), OutcomeAccepted)

	fix := decisionRequest(t, "dec-night", "op-2")
	fix.Predecessor = orig.Pin
	fix.Entry = json.RawMessage(strings.Replace(string(fix.Entry), "October trial", "October 2026 trial", 1))
	corrected := mustRecord(t, w, fix, OutcomeAccepted)
	if corrected.Pin.Revision != 2 {
		t.Fatalf("correction = %+v", corrected.Pin)
	}
	if _, err := w.Lookup(ctx, *orig.Pin); err != nil {
		t.Fatalf("the original revision no longer resolves: %v", err)
	}

	var rev map[string]any
	json.Unmarshal(example(t, "decision/valid-no-evidence.json"), &rev)
	rev["title"] = "Reverse: allow direct email requests again"
	rev["reverses"] = corrected.Pin
	revRaw, _ := json.Marshal(rev)
	reversal := mustRecord(t, w, Request{Project: "harbor-demo", Kind: "decision", RecordID: "dec-email-again",
		OperationKey: "op-3", Entry: revRaw, ProducedAt: "2026-11-23T12:00:00Z"}, OutcomeAccepted)

	risk := mustRecord(t, w, Request{Project: "harbor-demo", Kind: "risk", RecordID: "risk-fuel",
		OperationKey: "op-4", Entry: example(t, "risk/valid-supplier.json")}, OutcomeAccepted)
	page, err := w.Render(ctx, *risk.Pin)
	if err != nil || !strings.Contains(page, "# Risk: Single fuel supplier") {
		t.Fatalf("risk page = %q, %v", page, err)
	}

	view, err := w.RebuildSearchView(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := view.ReversalsOf(*corrected.Pin)
	if len(found) != 1 || found[0].Pin != *reversal.Pin {
		t.Fatalf("reversals of the corrected decision = %+v", found)
	}
	if got := view.Search("fuel"); len(got) != 1 || got[0].Kind != "risk" {
		t.Fatalf("search = %+v", got)
	}
	e, err := w.Lookup(ctx, *reversal.Pin)
	if err != nil || e.ProducedAt != "2026-11-23T12:00:00.000000Z" {
		t.Fatalf("reversal produced_at = %q, %v", e.ProducedAt, err)
	}

	// The view is derived: delete it and rebuild the same bytes.
	path := w.Project().SearchViewPath()
	before, _ := os.ReadFile(path)
	os.Remove(path)
	if _, err := w.RebuildSearchView(ctx); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(path)
	if len(before) == 0 || !bytes.Equal(before, after) {
		t.Fatal("rebuilt view differs")
	}
}
