package projectwriter

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"sort"

	"github.com/3leaps/canonprims/canon"
)

// Classification is a classification floor in the registry's vocabulary
// (the sensitivity and retention-lifecycle classifier keys).
type Classification struct {
	Sensitivity string `json:"sensitivity"`
	Retention   string `json:"retention-lifecycle"`
}

// Descriptor describes one revision of an entry kind.
type Descriptor struct {
	Kind              string         `json:"kind"`
	Owner             string         `json:"owner"`
	Revision          int            `json:"descriptor_revision"`
	EntryContract     string         `json:"entry_contract"`
	Schema            FileRef        `json:"schema"`
	Carrier           string         `json:"carrier"`
	RevisionPolicy    string         `json:"revision_policy"`
	WriterClass       string         `json:"writer_class"`
	Classification    Classification `json:"classification"`
	StreamVolatility  string         `json:"stream_volatility"`
	Example           string         `json:"example"`
	Retired           bool           `json:"retired,omitempty"`
	digest            string
	schemaRaw         []byte
	untrustedBecause  string
	registryDirectory string
}

// Digest is the JCS sha256 of the descriptor as listed in the registry. A
// record carries it, so a later reader can find this exact revision.
func (d *Descriptor) Digest() string { return d.digest }

// Trusted reports whether the descriptor's schema file matched its pinned digest.
func (d *Descriptor) Trusted() bool { return d.untrustedBecause == "" }

// SchemaJSON is the pinned payload schema.
func (d *Descriptor) SchemaJSON() []byte { return d.schemaRaw }

// ExamplePath is the absolute path of the descriptor's conforming example.
func (d *Descriptor) ExamplePath() string {
	return filepath.Join(d.registryDirectory, filepath.FromSlash(d.Example))
}

// RecordContract is the contract of the stored payload every record carries
// (the wrapper around an entry), with its pinned schema.
type RecordContract struct {
	PayloadContract string  `json:"payload_contract"`
	Schema          FileRef `json:"schema"`
	schemaRaw       []byte
}

// SchemaJSON is the pinned wrapper schema.
func (c *RecordContract) SchemaJSON() []byte { return c.schemaRaw }

// Registry is a versioned list of descriptors. It keeps earlier descriptor
// revisions so records written under them can still be checked; the newest
// unretired revision of each kind is the one used for new writes. Record
// names the contract of the stored payload; each descriptor names the
// contract of the entry inside it.
type Registry struct {
	Name        string          `json:"registry"`
	Version     string          `json:"registry_version"`
	Record      *RecordContract `json:"record"`
	Descriptors []*Descriptor   `json:"descriptors"`
	digest      string
}

