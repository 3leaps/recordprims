package journal

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/3leaps/recordprims/internal/canon"
	"github.com/3leaps/recordprims/internal/wire"
)

func reasonOf(err error) string {
	var r *wire.Reject
	if errors.As(err, &r) {
		return r.Reason
	}
	if err != nil {
		return "error: " + err.Error()
	}
	return ""
}

func node(t *testing.T, kind, text string) *wire.Node {
	t.Helper()
	n, err := wire.Ingest([]byte(text), kind)
	if err != nil {
		t.Fatalf("%s: %v", text, err)
	}
	return n
}

const stream = "records.example"

func at(sec int) string { return fmt.Sprintf("2026-10-20T00:00:%02d.000000Z", sec) }

func opened(seq int) string {
	return fmt.Sprintf(`{"v":1,"seq":%d,"group":%d,"group_size":1,"stream":%q,"type":"stream_opened","at":%q,"mode":"revisioned","failure_model":"survives_power_loss"}`, seq, seq, stream, at(seq))
}

func record(id string, rev int, supersedes string) string {
	s := fmt.Sprintf(`{"v":1,"stream":%q,"record_id":%q,"revision":%d,"payload_contract":"contract: example/v0","provenance":{"actor_kind":"human","actor":"operator"},"classification":{"retention":"standard","sensitivity":"0-public"}`, stream, id, rev)
	if supersedes != "" {
		s += `,"supersedes":` + supersedes
	}
	return s + "}"
}

func digestOf(t *testing.T, rec string) string {
	t.Helper()
	d, err := canon.Digest([]byte(rec))
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// content builds an appended or revised entry with its record's digest.
func content(t *testing.T, typ string, seq int, rec string) string {
	return fmt.Sprintf(`{"v":1,"seq":%d,"group":%d,"group_size":1,"stream":%q,"type":%q,"at":%q,"record":%s,"digest":%q,"idempotency_key":"k%d"}`, seq, seq, stream, typ, at(seq), rec, digestOf(t, rec), seq)
}

func entries(t *testing.T, lines ...string) []*wire.Node {
	t.Helper()
	out := make([]*wire.Node, len(lines))
	for i, l := range lines {
		out[i] = node(t, wire.KindJournalEntry, l)
	}
	return out
}

func set(entry, member, value string) string {
	i := strings.Index(entry, `"`+member+`":`)
	if i < 0 {
		panic(member)
	}
	j := i + len(member) + 3
	depth, k := 0, j
	for ; k < len(entry); k++ {
		switch entry[k] {
		case '{', '[':
			depth++
		case '}', ']':
			if depth == 0 {
				return entry[:j] + value + entry[k:]
			}
			depth--
		case ',':
			if depth == 0 {
				return entry[:j] + value + entry[k:]
			}
		case '"':
			for k++; entry[k] != '"'; k++ {
				if entry[k] == '\\' {
					k++
				}
			}
		}
	}
	panic(member)
}

func TestCheckJournal(t *testing.T) {
	r1 := record("a", 1, "")
	a1 := content(t, "appended", 2, r1)
	head1 := fmt.Sprintf(`{"revision":1,"digest":%q}`, digestOf(t, r1))
	r2 := record("a", 2, head1)
	rev2 := content(t, "revised", 3, r2)
	conflict := func(seq int, current string) string {
		s := fmt.Sprintf(`{"v":1,"seq":%d,"group":%d,"group_size":1,"stream":%q,"type":"conflicted","at":%q,"record_id":"a","idempotency_key":"c%d"`, seq, seq, stream, at(seq), seq)
		if current != "" {
			s += `,"current":` + current
		}
		return s + "}"
	}
	single := set(opened(1), "mode", `"single_revision"`)
	for _, c := range []struct {
		name  string
		lines []string
		want  string
	}{
		{"valid stream", []string{opened(1), a1, rev2, conflict(4, fmt.Sprintf(`{"revision":2,"digest":%q}`, digestOf(t, r2)))}, ""},
		{"empty journal", nil, ReasonEmpty},
		{"seq gap", []string{opened(1), set(a1, "seq", "3")}, ReasonSeqGap},
		{"first seq not 1", []string{set(opened(1), "seq", "2")}, ReasonSeqGap},
		{"second stream", []string{opened(1), set(a1, "stream", `"other"`)}, ReasonStreamMismatch},
		{"conflicted entry on another stream", []string{opened(1), set(conflict(2, ""), "stream", `"other"`)}, ReasonStreamMismatch},
		{"group size changed before it closed", []string{opened(1), set(set(a1, "group", "2"), "group_size", "3"), set(set(rev2, "group", "2"), "group_size", "2")}, ReasonGroup},
		{"record of another stream", []string{opened(1), content(t, "appended", 2, set(r1, "stream", `"other"`))}, ReasonStreamMismatch},
		{"no stream_opened first", []string{set(a1, "seq", "1")}, ReasonStreamOpenedPosition},
		{"stream_opened twice", []string{opened(1), opened(2)}, ReasonStreamOpenedPosition},
		{"group not at its own seq", []string{opened(1), set(a1, "group", "1")}, ReasonGroup},
		{"group changed before it closed", []string{opened(1), set(set(a1, "group", "2"), "group_size", "2"), set(set(rev2, "group", "3"), "group_size", "1")}, ReasonGroup},
		{"final group truncated", []string{opened(1), set(a1, "group_size", "2")}, ReasonInconsistentCut},
		{"two-entry group complete", []string{opened(1), set(a1, "group_size", "2"), set(set(rev2, "group", "2"), "group_size", "2")}, ""},
		{"wrong digest", []string{opened(1), set(a1, "digest", `"sha256:`+strings.Repeat("0", 64)+`"`)}, ReasonDigest},
		{"supersedes revision not revision-1", []string{opened(1), a1, content(t, "revised", 3, set(r2, "revision", "3"))}, ReasonSupersedesRevision},
		{"appended twice", []string{opened(1), a1, set(content(t, "appended", 3, r1), "seq", "3")}, ReasonRevisionChain},
		{"appended at revision 2", []string{opened(1), content(t, "appended", 2, record("a", 2, ""))}, ReasonRevisionChain},
		{"revised with no head", []string{opened(1), content(t, "revised", 2, r2)}, ReasonRevisionChain},
		{"revised over a stale head", []string{opened(1), a1, content(t, "revised", 3, record("a", 2, fmt.Sprintf(`{"revision":1,"digest":"sha256:%s"}`, strings.Repeat("0", 64))))}, ReasonRevisionChain},
		{"supersedes with an extra member", []string{opened(1), a1, content(t, "revised", 3, record("a", 2, fmt.Sprintf(`{"revision":1,"digest":%q,"x":1}`, digestOf(t, r1))))}, ReasonRevisionChain},
		{"revised in a single-revision stream", []string{single, a1, rev2}, ReasonModeViolation},
		{"conflicted naming a stale head", []string{opened(1), a1, rev2, conflict(4, head1)}, ReasonConflictCurrent},
		{"conflicted omitting the head", []string{opened(1), a1, conflict(3, "")}, ReasonConflictCurrent},
		{"conflicted null current while a head exists", []string{opened(1), a1, conflict(3, "null")}, ReasonConflictCurrent},
		{"conflicted naming a head that does not exist", []string{opened(1), conflict(2, head1)}, ReasonConflictCurrent},
		{"conflicted naming the head digest at the next revision", []string{opened(1), a1, conflict(3, fmt.Sprintf(`{"revision":2,"digest":%q}`, digestOf(t, r1)))}, ReasonConflictCurrent},
		{"conflicted naming the head digest at revision 0", []string{opened(1), a1, conflict(3, fmt.Sprintf(`{"revision":0,"digest":%q}`, digestOf(t, r1)))}, ReasonConflictCurrent},
		{"conflicted naming the head exactly", []string{opened(1), a1, conflict(3, head1)}, ""},
		{"group closing seq exactly the safe maximum", []string{opened(1), set(set(a1, "group", "2"), "group_size", "9007199254740990")}, ReasonInconsistentCut},
		{"conflicted with no head and none named", []string{opened(1), conflict(2, "")}, ""},
		{"group closing seq above the safe maximum", []string{opened(1), set(set(a1, "group", "2"), "group_size", "9007199254740991")}, wire.ReasonNumericDomain},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := reasonOf(CheckJournal(entries(t, c.lines...))); got != c.want {
				t.Fatalf("%q, want %q", got, c.want)
			}
		})
	}
}

