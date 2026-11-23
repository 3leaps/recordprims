package projectwriter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/3leaps/canonprims/canon"
	"github.com/3leaps/recordprims"
)

// ErrNotServed is the one outcome Lookup gives for any pin it does not
// serve: a held revision, a retired record, a pin no accepted revision has,
// a wrong digest, or a stream that is not one of the project's store kinds.
// An ordinary caller is not told which.
var ErrNotServed = errors.New("not_served")

// DescriptorRef is the descriptor revision a record was written under.
type DescriptorRef struct {
	Kind     string `json:"kind"`
	Owner    string `json:"owner"`
	Revision int    `json:"revision"`
	Digest   string `json:"digest"`
}

// Entry is one accepted revision as served by the store.
type Entry struct {
	Pin             recordprims.Pin      `json:"pin"`
	Project         string               `json:"project"`
	Kind            string               `json:"kind"`
	PayloadContract string               `json:"payload_contract"`
	EntryContract   string               `json:"entry_contract"`
	Descriptor      DescriptorRef        `json:"descriptor"`
	DescriptorFound bool                 `json:"descriptor_found"`
	Entry           json.RawMessage      `json:"entry"`
	Provenance      json.RawMessage      `json:"provenance"`
	Classification  RecordClassification `json:"classification"`
	ProducedAt      string               `json:"produced_at,omitempty"`
	Supersedes      *recordprims.Pin     `json:"supersedes,omitempty"`
}

type servedRecord struct {
	Stream          string               `json:"stream"`
	RecordID        string               `json:"record_id"`
	Revision        uint64               `json:"revision"`
	PayloadContract string               `json:"payload_contract"`
	Provenance      json.RawMessage      `json:"provenance"`
	Classification  RecordClassification `json:"classification"`
	ProducedAt      *struct {
		Instant string `json:"instant"`
	} `json:"produced_at"`
	Supersedes *struct {
		Revision uint64 `json:"revision"`
		Digest   string `json:"digest"`
	} `json:"supersedes"`
	Payload struct {
		Project       string          `json:"project"`
		Descriptor    DescriptorRef   `json:"descriptor"`
		EntryContract string          `json:"entry_contract"`
		Entry         json.RawMessage `json:"entry"`
	} `json:"payload"`
}

// collect is a ReleaseSink that keeps record frames and checks each frame's
// bytes against its pin digest.
type collect struct {
	records map[recordprims.Pin][]byte
	order   []recordprims.Pin
}

func (c *collect) Frame(_ context.Context, h recordprims.FrameHeader, r io.Reader) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	if h.Kind != recordprims.FrameKindRecord {
		return nil
	}
	d, err := canon.Digest(canon.JCS, b, canon.DefaultLimits(), "sha256")
	if err != nil || d != h.Pin.Digest {
		return fmt.Errorf("record frame does not match its pin")
	}
	c.records[h.Pin] = b
	c.order = append(c.order, h.Pin)
	return nil
}

func (w *Writer) kindForStream(stream string) (string, *recordprims.Stream, bool) {
	for name, k := range w.project.Kinds {
		if k.Authority == AuthorityStore && k.Stream == stream {
			return name, w.streams[name], w.streams[name] != nil
		}
	}
	return "", nil, false
}

// serve releases every revision the store serves on one stream at the
// current cut, as record frames only.
func (w *Writer) serve(ctx context.Context, s *recordprims.Stream) (*collect, error) {
	c := &collect{records: map[recordprims.Pin][]byte{}}
	res, err := s.Release(ctx, recordprims.UseRequest{
		Purpose: recordprims.PurposeServe,
		Closure: recordprims.ClosureMetadataOnly,
	}, c)
	if err != nil {
		return nil, err
	}
	if res.Outcome != recordprims.ReleaseReleased {
		return nil, fmt.Errorf("%w: release %s %s", ErrNotServed, res.Outcome, res.RefusalReason)
	}
	return c, nil
}

