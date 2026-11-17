package recordprims

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/3leaps/recordprims/internal/canon"
	"github.com/3leaps/recordprims/internal/fsroot"
	"github.com/3leaps/recordprims/internal/journal"
	"github.com/3leaps/recordprims/internal/shape"
	"github.com/3leaps/recordprims/internal/wire"
)

// ContinuationState summarizes the current restriction-review state of a Stream.
type ContinuationState struct {
	IsContinuation                bool       `json:"is_continuation"`
	Continues                     *Continues `json:"continues,omitempty"`
	ReviewPending                 bool       `json:"review_pending"`
	SourceStateDigest             string     `json:"source_state_digest,omitempty"`
	DestinationAppliedStateDigest string     `json:"destination_applied_state_digest,omitempty"`
	DestinationCutSeq             uint64     `json:"destination_cut_seq"`
	SourceSealed                  bool       `json:"source_sealed"`
	SourceSealDigest              string     `json:"source_seal_digest,omitempty"`
	LastReviewSeq                 uint64     `json:"last_review_seq,omitempty"`
	LastReviewAt                  string     `json:"last_review_at,omitempty"`
}

// RestrictionReview supplies the inputs required to record a
// durable-record/v1 restrictions_reviewed entry on a continuation stream.
type RestrictionReview struct {
	SourceCutSeq                  uint64        `json:"source_cut_seq"`
	SourceCutDigest               string        `json:"source_cut_digest"`
	SourceStateDigest             string        `json:"source_state_digest"`
	DestinationCutSeq             uint64        `json:"destination_cut_seq"`
	DestinationAppliedStateDigest string        `json:"destination_applied_state_digest"`
	Reviewer                      Provenance    `json:"reviewer"`
	Authorization                 Authorization `json:"authorization"`
	Note                          string        `json:"note,omitempty"`
}

// RestrictionReviewOutcome is the outcome of Stream.EstablishRestrictions.
type RestrictionReviewOutcome string

const (
	RestrictionReviewCompleted        RestrictionReviewOutcome = "completed"
	RestrictionReviewAlreadyCompleted RestrictionReviewOutcome = "already_completed"
)

// RestrictionReviewResult is returned by Stream.EstablishRestrictions.
type RestrictionReviewResult struct {
	Outcome RestrictionReviewOutcome `json:"outcome"`
	Seq     uint64                   `json:"seq"`
	At      string                   `json:"at"`
}

// AncestorSealRef is one entry in a StreamSeal's ancestor_seals array.
type AncestorSealRef struct {
	Store      string `json:"store"`
	Capability string `json:"capability"`
	Stream     string `json:"stream"`
	CutSeq     uint64 `json:"cut_seq"`
	SealDigest string `json:"seal_digest"`
}

// SealTrustProfile identifies the trust profile bound in a StreamSeal.
type SealTrustProfile struct {
	ID      string `json:"id"`
	Version string `json:"version"`
}

// StreamSeal is a detached durable-record/v1 stream-seal.schema.json document.
type StreamSeal struct {
	V                                  int               `json:"v"`
	Store                              string            `json:"store"`
	Capability                         string            `json:"capability"`
	Stream                             string            `json:"stream"`
	CutSeq                             uint64            `json:"cut_seq"`
	CutDigest                          string            `json:"cut_digest"`
	EffectiveRestrictionManifestDigest string            `json:"effective_restriction_manifest_digest"`
	AncestorSeals                      []AncestorSealRef `json:"ancestor_seals"`
	TrustProfile                       SealTrustProfile  `json:"trust_profile"`
	SealedAt                           string            `json:"sealed_at"`
	Provenance                         Provenance        `json:"provenance"`
	Authorization                      Authorization     `json:"authorization"`
	SealDigest                         string            `json:"seal_digest"`
	Raw                                json.RawMessage   `json:"raw,omitempty"`
}

// SealOutcome is the outcome of Store.SealStream.
type SealOutcome string

const (
	SealOutcomeSealed        SealOutcome = "sealed"
	SealOutcomeAlreadySealed SealOutcome = "already_sealed"
	SealOutcomeRefused       SealOutcome = "refused"
)

// SealRefusalReason identifies why Store.SealStream refused to seal a stream.
type SealRefusalReason string

const (
	SealRefusedHeld                SealRefusalReason = "source_held"
	SealRefusedRestrictionsPending SealRefusalReason = "restrictions_pending"
	SealRefusedObligationPending   SealRefusalReason = "obligation_pending"
)

// SealStreamRequest configures Store.SealStream.
type SealStreamRequest struct {
	Stream             string        `json:"stream"`
	Provenance         Provenance    `json:"provenance"`
	Authorization      Authorization `json:"authorization"`
	PendingObligations []string      `json:"pending_obligations,omitempty"`
}

// SealStreamResult is returned by Store.SealStream.
type SealStreamResult struct {
	Outcome       SealOutcome       `json:"outcome"`
	RefusalReason SealRefusalReason `json:"refusal_reason,omitempty"`
	Seal          *StreamSeal       `json:"seal,omitempty"`
}

// CompactionOutcome is the outcome of Store.SealAndCompactStream.
type CompactionOutcome string

const (
	CompactionOutcomeCompacted        CompactionOutcome = "compacted"
	CompactionOutcomeAlreadyCompacted CompactionOutcome = "already_compacted"
	CompactionOutcomeRefused          CompactionOutcome = "refused"
)

// CompactionRefusalReason identifies why Store.SealAndCompactStream refused a compaction.
type CompactionRefusalReason string

const (
	CompactionRefusedHeld                CompactionRefusalReason = "source_held"
	CompactionRefusedRestrictionsPending CompactionRefusalReason = "restrictions_pending"
	CompactionRefusedUnretiredDropped    CompactionRefusalReason = "unretired_record_dropped"
	CompactionRefusedBodyUnpreserved     CompactionRefusalReason = "external_body_unpreserved"
	CompactionRefusedObligationPending   CompactionRefusalReason = "obligation_pending"
)

// CompactedRecord specifies one retained record to import into the continuation
// stream during Store.SealAndCompactStream.
type CompactedRecord struct {
	Record         json.RawMessage `json:"record"`
	SourcePin      Pin             `json:"source_pin"`
	IdempotencyKey string          `json:"idempotency_key"`
}

// CompactionFaultPhase names a durable step boundary in Store.SealAndCompactStream.
type CompactionFaultPhase string

const (
	CompactionPhaseAfterSourceSeal        CompactionFaultPhase = "after_source_seal"
	CompactionPhaseAfterDestinationOpen   CompactionFaultPhase = "after_destination_open"
	CompactionPhaseAfterRecordImports     CompactionFaultPhase = "after_record_imports"
	CompactionPhaseAfterRestrictionReview CompactionFaultPhase = "after_restriction_review"
)

// CompactionFaultHook is an optional callback invoked at durable step boundaries
// during Store.SealAndCompactStream. Returning a non-nil error aborts the call
// at that boundary and wraps the error in *ErrCompactionIncomplete.
type CompactionFaultHook func(phase CompactionFaultPhase) error

// CompactionRequest configures Store.SealAndCompactStream.
type CompactionRequest struct {
	SourceStream       string              `json:"source_stream"`
	DestinationStream  string              `json:"destination_stream"`
	Mode               Mode                `json:"mode"`
	Records            []CompactedRecord   `json:"records"`
	AllowDropUnretired bool                `json:"allow_drop_unretired"`
	PendingObligations []string            `json:"pending_obligations,omitempty"`
	Reviewer           Provenance          `json:"reviewer"`
	Authorization      Authorization       `json:"authorization"`
	Note               string              `json:"note,omitempty"`
	ReclaimPredecessor bool                `json:"reclaim_predecessor"`
	ArchiveDir         string              `json:"archive_dir,omitempty"`
	FaultHook          CompactionFaultHook `json:"-"`
}

// ImportedPinMapping pairs a historical source pin with its imported destination pin.
type ImportedPinMapping struct {
	SourcePin      Pin `json:"source_pin"`
	DestinationPin Pin `json:"destination_pin"`
}

// CompactionResult is returned by Store.SealAndCompactStream.
type CompactionResult struct {
	Outcome       CompactionOutcome       `json:"outcome"`
	RefusalReason CompactionRefusalReason `json:"refusal_reason,omitempty"`
	Seal          *StreamSeal             `json:"seal,omitempty"`
	Continues     *Continues              `json:"continues,omitempty"`
	ImportedPins  []ImportedPinMapping    `json:"imported_pins,omitempty"`
	ReviewSeq     uint64                  `json:"review_seq,omitempty"`
	Reclaimed     *ReclaimResult          `json:"reclaimed,omitempty"`
}

// ReclaimOutcome is the outcome of Store.ReclaimSealedAncestor.
type ReclaimOutcome string

const (
	ReclaimOutcomeReclaimed        ReclaimOutcome = "reclaimed"
	ReclaimOutcomeAlreadyReclaimed ReclaimOutcome = "already_reclaimed"
	ReclaimOutcomeRefused          ReclaimOutcome = "refused"
)

// ReclaimRefusalReason identifies why Store.ReclaimSealedAncestor refused reclamation.
type ReclaimRefusalReason string

const (
	ReclaimRefusedNotSealed           ReclaimRefusalReason = "source_not_sealed"
	ReclaimRefusedHeld                ReclaimRefusalReason = "source_held"
	ReclaimRefusedRestrictionsPending ReclaimRefusalReason = "restrictions_pending"
	ReclaimRefusedBodyReferenced      ReclaimRefusalReason = "external_body_referenced"
	ReclaimRefusedObligationPending   ReclaimRefusalReason = "obligation_pending"
)

// ReclaimRequest configures Store.ReclaimSealedAncestor.
type ReclaimRequest struct {
	PredecessorStream  string   `json:"predecessor_stream"`
	SuccessorStream    string   `json:"successor_stream"`
	ArchiveDir         string   `json:"archive_dir,omitempty"`
	PendingObligations []string `json:"pending_obligations,omitempty"`
}

// ReclaimResult is returned by Store.ReclaimSealedAncestor.
type ReclaimResult struct {
	Outcome               ReclaimOutcome       `json:"outcome"`
	RefusalReason         ReclaimRefusalReason `json:"refusal_reason,omitempty"`
	PredecessorStream     string               `json:"predecessor_stream"`
	SuccessorStream       string               `json:"successor_stream"`
	SealDigest            string               `json:"seal_digest,omitempty"`
	ReclaimedJournalBytes int64                `json:"reclaimed_journal_bytes"`
	ReclaimedIndexBytes   int64                `json:"reclaimed_index_bytes"`
	ArchivedJournalPath   string               `json:"archived_journal_path,omitempty"`
	ArchivedSealPath      string               `json:"archived_seal_path,omitempty"`
}

// StreamStorageUsage reports logical vs. physical byte accounting for one stream.
type StreamStorageUsage struct {
	Stream                     string `json:"stream"`
	Sealed                     bool   `json:"sealed"`
	Reclaimed                  bool   `json:"reclaimed"`
	LogicalJournalBytes        int64  `json:"logical_journal_bytes"`
	DerivedIndexBytes          int64  `json:"derived_index_bytes"`
	SealMetadataBytes          int64  `json:"seal_metadata_bytes"`
	TempRewriteBytes           int64  `json:"temp_rewrite_bytes"`
	MaxSingleIndexRewriteBytes int64  `json:"max_single_index_rewrite_bytes"`
	PhysicalAllocatedBytes     int64  `json:"physical_allocated_bytes"`
}

// StoreStorageUsage reports logical vs. physical byte accounting across a Store.
type StoreStorageUsage struct {
	LogicalJournalBytes        int64                `json:"logical_journal_bytes"`
	ActiveLogicalJournalBytes  int64                `json:"active_logical_journal_bytes"`
	SealedLogicalJournalBytes  int64                `json:"sealed_logical_journal_bytes"`
	DerivedIndexBytes          int64                `json:"derived_index_bytes"`
	SealMetadataBytes          int64                `json:"seal_metadata_bytes"`
	BodyBytes                  int64                `json:"body_bytes"`
	TempRewriteBytes           int64                `json:"temp_rewrite_bytes"`
	MaxSingleIndexRewriteBytes int64                `json:"max_single_index_rewrite_bytes"`
	PhysicalAllocatedBytes     int64                `json:"physical_allocated_bytes"`
	Streams                    []StreamStorageUsage `json:"streams,omitempty"`
}