var (
	reContract = regexp.MustCompile(`^contract: [a-z0-9][a-z0-9-]*/v[0-9]+$`)
	reOwner    = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]*/[a-z0-9][a-z0-9.-]*$`)
)

// sensitivityRank and retentionRank order the classifier values that a
// record may carry; unknown is a deferred decision and never meets a floor.
var (
	sensitivityRank = map[string]int{
		"0-public": 0, "1-confidential": 1, "2-blinded": 2, "3-proprietary": 3,
		"4-personal": 4, "5-privileged": 5, "6-eyes-only": 6,
	}
	retentionRank = map[string]int{
		"transient": 0, "short": 1, "standard": 2, "long": 3, "legal-hold": 4,
	}
	classRank = map[string]int{"external": 0, "agent": 1, "maintainer": 2}
)

// LoadRegistry reads a registry file pinned by digest (see loadRegistry).
func LoadRegistry(path, digest string) (*Registry, error) { return loadRegistry(path, digest) }

// loadRegistry reads the registry pinned by ref and each descriptor's schema.
// A registry whose digest differs from the pin is refused as a whole. A
// descriptor whose schema file differs from its own pin is loaded but marked
// untrusted, so writes of that kind are refused.
func loadRegistry(path, digest string) (*Registry, error) {
	raw, err := readPinned(path, digest)
	if err != nil {
		return nil, fmt.Errorf("untrusted registry: %w", err)
	}
	var r Registry
	if err := decodeStrict(raw, &r); err != nil {
		return nil, fmt.Errorf("registry: %w", err)
	}
	r.digest = digest
	dir := filepath.Dir(path)
	if r.Record == nil || r.Record.PayloadContract != RecordPayloadContract ||
		!reDigest.MatchString(r.Record.Schema.Digest) || r.Record.Schema.Path == "" {
		return nil, fmt.Errorf("registry: the record contract must be %s with a pinned schema", RecordPayloadContract)
	}
	if r.Record.schemaRaw, err = readPinned(filepath.Join(dir, filepath.FromSlash(r.Record.Schema.Path)), r.Record.Schema.Digest); err != nil {
		return nil, fmt.Errorf("untrusted registry: record schema: %w", err)
	}
	if r.Record.Schema.Digest != recordSchemaDigest {
		return nil, fmt.Errorf("registry: this build's record wrapper does not implement schema %s", r.Record.Schema.Digest)
	}
	seen := map[string]bool{}
	for i, d := range r.Descriptors {
		if d == nil || !reKind.MatchString(d.Kind) || !reOwner.MatchString(d.Owner) || d.Revision < 1 ||
			!reContract.MatchString(d.EntryContract) || d.Carrier != "payload" ||
			(d.RevisionPolicy != "revisioned" && d.RevisionPolicy != "single") ||
			!knownClass(d.WriterClass) ||
			!reDigest.MatchString(d.Schema.Digest) || d.Schema.Path == "" || d.Example == "" {
			return nil, fmt.Errorf("registry: descriptor %d is malformed", i)
		}
		if _, ok := sensitivityRank[d.Classification.Sensitivity]; !ok {
			return nil, fmt.Errorf("registry: descriptor %s/%d has an unknown sensitivity floor", d.Kind, d.Revision)
		}
		if _, ok := retentionRank[d.Classification.Retention]; !ok {
			return nil, fmt.Errorf("registry: descriptor %s/%d has an unknown retention floor", d.Kind, d.Revision)
		}
		id := fmt.Sprintf("%s/%d", d.Kind, d.Revision)
		if seen[id] {
			return nil, fmt.Errorf("registry: duplicate descriptor %s", id)
		}
		seen[id] = true
		d.registryDirectory = dir
		d.digest, err = descriptorDigest(raw, i)
		if err != nil {
			return nil, err
		}
		schema, err := readPinned(filepath.Join(dir, filepath.FromSlash(d.Schema.Path)), d.Schema.Digest)
		if err != nil {
			d.untrustedBecause = err.Error()
			continue
		}
		d.schemaRaw = schema
	}
	return &r, nil
}

// descriptorDigest returns the JCS sha256 of descriptor i as it appears in
// the registry bytes.
func descriptorDigest(registryRaw []byte, i int) (string, error) {
	var doc struct {
		Descriptors []json.RawMessage `json:"descriptors"`
	}
	if err := json.Unmarshal(registryRaw, &doc); err != nil || i >= len(doc.Descriptors) {
		return "", fmt.Errorf("registry: descriptor %d: %v", i, err)
	}
	return canon.Digest(canon.JCS, doc.Descriptors[i], canon.DefaultLimits(), "sha256")
}

// Current returns the newest unretired descriptor revision of kind.
func (r *Registry) Current(kind string) (*Descriptor, bool) {
	var best *Descriptor
	for _, d := range r.Descriptors {
		if d.Kind == kind && !d.Retired && (best == nil || d.Revision > best.Revision) {
			best = d
		}
	}
	return best, best != nil
}

// Lookup returns descriptor kind/revision if its digest matches digest.
func (r *Registry) Lookup(kind string, revision int, digest string) (*Descriptor, bool) {
	for _, d := range r.Descriptors {
		if d.Kind == kind && d.Revision == revision && d.digest == digest {
			return d, true
		}
	}
	return nil, false
}

// Kinds lists the registry's kinds in name order.
func (r *Registry) Kinds() []string {
	set := map[string]bool{}
	for _, d := range r.Descriptors {
		set[d.Kind] = true
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func knownClass(c string) bool { _, ok := classRank[c]; return ok }

func bytesReader(b []byte) io.Reader { return bytes.NewReader(b) }

// errTrailing is returned when a JSON document has data after its value.
var errTrailing = errors.New("trailing data after the JSON value")
