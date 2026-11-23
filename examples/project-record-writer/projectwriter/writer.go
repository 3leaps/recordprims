package projectwriter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/3leaps/recordprims"
)

// Outcomes of a write. Every interface (library, CLI or a later control-plane
// caller) presents the same outcome and reason codes.
const (
	OutcomeAccepted      = "accepted"
	OutcomeConflict      = "conflict"
	OutcomeRefused       = "refused"
	OutcomeIndeterminate = "indeterminate"
)

// Reason codes.
const (
	ReasonUnknownKind         = "unknown_kind"
	ReasonGitAuthority        = "git_authority"
	ReasonUntrustedDescriptor = "untrusted_descriptor"
	ReasonUnknownContract     = "unknown_contract"
	ReasonUnauthorizedWriter  = "unauthorized_writer"
	ReasonBelowFloor          = "classification_below_floor"
	ReasonInvalidPayload      = "invalid_payload"
	ReasonInvalidRequest      = "invalid_request"
	ReasonWrongProject        = "wrong_project"
	ReasonSingleRevision      = "single_revision_kind"
	ReasonIdempotencyConflict = "idempotency_conflict"
	ReasonRevisionConflict    = "revision_conflict"
	ReasonNoHead              = "no_head"
	ReasonRetired             = "retired_record"
	ReasonUnprovable          = "coverage_unprovable"
	ReasonStoreUncertain      = "store_outcome_unknown"
)

// RecordClassification is the classification a write requests. Empty fields
// take the descriptor's floor.
type RecordClassification struct {
	Sensitivity string `json:"sensitivity,omitempty"`
	Retention   string `json:"retention,omitempty"`
}

// Request is one write. The same fields mean the same thing from every caller.
type Request struct {
	Project        string               `json:"project"`
	Kind           string               `json:"kind"`
	RecordID       string               `json:"record_id"`
	OperationKey   string               `json:"operation_key"`
	Entry          json.RawMessage      `json:"entry"`
	Predecessor    *recordprims.Pin     `json:"predecessor,omitempty"`
	Classification RecordClassification `json:"classification,omitempty"`
	ProducedAt     string               `json:"produced_at,omitempty"`
}

// Result is the presented outcome of a write or reconciliation.
type Result struct {
	Outcome            string               `json:"outcome"`
	Reason             string               `json:"reason,omitempty"`
	Detail             string               `json:"detail,omitempty"`
	Project            string               `json:"project"`
	Kind               string               `json:"kind"`
	Authority          Authority            `json:"authority,omitempty"`
	DescriptorRevision int                  `json:"descriptor_revision,omitempty"`
	Pin                *recordprims.Pin     `json:"pin,omitempty"`
	Current            *recordprims.Pin     `json:"current,omitempty"`
	Receipt            *recordprims.Receipt `json:"receipt,omitempty"`
	Next               string               `json:"next,omitempty"`
}

// Options configures Open and Init.
type Options struct {
	// Caller is the admitted writer's name from the project configuration.
	// It is not authenticated by this example.
	Caller string
	// Clock overrides the store clock (tests).
	Clock recordprims.Clock
	// LockTimeout bounds the wait for another process's store lock.
	LockTimeout time.Duration
	// AllowTmpfs and AllowOverlay admit those filesystems (test and CI hosts).
	AllowTmpfs   bool
	AllowOverlay bool
}

// Writer records and reads entries for one project.
type Writer struct {
	project  *Project
	registry *Registry
	store    *recordprims.Store
	streams  map[string]*recordprims.Stream // by project kind
	caller   WriterConfig
	hasUser  bool
	clock    recordprims.Clock
}

