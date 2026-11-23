// Package gitreview shows a decision accepted by a reviewed merge instead of
// by the store: a proposal branch gives a proposal reference, the proposal is
// revalidated against the actual merge parent, and the merge commit on the
// designated branch gives the acceptance reference.
//
// It drives a local repository with the git binary. It is a fixture for the
// example: it makes no durability claim for Git, talks to no forge, and its
// references carry no signature.
package gitreview

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/3leaps/canonprims/canon"
)

// Validate checks an entry and returns its normalized form.
type Validate func(json.RawMessage) (json.RawMessage, error)

// Repo is a local repository with a designated branch and a directory for entries.
type Repo struct {
	Dir    string
	Branch string
	Path   string
	env    []string
}

// ProposalRef identifies a proposed entry: its branch, the proposal commit
// and the designated-branch commit it was proposed against.
type ProposalRef struct {
	Branch   string `json:"branch"`
	Commit   string `json:"commit"`
	Base     string `json:"base"`
	RecordID string `json:"record_id"`
	File     string `json:"file"`
}

// Revalidation is the result of checking one proposal against the current
// head of the designated branch, which becomes the merge parent. It is bound
// to the proposal commit, its file and the digest of the validated entry.
type Revalidation struct {
	OK              bool   `json:"ok"`
	Stale           bool   `json:"stale"`
	ValidatedParent string `json:"validated_parent"`
	ProposalCommit  string `json:"proposal_commit"`
	File            string `json:"file"`
	EntryDigest     string `json:"entry_digest,omitempty"`
	Reason          string `json:"reason,omitempty"`
}

// AcceptanceRef identifies the accepted entry: the merge commit on the
// designated branch, its first parent (the validated parent), the proposal
// commit, and the digest of the accepted file.
type AcceptanceRef struct {
	Branch         string `json:"branch"`
	MergeCommit    string `json:"merge_commit"`
	Parent         string `json:"parent"`
	ProposalCommit string `json:"proposal_commit"`
	File           string `json:"file"`
	Digest         string `json:"digest"`
}

var (
	reRecordID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	reBranch   = regexp.MustCompile(`^[a-z0-9][a-z0-9._/-]{0,99}$`)
)

// ErrStale means the designated branch moved after revalidation.
var ErrStale = errors.New("stale: the designated branch moved; revalidate")