// memberRefusals are the release refusals that concern the selected pin
// itself. Lookup folds them into ErrNotServed; any other refusal is about
// the stream and is returned as an error.
var memberRefusals = map[string]bool{"protected_use": true, "target_not_found": true}

// Lookup reads exactly one revision through the store's serving path: a
// serve release whose selection is the one pin, so holds and retirement
// apply and no other revision is delivered. Every pin it does not serve
// gives the same ErrNotServed.
func (w *Writer) Lookup(ctx context.Context, pin recordprims.Pin) (Entry, error) {
	kind, s, ok := w.kindForStream(pin.Stream)
	if !ok {
		return Entry{}, ErrNotServed
	}
	c := &collect{records: map[recordprims.Pin][]byte{}}
	res, err := s.Release(ctx, recordprims.UseRequest{
		Purpose:   recordprims.PurposeServe,
		Closure:   recordprims.ClosureMetadataOnly,
		Selection: []recordprims.Pin{pin},
	}, c)
	if err != nil {
		if errors.Is(err, recordprims.ErrSchema) {
			return Entry{}, ErrNotServed
		}
		return Entry{}, err
	}
	switch {
	case res.Outcome == recordprims.ReleaseRefused && memberRefusals[res.RefusalReason]:
		return Entry{}, ErrNotServed
	case res.Outcome != recordprims.ReleaseReleased:
		return Entry{}, fmt.Errorf("release %s %s", res.Outcome, res.RefusalReason)
	}
	raw, ok := c.records[pin]
	if !ok || len(c.order) != 1 {
		return Entry{}, ErrNotServed
	}
	return w.entryOf(kind, pin, raw)
}

func (w *Writer) entryOf(kind string, pin recordprims.Pin, raw []byte) (Entry, error) {
	var rec servedRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return Entry{}, err
	}
	if rec.PayloadContract != RecordPayloadContract {
		return Entry{}, fmt.Errorf("%s: %s is not %s", ReasonUnknownContract, rec.PayloadContract, RecordPayloadContract)
	}
	e := Entry{
		Pin: pin, Project: rec.Payload.Project, Kind: kind, PayloadContract: rec.PayloadContract,
		EntryContract: rec.Payload.EntryContract,
		Descriptor:    rec.Payload.Descriptor, Entry: rec.Payload.Entry, Provenance: rec.Provenance,
		Classification: rec.Classification,
	}
	d, found := w.registry.Lookup(rec.Payload.Descriptor.Kind, rec.Payload.Descriptor.Revision, rec.Payload.Descriptor.Digest)
	if found && d.EntryContract != e.EntryContract {
		return Entry{}, fmt.Errorf("%s: entry contract %s differs from its descriptor's %s", ReasonUnknownContract, e.EntryContract, d.EntryContract)
	}
	e.DescriptorFound = found
	if rec.ProducedAt != nil {
		e.ProducedAt = rec.ProducedAt.Instant
	}
	if sup := rec.Supersedes; sup != nil {
		e.Supersedes = &recordprims.Pin{Stream: pin.Stream, RecordID: pin.RecordID, Revision: sup.Revision, Digest: sup.Digest}
	}
	return e, nil
}

// History lists every accepted revision of a record, oldest first.
func (w *Writer) History(kind, recordID string) ([]recordprims.RevisionView, error) {
	s, ok := w.streams[kind]
	if !ok {
		return nil, fmt.Errorf("%s: %s", ReasonUnknownKind, kind)
	}
	return s.Revisions(recordID)
}

// Render returns a Markdown page for the revision at pin, produced from the
// served record and ending with the exact pin, so an agent can retrieve the
// same revision the human read.
func (w *Writer) Render(ctx context.Context, pin recordprims.Pin) (string, error) {
	e, err := w.Lookup(ctx, pin)
	if err != nil {
		return "", err
	}
	return RenderEntry(e)
}

