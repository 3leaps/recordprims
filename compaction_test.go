package recordprims

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/3leaps/recordprims/internal/canon"
	"github.com/3leaps/recordprims/internal/wire"
)

type memReleaseSink struct {
	frames []FrameHeader
	data   [][]byte
}

func (m *memReleaseSink) Frame(_ context.Context, h FrameHeader, r io.Reader) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	m.frames = append(m.frames, h)
	m.data = append(m.data, b)
	return nil
}

var (
	testReviewer = Provenance{
		ActorKind: "human",
		Actor:     "alice@example.com",
	}
	testAuth = Authorization{
		Ref:    "auth:compaction-2026-01",
		Digest: "sha256:1111111111111111111111111111111111111111111111111111111111111111",
	}
)

func protectionHoldRequestJSON(stream string, pin Pin, key string) json.RawMessage {
	m := map[string]any{
		"v":               1,
		"stream":          stream,
		"idempotency_key": key,
		"action":          "tombstone",
		"target": map[string]any{
			"stream":    pin.Stream,
			"record_id": pin.RecordID,
			"revision":  pin.Revision,
			"digest":    pin.Digest,
		},
		"scope":       "record",
		"reason":      "security_hold",
		"evidence":    testAuth,
		"observed_at": "2026-10-20T00:00:00.000000Z",
		"provenance":  testReviewer,
	}
	b, err := json.Marshal(m)
	if err != nil {
		panic(err)
	}
	return b
}

func TestCompactionAndReclamationPreconditionRefusals(t *testing.T) {
	ctx := context.Background()
	store, _, _ := openBodyStore(t)

	s1, err := store.OpenStream("src-refusal", ModeRevisioned)
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}
	r1, err := s1.Append(ctx, Write{
		Record:         recordJSON("src-refusal", "rec-1", 1, nil, `{"v":1}`, nil),
		IdempotencyKey: "k-ref-1",
	})
	if err != nil || r1.Outcome != OutcomeAppended {
		t.Fatalf("Append r1: %+v, err=%v", r1, err)
	}

	// Publish a body and reference it on rec-body.
	bodyBytes := []byte("important external body payload")
	bRef, lease, err := store.PutBody(ctx, bytes.NewReader(bodyBytes), "text/plain")
	if err != nil {
		t.Fatalf("PutBody: %v", err)
	}
	r2, err := s1.Append(ctx, Write{
		Record:         recordJSON("src-refusal", "rec-body", 1, nil, `{"v":2}`, &bRef),
		IdempotencyKey: "k-ref-2",
		Lease:          &lease,
	})
	if err != nil || r2.Outcome != OutcomeAppended {
		t.Fatalf("Append r2: %+v, err=%v", r2, err)
	}

	// 1. ReclaimSealedAncestor refuses when predecessor is not sealed.
	reclaimUnsealed, err := store.ReclaimSealedAncestor(ctx, ReclaimRequest{
		PredecessorStream: "src-refusal",
		SuccessorStream:   "dst-refusal",
	})
	if err != nil {
		t.Fatalf("ReclaimSealedAncestor unsealed: %v", err)
	}
	if reclaimUnsealed.Outcome != ReclaimOutcomeRefused || reclaimUnsealed.RefusalReason != ReclaimRefusedNotSealed {
		t.Fatalf("expected ReclaimRefusedNotSealed, got %+v", reclaimUnsealed)
	}

	// 2. Pending obligations refuse SealStream and SealAndCompactStream.
	sealOblig, err := store.SealStream(ctx, SealStreamRequest{
		Stream:             "src-refusal",
		Provenance:         testReviewer,
		Authorization:      testAuth,
		PendingObligations: []string{"export:batch-9"},
	})
	if err != nil {
		t.Fatalf("SealStream pending obligation: %v", err)
	}
	if sealOblig.Outcome != SealOutcomeRefused || sealOblig.RefusalReason != SealRefusedObligationPending {
		t.Fatalf("expected SealRefusedObligationPending, got %+v", sealOblig)
	}

	compOblig, err := store.SealAndCompactStream(ctx, CompactionRequest{
		SourceStream:       "src-refusal",
		DestinationStream:  "dst-refusal",
		Mode:               ModeRevisioned,
		PendingObligations: []string{"export:batch-9"},
		Reviewer:           testReviewer,
		Authorization:      testAuth,
	})
	if err != nil {
		t.Fatalf("SealAndCompactStream pending obligation: %v", err)
	}
	if compOblig.Outcome != CompactionOutcomeRefused || compOblig.RefusalReason != CompactionRefusedObligationPending {
		t.Fatalf("expected CompactionRefusedObligationPending, got %+v", compOblig)
	}

	// 3. Dropping an unretired record without AllowDropUnretired refuses with CompactionRefusedUnretiredDropped.
	compDrop, err := store.SealAndCompactStream(ctx, CompactionRequest{
		SourceStream:      "src-refusal",
		DestinationStream: "dst-refusal",
		Mode:              ModeRevisioned,
		Records: []CompactedRecord{
			{
				Record:         recordJSON("dst-refusal", "rec-body", 1, nil, `{"v":2}`, &bRef),
				SourcePin:      *r2.Pin,
				IdempotencyKey: "k-imp-body",
			},
		},
		AllowDropUnretired: false,
		Reviewer:           testReviewer,
		Authorization:      testAuth,
	})
	if err != nil {
		t.Fatalf("SealAndCompactStream drop unretired: %v", err)
	}
	if compDrop.Outcome != CompactionOutcomeRefused || compDrop.RefusalReason != CompactionRefusedUnretiredDropped {
		t.Fatalf("expected CompactionRefusedUnretiredDropped, got %+v", compDrop)
	}

	// 4. Dropping an unretired external body reference (even with AllowDropUnretired: true)
	// refuses with CompactionRefusedBodyUnpreserved.
	compBodyDrop, err := store.SealAndCompactStream(ctx, CompactionRequest{
		SourceStream:      "src-refusal",
		DestinationStream: "dst-refusal",
		Mode:              ModeRevisioned,
		Records: []CompactedRecord{
			{
				Record:         recordJSON("dst-refusal", "rec-1", 1, nil, `{"v":1}`, nil),
				SourcePin:      *r1.Pin,
				IdempotencyKey: "k-imp-1",
			},
		},
		AllowDropUnretired: true,
		Reviewer:           testReviewer,
		Authorization:      testAuth,
	})
	if err != nil {
		t.Fatalf("SealAndCompactStream drop body: %v", err)
	}
	if compBodyDrop.Outcome != CompactionOutcomeRefused || compBodyDrop.RefusalReason != CompactionRefusedBodyUnpreserved {
		t.Fatalf("expected CompactionRefusedBodyUnpreserved, got %+v", compBodyDrop)
	}

	// 5. Active hold on source stream refuses SealStream and SealAndCompactStream with Held.
	pr, err := s1.Protect(ctx, ProtectionRequest{
		Raw: protectionHoldRequestJSON("src-refusal", *r1.Pin, "k-hold-1"),
	})
	if err != nil || pr.Outcome != "tombstoned" {
		t.Fatalf("Protect tombstone: %+v, err=%v", pr, err)
	}
	compHeld, err := store.SealAndCompactStream(ctx, CompactionRequest{
		SourceStream:      "src-refusal",
		DestinationStream: "dst-refusal",
		Mode:              ModeRevisioned,
		Records: []CompactedRecord{
			{
				Record:         recordJSON("dst-refusal", "rec-body", 1, nil, `{"v":2}`, &bRef),
				SourcePin:      *r2.Pin,
				IdempotencyKey: "k-imp-body",
			},
		},
		AllowDropUnretired: true,
		Reviewer:           testReviewer,
		Authorization:      testAuth,
	})
	if err != nil {
		t.Fatalf("SealAndCompactStream held: %v", err)
	}
	if compHeld.Outcome != CompactionOutcomeRefused || compHeld.RefusalReason != CompactionRefusedHeld {
		t.Fatalf("expected CompactionRefusedHeld, got %+v", compHeld)
	}
}