func TestNewWindow(t *testing.T) {
	for tok, want := range map[string]string{
		"60000": "", "1": "", "9007199254740991": "",
		"0": ReasonIdempotencyWindow, "-1": ReasonIdempotencyWindow, "-99999999999999999999": ReasonIdempotencyWindow,
		"1.5": ReasonIdempotencyWindow, "60000.0": ReasonIdempotencyWindow, "6e4": ReasonIdempotencyWindow,
		"-0":               ReasonIdempotencyWindow,
		"9007199254740992": wire.ReasonNumericDomain, "99999999999999999999999": wire.ReasonNumericDomain,
	} {
		n, err := wire.ParseJSON([]byte(tok))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := New(n); reasonOf(err) != want {
			t.Errorf("%s: %q, want %q", tok, reasonOf(err), want)
		}
	}
	if _, err := New(nil); reasonOf(err) != ReasonIdempotencyWindow {
		t.Errorf("missing window: %v", err)
	}
	s, _ := wire.ParseJSON([]byte(`"60000"`))
	if _, err := New(s); reasonOf(err) != ReasonIdempotencyWindow {
		t.Errorf("string window: %v", err)
	}
}

func window(t *testing.T) *State {
	n, _ := wire.ParseJSON([]byte("60000"))
	s, err := New(n)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// ingestAll replays lines and returns the first refusal.
func ingestAll(t *testing.T, s *State, lines ...string) string {
	t.Helper()
	for _, e := range entries(t, lines...) {
		if err := s.Ingest(e); err != nil {
			return reasonOf(err)
		}
	}
	return ""
}

func TestIngestEnvelopeAndOrder(t *testing.T) {
	r1 := record("a", 1, "")
	a1 := content(t, "appended", 2, r1)
	produced := func(instant string) string {
		return content(t, "appended", 2, set(r1, "classification", `{"retention":"standard","sensitivity":"0-public"},"produced_at":{"instant":"`+instant+`","clock":"wall"}`))
	}
	for _, c := range []struct {
		name  string
		lines []string
		want  string
	}{
		{"valid", []string{opened(1), a1}, ""},
		{"v0 entry", []string{set(opened(1), "v", "0")}, ReasonJournalVersion},
		{"v2 entry", []string{set(opened(1), "v", "2")}, ReasonJournalVersion},
		{"v0 record", []string{opened(1), content(t, "appended", 2, set(r1, "v", "0"))}, ReasonRecordVersion},
		{"at with an offset", []string{set(opened(1), "at", `"2026-10-20T00:00:01.000000+00:00"`)}, ReasonInstantSpelling},
		{"at with three fractional digits", []string{set(opened(1), "at", `"2026-10-20T00:00:01.000Z"`)}, ReasonInstantSpelling},
		{"at lowercase z", []string{set(opened(1), "at", `"2026-10-20T00:00:01.000000z"`)}, ReasonInstantSpelling},
		{"at year 0000", []string{set(opened(1), "at", `"0000-01-01T00:00:00.000000Z"`)}, ReasonInstantSpelling},
		{"at leap second", []string{set(opened(1), "at", `"2016-12-31T23:59:60.000000Z"`)}, ReasonInstantSpelling},
		{"at February 29 in a common year", []string{set(opened(1), "at", `"2026-02-29T00:00:00.000000Z"`)}, ReasonInstantSpelling},
		{"at February 29 in 1900", []string{set(opened(1), "at", `"1900-02-29T00:00:00.000000Z"`)}, ReasonInstantSpelling},
		{"at February 29 in 2000", []string{set(opened(1), "at", `"2000-02-29T00:00:00.000000Z"`), set(a1, "at", `"2026-10-20T00:00:02.000000Z"`)}, ""},
		{"at hour 24", []string{set(opened(1), "at", `"2026-10-20T24:00:00.000000Z"`)}, ReasonInstantSpelling},
		{"at year 0001 and 9999", []string{set(opened(1), "at", `"0001-01-01T00:00:00.000000Z"`), set(a1, "at", `"9999-12-31T23:59:59.999999Z"`)}, ""},
		{"produced_at canonical", []string{opened(1), produced("2026-10-20T00:00:00.500000Z")}, ""},
		{"produced_at impossible date", []string{opened(1), produced("2026-02-30T00:00:00.000000Z")}, ReasonInstantSpelling},
		{"time runs backwards", []string{opened(1), set(a1, "at", `"2026-10-20T00:00:00.999999Z"`)}, ReasonJournalTime},
		{"time stands still", []string{opened(1), set(a1, "at", `"`+at(1)+`"`)}, ""},
		{"seq gap found when the group closes", []string{opened(1), set(set(a1, "seq", "3"), "group", "3")}, ReasonSeqGap},
		{"an entry that does not close its group is not judged yet", []string{opened(1), set(a1, "seq", "3")}, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := ingestAll(t, window(t), c.lines...); got != c.want {
				t.Fatalf("%q, want %q", got, c.want)
			}
		})
	}
}