// RenderEntry renders an entry as Markdown.
func RenderEntry(e Entry) (string, error) {
	var b strings.Builder
	switch e.EntryContract {
	case ContractDecision:
		var d Decision
		if err := json.Unmarshal(e.Entry, &d); err != nil {
			return "", err
		}
		fmt.Fprintf(&b, "# Decision: %s\n\n", d.Title)
		fmt.Fprintf(&b, "**Decision.** %s\n\n**Rationale.** %s\n\n", d.Decision, d.Rationale)
		fmt.Fprintf(&b, "**Authority.** %s (%s)\n\n", d.Authority.Name, d.Authority.Role)
		if d.EvidenceAbsent != nil {
			fmt.Fprintf(&b, "**Evidence.** None: %s\n\n", d.EvidenceAbsent.Reason)
		} else {
			b.WriteString("**Evidence.**\n\n")
			for _, r := range d.Evidence {
				fmt.Fprintf(&b, "- %s `%s`%s\n", r.Kind, r.Ref, digestSuffix(r.Digest))
			}
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "**Decided at.** %s\n\n", d.DecidedAt)
		if p := d.Reverses; p != nil {
			fmt.Fprintf(&b, "**Reverses.** %s\n\n", pinText(*p))
		}
	case ContractUpdate:
		var u Update
		if err := json.Unmarshal(e.Entry, &u); err != nil {
			return "", err
		}
		fmt.Fprintf(&b, "# Project update: %s\n\n", u.Status)
		fmt.Fprintf(&b, "%s\n\n**Observed at.** %s\n\n", u.Summary, u.ObservedAt)
		if len(u.References) > 0 {
			b.WriteString("**References.**\n\n")
			for _, r := range u.References {
				fmt.Fprintf(&b, "- %s `%s`%s\n", r.Kind, r.Ref, digestSuffix(r.Digest))
			}
			b.WriteString("\n")
		}
	case ContractRisk:
		var r Risk
		if err := json.Unmarshal(e.Entry, &r); err != nil {
			return "", err
		}
		fmt.Fprintf(&b, "# Risk: %s\n\n", r.Title)
		fmt.Fprintf(&b, "**Likelihood.** %s · **Impact.** %s · **Owner.** %s\n\n", r.Likelihood, r.Impact, r.Owner)
		fmt.Fprintf(&b, "**Mitigation.** %s\n\n", r.Mitigation)
	default:
		return "", fmt.Errorf("%s: no renderer for %s", ReasonUnknownContract, e.EntryContract)
	}
	if e.Supersedes != nil {
		fmt.Fprintf(&b, "**Supersedes.** %s\n\n", pinText(*e.Supersedes))
	}
	fmt.Fprintf(&b, "---\n\nProject `%s` · kind `%s` · descriptor `%s` revision %d\n\n", e.Project, e.Kind, e.Descriptor.Kind, e.Descriptor.Revision)
	fmt.Fprintf(&b, "Pin: %s\n", pinText(e.Pin))
	return b.String(), nil
}

func pinText(p recordprims.Pin) string {
	return fmt.Sprintf("`%s` / `%s` revision %d `%s`", p.Stream, p.RecordID, p.Revision, p.Digest)
}

func digestSuffix(d string) string {
	if d == "" {
		return ""
	}
	return " (`" + d + "`)"
}

// SearchItem is one served revision in the search view.
type SearchItem struct {
	Pin      recordprims.Pin  `json:"pin"`
	Kind     string           `json:"kind"`
	Title    string           `json:"title"`
	Status   string           `json:"status,omitempty"`
	Reverses *recordprims.Pin `json:"reverses,omitempty"`
}

// SearchView is the application's searchable view. It is derived entirely
// from what the store serves, so it can be deleted and rebuilt.
type SearchView struct {
	Project string       `json:"project"`
	Items   []SearchItem `json:"items"`
}