func TestSingleHopSealAndCompactAndReclaimLifecycle(t *testing.T) {
	ctx := context.Background()
	store, root, _ := openBodyStore(t)

	s1, err := store.OpenStream("orders-v1", ModeRevisioned)
	if err != nil {
		t.Fatalf("OpenStream orders-v1: %v", err)
	}

	// Append rec-1 rev 1 & rev 2, rec-retired rev 1 (then retire it), and rec-body rev 1 with body.
	r1v1, err := s1.Append(ctx, Write{
		Record:         recordJSON("orders-v1", "rec-1", 1, nil, `{"step":1}`, nil),
		IdempotencyKey: "k-1-1",
	})
	if err != nil || r1v1.Outcome != OutcomeAppended {
		t.Fatalf("Append r1v1: %+v, err=%v", r1v1, err)
	}
	r1v2, err := s1.Append(ctx, Write{
		Record:         recordJSON("orders-v1", "rec-1", 2, r1v1.Pin, `{"step":2}`, nil),
		IdempotencyKey: "k-1-2",
	})
	if err != nil || r1v2.Outcome != OutcomeRevised {
		t.Fatalf("Append r1v2: %+v, err=%v", r1v2, err)
	}

	rRet, err := s1.Append(ctx, Write{
		Record:         recordJSON("orders-v1", "rec-retired", 1, nil, `{"temp":true}`, nil),
		IdempotencyKey: "k-ret-1",
	})
	if err != nil || rRet.Outcome != OutcomeAppended {
		t.Fatalf("Append rRet: %+v, err=%v", rRet, err)
	}
	retRes, err := s1.Retire(ctx, "rec-retired", RetireRetention, WithRetireKey("k-ret-op"))
	if err != nil || retRes.Outcome != RetireOutcomeRetired {
		t.Fatalf("Retire rec-retired: %+v, err=%v", retRes, err)
	}

	bodyPayload := []byte("attachment-bytes-for-rec-body")
	bRef, lease, err := store.PutBody(ctx, bytes.NewReader(bodyPayload), "application/octet-stream")
	if err != nil {
		t.Fatalf("PutBody: %v", err)
	}
	rBody, err := s1.Append(ctx, Write{
		Record:         recordJSON("orders-v1", "rec-body", 1, nil, `{"attached":true}`, &bRef),
		IdempotencyKey: "k-body-1",
		Lease:          &lease,
	})
	if err != nil || rBody.Outcome != OutcomeAppended {
		t.Fatalf("Append rBody: %+v, err=%v", rBody, err)
	}

	usageBefore, err := store.StorageUsage()
	if err != nil {
		t.Fatalf("StorageUsage before: %v", err)
	}
	s1UsageBefore, err := s1.StorageUsage()
	if err != nil {
		t.Fatalf("s1.StorageUsage before: %v", err)
	}
	if s1UsageBefore.LogicalJournalBytes <= 0 || s1UsageBefore.DerivedIndexBytes <= 0 {
		t.Fatalf("expected positive journal and index bytes on s1, got %+v", s1UsageBefore)
	}
	if usageBefore.BodyBytes != int64(len(bodyPayload)) {
		t.Fatalf("expected BodyBytes=%d, got %d", len(bodyPayload), usageBefore.BodyBytes)
	}

	// Seal and compact orders-v1 -> orders-v2 without immediate reclamation first.
	compRes, err := store.SealAndCompactStream(ctx, CompactionRequest{
		SourceStream:      "orders-v1",
		DestinationStream: "orders-v2",
		Mode:              ModeRevisioned,
		Records: []CompactedRecord{
			{
				Record:         recordJSON("orders-v2", "rec-1", 1, nil, `{"step":2}`, nil),
				SourcePin:      *r1v2.Pin,
				IdempotencyKey: "k-c-1",
			},
			{
				Record:         recordJSON("orders-v2", "rec-body", 1, nil, `{"attached":true}`, &bRef),
				SourcePin:      *rBody.Pin,
				IdempotencyKey: "k-c-2",
			},
		},
		Reviewer:           testReviewer,
		Authorization:      testAuth,
		Note:               "compact orders-v1 into orders-v2",
		ReclaimPredecessor: false,
	})
	if err != nil {
		t.Fatalf("SealAndCompactStream: %v", err)
	}
	if compRes.Outcome != CompactionOutcomeCompacted || compRes.Seal == nil || len(compRes.ImportedPins) != 2 {
		t.Fatalf("unexpected CompactionResult: %+v", compRes)
	}

	// Verify s1 is now write-fenced (Append, Retire, Protect return ErrStreamSealed).
	if _, err := s1.Append(ctx, Write{
		Record:         recordJSON("orders-v1", "rec-new", 1, nil, `{"x":1}`, nil),
		IdempotencyKey: "k-fenced",
	}); !errors.Is(err, ErrStreamSealed) {
		t.Fatalf("expected ErrStreamSealed on Append to sealed s1, got %v", err)
	}
	if _, err := s1.Retire(ctx, "rec-1", RetireOperator); !errors.Is(err, ErrStreamSealed) {
		t.Fatalf("expected ErrStreamSealed on Retire to sealed s1, got %v", err)
	}

	// Verify detached stream-seal.json on s1.
	sealDoc, found, err := store.VerifyStreamSeal("orders-v1")
	if err != nil || !found || sealDoc.SealDigest != compRes.Seal.SealDigest {
		t.Fatalf("VerifyStreamSeal before reclaim: %+v, found=%v, err=%v", sealDoc, found, err)
	}

	// Open orders-v2 and inspect ContinuationState, Head, Revisions, ResolveSourcePin, and Release.
	s2, err := store.OpenStream("orders-v2", ModeRevisioned, WithContinues(*compRes.Continues))
	if err != nil {
		t.Fatalf("OpenStream orders-v2: %v", err)
	}
	cs, err := s2.ContinuationState()
	if err != nil {
		t.Fatalf("s2.ContinuationState: %v", err)
	}
	if !cs.IsContinuation || cs.ReviewPending || !cs.SourceSealed || cs.SourceSealDigest != compRes.Seal.SealDigest {
		t.Fatalf("unexpected ContinuationState: %+v", cs)
	}

	h1, ok, err := s2.Head("rec-1")
	if err != nil || !ok || h1.ImportedFrom == nil || h1.ImportedFrom.Pin != *r1v2.Pin {
		t.Fatalf("s2.Head(rec-1): %+v, ok=%v, err=%v", h1, ok, err)
	}
	revs1, err := s2.Revisions("rec-1")
	if err != nil || len(revs1) != 1 || revs1[0].ImportedFrom == nil || revs1[0].ImportedFrom.Pin != *r1v2.Pin {
		t.Fatalf("s2.Revisions(rec-1): %+v, err=%v", revs1, err)
	}
	resolvedPin, ok, err := s2.ResolveSourcePin(*r1v2.Pin)
	if err != nil || !ok || resolvedPin != h1.Pin {
		t.Fatalf("s2.ResolveSourcePin: %+v, ok=%v, err=%v", resolvedPin, ok, err)
	}

	sinkBefore := &memReleaseSink{}
	relBefore, err := s2.Release(ctx, UseRequest{Purpose: PurposeServe, Closure: ClosureFullContent}, sinkBefore)
	if err != nil || relBefore.Outcome != ReleaseReleased || len(sinkBefore.frames) != 3 {
		t.Fatalf("s2.Release before reclaim: %+v, frames=%d, err=%v", relBefore, len(sinkBefore.frames), err)
	}

	// Now reclaim the sealed predecessor orders-v1 with cold archive.
	archiveDir := newStoreDir(t)
	reclaimRes, err := store.ReclaimSealedAncestor(ctx, ReclaimRequest{
		PredecessorStream: "orders-v1",
		SuccessorStream:   "orders-v2",
		ArchiveDir:        archiveDir,
	})
	if err != nil {
		t.Fatalf("ReclaimSealedAncestor: %v", err)
	}
	if reclaimRes.Outcome != ReclaimOutcomeReclaimed ||
		reclaimRes.ReclaimedJournalBytes != s1UsageBefore.LogicalJournalBytes ||
		reclaimRes.ReclaimedIndexBytes != s1UsageBefore.DerivedIndexBytes {
		t.Fatalf("unexpected ReclaimResult: %+v (before=%+v)", reclaimRes, s1UsageBefore)
	}
	if _, err := os.Stat(reclaimRes.ArchivedJournalPath); err != nil {
		t.Fatalf("expected archived journal at %s: %v", reclaimRes.ArchivedJournalPath, err)
	}
	if _, err := os.Stat(reclaimRes.ArchivedSealPath); err != nil {
		t.Fatalf("expected archived seal at %s: %v", reclaimRes.ArchivedSealPath, err)
	}

	// Verify physical files in orders-v1 directory: journal.ndjson and *.idx are gone;
	// stream-seal.json and reclaimed.json remain.
	s1Dir := filepath.Join(root, streamDir("orders-v1"))
	for _, removedName := range []string{"journal.ndjson", "heads.idx", "keys.idx", "records.idx"} {
		if _, err := os.Stat(filepath.Join(s1Dir, removedName)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("expected %s to be unlinked after reclamation, got err=%v", removedName, err)
		}
	}
	for _, keptName := range []string{"stream-seal.json", "reclaimed.json"} {
		if _, err := os.Stat(filepath.Join(s1Dir, keptName)); err != nil {
			t.Fatalf("expected %s to remain after reclamation: %v", keptName, err)
		}
	}

	// Idempotent retry of ReclaimSealedAncestor returns ReclaimOutcomeAlreadyReclaimed.
	reclaimRetry, err := store.ReclaimSealedAncestor(ctx, ReclaimRequest{
		PredecessorStream: "orders-v1",
		SuccessorStream:   "orders-v2",
	})
	if err != nil || reclaimRetry.Outcome != ReclaimOutcomeAlreadyReclaimed {
		t.Fatalf("expected ReclaimOutcomeAlreadyReclaimed on retry, got %+v, err=%v", reclaimRetry, err)
	}

	// Verify StorageUsage after reclamation: s1 LogicalJournalBytes == 0 and DerivedIndexBytes == 0.
	s1UsageAfter, err := s1.StorageUsage()
	if err != nil {
		t.Fatalf("s1.StorageUsage after: %v", err)
	}
	if !s1UsageAfter.Sealed || !s1UsageAfter.Reclaimed || s1UsageAfter.LogicalJournalBytes != 0 || s1UsageAfter.DerivedIndexBytes != 0 {
		t.Fatalf("unexpected s1.StorageUsage after reclaim: %+v", s1UsageAfter)
	}
	storeUsageAfter, err := store.StorageUsage()
	if err != nil {
		t.Fatalf("store.StorageUsage after: %v", err)
	}
	if storeUsageAfter.TempRewriteBytes != 0 {
		t.Fatalf("expected 0 TempRewriteBytes, got %d", storeUsageAfter.TempRewriteBytes)
	}

	// Verify VerifyStreamSeal on orders-v1 still succeeds after journal reclamation.
	sealAfter, found, err := store.VerifyStreamSeal("orders-v1")
	if err != nil || !found || sealAfter.SealDigest != compRes.Seal.SealDigest {
		t.Fatalf("VerifyStreamSeal after reclaim: %+v, found=%v, err=%v", sealAfter, found, err)
	}

	// Verify s2 Head, Revisions, ResolveSourcePin, and Release still work after s1 is reclaimed!
	h1After, ok, err := s2.Head("rec-1")
	if err != nil || !ok || h1After.Pin != h1.Pin {
		t.Fatalf("s2.Head after reclaim: %+v, ok=%v, err=%v", h1After, ok, err)
	}
	resPinAfter, ok, err := s2.ResolveSourcePin(*r1v2.Pin)
	if err != nil || !ok || resPinAfter != h1.Pin {
		t.Fatalf("s2.ResolveSourcePin after reclaim: %+v, ok=%v, err=%v", resPinAfter, ok, err)
	}
	sinkAfter := &memReleaseSink{}
	relAfter, err := s2.Release(ctx, UseRequest{Purpose: PurposeServe, Closure: ClosureFullContent}, sinkAfter)
	if err != nil || relAfter.Outcome != ReleaseReleased || len(sinkAfter.frames) != 3 {
		t.Fatalf("s2.Release after reclaim: %+v, frames=%d, err=%v", relAfter, len(sinkAfter.frames), err)
	}

	// Verify RemoveBody is refused with BodyStillReferenced while s2 references bRef.
	remRefd, err := store.RemoveBody(ctx, RemoveBodyRequest{
		Digest:        bRef.Digest,
		Length:        bRef.Length,
		Reason:        RemoveReasonRetention,
		Provenance:    testReviewer,
		Authorization: &testAuth,
	})
	if err != nil || remRefd.Outcome != BodyStillReferenced {
		t.Fatalf("expected BodyStillReferenced while s2 references body, got %+v, err=%v", remRefd, err)
	}
}