type reclaimedPredecessorDoc struct {
	V                                  int                     `json:"v"`
	PredecessorStream                  string                  `json:"predecessor_stream"`
	SuccessorStream                    string                  `json:"successor_stream"`
	ContinuesStream                    string                  `json:"continues_stream,omitempty"`
	CutSeq                             uint64                  `json:"cut_seq"`
	CutDigest                          string                  `json:"cut_digest"`
	SealDigest                         string                  `json:"seal_digest"`
	EffectiveRestrictionManifestDigest string                  `json:"effective_restriction_manifest_digest"`
	Manifest                           json.RawMessage         `json:"manifest"`
	HasAncestorHold                    bool                    `json:"has_ancestor_hold"`
	KnownPins                          []Pin                   `json:"known_pins"`
	KnownPinSeqs                       []uint64                `json:"known_pin_seqs,omitempty"`
	HistoricalCuts                     []journal.SealedCutInfo `json:"historical_cuts,omitempty"`
	ImportedBindings                   []ImportedPinMapping    `json:"imported_bindings,omitempty"`
	UnretiredBodies                    []journal.BodyKey       `json:"unretired_bodies,omitempty"`
	ReclaimedJournalBytes              int64                   `json:"reclaimed_journal_bytes"`
	ReclaimedIndexBytes                int64                   `json:"reclaimed_index_bytes"`
	ArchivedJournalPath                string                  `json:"archived_journal_path,omitempty"`
	ArchivedSealPath                   string                  `json:"archived_seal_path,omitempty"`
	ReclaimedAt                        string                  `json:"reclaimed_at"`
	MAC                                string                  `json:"mac,omitempty"`
}

func computeReclaimedMAC(diagKey []byte, doc reclaimedPredecessorDoc) (string, error) {
	cp := doc
	cp.MAC = ""
	canonBytes, err := canonicalJSON(cp)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, diagKey)
	mac.Write([]byte("recordprims/reclaimed_predecessor/v1\n"))
	mac.Write(canonBytes)
	return " hmac-sha256:"[1:] + hex.EncodeToString(mac.Sum(nil)), nil
}

// ContinuationState returns the current restriction-review state of st under
// the store lock.
func (st *Stream) ContinuationState() (ContinuationState, error) {
	var out ContinuationState
	err := st.store.withLock(func() error {
		rep, err := st.store.replayStream(st.id)
		if err != nil {
			return err
		}
		out.DestinationCutSeq = uint64(rep.lastSeq)
		appDig, err := rep.st.AppliedRestrictionManifestDigest()
		if err != nil {
			return &ErrJournalInvalid{Reason: rejectReason(err)}
		}
		out.DestinationAppliedStateDigest = appDig
		if rep.continues == nil {
			return nil
		}
		cp := *rep.continues
		out.IsContinuation = true
		out.Continues = &cp
		out.ReviewPending = rep.st.ReviewPending()
		srcDig, err := rep.st.SourceManifestDigest()
		if err != nil {
			return &ErrJournalInvalid{Reason: rejectReason(err)}
		}
		out.SourceStateDigest = srcDig
		if revNode := rep.st.ReviewNode(); revNode != nil {
			out.LastReviewSeq = uint64(revNode.Get("seq").Int)
			out.LastReviewAt = revNode.Get("at").Str
		}
		srcSealPath := streamDir(rep.continues.Stream) + "/stream-seal.json"
		if hasSeal, serr := st.store.exists(srcSealPath); serr == nil && hasSeal {
			if rawSeal, rerr := readFileVerified(st.store.root, srcSealPath, st.store.fso, maxJournalBytes); rerr == nil {
				if sealDig, derr := canon.Digest(rawSeal); derr == nil {
					out.SourceSealed = true
					out.SourceSealDigest = sealDig
				}
			}
		}
		return nil
	})
	return out, err
}

// EstablishRestrictions records a restrictions_reviewed entry on a continuation
// stream after verifying that the caller-supplied source and destination state
// digests match the current replayed state under the store lock.
func (st *Stream) EstablishRestrictions(ctx context.Context, rev RestrictionReview) (RestrictionReviewResult, error) {
	if err := ctx.Err(); err != nil {
		return RestrictionReviewResult{}, err
	}
	if !validProvenance(rev.Reviewer) {
		return RestrictionReviewResult{}, fmt.Errorf("%w: invalid reviewer provenance", ErrSchema)
	}
	if !validAuthorization(&rev.Authorization) {
		return RestrictionReviewResult{}, fmt.Errorf("%w: invalid authorization", ErrSchema)
	}
	if rev.Note != "" && !validWireString(rev.Note, 1, 1024) {
		return RestrictionReviewResult{}, fmt.Errorf("%w: invalid review note", ErrSchema)
	}
	if rev.SourceCutSeq < 1 || !reDigest.MatchString(rev.SourceCutDigest) ||
		!reDigest.MatchString(rev.SourceStateDigest) ||
		rev.DestinationCutSeq < 1 || !reDigest.MatchString(rev.DestinationAppliedStateDigest) {
		return RestrictionReviewResult{}, fmt.Errorf("%w: invalid restriction review digests or cuts", ErrSchema)
	}

	var res RestrictionReviewResult
	err := st.store.withLock(func() error {
		rep, err := st.store.replayStream(st.id)
		if err != nil {
			return err
		}
		if rep.sealed {
			return ErrStreamSealed
		}
		return st.store.establishRestrictionsLocked(rep, rev, &res, true)
	})
	if err != nil {
		return RestrictionReviewResult{}, err
	}
	return res, nil
}

func (s *Store) sourceRetiredRecordIDs(streamID string, seen map[string]bool) (map[string]bool, bool) {
	sh := streamHash(streamID)
	if seen[sh] || len(seen) > 16 {
		return nil, false
	}
	nextSeen := copySeenMap(seen)
	nextSeen[sh] = true
	out := map[string]bool{}
	sdir := streamDir(streamID)
	if reclaimed, err := s.exists(sdir + "/reclaimed.json"); err != nil {
		return nil, false
	} else if reclaimed {
		doc, _, ok := s.loadAndVerifyReclaimedDoc(streamID, copySeenMap(nextSeen))
		if !ok {
			return nil, false
		}
		mNode, err := wire.ParseJSON(doc.Manifest)
		if err != nil {
			return nil, false
		}
		if retArr := mNode.Get("retired"); retArr != nil && retArr.Kind == wire.Array {
			for _, el := range retArr.Elems {
				if el != nil && el.Kind == wire.Object && el.Get("record_id") != nil {
					out[el.Get("record_id").Str] = true
				}
			}
		}
		if doc.ContinuesStream != "" {
			upRet, uok := s.sourceRetiredRecordIDs(doc.ContinuesStream, nextSeen)
			if !uok {
				return nil, false
			}
			for k := range upRet {
				out[k] = true
			}
		}
		return out, true
	}
	srcSt, _, ok := s.replaySourceStreamDir(sh, copySeenMap(nextSeen))
	if !ok {
		return nil, false
	}
	for pk := range srcSt.Records() {
		if srcSt.IsRetired(pk.RecordID) {
			out[pk.RecordID] = true
		}
	}
	if cn := srcSt.ContinuesNode(); cn != nil {
		upRet, uok := s.sourceRetiredRecordIDs(cn.Get("stream").Str, nextSeen)
		if !uok {
			return nil, false
		}
		for k := range upRet {
			out[k] = true
		}
	}
	return out, true
}

func (s *Store) establishRestrictionsLocked(rep *replayedStream, rev RestrictionReview, res *RestrictionReviewResult, syncIdx bool) error {
	if rep.continues == nil {
		return fmt.Errorf("%w: stream is not a continuation", ErrSchema)
	}
	srcRetired, ok := s.sourceRetiredRecordIDs(rep.continues.Stream, map[string]bool{streamHash(rep.st.Stream()): true})
	if !ok {
		return &ErrJournalInvalid{Reason: journal.ReasonContinuation}
	}
	for pk := range rep.st.Records() {
		if srcRetired[pk.RecordID] && !rep.st.IsRetired(pk.RecordID) {
			return fmt.Errorf("%w: unimported_retired_record_id: %s", ErrSchema, pk.RecordID)
		}
	}
	// Check idempotent retry: if the tail entry is already a matching restrictions_reviewed
	// and ReviewPending() is false, return RestrictionReviewAlreadyCompleted.
	if revNode := rep.st.ReviewNode(); revNode != nil && !rep.st.ReviewPending() {
		srcNode := revNode.Get("source")
		if srcNode != nil &&
			uint64(srcNode.Get("cut_seq").Int) == rev.SourceCutSeq &&
			srcNode.Get("cut_digest").Str == rev.SourceCutDigest &&
			revNode.Get("source_state_digest").Str == rev.SourceStateDigest &&
			uint64(revNode.Get("destination_cut_seq").Int) == rev.DestinationCutSeq &&
			revNode.Get("destination_applied_state_digest").Str == rev.DestinationAppliedStateDigest {
			*res = RestrictionReviewResult{
				Outcome: RestrictionReviewAlreadyCompleted,
				Seq:     uint64(revNode.Get("seq").Int),
				At:      revNode.Get("at").Str,
			}
			return nil
		}
	}

	if uint64(rep.lastSeq) != rev.DestinationCutSeq {
		return ErrStaleReview
	}
	curSrcDig, err := rep.st.SourceManifestDigestAt(int64(rev.SourceCutSeq))
	if err != nil || curSrcDig != rev.SourceStateDigest {
		return ErrStaleReview
	}
	curCutDig, err := rep.st.SourceCutDigestAt(int64(rev.SourceCutSeq))
	if err != nil || curCutDig != rev.SourceCutDigest {
		return ErrStaleReview
	}
	curAppDig, err := rep.st.AppliedRestrictionManifestDigest()
	if err != nil || curAppDig != rev.DestinationAppliedStateDigest {
		return ErrStaleReview
	}

	if rep.lastSeq >= maxSafeInteger {
		return fmt.Errorf("%w: stream seq exhausted", ErrSchema)
	}
	now := s.opts.Clock.Now().UTC().Truncate(time.Microsecond)
	if now.Before(rep.lastAt) {
		return ErrClockRegression
	}
	atStr := FormatInstant(now)
	seq := rep.lastSeq + 1
	seqTok := strconv.FormatInt(seq, 10)
	srcCutTok := strconv.FormatUint(rev.SourceCutSeq, 10)
	dstCutTok := strconv.FormatUint(rev.DestinationCutSeq, 10)

	fields := []wire.Member{
		{Name: "v", Value: &wire.Node{Kind: wire.Number, Token: "1", IsInt: true, Int: 1}},
		{Name: "seq", Value: &wire.Node{Kind: wire.Number, Token: seqTok, IsInt: true, Int: seq}},
		{Name: "group", Value: &wire.Node{Kind: wire.Number, Token: seqTok, IsInt: true, Int: seq}},
		{Name: "group_size", Value: &wire.Node{Kind: wire.Number, Token: "1", IsInt: true, Int: 1}},
		{Name: "stream", Value: &wire.Node{Kind: wire.String, Str: rep.st.Stream()}},
		{Name: "type", Value: &wire.Node{Kind: wire.String, Str: "restrictions_reviewed"}},
		{Name: "at", Value: &wire.Node{Kind: wire.String, Str: atStr}},
		{Name: "source", Value: &wire.Node{
			Kind: wire.Object,
			Fields: []wire.Member{
				{Name: "capability", Value: &wire.Node{Kind: wire.String, Str: rep.continues.Capability}},
				{Name: "stream", Value: &wire.Node{Kind: wire.String, Str: rep.continues.Stream}},
				{Name: "cut_seq", Value: &wire.Node{Kind: wire.Number, Token: srcCutTok, IsInt: true, Int: int64(rev.SourceCutSeq)}},
				{Name: "cut_digest", Value: &wire.Node{Kind: wire.String, Str: rev.SourceCutDigest}},
			},
		}},
		{Name: "source_state_digest", Value: &wire.Node{Kind: wire.String, Str: rev.SourceStateDigest}},
		{Name: "destination_cut_seq", Value: &wire.Node{Kind: wire.Number, Token: dstCutTok, IsInt: true, Int: int64(rev.DestinationCutSeq)}},
		{Name: "destination_applied_state_digest", Value: &wire.Node{Kind: wire.String, Str: rev.DestinationAppliedStateDigest}},
		{Name: "provenance", Value: provenanceNode(rev.Reviewer)},
		{Name: "authorization", Value: &wire.Node{
			Kind: wire.Object,
			Fields: []wire.Member{
				{Name: "ref", Value: &wire.Node{Kind: wire.String, Str: rev.Authorization.Ref}},
				{Name: "digest", Value: &wire.Node{Kind: wire.String, Str: rev.Authorization.Digest}},
			},
		}},
	}
	entry := &wire.Node{Kind: wire.Object, Fields: fields}
	if err := shape.Validate("v1/journal-entry.schema.json", entry); err != nil {
		return fmt.Errorf("%w: restrictions_reviewed entry shape", ErrSchema)
	}
	if err := rep.st.Ingest(entry); err != nil {
		return fmt.Errorf("%w: %s", ErrSchema, rejectReason(err))
	}
	line := append(wire.Encode(entry), '\n')
	if syncIdx {
		if err := rep.appendLine(s, line); err != nil {
			return err
		}
	} else {
		if err := rep.appendLineNoIndexSync(s, line); err != nil {
			return err
		}
	}
	rep.lastAt = now
	*res = RestrictionReviewResult{
		Outcome: RestrictionReviewCompleted,
		Seq:     uint64(seq),
		At:      atStr,
	}
	return nil
}