// The journal invariants are checked when an entry closes its group: an
// entry inside an open group is not judged until the group closes.
func TestIngestChecksCompleteGroups(t *testing.T) {
	r1 := record("a", 1, "")
	open := set(content(t, "appended", 2, r1), "group_size", "2")
	s := window(t)
	if got := ingestAll(t, s, opened(1), open); got != "" {
		t.Fatalf("open group: %q", got)
	}
	bad := set(set(content(t, "revised", 3, record("a", 2, `{"revision":1,"digest":"sha256:`+strings.Repeat("0", 64)+`"}`)), "group", "2"), "group_size", "2")
	if got := ingestAll(t, s, bad); got != ReasonRevisionChain {
		t.Fatalf("closing entry: %q, want revision_chain", got)
	}
}

func TestIngestRequestObservedAt(t *testing.T) {
	r1 := record("a", 1, "")
	d1 := digestOf(t, r1)
	req := func(obs string) string {
		r := fmt.Sprintf(`{"v":1,"stream":%q,"idempotency_key":"hold-obs","action":"tombstone","scope":"revision","target":{"stream":%q,"record_id":"a","revision":1,"digest":%q},"reason":"legal","observed_at":%q}`, stream, stream, d1, obs)
		return fmt.Sprintf(`{"v":1,"seq":3,"group":3,"group_size":1,"stream":%q,"type":"tombstoned","at":%q,"request":%s,"request_digest":%q}`, stream, at(3), r, digestOf(t, r))
	}
	if got := ingestAll(t, window(t), opened(1), content(t, "appended", 2, r1), req("2026-10-20T00:00:01.000000Z")); got != "" {
		t.Fatalf("canonical observed_at: %q", got)
	}
	if got := ingestAll(t, window(t), opened(1), content(t, "appended", 2, r1), req("2026-10-20T00:00:01.0000000Z")); got != ReasonInstantSpelling {
		t.Fatalf("seven fractional digits: %q", got)
	}
}

func TestIngestBodyRemovalPlacement(t *testing.T) {
	life := func(seq int, strm, typ string) string {
		return fmt.Sprintf(`{"v":1,"seq":%d,"group":%d,"group_size":1,"stream":%q,"type":%q,"at":%q,"incarnation":1,"body":{"digest":"sha256:%s","length":8}}`, seq, seq, strm, typ, at(seq), strings.Repeat("1", 64))
	}
	openedOn := func(strm string) string { return set(opened(1), "stream", fmt.Sprintf("%q", strm)) }
	r1 := record("a", 1, "")
	for _, c := range []struct {
		name    string
		removal string // "" means no designated stream
		lines   []string
		want    string
	}{
		{"lifecycle on the designated stream", "bodies", []string{openedOn("bodies"), life(2, "bodies", "body_published"), life(3, "bodies", "body_removal_started"), life(4, "bodies", "body_removed")}, ""},
		{"content on the designated stream", stream, []string{opened(1), content(t, "appended", 2, r1)}, ReasonBodyRemovalStream},
		{"lifecycle on a content stream", "bodies", []string{opened(1), life(2, stream, "body_published")}, ReasonBodyRemovalStream},
		{"lifecycle without a designated stream", "", []string{opened(1), life(2, stream, "body_published")}, ReasonBodyRemovalStream},
		{"designated stream opened as a continuation", "bodies", []string{set(openedOn("bodies"), "failure_model", `"survives_power_loss","continues":{}`)}, ReasonBodyRemovalStream},
		{"content stream beside a designated one", "bodies", []string{opened(1), content(t, "appended", 2, r1)}, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := window(t)
			if c.removal != "" {
				s.SetRemovalStream(c.removal)
			}
			if got := ingestAll(t, s, c.lines...); got != c.want {
				t.Fatalf("%q, want %q", got, c.want)
			}
		})
	}
}

func TestPartial(t *testing.T) {
	s := window(t)
	if got := ingestAll(t, s, opened(1)); got != "" || s.Partial() {
		t.Fatalf("stream_opened alone: %q partial=%v", got, s.Partial())
	}
	r1 := record("a", 1, "")
	if got := ingestAll(t, s, content(t, "appended", 2, r1)); got != "" || s.Partial() {
		t.Fatalf("appended without imported_from: %q partial=%v, want false", got, s.Partial())
	}
	imp := window(t)
	aImp := set(content(t, "appended", 2, r1), "idempotency_key", `"k2","imported_from":{"capability":"contract: durable-record/v1","pin":{"stream":"src","record_id":"a","revision":1,"digest":`+fmt.Sprintf("%q", digestOf(t, r1))+`}}`)
	if got := ingestAll(t, imp, opened(1), aImp); got != ReasonContinuation || imp.Partial() {
		t.Fatalf("appended with imported_from on non-continuation stream: %q partial=%v, want %q/false", got, imp.Partial(), ReasonContinuation)
	}
	c := window(t)
	if got := ingestAll(t, c, set(opened(1), "failure_model", `"survives_power_loss","continues":{}`)); got != ReasonContinuation || c.Partial() {
		t.Fatalf("continuation without source: %q partial=%v, want %q/false", got, c.Partial(), ReasonContinuation)
	}
	r := window(t)
	if got := ingestAll(t, r, set(opened(1), "failure_model", `"survives_power_loss","recovery":{}`)); got != "" || r.Partial() {
		t.Fatalf("recovery: %q partial=%v, want false", got, r.Partial())
	}
}