func TestMultiHopCompactionReclamationAndBodyRemoval(t *testing.T) {
	ctx := context.Background()
	store, _, _ := openBodyStore(t)

	// S1: create rec-keep and rec-ephemeral (with external body).
	s1, err := store.OpenStream("hop-1", ModeRevisioned)
	if err != nil {
		t.Fatalf("OpenStream hop-1: %v", err)
	}
	rKeep1, err := s1.Append(ctx, Write{
		Record:         recordJSON("hop-1", "rec-keep", 1, nil, `{"gen":1}`, nil),
		IdempotencyKey: "k-h1-keep",
	})
	if err != nil || rKeep1.Outcome != OutcomeAppended {
		t.Fatalf("Append rKeep1: %+v, err=%v", rKeep1, err)
	}

	ephemeralBytes := []byte("ephemeral-body-bytes-to-reclaim-after-hop-3")
	bRef, lease, err := store.PutBody(ctx, bytes.NewReader(ephemeralBytes), "text/plain")
	if err != nil {
		t.Fatalf("PutBody: %v", err)
	}
	rEph1, err := s1.Append(ctx, Write{
		Record:         recordJSON("hop-1", "rec-eph", 1, nil, `{"eph":1}`, &bRef),
		IdempotencyKey: "k-h1-eph",
		Lease:          &lease,
	})
	if err != nil || rEph1.Outcome != OutcomeAppended {
		t.Fatalf("Append rEph1: %+v, err=%v", rEph1, err)
	}

	// Hop 1 -> Hop 2: compact both records and immediately reclaim hop-1.
	c12, err := store.SealAndCompactStream(ctx, CompactionRequest{
		SourceStream:      "hop-1",
		DestinationStream: "hop-2",
		Mode:              ModeRevisioned,
		Records: []CompactedRecord{
			{
				Record:         recordJSON("hop-2", "rec-keep", 1, nil, `{"gen":1}`, nil),
				SourcePin:      *rKeep1.Pin,
				IdempotencyKey: "k-h2-keep",
			},
			{
				Record:         recordJSON("hop-2", "rec-eph", 1, nil, `{"eph":1}`, &bRef),
				SourcePin:      *rEph1.Pin,
				IdempotencyKey: "k-h2-eph",
			},
		},
		Reviewer:           testReviewer,
		Authorization:      testAuth,
		ReclaimPredecessor: true,
	})
	if err != nil || c12.Outcome != CompactionOutcomeCompacted || c12.Reclaimed == nil || c12.Reclaimed.Outcome != ReclaimOutcomeReclaimed {
		t.Fatalf("SealAndCompactStream hop-1 -> hop-2: %+v, err=%v", c12, err)
	}

	// On hop-2: revise rec-keep to rev 2 and retire rec-eph on hop-2. Also append
	// a new record rec-h2-eph referencing bRef2, and retire rec-h2-eph on hop-2
	// BEFORE hop-2 is sealed.
	s2, err := store.OpenStream("hop-2", ModeRevisioned, WithContinues(*c12.Continues))
	if err != nil {
		t.Fatalf("OpenStream hop-2: %v", err)
	}
	ephemeralBytes2 := []byte("hop2-only-ephemeral-body-bytes-retired-before-seal")
	bRef2, lease2, err := store.PutBody(ctx, bytes.NewReader(ephemeralBytes2), "text/plain")
	if err != nil {
		t.Fatalf("PutBody bRef2: %v", err)
	}
	if _, err := s2.Append(ctx, Write{
		Record:         recordJSON("hop-2", "rec-h2-eph", 1, nil, `{"eph":2}`, &bRef2),
		IdempotencyKey: "k-h2-only-eph",
		Lease:          &lease2,
	}); err != nil {
		t.Fatalf("s2.Append rec-h2-eph: %v", err)
	}
	if _, err := s2.Retire(ctx, "rec-h2-eph", RetireRetention, WithRetireKey("k-h2-ret-only-eph")); err != nil {
		t.Fatalf("s2.Retire rec-h2-eph: %v", err)
	}
	hKeep2, ok, err := s2.Head("rec-keep")
	if err != nil || !ok {
		t.Fatalf("s2.Head(rec-keep): %v", err)
	}
	rKeep2v2, err := s2.Append(ctx, Write{
		Record:         recordJSON("hop-2", "rec-keep", 2, &hKeep2.Pin, `{"gen":2}`, nil),
		IdempotencyKey: "k-h2-keep-v2",
	})
	if err != nil || rKeep2v2.Outcome != OutcomeRevised {
		t.Fatalf("s2.Append rev 2: %+v, err=%v", rKeep2v2, err)
	}
	if _, err := s2.Retire(ctx, "rec-eph", RetireRetention, WithRetireKey("k-h2-ret-eph")); err != nil {
		t.Fatalf("s2.Retire rec-eph: %v", err)
	}
	// Retiring a record on a continuation stream adds a restriction entry after its
	// initial restrictions_reviewed; re-establish restrictions on hop-2 before sealing it.
	cs2, err := s2.ContinuationState()
	if err != nil || !cs2.ReviewPending {
		t.Fatalf("expected ReviewPending on s2 after Retire, got %+v, err=%v", cs2, err)
	}
	if _, err := s2.EstablishRestrictions(ctx, RestrictionReview{
		SourceCutSeq:                  cs2.Continues.CutSeq,
		SourceCutDigest:               cs2.Continues.CutDigest,
		SourceStateDigest:             cs2.SourceStateDigest,
		DestinationCutSeq:             cs2.DestinationCutSeq,
		DestinationAppliedStateDigest: cs2.DestinationAppliedStateDigest,
		Reviewer:                      testReviewer,
		Authorization:                 testAuth,
	}); err != nil {
		t.Fatalf("s2.EstablishRestrictions after Retire: %v", err)
	}

	// Hop 2 -> Hop 3: compact rec-keep (including rev 1 imported from hop-1 so we can test
	// 2-hop ResolveSourcePin!) and drop retired rec-eph and rec-h2-eph, reclaiming hop-2 as well!
	c23, err := store.SealAndCompactStream(ctx, CompactionRequest{
		SourceStream:      "hop-2",
		DestinationStream: "hop-3",
		Mode:              ModeRevisioned,
		Records: []CompactedRecord{
			{
				Record:         recordJSON("hop-3", "rec-keep", 1, nil, `{"gen":1}`, nil),
				SourcePin:      hKeep2.Pin,
				IdempotencyKey: "k-h3-keep-v1",
			},
		},
		Reviewer:           testReviewer,
		Authorization:      testAuth,
		ReclaimPredecessor: true,
	})
	if err != nil || c23.Outcome != CompactionOutcomeCompacted || c23.Reclaimed == nil || c23.Reclaimed.Outcome != ReclaimOutcomeReclaimed {
		t.Fatalf("SealAndCompactStream hop-2 -> hop-3: %+v, err=%v", c23, err)
	}

	// Verify hop-2's seal includes hop-1 in ancestor_seals.
	if len(c23.Seal.AncestorSeals) != 1 || c23.Seal.AncestorSeals[0].Stream != "hop-1" {
		t.Fatalf("expected hop-2 seal to include hop-1 in ancestor_seals, got %+v", c23.Seal.AncestorSeals)
	}

	// Open hop-3 and verify 2-hop ResolveSourcePin(rKeep1.Pin from hop-1 -> hop-2 -> hop-3)
	// with BOTH hop-1 and hop-2 reclaimed!
	s3, err := store.OpenStream("hop-3", ModeRevisioned, WithContinues(*c23.Continues))
	if err != nil {
		t.Fatalf("OpenStream hop-3: %v", err)
	}
	hKeep3, ok, err := s3.Head("rec-keep")
	if err != nil || !ok {
		t.Fatalf("s3.Head(rec-keep): %v", err)
	}
	resolvedFromHop1, ok, err := s3.ResolveSourcePin(*rKeep1.Pin)
	if err != nil || !ok || resolvedFromHop1 != hKeep3.Pin {
		t.Fatalf("2-hop ResolveSourcePin(hop-1 -> hop-3) failed: got %+v, ok=%v, err=%v, want %+v", resolvedFromHop1, ok, err, hKeep3.Pin)
	}

	// Verify s3.Release succeeds when both hop-1 and hop-2 are reclaimed.
	sink3 := &memReleaseSink{}
	rel3, err := s3.Release(ctx, UseRequest{Purpose: PurposeServe, Closure: ClosureFullContent}, sink3)
	if err != nil || rel3.Outcome != ReleaseReleased || len(sink3.frames) != 1 {
		t.Fatalf("s3.Release: %+v, frames=%d, err=%v", rel3, len(sink3.frames), err)
	}

	// Per durable-record/v1 §8, re-recording rec-eph in hop-2 did not extinguish hop-1's
	// unretired reference to bRef (rec-eph was unretired when hop-1 was sealed and reclaimed),
	// so RemoveBody(bRef) must still return BodyStillReferenced!
	remUnretiredAnc, err := store.RemoveBody(ctx, RemoveBodyRequest{
		Digest:        bRef.Digest,
		Length:        bRef.Length,
		Reason:        RemoveReasonRetention,
		Provenance:    testReviewer,
		Authorization: &testAuth,
	})
	if err != nil || remUnretiredAnc.Outcome != BodyStillReferenced {
		t.Fatalf("expected BodyStillReferenced for bRef (unretired at hop-1 cut), got %+v, err=%v", remUnretiredAnc, err)
	}

	// By contrast, rec-h2-eph (referencing bRef2) was retired on hop-2 BEFORE hop-2 was
	// sealed and reclaimed, so RemoveBody(bRef2) must succeed with BodyRemoved!
	remRes, err := store.RemoveBody(ctx, RemoveBodyRequest{
		Digest:        bRef2.Digest,
		Length:        bRef2.Length,
		Reason:        RemoveReasonRetention,
		Provenance:    testReviewer,
		Authorization: &testAuth,
	})
	if err != nil || remRes.Outcome != BodyRemoved {
		t.Fatalf("expected BodyRemoved for bRef2 after hop-2 retired it before seal+reclaim, got %+v, err=%v", remRes, err)
	}
}