// SealStream creates and persists a detached durable-record/v1 stream-seal.json
// for req.Stream at its current cut, fencing any subsequent writes to the stream.
func (s *Store) SealStream(ctx context.Context, req SealStreamRequest) (SealStreamResult, error) {
	if err := ctx.Err(); err != nil {
		return SealStreamResult{}, err
	}
	if !reStream.MatchString(req.Stream) {
		return SealStreamResult{}, fmt.Errorf("%w: invalid stream id", ErrSchema)
	}
	if req.Stream == s.profile.Runtime.BodyRemovalStream {
		return SealStreamResult{}, fmt.Errorf("%w: cannot seal designated body-removal stream", ErrSchema)
	}
	if !validProvenance(req.Provenance) {
		return SealStreamResult{}, fmt.Errorf("%w: invalid seal provenance", ErrSchema)
	}
	if !validAuthorization(&req.Authorization) {
		return SealStreamResult{}, fmt.Errorf("%w: invalid seal authorization", ErrSchema)
	}

	var res SealStreamResult
	err := s.withLock(func() error {
		return s.sealStreamLocked(req, &res)
	})
	if err != nil {
		return SealStreamResult{}, err
	}
	return res, nil
}

func defaultSealTrustProfileNode() *wire.Node {
	return &wire.Node{
		Kind: wire.Object,
		Fields: []wire.Member{
			{Name: "id", Value: &wire.Node{Kind: wire.String, Str: "recordprims-fs"}},
			{Name: "version", Value: &wire.Node{Kind: wire.String, Str: "v2"}},
		},
	}
}

func (s *Store) sealStreamLocked(req SealStreamRequest, res *SealStreamResult) error {
	if len(req.PendingObligations) > 0 {
		*res = SealStreamResult{
			Outcome:       SealOutcomeRefused,
			RefusalReason: SealRefusedObligationPending,
		}
		return nil
	}
	sdir := streamDir(req.Stream)
	if reclaimed, err := s.exists(sdir + "/reclaimed.json"); err != nil {
		return err
	} else if reclaimed {
		seal, ok := s.loadAndVerifySealDoc(req.Stream)
		if !ok {
			return &ErrJournalInvalid{Reason: journal.ReasonContinuation}
		}
		*res = SealStreamResult{
			Outcome: SealOutcomeAlreadySealed,
			Seal:    &seal,
		}
		return nil
	}

	rep, err := s.replayStream(req.Stream)
	if err != nil {
		return err
	}
	if rep.sealed {
		seal, ok := s.loadAndVerifySealDoc(req.Stream)
		if !ok {
			return &ErrJournalInvalid{Reason: journal.ReasonContinuation}
		}
		*res = SealStreamResult{
			Outcome: SealOutcomeAlreadySealed,
			Seal:    &seal,
		}
		return nil
	}
	if len(rep.st.ActiveHolds()) > 0 || rep.st.HasAncestorHold() {
		*res = SealStreamResult{
			Outcome:       SealOutcomeRefused,
			RefusalReason: SealRefusedHeld,
		}
		return nil
	}
	if rep.st.ReviewPending() {
		*res = SealStreamResult{
			Outcome:       SealOutcomeRefused,
			RefusalReason: SealRefusedRestrictionsPending,
		}
		return nil
	}

	// Collect transitive ancestor seals if this stream is a continuation.
	var ancRefs []AncestorSealRef
	var ancNodes []*wire.Node
	if rep.continues != nil {
		refs, nodes, ok := s.collectAncestorSeals(rep.continues.Stream, map[string]bool{streamHash(req.Stream): true})
		if !ok {
			*res = SealStreamResult{
				Outcome:       SealOutcomeRefused,
				RefusalReason: SealRefusedRestrictionsPending,
			}
			return nil
		}
		ancRefs = refs
		ancNodes = nodes
	} else {
		ancRefs = []AncestorSealRef{}
		ancNodes = []*wire.Node{}
	}

	cutDig, err := rep.st.CutDigest(rep.lastSeq)
	if err != nil {
		return &ErrJournalInvalid{Reason: rejectReason(err)}
	}
	effDig, err := rep.st.EffectiveRestrictionManifestDigest()
	if err != nil {
		return &ErrJournalInvalid{Reason: rejectReason(err)}
	}
	now := s.opts.Clock.Now().UTC().Truncate(time.Microsecond)
	if now.Before(rep.lastAt) {
		return ErrClockRegression
	}
	sealedAt := FormatInstant(now)
	cutTok := strconv.FormatInt(rep.lastSeq, 10)
	trustNode := defaultSealTrustProfileNode()

	sealNode := &wire.Node{
		Kind: wire.Object,
		Fields: []wire.Member{
			{Name: "v", Value: &wire.Node{Kind: wire.Number, Token: "1", IsInt: true, Int: 1}},
			{Name: "store", Value: &wire.Node{Kind: wire.String, Str: s.meta.StoreID}},
			{Name: "capability", Value: &wire.Node{Kind: wire.String, Str: "contract: durable-record/v1"}},
			{Name: "stream", Value: &wire.Node{Kind: wire.String, Str: req.Stream}},
			{Name: "cut_seq", Value: &wire.Node{Kind: wire.Number, Token: cutTok, IsInt: true, Int: rep.lastSeq}},
			{Name: "cut_digest", Value: &wire.Node{Kind: wire.String, Str: cutDig}},
			{Name: "effective_restriction_manifest_digest", Value: &wire.Node{Kind: wire.String, Str: effDig}},
			{Name: "ancestor_seals", Value: &wire.Node{Kind: wire.Array, Elems: ancNodes}},
			{Name: "trust_profile", Value: trustNode},
			{Name: "sealed_at", Value: &wire.Node{Kind: wire.String, Str: sealedAt}},
			{Name: "provenance", Value: provenanceNode(req.Provenance)},
			{Name: "authorization", Value: &wire.Node{
				Kind: wire.Object,
				Fields: []wire.Member{
					{Name: "ref", Value: &wire.Node{Kind: wire.String, Str: req.Authorization.Ref}},
					{Name: "digest", Value: &wire.Node{Kind: wire.String, Str: req.Authorization.Digest}},
				},
			}},
		},
	}
	if err := shape.Validate("v1/stream-seal.schema.json", sealNode); err != nil {
		return fmt.Errorf("%w: stream seal shape: %v", ErrSchema, err)
	}
	canonSeal, err := canon.Canonicalize(wire.Encode(sealNode))
	if err != nil {
		return fmt.Errorf("%w: canonicalize stream seal: %v", ErrSchema, err)
	}
	sealDig := sha256HexPrefixed(canonSeal)
	sealData := append(canonSeal, '\n')
	if err := s.writeIndexFileAtomic(sdir, "stream-seal.json", sealData); err != nil {
		return err
	}
	if !s.verifySealedStreamState(req.Stream, rep) {
		_ = s.root.Remove(sdir + "/stream-seal.json")
		return &ErrJournalInvalid{Reason: journal.ReasonContinuation}
	}
	rep.sealed = true
	*res = SealStreamResult{
		Outcome: SealOutcomeSealed,
		Seal: &StreamSeal{
			V:                                  1,
			Store:                              s.meta.StoreID,
			Capability:                         "contract: durable-record/v1",
			Stream:                             req.Stream,
			CutSeq:                             uint64(rep.lastSeq),
			CutDigest:                          cutDig,
			EffectiveRestrictionManifestDigest: effDig,
			AncestorSeals:                      ancRefs,
			TrustProfile: SealTrustProfile{
				ID:      "recordprims-fs",
				Version: "v2",
			},
			SealedAt:      sealedAt,
			Provenance:    req.Provenance,
			Authorization: req.Authorization,
			SealDigest:    sealDig,
			Raw:           append([]byte(nil), canonSeal...),
		},
	}
	return nil
}

// VerifyStreamSeal reads and verifies the detached stream-seal.json for streamID
// against the stream's journal (or its verified reclaimed-predecessor summary
// after Store.ReclaimSealedAncestor).
func (s *Store) VerifyStreamSeal(streamID string) (StreamSeal, bool, error) {
	if !reStream.MatchString(streamID) {
		return StreamSeal{}, false, fmt.Errorf("%w: invalid stream id", ErrSchema)
	}
	var out StreamSeal
	var found bool
	err := s.withLock(func() error {
		sdir := streamDir(streamID)
		hasSeal, err := s.exists(sdir + "/stream-seal.json")
		if err != nil {
			return err
		}
		if !hasSeal {
			return nil
		}
		reclaimed, err := s.exists(sdir + "/reclaimed.json")
		if err != nil {
			return err
		}
		if reclaimed {
			if _, ok := s.verifyReclaimedPredecessorDir(streamID, map[string]bool{}); !ok {
				return &ErrJournalInvalid{Reason: journal.ReasonContinuation}
			}
		} else {
			rep, err := s.replayStream(streamID)
			if err != nil {
				return err
			}
			if !rep.sealed {
				return &ErrJournalInvalid{Reason: journal.ReasonContinuation}
			}
		}
		seal, ok := s.loadAndVerifySealDoc(streamID)
		if !ok {
			return &ErrJournalInvalid{Reason: journal.ReasonContinuation}
		}
		found = true
		out = seal
		return nil
	})
	return out, found, err
}

func (s *Store) loadAndVerifySealDoc(streamID string) (StreamSeal, bool) {
	sdir := streamDir(streamID)
	raw, err := readFileVerified(s.root, sdir+"/stream-seal.json", s.fso, maxJournalBytes)
	if err != nil {
		return StreamSeal{}, false
	}
	trimmed := bytes.TrimSuffix(raw, []byte{'\n'})
	node, err := wire.Ingest(trimmed, wire.KindStreamSeal)
	if err != nil {
		return StreamSeal{}, false
	}
	if err := shape.Validate("v1/stream-seal.schema.json", node); err != nil {
		return StreamSeal{}, false
	}
	if node.Get("store").Str != s.meta.StoreID ||
		node.Get("capability").Str != "contract: durable-record/v1" ||
		node.Get("stream").Str != streamID {
		return StreamSeal{}, false
	}
	tpNode := node.Get("trust_profile")
	if tpNode == nil || tpNode.Get("id").Str != "recordprims-fs" || tpNode.Get("version").Str != "v2" {
		return StreamSeal{}, false
	}
	if _, ok := journal.ParseInstant(node.Get("sealed_at").Str); !ok {
		return StreamSeal{}, false
	}
	canonBytes, err := canon.Canonicalize(trimmed)
	if err != nil {
		return StreamSeal{}, false
	}
	var parsed StreamSeal
	if err := json.Unmarshal(canonBytes, &parsed); err != nil {
		return StreamSeal{}, false
	}
	if parsed.AncestorSeals == nil {
		parsed.AncestorSeals = []AncestorSealRef{}
	}
	parsed.SealDigest = sha256HexPrefixed(canonBytes)
	parsed.Raw = canonBytes
	return parsed, true
}

