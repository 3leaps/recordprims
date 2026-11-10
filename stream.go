package recordprims

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"slices"
	"strconv"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/3leaps/recordprims/internal/canon"
	"github.com/3leaps/recordprims/internal/journal"
	"github.com/3leaps/recordprims/internal/shape"
	"github.com/3leaps/recordprims/internal/wire"
)

// Mode is a stream's revision policy, fixed when the stream is opened.
type Mode string

const (
	ModeSingleRevision Mode = "single_revision"
	ModeRevisioned     Mode = "revisioned"
)

// Continues binds a continuation stream to a specific source stream cut.
type Continues struct {
	Capability string `json:"capability"`
	Stream     string `json:"stream"`
	CutSeq     uint64 `json:"cut_seq"`
	CutDigest  string `json:"cut_digest"`
}

// SourcePin identifies a record revision in a continuation's source stream.
type SourcePin struct {
	Capability string `json:"capability"`
	Pin        Pin    `json:"pin"`
}

// StreamOption configures OpenStream.
type StreamOption func(*streamConfig)

type streamConfig struct {
	continues *Continues
}

// WithContinues opens the stream as a continuation of c.
func WithContinues(c Continues) StreamOption {
	return func(cfg *streamConfig) {
		cp := c
		cfg.continues = &cp
	}
}

// Stream is a handle to a single durable-record/v1 stream inside a Store.
type Stream struct {
	store *Store
	id    string
	mode  Mode
}

// ID returns the stream's identifier.
func (st *Stream) ID() string { return st.id }

// Mode returns the stream's revision mode.
func (st *Stream) Mode() Mode { return st.mode }

// Write is a content write request (append or revise) on a Stream.
type Write struct {
	// Record is the raw JSON of the durable-record/v1 record document.
	Record json.RawMessage
	// IdempotencyKey is the caller's idempotency key (1..256 code points).
	IdempotencyKey string
	// Lease, when non-nil, is the publication lease for the record's external body.
	Lease *PublicationLease
	// ImportedFrom, when non-nil, binds a continuation write that re-records
	// source content to its exact source revision pin.
	ImportedFrom *SourcePin
}

// WriteOutcome is the contract outcome of a content write.
type WriteOutcome string

const (
	OutcomeAppended            WriteOutcome = "appended"
	OutcomeRevised             WriteOutcome = "revised"
	OutcomeRevisionConflict    WriteOutcome = "revision_conflict"
	OutcomeIdempotencyConflict WriteOutcome = "idempotency_conflict"
)

// Receipt is the durable-record/v1 write receipt returned by Append.
type Receipt struct {
	V              int          `json:"v"`
	Kind           string       `json:"kind"`
	Stream         string       `json:"stream"`
	IdempotencyKey string       `json:"idempotency_key"`
	Outcome        WriteOutcome `json:"outcome"`
	Seq            uint64       `json:"seq,omitempty"`
	At             string       `json:"at,omitempty"`
	Pin            *Pin         `json:"pin,omitempty"`
	BodyStatus     string       `json:"body_status,omitempty"`
	FailureModel   string       `json:"failure_model,omitempty"`
	Reason         string       `json:"reason,omitempty"`
	Current        *Pin         `json:"current,omitempty"`
}

// RetireReason is the closed vocabulary of retirement reasons.
type RetireReason string

const (
	RetireSuperseded RetireReason = "superseded"
	RetireWithdrawn  RetireReason = "withdrawn"
	RetireRetention  RetireReason = "retention"
	RetireOperator   RetireReason = "operator"
)

// RetireOption configures Stream.Retire.
type RetireOption func(*retireConfig)

type retireConfig struct {
	key string
}

// WithRetireKey supplies an idempotency key for a keyed retirement (A3).
func WithRetireKey(key string) RetireOption {
	return func(cfg *retireConfig) { cfg.key = key }
}

// RetireOutcome is the outcome of Stream.Retire.
type RetireOutcome string

const (
	RetireOutcomeRetired             RetireOutcome = "retired"
	RetireOutcomeAlreadyRetired      RetireOutcome = "already_retired"
	RetireOutcomeNoRecord            RetireOutcome = "no_record"
	RetireOutcomeTargetNotFound      RetireOutcome = "target_not_found"
	RetireOutcomeIdempotencyConflict RetireOutcome = "idempotency_conflict"
)

// RetireResult is returned by Stream.Retire.
type RetireResult struct {
	Outcome RetireOutcome   `json:"outcome"`
	Seq     uint64          `json:"seq,omitempty"`
	At      string          `json:"at,omitempty"`
	Receipt json.RawMessage `json:"receipt,omitempty"`
}

// ProtectionRequest is a tombstone or restore request on a Stream.
type ProtectionRequest struct {
	Raw json.RawMessage
}

// ProtectionReceipt is the result of Stream.Protect.
type ProtectionReceipt struct {
	Outcome string          `json:"outcome"`
	Seq     uint64          `json:"seq,omitempty"`
	At      string          `json:"at,omitempty"`
	Reason  string          `json:"reason,omitempty"`
	Raw     json.RawMessage `json:"raw,omitempty"`
}

// Reconciliation is the durable-record/v1 reconciliation result.
type Reconciliation struct {
	Result         string          `json:"result"` // "found" or "unknown"
	Reason         string          `json:"reason,omitempty"`
	Stream         string          `json:"stream"`
	IdempotencyKey string          `json:"idempotency_key"`
	AttemptedAt    string          `json:"attempted_at"`
	Receipt        json.RawMessage `json:"receipt,omitempty"`
}

// HeadInfo summarizes the latest revision of a record on a Stream.
type HeadInfo struct {
	Pin     Pin  `json:"pin"`
	Retired bool `json:"retired"`
}

// RevisionView is the redacted metadata summary of one record revision.
type RevisionView struct {
	Pin             Pin    `json:"pin"`
	Supersedes      *Pin   `json:"supersedes,omitempty"`
	PayloadContract string `json:"payload_contract"`
	HasBody         bool   `json:"has_body"`
	Retired         bool   `json:"retired"`
	At              string `json:"at"`
}

// UsePurpose is the purpose of a Stream.Release call.
type UsePurpose string

const (
	PurposeServe  UsePurpose = "serve"
	PurposeExport UsePurpose = "export"
	PurposeReuse  UsePurpose = "reuse"
)

// UseClosure specifies whether external body bytes are required and delivered.
type UseClosure string

const (
	ClosureFullContent  UseClosure = "full_content"
	ClosureMetadataOnly UseClosure = "metadata_only"
)

// UseRequest asks the store to evaluate and deliver a release at the current cut.
type UseRequest struct {
	Purpose   UsePurpose
	Selection []Pin // only for PurposeExport; empty selects all eligible for serve/reuse
	Closure   UseClosure
}

// ReleaseOutcome is the outcome of Stream.Release.
type ReleaseOutcome string