func TestIngestContentAndKeys(t *testing.T) {
	r1 := record("a", 1, "")
	head1 := fmt.Sprintf(`{"revision":1,"digest":%q}`, digestOf(t, r1))
	r2 := record("a", 2, head1)
	rb := record("b", 1, "")
	dedup := func(seq int, atStr, rid, key string) string {
		return fmt.Sprintf(`{"v":1,"seq":%d,"group":%d,"group_size":1,"stream":%q,"type":"deduplicated","at":%q,"record_id":%q,"idempotency_key":%q}`, seq, seq, stream, atStr, rid, key)
	}
	a1At := func(seq int, atStr, rec, key string) string {
		return set(set(content(t, "appended", seq, rec), "at", fmt.Sprintf("%q", atStr)), "idempotency_key", fmt.Sprintf("%q", key))
	}
	revAt := func(seq int, atStr, rec, key string) string {
		return set(set(content(t, "revised", seq, rec), "at", fmt.Sprintf("%q", atStr)), "idempotency_key", fmt.Sprintf("%q", key))
	}
	for _, c := range []struct {
		name  string
		lines []string
		want  string
	}{
		{"append and revise accepted", []string{opened(1), content(t, "appended", 2, r1), content(t, "revised", 3, r2)}, ""},
		{"live key reuse on append", []string{opened(1), a1At(2, "2026-10-20T00:00:02.000000Z", r1, "k"), a1At(3, "2026-10-20T00:01:01.999999Z", rb, "k")}, ReasonAcceptedKeyReuse},
		{"live key reuse on revise", []string{opened(1), a1At(2, "2026-10-20T00:00:02.000000Z", r1, "k"), revAt(3, "2026-10-20T00:00:03.000000Z", r2, "k")}, ReasonAcceptedKeyReuse},
		{"key reuse at exact window expiry", []string{opened(1), a1At(2, "2026-10-20T00:00:02.000000Z", r1, "k"), a1At(3, "2026-10-20T00:01:02.000000Z", rb, "k")}, ""},
		{"dedup live content key", []string{opened(1), a1At(2, "2026-10-20T00:00:02.000000Z", r1, "k"), dedup(3, "2026-10-20T00:01:01.999999Z", "a", "k")}, ""},
		{"dedup absent key", []string{opened(1), content(t, "appended", 2, r1), dedup(3, at(3), "a", "missing")}, ReasonInvalidDeduplication},
		{"dedup expired key at exact boundary", []string{opened(1), a1At(2, "2026-10-20T00:00:02.000000Z", r1, "k"), dedup(3, "2026-10-20T00:01:02.000000Z", "a", "k")}, ReasonInvalidDeduplication},
		{"dedup wrong record_id", []string{opened(1), a1At(2, "2026-10-20T00:00:02.000000Z", r1, "k"), dedup(3, at(3), "b", "k")}, ReasonInvalidDeduplication},
		{"dedup does not slide the window", []string{
			opened(1),
			a1At(2, "2026-10-20T00:00:02.000000Z", r1, "k"),
			dedup(3, "2026-10-20T00:00:32.000000Z", "a", "k"),
			dedup(4, "2026-10-20T00:01:02.000000Z", "a", "k"),
		}, ReasonInvalidDeduplication},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := ingestAll(t, window(t), c.lines...); got != c.want {
				t.Fatalf("%q, want %q", got, c.want)
			}
		})
	}

	// Records map holds both revisions keyed by Pin.
	s := window(t)
	if got := ingestAll(t, s, opened(1), content(t, "appended", 2, r1), content(t, "revised", 3, r2)); got != "" {
		t.Fatal(got)
	}
	recs := s.Records()
	p1 := Pin{Stream: stream, RecordID: "a", Revision: 1, Digest: digestOf(t, r1)}
	p2 := Pin{Stream: stream, RecordID: "a", Revision: 2, Digest: digestOf(t, r2)}
	if len(recs) != 2 || recs[p1] == nil || recs[p2] == nil {
		t.Fatalf("records map: got %d entries, want p1 and p2", len(recs))
	}

	// Retired record check refuses a content write to a retired record_id.
	s.retired["z"] = true
	if got := ingestAll(t, s, content(t, "appended", 4, record("z", 1, ""))); got != ReasonRetiredRecord {
		t.Fatalf("retired record write: %q, want %q", got, ReasonRetiredRecord)
	}
}

func TestReconcile(t *testing.T) {
	r1 := record("a", 1, "")
	a1 := set(content(t, "appended", 2, r1), "idempotency_key", `"k2","body_status":"verified"`)
	s := window(t)
	if got := ingestAll(t, s, opened(1), a1); got != "" {
		t.Fatal(got)
	}
	found, err := s.Reconcile("k2", "2026-10-20T00:01:01.999999Z", "2026-10-20T00:00:05.000000Z")
	if err != nil {
		t.Fatalf("found reconcile: %v", err)
	}
	wantFound := node(t, wire.KindWriteReceipt, fmt.Sprintf(`{"v":1,"kind":"reconciliation","stream":%q,"idempotency_key":"k2","attempted_at":"2026-10-20T00:00:05.000000Z","result":"found","receipt":{"v":1,"kind":"receipt","stream":%q,"idempotency_key":"k2","outcome":"appended","pin":{"stream":%q,"record_id":"a","revision":1,"digest":%q},"seq":2,"failure_model":"survives_power_loss","body_status":"verified"}}`, stream, stream, stream, digestOf(t, r1)))
	if !Equal(found, wantFound) {
		t.Fatalf("found reconcile mismatch: got %s", wire.Encode(found))
	}

	unk, err := s.Reconcile("k2", "2026-10-20T00:01:02.000000Z", "2026-10-20T00:00:05.000000Z")
	if err != nil {
		t.Fatalf("expired reconcile: %v", err)
	}
	wantUnk := node(t, wire.KindWriteReceipt, fmt.Sprintf(`{"v":1,"kind":"reconciliation","stream":%q,"idempotency_key":"k2","attempted_at":"2026-10-20T00:00:05.000000Z","result":"unknown","reason":"coverage_unprovable"}`, stream))
	if !Equal(unk, wantUnk) {
		t.Fatalf("expired reconcile mismatch: got %s", wire.Encode(unk))
	}

	if _, err := s.Reconcile("k2", "2026-10-20T00:00:01.999999Z", "2026-10-20T00:00:02.000000Z"); reasonOf(err) != ReasonJournalTime {
		t.Fatalf("reconcile before journal: %q, want %q", reasonOf(err), ReasonJournalTime)
	}
	if _, err := s.Reconcile("k2", "2026-10-20T00:00:02Z", "2026-10-20T00:00:02.000000Z"); reasonOf(err) != ReasonInstantSpelling {
		t.Fatalf("reconcile noncanonical at: %q, want %q", reasonOf(err), ReasonInstantSpelling)
	}
	if _, err := s.Reconcile("k2", "2026-10-20T00:00:02.000000Z", "0000-01-01T00:00:00.000000Z"); reasonOf(err) != ReasonInstantSpelling {
		t.Fatalf("reconcile year-zero attempted_at: %q, want %q", reasonOf(err), ReasonInstantSpelling)
	}
}