func (s *Store) collectAncestorSeals(srcStream string, seen map[string]bool) ([]AncestorSealRef, []*wire.Node, bool) {
	byStream := map[string]AncestorSealRef{}
	cur := srcStream
	for cur != "" {
		sh := streamHash(cur)
		if seen[sh] || len(seen) > 16 {
			return nil, nil, false
		}
		seen[sh] = true
		seal, ok := s.loadAndVerifySealDoc(cur)
		if !ok {
			return nil, nil, false
		}
		sdir := streamDir(cur)
		if reclaimed, err := s.exists(sdir + "/reclaimed.json"); err != nil {
			return nil, nil, false
		} else if reclaimed {
			if _, ok := s.verifyReclaimedPredecessorDir(cur, copySeenMap(seen)); !ok {
				return nil, nil, false
			}
			byStream[cur] = AncestorSealRef{
				Store:      seal.Store,
				Capability: seal.Capability,
				Stream:     seal.Stream,
				CutSeq:     seal.CutSeq,
				SealDigest: seal.SealDigest,
			}
			for _, ar := range seal.AncestorSeals {
				byStream[ar.Stream] = ar
			}
			break
		}
		st, sealedFinal, ok := s.replaySourceStreamDir(sh, copySeenMap(seen))
		if !ok || !sealedFinal {
			return nil, nil, false
		}
		byStream[cur] = AncestorSealRef{
			Store:      seal.Store,
			Capability: seal.Capability,
			Stream:     seal.Stream,
			CutSeq:     seal.CutSeq,
			SealDigest: seal.SealDigest,
		}
		for _, ar := range seal.AncestorSeals {
			byStream[ar.Stream] = ar
		}
		if cn := st.ContinuesNode(); cn != nil {
			cur = cn.Get("stream").Str
		} else {
			cur = ""
		}
	}

	type pair struct {
		jcs  string
		ref  AncestorSealRef
		node *wire.Node
	}
	var pairs []pair
	for _, r := range byStream {
		cutTok := strconv.FormatUint(r.CutSeq, 10)
		n := &wire.Node{
			Kind: wire.Object,
			Fields: []wire.Member{
				{Name: "store", Value: &wire.Node{Kind: wire.String, Str: r.Store}},
				{Name: "capability", Value: &wire.Node{Kind: wire.String, Str: r.Capability}},
				{Name: "stream", Value: &wire.Node{Kind: wire.String, Str: r.Stream}},
				{Name: "cut_seq", Value: &wire.Node{Kind: wire.Number, Token: cutTok, IsInt: true, Int: int64(r.CutSeq)}},
				{Name: "seal_digest", Value: &wire.Node{Kind: wire.String, Str: r.SealDigest}},
			},
		}
		jcsBytes, err := canon.Canonicalize(wire.Encode(n))
		if err != nil {
			return nil, nil, false
		}
		pairs = append(pairs, pair{jcs: string(jcsBytes), ref: r, node: n})
	}
	slices.SortFunc(pairs, func(a, b pair) int {
		return strings.Compare(a.jcs, b.jcs)
	})
	refs := make([]AncestorSealRef, len(pairs))
	nodes := make([]*wire.Node, len(pairs))
	for i, p := range pairs {
		refs[i] = p.ref
		nodes[i] = p.node
	}
	return refs, nodes, true
}

func (s *Store) verifySealedStreamState(streamID string, rep *replayedStream) bool {
	sdir := streamDir(streamID)
	rawSeal, err := readFileVerified(s.root, sdir+"/stream-seal.json", s.fso, maxJournalBytes)
	if err != nil {
		return false
	}
	trimmed := bytes.TrimSuffix(rawSeal, []byte{'\n'})
	sealNode, err := wire.Ingest(trimmed, wire.KindStreamSeal)
	if err != nil {
		return false
	}
	if err := shape.Validate("v1/stream-seal.schema.json", sealNode); err != nil {
		return false
	}
	if sealNode.Get("store").Str != s.meta.StoreID ||
		sealNode.Get("capability").Str != "contract: durable-record/v1" ||
		sealNode.Get("stream").Str != streamID {
		return false
	}
	cutSeq := sealNode.Get("cut_seq").Int
	if cutSeq != rep.lastSeq {
		return false
	}
	mDoc, effDig, err := rep.st.EffectiveRestrictionManifest()
	if err != nil || effDig != sealNode.Get("effective_restriction_manifest_digest").Str {
		return false
	}
	cutDig, err := rep.st.CutDigest(rep.lastSeq)
	if err != nil || cutDig != sealNode.Get("cut_digest").Str {
		return false
	}
	ancSeq := sealNode.Get("ancestor_seals")
	var ancCtxNodes []*wire.Node
	if rep.st.ContinuesNode() == nil {
		if ancSeq == nil || len(ancSeq.Elems) != 0 {
			return false
		}
		ancCtxNodes = []*wire.Node{}
	} else {
		if ancSeq == nil || len(ancSeq.Elems) == 0 {
			return false
		}
		// Verify every referenced ancestor seal exists and is valid.
		seen := map[string]bool{streamHash(streamID): true}
		for _, el := range ancSeq.Elems {
			ancStream := el.Get("stream").Str
			ancStore := el.Get("store").Str
			ancCap := el.Get("capability").Str
			ancCutSeq := uint64(el.Get("cut_seq").Int)
			ancSealDig := el.Get("seal_digest").Str
			if ancStore != s.meta.StoreID || ancCap != "contract: durable-record/v1" {
				return false
			}
			ancSeal, ok := s.loadAndVerifySealDoc(ancStream)
			if !ok || ancSeal.CutSeq != ancCutSeq || ancSeal.SealDigest != ancSealDig {
				return false
			}
			ancSealNode, err := wire.Ingest(ancSeal.Raw, wire.KindStreamSeal)
			if err != nil {
				return false
			}
			ancDir := streamDir(ancStream)
			var ancManifest *wire.Node
			if reclaimed, rerr := s.exists(ancDir + "/reclaimed.json"); rerr != nil {
				return false
			} else if reclaimed {
				recDoc, _, rok := s.loadAndVerifyReclaimedDoc(ancStream, copySeenMap(seen))
				if !rok {
					return false
				}
				mNode, merr := wire.ParseJSON(recDoc.Manifest)
				if merr != nil {
					return false
				}
				ancManifest = mNode
			} else {
				ancSt, ancSealed, aok := s.replaySourceStreamDir(streamHash(ancStream), copySeenMap(seen))
				if !aok || !ancSealed {
					return false
				}
				mNode, _, merr := ancSt.EffectiveRestrictionManifest()
				if merr != nil {
					return false
				}
				ancManifest = mNode
			}
			ancCtxNodes = append(ancCtxNodes, &wire.Node{
				Kind: wire.Object,
				Fields: []wire.Member{
					{Name: "seal", Value: ancSealNode},
					{Name: "closer_authenticated", Value: &wire.Node{Kind: wire.Bool, Bool: true}},
					{Name: "source_write_fenced", Value: &wire.Node{Kind: wire.Bool, Bool: true}},
					{Name: "expected_store", Value: &wire.Node{Kind: wire.String, Str: s.meta.StoreID}},
					{Name: "accepted_trust_profile", Value: defaultSealTrustProfileNode()},
					{Name: "manifest", Value: ancManifest},
				},
			})
		}
	}
	stepNode := &wire.Node{
		Kind: wire.Object,
		Fields: []wire.Member{
			{Name: "seal", Value: sealNode},
			{Name: "closer_authenticated", Value: &wire.Node{Kind: wire.Bool, Bool: true}},
			{Name: "source_write_fenced", Value: &wire.Node{Kind: wire.Bool, Bool: true}},
			{Name: "expected_store", Value: &wire.Node{Kind: wire.String, Str: s.meta.StoreID}},
			{Name: "accepted_trust_profile", Value: defaultSealTrustProfileNode()},
			{Name: "manifest", Value: mDoc},
			{Name: "journal", Value: &wire.Node{Kind: wire.Array, Elems: rep.st.Journal()}},
			{Name: "ancestor_seal_contexts", Value: &wire.Node{Kind: wire.Array, Elems: ancCtxNodes}},
		},
	}
	outNode, err := rep.st.VerifySealStep(stepNode)
	if err != nil {
		return false
	}
	return outNode.Get("outcome").Str == "seal_verified" &&
		outNode.Get("final") != nil &&
		outNode.Get("final").Bool
}

func (s *Store) loadAndVerifyReclaimedDoc(streamID string, seen map[string]bool) (reclaimedPredecessorDoc, *journal.SealedSourceSummary, bool) {
	sh := streamHash(streamID)
	if len(seen) > 16 {
		return reclaimedPredecessorDoc{}, nil, false
	}
	nextSeen := copySeenMap(seen)
	nextSeen[sh] = true
	sdir := streamDir(streamID)
	diagKey, err := readFileVerified(s.root, "diag.key", s.fso, keyLen)
	if err != nil {
		return reclaimedPredecessorDoc{}, nil, false
	}
	seal, ok := s.loadAndVerifySealDoc(streamID)
	if !ok {
		return reclaimedPredecessorDoc{}, nil, false
	}
	rawRec, err := readFileVerified(s.root, sdir+"/reclaimed.json", s.fso, maxJournalBytes)
	if err != nil {
		return reclaimedPredecessorDoc{}, nil, false
	}
	var doc reclaimedPredecessorDoc
	if err := json.Unmarshal(rawRec, &doc); err != nil {
		return reclaimedPredecessorDoc{}, nil, false
	}
	if doc.V != 1 ||
		doc.PredecessorStream != streamID ||
		!reStream.MatchString(doc.SuccessorStream) ||
		doc.CutSeq != seal.CutSeq ||
		doc.CutDigest != seal.CutDigest ||
		doc.SealDigest != seal.SealDigest ||
		doc.EffectiveRestrictionManifestDigest != seal.EffectiveRestrictionManifestDigest ||
		doc.MAC == "" {
		return reclaimedPredecessorDoc{}, nil, false
	}
	wantMAC, err := computeReclaimedMAC(diagKey, doc)
	if err != nil || !hmac.Equal([]byte(doc.MAC), []byte(wantMAC)) {
		return reclaimedPredecessorDoc{}, nil, false
	}
	mNode, err := wire.ParseJSON(doc.Manifest)
	if err != nil {
		return reclaimedPredecessorDoc{}, nil, false
	}
	hasHolds, err := journal.VerifyManifestPreimage(mNode, seal.EffectiveRestrictionManifestDigest)
	if err != nil || hasHolds || doc.HasAncestorHold {
		return reclaimedPredecessorDoc{}, nil, false
	}
	// Verify any ancestor seals referenced in seal.AncestorSeals and enforce exact reachability.
	if doc.ContinuesStream == "" && len(seal.AncestorSeals) != 0 {
		return reclaimedPredecessorDoc{}, nil, false
	}
	upstreamOf := make(map[string]string, len(seal.AncestorSeals))
	for _, ar := range seal.AncestorSeals {
		if ar.Store != s.meta.StoreID || ar.Capability != "contract: durable-record/v1" {
			return reclaimedPredecessorDoc{}, nil, false
		}
		arHash := streamHash(ar.Stream)
		if nextSeen[arHash] {
			return reclaimedPredecessorDoc{}, nil, false
		}
		ancSeal, aok := s.loadAndVerifySealDoc(ar.Stream)
		if !aok || ancSeal.CutSeq != ar.CutSeq || ancSeal.SealDigest != ar.SealDigest {
			return reclaimedPredecessorDoc{}, nil, false
		}
		ancDir := streamDir(ar.Stream)
		ancSeen := copySeenMap(nextSeen)
		ancSeen[arHash] = true
		if ancReclaimed, rerr := s.exists(ancDir + "/reclaimed.json"); rerr != nil {
			return reclaimedPredecessorDoc{}, nil, false
		} else if ancReclaimed {
			ancDoc, _, rok := s.loadAndVerifyReclaimedDoc(ar.Stream, ancSeen)
			if !rok {
				return reclaimedPredecessorDoc{}, nil, false
			}
			if ancDoc.ContinuesStream != "" {
				upstreamOf[ar.Stream] = ancDoc.ContinuesStream
			}
		} else {
			ancSt, ancSealed, sok := s.replaySourceStreamDir(arHash, ancSeen)
			if !sok || !ancSealed {
				return reclaimedPredecessorDoc{}, nil, false
			}
			if cn := ancSt.ContinuesNode(); cn != nil {
				upstreamOf[ar.Stream] = cn.Get("stream").Str
			}
		}
	}
	if doc.ContinuesStream != "" {
		reachable := make(map[string]bool, len(seal.AncestorSeals))
		cur := doc.ContinuesStream
		for cur != "" {
			if reachable[cur] {
				return reclaimedPredecessorDoc{}, nil, false
			}
			reachable[cur] = true
			cur = upstreamOf[cur]
		}
		if len(reachable) != len(seal.AncestorSeals) {
			return reclaimedPredecessorDoc{}, nil, false
		}
	}
	if len(doc.KnownPinSeqs) != 0 && len(doc.KnownPinSeqs) != len(doc.KnownPins) {
		return reclaimedPredecessorDoc{}, nil, false
	}
	known := make(map[journal.Pin]bool, len(doc.KnownPins))
	knownSeq := make(map[journal.Pin]int64, len(doc.KnownPinSeqs))
	for i, p := range doc.KnownPins {
		jp := journal.Pin{
			Stream:   p.Stream,
			RecordID: p.RecordID,
			Revision: int64(p.Revision),
			Digest:   p.Digest,
		}
		known[jp] = true
		if i < len(doc.KnownPinSeqs) {
			knownSeq[jp] = int64(doc.KnownPinSeqs[i])
		}
	}
	summary := &journal.SealedSourceSummary{
		Capability:                         "contract: durable-record/v1",
		Stream:                             streamID,
		CutSeq:                             int64(doc.CutSeq),
		CutDigest:                          doc.CutDigest,
		EffectiveRestrictionManifestDigest: doc.EffectiveRestrictionManifestDigest,
		KnownPins:                          known,
		KnownPinSeq:                        knownSeq,
		HistoricalCuts:                     doc.HistoricalCuts,
		HasAncestorHold:                    hasHolds || doc.HasAncestorHold,
	}
	return doc, summary, true
}

