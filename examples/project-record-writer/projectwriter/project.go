// Package projectwriter records project decisions and updates in a
// recordprims store and reads them back.
//
// It is an example of an application built on the public recordprims API. It
// carries no compatibility promise and is not a release artifact.
//
// The package owns everything above the store: project configuration and
// stream mapping, the type registry and descriptor lookup, payload
// validation, writer policy, receipt presentation, lookup, history, Markdown
// rendering and a rebuildable search view. The store supplies idempotent
// append with receipts, conflicts, reconciliation, revisions, holds and
// release.
package projectwriter

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

// Authority names the system that accepts entries of a configured kind.
type Authority string

const (
	// AuthorityStore: the recordprims store accepts the entry.
	AuthorityStore Authority = "recordprims"
	// AuthorityGit: a reviewed merge to the designated branch accepts the
	// entry (see the gitreview package).
	AuthorityGit Authority = "git"
)

// FileRef names a file by path (relative to the referring file) and sha256.
type FileRef struct {
	Path   string `json:"path"`
	Digest string `json:"digest"`
}

// KindConfig maps an enabled kind of a project to its stream and authority.
type KindConfig struct {
	// Descriptor is the registry kind this project kind uses; empty means
	// the project kind's own name.
	Descriptor string    `json:"descriptor,omitempty"`
	Stream     string    `json:"stream,omitempty"`
	Authority  Authority `json:"authority"`
	Git        *GitRoute `json:"git,omitempty"`
}

// GitRoute is where a Git-accepted kind lives in its repository.
type GitRoute struct {
	Branch string `json:"branch"`
	Path   string `json:"path"`
}

// WriterConfig is a writer admitted by the project. The example takes the
// caller's name from local configuration and does not authenticate it.
type WriterConfig struct {
	Name      string `json:"name"`
	Class     string `json:"class"`      // maintainer, agent or external
	ActorKind string `json:"actor_kind"` // human, agent or process
}

// Project is a project configuration file.
type Project struct {
	ID                string                `json:"project"`
	Registry          FileRef               `json:"registry"`
	Store             string                `json:"store"`
	SearchView        string                `json:"search_view"`
	IdempotencyWindow string                `json:"idempotency_window"`
	Kinds             map[string]KindConfig `json:"kinds"`
	Writers           []WriterConfig        `json:"writers"`

	dir    string
	window time.Duration
}

var (
	reProjectID = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	reStream    = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)
	reKind      = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)
)

// LoadProject reads and checks a project configuration file. Relative paths
// in it are resolved against the file's directory.
func LoadProject(path string) (*Project, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var p Project
	if err := decodeStrict(raw, &p); err != nil {
		return nil, fmt.Errorf("project %s: %w", path, err)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	p.dir = filepath.Dir(abs)
	if !reProjectID.MatchString(p.ID) {
		return nil, fmt.Errorf("project %s: invalid project id", path)
	}
	if p.Store == "" || p.Registry.Path == "" || !reDigest.MatchString(p.Registry.Digest) {
		return nil, fmt.Errorf("project %s: store and a registry path and digest are required", path)
	}
	p.window, err = time.ParseDuration(p.IdempotencyWindow)
	if err != nil || p.window <= 0 {
		return nil, fmt.Errorf("project %s: idempotency_window must be a positive duration", path)
	}
	if len(p.Kinds) == 0 {
		return nil, fmt.Errorf("project %s: no kinds enabled", path)
	}
	streams := map[string]string{}
	for name, k := range p.Kinds {
		if !reKind.MatchString(name) {
			return nil, fmt.Errorf("project %s: invalid kind %q", path, name)
		}
		switch k.Authority {
		case AuthorityStore:
			if !reStream.MatchString(k.Stream) || k.Git != nil {
				return nil, fmt.Errorf("project %s: kind %q needs a valid stream and no git route", path, name)
			}
			if other, dup := streams[k.Stream]; dup {
				return nil, fmt.Errorf("project %s: kinds %q and %q share stream %q", path, other, name, k.Stream)
			}
			streams[k.Stream] = name
		case AuthorityGit:
			if k.Git == nil || k.Git.Branch == "" || k.Git.Path == "" || k.Stream != "" {
				return nil, fmt.Errorf("project %s: kind %q needs a git route and no stream", path, name)
			}
		default:
			return nil, fmt.Errorf("project %s: kind %q has unknown authority %q", path, name, k.Authority)
		}
	}
	seen := map[string]bool{}
	for _, w := range p.Writers {
		if w.Name == "" || seen[w.Name] {
			return nil, fmt.Errorf("project %s: writer names must be unique and non-empty", path)
		}
		seen[w.Name] = true
		if _, ok := classRank[w.Class]; !ok {
			return nil, fmt.Errorf("project %s: writer %q has unknown class %q", path, w.Name, w.Class)
		}
		if w.ActorKind != "human" && w.ActorKind != "agent" && w.ActorKind != "process" {
			return nil, fmt.Errorf("project %s: writer %q has unknown actor_kind %q", path, w.Name, w.ActorKind)
		}
	}
	return &p, nil
}

// resolve returns rel resolved against the project file's directory.
func (p *Project) resolve(rel string) string {
	if filepath.IsAbs(rel) {
		return rel
	}
	return filepath.Join(p.dir, filepath.FromSlash(rel))
}

// StoreDir is the absolute store directory.
func (p *Project) StoreDir() string { return p.resolve(p.Store) }

// SearchViewPath is the absolute path of the rebuildable search view.
func (p *Project) SearchViewPath() string { return p.resolve(p.SearchView) }

// Window is the store's idempotency window for this project.
func (p *Project) Window() time.Duration { return p.window }

func (p *Project) writer(name string) (WriterConfig, bool) {
	for _, w := range p.Writers {
		if w.Name == name {
			return w, true
		}
	}
	return WriterConfig{}, false
}

var reDigest = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

func fileDigest(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// readPinned reads path and checks its sha256 against want.
func readPinned(path, want string) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if got := fileDigest(raw); got != want {
		return nil, fmt.Errorf("%s: digest %s does not match the pinned %s", filepath.Base(path), got, want)
	}
	return raw, nil
}

// decodeStrict decodes one JSON value, refusing unknown fields and trailing data.
func decodeStrict(raw []byte, v any) error {
	dec := json.NewDecoder(bytesReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return errTrailing
	}
	return nil
}