func TestVerifyReceipt(t *testing.T) {
	r1 := record("a", 1, "")
	d1 := digestOf(t, r1)
	req := fmt.Sprintf(`{"v":1,"stream":%q,"idempotency_key":"hold-1","action":"tombstone","scope":"revision","target":{"stream":%q,"record_id":"a","revision":1,"digest":%q},"reason":"legal","observed_at":"2026-10-20T00:00:02.000000Z"}`, stream, stream, d1)
	reqDig := digestOf(t, req)
	tomb := fmt.Sprintf(`{"v":1,"seq":3,"group":3,"group_size":1,"stream":%q,"type":"tombstoned","at":%q,"request":%s,"request_digest":%q}`, stream, at(3), req, reqDig)
	retReq := fmt.Sprintf(`{"stream":%q,"record_id":"a","reason":"retention_expired","idempotency_key":"ret-1"}`, stream)
	retDig := digestOf(t, retReq)
	ret := fmt.Sprintf(`{"v":1,"seq":4,"group":4,"group_size":1,"stream":%q,"type":"retired","at":%q,"record_id":"a","reason":"retention_expired","idempotency_key":"ret-1","request_digest":%q}`, stream, at(4), retDig)

	s := window(t)
	if got := ingestAll(t, s, opened(1), content(t, "appended", 2, r1), tomb, ret); got != "" {
		t.Fatal(got)
	}

	validProt := fmt.Sprintf(`{"v":1,"stream":%q,"idempotency_key":"hold-1","outcome":"tombstoned","seq":3,"request_digest":%q,"failure_model":"survives_power_loss"}`, stream, reqDig)
	if _, err := s.VerifyReceipt(node(t, wire.KindProtectionReceipt, validProt)); err != nil {
		t.Fatalf("valid protection receipt: %v", err)
	}
	if _, err := s.VerifyReceipt(node(t, wire.KindProtectionReceipt, set(validProt, "seq", "2"))); reasonOf(err) != ReasonReceiptMismatch {
		t.Fatalf("wrong seq protection receipt: %q", reasonOf(err))
	}

	validRet := fmt.Sprintf(`{"v":1,"kind":"receipt","stream":%q,"idempotency_key":"ret-1","outcome":"retired","seq":4,"record_id":"a","request_digest":%q,"failure_model":"survives_power_loss"}`, stream, retDig)
	if _, err := s.VerifyReceipt(node(t, wire.KindWriteReceipt, validRet)); err != nil {
		t.Fatalf("valid retirement receipt: %v", err)
	}
	if _, err := s.VerifyReceipt(node(t, wire.KindWriteReceipt, set(validRet, "failure_model", `"none"`))); reasonOf(err) != ReasonReceiptMismatch {
		t.Fatalf("wrong failure_model retirement receipt: %q", reasonOf(err))
	}

	validRec := fmt.Sprintf(`{"v":1,"kind":"reconciliation","stream":%q,"idempotency_key":"k","attempted_at":"2026-10-20T00:00:05.000000Z","result":"not_found","window_covers_from":"2026-10-20T00:00:01.000000Z","window_covers_until":"2026-10-20T00:00:06.000000Z"}`, stream)
	if _, err := s.VerifyReceipt(node(t, wire.KindWriteReceipt, validRec)); err != nil {
		t.Fatalf("valid reconciliation: %v", err)
	}
	if _, err := s.VerifyReceipt(node(t, wire.KindWriteReceipt, set(validRec, "window_covers_from", `"2026-02-30T00:00:01.000000Z"`))); reasonOf(err) != ReasonInstantSpelling {
		t.Fatalf("impossible window_covers_from: %q", reasonOf(err))
	}

	// Unkeyed retirement entry cannot verify a retirement receipt.
	rb := record("b", 1, "")
	unkeyedRet := fmt.Sprintf(`{"v":1,"seq":6,"group":6,"group_size":1,"stream":%q,"type":"retired","at":%q,"record_id":"b","reason":"retention_expired"}`, stream, at(6))
	if got := ingestAll(t, s, content(t, "appended", 5, rb), unkeyedRet); got != "" {
		t.Fatal(got)
	}
	unkeyedRcpt := set(set(validRet, "seq", "6"), "record_id", `"b"`)
	if _, err := s.VerifyReceipt(node(t, wire.KindWriteReceipt, unkeyedRcpt)); reasonOf(err) != ReasonReceiptMismatch {
		t.Fatalf("unkeyed retirement receipt: %q, want %q", reasonOf(err), ReasonReceiptMismatch)
	}
}

func TestCheckReceipt(t *testing.T) {
	r1 := record("a", 1, "")
	d1 := digestOf(t, r1)
	a1 := content(t, "appended", 2, r1)
	lines := entries(t, opened(1), a1)

	base := fmt.Sprintf(`{"v":1,"kind":"receipt","stream":%q,"idempotency_key":"k2","outcome":"appended","pin":{"stream":%q,"record_id":"a","revision":1,"digest":%q},"seq":2,"failure_model":"survives_power_loss"}`, stream, stream, d1)
	found := fmt.Sprintf(`{"v":1,"kind":"reconciliation","stream":%q,"idempotency_key":"k2","attempted_at":"2026-10-20T00:00:03.000000Z","result":"found","receipt":%s}`, stream, base)
	notFound := fmt.Sprintf(`{"v":1,"kind":"reconciliation","stream":%q,"idempotency_key":"absent","attempted_at":"2026-10-20T00:00:03.000000Z","result":"not_found","window_covers_from":"2026-10-20T00:00:01.000000Z","window_covers_until":"2026-10-20T00:00:05.000000Z"}`, stream)
	unknown := fmt.Sprintf(`{"v":1,"kind":"reconciliation","stream":%q,"idempotency_key":"k2","attempted_at":"2026-10-20T00:00:03.000000Z","result":"unknown","reason":"coverage_unprovable"}`, stream)
	conflict := fmt.Sprintf(`{"v":1,"kind":"receipt","stream":%q,"idempotency_key":"k3","outcome":"revision_conflict","current":{"stream":%q,"record_id":"a","revision":1,"digest":%q}}`, stream, stream, d1)

	for _, c := range []struct{ name, doc, want string }{
		{"valid accepted receipt", base, ""},
		{"valid found reconciliation", found, ""},
		{"valid not_found reconciliation", notFound, ""},
		{"valid unknown reconciliation", unknown, ""},
		{"valid revision_conflict receipt", conflict, ""},
		{"found reconciliation with non-accepted outcome", set(found, "outcome", `"no_head"`), ReasonReceiptNotAccepted},
		{"seq not accepted", set(base, "seq", "1"), ReasonReceiptSeq},
		{"wrong stream", set(base, "stream", `"other"`), ReasonReceiptMismatch},
		{"wrong idempotency_key", set(base, "idempotency_key", `"other"`), ReasonReceiptMismatch},
		{"wrong failure_model", set(base, "failure_model", `"none"`), ReasonReceiptMismatch},
		{"invented body_status", set(base, "failure_model", `"survives_power_loss","body_status":"verified"`), ReasonReceiptMismatch},
		{"conflict current not an accepted head", set(conflict, "current", fmt.Sprintf(`{"stream":%q,"record_id":"a","revision":2,"digest":%q}`, stream, d1)), ReasonReceiptMismatch},
		{"found embedded key mismatch", set(found, "idempotency_key", `"other"`), ReasonReconciliationMismatch},
		{"not_found for an accepted key", set(notFound, "idempotency_key", `"k2"`), ReasonReconciliationMismatch},
		{"coverage start after attempt", set(notFound, "window_covers_from", `"2026-10-20T00:00:03.000001Z"`), ReasonCoverage},
		{"coverage until before attempt", set(notFound, "window_covers_until", `"2026-10-20T00:00:02.999999Z"`), ReasonCoverage},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := reasonOf(CheckReceipt(node(t, wire.KindWriteReceipt, c.doc), lines)); got != c.want {
				t.Fatalf("%q, want %q", got, c.want)
			}
		})
	}
}