// BuildSearchView releases every served revision of the project's store
// kinds and derives the view. Held and retired revisions are not served and
// so do not appear.
func (w *Writer) BuildSearchView(ctx context.Context) (SearchView, error) {
	names := make([]string, 0, len(w.streams))
	for n := range w.streams {
		names = append(names, n)
	}
	sort.Strings(names)
	view := SearchView{Project: w.project.ID, Items: []SearchItem{}}
	for _, kind := range names {
		c, err := w.serve(ctx, w.streams[kind])
		if err != nil {
			return SearchView{}, err
		}
		for _, pin := range c.order {
			e, err := w.entryOf(kind, pin, c.records[pin])
			if err != nil {
				return SearchView{}, err
			}
			var f struct {
				Title    string           `json:"title"`
				Summary  string           `json:"summary"`
				Status   string           `json:"status"`
				Reverses *recordprims.Pin `json:"reverses"`
			}
			if err := json.Unmarshal(e.Entry, &f); err != nil {
				return SearchView{}, err
			}
			title := f.Title
			if title == "" {
				title = f.Summary
			}
			view.Items = append(view.Items, SearchItem{Pin: pin, Kind: kind, Title: title, Status: f.Status, Reverses: f.Reverses})
		}
	}
	sort.Slice(view.Items, func(i, j int) bool {
		a, b := view.Items[i].Pin, view.Items[j].Pin
		if a.Stream != b.Stream {
			return a.Stream < b.Stream
		}
		if a.RecordID != b.RecordID {
			return a.RecordID < b.RecordID
		}
		return a.Revision < b.Revision
	})
	return view, nil
}

// Search returns the view's items whose title contains query (case-insensitive).
func (v SearchView) Search(query string) []SearchItem {
	q := strings.ToLower(query)
	out := []SearchItem{}
	for _, it := range v.Items {
		if strings.Contains(strings.ToLower(it.Title), q) {
			out = append(out, it)
		}
	}
	return out
}

// ReversalsOf returns the items that reverse pin.
func (v SearchView) ReversalsOf(pin recordprims.Pin) []SearchItem {
	out := []SearchItem{}
	for _, it := range v.Items {
		if it.Reverses != nil && *it.Reverses == pin {
			out = append(out, it)
		}
	}
	return out
}

// RebuildSearchView derives the search view from the store and replaces the
// project's view file with it. Deleting the file loses nothing.
func (w *Writer) RebuildSearchView(ctx context.Context) (SearchView, error) {
	view, err := w.BuildSearchView(ctx)
	if err != nil {
		return SearchView{}, err
	}
	raw, err := json.MarshalIndent(view, "", "  ")
	if err != nil {
		return SearchView{}, err
	}
	path := w.project.SearchViewPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return SearchView{}, err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return SearchView{}, err
	}
	if err := os.Rename(tmp, path); err != nil {
		return SearchView{}, err
	}
	return view, nil
}

// LoadSearchView reads the project's view file.
func (w *Writer) LoadSearchView() (SearchView, error) {
	raw, err := os.ReadFile(w.project.SearchViewPath())
	if err != nil {
		return SearchView{}, err
	}
	var v SearchView
	err = json.Unmarshal(raw, &v)
	return v, err
}

// servedPins is the set of revisions the store serves now, across the
// project's store kinds.
func (w *Writer) servedPins(ctx context.Context) (map[recordprims.Pin]bool, error) {
	out := map[recordprims.Pin]bool{}
	for _, s := range w.streams {
		c, err := w.serve(ctx, s)
		if err != nil {
			return nil, err
		}
		for pin := range c.records {
			out[pin] = true
		}
	}
	return out, nil
}

// Search searches the persisted view, then drops any hit the store no longer
// serves (held since the view was built, or retired), so a stale view never
// returns content the serving path would refuse. Rebuild the view to pick up
// new revisions.
func (w *Writer) Search(ctx context.Context, query string) ([]SearchItem, error) {
	view, err := w.LoadSearchView()
	if err != nil {
		return nil, err
	}
	served, err := w.servedPins(ctx)
	if err != nil {
		return nil, err
	}
	out := []SearchItem{}
	for _, it := range view.Search(query) {
		if served[it.Pin] {
			out = append(out, it)
		}
	}
	return out, nil
}