// Init creates a repository at dir with one initial commit on branch.
func Init(dir, branch, entryPath string) (*Repo, error) {
	if !reBranch.MatchString(branch) {
		return nil, fmt.Errorf("invalid branch %q", branch)
	}
	cfg := filepath.Join(dir, ".gitconfig-fixture")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(cfg, nil, 0o600); err != nil {
		return nil, err
	}
	r := &Repo{Dir: filepath.Join(dir, "repo"), Branch: branch, Path: entryPath, env: append(os.Environ(),
		"GIT_CONFIG_GLOBAL="+cfg, "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=Fixture", "GIT_AUTHOR_EMAIL=fixture@example.org",
		"GIT_COMMITTER_NAME=Fixture", "GIT_COMMITTER_EMAIL=fixture@example.org",
		"GIT_AUTHOR_DATE=2026-11-23T12:00:00Z", "GIT_COMMITTER_DATE=2026-11-23T12:00:00Z",
	)}
	if err := os.MkdirAll(r.Dir, 0o700); err != nil {
		return nil, err
	}
	if _, err := r.git("init", "-q", "-b", branch); err != nil {
		return nil, err
	}
	if _, err := r.git("config", "commit.gpgsign", "false"); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(r.Dir, "README.md"), []byte("# Runbook decisions\n"), 0o600); err != nil {
		return nil, err
	}
	if _, err := r.git("add", "README.md"); err != nil {
		return nil, err
	}
	if _, err := r.git("commit", "-q", "-m", "Start the runbook"); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *Repo) git(args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = r.Dir
	cmd.Env = r.env
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

// Head is the commit at the tip of branch.
func (r *Repo) Head(branch string) (string, error) { return r.git("rev-parse", "refs/heads/"+branch) }

func (r *Repo) file(recordID string) string { return path.Join(r.Path, recordID+".json") }

// Propose writes the validated entry for recordID on a new branch from the
// designated branch's head and commits it.
func (r *Repo) Propose(branch, recordID string, entry json.RawMessage, validate Validate) (ProposalRef, error) {
	if !reBranch.MatchString(branch) || !reRecordID.MatchString(recordID) {
		return ProposalRef{}, fmt.Errorf("invalid branch or record id")
	}
	norm, err := validate(entry)
	if err != nil {
		return ProposalRef{}, fmt.Errorf("invalid entry: %w", err)
	}
	body, err := canon.Canonicalize(canon.JCS, norm, canon.DefaultLimits())
	if err != nil {
		return ProposalRef{}, err
	}
	base, err := r.Head(r.Branch)
	if err != nil {
		return ProposalRef{}, err
	}
	if _, err := r.git("checkout", "-q", "-b", branch, base); err != nil {
		return ProposalRef{}, err
	}
	defer r.git("checkout", "-q", r.Branch)
	file := r.file(recordID)
	if err := os.MkdirAll(filepath.Join(r.Dir, filepath.FromSlash(r.Path)), 0o700); err != nil {
		return ProposalRef{}, err
	}
	if err := os.WriteFile(filepath.Join(r.Dir, filepath.FromSlash(file)), append(body, '\n'), 0o600); err != nil {
		return ProposalRef{}, err
	}
	if _, err := r.git("add", file); err != nil {
		return ProposalRef{}, err
	}
	if _, err := r.git("commit", "-q", "-m", "Propose "+recordID); err != nil {
		return ProposalRef{}, err
	}
	commit, err := r.Head(branch)
	if err != nil {
		return ProposalRef{}, err
	}
	return ProposalRef{Branch: branch, Commit: commit, Base: base, RecordID: recordID, File: file}, nil
}

// Revalidate checks a proposal against the designated branch's current head,
// the commit a merge would use as its first parent. A proposal whose base is
// no longer the head is stale; it is still acceptable if the head has not
// touched the proposal's file and the proposed entry still validates.
func (r *Repo) Revalidate(p ProposalRef, validate Validate) (Revalidation, error) {
	head, err := r.Head(r.Branch)
	if err != nil {
		return Revalidation{}, err
	}
	rv := Revalidation{Stale: head != p.Base, ValidatedParent: head, ProposalCommit: p.Commit, File: p.File}
	if rv.Stale {
		changed, err := r.git("diff", "--name-only", p.Base, head, "--", p.File)
		if err != nil {
			return Revalidation{}, err
		}
		if changed != "" {
			rv.Reason = "competing_edit: the designated branch changed " + p.File + " after the proposal's base"
			return rv, nil
		}
	}
	content, err := r.git("show", p.Commit+":"+p.File)
	if err != nil {
		return Revalidation{}, err
	}
	norm, err := validate(json.RawMessage(content))
	if err != nil {
		rv.Reason = "invalid_entry: " + err.Error()
		return rv, nil
	}
	// The committed file must already be the canonical form of the
	// validated entry, so the digest names exactly what was checked.
	want, err := canon.Canonicalize(canon.JCS, norm, canon.DefaultLimits())
	if err != nil {
		return Revalidation{}, err
	}
	if string(want) != content {
		rv.Reason = "invalid_entry: the committed file is not the canonical validated entry"
		return rv, nil
	}
	if rv.EntryDigest, err = digestOf(content); err != nil {
		return Revalidation{}, err
	}
	rv.OK = true
	return rv, nil
}

func digestOf(content string) (string, error) {
	return canon.Digest(canon.JCS, []byte(content), canon.DefaultLimits(), "sha256")
}

// Accept merges a revalidated proposal into the designated branch. The
// revalidation must be for this proposal. The merge commit is built and
// checked (parents, accepted file digest) before the branch moves, and the
// branch is then advanced only if it still points at the validated parent.
func (r *Repo) Accept(p ProposalRef, rv Revalidation) (AcceptanceRef, error) {
	if !rv.OK {
		return AcceptanceRef{}, fmt.Errorf("proposal not revalidated: %s", rv.Reason)
	}
	if rv.ProposalCommit != p.Commit || rv.File != p.File {
		return AcceptanceRef{}, fmt.Errorf("the revalidation is for another proposal")
	}
	head, err := r.Head(r.Branch)
	if err != nil {
		return AcceptanceRef{}, err
	}
	if head != rv.ValidatedParent {
		return AcceptanceRef{}, ErrStale
	}
	// Build the candidate on a detached head at the validated parent.
	if _, err := r.git("checkout", "-q", "--detach", rv.ValidatedParent); err != nil {
		return AcceptanceRef{}, err
	}
	defer r.git("checkout", "-q", "-f", r.Branch)
	if _, err := r.git("merge", "-q", "--no-ff", "-m", "Accept "+p.RecordID, p.Commit); err != nil {
		r.git("merge", "--abort")
		return AcceptanceRef{}, err
	}
	candidate, err := r.git("rev-parse", "HEAD")
	if err != nil {
		return AcceptanceRef{}, err
	}
	parents, err := r.git("rev-list", "--parents", "-n", "1", candidate)
	if err != nil {
		return AcceptanceRef{}, err
	}
	f := strings.Fields(parents)
	if len(f) != 3 || f[1] != rv.ValidatedParent || f[2] != p.Commit {
		return AcceptanceRef{}, fmt.Errorf("candidate parents %v do not match the validated parent and proposal", f[1:])
	}
	content, err := r.git("show", candidate+":"+p.File)
	if err != nil {
		return AcceptanceRef{}, err
	}
	digest, err := digestOf(content)
	if err != nil {
		return AcceptanceRef{}, err
	}
	if digest != rv.EntryDigest {
		return AcceptanceRef{}, fmt.Errorf("the merged file differs from the validated entry")
	}
	// Advance the designated branch only from the validated parent.
	if _, err := r.git("update-ref", "refs/heads/"+r.Branch, candidate, rv.ValidatedParent); err != nil {
		return AcceptanceRef{}, ErrStale
	}
	return AcceptanceRef{Branch: r.Branch, MergeCommit: candidate, Parent: f[1], ProposalCommit: f[2], File: p.File, Digest: digest}, nil
}