func TestEstablishRestrictionsManualFlowAndStaleReview(t *testing.T) {
	ctx := context.Background()
	store, _, _ := openBodyStore(t)

	s1, err := store.OpenStream("manual-src", ModeSingleRevision)
	if err != nil {
		t.Fatalf("OpenStream manual-src: %v", err)
	}
	r1, err := s1.Append(ctx, Write{
		Record:         recordJSON("manual-src", "doc-1", 1, nil, `{"ok":true}`, nil),
		IdempotencyKey: "k-m-1",
	})
	if err != nil || r1.Outcome != OutcomeAppended {
		t.Fatalf("Append r1: %+v, err=%v", r1, err)
	}

	sealRes, err := store.SealStream(ctx, SealStreamRequest{
		Stream:        "manual-src",
		Provenance:    testReviewer,
		Authorization: testAuth,
	})
	if err != nil || sealRes.Outcome != SealOutcomeSealed {
		t.Fatalf("SealStream manual-src: %+v, err=%v", sealRes, err)
	}

	cont := Continues{
		Capability: "contract: durable-record/v1",
		Stream:     "manual-src",
		CutSeq:     sealRes.Seal.CutSeq,
		CutDigest:  sealRes.Seal.CutDigest,
	}
	s2, err := store.OpenStream("manual-dst", ModeSingleRevision, WithContinues(cont))
	if err != nil {
		t.Fatalf("OpenStream manual-dst: %v", err)
	}
	_, err = s2.Append(ctx, Write{
		Record:         recordJSON("manual-dst", "doc-1", 1, nil, `{"ok":true}`, nil),
		IdempotencyKey: "k-m-2",
		ImportedFrom: &SourcePin{
			Capability: "contract: durable-record/v1",
			Pin:        *r1.Pin,
		},
	})
	if err != nil {
		t.Fatalf("s2.Append imported: %v", err)
	}

	// Before EstablishRestrictions, Release on s2 must refuse with restrictions_pending.
	relPending, err := s2.Release(ctx, UseRequest{Purpose: PurposeServe, Closure: ClosureMetadataOnly}, nil)
	if err != nil || relPending.Outcome != ReleaseRefused || relPending.RefusalReason != "restrictions_pending" {
		t.Fatalf("expected ReleaseRefused(restrictions_pending), got %+v, err=%v", relPending, err)
	}

	cs, err := s2.ContinuationState()
	if err != nil || !cs.ReviewPending {
		t.Fatalf("ContinuationState before review: %+v, err=%v", cs, err)
	}

	// Stale digest must return ErrStaleReview.
	_, err = s2.EstablishRestrictions(ctx, RestrictionReview{
		SourceCutSeq:                  cont.CutSeq,
		SourceCutDigest:               cont.CutDigest,
		SourceStateDigest:             "sha256:" + strings.Repeat("0", 64),
		DestinationCutSeq:             cs.DestinationCutSeq,
		DestinationAppliedStateDigest: cs.DestinationAppliedStateDigest,
		Reviewer:                      testReviewer,
		Authorization:                 testAuth,
	})
	if !errors.Is(err, ErrStaleReview) {
		t.Fatalf("expected ErrStaleReview on bad SourceStateDigest, got %v", err)
	}

	// Valid EstablishRestrictions succeeds with RestrictionReviewCompleted.
	revRes, err := s2.EstablishRestrictions(ctx, RestrictionReview{
		SourceCutSeq:                  cont.CutSeq,
		SourceCutDigest:               cont.CutDigest,
		SourceStateDigest:             cs.SourceStateDigest,
		DestinationCutSeq:             cs.DestinationCutSeq,
		DestinationAppliedStateDigest: cs.DestinationAppliedStateDigest,
		Reviewer:                      testReviewer,
		Authorization:                 testAuth,
		Note:                          "manual review complete",
	})
	if err != nil || revRes.Outcome != RestrictionReviewCompleted {
		t.Fatalf("EstablishRestrictions: %+v, err=%v", revRes, err)
	}

	// Idempotent retry returns RestrictionReviewAlreadyCompleted.
	revRetry, err := s2.EstablishRestrictions(ctx, RestrictionReview{
		SourceCutSeq:                  cont.CutSeq,
		SourceCutDigest:               cont.CutDigest,
		SourceStateDigest:             cs.SourceStateDigest,
		DestinationCutSeq:             cs.DestinationCutSeq,
		DestinationAppliedStateDigest: cs.DestinationAppliedStateDigest,
		Reviewer:                      testReviewer,
		Authorization:                 testAuth,
		Note:                          "manual review complete",
	})
	if err != nil || revRetry.Outcome != RestrictionReviewAlreadyCompleted || revRetry.Seq != revRes.Seq {
		t.Fatalf("EstablishRestrictions retry: %+v, err=%v", revRetry, err)
	}

	// Release now succeeds!
	relOK, err := s2.Release(ctx, UseRequest{Purpose: PurposeServe, Closure: ClosureMetadataOnly}, nil)
	if err != nil || relOK.Outcome != ReleaseReleased {
		t.Fatalf("expected ReleaseReleased after EstablishRestrictions, got %+v, err=%v", relOK, err)
	}
}