func TestProtection(t *testing.T) {
	bodyDig := "sha256:" + strings.Repeat("b", 64)
	r1 := set(record("a", 1, ""), "payload_contract", fmt.Sprintf(`"contract: example/v0","body":{"digest":%q,"length":42,"media_type":"application/octet-stream"}`, bodyDig))
	d1 := digestOf(t, r1)
	target := fmt.Sprintf(`{"stream":%q,"record_id":"a","revision":1,"digest":%q}`, stream, d1)

	makeReq := func(key, action, scope, tgt, extra, obs string) string {
		if extra != "" {
			extra = "," + extra
		}
		return fmt.Sprintf(`{"v":1,"stream":%q,"idempotency_key":%q,"action":%q,"scope":%q,"target":%s,"reason":"legal","observed_at":%q%s}`, stream, key, action, scope, tgt, obs, extra)
	}
	makeEntry := func(seq int, typ, atStr, req string) string {
		return fmt.Sprintf(`{"v":1,"seq":%d,"group":%d,"group_size":1,"stream":%q,"type":%q,"at":%q,"request":%s,"request_digest":%q}`, seq, seq, stream, typ, atStr, req, digestOf(t, req))
	}

	tombRev := makeReq("h1", "tombstone", "revision", target, "", at(2))
	untombRev := makeReq("u1", "restore", "revision", target, `"restores":3`, at(3))
	tombBody := makeReq("hb", "tombstone", "body", target, fmt.Sprintf(`"body":{"digest":%q,"length":42}`, bodyDig), at(2))
	tombWrongBody := makeReq("hwb", "tombstone", "body", target, fmt.Sprintf(`"body":{"digest":%q,"length":99}`, bodyDig), at(2))
	tombFutureObs := makeReq("hfo", "tombstone", "revision", target, "", at(5))
	foreignTarget := fmt.Sprintf(`{"stream":"other","record_id":"a","revision":1,"digest":%q}`, d1)
	tombForeignTarget := makeReq("hft", "tombstone", "revision", foreignTarget, "", at(2))
	untombWrongScope := makeReq("uws", "restore", "record", target, `"restores":3`, at(3))
	untombOldObs := makeReq("uoo", "restore", "revision", target, `"restores":3`, at(1))

	for _, c := range []struct {
		name  string
		lines []string
		want  string
	}{
		{"tombstone and restore revision", []string{opened(1), content(t, "appended", 2, r1), makeEntry(3, "tombstoned", at(3), tombRev), makeEntry(4, "untombstoned", at(4), untombRev)}, ""},
		{"tombstone body with extra record body fields", []string{opened(1), content(t, "appended", 2, r1), makeEntry(3, "tombstoned", at(3), tombBody)}, ""},
		{"request_digest mismatch", []string{opened(1), content(t, "appended", 2, r1), set(makeEntry(3, "tombstoned", at(3), tombRev), "request_digest", `"sha256:`+strings.Repeat("0", 64)+`"`)}, ReasonRequestDigest},
		{"operation_type mismatch", []string{opened(1), content(t, "appended", 2, r1), makeEntry(3, "untombstoned", at(3), tombRev)}, ReasonOperationType},
		{"accepted_key_reuse against content key", []string{opened(1), content(t, "appended", 2, r1), makeEntry(3, "tombstoned", at(3), makeReq("k2", "tombstone", "revision", target, "", at(2)))}, ReasonAcceptedKeyReuse},
		{"target_not_found foreign target", []string{opened(1), content(t, "appended", 2, r1), makeEntry(3, "tombstoned", at(3), tombForeignTarget)}, ReasonTargetNotFound},
		{"body_mismatch wrong length", []string{opened(1), content(t, "appended", 2, r1), makeEntry(3, "tombstoned", at(3), tombWrongBody)}, ReasonBodyMismatch},
		{"observation_order observed_at after entry at", []string{opened(1), content(t, "appended", 2, r1), makeEntry(3, "tombstoned", at(3), tombFutureObs)}, ReasonObservationOrder},
		{"hold_not_active restore before tombstone", []string{opened(1), content(t, "appended", 2, r1), makeEntry(3, "untombstoned", at(3), untombRev)}, ReasonHoldNotActive},
		{"hold_mismatch wrong scope", []string{opened(1), content(t, "appended", 2, r1), makeEntry(3, "tombstoned", at(3), tombRev), makeEntry(4, "untombstoned", at(4), untombWrongScope)}, ReasonHoldMismatch},
		{"observation_order restore before hold observed_at", []string{opened(1), content(t, "appended", 2, r1), makeEntry(3, "tombstoned", at(3), tombRev), makeEntry(4, "untombstoned", at(4), untombOldObs)}, ReasonObservationOrder},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := window(t)
			if got := ingestAll(t, s, c.lines...); got != c.want {
				t.Fatalf("%q, want %q", got, c.want)
			}
			if c.want == "" && s.Partial() {
				t.Fatalf("expected non-partial State")
			}
		})
	}

	// Request step: accept, replay, conflict, and Blocked checks.
	s := window(t)
	if got := ingestAll(t, s, opened(1), content(t, "appended", 2, r1)); got != "" {
		t.Fatal(got)
	}
	p1 := Pin{Stream: stream, RecordID: "a", Revision: 1, Digest: d1}
	rcpt1, err := s.Request(node(t, wire.KindProtectionRequest, tombRev), at(3))
	if err != nil {
		t.Fatalf("Request tombstone: %v", err)
	}
	if holds := s.ActiveHolds(); len(holds) != 1 || holds[0] != 3 {
		t.Fatalf("ActiveHolds after tombstone: %v, want [3]", holds)
	}
	if b, err := s.Blocked(p1); err != nil || !b {
		t.Fatalf("Blocked(p1): %v, %v, want true", b, err)
	}
	// Replay of identical request within window returns identical receipt without appending.
	replay, err := s.Request(node(t, wire.KindProtectionRequest, tombRev), at(4))
	if err != nil || !Equal(replay, rcpt1) || s.Len() != 3 {
		t.Fatalf("Request replay: err=%v equal=%v len=%d", err, Equal(replay, rcpt1), s.Len())
	}
	// Different request under same live key returns idempotency_conflict receipt.
	diffReq := makeReq("h1", "tombstone", "record", target, "", at(2))
	conflictRcpt, err := s.Request(node(t, wire.KindProtectionRequest, diffReq), at(4))
	if err != nil || conflictRcpt.Get("outcome").Str != ReasonIdempotencyConflict || s.Len() != 3 {
		t.Fatalf("Request conflict: err=%v rcpt=%s len=%d", err, wire.Encode(conflictRcpt), s.Len())
	}
	// Foreign stream request with same key does not replay; refuses target_not_found.
	foreignStreamReq := set(tombRev, "stream", `"other"`)
	foreignRcpt, err := s.Request(node(t, wire.KindProtectionRequest, foreignStreamReq), at(4))
	if err != nil || foreignRcpt.Get("outcome").Str != ReasonTargetNotFound {
		t.Fatalf("Request foreign stream: err=%v rcpt=%s", err, wire.Encode(foreignRcpt))
	}
	// Restore clears active hold.
	if _, err := s.Request(node(t, wire.KindProtectionRequest, untombRev), at(5)); err != nil {
		t.Fatalf("Request restore: %v", err)
	}
	if holds := s.ActiveHolds(); len(holds) != 0 {
		t.Fatalf("ActiveHolds after restore: %v, want []", holds)
	}
	if b, err := s.Blocked(p1); err != nil || b {
		t.Fatalf("Blocked(p1) after restore: %v, %v, want false", b, err)
	}
}