// Init creates the project's store and its streams. The store directory must
// not exist yet; its parent must.
func Init(projectPath string, opts Options) error {
	p, err := LoadProject(projectPath)
	if err != nil {
		return err
	}
	if _, err := loadRegistry(p.resolve(p.Registry.Path), p.Registry.Digest); err != nil {
		return err
	}
	dir := p.StoreDir()
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return err
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		return err
	}
	if err := recordprims.Init(dir, recordprims.InitOptions{
		IdempotencyWindow: p.window, Clock: opts.Clock, AllowTmpfs: opts.AllowTmpfs, AllowOverlay: opts.AllowOverlay,
	}); err != nil {
		return err
	}
	w, err := Open(projectPath, opts)
	if err != nil {
		return err
	}
	return w.Close()
}

// Open opens the project's registry and store. A registry whose digest does
// not match the project's pin is refused.
func Open(projectPath string, opts Options) (*Writer, error) {
	p, err := LoadProject(projectPath)
	if err != nil {
		return nil, err
	}
	reg, err := loadRegistry(p.resolve(p.Registry.Path), p.Registry.Digest)
	if err != nil {
		return nil, err
	}
	st, err := recordprims.Open(p.StoreDir(), recordprims.OpenOptions{
		Clock: opts.Clock, LockTimeout: opts.LockTimeout, AllowTmpfs: opts.AllowTmpfs, AllowOverlay: opts.AllowOverlay,
	})
	if err != nil {
		return nil, err
	}
	w := &Writer{project: p, registry: reg, store: st, streams: map[string]*recordprims.Stream{}, clock: opts.Clock}
	if w.clock == nil {
		w.clock = systemClock{}
	}
	if opts.Caller != "" {
		w.caller, w.hasUser = p.writer(opts.Caller)
	}
	for name, k := range p.Kinds {
		if k.Authority != AuthorityStore {
			continue
		}
		d, ok := reg.Current(w.descriptorKind(name))
		if !ok {
			continue
		}
		mode := recordprims.ModeRevisioned
		if d.RevisionPolicy == "single" {
			mode = recordprims.ModeSingleRevision
		}
		s, err := st.OpenStream(k.Stream, mode)
		if err != nil {
			st.Close()
			return nil, fmt.Errorf("kind %s: %w", name, err)
		}
		w.streams[name] = s
	}
	return w, nil
}

// Close closes the store.
func (w *Writer) Close() error { return w.store.Close() }

// Project is the loaded project configuration.
func (w *Writer) Project() *Project { return w.project }

// Registry is the loaded, pinned registry.
func (w *Writer) Registry() *Registry { return w.registry }

func (w *Writer) descriptorKind(kind string) string {
	if k := w.project.Kinds[kind]; k.Descriptor != "" {
		return k.Descriptor
	}
	return kind
}

// KindInfo describes an enabled kind for discovery.
type KindInfo struct {
	Kind               string          `json:"kind"`
	Authority          Authority       `json:"authority"`
	Stream             string          `json:"stream,omitempty"`
	Owner              string          `json:"owner"`
	DescriptorRevision int             `json:"descriptor_revision"`
	DescriptorDigest   string          `json:"descriptor_digest"`
	PayloadContract    string          `json:"payload_contract"`
	WriterClass        string          `json:"writer_class"`
	RevisionPolicy     string          `json:"revision_policy"`
	Floor              Classification  `json:"classification_floor"`
	Trusted            bool            `json:"trusted"`
	Example            json.RawMessage `json:"example,omitempty"`
}

// Kinds lists the project's enabled kinds.
func (w *Writer) Kinds() []KindInfo {
	names := make([]string, 0, len(w.project.Kinds))
	for n := range w.project.Kinds {
		names = append(names, n)
	}
	sort.Strings(names)
	out := []KindInfo{}
	for _, n := range names {
		if info, ok := w.kindInfo(n, false); ok {
			out = append(out, info)
		}
	}
	return out
}

// Inspect describes one kind, including its conforming example.
func (w *Writer) Inspect(kind string) (KindInfo, bool) { return w.kindInfo(kind, true) }