func TestSealAndReclaimTamperFailsClosed(t *testing.T) {
	ctx := context.Background()
	store, root, _ := openBodyStore(t)

	s1, err := store.OpenStream("tamper-src", ModeSingleRevision)
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}
	r1, err := s1.Append(ctx, Write{
		Record:         recordJSON("tamper-src", "rec-1", 1, nil, `{"v":1}`, nil),
		IdempotencyKey: "k-t-1",
	})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}

	bodyBytes := []byte("tamper-test-body")
	bRef, lease, err := store.PutBody(ctx, bytes.NewReader(bodyBytes), "text/plain")
	if err != nil {
		t.Fatalf("PutBody: %v", err)
	}
	_, _ = lease, bRef

	compRes, err := store.SealAndCompactStream(ctx, CompactionRequest{
		SourceStream:      "tamper-src",
		DestinationStream: "tamper-dst",
		Mode:              ModeSingleRevision,
		Records: []CompactedRecord{
			{
				Record:         recordJSON("tamper-dst", "rec-1", 1, nil, `{"v":1}`, nil),
				SourcePin:      *r1.Pin,
				IdempotencyKey: "k-t-imp-1",
			},
		},
		Reviewer:           testReviewer,
		Authorization:      testAuth,
		ReclaimPredecessor: true,
	})
	if err != nil || compRes.Outcome != CompactionOutcomeCompacted {
		t.Fatalf("SealAndCompactStream: %+v, err=%v", compRes, err)
	}

	s2, err := store.OpenStream("tamper-dst", ModeSingleRevision, WithContinues(*compRes.Continues))
	if err != nil {
		t.Fatalf("OpenStream tamper-dst: %v", err)
	}

	s1Dir := filepath.Join(root, streamDir("tamper-src"))
	sealPath := filepath.Join(s1Dir, "stream-seal.json")
	reclaimedPath := filepath.Join(s1Dir, "reclaimed.json")

	origSeal, err := os.ReadFile(sealPath)
	if err != nil {
		t.Fatalf("ReadFile seal: %v", err)
	}
	origReclaimed, err := os.ReadFile(reclaimedPath)
	if err != nil {
		t.Fatalf("ReadFile reclaimed: %v", err)
	}

	// 1. Tamper with reclaimed.json (modify cut_seq or manifest while keeping JSON valid) -> fails closed.
	tamperedReclaimed := bytes.Replace(origReclaimed, []byte(`"v":1`), []byte(`"v":1,"has_ancestor_hold":true`), 1)
	if err := os.WriteFile(reclaimedPath, tamperedReclaimed, 0o600); err != nil {
		t.Fatalf("WriteFile tampered reclaimed: %v", err)
	}
	os.Chmod(reclaimedPath, 0o600)

	if _, _, err := store.VerifyStreamSeal("tamper-src"); err == nil {
		t.Fatal("expected VerifyStreamSeal to fail closed on tampered reclaimed.json")
	}
	if _, _, err := s2.Head("rec-1"); err == nil {
		t.Fatal("expected s2.Head to fail closed on tampered predecessor reclaimed.json")
	}
	relTamper1, err := s2.Release(ctx, UseRequest{Purpose: PurposeServe, Closure: ClosureMetadataOnly}, nil)
	if err != nil || relTamper1.Outcome != ReleaseRefused || relTamper1.RefusalReason != "protection_unavailable" {
		t.Fatalf("expected s2.Release to refuse with protection_unavailable on tampered reclaimed.json, got %+v, err=%v", relTamper1, err)
	}
	remTamper1, err := store.RemoveBody(ctx, RemoveBodyRequest{
		Digest:        bRef.Digest,
		Length:        bRef.Length,
		Reason:        RemoveReasonOperator,
		Provenance:    testReviewer,
		Authorization: &testAuth,
	})
	if err != nil || remTamper1.Outcome != BodyProtectionUnknown {
		t.Fatalf("expected RemoveBody to return BodyProtectionUnknown on tampered reclaimed.json, got %+v, err=%v", remTamper1, err)
	}

	// Restore reclaimed.json; now tamper with stream-seal.json -> fails closed.
	if err := os.WriteFile(reclaimedPath, origReclaimed, 0o600); err != nil {
		t.Fatalf("restore reclaimed.json: %v", err)
	}
	os.Chmod(reclaimedPath, 0o600)

	tamperedSeal := bytes.Replace(origSeal, []byte(`"alice@example.com"`), []byte(`"mallory@example.com"`), 1)
	if err := os.WriteFile(sealPath, tamperedSeal, 0o600); err != nil {
		t.Fatalf("WriteFile tampered seal: %v", err)
	}
	os.Chmod(sealPath, 0o600)

	if _, _, err := store.VerifyStreamSeal("tamper-src"); err == nil {
		t.Fatal("expected VerifyStreamSeal to fail closed on tampered stream-seal.json")
	}
	if _, _, err := s2.Head("rec-1"); err == nil {
		t.Fatal("expected s2.Head to fail closed on tampered predecessor stream-seal.json")
	}

	// Restore stream-seal.json and verify everything succeeds again.
	if err := os.WriteFile(sealPath, origSeal, 0o600); err != nil {
		t.Fatalf("restore stream-seal.json: %v", err)
	}
	os.Chmod(sealPath, 0o600)
	if _, ok, err := s2.Head("rec-1"); err != nil || !ok {
		t.Fatalf("s2.Head after restoring seal: ok=%v, err=%v", ok, err)
	}
}

func TestCompactionFaultInjectionAndIdempotentRetry(t *testing.T) {
	ctx := context.Background()
	phases := []CompactionFaultPhase{
		CompactionPhaseAfterSourceSeal,
		CompactionPhaseAfterDestinationOpen,
		CompactionPhaseAfterRecordImports,
		CompactionPhaseAfterRestrictionReview,
	}

	for _, faultPhase := range phases {
		t.Run(string(faultPhase), func(t *testing.T) {
			store, _, _ := openBodyStore(t)
			s1, err := store.OpenStream("src-fault", ModeRevisioned)
			if err != nil {
				t.Fatalf("OpenStream: %v", err)
			}
			r1, err := s1.Append(ctx, Write{
				Record:         recordJSON("src-fault", "rec-1", 1, nil, `{"v":1}`, nil),
				IdempotencyKey: "k-f-1",
			})
			if err != nil {
				t.Fatalf("Append r1: %v", err)
			}
			r2, err := s1.Append(ctx, Write{
				Record:         recordJSON("src-fault", "rec-2", 1, nil, `{"v":2}`, nil),
				IdempotencyKey: "k-f-2",
			})
			if err != nil {
				t.Fatalf("Append r2: %v", err)
			}

			req := CompactionRequest{
				SourceStream:      "src-fault",
				DestinationStream: "dst-fault",
				Mode:              ModeRevisioned,
				Records: []CompactedRecord{
					{
						Record:         recordJSON("dst-fault", "rec-1", 1, nil, `{"v":1}`, nil),
						SourcePin:      *r1.Pin,
						IdempotencyKey: "k-imp-1",
					},
					{
						Record:         recordJSON("dst-fault", "rec-2", 1, nil, `{"v":2}`, nil),
						SourcePin:      *r2.Pin,
						IdempotencyKey: "k-imp-2",
					},
				},
				Reviewer:           testReviewer,
				Authorization:      testAuth,
				ReclaimPredecessor: true,
				FaultHook: func(p CompactionFaultPhase) error {
					if p == faultPhase {
						return errors.New("injected compaction fault")
					}
					return nil
				},
			}

			_, err = store.SealAndCompactStream(ctx, req)
			if err == nil {
				t.Fatalf("expected error at fault phase %s", faultPhase)
			}
			if !IsIndeterminate(err) {
				t.Fatalf("expected IsIndeterminate(err)==true at phase %s, got %v", faultPhase, err)
			}
			var ci *ErrCompactionIncomplete
			if !errors.As(err, &ci) {
				t.Fatalf("expected *ErrCompactionIncomplete at phase %s, got %T (%v)", faultPhase, err, err)
			}

			// Retry the exact same request without the fault hook: must converge to compacted!
			req.FaultHook = nil
			res, err := store.SealAndCompactStream(ctx, req)
			if err != nil {
				t.Fatalf("retry SealAndCompactStream after %s: %v", faultPhase, err)
			}
			if res.Outcome != CompactionOutcomeCompacted || res.Reclaimed == nil || res.Reclaimed.Outcome != ReclaimOutcomeReclaimed {
				t.Fatalf("unexpected retry result after %s: %+v", faultPhase, res)
			}

			// Second retry must return CompactionOutcomeAlreadyCompacted.
			res2, err := store.SealAndCompactStream(ctx, req)
			if err != nil || res2.Outcome != CompactionOutcomeAlreadyCompacted {
				t.Fatalf("expected CompactionOutcomeAlreadyCompacted on second retry, got %+v, err=%v", res2, err)
			}

			// Verify destination stream serves both records after recovery.
			s2, err := store.OpenStream("dst-fault", ModeRevisioned, WithContinues(*res.Continues))
			if err != nil {
				t.Fatalf("OpenStream dst-fault: %v", err)
			}
			sink := &memReleaseSink{}
			rel, err := s2.Release(ctx, UseRequest{Purpose: PurposeServe, Closure: ClosureMetadataOnly}, sink)
			if err != nil || rel.Outcome != ReleaseReleased || len(sink.frames) != 2 {
				t.Fatalf("s2.Release after recovery: %+v, frames=%d, err=%v", rel, len(sink.frames), err)
			}
		})
	}
}