func TestRetirement(t *testing.T) {
	r1 := record("a", 1, "")
	retReq := fmt.Sprintf(`{"stream":%q,"record_id":"a","reason":"retention_expired","idempotency_key":"ret-1"}`, stream)
	retDig := digestOf(t, retReq)
	keyedRet := fmt.Sprintf(`{"v":1,"seq":3,"group":3,"group_size":1,"stream":%q,"type":"retired","at":%q,"record_id":"a","reason":"retention_expired","idempotency_key":"ret-1","request_digest":%q}`, stream, at(3), retDig)
	unkeyedRet := fmt.Sprintf(`{"v":1,"seq":3,"group":3,"group_size":1,"stream":%q,"type":"retired","at":%q,"record_id":"a","reason":"retention_expired"}`, stream, at(3))

	for _, c := range []struct {
		name  string
		lines []string
		want  string
	}{
		{"keyed retirement accepted", []string{opened(1), content(t, "appended", 2, r1), keyedRet}, ""},
		{"unkeyed retirement accepted", []string{opened(1), content(t, "appended", 2, r1), unkeyedRet}, ""},
		{"retire absent record", []string{opened(1), content(t, "appended", 2, r1), set(unkeyedRet, "record_id", `"missing"`)}, ReasonTargetNotFound},
		{"keyed retirement digest mismatch", []string{opened(1), content(t, "appended", 2, r1), set(keyedRet, "request_digest", `"sha256:`+strings.Repeat("0", 64)+`"`)}, ReasonRequestDigest},
		{"keyed retirement live key reuse", []string{opened(1), content(t, "appended", 2, r1), set(set(keyedRet, "idempotency_key", `"k2"`), "request_digest", fmt.Sprintf("%q", digestOf(t, set(retReq, "idempotency_key", `"k2"`))))}, ReasonAcceptedKeyReuse},
		{"write to retired record refused", []string{opened(1), content(t, "appended", 2, r1), unkeyedRet, content(t, "revised", 4, record("a", 2, fmt.Sprintf(`{"revision":1,"digest":%q}`, digestOf(t, r1))))}, ReasonRetiredRecord},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := window(t)
			if got := ingestAll(t, s, c.lines...); got != c.want {
				t.Fatalf("%q, want %q", got, c.want)
			}
			if c.want == "" && s.Partial() {
				t.Fatalf("expected non-partial State")
			}
		})
	}

	// State.Retire step: accept, replay, conflict, foreign stream, target_not_found.
	s := window(t)
	if got := ingestAll(t, s, opened(1), content(t, "appended", 2, r1)); got != "" {
		t.Fatal(got)
	}
	reqNode, _ := wire.ParseJSON([]byte(retReq))
	rcpt, err := s.Retire(reqNode, at(3))
	if err != nil || rcpt.Get("outcome").Str != "retired" || s.Len() != 3 {
		t.Fatalf("Retire accept: err=%v rcpt=%s len=%d", err, wire.Encode(rcpt), s.Len())
	}
	replay, err := s.Retire(reqNode, at(4))
	if err != nil || !Equal(replay, rcpt) || s.Len() != 3 {
		t.Fatalf("Retire replay: err=%v equal=%v len=%d", err, Equal(replay, rcpt), s.Len())
	}
	conflictNode, _ := wire.ParseJSON([]byte(set(retReq, "reason", `"operator"`)))
	conflictRcpt, err := s.Retire(conflictNode, at(4))
	if err != nil || conflictRcpt.Get("outcome").Str != ReasonIdempotencyConflict || s.Len() != 3 {
		t.Fatalf("Retire conflict: err=%v rcpt=%s", err, wire.Encode(conflictRcpt))
	}
	foreignNode, _ := wire.ParseJSON([]byte(set(retReq, "stream", `"other"`)))
	foreignRcpt, err := s.Retire(foreignNode, at(4))
	if err != nil || foreignRcpt.Get("outcome").Str != ReasonTargetNotFound {
		t.Fatalf("Retire foreign stream: err=%v rcpt=%s", err, wire.Encode(foreignRcpt))
	}
}