func (s *Store) verifyReclaimedPredecessorDir(streamID string, seen map[string]bool) (*journal.SealedSourceSummary, bool) {
	_, summary, ok := s.loadAndVerifyReclaimedDoc(streamID, seen)
	return summary, ok
}

func (s *Store) checkReclaimedStreamDirForRemoval(sh string, target journal.BodyKey) (held bool, referenced bool, ok bool) {
	sdir := dirStreams + "/" + sh
	rawSeal, err := readFileVerified(s.root, sdir+"/stream-seal.json", s.fso, maxJournalBytes)
	if err != nil {
		return false, false, false
	}
	var partial struct {
		Stream string `json:"stream"`
	}
	if err := json.Unmarshal(rawSeal, &partial); err != nil || streamHash(partial.Stream) != sh {
		return false, false, false
	}
	doc, summary, valid := s.loadAndVerifyReclaimedDoc(partial.Stream, map[string]bool{})
	if !valid {
		return false, false, false
	}
	if summary.HasAncestorHold {
		return true, false, true
	}
	refd := slices.Contains(doc.UnretiredBodies, target)
	succHash := streamHash(doc.SuccessorStream)
	succDir := dirStreams + "/" + succHash
	if succReclaimed, rerr := s.exists(succDir + "/reclaimed.json"); rerr != nil {
		return false, false, false
	} else if succReclaimed {
		succHeld, succRefd, sok := s.checkReclaimedStreamDirForRemoval(succHash, target)
		return succHeld, refd || succRefd, sok
	}
	succSt, sok := s.replaySharingStreamDir(succHash)
	if !sok || succSt.Stream() != doc.SuccessorStream {
		return false, false, false
	}
	cn := succSt.ContinuesNode()
	if cn == nil || cn.Get("stream").Str != partial.Stream || uint64(cn.Get("cut_seq").Int) > doc.CutSeq {
		return false, false, false
	}
	return false, refd, true
}