func TestArchiveSymlinkAndTrustProfileAndProvenanceRefusals(t *testing.T) {
	ctx := context.Background()
	store, root, _ := openBodyStore(t)

	s1, err := store.OpenStream("guard-src", ModeSingleRevision)
	if err != nil {
		t.Fatalf("OpenStream guard-src: %v", err)
	}
	r1, err := s1.Append(ctx, Write{
		Record:         recordJSON("guard-src", "rec-1", 1, nil, `{"v":1}`, nil),
		IdempotencyKey: "k-g-1",
	})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}

	// Seal guard-src.
	sealRes, err := store.SealStream(ctx, SealStreamRequest{
		Stream:        "guard-src",
		Provenance:    testReviewer,
		Authorization: testAuth,
	})
	if err != nil || sealRes.Outcome != SealOutcomeSealed {
		t.Fatalf("SealStream: %+v, err=%v", sealRes, err)
	}

	// 1. Tampering with root stream's trust_profile in stream-seal.json must fail VerifyStreamSeal.
	sealPath := filepath.Join(root, streamDir("guard-src"), "stream-seal.json")
	origSeal, err := os.ReadFile(sealPath)
	if err != nil {
		t.Fatalf("ReadFile seal: %v", err)
	}
	badProfileSeal := bytes.Replace(origSeal, []byte(`"recordprims-fs"`), []byte(`"unaccepted-profile"`), 1)
	if err := os.WriteFile(sealPath, badProfileSeal, 0o600); err != nil {
		t.Fatalf("WriteFile bad profile seal: %v", err)
	}
	os.Chmod(sealPath, 0o600)
	if _, _, err := store.VerifyStreamSeal("guard-src"); err == nil {
		t.Fatal("expected VerifyStreamSeal to reject unaccepted trust_profile on root stream")
	}
	if err := os.WriteFile(sealPath, origSeal, 0o600); err != nil {
		t.Fatalf("restore seal: %v", err)
	}
	os.Chmod(sealPath, 0o600)

	// 2. Pre-populating destination stream with a record that lacks imported_from must be refused.
	cont := Continues{
		Capability: "contract: durable-record/v1",
		Stream:     "guard-src",
		CutSeq:     sealRes.Seal.CutSeq,
		CutDigest:  sealRes.Seal.CutDigest,
	}
	sBadDst, err := store.OpenStream("guard-bad-dst", ModeSingleRevision, WithContinues(cont))
	if err != nil {
		t.Fatalf("OpenStream guard-bad-dst: %v", err)
	}
	if _, err := sBadDst.Append(ctx, Write{
		Record:         recordJSON("guard-bad-dst", "rec-1", 1, nil, `{"v":1}`, nil),
		IdempotencyKey: "k-unimported",
	}); err != nil {
		t.Fatalf("Append unimported to guard-bad-dst: %v", err)
	}
	if _, err := store.SealAndCompactStream(ctx, CompactionRequest{
		SourceStream:      "guard-src",
		DestinationStream: "guard-bad-dst",
		Mode:              ModeSingleRevision,
		Records: []CompactedRecord{
			{
				Record:         recordJSON("guard-bad-dst", "rec-1", 1, nil, `{"v":1}`, nil),
				SourcePin:      *r1.Pin,
				IdempotencyKey: "k-unimported",
			},
		},
		Reviewer:      testReviewer,
		Authorization: testAuth,
	}); !errors.Is(err, ErrSchema) {
		t.Fatalf("expected ErrSchema when destination record lacks imported_from provenance, got %v", err)
	}

	// 3. Compact into a clean destination without reclaiming yet, then test symlink refusal in ArchiveDir.
	compRes, err := store.SealAndCompactStream(ctx, CompactionRequest{
		SourceStream:      "guard-src",
		DestinationStream: "guard-good-dst",
		Mode:              ModeSingleRevision,
		Records: []CompactedRecord{
			{
				Record:         recordJSON("guard-good-dst", "rec-1", 1, nil, `{"v":1}`, nil),
				SourcePin:      *r1.Pin,
				IdempotencyKey: "k-imp-good",
			},
		},
		Reviewer:           testReviewer,
		Authorization:      testAuth,
		ReclaimPredecessor: false,
	})
	if err != nil || compRes.Outcome != CompactionOutcomeCompacted {
		t.Fatalf("SealAndCompactStream guard-good-dst: %+v, err=%v", compRes, err)
	}

	sentinelDir := newStoreDir(t)
	sentinelPath := filepath.Join(sentinelDir, "sentinel.txt")
	if err := os.WriteFile(sentinelPath, []byte("DO-NOT-OVERWRITE"), 0o600); err != nil {
		t.Fatalf("WriteFile sentinel: %v", err)
	}

	archDir := newStoreDir(t)
	sh := streamHash("guard-src")
	// Plant a symlink at the target archive filename pointing to sentinelPath.
	if err := os.Symlink(sentinelPath, filepath.Join(archDir, sh+"-journal.ndjson")); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	if _, err := store.ReclaimSealedAncestor(ctx, ReclaimRequest{
		PredecessorStream: "guard-src",
		SuccessorStream:   "guard-good-dst",
		ArchiveDir:        archDir,
	}); !errors.Is(err, ErrStoreInvalid) {
		t.Fatalf("expected ErrStoreInvalid on symlinked archive target, got %v", err)
	}
	gotSentinel, err := os.ReadFile(sentinelPath)
	if err != nil || string(gotSentinel) != "DO-NOT-OVERWRITE" {
		t.Fatalf("sentinel file modified or unreadable: %q, err=%v", gotSentinel, err)
	}
}