func TestBodyLifecycleAndState(t *testing.T) {
	bodyDig := "sha256:" + strings.Repeat("a", 64)
	bodyRef := fmt.Sprintf(`{"digest":%q,"length":12}`, bodyDig)
	openedBodies := set(opened(1), "stream", `"bodies"`)
	life := func(seq int, typ string, inc int) string {
		extra := ""
		if typ == "body_removal_started" {
			extra = `,"reason":"retention"`
		}
		return fmt.Sprintf(`{"v":1,"seq":%d,"group":%d,"group_size":1,"stream":"bodies","type":%q,"at":%q,"incarnation":%d,"body":%s%s}`, seq, seq, typ, at(seq), inc, bodyRef, extra)
	}
	for _, c := range []struct {
		name  string
		lines []string
		want  string
	}{
		{"publish start remove republish", []string{openedBodies, life(2, "body_published", 1), life(3, "body_removal_started", 1), life(4, "body_removed", 1), life(5, "body_published", 2)}, ""},
		{"publish nonincreasing incarnation", []string{openedBodies, life(2, "body_published", 2), life(3, "body_published", 2)}, ReasonBodyLifecycle},
		{"removal_started before publish", []string{openedBodies, life(2, "body_removal_started", 1)}, ReasonBodyLifecycle},
		{"removed before removal_started", []string{openedBodies, life(2, "body_published", 1), life(3, "body_removed", 1)}, ReasonBodyLifecycle},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := window(t)
			s.SetRemovalStream("bodies")
			if got := ingestAll(t, s, c.lines...); got != c.want {
				t.Fatalf("%q, want %q", got, c.want)
			}
		})
	}

	s := window(t)
	s.SetRemovalStream("bodies")
	if got := ingestAll(t, s, openedBodies, life(2, "body_published", 1)); got != "" {
		t.Fatal(got)
	}
	bsStep, _ := wire.ParseJSON([]byte(fmt.Sprintf(`{"body":%s,"present":true}`, bodyRef)))
	res, err := s.BodyState(bsStep)
	if err != nil || res.Get("state").Str != "available" {
		t.Fatalf("BodyState published+present: err=%v res=%s", err, wire.Encode(res))
	}
	bsAbsent, _ := wire.ParseJSON([]byte(fmt.Sprintf(`{"body":%s,"present":false}`, bodyRef)))
	res, err = s.BodyState(bsAbsent)
	if err != nil || res.Get("state").Str != "integrity_loss" {
		t.Fatalf("BodyState published+absent: err=%v res=%s", err, wire.Encode(res))
	}
	badStep, _ := wire.ParseJSON([]byte(fmt.Sprintf(`{"body":%s,"present":"unknown"}`, bodyRef)))
	if _, err := s.BodyState(badStep); reasonOf(err) != ReasonBodyObservation {
		t.Fatalf("BodyState non-bool present: %q, want %q", reasonOf(err), ReasonBodyObservation)
	}
}

func TestCheckProfile(t *testing.T) {
	p := func(compiled, enabled, strata, stratum string) string {
		return fmt.Sprintf(`{"build":{"compiled_capabilities":%s,"compiled_strata":%s},"runtime":{"enabled_capabilities":%s,"stratum":%q}}`, compiled, strata, enabled, stratum)
	}
	for _, c := range []struct{ name, doc, want string }{
		{"valid", p(`["a","b"]`, `["a"]`, `["fs"]`, "fs"), ""},
		{"none enabled", p(`["a"]`, `[]`, `["fs"]`, "fs"), ""},
		{"enabled but not compiled", p(`["a"]`, `["a","b"]`, `["fs"]`, "fs"), ReasonCapabilityNotCompiled},
		{"stratum not compiled", p(`["a"]`, `["a"]`, `["fs"]`, "mem"), ReasonStratumNotCompiled},
		{"capabilities checked before strata", p(`[]`, `["a"]`, `[]`, "fs"), ReasonCapabilityNotCompiled},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := reasonOf(CheckProfile(node(t, wire.KindStratumProfile, c.doc))); got != c.want {
				t.Fatalf("%q, want %q", got, c.want)
			}
		})
	}
}

func TestUseAndRemovalEligibility(t *testing.T) {
	s := window(t)
	r1 := record("a", 1, "")
	d1 := digestOf(t, r1)
	if got := ingestAll(t, s, opened(1), content(t, "appended", 2, r1)); got != "" {
		t.Fatal(got)
	}
	useOK := node(t, wire.KindExportRequest, fmt.Sprintf(`{"v":1,"stream":%q,"purpose":"export","closure":"full_content","protection_state":"verified","protection_cut":2,"current_cut":2,"inherited_block":false,"selection":[{"stream":%q,"record_id":"a","revision":1,"digest":%q}],"omitted":[],"bodies":[]}`, stream, stream, d1))
	res, err := s.Use(useOK)
	if err != nil || res.Get("outcome").Str != "allowed" {
		t.Fatalf("Use allowed: err=%v res=%v", err, res)
	}
	useBadPurpose := node(t, wire.KindExportRequest, fmt.Sprintf(`{"v":1,"stream":%q,"purpose":"archive","closure":"full_content","protection_state":"verified","protection_cut":2,"current_cut":2,"inherited_block":false,"selection":[],"omitted":[]}`, stream))
	res, err = s.Use(useBadPurpose)
	if err != nil || res.Get("outcome").Str != "refused" || res.Get("reason").Str != ReasonInvalidPurpose {
		t.Fatalf("Use invalid_purpose: err=%v res=%v", err, res)
	}

	// RemovalEligibility on non-designated stream refuses body_removal_stream.
	remStep := node(t, wire.KindRemovalEligibility, fmt.Sprintf(`{"store":"store.alpha","store_streams":[%q],"observed_streams":[],"body":{"digest":"sha256:%s","length":8},"incarnation":1,"mode":"start","reason":"retention","decision_time":%q,"expected_policy":{"id":"ret-1","version":"1"}}`, stream, strings.Repeat("1", 64), at(10)))
	remRes, err := s.RemovalEligibility(remStep)
	if err != nil || remRes.Get("verdict").Str != "refused" || remRes.Get("reason").Str != ReasonBodyRemovalStream {
		t.Fatalf("RemovalEligibility non-designated: err=%v res=%v", err, remRes)
	}
}
