package gitreview

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	pw "github.com/3leaps/recordprims/examples/project-record-writer/projectwriter"
)

func decisionValidator(raw json.RawMessage) (json.RawMessage, error) {
	return pw.NormalizeEntry("contract: project-decision/v1", raw)
}

func decision(t *testing.T, title string) json.RawMessage {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "registry", "decision", "valid-no-evidence.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	json.Unmarshal(b, &m)
	m["title"] = title
	out, _ := json.Marshal(m)
	return out
}

// E7: a proposal reference, revalidation of a stale proposal against the
// actual merge parent, and an acceptance reference at the merge commit.
func TestReviewedDecisionFixture(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Fatal("the git fixture needs the git binary on PATH")
	}
	r, err := Init(t.TempDir(), "main", "decisions")
	if err != nil {
		t.Fatal(err)
	}

	// Two proposals from the same base.
	night, err := r.Propose("propose/night-arrivals", "dec-night-arrivals", decision(t, "Pause night arrivals"), decisionValidator)
	if err != nil {
		t.Fatal(err)
	}
	pilot, err := r.Propose("propose/pilot-boarding", "dec-pilot-boarding", decision(t, "Board pilots at the outer buoy"), decisionValidator)
	if err != nil {
		t.Fatal(err)
	}
	if night.Base != pilot.Base || night.Commit == pilot.Commit {
		t.Fatalf("proposals = %+v %+v", night, pilot)
	}

	// The first is fresh and merges with its base as the first parent.
	rv, err := r.Revalidate(night, decisionValidator)
	if err != nil || !rv.OK || rv.Stale || rv.ValidatedParent != night.Base {
		t.Fatalf("revalidate night = %+v, %v", rv, err)
	}
	acc, err := r.Accept(night, rv)
	if err != nil || acc.Parent != night.Base || acc.ProposalCommit != night.Commit {
		t.Fatalf("accept night = %+v, %v", acc, err)
	}

	// The second is now stale. Revalidation is against the actual merge
	// parent (the first merge), not its original base; it still passes
	// because the first merge did not touch its file.
	rv2, err := r.Revalidate(pilot, decisionValidator)
	if err != nil || !rv2.OK || !rv2.Stale || rv2.ValidatedParent != acc.MergeCommit {
		t.Fatalf("revalidate stale pilot = %+v, %v", rv2, err)
	}
	// Accepting against an outdated revalidation is refused.
	if _, err := r.Accept(pilot, rv); !errors.Is(err, ErrStale) {
		t.Fatalf("accept with an outdated revalidation = %v", err)
	}
	acc2, err := r.Accept(pilot, rv2)
	if err != nil || acc2.Parent != acc.MergeCommit {
		t.Fatalf("accept pilot = %+v, %v", acc2, err)
	}
	if !strings.HasPrefix(acc2.Digest, "sha256:") || acc2.File != "decisions/dec-pilot-boarding.json" {
		t.Fatalf("acceptance = %+v", acc2)
	}

	// Two proposals of the same record: after one merges, the other is a
	// competing edit and is not accepted without a new proposal.
	a, _ := r.Propose("propose/fuel-a", "dec-fuel", decision(t, "Second fuel supplier: Alpha"), decisionValidator)
	b, _ := r.Propose("propose/fuel-b", "dec-fuel", decision(t, "Second fuel supplier: Beta"), decisionValidator)
	rva, _ := r.Revalidate(a, decisionValidator)
	if _, err := r.Accept(a, rva); err != nil {
		t.Fatal(err)
	}
	rvb, err := r.Revalidate(b, decisionValidator)
	if err != nil || rvb.OK || !strings.HasPrefix(rvb.Reason, "competing_edit") {
		t.Fatalf("revalidate competing proposal = %+v, %v", rvb, err)
	}
	if _, err := r.Accept(b, rvb); err == nil {
		t.Fatal("a competing proposal was accepted")
	}

	// An invalid entry is refused at proposal time.
	if _, err := r.Propose("propose/bad", "dec-bad", json.RawMessage(`{"title":"x"}`), decisionValidator); err == nil {
		t.Fatal("an invalid entry was proposed")
	}
}