func TestSupersededBodyPreservationAndRetiredIDReuseRefusal(t *testing.T) {
	ctx := context.Background()
	store, _, _ := openBodyStore(t)

	s1, err := store.OpenStream("sup-src", ModeRevisioned)
	if err != nil {
		t.Fatalf("OpenStream sup-src: %v", err)
	}
	bRef1, lease1, err := store.PutBody(ctx, bytes.NewReader([]byte("superseded-rev-1-body")), "text/plain")
	if err != nil {
		t.Fatalf("PutBody bRef1: %v", err)
	}
	r1v1, err := s1.Append(ctx, Write{
		Record:         recordJSON("sup-src", "rec-sup", 1, nil, `{"v":1}`, &bRef1),
		IdempotencyKey: "k-sup-1",
		Lease:          &lease1,
	})
	if err != nil {
		t.Fatalf("Append rev 1: %v", err)
	}
	// Revision 2 is inline (no body), superseding rev 1 which had bRef1.
	r1v2, err := s1.Append(ctx, Write{
		Record:         recordJSON("sup-src", "rec-sup", 2, r1v1.Pin, `{"v":2}`, nil),
		IdempotencyKey: "k-sup-2",
	})
	if err != nil {
		t.Fatalf("Append rev 2: %v", err)
	}

	//Also append and retire rec-retired on sup-src.
	if _, err := s1.Append(ctx, Write{
		Record:         recordJSON("sup-src", "rec-retired", 1, nil, `{"ret":true}`, nil),
		IdempotencyKey: "k-ret-1",
	}); err != nil {
		t.Fatalf("Append rec-retired: %v", err)
	}
	if _, err := s1.Retire(ctx, "rec-retired", RetireRetention, WithRetireKey("k-ret-2")); err != nil {
		t.Fatalf("Retire rec-retired: %v", err)
	}

	// 1. Compacting only rev 2 (inline) of unretired rec-sup drops bRef1 from rev 1 -> must refuse with CompactionRefusedBodyUnpreserved!
	cDropSupBody, err := store.SealAndCompactStream(ctx, CompactionRequest{
		SourceStream:      "sup-src",
		DestinationStream: "sup-dst",
		Mode:              ModeRevisioned,
		Records: []CompactedRecord{
			{
				Record:         recordJSON("sup-dst", "rec-sup", 1, nil, `{"v":2}`, nil),
				SourcePin:      *r1v2.Pin,
				IdempotencyKey: "k-imp-v2-only",
			},
		},
		Reviewer:      testReviewer,
		Authorization: testAuth,
	})
	if err != nil || cDropSupBody.Outcome != CompactionOutcomeRefused || cDropSupBody.RefusalReason != CompactionRefusedBodyUnpreserved {
		t.Fatalf("expected CompactionRefusedBodyUnpreserved when superseded revision body is omitted, got %+v, err=%v", cDropSupBody, err)
	}

	// 2. Manual continuation stream that reuses retired record_id "rec-retired" without imported_from
	// must be rejected by EstablishRestrictions with ErrSchema (unimported_retired_record_id).
	sealRes, err := store.SealStream(ctx, SealStreamRequest{
		Stream:        "sup-src",
		Provenance:    testReviewer,
		Authorization: testAuth,
	})
	if err != nil || sealRes.Outcome != SealOutcomeSealed {
		t.Fatalf("SealStream sup-src: %+v, err=%v", sealRes, err)
	}
	cont := Continues{
		Capability: "contract: durable-record/v1",
		Stream:     "sup-src",
		CutSeq:     sealRes.Seal.CutSeq,
		CutDigest:  sealRes.Seal.CutDigest,
	}
	sReuse, err := store.OpenStream("sup-reuse-dst", ModeRevisioned, WithContinues(cont))
	if err != nil {
		t.Fatalf("OpenStream sup-reuse-dst: %v", err)
	}
	rcptReborn, err := sReuse.Append(ctx, Write{
		Record:         recordJSON("sup-reuse-dst", "rec-retired", 1, nil, `{"reborn":true}`, nil),
		IdempotencyKey: "k-reborn",
	})
	if err != nil || rcptReborn.Outcome != OutcomeRetiredRecord {
		t.Fatalf("expected OutcomeRetiredRecord on Append reborn rec-retired, got %+v, err=%v", rcptReborn, err)
	}
	// Appending an unrequested record to sup-reuse-dst causes SealAndCompactStream with only rec-sup to fail with ErrSchema!
	if _, err := sReuse.Append(ctx, Write{
		Record:         recordJSON("sup-reuse-dst", "rec-extra", 1, nil, `{"extra":true}`, nil),
		IdempotencyKey: "k-extra",
	}); err != nil {
		t.Fatalf("Append rec-extra: %v", err)
	}
	if _, err := store.SealAndCompactStream(ctx, CompactionRequest{
		SourceStream:       "sup-src",
		DestinationStream:  "sup-reuse-dst",
		Mode:               ModeRevisioned,
		AllowDropUnretired: true,
		Records: []CompactedRecord{
			{
				Record:         recordJSON("sup-reuse-dst", "rec-sup", 1, nil, `{"v":1}`, &bRef1),
				SourcePin:      *r1v1.Pin,
				IdempotencyKey: "k-imp-sup-1",
			},
		},
		Reviewer:      testReviewer,
		Authorization: testAuth,
	}); !errors.Is(err, ErrSchema) {
		t.Fatalf("expected ErrSchema when pre-existing destination has unrequested records, got %v", err)
	}

	// 3. If a record is appended to a continuation stream before being retired in an unsealed source stream,
	// EstablishRestrictions must reject it with ErrSchema (unimported_retired_record_id) and Release must refuse!
	sLateSrc, err := store.OpenStream("late-ret-src", ModeSingleRevision)
	if err != nil {
		t.Fatalf("OpenStream late-ret-src: %v", err)
	}
	rLate, err := sLateSrc.Append(ctx, Write{
		Record:         recordJSON("late-ret-src", "rec-late", 1, nil, `{"late":1}`, nil),
		IdempotencyKey: "k-late-1",
	})
	if err != nil {
		t.Fatalf("Append rec-late: %v", err)
	}
	// Read cut digest of late-ret-src at seq 2 via a temporary seal check or ContinuationState.
	lateCont := Continues{
		Capability: "contract: durable-record/v1",
		Stream:     "late-ret-src",
		CutSeq:     uint64(rLate.Seq),
		CutDigest:  mustCutDigest(t, store, "late-ret-src", rLate.Seq),
	}
	sLateDst, err := store.OpenStream("late-ret-dst", ModeSingleRevision, WithContinues(lateCont))
	if err != nil {
		t.Fatalf("OpenStream late-ret-dst: %v", err)
	}
	if _, err := sLateDst.Append(ctx, Write{
		Record:         recordJSON("late-ret-dst", "rec-late", 1, nil, `{"late":1}`, nil),
		IdempotencyKey: "k-late-dst-1",
		ImportedFrom: &SourcePin{
			Capability: "contract: durable-record/v1",
			Pin:        *rLate.Pin,
		},
	}); err != nil {
		t.Fatalf("sLateDst.Append rec-late: %v", err)
	}
	// Now retire rec-late on late-ret-src without retiring it on late-ret-dst!
	if _, err := sLateSrc.Retire(ctx, "rec-late", RetireWithdrawn, WithRetireKey("k-late-ret")); err != nil {
		t.Fatalf("sLateSrc.Retire rec-late: %v", err)
	}
	csLate, err := sLateDst.ContinuationState()
	if err != nil {
		t.Fatalf("sLateDst.ContinuationState: %v", err)
	}
	if _, err := sLateDst.EstablishRestrictions(ctx, RestrictionReview{
		SourceCutSeq:                  3,
		SourceCutDigest:               mustCutDigest(t, store, "late-ret-src", 3),
		SourceStateDigest:             csLate.SourceStateDigest,
		DestinationCutSeq:             csLate.DestinationCutSeq,
		DestinationAppliedStateDigest: csLate.DestinationAppliedStateDigest,
		Reviewer:                      testReviewer,
		Authorization:                 testAuth,
	}); !errors.Is(err, ErrSchema) {
		t.Fatalf("expected ErrSchema (unimported_retired_record_id) on EstablishRestrictions when source record is retired, got %v", err)
	}
	relLate, err := sLateDst.Release(ctx, UseRequest{Purpose: PurposeServe, Closure: ClosureMetadataOnly}, nil)
	if err != nil || relLate.Outcome != ReleaseRefused || relLate.RefusalReason != "restrictions_pending" {
		t.Fatalf("expected ReleaseRefused(restrictions_pending) on sLateDst, got %+v, err=%v", relLate, err)
	}

	// 4. Compact sup-src cleanly into sup-clean-dst (preserving both revisions of rec-sup, dropping retired rec-retired).
	cClean, err := store.SealAndCompactStream(ctx, CompactionRequest{
		SourceStream:      "sup-src",
		DestinationStream: "sup-clean-dst",
		Mode:              ModeRevisioned,
		Records: []CompactedRecord{
			{
				Record:         recordJSON("sup-clean-dst", "rec-sup", 1, nil, `{"v":1}`, &bRef1),
				SourcePin:      *r1v1.Pin,
				IdempotencyKey: "k-clean-v1",
			},
			{
				Record:         recordJSON("sup-clean-dst", "rec-sup", 2, &Pin{Stream: "sup-clean-dst", RecordID: "rec-sup", Revision: 1, Digest: mustRecordDigest(t, recordJSON("sup-clean-dst", "rec-sup", 1, nil, `{"v":1}`, &bRef1))}, `{"v":2}`, nil),
				SourcePin:      *r1v2.Pin,
				IdempotencyKey: "k-clean-v2",
			},
		},
		Reviewer:           testReviewer,
		Authorization:      testAuth,
		ReclaimPredecessor: false,
	})
	if err != nil || cClean.Outcome != CompactionOutcomeCompacted {
		t.Fatalf("SealAndCompactStream sup-clean-dst: %+v, err=%v", cClean, err)
	}
	sClean, err := store.OpenStream("sup-clean-dst", ModeRevisioned, WithContinues(*cClean.Continues))
	if err != nil {
		t.Fatalf("OpenStream sup-clean-dst: %v", err)
	}
	// Attempting to append retired ancestor record "rec-retired" after review (with live predecessor) must return OutcomeRetiredRecord!
	rcptRetLive, err := sClean.Append(ctx, Write{
		Record:         recordJSON("sup-clean-dst", "rec-retired", 1, nil, `{"after_review":1}`, nil),
		IdempotencyKey: "k-after-rev-live",
	})
	if err != nil || rcptRetLive.Outcome != OutcomeRetiredRecord {
		t.Fatalf("expected OutcomeRetiredRecord appending retired ancestor record with live predecessor, got %+v, err=%v", rcptRetLive, err)
	}
	// Reclaim sup-src and verify appending retired ancestor record "rec-retired" STILL returns OutcomeRetiredRecord!
	reclClean, err := store.ReclaimSealedAncestor(ctx, ReclaimRequest{
		PredecessorStream: "sup-src",
		SuccessorStream:   "sup-clean-dst",
	})
	if err != nil || reclClean.Outcome != ReclaimOutcomeReclaimed {
		t.Fatalf("ReclaimSealedAncestor sup-src: %+v, err=%v", reclClean, err)
	}
	rcptRetRecl, err := sClean.Append(ctx, Write{
		Record:         recordJSON("sup-clean-dst", "rec-retired", 1, nil, `{"after_review":2}`, nil),
		IdempotencyKey: "k-after-rev-recl",
	})
	if err != nil || rcptRetRecl.Outcome != OutcomeRetiredRecord {
		t.Fatalf("expected OutcomeRetiredRecord appending retired ancestor record with reclaimed predecessor, got %+v, err=%v", rcptRetRecl, err)
	}
	// Also verify Head("rec-late") on sLateDst reports Retired == true because rec-late is retired in late-ret-src!
	hLate, ok, err := sLateDst.Head("rec-late")
	if err != nil || !ok || !hLate.Retired {
		t.Fatalf("expected sLateDst.Head(rec-late) to report Retired=true, got %+v, ok=%v, err=%v", hLate, ok, err)
	}
}