// SealAndCompactStream seals req.SourceStream, opens or resumes
// req.DestinationStream with WithContinues bound to the sealed cut, imports
// req.Records with imported_from provenance (syncing derived indexes once at
// the end of the import batch), commits restrictions_reviewed, and optionally
// reclaims the sealed predecessor journal.
func (s *Store) SealAndCompactStream(ctx context.Context, req CompactionRequest) (CompactionResult, error) {
	if err := ctx.Err(); err != nil {
		return CompactionResult{}, err
	}
	if !reStream.MatchString(req.SourceStream) || !reStream.MatchString(req.DestinationStream) {
		return CompactionResult{}, fmt.Errorf("%w: invalid stream id", ErrSchema)
	}
	if req.SourceStream == req.DestinationStream ||
		req.SourceStream == s.profile.Runtime.BodyRemovalStream ||
		req.DestinationStream == s.profile.Runtime.BodyRemovalStream {
		return CompactionResult{}, fmt.Errorf("%w: invalid source or destination stream", ErrSchema)
	}
	if req.Mode != ModeSingleRevision && req.Mode != ModeRevisioned {
		return CompactionResult{}, fmt.Errorf("%w: invalid stream mode", ErrSchema)
	}
	if !validProvenance(req.Reviewer) {
		return CompactionResult{}, fmt.Errorf("%w: invalid reviewer provenance", ErrSchema)
	}
	if !validAuthorization(&req.Authorization) {
		return CompactionResult{}, fmt.Errorf("%w: invalid authorization", ErrSchema)
	}
	if req.Note != "" && !validWireString(req.Note, 1, 1024) {
		return CompactionResult{}, fmt.Errorf("%w: invalid review note", ErrSchema)
	}

	type preparedImport struct {
		recNode   *wire.Node
		recDigest string
		rid       string
		rev       int64
		hasBody   bool
		bodyHex   string
		bodyLen   int64
		bodyDig   string
		sourcePin Pin
		key       string
	}
	prepared := make([]preparedImport, len(req.Records))
	seenKeys := make(map[string]bool, len(req.Records))
	seenSrcPins := make(map[Pin]bool, len(req.Records))
	type dstIDRev struct {
		rid string
		rev int64
	}
	seenDstIDRevs := make(map[dstIDRev]bool, len(req.Records))
	for i, cr := range req.Records {
		if !validWireString(cr.IdempotencyKey, 1, 256) || seenKeys[cr.IdempotencyKey] {
			return CompactionResult{}, fmt.Errorf("%w: invalid or duplicate compaction idempotency key", ErrSchema)
		}
		seenKeys[cr.IdempotencyKey] = true
		if cr.SourcePin.Stream != req.SourceStream || !reRecordID.MatchString(cr.SourcePin.RecordID) ||
			cr.SourcePin.Revision < 1 || !reDigest.MatchString(cr.SourcePin.Digest) || seenSrcPins[cr.SourcePin] {
			return CompactionResult{}, fmt.Errorf("%w: invalid or duplicate compaction source pin", ErrSchema)
		}
		seenSrcPins[cr.SourcePin] = true
		rn, err := wire.Ingest(cr.Record, wire.KindRecord)
		if err != nil {
			return CompactionResult{}, fmt.Errorf("%w: record ingest: %s", ErrSchema, rejectReason(err))
		}
		if err := shape.Validate("v1/record.schema.json", rn); err != nil {
			return CompactionResult{}, fmt.Errorf("%w: record shape", ErrSchema)
		}
		if rn.Get("stream").Str != req.DestinationStream || rn.Get("record_id").Str != cr.SourcePin.RecordID {
			return CompactionResult{}, fmt.Errorf("%w: compacted record stream or record_id mismatch", ErrSchema)
		}
		rev := rn.Get("revision").Int
		if rev > 1 {
			sup := rn.Get("supersedes")
			if sup == nil || sup.Get("revision").Int != rev-1 {
				return CompactionResult{}, fmt.Errorf("%w: supersedes_revision", ErrSchema)
			}
		}
		dk := dstIDRev{rid: rn.Get("record_id").Str, rev: rev}
		if seenDstIDRevs[dk] {
			return CompactionResult{}, fmt.Errorf("%w: duplicate destination record revision", ErrSchema)
		}
		seenDstIDRevs[dk] = true
		dig, err := canon.Digest(wire.Encode(rn))
		if err != nil {
			return CompactionResult{}, fmt.Errorf("%w: record digest", ErrSchema)
		}
		pi := preparedImport{
			recNode:   rn,
			recDigest: dig,
			rid:       rn.Get("record_id").Str,
			rev:       rev,
			sourcePin: cr.SourcePin,
			key:       cr.IdempotencyKey,
		}
		if bNode := rn.Get("body"); bNode != nil {
			pi.hasBody = true
			pi.bodyDig = bNode.Get("digest").Str
			pi.bodyLen = bNode.Get("length").Int
			pi.bodyHex = digestHex(pi.bodyDig)
		}
		prepared[i] = pi
	}

	var res CompactionResult
	progressedPhase := ""
	err := s.withLock(func() error {
		if len(req.PendingObligations) > 0 {
			res = CompactionResult{
				Outcome:       CompactionOutcomeRefused,
				RefusalReason: CompactionRefusedObligationPending,
			}
			return nil
		}

		// Step 1: Inspect source stream (either live/sealed or already reclaimed on retry).
		srcDir := streamDir(req.SourceStream)
		srcReclaimed, err := s.exists(srcDir + "/reclaimed.json")
		if err != nil {
			return err
		}

		srcRetired, rok := s.sourceRetiredRecordIDs(req.SourceStream, map[string]bool{})
		if !rok {
			return &ErrJournalInvalid{Reason: journal.ReasonContinuation}
		}
		for _, pi := range prepared {
			if srcRetired[pi.rid] {
				return fmt.Errorf("%w: unimported_retired_record_id", ErrSchema)
			}
		}

		var seal StreamSeal
		if !srcReclaimed {
			srcRep, err := s.replayStream(req.SourceStream)
			if err != nil {
				return err
			}
			if len(srcRep.st.ActiveHolds()) > 0 || srcRep.st.HasAncestorHold() {
				res = CompactionResult{
					Outcome:       CompactionOutcomeRefused,
					RefusalReason: CompactionRefusedHeld,
				}
				return nil
			}
			if srcRep.st.ReviewPending() {
				res = CompactionResult{
					Outcome:       CompactionOutcomeRefused,
					RefusalReason: CompactionRefusedRestrictionsPending,
				}
				return nil
			}

			// Verify every prepared source pin exists in source stream and check
			// unretired record preservation & external body reference preservation.
			srcRecords := srcRep.st.Records()
			importedRecordIDs := make(map[string]bool, len(prepared))
			preservedBodies := make(map[journal.BodyKey]bool)
			for _, pi := range prepared {
				spk := journal.Pin{
					Stream:   pi.sourcePin.Stream,
					RecordID: pi.sourcePin.RecordID,
					Revision: int64(pi.sourcePin.Revision),
					Digest:   pi.sourcePin.Digest,
				}
				if srcRecords[spk] == nil {
					return &ErrJournalInvalid{Reason: journal.ReasonContinuation}
				}
				importedRecordIDs[pi.rid] = true
				if pi.hasBody {
					preservedBodies[journal.BodyKey{Digest: pi.bodyDig, Length: pi.bodyLen}] = true
				}
			}
			if !req.AllowDropUnretired {
				seenRID := map[string]bool{}
				for pk := range srcRecords {
					if seenRID[pk.RecordID] {
						continue
					}
					seenRID[pk.RecordID] = true
					if !srcRep.st.IsRetired(pk.RecordID) && !importedRecordIDs[pk.RecordID] {
						res = CompactionResult{
							Outcome:       CompactionOutcomeRefused,
							RefusalReason: CompactionRefusedUnretiredDropped,
						}
						return nil
					}
				}
			}
			// Check that any unretired external body reference in the source stream is
			// preserved in the compacted records unless the record is retired.
			for pk, recNode := range srcRecords {
				if srcRep.st.IsRetired(pk.RecordID) {
					continue
				}
				if bNode := recNode.Get("body"); bNode != nil {
					bk := journal.BodyKey{
						Digest: bNode.Get("digest").Str,
						Length: bNode.Get("length").Int,
					}
					if !preservedBodies[bk] {
						res = CompactionResult{
							Outcome:       CompactionOutcomeRefused,
							RefusalReason: CompactionRefusedBodyUnpreserved,
						}
						return nil
					}
				}
			}

			var sealRes SealStreamResult
			if err := s.sealStreamLocked(SealStreamRequest{
				Stream:        req.SourceStream,
				Provenance:    req.Reviewer,
				Authorization: req.Authorization,
			}, &sealRes); err != nil {
				return err
			}
			if sealRes.Outcome == SealOutcomeRefused {
				res = CompactionResult{
					Outcome:       CompactionOutcomeRefused,
					RefusalReason: CompactionRefusalReason(sealRes.RefusalReason),
				}
				return nil
			}
			seal = *sealRes.Seal
			if sealRes.Outcome == SealOutcomeSealed {
				progressedPhase = "sealing_source"
			}
		} else {
			loadedSeal, ok := s.loadAndVerifySealDoc(req.SourceStream)
			if !ok {
				return &ErrJournalInvalid{Reason: journal.ReasonContinuation}
			}
			seal = loadedSeal
		}

		if req.FaultHook != nil {
			if ferr := req.FaultHook(CompactionPhaseAfterSourceSeal); ferr != nil {
				progressedPhase = "sealing_source"
				return ferr
			}
		}

		contSpec := Continues{
			Capability: "contract: durable-record/v1",
			Stream:     req.SourceStream,
			CutSeq:     seal.CutSeq,
			CutDigest:  seal.CutDigest,
		}

		// Step 2: Open or resume destination continuation stream.
		dstDir := streamDir(req.DestinationStream)
		dstExists, err := s.exists(dstDir)
		if err != nil {
			return err
		}
		if !dstExists {
			if err := s.verifyContinuationSource(req.DestinationStream, &contSpec); err != nil {
				return err
			}
			cutTok := strconv.FormatUint(contSpec.CutSeq, 10)
			contNode := &wire.Node{
				Kind: wire.Object,
				Fields: []wire.Member{
					{Name: "capability", Value: &wire.Node{Kind: wire.String, Str: contSpec.Capability}},
					{Name: "stream", Value: &wire.Node{Kind: wire.String, Str: contSpec.Stream}},
					{Name: "cut_seq", Value: &wire.Node{Kind: wire.Number, Token: cutTok, IsInt: true, Int: int64(contSpec.CutSeq)}},
					{Name: "cut_digest", Value: &wire.Node{Kind: wire.String, Str: contSpec.CutDigest}},
				},
			}
			now := FormatInstant(s.opts.Clock.Now())
			if err := createStreamJournal(s.root, s.fso, s.appender, req.DestinationStream, string(req.Mode), s.profile.Runtime.FailureModel, now, contNode, false); err != nil {
				return err
			}
			progressedPhase = "opening_destination"
		}

		if req.FaultHook != nil {
			if ferr := req.FaultHook(CompactionPhaseAfterDestinationOpen); ferr != nil {
				progressedPhase = "opening_destination"
				return ferr
			}
		}

		dstRep, err := s.replayStream(req.DestinationStream)
		if err != nil {
			return err
		}
		if dstRep.mode != req.Mode || dstRep.continues == nil || *dstRep.continues != contSpec {
			return fmt.Errorf("%w: destination stream continuation mismatch", ErrSchema)
		}

		// Step 3: Append any not-yet-imported records without intermediate index rewrites.
		existingRecords := dstRep.st.Records()
		preparedDstPins := make(map[journal.Pin]bool, len(prepared))
		for _, pi := range prepared {
			preparedDstPins[journal.Pin{
				Stream:   req.DestinationStream,
				RecordID: pi.rid,
				Revision: pi.rev,
				Digest:   pi.recDigest,
			}] = true
		}
		for epk := range existingRecords {
			if !preparedDstPins[epk] {
				return fmt.Errorf("%w: existing destination stream contains records not in compaction request", ErrSchema)
			}
		}
		importedMappings := make([]ImportedPinMapping, 0, len(prepared))
		var desig *designatedJournal
		anyAppended := false

		for _, pi := range prepared {
			dstPinKey := journal.Pin{
				Stream:   req.DestinationStream,
				RecordID: pi.rid,
				Revision: pi.rev,
				Digest:   pi.recDigest,
			}
			if existingRecords[dstPinKey] != nil {
				wantSrcPin := journal.Pin{
					Stream:   pi.sourcePin.Stream,
					RecordID: pi.sourcePin.RecordID,
					Revision: int64(pi.sourcePin.Revision),
					Digest:   pi.sourcePin.Digest,
				}
				impSrc, ok := dstRep.st.ImportedFromForPin(dstPinKey)
				if !ok || impSrc.SourceCapability != "contract: durable-record/v1" || impSrc.SourcePin != wantSrcPin {
					return fmt.Errorf("%w: existing destination record missing matching imported_from provenance", ErrSchema)
				}
				importedMappings = append(importedMappings, ImportedPinMapping{
					SourcePin: pi.sourcePin,
					DestinationPin: Pin{
						Stream:   req.DestinationStream,
						RecordID: pi.rid,
						Revision: uint64(pi.rev),
						Digest:   pi.recDigest,
					},
				})
				continue
			}
			if dstRep.sealed {
				return ErrStreamSealed
			}
			if pi.hasBody {
				if desig == nil {
					desig, err = s.readDesignatedState()
					if err != nil {
						return err
					}
				}
				bst, err := s.evalBodyState(desig, pi.bodyHex, pi.bodyLen)
				if err != nil {
					return err
				}
				if bst.kind != stateAvailable {
					res = CompactionResult{
						Outcome:       CompactionOutcomeRefused,
						RefusalReason: CompactionRefusedBodyUnpreserved,
					}
					return nil
				}
			}
			now := s.opts.Clock.Now().UTC().Truncate(time.Microsecond)
			if now.Before(dstRep.lastAt) {
				return ErrClockRegression
			}
			atStr := FormatInstant(now)
			seq := dstRep.lastSeq + 1
			seqTok := strconv.FormatInt(seq, 10)
			entryType := "appended"
			if pi.rev > 1 {
				entryType = "revised"
			}
			impRevTok := strconv.FormatUint(pi.sourcePin.Revision, 10)
			fields := []wire.Member{
				{Name: "v", Value: &wire.Node{Kind: wire.Number, Token: "1", IsInt: true, Int: 1}},
				{Name: "seq", Value: &wire.Node{Kind: wire.Number, Token: seqTok, IsInt: true, Int: seq}},
				{Name: "group", Value: &wire.Node{Kind: wire.Number, Token: seqTok, IsInt: true, Int: seq}},
				{Name: "group_size", Value: &wire.Node{Kind: wire.Number, Token: "1", IsInt: true, Int: 1}},
				{Name: "stream", Value: &wire.Node{Kind: wire.String, Str: req.DestinationStream}},
				{Name: "type", Value: &wire.Node{Kind: wire.String, Str: entryType}},
				{Name: "at", Value: &wire.Node{Kind: wire.String, Str: atStr}},
				{Name: "record", Value: pi.recNode},
				{Name: "digest", Value: &wire.Node{Kind: wire.String, Str: pi.recDigest}},
				{Name: "idempotency_key", Value: &wire.Node{Kind: wire.String, Str: pi.key}},
			}
			if pi.hasBody {
				fields = append(fields, wire.Member{Name: "body_status", Value: &wire.Node{Kind: wire.String, Str: "verified"}})
			}
			fields = append(fields, wire.Member{
				Name: "imported_from",
				Value: &wire.Node{
					Kind: wire.Object,
					Fields: []wire.Member{
						{Name: "capability", Value: &wire.Node{Kind: wire.String, Str: "contract: durable-record/v1"}},
						{Name: "pin", Value: &wire.Node{
							Kind: wire.Object,
							Fields: []wire.Member{
								{Name: "stream", Value: &wire.Node{Kind: wire.String, Str: pi.sourcePin.Stream}},
								{Name: "record_id", Value: &wire.Node{Kind: wire.String, Str: pi.sourcePin.RecordID}},
								{Name: "revision", Value: &wire.Node{Kind: wire.Number, Token: impRevTok, IsInt: true, Int: int64(pi.sourcePin.Revision)}},
								{Name: "digest", Value: &wire.Node{Kind: wire.String, Str: pi.sourcePin.Digest}},
							},
						}},
					},
				},
			})
			entry := &wire.Node{Kind: wire.Object, Fields: fields}
			if err := shape.Validate("v1/journal-entry.schema.json", entry); err != nil {
				return fmt.Errorf("%w: imported entry shape", ErrSchema)
			}
			if err := dstRep.st.Ingest(entry); err != nil {
				return fmt.Errorf("%w: %s", ErrSchema, rejectReason(err))
			}
			line := append(wire.Encode(entry), '\n')
			if err := dstRep.appendLineNoIndexSync(s, line); err != nil {
				return err
			}
			dstRep.lastAt = now
			anyAppended = true
			progressedPhase = "importing_records"
			importedMappings = append(importedMappings, ImportedPinMapping{
				SourcePin: pi.sourcePin,
				DestinationPin: Pin{
					Stream:   req.DestinationStream,
					RecordID: pi.rid,
					Revision: uint64(pi.rev),
					Digest:   pi.recDigest,
				},
			})
		}

		if req.FaultHook != nil {
			if ferr := req.FaultHook(CompactionPhaseAfterRecordImports); ferr != nil {
				progressedPhase = "importing_records"
				return ferr
			}
		}

		// Step 4: Record restrictions_reviewed (or verify already completed on retry)
		// and sync destination indexes once for the entire batch.
		appDig, err := dstRep.st.AppliedRestrictionManifestDigest()
		if err != nil {
			return &ErrJournalInvalid{Reason: rejectReason(err)}
		}
		var revRes RestrictionReviewResult
		if !dstRep.st.ReviewPending() && dstRep.st.ReviewNode() != nil {
			revNode := dstRep.st.ReviewNode()
			revRes = RestrictionReviewResult{
				Outcome: RestrictionReviewAlreadyCompleted,
				Seq:     uint64(revNode.Get("seq").Int),
				At:      revNode.Get("at").Str,
			}
		} else {
			if err := s.establishRestrictionsLocked(dstRep, RestrictionReview{
				SourceCutSeq:                  seal.CutSeq,
				SourceCutDigest:               seal.CutDigest,
				SourceStateDigest:             seal.EffectiveRestrictionManifestDigest,
				DestinationCutSeq:             uint64(dstRep.lastSeq),
				DestinationAppliedStateDigest: appDig,
				Reviewer:                      req.Reviewer,
				Authorization:                 req.Authorization,
				Note:                          req.Note,
			}, &revRes, false); err != nil {
				return err
			}
			if revRes.Outcome == RestrictionReviewCompleted {
				anyAppended = true
				progressedPhase = "reviewing_restrictions"
			}
		}
		if anyAppended {
			if err := s.syncStreamIndexes(dstRep, dstRep.raw); err != nil {
				return err
			}
		}

		if req.FaultHook != nil {
			if ferr := req.FaultHook(CompactionPhaseAfterRestrictionReview); ferr != nil {
				progressedPhase = "reviewing_restrictions"
				return ferr
			}
		}

		outcome := CompactionOutcomeCompacted
		if !anyAppended && revRes.Outcome == RestrictionReviewAlreadyCompleted && (!req.ReclaimPredecessor || srcReclaimed) {
			outcome = CompactionOutcomeAlreadyCompacted
		}

		var reclaimRes *ReclaimResult
		if req.ReclaimPredecessor {
			var rr ReclaimResult
			if err := s.reclaimSealedAncestorLocked(ReclaimRequest{
				PredecessorStream: req.SourceStream,
				SuccessorStream:   req.DestinationStream,
				ArchiveDir:        req.ArchiveDir,
			}, &rr); err != nil {
				progressedPhase = "reclaiming_predecessor"
				return err
			}
			if rr.Outcome == ReclaimOutcomeRefused {
				res = CompactionResult{
					Outcome:       CompactionOutcomeRefused,
					RefusalReason: CompactionRefusalReason(rr.RefusalReason),
				}
				return nil
			}
			reclaimRes = &rr
		}

		res = CompactionResult{
			Outcome:      outcome,
			Seal:         &seal,
			Continues:    &contSpec,
			ImportedPins: importedMappings,
			ReviewSeq:    revRes.Seq,
			Reclaimed:    reclaimRes,
		}
		return nil
	})
	if err != nil {
		if progressedPhase != "" {
			return CompactionResult{}, &ErrCompactionIncomplete{
				Phase: progressedPhase,
				Cause: err,
			}
		}
		return CompactionResult{}, err
	}
	return res, nil
}

// ReclaimSealedAncestor reclaims the raw journal and derived index files of a
// sealed predecessor stream after verifying its detached stream-seal.json,
// zero active or inherited holds, and a current restrictions_reviewed entry on
// req.SuccessorStream covering the sealed cut.
func (s *Store) ReclaimSealedAncestor(ctx context.Context, req ReclaimRequest) (ReclaimResult, error) {
	if err := ctx.Err(); err != nil {
		return ReclaimResult{}, err
	}
	if !reStream.MatchString(req.PredecessorStream) || !reStream.MatchString(req.SuccessorStream) {
		return ReclaimResult{}, fmt.Errorf("%w: invalid stream id", ErrSchema)
	}
	if req.PredecessorStream == req.SuccessorStream ||
		req.PredecessorStream == s.profile.Runtime.BodyRemovalStream ||
		req.SuccessorStream == s.profile.Runtime.BodyRemovalStream {
		return ReclaimResult{}, fmt.Errorf("%w: invalid predecessor or successor stream", ErrSchema)
	}

	var res ReclaimResult
	err := s.withLock(func() error {
		return s.reclaimSealedAncestorLocked(req, &res)
	})
	if err != nil {
		return ReclaimResult{}, err
	}
	return res, nil
}