const (
	ReleaseReleased  ReleaseOutcome = "released"
	ReleasePartial   ReleaseOutcome = "released_partial"
	ReleaseUncertain ReleaseOutcome = "released_uncertain"
	ReleaseRefused   ReleaseOutcome = "refused"
)

// ReleaseResult is the caller-visible result of Stream.Release.
type ReleaseResult struct {
	Outcome            ReleaseOutcome `json:"outcome"`
	RefusalReason      string         `json:"refusal_reason,omitempty"`
	Cut                uint64         `json:"cut"`
	Selection          []Pin          `json:"selection,omitempty"`
	FramesCompleted    int            `json:"frames_completed"`
	FrameInFlight      *int           `json:"frame_in_flight,omitempty"`
	ReleasedThroughPin *Pin           `json:"released_through_pin,omitempty"`
	OmittedCount       int            `json:"omitted_count"`
	Omitted            []Pin          `json:"omitted,omitempty"`
}

// FrameKind distinguishes record frames from external body frames in ReleaseSink.
type FrameKind string

const (
	FrameKindRecord FrameKind = "record"
	FrameKindBody   FrameKind = "body"
)

// FrameHeader describes one frame delivered to a ReleaseSink.
type FrameHeader struct {
	Index  int
	Pin    Pin
	Kind   FrameKind
	Length int64
	Digest string
}

// ReleaseSink receives content frames during Stream.Release while the store lock
// is held. Its reader is revoked as soon as Frame returns.
type ReleaseSink interface {
	Frame(ctx context.Context, h FrameHeader, r io.Reader) error
}

// ErrSink is returned by Release when a ReleaseSink callback returns an error,
// without disclosing the caller error's text.
var ErrSink = errors.New("recordprims: sink_error")

type replayedStream struct {
	st        *journal.State
	jpath     string
	sdir      string
	raw       []byte
	cutOffset int64
	lastSeq   int64
	lastAt    time.Time
	mode      Mode
	continues *Continues
}

func (rep *replayedStream) appendLine(s *Store, line []byte) error {
	if err := s.appender.Append(rep.jpath, rep.cutOffset, line, false); err != nil {
		return mapJFileErr(err)
	}
	rep.raw = append(rep.raw, line...)
	rep.cutOffset = int64(len(rep.raw))
	rep.lastSeq = int64(rep.st.Len())
	return s.syncStreamIndexes(rep, rep.raw)
}