func (w *Writer) kindInfo(kind string, example bool) (KindInfo, bool) {
	k, ok := w.project.Kinds[kind]
	if !ok {
		return KindInfo{}, false
	}
	d, ok := w.registry.Current(w.descriptorKind(kind))
	if !ok {
		return KindInfo{}, false
	}
	info := KindInfo{
		Kind: kind, Authority: k.Authority, Stream: k.Stream, Owner: d.Owner,
		DescriptorRevision: d.Revision, DescriptorDigest: d.Digest(), PayloadContract: d.PayloadContract,
		WriterClass: d.WriterClass, RevisionPolicy: d.RevisionPolicy, Floor: d.Classification, Trusted: d.Trusted(),
	}
	if example {
		if raw, err := os.ReadFile(d.ExamplePath()); err == nil {
			info.Example = json.RawMessage(raw)
		}
	}
	return info, true
}

// prepared is a validated request ready to append.
type prepared struct {
	stream *recordprims.Stream
	record []byte
	desc   *Descriptor
	auth   Authority
}

func (w *Writer) refuse(req Request, reason, detail string) Result {
	return Result{Outcome: OutcomeRefused, Reason: reason, Detail: detail, Project: w.project.ID, Kind: req.Kind}
}

// Validate runs every check Record runs before appending, without writing.
func (w *Writer) Validate(req Request) Result {
	_, res, ok := w.prepare(req)
	if !ok {
		return res
	}
	return Result{Outcome: "valid", Project: w.project.ID, Kind: req.Kind, Authority: AuthorityStore}
}