func (s *Store) reclaimSealedAncestorLocked(req ReclaimRequest, res *ReclaimResult) error {
	if len(req.PendingObligations) > 0 {
		*res = ReclaimResult{
			Outcome:           ReclaimOutcomeRefused,
			RefusalReason:     ReclaimRefusedObligationPending,
			PredecessorStream: req.PredecessorStream,
			SuccessorStream:   req.SuccessorStream,
		}
		return nil
	}
	predDir := streamDir(req.PredecessorStream)
	if hasSeal, err := s.exists(predDir + "/stream-seal.json"); err != nil {
		return err
	} else if !hasSeal {
		*res = ReclaimResult{
			Outcome:           ReclaimOutcomeRefused,
			RefusalReason:     ReclaimRefusedNotSealed,
			PredecessorStream: req.PredecessorStream,
			SuccessorStream:   req.SuccessorStream,
		}
		return nil
	}

	if alreadyReclaimed, err := s.exists(predDir + "/reclaimed.json"); err != nil {
		return err
	} else if alreadyReclaimed {
		doc, _, ok := s.loadAndVerifyReclaimedDoc(req.PredecessorStream, map[string]bool{})
		if !ok {
			return &ErrJournalInvalid{Reason: journal.ReasonContinuation}
		}
		if doc.SuccessorStream != req.SuccessorStream {
			*res = ReclaimResult{
				Outcome:           ReclaimOutcomeRefused,
				RefusalReason:     ReclaimRefusedRestrictionsPending,
				PredecessorStream: req.PredecessorStream,
				SuccessorStream:   req.SuccessorStream,
			}
			return nil
		}
		// Finish unlinking any leftover journal or index files if a prior run crashed mid-cleanup.
		for _, fname := range []string{"journal.ndjson", "heads.idx", "keys.idx", "records.idx"} {
			_ = s.root.Remove(predDir + "/" + fname)
		}
		s.cleanStaleIndexTemps(predDir)
		*res = ReclaimResult{
			Outcome:               ReclaimOutcomeAlreadyReclaimed,
			PredecessorStream:     req.PredecessorStream,
			SuccessorStream:       doc.SuccessorStream,
			SealDigest:            doc.SealDigest,
			ReclaimedJournalBytes: doc.ReclaimedJournalBytes,
			ReclaimedIndexBytes:   doc.ReclaimedIndexBytes,
			ArchivedJournalPath:   doc.ArchivedJournalPath,
			ArchivedSealPath:      doc.ArchivedSealPath,
		}
		return nil
	}

	predRep, err := s.replayStream(req.PredecessorStream)
	if err != nil {
		return err
	}
	if !predRep.sealed {
		*res = ReclaimResult{
			Outcome:           ReclaimOutcomeRefused,
			RefusalReason:     ReclaimRefusedNotSealed,
			PredecessorStream: req.PredecessorStream,
			SuccessorStream:   req.SuccessorStream,
		}
		return nil
	}
	if len(predRep.st.ActiveHolds()) > 0 || predRep.st.HasAncestorHold() {
		*res = ReclaimResult{
			Outcome:           ReclaimOutcomeRefused,
			RefusalReason:     ReclaimRefusedHeld,
			PredecessorStream: req.PredecessorStream,
			SuccessorStream:   req.SuccessorStream,
		}
		return nil
	}
	seal, ok := s.loadAndVerifySealDoc(req.PredecessorStream)
	if !ok {
		return &ErrJournalInvalid{Reason: journal.ReasonContinuation}
	}

	// Verify successor stream exists, continues PredecessorStream at or before seal.CutSeq,
	// and has a current restrictions_reviewed covering seal.EffectiveRestrictionManifestDigest at seal.CutSeq.
	succExists, err := s.exists(streamDir(req.SuccessorStream))
	if err != nil {
		return err
	}
	if !succExists {
		*res = ReclaimResult{
			Outcome:           ReclaimOutcomeRefused,
			RefusalReason:     ReclaimRefusedRestrictionsPending,
			PredecessorStream: req.PredecessorStream,
			SuccessorStream:   req.SuccessorStream,
		}
		return nil
	}
	succRep, err := s.replayStream(req.SuccessorStream)
	if err != nil {
		return err
	}
	revNode := succRep.st.ReviewNode()
	revCoversSeal := revNode != nil &&
		revNode.Get("source") != nil &&
		uint64(revNode.Get("source").Get("cut_seq").Int) == seal.CutSeq &&
		revNode.Get("source").Get("cut_digest").Str == seal.CutDigest
	if succRep.continues == nil ||
		succRep.continues.Stream != req.PredecessorStream ||
		succRep.continues.CutSeq > seal.CutSeq ||
		!revCoversSeal ||
		succRep.st.ReviewPending() {
		*res = ReclaimResult{
			Outcome:           ReclaimOutcomeRefused,
			RefusalReason:     ReclaimRefusedRestrictionsPending,
			PredecessorStream: req.PredecessorStream,
			SuccessorStream:   req.SuccessorStream,
		}
		return nil
	}

	// Check that any unretired record revision in PredecessorStream with an external body
	// is either retired or has its body referenced in SuccessorStream, and record all
	// unretired body references so RemoveBody continues to see them after reclamation.
	unretiredBodySet := make(map[journal.BodyKey]bool)
	for pk, recNode := range predRep.st.Records() {
		if predRep.st.IsRetired(pk.RecordID) {
			continue
		}
		if bNode := recNode.Get("body"); bNode != nil {
			bk := journal.BodyKey{
				Digest: bNode.Get("digest").Str,
				Length: bNode.Get("length").Int,
			}
			if !succRep.st.BodyReferenced(bk) {
				*res = ReclaimResult{
					Outcome:           ReclaimOutcomeRefused,
					RefusalReason:     ReclaimRefusedBodyReferenced,
					PredecessorStream: req.PredecessorStream,
					SuccessorStream:   req.SuccessorStream,
				}
				return nil
			}
			unretiredBodySet[bk] = true
		}
	}
	var unretiredBodies []journal.BodyKey
	for bk := range unretiredBodySet {
		unretiredBodies = append(unretiredBodies, bk)
	}
	slices.SortFunc(unretiredBodies, func(a, b journal.BodyKey) int {
		if a.Digest != b.Digest {
			return strings.Compare(a.Digest, b.Digest)
		}
		if a.Length < b.Length {
			return -1
		}
		if a.Length > b.Length {
			return 1
		}
		return 0
	})

	// Measure bytes to be reclaimed.
	reclaimedJournalBytes := int64(len(predRep.raw))
	var reclaimedIndexBytes int64
	for _, idxName := range []string{"heads.idx", "keys.idx", "records.idx"} {
		if b, rerr := readFileVerified(s.root, predDir+"/"+idxName, s.fso, maxJournalBytes); rerr == nil {
			reclaimedIndexBytes += int64(len(b))
		}
	}

	// Optional cold archive before unlinking predecessor journal.
	var archivedJournalPath, archivedSealPath string
	if req.ArchiveDir != "" {
		if _, lerr := os.Lstat(req.ArchiveDir); errors.Is(lerr, fs.ErrNotExist) {
			if merr := os.Mkdir(req.ArchiveDir, 0o700); merr != nil {
				return ioErr("mkdir", merr, false)
			}
		}
		archRoot, err := fsroot.Open(req.ArchiveDir, s.fso)
		if err != nil {
			if errors.Is(err, fsroot.ErrSymlink) || errors.Is(err, fsroot.ErrSpecialFile) ||
				errors.Is(err, fsroot.ErrMultipleLink) || errors.Is(err, fsroot.ErrOwner) ||
				errors.Is(err, fsroot.ErrMode) || errors.Is(err, fsroot.ErrFSType) {
				return fmt.Errorf("%w: invalid archive directory", ErrStoreInvalid)
			}
			return ioErr("open", err, false)
		}
		defer archRoot.Close()
		sh := streamHash(req.PredecessorStream)
		ajName := sh + "-journal.ndjson"
		asName := sh + "-stream-seal.json"
		if err := writeArchiveFileDurable(archRoot, ajName, predRep.raw); err != nil {
			return err
		}
		rawSeal, err := readFileVerified(s.root, predDir+"/stream-seal.json", s.fso, maxJournalBytes)
		if err != nil {
			return ioErr("read", err, false)
		}
		if err := writeArchiveFileDurable(archRoot, asName, rawSeal); err != nil {
			return err
		}
		archivedJournalPath = filepath.Join(req.ArchiveDir, ajName)
		archivedSealPath = filepath.Join(req.ArchiveDir, asName)
	}

	mDoc, effDig, err := predRep.st.EffectiveRestrictionManifest()
	if err != nil || effDig != seal.EffectiveRestrictionManifestDigest {
		return &ErrJournalInvalid{Reason: journal.ReasonContinuation}
	}
	canonManifest, err := canon.Canonicalize(wire.Encode(mDoc))
	if err != nil {
		return err
	}

	var knownPins []Pin
	for pk := range predRep.st.Records() {
		knownPins = append(knownPins, Pin{
			Stream:   pk.Stream,
			RecordID: pk.RecordID,
			Revision: uint64(pk.Revision),
			Digest:   pk.Digest,
		})
	}
	slices.SortFunc(knownPins, func(a, b Pin) int {
		if a.RecordID != b.RecordID {
			return strings.Compare(a.RecordID, b.RecordID)
		}
		if a.Revision < b.Revision {
			return -1
		}
		if a.Revision > b.Revision {
			return 1
		}
		return strings.Compare(a.Digest, b.Digest)
	})
	if knownPins == nil {
		knownPins = []Pin{}
	}
	pinSeqMap := predRep.st.PinSeqs()
	knownPinSeqs := make([]uint64, len(knownPins))
	for i, p := range knownPins {
		jp := journal.Pin{
			Stream:   p.Stream,
			RecordID: p.RecordID,
			Revision: int64(p.Revision),
			Digest:   p.Digest,
		}
		knownPinSeqs[i] = uint64(pinSeqMap[jp])
	}
	var historicalCuts []journal.SealedCutInfo
	for seq := int64(1); seq < int64(seal.CutSeq); seq++ {
		cdig, cerr := predRep.st.CutDigest(seq)
		mdig, merr := predRep.st.ManifestDigestAt(seq)
		if cerr == nil && merr == nil {
			historicalCuts = append(historicalCuts, journal.SealedCutInfo{
				CutSeq:         seq,
				CutDigest:      cdig,
				ManifestDigest: mdig,
			})
		}
	}

	// Preserve transitive source-pin mappings across multi-hop chains so ResolveSourcePin
	// works even after multiple ancestors are reclaimed.
	var impBindings []ImportedPinMapping
	for _, b := range predRep.st.ImportedPinBindings() {
		srcPin := Pin{
			Stream:   b.SourcePin.Stream,
			RecordID: b.SourcePin.RecordID,
			Revision: uint64(b.SourcePin.Revision),
			Digest:   b.SourcePin.Digest,
		}
		dstPin := Pin{
			Stream:   b.DestinationPin.Stream,
			RecordID: b.DestinationPin.RecordID,
			Revision: uint64(b.DestinationPin.Revision),
			Digest:   b.DestinationPin.Digest,
		}
		impBindings = append(impBindings, ImportedPinMapping{
			SourcePin:      srcPin,
			DestinationPin: dstPin,
		})
	}
	if predRep.continues != nil {
		predAncBindings := s.collectReclaimedOrLivePinMappings(predRep.continues.Stream, map[string]bool{streamHash(req.PredecessorStream): true})
		for _, ab := range predAncBindings {
			for _, curB := range impBindings {
				if ab.DestinationPin == curB.SourcePin {
					impBindings = append(impBindings, ImportedPinMapping{
						SourcePin:      ab.SourcePin,
						DestinationPin: curB.DestinationPin,
					})
				}
			}
		}
	}

	diagKey, err := readFileVerified(s.root, "diag.key", s.fso, keyLen)
	if err != nil {
		return ioErr("read", err, false)
	}
	var continuesStream string
	if predRep.continues != nil {
		continuesStream = predRep.continues.Stream
	}
	doc := reclaimedPredecessorDoc{
		V:                                  1,
		PredecessorStream:                  req.PredecessorStream,
		SuccessorStream:                    req.SuccessorStream,
		ContinuesStream:                    continuesStream,
		CutSeq:                             seal.CutSeq,
		CutDigest:                          seal.CutDigest,
		SealDigest:                         seal.SealDigest,
		EffectiveRestrictionManifestDigest: seal.EffectiveRestrictionManifestDigest,
		Manifest:                           canonManifest,
		HasAncestorHold:                    false,
		KnownPins:                          knownPins,
		KnownPinSeqs:                       knownPinSeqs,
		HistoricalCuts:                     historicalCuts,
		UnretiredBodies:                    unretiredBodies,
		ImportedBindings:                   impBindings,
		ReclaimedJournalBytes:              reclaimedJournalBytes,
		ReclaimedIndexBytes:                reclaimedIndexBytes,
		ArchivedJournalPath:                archivedJournalPath,
		ArchivedSealPath:                   archivedSealPath,
		ReclaimedAt:                        FormatInstant(s.opts.Clock.Now()),
	}
	mac, err := computeReclaimedMAC(diagKey, doc)
	if err != nil {
		return err
	}
	doc.MAC = mac
	docBytes, err := canonicalJSON(doc)
	if err != nil {
		return err
	}
	if err := s.writeIndexFileAtomic(predDir, "reclaimed.json", docBytes); err != nil {
		return err
	}
	for _, fname := range []string{"journal.ndjson", "heads.idx", "keys.idx", "records.idx"} {
		if err := s.root.Remove(predDir + "/" + fname); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return &ErrCompactionIncomplete{
				Phase: "reclaiming_predecessor",
				Cause: ioErr("unlink", err, true),
			}
		}
	}
	s.cleanStaleIndexTemps(predDir)
	if err := s.root.SyncDir(predDir); err != nil {
		return &ErrCompactionIncomplete{
			Phase: "reclaiming_predecessor",
			Cause: ioErr("fsync_dir", err, true),
		}
	}

	*res = ReclaimResult{
		Outcome:               ReclaimOutcomeReclaimed,
		PredecessorStream:     req.PredecessorStream,
		SuccessorStream:       req.SuccessorStream,
		SealDigest:            seal.SealDigest,
		ReclaimedJournalBytes: reclaimedJournalBytes,
		ReclaimedIndexBytes:   reclaimedIndexBytes,
		ArchivedJournalPath:   archivedJournalPath,
		ArchivedSealPath:      archivedSealPath,
	}
	return nil
}