func TestHistoricalOpeningCutReclaimedPredecessor(t *testing.T) {
	ctx := context.Background()
	store, _, _ := openBodyStore(t)

	sSrc, err := store.OpenStream("hist-src", ModeSingleRevision)
	if err != nil {
		t.Fatalf("OpenStream hist-src: %v", err)
	}
	r1, err := sSrc.Append(ctx, Write{
		Record:         recordJSON("hist-src", "h-1", 1, nil, `{"v":1}`, nil),
		IdempotencyKey: "k-h1",
	})
	if err != nil {
		t.Fatalf("Append h-1: %v", err)
	}

	// Open a continuation stream at cut 2 and import h-1.
	histCont := Continues{
		Capability: "contract: durable-record/v1",
		Stream:     "hist-src",
		CutSeq:     uint64(r1.Seq),
		CutDigest:  mustCutDigest(t, store, "hist-src", r1.Seq),
	}
	sHistCont, err := store.OpenStream("hist-cont", ModeSingleRevision, WithContinues(histCont))
	if err != nil {
		t.Fatalf("OpenStream hist-cont: %v", err)
	}
	if _, err := sHistCont.Append(ctx, Write{
		Record:         recordJSON("hist-cont", "h-1", 1, nil, `{"v":1}`, nil),
		IdempotencyKey: "k-hist-imp-1",
		ImportedFrom: &SourcePin{
			Capability: "contract: durable-record/v1",
			Pin:        *r1.Pin,
		},
	}); err != nil {
		t.Fatalf("sHistCont.Append h-1: %v", err)
	}

	// Advance hist-src to cut 3, then seal hist-src at cut 3 via SealAndCompactStream into hist-comp-dst.
	if _, err := sSrc.Append(ctx, Write{
		Record:         recordJSON("hist-src", "h-2", 1, nil, `{"v":2}`, nil),
		IdempotencyKey: "k-h2",
	}); err != nil {
		t.Fatalf("Append h-2: %v", err)
	}
	cRes, err := store.SealAndCompactStream(ctx, CompactionRequest{
		SourceStream:       "hist-src",
		DestinationStream:  "hist-comp-dst",
		Mode:               ModeSingleRevision,
		AllowDropUnretired: true,
		Records: []CompactedRecord{
			{
				Record:         recordJSON("hist-comp-dst", "h-1", 1, nil, `{"v":1}`, nil),
				SourcePin:      *r1.Pin,
				IdempotencyKey: "k-comp-h1",
			},
		},
		Reviewer:           testReviewer,
		Authorization:      testAuth,
		ReclaimPredecessor: false,
	})
	if err != nil || cRes.Outcome != CompactionOutcomeCompacted {
		t.Fatalf("SealAndCompactStream hist-src: %+v, err=%v", cRes, err)
	}

	// Review hist-cont (which opened at cut 2) against the final sealed cut 3 of hist-src.
	csHist, err := sHistCont.ContinuationState()
	if err != nil {
		t.Fatalf("sHistCont.ContinuationState: %v", err)
	}
	if _, err := sHistCont.EstablishRestrictions(ctx, RestrictionReview{
		SourceCutSeq:                  cRes.Continues.CutSeq,
		SourceCutDigest:               cRes.Continues.CutDigest,
		SourceStateDigest:             csHist.SourceStateDigest,
		DestinationCutSeq:             csHist.DestinationCutSeq,
		DestinationAppliedStateDigest: csHist.DestinationAppliedStateDigest,
		Reviewer:                      testReviewer,
		Authorization:                 testAuth,
	}); err != nil {
		t.Fatalf("sHistCont.EstablishRestrictions at cut 3: %v", err)
	}

	// Reclaim hist-src naming hist-cont (opened at historical cut 2, reviewed at sealed cut 3) as SuccessorStream!
	reclRes, err := store.ReclaimSealedAncestor(ctx, ReclaimRequest{
		PredecessorStream: "hist-src",
		SuccessorStream:   "hist-cont",
	})
	if err != nil || reclRes.Outcome != ReclaimOutcomeReclaimed {
		t.Fatalf("ReclaimSealedAncestor hist-src via hist-cont: %+v, err=%v", reclRes, err)
	}

	// Reclaiming again naming a different successor must be refused!
	reclOther, err := store.ReclaimSealedAncestor(ctx, ReclaimRequest{
		PredecessorStream: "hist-src",
		SuccessorStream:   "hist-comp-dst",
	})
	if err != nil || reclOther.Outcome != ReclaimOutcomeRefused {
		t.Fatalf("expected ReclaimOutcomeRefused when already reclaimed into different successor, got %+v, err=%v", reclOther, err)
	}

	// Both hist-cont (opened at cut 2) and hist-comp-dst (opened at cut 3) must remain readable and releasable after reclamation!
	if h, ok, err := sHistCont.Head("h-1"); err != nil || !ok || h.Pin.Revision != 1 {
		t.Fatalf("sHistCont.Head(h-1) after predecessor reclamation: h=%+v, ok=%v, err=%v", h, ok, err)
	}
	relHist, err := sHistCont.Release(ctx, UseRequest{Purpose: PurposeServe, Closure: ClosureMetadataOnly}, &memReleaseSink{})
	if err != nil || relHist.Outcome != ReleaseReleased {
		t.Fatalf("sHistCont.Release after predecessor reclamation: %+v, err=%v", relHist, err)
	}
	sCompDst, err := store.OpenStream("hist-comp-dst", ModeSingleRevision, WithContinues(*cRes.Continues))
	if err != nil {
		t.Fatalf("OpenStream hist-comp-dst after predecessor reclamation: %v", err)
	}
	relComp, err := sCompDst.Release(ctx, UseRequest{Purpose: PurposeServe, Closure: ClosureMetadataOnly}, &memReleaseSink{})
	if err != nil || relComp.Outcome != ReleaseReleased {
		t.Fatalf("sCompDst.Release after predecessor reclamation: %+v, err=%v", relComp, err)
	}
}

func mustCutDigest(t *testing.T, store *Store, streamID string, cutSeq uint64) string {
	t.Helper()
	rep, err := store.replayStream(streamID)
	if err != nil {
		t.Fatalf("replayStream(%s): %v", streamID, err)
	}
	dig, err := rep.st.CutDigest(int64(cutSeq))
	if err != nil {
		t.Fatalf("CutDigest(%s, %d): %v", streamID, cutSeq, err)
	}
	return dig
}

func mustRecordDigest(t *testing.T, recBytes []byte) string {
	t.Helper()
	rn, err := wire.Ingest(recBytes, wire.KindRecord)
	if err != nil {
		t.Fatalf("wire.Ingest: %v", err)
	}
	d, err := canon.Digest(wire.Encode(rn))
	if err != nil {
		t.Fatalf("canon.Digest: %v", err)
	}
	return d
}