// OpenStream opens or creates a content stream on s. It refuses the store's
// designated body-removal stream (A7).
func (s *Store) OpenStream(id string, mode Mode, opts ...StreamOption) (*Stream, error) {
	if !reStream.MatchString(id) {
		return nil, fmt.Errorf("%w: invalid stream id", ErrSchema)
	}
	if mode != ModeSingleRevision && mode != ModeRevisioned {
		return nil, fmt.Errorf("%w: invalid stream mode", ErrSchema)
	}
	if id == s.profile.Runtime.BodyRemovalStream {
		return nil, fmt.Errorf("%w: %s is the designated body-removal stream", ErrSchema, id)
	}
	var cfg streamConfig
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	var contNode *wire.Node
	if cfg.continues != nil {
		c := cfg.continues
		if c.Capability != "contract: durable-record/v1" {
			return nil, ErrContinuationUnsupported
		}
		if !reStream.MatchString(c.Stream) || c.CutSeq < 1 || c.CutSeq > maxSafeInteger || !reDigest.MatchString(c.CutDigest) {
			return nil, fmt.Errorf("%w: invalid continues specification", ErrSchema)
		}
		if c.Stream == id || c.Stream == s.profile.Runtime.BodyRemovalStream {
			return nil, &ErrJournalInvalid{Reason: journal.ReasonContinuation}
		}
		seqTok := strconv.FormatUint(c.CutSeq, 10)
		contNode = &wire.Node{
			Kind: wire.Object,
			Fields: []wire.Member{
				{Name: "capability", Value: &wire.Node{Kind: wire.String, Str: c.Capability}},
				{Name: "stream", Value: &wire.Node{Kind: wire.String, Str: c.Stream}},
				{Name: "cut_seq", Value: &wire.Node{Kind: wire.Number, Token: seqTok, IsInt: true, Int: int64(c.CutSeq)}},
				{Name: "cut_digest", Value: &wire.Node{Kind: wire.String, Str: c.CutDigest}},
			},
		}
	}

	err := s.withLock(func() error {
		sdir := streamDir(id)
		exists, err := s.exists(sdir)
		if err != nil {
			return err
		}
		if !exists {
			if cfg.continues != nil {
				if err := s.verifyContinuationSource(id, cfg.continues); err != nil {
					return err
				}
			}
			now := FormatInstant(s.opts.Clock.Now())
			return createStreamJournal(s.root, s.fso, s.appender, id, string(mode), s.profile.Runtime.FailureModel, now, contNode, false)
		}
		rep, err := s.replayStream(id)
		if err != nil {
			return err
		}
		if rep.mode != mode {
			return fmt.Errorf("%w: stream mode mismatch", ErrSchema)
		}
		if (rep.continues == nil) != (cfg.continues == nil) {
			return fmt.Errorf("%w: stream continuation mismatch", ErrSchema)
		}
		if rep.continues != nil && *rep.continues != *cfg.continues {
			return fmt.Errorf("%w: stream continuation mismatch", ErrSchema)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &Stream{store: s, id: id, mode: mode}, nil
}

func (s *Store) verifyContinuationSource(dstID string, c *Continues) error {
	seen := map[string]bool{streamHash(dstID): true}
	srcExists, err := s.exists(streamDir(c.Stream))
	if err != nil {
		return err
	}
	if !srcExists {
		return ErrContinuationUnsupported
	}
	srcRep, err := s.replayStream(c.Stream)
	if err != nil {
		return &ErrJournalInvalid{Reason: journal.ReasonContinuation}
	}
	if int64(c.CutSeq) > srcRep.lastSeq {
		return &ErrJournalInvalid{Reason: journal.ReasonContinuation}
	}
	if err := journal.CheckJournal(srcRep.st.Journal()[:c.CutSeq]); err != nil {
		return &ErrJournalInvalid{Reason: journal.ReasonContinuation}
	}
	gotDig, err := srcRep.st.CutDigest(int64(c.CutSeq))
	if err != nil || gotDig != c.CutDigest {
		return &ErrJournalInvalid{Reason: journal.ReasonContinuation}
	}
	cur := srcRep.continues
	for cur != nil {
		sh := streamHash(cur.Stream)
		if seen[sh] || len(seen) > 16 {
			return &ErrJournalInvalid{Reason: journal.ReasonContinuation}
		}
		seen[sh] = true
		ancRep, err := s.replayStream(cur.Stream)
		if err != nil {
			return &ErrJournalInvalid{Reason: journal.ReasonContinuation}
		}
		cur = ancRep.continues
	}
	return nil
}

// replayStream reads and validates a content stream's journal under the store lock.
// Torn lines and incomplete final atomic groups are reported as *ErrDamagedTail.
func (s *Store) replayStream(id string) (*replayedStream, error) {
	sdir := streamDir(id)
	if sealed, err := s.exists(sdir + "/SEALED"); err != nil {
		return nil, err
	} else if sealed {
		return nil, ErrStreamSealed
	}
	jpath := streamJournalFile(id)
	raw, err := readFileVerified(s.root, jpath, s.fso, maxJournalBytes)
	if err != nil {
		return nil, ioErr("read", err, false)
	}
	nodes, cutSeq, cutOff, dmgKind, dmgBytes, err := parseStreamPrefix(raw)
	if err != nil {
		return nil, err
	}
	if dmgKind != "" {
		return nil, &ErrDamagedTail{
			Kind:              dmgKind,
			LastCutSeq:        cutSeq,
			LastCutOffset:     cutOff,
			DamagedBytesCount: dmgBytes,
		}
	}
	st, err := journal.New(s.windowNode())
	if err != nil {
		return nil, &ErrJournalInvalid{Reason: rejectReason(err)}
	}
	st.SetProfile(s.profileNode)
	var mode Mode
	var cont *Continues
	var lastAt time.Time
	for i, node := range nodes {
		if err := shape.Validate("v1/journal-entry.schema.json", node); err != nil {
			return nil, &ErrJournalInvalid{Reason: journal.ReasonShape}
		}
		if i == 0 {
			if m := node.Get("mode"); m != nil && m.Kind == wire.String {
				mode = Mode(m.Str)
			}
			if cn := node.Get("continues"); cn != nil && cn.Kind == wire.Object {
				if !s.attachContinuationChain(st, cn, map[string]bool{streamHash(id): true}) {
					return nil, &ErrJournalInvalid{Reason: journal.ReasonContinuation}
				}
				cSeq := cn.Get("cut_seq")
				var cutSeqU uint64
				if cSeq != nil && cSeq.IsInt {
					cutSeqU = uint64(cSeq.Int)
				}
				cont = &Continues{
					Capability: cn.Get("capability").Str,
					Stream:     cn.Get("stream").Str,
					CutSeq:     cutSeqU,
					CutDigest:  cn.Get("cut_digest").Str,
				}
			}
		}
		if err := st.Ingest(node); err != nil {
			return nil, &ErrJournalInvalid{Reason: rejectReason(err)}
		}
		if atNode := node.Get("at"); atNode != nil && atNode.Kind == wire.String {
			lastAt, _ = journal.ParseInstant(atNode.Str)
		}
	}
	if err := journal.CheckJournal(st.Journal()); err != nil {
		return nil, &ErrJournalInvalid{Reason: rejectReason(err)}
	}
	if st.Stream() != id {
		return nil, &ErrJournalInvalid{Reason: journal.ReasonStreamMismatch}
	}
	rep := &replayedStream{
		st:        st,
		jpath:     jpath,
		sdir:      sdir,
		raw:       raw,
		cutOffset: int64(len(raw)),
		lastSeq:   int64(st.Len()),
		lastAt:    lastAt,
		mode:      mode,
		continues: cont,
	}
	if err := s.syncStreamIndexes(rep, raw); err != nil {
		return nil, err
	}
	return rep, nil
}

func parseStreamPrefix(raw []byte) (nodes []*wire.Node, lastCutSeq uint64, lastCutOff int64, dmgKind DamagedTailKind, dmgBytes int64, err error) {
	if len(raw) == 0 {
		return nil, 0, 0, "", 0, &ErrJournalInvalid{Reason: journal.ReasonEmpty}
	}
	if bytes.IndexByte(raw, '\r') >= 0 {
		return nil, 0, 0, "", 0, &ErrJournalInvalid{Reason: wire.ReasonIngest}
	}
	var offset int64
	var groupStart, groupSize int64
	openGroup := false
	rest := raw
	for len(rest) > 0 {
		nl := bytes.IndexByte(rest, '\n')
		if nl < 0 {
			// Torn final line.
			return nodes, lastCutSeq, lastCutOff, DamagedTornLine, int64(len(raw)) - lastCutOff, nil
		}
		line := rest[:nl]
		lineLen := int64(nl + 1)
		rest = rest[nl+1:]
		n, ierr := wire.Ingest(line, wire.KindJournalEntry)
		if ierr != nil {
			if len(rest) == 0 {
				return nodes, lastCutSeq, lastCutOff, DamagedTornLine, int64(len(raw)) - lastCutOff, nil
			}
			return nil, 0, 0, "", 0, &ErrJournalInvalid{Reason: rejectReason(ierr)}
		}
		seqNode, grpNode, szNode := n.Get("seq"), n.Get("group"), n.Get("group_size")
		if seqNode == nil || !seqNode.IsInt || grpNode == nil || !grpNode.IsInt || szNode == nil || !szNode.IsInt {
			return nil, 0, 0, "", 0, &ErrJournalInvalid{Reason: journal.ReasonShape}
		}
		seq, grp, sz := seqNode.Int, grpNode.Int, szNode.Int
		if !openGroup || seq == grp {
			groupStart, groupSize, openGroup = grp, sz, true
		}
		nodes = append(nodes, n)
		offset += lineLen
		if seq-groupStart+1 == groupSize {
			lastCutSeq = uint64(seq)
			lastCutOff = offset
			openGroup = false
		}
	}
	if openGroup {
		return nodes, lastCutSeq, lastCutOff, DamagedIncompleteGroup, int64(len(raw)) - lastCutOff, nil
	}
	return nodes, lastCutSeq, lastCutOff, "", 0, nil
}

// Append writes a content record (appended or revised) to st.
func (st *Stream) Append(ctx context.Context, w Write) (Receipt, error) {
	if err := ctx.Err(); err != nil {
		return Receipt{}, err
	}
	if kl := utf8.RuneCountInString(w.IdempotencyKey); kl < 1 || kl > 256 {
		return Receipt{}, fmt.Errorf("%w: idempotency key length", ErrSchema)
	}
	recNode, err := wire.Ingest(w.Record, wire.KindRecord)
	if err != nil {
		return Receipt{}, fmt.Errorf("%w: record ingest: %s", ErrSchema, rejectReason(err))
	}
	if err := shape.Validate("v1/record.schema.json", recNode); err != nil {
		return Receipt{}, fmt.Errorf("%w: record shape", ErrSchema)
	}
	if recNode.Get("stream").Str != st.id {
		return Receipt{}, fmt.Errorf("%w: record stream mismatch", ErrSchema)
	}
	rid := recNode.Get("record_id").Str
	rev := recNode.Get("revision").Int
	if rev > 1 {
		sup := recNode.Get("supersedes")
		if sup == nil || sup.Get("revision").Int != rev-1 {
			return Receipt{}, fmt.Errorf("%w: supersedes_revision", ErrSchema)
		}
	}
	recDigest, err := canon.Digest(wire.Encode(recNode))
	if err != nil {
		return Receipt{}, fmt.Errorf("%w: record digest", ErrSchema)
	}

	var out Receipt
	err = st.store.withLock(func() error {
		rep, err := st.store.replayStream(st.id)
		if err != nil {
			return err
		}
		if rep.lastSeq >= maxSafeInteger {
			return fmt.Errorf("%w: stream seq exhausted", ErrSchema)
		}
		now := st.store.opts.Clock.Now().UTC().Truncate(time.Microsecond)
		if now.Before(rep.lastAt) {
			return ErrClockRegression
		}
		atStr := FormatInstant(now)

		// Check live idempotency key window first.
		if kind, val, _, savedReceipt, live := rep.st.LiveKey(w.IdempotencyKey, now); live {
			if kind != "content" || val != recDigest {
				out = Receipt{
					V:              1,
					Kind:           "write_receipt",
					Stream:         st.id,
					IdempotencyKey: w.IdempotencyKey,
					Outcome:        OutcomeIdempotencyConflict,
					Reason:         journal.ReasonIdempotencyConflict,
				}
				return nil
			}
			seq := rep.lastSeq + 1
			seqTok := strconv.FormatInt(seq, 10)
			dedup := &wire.Node{
				Kind: wire.Object,
				Fields: []wire.Member{
					{Name: "v", Value: &wire.Node{Kind: wire.Number, Token: "1", IsInt: true, Int: 1}},
					{Name: "seq", Value: &wire.Node{Kind: wire.Number, Token: seqTok, IsInt: true, Int: seq}},
					{Name: "group", Value: &wire.Node{Kind: wire.Number, Token: seqTok, IsInt: true, Int: seq}},
					{Name: "group_size", Value: &wire.Node{Kind: wire.Number, Token: "1", IsInt: true, Int: 1}},
					{Name: "stream", Value: &wire.Node{Kind: wire.String, Str: st.id}},
					{Name: "type", Value: &wire.Node{Kind: wire.String, Str: "deduplicated"}},
					{Name: "at", Value: &wire.Node{Kind: wire.String, Str: atStr}},
					{Name: "record_id", Value: &wire.Node{Kind: wire.String, Str: rid}},
					{Name: "idempotency_key", Value: &wire.Node{Kind: wire.String, Str: w.IdempotencyKey}},
				},
			}
			if err := rep.st.Ingest(dedup); err != nil {
				return &ErrJournalInvalid{Reason: rejectReason(err)}
			}
			line := append(wire.Encode(dedup), '\n')
			if err := st.store.appender.Append(rep.jpath, rep.cutOffset, line, false); err != nil {
				return mapJFileErr(err)
			}
			return json.Unmarshal(wire.Encode(savedReceipt), &out)
		}

		// Verify external body if referenced.
		var bodyHex string
		hasBody := recNode.Get("body") != nil
		if hasBody {
			bodyNode := recNode.Get("body")
			bDig := bodyNode.Get("digest").Str
			bLen := bodyNode.Get("length").Int
			bodyHex = digestHex(bDig)
			desig, err := st.store.readDesignatedState()
			if err != nil {
				return err
			}
			bst, err := st.store.evalBodyState(desig, bodyHex, bLen)
			if err != nil {
				return err
			}
			if bst.kind != stateAvailable {
				return fmt.Errorf("%w: referenced body is not available (state %s)", ErrSchema, bst.kind)
			}
			if w.Lease != nil {
				if w.Lease.Digest != bDig || w.Lease.ID == "" || !now.Before(w.Lease.ExpiresAt) {
					return fmt.Errorf("%w: invalid or expired publication lease", ErrSchema)
				}
			}
		}

		// Check retirement: a retired record refuses new revisions.
		if rep.st.IsRetired(rid) {
			return fmt.Errorf("%w: record %s is retired", ErrSchema, rid)
		}

		// Check revision chain against current head.
		head, hasHead := rep.st.HeadPin(rid)
		if rev > 1 && rep.mode == ModeSingleRevision {
			return fmt.Errorf("%w: mode_violation", ErrSchema)
		}
		conflict := false
		if rev == 1 {
			if hasHead {
				conflict = true
			}
		} else {
			sup := recNode.Get("supersedes")
			if !hasHead || head.Revision != rev-1 || sup.Get("digest").Str != head.Digest {
				conflict = true
			}
		}
		if conflict {
			seq := rep.lastSeq + 1
			seqTok := strconv.FormatInt(seq, 10)
			revTok := strconv.FormatInt(rev, 10)
			fields := []wire.Member{
				{Name: "v", Value: &wire.Node{Kind: wire.Number, Token: "1", IsInt: true, Int: 1}},
				{Name: "seq", Value: &wire.Node{Kind: wire.Number, Token: seqTok, IsInt: true, Int: seq}},
				{Name: "group", Value: &wire.Node{Kind: wire.Number, Token: seqTok, IsInt: true, Int: seq}},
				{Name: "group_size", Value: &wire.Node{Kind: wire.Number, Token: "1", IsInt: true, Int: 1}},
				{Name: "stream", Value: &wire.Node{Kind: wire.String, Str: st.id}},
				{Name: "type", Value: &wire.Node{Kind: wire.String, Str: "conflicted"}},
				{Name: "at", Value: &wire.Node{Kind: wire.String, Str: atStr}},
				{Name: "record_id", Value: &wire.Node{Kind: wire.String, Str: rid}},
				{Name: "reason", Value: &wire.Node{Kind: wire.String, Str: "revision_conflict"}},
				{Name: "presented", Value: &wire.Node{
					Kind: wire.Object,
					Fields: []wire.Member{
						{Name: "revision", Value: &wire.Node{Kind: wire.Number, Token: revTok, IsInt: true, Int: rev}},
						{Name: "digest", Value: &wire.Node{Kind: wire.String, Str: recDigest}},
					},
				}},
			}
			var curPin *Pin
			if hasHead {
				hTok := strconv.FormatInt(head.Revision, 10)
				fields = append(fields, wire.Member{
					Name: "current",
					Value: &wire.Node{
						Kind: wire.Object,
						Fields: []wire.Member{
							{Name: "revision", Value: &wire.Node{Kind: wire.Number, Token: hTok, IsInt: true, Int: head.Revision}},
							{Name: "digest", Value: &wire.Node{Kind: wire.String, Str: head.Digest}},
						},
					},
				})
				curPin = &Pin{Stream: head.Stream, RecordID: head.RecordID, Revision: uint64(head.Revision), Digest: head.Digest}
			}
			confEntry := &wire.Node{Kind: wire.Object, Fields: fields}
			if err := rep.st.Ingest(confEntry); err != nil {
				return &ErrJournalInvalid{Reason: rejectReason(err)}
			}
			line := append(wire.Encode(confEntry), '\n')
			if err := rep.appendLine(st.store, line); err != nil {
				return err
			}
			out = Receipt{
				V:              1,
				Kind:           "write_receipt",
				Stream:         st.id,
				IdempotencyKey: w.IdempotencyKey,
				Outcome:        OutcomeRevisionConflict,
				Seq:            uint64(seq),
				At:             atStr,
				Reason:         "revision_conflict",
				Current:        curPin,
			}
			return nil
		}

		entryType := "appended"
		outcome := OutcomeAppended
		if rev > 1 {
			entryType = "revised"
			outcome = OutcomeRevised
		}
		seq := rep.lastSeq + 1
		seqTok := strconv.FormatInt(seq, 10)
		fields := []wire.Member{
			{Name: "v", Value: &wire.Node{Kind: wire.Number, Token: "1", IsInt: true, Int: 1}},
			{Name: "seq", Value: &wire.Node{Kind: wire.Number, Token: seqTok, IsInt: true, Int: seq}},
			{Name: "group", Value: &wire.Node{Kind: wire.Number, Token: seqTok, IsInt: true, Int: seq}},
			{Name: "group_size", Value: &wire.Node{Kind: wire.Number, Token: "1", IsInt: true, Int: 1}},
			{Name: "stream", Value: &wire.Node{Kind: wire.String, Str: st.id}},
			{Name: "type", Value: &wire.Node{Kind: wire.String, Str: entryType}},
			{Name: "at", Value: &wire.Node{Kind: wire.String, Str: atStr}},
			{Name: "record", Value: recNode},
			{Name: "digest", Value: &wire.Node{Kind: wire.String, Str: recDigest}},
			{Name: "idempotency_key", Value: &wire.Node{Kind: wire.String, Str: w.IdempotencyKey}},
		}
		var bodyStatus string
		if hasBody {
			bodyStatus = "verified"
			fields = append(fields, wire.Member{Name: "body_status", Value: &wire.Node{Kind: wire.String, Str: bodyStatus}})
		}
		if w.ImportedFrom != nil {
			impRevTok := strconv.FormatUint(w.ImportedFrom.Pin.Revision, 10)
			fields = append(fields, wire.Member{
				Name: "imported_from",
				Value: &wire.Node{
					Kind: wire.Object,
					Fields: []wire.Member{
						{Name: "capability", Value: &wire.Node{Kind: wire.String, Str: w.ImportedFrom.Capability}},
						{Name: "pin", Value: &wire.Node{
							Kind: wire.Object,
							Fields: []wire.Member{
								{Name: "stream", Value: &wire.Node{Kind: wire.String, Str: w.ImportedFrom.Pin.Stream}},
								{Name: "record_id", Value: &wire.Node{Kind: wire.String, Str: w.ImportedFrom.Pin.RecordID}},
								{Name: "revision", Value: &wire.Node{Kind: wire.Number, Token: impRevTok, IsInt: true, Int: int64(w.ImportedFrom.Pin.Revision)}},
								{Name: "digest", Value: &wire.Node{Kind: wire.String, Str: w.ImportedFrom.Pin.Digest}},
							},
						}},
					},
				},
			})
		}
		entry := &wire.Node{Kind: wire.Object, Fields: fields}
		if err := shape.Validate("v1/journal-entry.schema.json", entry); err != nil {
			return fmt.Errorf("%w: journal entry shape", ErrSchema)
		}
		if err := rep.st.Ingest(entry); err != nil {
			return fmt.Errorf("%w: %s", ErrSchema, rejectReason(err))
		}
		line := append(wire.Encode(entry), '\n')
		if err := rep.appendLine(st.store, line); err != nil {
			return err
		}
		if hasBody && w.Lease != nil {
			_ = st.store.consumeLease(bodyHex, *w.Lease)
		}
		pin := Pin{Stream: st.id, RecordID: rid, Revision: uint64(rev), Digest: recDigest}
		out = Receipt{
			V:              1,
			Kind:           "write_receipt",
			Stream:         st.id,
			IdempotencyKey: w.IdempotencyKey,
			Outcome:        outcome,
			Seq:            uint64(seq),
			At:             atStr,
			Pin:            &pin,
			BodyStatus:     bodyStatus,
			FailureModel:   st.store.profile.Runtime.FailureModel,
		}
		return nil
	})
	if err != nil {
		return Receipt{}, err
	}
	return out, nil
}

// Retire withdraws recordID from ordinary head resolution on st without
// removing retained history. When WithRetireKey is supplied, retirement shares
// the stream's idempotency window and returns a durable-record/v1 receipt (A3, G13).
func (st *Stream) Retire(ctx context.Context, recordID string, reason RetireReason, opts ...RetireOption) (RetireResult, error) {
	if err := ctx.Err(); err != nil {
		return RetireResult{}, err
	}
	if !reRecordID.MatchString(recordID) {
		return RetireResult{}, fmt.Errorf("%w: invalid record_id", ErrSchema)
	}
	if reason != RetireSuperseded && reason != RetireWithdrawn && reason != RetireRetention && reason != RetireOperator {
		return RetireResult{}, fmt.Errorf("%w: invalid retire reason", ErrSchema)
	}
	var cfg retireConfig
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	if cfg.key != "" {
		if kl := utf8.RuneCountInString(cfg.key); kl < 1 || kl > 256 {
			return RetireResult{}, fmt.Errorf("%w: idempotency key length", ErrSchema)
		}
	}

	var res RetireResult
	err := st.store.withLock(func() error {
		rep, err := st.store.replayStream(st.id)
		if err != nil {
			return err
		}
		if rep.lastSeq >= maxSafeInteger {
			return fmt.Errorf("%w: stream seq exhausted", ErrSchema)
		}
		now := st.store.opts.Clock.Now().UTC().Truncate(time.Microsecond)
		if now.Before(rep.lastAt) {
			return ErrClockRegression
		}
		atStr := FormatInstant(now)

		if cfg.key != "" {
			// Keyed retirement (A3, G13: key lookup precedes target_not_found).
			reqFields := []wire.Member{
				{Name: "stream", Value: &wire.Node{Kind: wire.String, Str: st.id}},
				{Name: "record_id", Value: &wire.Node{Kind: wire.String, Str: recordID}},
				{Name: "reason", Value: &wire.Node{Kind: wire.String, Str: string(reason)}},
				{Name: "idempotency_key", Value: &wire.Node{Kind: wire.String, Str: cfg.key}},
			}
			reqNode := &wire.Node{Kind: wire.Object, Fields: reqFields}
			prevLen := rep.st.Len()
			rcNode, err := rep.st.Retire(reqNode, atStr)
			if err != nil {
				return fmt.Errorf("%w: %s", ErrSchema, rejectReason(err))
			}
			rcBytes := wire.Encode(rcNode)
			outcome := rcNode.Get("outcome").Str
			if outcome != "retired" {
				if outcome == journal.ReasonIdempotencyConflict {
					res = RetireResult{Outcome: RetireOutcomeIdempotencyConflict, Receipt: rcBytes}
				} else {
					res = RetireResult{Outcome: RetireOutcomeTargetNotFound, Receipt: rcBytes}
				}
				return nil
			}
			if rep.st.Len() > prevLen {
				entry := rep.st.Journal()[prevLen]
				line := append(wire.Encode(entry), '\n')
				if err := rep.appendLine(st.store, line); err != nil {
					return err
				}
			}
			sq := uint64(rcNode.Get("seq").Int)
			res = RetireResult{
				Outcome: RetireOutcomeRetired,
				Seq:     sq,
				At:      rep.st.Journal()[sq-1].Get("at").Str,
				Receipt: rcBytes,
			}
			return nil
		}

		// Unkeyed retirement (local convenience, A3 & G13).
		if _, hasHead := rep.st.HeadPin(recordID); !hasHead {
			res = RetireResult{Outcome: RetireOutcomeNoRecord}
			return nil
		}
		if origSeq, origAt, retired := rep.st.FirstRetired(recordID); retired {
			res = RetireResult{
				Outcome: RetireOutcomeAlreadyRetired,
				Seq:     uint64(origSeq),
				At:      origAt,
			}
			return nil
		}
		seq := rep.lastSeq + 1
		seqTok := strconv.FormatInt(seq, 10)
		entry := &wire.Node{
			Kind: wire.Object,
			Fields: []wire.Member{
				{Name: "v", Value: &wire.Node{Kind: wire.Number, Token: "1", IsInt: true, Int: 1}},
				{Name: "seq", Value: &wire.Node{Kind: wire.Number, Token: seqTok, IsInt: true, Int: seq}},
				{Name: "group", Value: &wire.Node{Kind: wire.Number, Token: seqTok, IsInt: true, Int: seq}},
				{Name: "group_size", Value: &wire.Node{Kind: wire.Number, Token: "1", IsInt: true, Int: 1}},
				{Name: "stream", Value: &wire.Node{Kind: wire.String, Str: st.id}},
				{Name: "type", Value: &wire.Node{Kind: wire.String, Str: "retired"}},
				{Name: "at", Value: &wire.Node{Kind: wire.String, Str: atStr}},
				{Name: "record_id", Value: &wire.Node{Kind: wire.String, Str: recordID}},
				{Name: "reason", Value: &wire.Node{Kind: wire.String, Str: string(reason)}},
			},
		}
		if err := shape.Validate("v1/journal-entry.schema.json", entry); err != nil {
			return fmt.Errorf("%w: retired entry shape", ErrSchema)
		}
		if err := rep.st.Ingest(entry); err != nil {
			return &ErrJournalInvalid{Reason: rejectReason(err)}
		}
		line := append(wire.Encode(entry), '\n')
		if err := rep.appendLine(st.store, line); err != nil {
			return err
		}
		res = RetireResult{
			Outcome: RetireOutcomeRetired,
			Seq:     uint64(seq),
			At:      atStr,
		}
		return nil
	})
	if err != nil {
		return RetireResult{}, err
	}
	return res, nil
}

// Protect applies or restores a protection hold on st.
func (st *Stream) Protect(ctx context.Context, req ProtectionRequest) (ProtectionReceipt, error) {
	if err := ctx.Err(); err != nil {
		return ProtectionReceipt{}, err
	}
	reqNode, err := wire.Ingest(req.Raw, wire.KindProtectionRequest)
	if err != nil {
		return ProtectionReceipt{}, fmt.Errorf("%w: protection request ingest: %s", ErrSchema, rejectReason(err))
	}
	if err := shape.Validate("v1/protection-request.schema.json", reqNode); err != nil {
		return ProtectionReceipt{}, fmt.Errorf("%w: protection request shape", ErrSchema)
	}
	if reqNode.Get("stream").Str != st.id {
		return ProtectionReceipt{}, fmt.Errorf("%w: protection request stream mismatch", ErrSchema)
	}

	var out ProtectionReceipt
	err = st.store.withLock(func() error {
		rep, err := st.store.replayStream(st.id)
		if err != nil {
			return err
		}
		if rep.lastSeq >= maxSafeInteger {
			return fmt.Errorf("%w: stream seq exhausted", ErrSchema)
		}
		now := st.store.opts.Clock.Now().UTC().Truncate(time.Microsecond)
		if now.Before(rep.lastAt) {
			return ErrClockRegression
		}
		atStr := FormatInstant(now)
		prevLen := rep.st.Len()
		rcNode, err := rep.st.Request(reqNode, atStr)
		if err != nil {
			return fmt.Errorf("%w: %s", ErrSchema, rejectReason(err))
		}
		outcome := rcNode.Get("outcome").Str
		if outcome != "tombstoned" && outcome != "untombstoned" {
			out = ProtectionReceipt{
				Outcome: "rejected",
				Reason:  outcome,
				Raw:     wire.Encode(rcNode),
			}
			return nil
		}
		if rep.st.Len() > prevLen {
			entry := rep.st.Journal()[prevLen]
			line := append(wire.Encode(entry), '\n')
			if err := rep.appendLine(st.store, line); err != nil {
				return err
			}
		}
		sq := uint64(rcNode.Get("seq").Int)
		out = ProtectionReceipt{
			Outcome: outcome,
			Seq:     sq,
			At:      rep.st.Journal()[sq-1].Get("at").Str,
			Raw:     wire.Encode(rcNode),
		}
		return nil
	})
	if err != nil {
		return ProtectionReceipt{}, err
	}
	return out, nil
}

// Reconcile looks up key in the stream's idempotency window using a
// caller-supplied attemptedAt timestamp, returning "found" or "unknown" only
// (Freeze 4.5, G10).
func (st *Stream) Reconcile(key string, attemptedAt time.Time) (Reconciliation, error) {
	if kl := utf8.RuneCountInString(key); kl < 1 || kl > 256 {
		return Reconciliation{}, fmt.Errorf("%w: idempotency key length", ErrSchema)
	}
	// G10: caller-supplied instants that aren't exact at microseconds are refused.
	if attemptedAt.Nanosecond()%1000 != 0 {
		return Reconciliation{}, fmt.Errorf("%w: attemptedAt has sub-microsecond precision", ErrSchema)
	}
	attStr := FormatInstant(attemptedAt)
	if _, err := ParseInstant(attStr); err != nil {
		return Reconciliation{}, err
	}

	var out Reconciliation
	err := st.store.withLock(func() error {
		rep, err := st.store.replayStream(st.id)
		if err != nil {
			return err
		}
		now := st.store.opts.Clock.Now().UTC().Truncate(time.Microsecond)
		if now.Before(rep.lastAt) {
			return ErrClockRegression
		}
		recNode, err := rep.st.Reconcile(key, FormatInstant(now), attStr)
		if err != nil {
			return fmt.Errorf("%w: %s", ErrSchema, rejectReason(err))
		}
		out = Reconciliation{
			Result:         recNode.Get("result").Str,
			Stream:         st.id,
			IdempotencyKey: key,
			AttemptedAt:    attStr,
		}
		if r := recNode.Get("reason"); r != nil && r.Kind == wire.String {
			out.Reason = r.Str
		}
		if rc := recNode.Get("receipt"); rc != nil {
			out.Receipt = wire.Encode(rc)
		}
		return nil
	})
	if err != nil {
		return Reconciliation{}, err
	}
	return out, nil
}

// Head returns the latest revision pin and retirement status of recordID on st.
func (st *Stream) Head(recordID string) (HeadInfo, bool, error) {
	if !reRecordID.MatchString(recordID) {
		return HeadInfo{}, false, fmt.Errorf("%w: invalid record_id", ErrSchema)
	}
	var info HeadInfo
	var found bool
	err := st.store.withLock(func() error {
		rep, err := st.store.replayStream(st.id)
		if err != nil {
			return err
		}
		hp, ok := rep.st.HeadPin(recordID)
		if !ok {
			return nil
		}
		found = true
		info = HeadInfo{
			Pin:     Pin{Stream: hp.Stream, RecordID: hp.RecordID, Revision: uint64(hp.Revision), Digest: hp.Digest},
			Retired: rep.st.IsRetired(recordID),
		}
		return nil
	})
	return info, found, err
}

// Revisions returns the accepted revisions of recordID on st in ascending
// revision order.
func (st *Stream) Revisions(recordID string) ([]RevisionView, error) {
	if !reRecordID.MatchString(recordID) {
		return nil, fmt.Errorf("%w: invalid record_id", ErrSchema)
	}
	var out []RevisionView
	err := st.store.withLock(func() error {
		rep, err := st.store.replayStream(st.id)
		if err != nil {
			return err
		}
		retired := rep.st.IsRetired(recordID)
		for _, e := range rep.st.Journal() {
			typ := e.Get("type").Str
			if typ != "appended" && typ != "revised" {
				continue
			}
			rec := e.Get("record")
			if rec.Get("record_id").Str != recordID {
				continue
			}
			rev := uint64(rec.Get("revision").Int)
			dig := e.Get("digest").Str
			rv := RevisionView{
				Pin:             Pin{Stream: st.id, RecordID: recordID, Revision: rev, Digest: dig},
				PayloadContract: rec.Get("payload_contract").Str,
				HasBody:         rec.Get("body") != nil,
				Retired:         retired,
				At:              e.Get("at").Str,
			}
			if sup := rec.Get("supersedes"); sup != nil && sup.Kind == wire.Object {
				rv.Supersedes = &Pin{
					Stream:   st.id,
					RecordID: recordID,
					Revision: uint64(sup.Get("revision").Int),
					Digest:   sup.Get("digest").Str,
				}
			}
			out = append(out, rv)
		}
		return nil
	})
	return out, err
}

type frameReader struct {
	mu      sync.Mutex
	r       *bytes.Reader
	revoked bool
	read    int64
	hasher  hash.Hash
}

func newFrameReader(data []byte) *frameReader {
	return &frameReader{
		r:      bytes.NewReader(data),
		hasher: sha256.New(),
	}
}

func (fr *frameReader) Read(p []byte) (int, error) {
	fr.mu.Lock()
	defer fr.mu.Unlock()
	if fr.revoked {
		return 0, ErrReaderRevoked
	}
	n, err := fr.r.Read(p)
	if n > 0 {
		fr.read += int64(n)
		fr.hasher.Write(p[:n])
	}
	return n, err
}

func (fr *frameReader) revoke() (readBytes int64, digest string) {
	fr.mu.Lock()
	defer fr.mu.Unlock()
	fr.revoked = true
	return fr.read, "sha256:" + hex.EncodeToString(fr.hasher.Sum(nil))
}

// Release evaluates a use request at the current consistent cut while holding
// the store lock and streams eligible frames to sink (Freeze 4.3, G17).
func (st *Stream) Release(ctx context.Context, req UseRequest, sink ReleaseSink) (ReleaseResult, error) {
	if err := ctx.Err(); err != nil {
		return ReleaseResult{}, err
	}
	if req.Purpose != PurposeServe && req.Purpose != PurposeExport && req.Purpose != PurposeReuse {
		return ReleaseResult{}, fmt.Errorf("%w: invalid release purpose", ErrSchema)
	}
	closure := req.Closure
	if closure == "" {
		closure = ClosureFullContent
	}
	if closure != ClosureFullContent && closure != ClosureMetadataOnly {
		return ReleaseResult{}, fmt.Errorf("%w: invalid release closure", ErrSchema)
	}

	var res ReleaseResult
	var sinkErr error
	err := st.store.withLock(func() error {
		// G17: Release with full_content on a store whose designated stream is
		// damaged or missing returns ErrJournalInvalid determinately before delivery.
		var desig *designatedJournal
		if closure == ClosureFullContent {
			var err error
			desig, err = st.store.readDesignatedState()
			if err != nil {
				return err
			}
		}

		rep, err := st.store.replayStream(st.id)
		if err != nil {
			if errors.Is(err, ErrStreamSealed) {
				res = ReleaseResult{
					Outcome:       ReleaseRefused,
					RefusalReason: journal.ReasonProtectionUnavailable,
				}
				return nil
			}
			varji := new(ErrJournalInvalid)
			if errors.As(err, &varji) {
				res = ReleaseResult{
					Outcome:       ReleaseRefused,
					RefusalReason: journal.ReasonProtectionUnavailable,
				}
				return nil
			}
			return err
		}

		records := rep.st.Records()
		allPins := make([]journal.Pin, 0, len(records))
		for pk := range records {
			allPins = append(allPins, pk)
		}
		slices.SortFunc(allPins, func(a, b journal.Pin) int {
			if a.RecordID != b.RecordID {
				if a.RecordID < b.RecordID {
					return -1
				}
				return 1
			}
			if a.Revision < b.Revision {
				return -1
			}
			if a.Revision > b.Revision {
				return 1
			}
			return 0
		})

		var selPins []journal.Pin
		if len(req.Selection) > 0 {
			for _, sp := range req.Selection {
				selPins = append(selPins, journal.Pin{
					Stream:   sp.Stream,
					RecordID: sp.RecordID,
					Revision: int64(sp.Revision),
					Digest:   sp.Digest,
				})
			}
		} else {
			for _, pk := range allPins {
				if req.Purpose == PurposeServe && rep.st.IsRetired(pk.RecordID) {
					continue
				}
				if blk, berr := rep.st.Blocked(pk); berr == nil && !blk {
					selPins = append(selPins, pk)
				}
			}
		}

		// Compute omitted complement and gather physical bodies for full_content.
		selSet := make(map[journal.Pin]bool, len(selPins))
		for _, pk := range selPins {
			selSet[pk] = true
		}
		var omittedNodes []*wire.Node
		var omittedPins []Pin
		for _, pk := range allPins {
			if !selSet[pk] {
				omittedNodes = append(omittedNodes, pinToWire(pk))
				omittedPins = append(omittedPins, Pin{
					Stream:   pk.Stream,
					RecordID: pk.RecordID,
					Revision: uint64(pk.Revision),
					Digest:   pk.Digest,
				})
			}
		}
		selNodes := make([]*wire.Node, len(selPins))
		for i, pk := range selPins {
			selNodes[i] = pinToWire(pk)
		}

		type bodyFrame struct {
			pin    Pin
			digest string
			bytes  []byte
		}
		var bodyNodes []*wire.Node
		seenBodyDig := map[string]bool{}
		bodyByPin := map[journal.Pin]bodyFrame{}
		if closure == ClosureFullContent {
			for _, pk := range selPins {
				rec := records[pk]
				if rec == nil || rec.Get("body") == nil {
					continue
				}
				bNode := rec.Get("body")
				bDig := bNode.Get("digest").Str
				bLen := bNode.Get("length").Int
				h := digestHex(bDig)
				bst, berr := st.store.evalBodyState(desig, h, bLen)
				if berr != nil || bst.kind != stateAvailable {
					continue
				}
				rawBody, rerr := readFileVerified(st.store.root, bodyFile(h), st.store.fso, maxBodyLength)
				if rerr != nil {
					continue
				}
				if !seenBodyDig[bDig] {
					seenBodyDig[bDig] = true
					bodyNodes = append(bodyNodes, &wire.Node{
						Kind: wire.Object,
						Fields: []wire.Member{
							{Name: "digest", Value: &wire.Node{Kind: wire.String, Str: bDig}},
							{Name: "hex", Value: &wire.Node{Kind: wire.String, Str: hex.EncodeToString(rawBody)}},
						},
					})
				}
				bodyByPin[pk] = bodyFrame{
					pin:    Pin{Stream: pk.Stream, RecordID: pk.RecordID, Revision: uint64(pk.Revision), Digest: pk.Digest},
					digest: bDig,
					bytes:  rawBody,
				}
			}
		}

		cutTok := strconv.FormatInt(rep.lastSeq, 10)
		cutNode := &wire.Node{Kind: wire.Number, Token: cutTok, IsInt: true, Int: rep.lastSeq}
		useStep := &wire.Node{
			Kind: wire.Object,
			Fields: []wire.Member{
				{Name: "stream", Value: &wire.Node{Kind: wire.String, Str: st.id}},
				{Name: "purpose", Value: &wire.Node{Kind: wire.String, Str: string(req.Purpose)}},
				{Name: "protection_state", Value: &wire.Node{Kind: wire.String, Str: "verified"}},
				{Name: "protection_cut", Value: cutNode},
				{Name: "current_cut", Value: cutNode},
				{Name: "inherited_block", Value: &wire.Node{Kind: wire.Bool, Bool: false}},
				{Name: "selection", Value: &wire.Node{Kind: wire.Array, Elems: selNodes}},
				{Name: "omitted", Value: &wire.Node{Kind: wire.Array, Elems: omittedNodes}},
				{Name: "closure", Value: &wire.Node{Kind: wire.String, Str: string(closure)}},
				{Name: "bodies", Value: &wire.Node{Kind: wire.Array, Elems: bodyNodes}},
			},
		}

		useOut, uerr := rep.st.Use(useStep)
		if uerr != nil {
			res = ReleaseResult{
				Outcome:       ReleaseRefused,
				RefusalReason: rejectReason(uerr),
				Cut:           uint64(rep.lastSeq),
			}
			return nil
		}
		if useOut.Get("outcome").Str == "refused" {
			res = ReleaseResult{
				Outcome:       ReleaseRefused,
				RefusalReason: useOut.Get("reason").Str,
				Cut:           uint64(rep.lastSeq),
			}
			return nil
		}

		// Build frames in selection order: each record frame, followed by its
		// body frame when closure == full_content.
		type frameItem struct {
			header FrameHeader
			data   []byte
		}
		var frames []frameItem
		var outSelection []Pin
		for _, pk := range selPins {
			rec := records[pk]
			recBytes, cerr := canon.Canonicalize(wire.Encode(rec))
			if cerr != nil {
				return fmt.Errorf("%w: %v", ErrSchema, cerr)
			}
			p := Pin{Stream: pk.Stream, RecordID: pk.RecordID, Revision: uint64(pk.Revision), Digest: pk.Digest}
			outSelection = append(outSelection, p)
			frames = append(frames, frameItem{
				header: FrameHeader{
					Index:  len(frames),
					Pin:    p,
					Kind:   FrameKindRecord,
					Length: int64(len(recBytes)),
					Digest: pk.Digest,
				},
				data: recBytes,
			})
			if bf, ok := bodyByPin[pk]; ok {
				frames = append(frames, frameItem{
					header: FrameHeader{
						Index:  len(frames),
						Pin:    p,
						Kind:   FrameKindBody,
						Length: int64(len(bf.bytes)),
						Digest: bf.digest,
					},
					data: bf.bytes,
				})
			}
		}

		res = ReleaseResult{
			Outcome:      ReleaseReleased,
			Cut:          uint64(rep.lastSeq),
			Selection:    outSelection,
			OmittedCount: len(omittedPins),
			Omitted:      omittedPins,
		}
		if sink == nil {
			return nil
		}

		for i, item := range frames {
			if err := ctx.Err(); err != nil {
				if i == 0 {
					return err
				}
				res.Outcome = ReleasePartial
				return nil
			}
			fr := newFrameReader(item.data)
			st.store.releaseGID.Store(currentGoroutineID())
			cbErr := sink.Frame(ctx, item.header, fr)
			st.store.releaseGID.Store(0)
			readBytes, gotDigest := fr.revoke()

			if cbErr != nil || readBytes != item.header.Length || gotDigest != item.header.Digest {
				idx := i
				res.Outcome = ReleaseUncertain
				res.FrameInFlight = &idx
				if cbErr != nil {
					sinkErr = ErrSink
				}
				return nil
			}
			res.FramesCompleted++
			pinCopy := item.header.Pin
			res.ReleasedThroughPin = &pinCopy
		}
		return nil
	})
	if err != nil {
		return ReleaseResult{}, err
	}
	return res, sinkErr
}

func pinToWire(p journal.Pin) *wire.Node {
	revTok := strconv.FormatInt(p.Revision, 10)
	return &wire.Node{
		Kind: wire.Object,
		Fields: []wire.Member{
			{Name: "stream", Value: &wire.Node{Kind: wire.String, Str: p.Stream}},
			{Name: "record_id", Value: &wire.Node{Kind: wire.String, Str: p.RecordID}},
			{Name: "revision", Value: &wire.Node{Kind: wire.Number, Token: revTok, IsInt: true, Int: p.Revision}},
			{Name: "digest", Value: &wire.Node{Kind: wire.String, Str: p.Digest}},
		},
	}
}