func (w *Writer) prepare(req Request) (prepared, Result, bool) {
	if req.Project != w.project.ID {
		return prepared{}, w.refuse(req, ReasonWrongProject, "the request names another project"), false
	}
	k, ok := w.project.Kinds[req.Kind]
	if !ok {
		return prepared{}, w.refuse(req, ReasonUnknownKind, "the kind is not enabled for this project"), false
	}
	d, ok := w.registry.Current(w.descriptorKind(req.Kind))
	if !ok {
		return prepared{}, w.refuse(req, ReasonUnknownKind, "the registry has no descriptor for this kind"), false
	}
	if !d.Trusted() {
		return prepared{}, w.refuse(req, ReasonUntrustedDescriptor, d.untrustedBecause), false
	}
	if k.Authority == AuthorityGit {
		r := w.refuse(req, ReasonGitAuthority, "entries of this kind are accepted by a reviewed merge")
		r.Authority = AuthorityGit
		r.Next = "propose the entry for review on branch " + k.Git.Branch
		return prepared{}, r, false
	}
	if !w.hasUser {
		return prepared{}, w.refuse(req, ReasonUnauthorizedWriter, "the caller is not an admitted writer"), false
	}
	if w.caller.Class != d.WriterClass && w.caller.Class != "maintainer" {
		return prepared{}, w.refuse(req, ReasonUnauthorizedWriter,
			fmt.Sprintf("kind %s needs a %s writer", req.Kind, d.WriterClass)), false
	}
	sens, ret := req.Classification.Sensitivity, req.Classification.Retention
	if sens == "" {
		sens = d.Classification.Sensitivity
	}
	if ret == "" {
		ret = d.Classification.Retention
	}
	sr, sok := sensitivityRank[sens]
	rr, rok := retentionRank[ret]
	if !sok || !rok || sr < sensitivityRank[d.Classification.Sensitivity] || rr < retentionRank[d.Classification.Retention] {
		return prepared{}, w.refuse(req, ReasonBelowFloor, fmt.Sprintf("the floor for %s is %s / %s",
			req.Kind, d.Classification.Sensitivity, d.Classification.Retention)), false
	}
	check, ok := validators[d.PayloadContract]
	if !ok {
		return prepared{}, w.refuse(req, ReasonUnknownContract, "this build has no checks for "+d.PayloadContract), false
	}
	entry, err := check(req.Entry)
	if err != nil {
		return prepared{}, w.refuse(req, ReasonInvalidPayload, err.Error()), false
	}
	if !reRecordID.MatchString(req.RecordID) {
		return prepared{}, w.refuse(req, ReasonInvalidRequest, "record_id is not a valid record id"), false
	}
	if n := len([]rune(req.OperationKey)); n < 1 || n > 256 {
		return prepared{}, w.refuse(req, ReasonInvalidRequest, "operation_key must be 1 to 256 characters"), false
	}
	revision := uint64(1)
	var supersedes map[string]any
	if p := req.Predecessor; p != nil {
		if d.RevisionPolicy == "single" {
			return prepared{}, w.refuse(req, ReasonSingleRevision, "this kind does not take revisions"), false
		}
		if p.Stream != k.Stream || p.RecordID != req.RecordID || p.Revision < 1 || !reDigest.MatchString(p.Digest) {
			return prepared{}, w.refuse(req, ReasonInvalidRequest, "predecessor must be a pin of this record on this kind's stream"), false
		}
		revision = p.Revision + 1
		supersedes = map[string]any{"revision": p.Revision, "digest": p.Digest}
	}
	payload := map[string]any{
		"project": w.project.ID,
		"descriptor": map[string]any{
			"kind": d.Kind, "owner": d.Owner, "revision": d.Revision, "digest": d.Digest(),
		},
		"entry": json.RawMessage(entry),
	}
	rec := map[string]any{
		"v":                1,
		"stream":           k.Stream,
		"record_id":        req.RecordID,
		"revision":         revision,
		"payload_contract": d.PayloadContract,
		"payload":          payload,
		"provenance":       map[string]any{"actor_kind": w.caller.ActorKind, "actor": w.caller.Name},
		"classification":   map[string]any{"sensitivity": sens, "retention": ret},
	}
	if supersedes != nil {
		rec["supersedes"] = supersedes
	}
	if req.ProducedAt != "" {
		at, err := recordprims.NormalizeProducerInstant(req.ProducedAt)
		if err != nil {
			return prepared{}, w.refuse(req, ReasonInvalidRequest, "produced_at must be a UTC instant at seconds, milliseconds or microseconds"), false
		}
		rec["produced_at"] = map[string]any{"instant": at, "source": "producer_clock"}
	}
	var dec struct {
		Reverses *recordprims.Pin `json:"reverses"`
	}
	if json.Unmarshal(entry, &dec) == nil && dec.Reverses != nil && dec.Reverses.Stream == k.Stream {
		rec["links"] = []map[string]any{{"record_id": dec.Reverses.RecordID, "revision": dec.Reverses.Revision, "rel": "reverses"}}
	}
	raw, err := json.Marshal(rec)
	if err != nil {
		return prepared{}, w.refuse(req, ReasonInvalidRequest, err.Error()), false
	}
	return prepared{stream: w.streams[req.Kind], record: raw, desc: d, auth: k.Authority}, Result{}, true
}

// Record validates req and appends it. A retry with the same operation key
// and the same request returns the original receipt.
func (w *Writer) Record(ctx context.Context, req Request) (Result, error) {
	p, res, ok := w.prepare(req)
	if !ok {
		return res, nil
	}
	rc, err := p.stream.Append(ctx, recordprims.Write{Record: p.record, IdempotencyKey: req.OperationKey})
	base := Result{Project: w.project.ID, Kind: req.Kind, Authority: p.auth, DescriptorRevision: p.desc.Revision}
	if err != nil {
		if recordprims.IsIndeterminate(err) {
			base.Outcome, base.Reason = OutcomeIndeterminate, ReasonStoreUncertain
			base.Next = "reconcile with the same operation key before retrying"
			return base, nil
		}
		if errors.Is(err, recordprims.ErrSchema) {
			return w.refuse(req, ReasonInvalidRequest, err.Error()), nil
		}
		return Result{}, err
	}
	return presentReceipt(base, rc), nil
}