func writeArchiveFileDurable(archRoot *fsroot.Root, name string, data []byte) error {
	if fi, err := archRoot.Lstat(name); err == nil {
		if fi.Mode()&fs.ModeSymlink != 0 || !fi.Mode().IsRegular() {
			return fmt.Errorf("%w: invalid archive target", ErrStoreInvalid)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return ioErr("stat", err, false)
	}
	var rbuf [8]byte
	if _, err := io.ReadFull(rand.Reader, rbuf[:]); err != nil {
		return ioErr("entropy", err, false)
	}
	tmp := ".tmp-" + name + "-" + hex.EncodeToString(rbuf[:])
	f, err := archRoot.CreateExclusive(tmp)
	if err != nil {
		return ioErr("create", err, false)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = archRoot.Remove(tmp)
		return ioErr("write", err, false)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = archRoot.Remove(tmp)
		return ioErr("fsync", err, false)
	}
	if err := f.Close(); err != nil {
		_ = archRoot.Remove(tmp)
		return ioErr("close", err, false)
	}
	if err := archRoot.Rename(tmp, name); err != nil {
		_ = archRoot.Remove(tmp)
		return ioErr("rename", err, false)
	}
	if err := archRoot.SyncDir("."); err != nil {
		return ioErr("fsync_dir", err, false)
	}
	return nil
}

func (s *Store) collectReclaimedOrLivePinMappings(streamID string, seen map[string]bool) []ImportedPinMapping {
	sh := streamHash(streamID)
	if seen[sh] || len(seen) > 16 {
		return nil
	}
	seen[sh] = true
	sdir := streamDir(streamID)
	if reclaimed, err := s.exists(sdir + "/reclaimed.json"); err == nil && reclaimed {
		doc, _, ok := s.loadAndVerifyReclaimedDoc(streamID, copySeenMap(seen))
		if !ok {
			return nil
		}
		return doc.ImportedBindings
	}
	st, _, ok := s.replaySourceStreamDir(sh, copySeenMap(seen))
	if !ok {
		return nil
	}
	var out []ImportedPinMapping
	for _, b := range st.ImportedPinBindings() {
		out = append(out, ImportedPinMapping{
			SourcePin: Pin{
				Stream:   b.SourcePin.Stream,
				RecordID: b.SourcePin.RecordID,
				Revision: uint64(b.SourcePin.Revision),
				Digest:   b.SourcePin.Digest,
			},
			DestinationPin: Pin{
				Stream:   b.DestinationPin.Stream,
				RecordID: b.DestinationPin.RecordID,
				Revision: uint64(b.DestinationPin.Revision),
				Digest:   b.DestinationPin.Digest,
			},
		})
	}
	if cn := st.ContinuesNode(); cn != nil {
		anc := s.collectReclaimedOrLivePinMappings(cn.Get("stream").Str, copySeenMap(seen))
		for _, ab := range anc {
			for _, cb := range out {
				if ab.DestinationPin == cb.SourcePin {
					out = append(out, ImportedPinMapping{
						SourcePin:      ab.SourcePin,
						DestinationPin: cb.DestinationPin,
					})
				}
			}
		}
	}
	return out
}

// ResolveSourcePin resolves a pin from st or any sealed/reclaimed ancestor in
// st's continuation chain to its corresponding imported pin on st.
func (st *Stream) ResolveSourcePin(sourcePin Pin) (Pin, bool, error) {
	if !reStream.MatchString(sourcePin.Stream) || !reRecordID.MatchString(sourcePin.RecordID) ||
		sourcePin.Revision < 1 || !reDigest.MatchString(sourcePin.Digest) {
		return Pin{}, false, fmt.Errorf("%w: invalid source pin", ErrSchema)
	}
	var resolved Pin
	var found bool
	err := st.store.withLock(func() error {
		rep, err := st.store.replayStream(st.id)
		if err != nil {
			return err
		}
		if sourcePin.Stream == st.id {
			pk := journal.Pin{
				Stream:   sourcePin.Stream,
				RecordID: sourcePin.RecordID,
				Revision: int64(sourcePin.Revision),
				Digest:   sourcePin.Digest,
			}
			if rep.st.Records()[pk] != nil {
				resolved = sourcePin
				found = true
			}
			return nil
		}
		curPin := sourcePin
		// Walk direct and transitive imported pin mappings up to st.
		direct := make([]ImportedPinMapping, 0)
		for _, b := range rep.st.ImportedPinBindings() {
			direct = append(direct, ImportedPinMapping{
				SourcePin: Pin{
					Stream:   b.SourcePin.Stream,
					RecordID: b.SourcePin.RecordID,
					Revision: uint64(b.SourcePin.Revision),
					Digest:   b.SourcePin.Digest,
				},
				DestinationPin: Pin{
					Stream:   b.DestinationPin.Stream,
					RecordID: b.DestinationPin.RecordID,
					Revision: uint64(b.DestinationPin.Revision),
					Digest:   b.DestinationPin.Digest,
				},
			})
		}
		for _, m := range direct {
			if m.SourcePin == curPin {
				resolved = m.DestinationPin
				found = true
				return nil
			}
		}
		if rep.continues != nil {
			ancMappings := st.store.collectReclaimedOrLivePinMappings(rep.continues.Stream, map[string]bool{streamHash(st.id): true})
			for _, am := range ancMappings {
				if am.SourcePin == curPin {
					for _, dm := range direct {
						if dm.SourcePin == am.DestinationPin {
							resolved = dm.DestinationPin
							found = true
							return nil
						}
					}
				}
			}
		}
		return nil
	})
	return resolved, found, err
}

// StorageUsage returns logical vs. physical byte accounting for st.
func (st *Stream) StorageUsage() (StreamStorageUsage, error) {
	var out StreamStorageUsage
	err := st.store.withLock(func() error {
		u, err := st.store.streamStorageUsageLocked(streamHash(st.id), st.id)
		if err != nil {
			return err
		}
		out = u
		return nil
	})
	return out, err
}

// StorageUsage returns logical vs. physical byte accounting across the Store,
// distinguishing active stream journals, sealed/reclaimed stream metadata,
// derived indexes, external bodies, and temporary index-rewrite files.
func (s *Store) StorageUsage() (StoreStorageUsage, error) {
	var out StoreStorageUsage
	err := s.withLock(func() error {
		streams, err := s.root.ReadDirNames(dirStreams, s.fso)
		if err != nil {
			return ioErr("read", err, false)
		}
		for _, sh := range streams {
			u, err := s.streamStorageUsageLocked(sh, "")
			if err != nil {
				return err
			}
			out.LogicalJournalBytes += u.LogicalJournalBytes
			if u.Sealed {
				out.SealedLogicalJournalBytes += u.LogicalJournalBytes
			} else {
				out.ActiveLogicalJournalBytes += u.LogicalJournalBytes
			}
			out.DerivedIndexBytes += u.DerivedIndexBytes
			out.SealMetadataBytes += u.SealMetadataBytes
			out.TempRewriteBytes += u.TempRewriteBytes
			if u.MaxSingleIndexRewriteBytes > out.MaxSingleIndexRewriteBytes {
				out.MaxSingleIndexRewriteBytes = u.MaxSingleIndexRewriteBytes
			}
			out.PhysicalAllocatedBytes += u.PhysicalAllocatedBytes
			out.Streams = append(out.Streams, u)
		}
		if shardDirs, err := s.root.ReadDirNames(dirBodySHA256, s.fso); err == nil {
			for _, shard := range shardDirs {
				shardPath := dirBodySHA256 + "/" + shard
				if bodyNames, berr := s.root.ReadDirNames(shardPath, s.fso); berr == nil {
					for _, bn := range bodyNames {
						if b, rerr := readFileVerified(s.root, shardPath+"/"+bn, s.fso, maxBodyLength); rerr == nil {
							sz := int64(len(b))
							out.BodyBytes += sz
							out.PhysicalAllocatedBytes += sz
						}
					}
				}
			}
		}
		return nil
	})
	return out, err
}

func (s *Store) streamStorageUsageLocked(sh, knownStreamID string) (StreamStorageUsage, error) {
	sdir := dirStreams + "/" + sh
	names, err := s.root.ReadDirNames(sdir, s.fso)
	if err != nil {
		return StreamStorageUsage{}, ioErr("read", err, false)
	}
	u := StreamStorageUsage{Stream: knownStreamID}
	for _, n := range names {
		p := sdir + "/" + n
		switch {
		case n == "journal.ndjson":
			raw, rerr := readFileVerified(s.root, p, s.fso, maxJournalBytes)
			if rerr != nil {
				return StreamStorageUsage{}, ioErr("read", rerr, false)
			}
			sz := int64(len(raw))
			u.LogicalJournalBytes += sz
			u.PhysicalAllocatedBytes += sz
			if u.Stream == "" {
				if nl := bytes.IndexByte(raw, '\n'); nl > 0 {
					var hdr struct {
						Stream string `json:"stream"`
					}
					if json.Unmarshal(raw[:nl], &hdr) == nil {
						u.Stream = hdr.Stream
					}
				}
			}
		case n == "heads.idx" || n == "keys.idx" || n == "records.idx":
			raw, rerr := readFileVerified(s.root, p, s.fso, maxJournalBytes)
			if rerr != nil {
				return StreamStorageUsage{}, ioErr("read", rerr, false)
			}
			sz := int64(len(raw))
			u.DerivedIndexBytes += sz
			u.PhysicalAllocatedBytes += sz
			if sz > u.MaxSingleIndexRewriteBytes {
				u.MaxSingleIndexRewriteBytes = sz
			}
		case n == "stream-seal.json" || n == "reclaimed.json" || n == "SEALED":
			raw, rerr := readFileVerified(s.root, p, s.fso, maxJournalBytes)
			if rerr != nil {
				return StreamStorageUsage{}, ioErr("read", rerr, false)
			}
			sz := int64(len(raw))
			u.SealMetadataBytes += sz
			u.PhysicalAllocatedBytes += sz
			if n == "stream-seal.json" || n == "SEALED" {
				u.Sealed = true
			}
			if n == "reclaimed.json" {
				u.Reclaimed = true
				u.Sealed = true
				var doc struct {
					PredecessorStream string `json:"predecessor_stream"`
				}
				if u.Stream == "" && json.Unmarshal(raw, &doc) == nil {
					u.Stream = doc.PredecessorStream
				}
			}
		case strings.HasPrefix(n, ".tmp-"):
			if raw, rerr := readFileVerified(s.root, p, s.fso, maxJournalBytes); rerr == nil {
				sz := int64(len(raw))
				u.TempRewriteBytes += sz
				u.PhysicalAllocatedBytes += sz
			}
		}
	}
	return u, nil
}
