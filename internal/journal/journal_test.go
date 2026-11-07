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
	req := func(obs string) string {
		return fmt.Sprintf(`{"v":1,"seq":2,"group":2,"group_size":1,"stream":%q,"type":"tombstoned","at":%q,"request":{"observed_at":%q},"request_digest":"sha256:%s"}`, stream, at(2), obs, strings.Repeat("0", 64))
	}
	if got := ingestAll(t, window(t), opened(1), req("2026-10-20T00:00:01.000000Z")); got != "" {
		t.Fatalf("canonical observed_at: %q", got)
	}
	if got := ingestAll(t, window(t), opened(1), req("2026-10-20T00:00:01.0000000Z")); got != ReasonInstantSpelling {
		t.Fatalf("seven fractional digits: %q", got)
	}
}

func TestIngestBodyRemovalPlacement(t *testing.T) {
	life := func(seq int, strm, typ string) string {
		return fmt.Sprintf(`{"v":1,"seq":%d,"group":%d,"group_size":1,"stream":%q,"type":%q,"at":%q,"body":{"digest":"sha256:%s","length":8}}`, seq, seq, strm, typ, at(seq), strings.Repeat("1", 64))
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
	if got := ingestAll(t, s, content(t, "appended", 2, record("a", 1, ""))); got != "" || !s.Partial() {
		t.Fatalf("appended: %q partial=%v, want partial", got, s.Partial())
	}
	c := window(t)
	if got := ingestAll(t, c, set(opened(1), "failure_model", `"survives_power_loss","continues":{}`)); got != "" || !c.Partial() {
		t.Fatalf("continuation: %q partial=%v, want partial", got, c.Partial())
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
