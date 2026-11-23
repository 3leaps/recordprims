package projectwriter

import (
	"encoding/json"
	"fmt"
	"regexp"
	"unicode/utf8"

	"github.com/3leaps/recordprims"
)

// validator checks one payload contract's entry and returns its normalized
// JSON: instants in the canonical spelling, unknown members refused.
type validator func(raw json.RawMessage) (json.RawMessage, error)

// validators maps each payload contract this build understands to its typed
// checks. The schemas in the registry describe the same rules; a make target
// validates the registry's examples against those schemas.
var validators = map[string]validator{
	"contract: project-decision/v1": validateDecision,
	"contract: project-update/v1":   validateUpdate,
	"contract: harbor-demo-risk/v1": validateRisk,
}

// Reference is an exact reference to a document, run, evidence or record.
type Reference struct {
	Kind   string `json:"kind"`
	Ref    string `json:"ref"`
	Digest string `json:"digest,omitempty"`
}

// Authority is who made a decision.
type DecisionAuthority struct {
	Name string `json:"name"`
	Role string `json:"role"`
}

// EvidenceAbsent states why a decision has no evidence.
type EvidenceAbsent struct {
	Reason string `json:"reason"`
}

// Decision is the contract: project-decision/v1 entry.
type Decision struct {
	Title          string            `json:"title"`
	Decision       string            `json:"decision"`
	Rationale      string            `json:"rationale"`
	Authority      DecisionAuthority `json:"authority"`
	Evidence       []Reference       `json:"evidence,omitempty"`
	EvidenceAbsent *EvidenceAbsent   `json:"evidence_absent,omitempty"`
	DecidedAt      string            `json:"decided_at"`
	Reverses       *recordprims.Pin  `json:"reverses,omitempty"`
}

// Update is the contract: project-update/v1 entry.
type Update struct {
	Summary    string      `json:"summary"`
	Status     string      `json:"status"`
	ObservedAt string      `json:"observed_at"`
	References []Reference `json:"references,omitempty"`
}

// Risk is the project-defined contract: harbor-demo-risk/v1 entry.
type Risk struct {
	Title      string `json:"title"`
	Likelihood string `json:"likelihood"`
	Impact     string `json:"impact"`
	Owner      string `json:"owner"`
	Mitigation string `json:"mitigation"`
}

var (
	reRecordID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
	reStreamID = reStream
)

// invalid is a payload refusal with a field path, never field content.
type invalid struct{ field, rule string }

func (e *invalid) Error() string { return e.field + ": " + e.rule }

func text(field, v string, max int) error {
	if v == "" || !utf8.ValidString(v) || utf8.RuneCountInString(v) > max {
		return &invalid{field, fmt.Sprintf("must be 1 to %d characters", max)}
	}
	return nil
}

func instant(field string, v *string) error {
	c, err := recordprims.NormalizeProducerInstant(*v)
	if err != nil {
		return &invalid{field, "must be a UTC instant at seconds, milliseconds or microseconds"}
	}
	*v = c
	return nil
}

func references(field string, refs []Reference, present bool) error {
	if present && (len(refs) < 1 || len(refs) > 32) {
		return &invalid{field, "must list 1 to 32 references"}
	}
	for i, r := range refs {
		f := fmt.Sprintf("%s[%d]", field, i)
		switch r.Kind {
		case "document", "run", "evidence", "record":
		default:
			return &invalid{f + ".kind", "must be document, run, evidence or record"}
		}
		if err := text(f+".ref", r.Ref, 256); err != nil {
			return err
		}
		if r.Digest != "" && !reDigest.MatchString(r.Digest) {
			return &invalid{f + ".digest", "must be sha256:<64 hex>"}
		}
	}
	return nil
}

func hasMember(raw json.RawMessage, name string) bool {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return false
	}
	_, ok := m[name]
	return ok
}

func validateDecision(raw json.RawMessage) (json.RawMessage, error) {
	var d Decision
	if err := decodeStrict(raw, &d); err != nil {
		return nil, &invalid{"entry", "is not a decision: " + err.Error()}
	}
	for _, c := range []struct {
		f, v string
		n    int
	}{{"title", d.Title, 200}, {"decision", d.Decision, 4000}, {"rationale", d.Rationale, 8000},
		{"authority.name", d.Authority.Name, 128}, {"authority.role", d.Authority.Role, 128}} {
		if err := text(c.f, c.v, c.n); err != nil {
			return nil, err
		}
	}
	hasEvidence := hasMember(raw, "evidence")
	if hasEvidence == (d.EvidenceAbsent != nil) {
		return nil, &invalid{"evidence", "give evidence or evidence_absent, not both or neither"}
	}
	if err := references("evidence", d.Evidence, hasEvidence); err != nil {
		return nil, err
	}
	if d.EvidenceAbsent != nil {
		if err := text("evidence_absent.reason", d.EvidenceAbsent.Reason, 500); err != nil {
			return nil, err
		}
	}
	if err := instant("decided_at", &d.DecidedAt); err != nil {
		return nil, err
	}
	if p := d.Reverses; p != nil {
		if !reStreamID.MatchString(p.Stream) || !reRecordID.MatchString(p.RecordID) || p.Revision < 1 ||
			!reDigest.MatchString(p.Digest) {
			return nil, &invalid{"reverses", "must be an exact record pin"}
		}
	}
	return json.Marshal(d)
}

func validateUpdate(raw json.RawMessage) (json.RawMessage, error) {
	var u Update
	if err := decodeStrict(raw, &u); err != nil {
		return nil, &invalid{"entry", "is not a project update: " + err.Error()}
	}
	if err := text("summary", u.Summary, 4000); err != nil {
		return nil, err
	}
	switch u.Status {
	case "on_track", "at_risk", "blocked", "done":
	default:
		return nil, &invalid{"status", "must be on_track, at_risk, blocked or done"}
	}
	if err := instant("observed_at", &u.ObservedAt); err != nil {
		return nil, err
	}
	if err := references("references", u.References, hasMember(raw, "references")); err != nil {
		return nil, err
	}
	return json.Marshal(u)
}

func validateRisk(raw json.RawMessage) (json.RawMessage, error) {
	var r Risk
	if err := decodeStrict(raw, &r); err != nil {
		return nil, &invalid{"entry", "is not a risk: " + err.Error()}
	}
	for _, c := range []struct{ f, v string }{{"likelihood", r.Likelihood}, {"impact", r.Impact}} {
		if c.v != "low" && c.v != "medium" && c.v != "high" {
			return nil, &invalid{c.f, "must be low, medium or high"}
		}
	}
	for _, c := range []struct {
		f, v string
		n    int
	}{{"title", r.Title, 200}, {"owner", r.Owner, 128}, {"mitigation", r.Mitigation, 4000}} {
		if err := text(c.f, c.v, c.n); err != nil {
			return nil, err
		}
	}
	return json.Marshal(r)
}

// NormalizeEntry runs the typed checks for payloadContract on raw and returns
// the normalized entry. Callers with another authority (such as a reviewed
// Git merge) use it to apply the same checks as Record.
func NormalizeEntry(payloadContract string, raw json.RawMessage) (json.RawMessage, error) {
	check, ok := validators[payloadContract]
	if !ok {
		return nil, fmt.Errorf("%s: %s", ReasonUnknownContract, payloadContract)
	}
	return check(raw)
}