func presentReceipt(base Result, rc recordprims.Receipt) Result {
	base.Receipt = &rc
	switch rc.Outcome {
	case recordprims.OutcomeAppended, recordprims.OutcomeRevised:
		base.Outcome, base.Pin = OutcomeAccepted, rc.Pin
	case recordprims.OutcomeIdempotencyConflict:
		base.Outcome, base.Reason = OutcomeConflict, ReasonIdempotencyConflict
		base.Next = "the operation key was used for different content; use a new key for a new entry"
	case recordprims.OutcomeRevisionConflict:
		base.Outcome, base.Reason, base.Current = OutcomeConflict, ReasonRevisionConflict, rc.Current
		base.Next = "another revision won; review the current pin and revise from it"
	case recordprims.OutcomeNoHead:
		base.Outcome, base.Reason = OutcomeConflict, ReasonNoHead
		base.Next = "the predecessor does not exist; write revision 1 first"
	case recordprims.OutcomeRetiredRecord:
		base.Outcome, base.Reason = OutcomeRefused, ReasonRetired
	default:
		base.Outcome, base.Reason = OutcomeRefused, string(rc.Outcome)
	}
	return base
}

// Reconcile looks up an operation key after a lost reply. attemptedAt is
// when the caller sent the original write. Inside the store's idempotency
// window it returns the original receipt; otherwise acceptance cannot be
// established and the outcome is indeterminate.
func (w *Writer) Reconcile(kind, operationKey string, attemptedAt time.Time) (Result, error) {
	base := Result{Project: w.project.ID, Kind: kind}
	s, ok := w.streams[kind]
	if !ok {
		base.Outcome, base.Reason = OutcomeRefused, ReasonUnknownKind
		return base, nil
	}
	base.Authority = AuthorityStore
	rc, err := s.Reconcile(operationKey, attemptedAt)
	if err != nil {
		return Result{}, err
	}
	if rc.Result != "found" {
		base.Outcome, base.Reason = OutcomeIndeterminate, rc.Reason
		if base.Reason == "" {
			base.Reason = ReasonUnprovable
		}
		base.Next = "acceptance cannot be established from the store; check history for the entry before writing it again"
		return base, nil
	}
	var receipt recordprims.Receipt
	if err := json.Unmarshal(rc.Receipt, &receipt); err != nil {
		return Result{}, err
	}
	return presentReceipt(base, receipt), nil
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

// Hold places an operator hold on one revision. Held revisions are not served.
// Only a maintainer may place a hold.
func (w *Writer) Hold(ctx context.Context, pin recordprims.Pin, operationKey, evidenceRef, evidenceDigest string) (recordprims.ProtectionReceipt, error) {
	if !w.hasUser || w.caller.Class != "maintainer" {
		return recordprims.ProtectionReceipt{}, fmt.Errorf("%s: only a maintainer may place a hold", ReasonUnauthorizedWriter)
	}
	var s *recordprims.Stream
	for name, k := range w.project.Kinds {
		if k.Stream == pin.Stream {
			s = w.streams[name]
		}
	}
	if s == nil {
		return recordprims.ProtectionReceipt{}, fmt.Errorf("%s: no kind of this project uses stream %s", ReasonUnknownKind, pin.Stream)
	}
	observed, err := recordprims.ProducerInstant(w.clock.Now().UTC().Truncate(time.Microsecond))
	if err != nil {
		return recordprims.ProtectionReceipt{}, err
	}
	req, err := json.Marshal(map[string]any{
		"v": 1, "stream": pin.Stream, "idempotency_key": operationKey, "action": "tombstone",
		"target": pin, "scope": "revision", "reason": "operator_hold",
		"evidence":    map[string]any{"ref": evidenceRef, "digest": evidenceDigest},
		"observed_at": observed,
		"provenance":  map[string]any{"actor_kind": w.caller.ActorKind, "actor": w.caller.Name},
	})
	if err != nil {
		return recordprims.ProtectionReceipt{}, err
	}
	return s.Protect(ctx, recordprims.ProtectionRequest{Raw: req})
}
