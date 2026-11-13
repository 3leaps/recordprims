package bucketcas_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/3leaps/recordprims/internal/journal"
	"github.com/3leaps/recordprims/internal/stratum"
	"github.com/3leaps/recordprims/internal/stratum/bucketcas"
)

func fixedClock(t0 time.Time) func() time.Time {
	cur := t0.UTC().Truncate(time.Microsecond)
	return func() time.Time {
		cur = cur.Add(time.Millisecond)
		return cur
	}
}

func makeRecordJSON(stream, recordID string, rev int, payload string) []byte {
	return []byte(fmt.Sprintf(
		`{"v":1,"stream":%q,"record_id":%q,"revision":%d,"payload_contract":"contract: notes/v1","payload":%s,"provenance":{"actor_kind":"process","actor":"test"},"classification":{"retention":"standard","sensitivity":"1-confidential"}}`,
		stream, recordID, rev, payload,
	))
}

func TestCapabilitiesRefusalNeverDegrades(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name string
		caps stratum.StoreCaps
	}{
		{
			name: "missing MustNotExist",
			caps: stratum.StoreCaps{ConditionalPutMustNotExist: false, ConditionalPutValidatorMatch: true, CoherentRead: true},
		},
		{
			name: "missing ValidatorMatch",
			caps: stratum.StoreCaps{ConditionalPutMustNotExist: true, ConditionalPutValidatorMatch: false, CoherentRead: true},
		},
		{
			name: "missing CoherentRead",
			caps: stratum.StoreCaps{ConditionalPutMustNotExist: true, ConditionalPutValidatorMatch: true, CoherentRead: false},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mem := bucketcas.NewMemStore()
			mem.SetCapabilities(tc.caps)
			_, err := bucketcas.Init(ctx, mem, bucketcas.Options{Prefix: "tenant-a"})
			if !errors.Is(err, bucketcas.ErrCapabilities) {
				t.Fatalf("Init(%s): got %v, want ErrCapabilities", tc.name, err)
			}
			_, err = bucketcas.Open(ctx, mem, bucketcas.Options{Prefix: "tenant-a"})
			if !errors.Is(err, bucketcas.ErrCapabilities) {
				t.Fatalf("Open(%s): got %v, want ErrCapabilities", tc.name, err)
			}
		})
	}
}

func TestInitOpenAppendAndReconcile(t *testing.T) {
	ctx := context.Background()
	mem := bucketcas.NewMemStore()
	nowFn := fixedClock(time.Date(2026, 3, 29, 12, 0, 0, 0, time.UTC))

	bs, err := bucketcas.Init(ctx, mem, bucketcas.Options{
		Prefix: "org/store1",
		Now:    nowFn,
	})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := bs.OpenStream(ctx, "notes", "revisioned"); err != nil {
		t.Fatalf("OpenStream: %v", err)
	}

	rcpt, err := bs.Append(ctx, bucketcas.AppendInput{
		Stream:         "notes",
		Record:         makeRecordJSON("notes", "n-1", 1, `{"title":"hello"}`),
		IdempotencyKey: "k-n1-v1",
	})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if rcpt.Outcome != "appended" || rcpt.Seq != 2 {
		t.Fatalf("Append receipt = %+v, want appended seq=2", rcpt)
	}

	// Re-open store from MemStore and verify state and Reconcile.
	bs2, err := bucketcas.Open(ctx, mem, bucketcas.Options{
		Prefix: "org/store1",
		Now:    nowFn,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	recNode, err := bs2.Reconcile(ctx, "notes", "k-n1-v1", "2026-03-29T12:00:00.000000Z")
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got := recNode.Get("result").Str; got != "found" {
		t.Fatalf("Reconcile result = %q, want found", got)
	}
}

func TestAppendContentionRetryAndRevisionConflict(t *testing.T) {
	ctx := context.Background()
	mem := bucketcas.NewMemStore()
	nowFn := fixedClock(time.Date(2026, 3, 29, 12, 0, 0, 0, time.UTC))

	bsA, err := bucketcas.Init(ctx, mem, bucketcas.Options{Prefix: "race", Now: nowFn})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := bsA.OpenStream(ctx, "events", "single_revision"); err != nil {
		t.Fatalf("OpenStream: %v", err)
	}
	bsB, err := bucketcas.Open(ctx, mem, bucketcas.Options{Prefix: "race", Now: nowFn})
	if err != nil {
		t.Fatalf("Open B: %v", err)
	}

	// Scenario A: Writer B commits "ev-b" while Writer A is staging "ev-a".
	// Writer A gets 412 ConflictTokenMismatch on its first PutIfMatch, reloads
	// head.json, ingests "ev-b", and commits "ev-a" at seq 3.
	injectedRace := false
	mem.SetPutIfMatchHook(func(key, expectedValidator string, body []byte) (bool, bool, stratum.CommitKnowledge, error) {
		if !injectedRace {
			injectedRace = true
			mem.SetPutIfMatchHook(nil)
			if _, err := bsB.Append(ctx, bucketcas.AppendInput{
				Stream:         "events",
				Record:         makeRecordJSON("events", "ev-b", 1, `{"from":"B"}`),
				IdempotencyKey: "key-b",
			}); err != nil {
				t.Errorf("concurrent Append B: %v", err)
			}
			// Now let MemStore evaluate Writer A's stale expectedValidator normally (which yields 412).
			return false, false, 0, nil
		}
		return false, false, 0, nil
	})

	rcptA, err := bsA.Append(ctx, bucketcas.AppendInput{
		Stream:         "events",
		Record:         makeRecordJSON("events", "ev-a", 1, `{"from":"A"}`),
		IdempotencyKey: "key-a",
	})
	if err != nil {
		t.Fatalf("Append A after 412 retry: %v", err)
	}
	if rcptA.Outcome != "appended" || rcptA.Seq != 3 {
		t.Fatalf("Append A receipt = %+v, want appended at seq=3", rcptA)
	}

	// Scenario B: Competing writers both write record_id "ev-same" rev 1.
	// Writer B lands first during Writer A's CAS window; Writer A reloads head.json
	// and deterministically returns Outcome == "revision_conflict".
	injectedConflictRace := false
	mem.SetPutIfMatchHook(func(key, expectedValidator string, body []byte) (bool, bool, stratum.CommitKnowledge, error) {
		if !injectedConflictRace {
			injectedConflictRace = true
			mem.SetPutIfMatchHook(nil)
			if _, err := bsB.Append(ctx, bucketcas.AppendInput{
				Stream:         "events",
				Record:         makeRecordJSON("events", "ev-same", 1, `{"winner":"B"}`),
				IdempotencyKey: "key-same-b",
			}); err != nil {
				t.Errorf("concurrent Append B (same record): %v", err)
			}
			return false, false, 0, nil
		}
		return false, false, 0, nil
	})

	rcptConflict, err := bsA.Append(ctx, bucketcas.AppendInput{
		Stream:         "events",
		Record:         makeRecordJSON("events", "ev-same", 1, `{"winner":"A"}`),
		IdempotencyKey: "key-same-a",
	})
	if err != nil {
		t.Fatalf("Append A competing revision: %v", err)
	}
	if rcptConflict.Outcome != "revision_conflict" {
		t.Fatalf("Append A competing revision outcome = %q, want revision_conflict", rcptConflict.Outcome)
	}
}

func TestAppendCommitUnknownRecoveryAndIndeterminateRetry(t *testing.T) {
	ctx := context.Background()
	mem := bucketcas.NewMemStore()
	nowFn := fixedClock(time.Date(2026, 3, 29, 12, 0, 0, 0, time.UTC))

	bs, err := bucketcas.Init(ctx, mem, bucketcas.Options{Prefix: "unknown", Now: nowFn})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := bs.OpenStream(ctx, "orders", "single_revision"); err != nil {
		t.Fatalf("OpenStream: %v", err)
	}

	// Case 1: PutIfMatch returns CommitUnknown, but the write DID land on the backend.
	// Append must immediately probe GetCoherent("head.json"), observe its live key,
	// and return the committed receipt without error.
	mem.SetPutIfMatchHook(func(key, expectedValidator string, body []byte) (bool, bool, stratum.CommitKnowledge, error) {
		mem.SetPutIfMatchHook(nil)
		return true, true, stratum.CommitUnknown, errors.New("connection reset after backend commit")
	})

	rcpt1, err := bs.Append(ctx, bucketcas.AppendInput{
		Stream:         "orders",
		Record:         makeRecordJSON("orders", "ord-1", 1, `{"item":1}`),
		IdempotencyKey: "key-ord-1",
	})
	if err != nil {
		t.Fatalf("Append (CommitUnknown after commit): unexpected error %v", err)
	}
	if rcpt1.Outcome != "appended" || rcpt1.Seq != 2 {
		t.Fatalf("Append (CommitUnknown after commit) receipt = %+v, want appended seq=2", rcpt1)
	}

	// Case 2: PutIfMatch returns CommitUnknown and the write did NOT land yet.
	// Append must surface *stratum.IndeterminateError; caller retry with the same
	// idempotency key then commits cleanly.
	mem.SetPutIfMatchHook(func(key, expectedValidator string, body []byte) (bool, bool, stratum.CommitKnowledge, error) {
		mem.SetPutIfMatchHook(nil)
		return true, false, stratum.CommitUnknown, errors.New("timeout before backend commit")
	})

	_, err = bs.Append(ctx, bucketcas.AppendInput{
		Stream:         "orders",
		Record:         makeRecordJSON("orders", "ord-2", 1, `{"item":2}`),
		IdempotencyKey: "key-ord-2",
	})
	var indErr *stratum.IndeterminateError
	if !errors.As(err, &indErr) {
		t.Fatalf("Append (CommitUnknown without commit): got %v, want *stratum.IndeterminateError", err)
	}
	if indErr.Key != "key-ord-2" {
		t.Fatalf("IndeterminateError.Key = %q, want key-ord-2", indErr.Key)
	}

	// Caller retries with the same IdempotencyKey and converges idempotently.
	rcpt2, err := bs.Append(ctx, bucketcas.AppendInput{
		Stream:         "orders",
		Record:         makeRecordJSON("orders", "ord-2", 1, `{"item":2}`),
		IdempotencyKey: "key-ord-2",
	})
	if err != nil {
		t.Fatalf("Append retry after IndeterminateError: %v", err)
	}
	if rcpt2.Outcome != "appended" || rcpt2.Seq != 3 {
		t.Fatalf("Append retry receipt = %+v, want appended seq=3", rcpt2)
	}
}

func TestCheckpointCompactionAndColdStartO1Reads(t *testing.T) {
	ctx := context.Background()
	mem := bucketcas.NewMemStore()
	nowFn := fixedClock(time.Date(2026, 3, 29, 12, 0, 0, 0, time.UTC))

	bs, err := bucketcas.Init(ctx, mem, bucketcas.Options{
		Prefix:                    "ckpt",
		Now:                       nowFn,
		CheckpointIntervalEntries: 4,
	})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := bs.OpenStream(ctx, "metrics", "single_revision"); err != nil {
		t.Fatalf("OpenStream: %v", err)
	}

	// Append 9 records (total seq = 10: 1 stream_opened + 9 appended).
	// With CheckpointIntervalEntries=4, checkpoints happen at seq 4 and seq 8,
	// leaving only seq 9 and seq 10 in SegmentsTail!
	for i := 1; i <= 9; i++ {
		rid := fmt.Sprintf("m-%02d", i)
		_, err := bs.Append(ctx, bucketcas.AppendInput{
			Stream:         "metrics",
			Record:         makeRecordJSON("metrics", rid, 1, fmt.Sprintf(`{"i":%d}`, i)),
			IdempotencyKey: "key-" + rid,
		})
		if err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}

	// Reset counters and perform a cold-start load of "metrics".
	mem.ResetCounters()
	st, head, err := bs.State(ctx, "metrics")
	if err != nil {
		t.Fatalf("State(metrics): %v", err)
	}
	if head.CutSeq != 10 {
		t.Fatalf("head.CutSeq = %d, want 10", head.CutSeq)
	}
	if head.LatestCheckpoint == nil || head.LatestCheckpoint.CutSeq != 8 {
		t.Fatalf("head.LatestCheckpoint = %+v, want CutSeq=8", head.LatestCheckpoint)
	}
	if len(head.SegmentsTail) != 2 {
		t.Fatalf("len(head.SegmentsTail) = %d, want 2", len(head.SegmentsTail))
	}
	if gotSegGets := mem.SegmentGetCount(); gotSegGets != 2 {
		t.Fatalf("cold-start SegmentGetCount = %d, want 2 (historical segments 1..8 must not be fetched)", gotSegGets)
	}
	if st.Len() != 10 {
		t.Fatalf("replayed State.Len() = %d, want 10", st.Len())
	}
}

func TestPutBodyAndBodyStateOnBucketCAS(t *testing.T) {
	ctx := context.Background()
	mem := bucketcas.NewMemStore()
	nowFn := fixedClock(time.Date(2026, 3, 29, 12, 0, 0, 0, time.UTC))

	bs, err := bucketcas.Init(ctx, mem, bucketcas.Options{Prefix: "bodies", Now: nowFn})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}

	payload := []byte("external body content on object storage")
	dig, length, inc, err := bs.PutBody(ctx, payload)
	if err != nil {
		t.Fatalf("PutBody: %v", err)
	}
	if inc != 1 || length != int64(len(payload)) {
		t.Fatalf("PutBody = (%s, %d, %d), want inc=1 len=%d", dig, length, inc, len(payload))
	}

	// Idempotent repeat of PutBody returns incarnation 1 and state "available".
	dig2, length2, inc2, err := bs.PutBody(ctx, payload)
	if err != nil {
		t.Fatalf("PutBody repeat: %v", err)
	}
	if dig2 != dig || length2 != length || inc2 != 1 {
		t.Fatalf("PutBody repeat = (%s, %d, %d), want (%s, %d, 1)", dig2, length2, inc2, dig, length)
	}

	st, err := bs.BodyState(ctx, dig, length)
	if err != nil {
		t.Fatalf("BodyState: %v", err)
	}
	if st != "available" {
		t.Fatalf("BodyState = %q, want available", st)
	}
}

func TestAppendVerifiesExternalBodyIsAvailableAndOpenRequiresDesignatedStream(t *testing.T) {
	ctx := context.Background()
	mem := bucketcas.NewMemStore()
	bs, err := bucketcas.Init(ctx, mem, bucketcas.Options{Prefix: "verify"})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := bs.OpenStream(ctx, "notes", "single_revision"); err != nil {
		t.Fatalf("OpenStream: %v", err)
	}

	// 1. Append referencing an unpublished body must fail before writing body_status: "verified".
	missingDig := "sha256:" + strings.Repeat("a", 64)
	recMissing := []byte(fmt.Sprintf(
		`{"v":1,"stream":"notes","record_id":"r1","revision":1,"payload_contract":"contract: notes/v1","body":{"digest":%q,"length":3,"media_type":"text/plain"},"provenance":{"actor_kind":"process","actor":"test"},"classification":{"retention":"standard","sensitivity":"1-confidential"}}`,
		missingDig,
	))
	if _, err := bs.Append(ctx, bucketcas.AppendInput{
		Stream:         "notes",
		Record:         recMissing,
		IdempotencyKey: "k-missing",
	}); !errors.Is(err, bucketcas.ErrInvalidStore) {
		t.Fatalf("Append with unpublished body: got err=%v, want ErrInvalidStore", err)
	}

	// 2. After PutBody publishes the body, Append succeeds and marks body_status: "verified".
	dig, length, _, err := bs.PutBody(ctx, []byte("abc"))
	if err != nil {
		t.Fatalf("PutBody: %v", err)
	}
	recAvail := []byte(fmt.Sprintf(
		`{"v":1,"stream":"notes","record_id":"r1","revision":1,"payload_contract":"contract: notes/v1","body":{"digest":%q,"length":%d,"media_type":"text/plain"},"provenance":{"actor_kind":"process","actor":"test"},"classification":{"retention":"standard","sensitivity":"1-confidential"}}`,
		dig, length,
	))
	rcpt, err := bs.Append(ctx, bucketcas.AppendInput{
		Stream:         "notes",
		Record:         recAvail,
		IdempotencyKey: "k-avail",
	})
	if err != nil || rcpt.Outcome != "appended" {
		t.Fatalf("Append with available body: rcpt=%+v err=%v", rcpt, err)
	}

	// 3. Deleting the designated stream's head.json causes Open to fail with ErrInvalidStore.
	if _, err := mem.Delete(ctx, "verify/streams/body-removal/head.json"); err != nil {
		t.Fatalf("Delete head.json: %v", err)
	}
	if _, err := bucketcas.Open(ctx, mem, bucketcas.Options{Prefix: "verify"}); !errors.Is(err, bucketcas.ErrInvalidStore) {
		t.Fatalf("Open with missing designated stream head.json: got err=%v, want ErrInvalidStore", err)
	}
}

func TestUnknownInitIsIndeterminate(t *testing.T) {
	ctx := context.Background()
	for _, target := range []string{"profile.json", "STORE.json"} {
		t.Run(target, func(t *testing.T) {
			mem := bucketcas.NewMemStore()
			mem.SetPutIfAbsentHook(func(k string, _ []byte) (bool, bool, stratum.CommitKnowledge, error) {
				if strings.HasSuffix(k, target) {
					return true, true, stratum.CommitUnknown, errors.New("lost reply")
				}
				return false, false, 0, nil
			})
			_, err := bucketcas.Init(ctx, mem, bucketcas.Options{Prefix: "test"})
			var ind *stratum.IndeterminateError
			if !errors.As(err, &ind) {
				t.Fatalf("CommitUnknown on %s returned %T (%v), want *stratum.IndeterminateError", target, err, err)
			}
		})
	}
}

func TestBucketCASSealAndCompactAndReclaim(t *testing.T) {
	ctx := context.Background()
	mem := bucketcas.NewMemStore()
	nowFn := fixedClock(time.Date(2026, 3, 29, 12, 0, 0, 0, time.UTC))

	bs, err := bucketcas.Init(ctx, mem, bucketcas.Options{
		Prefix: "compact",
		Now:    nowFn,
	})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := bs.OpenStream(ctx, "events-v1", "single_revision"); err != nil {
		t.Fatalf("OpenStream events-v1: %v", err)
	}

	r1, err := bs.Append(ctx, bucketcas.AppendInput{
		Stream:         "events-v1",
		Record:         makeRecordJSON("events-v1", "ev-1", 1, `{"step":1}`),
		IdempotencyKey: "k-ev-1",
	})
	if err != nil || r1.Outcome != "appended" || r1.Digest == "" {
		t.Fatalf("Append ev-1: %+v, err=%v", r1, err)
	}

	out, err := bs.SealAndCompactStream(ctx, bucketcas.CompactionInput{
		SourceStream:      "events-v1",
		DestinationStream: "events-v2",
		Mode:              "single_revision",
		Records: []bucketcas.CompactRecordInput{
			{
				Record: makeRecordJSON("events-v2", "ev-1", 1, `{"step":1}`),
				SourcePin: journal.Pin{
					Stream:   "events-v1",
					RecordID: "ev-1",
					Revision: 1,
					Digest:   r1.Digest,
				},
				IdempotencyKey: "k-imp-ev-1",
			},
		},
		ActorKind:          "human",
		Actor:              "alice@example.com",
		AuthRef:            "auth:cas-compact-1",
		AuthDigest:         "sha256:" + strings.Repeat("3", 64),
		ReclaimPredecessor: true,
	})
	if err != nil {
		t.Fatalf("SealAndCompactStream: %v", err)
	}
	if out.ReclaimedSegmentCount == 0 || out.ReclaimedSegmentBytes <= 0 {
		t.Fatalf("expected predecessor segments to be reclaimed, got %+v", out)
	}

	// Verify sealed source stream refuses writes with ErrStreamSealed.
	if _, err := bs.Append(ctx, bucketcas.AppendInput{
		Stream:         "events-v1",
		Record:         makeRecordJSON("events-v1", "ev-2", 1, `{"step":2}`),
		IdempotencyKey: "k-ev-2",
	}); !errors.Is(err, bucketcas.ErrStreamSealed) {
		t.Fatalf("expected ErrStreamSealed on sealed events-v1, got %v", err)
	}

	// Verify destination stream loads cleanly even after events-v1's segments were deleted!
	dstSt, dstHead, err := bs.State(ctx, "events-v2")
	if err != nil {
		t.Fatalf("State(events-v2) after predecessor reclamation: %v", err)
	}
	if dstHead.CutSeq != 3 || dstSt.ReviewPending() {
		t.Fatalf("unexpected events-v2 state: head=%+v, reviewPending=%v", dstHead, dstSt.ReviewPending())
	}
}

func TestBucketCASMultiHopCompactionRetryAndRefusals(t *testing.T) {
	ctx := context.Background()
	mem := bucketcas.NewMemStore()
	nowFn := fixedClock(time.Date(2026, 3, 29, 12, 0, 0, 0, time.UTC))

	bs, err := bucketcas.Init(ctx, mem, bucketcas.Options{
		Prefix: "multihop",
		Now:    nowFn,
	})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := bs.OpenStream(ctx, "s-a", "single_revision"); err != nil {
		t.Fatalf("OpenStream s-a: %v", err)
	}
	rA, err := bs.Append(ctx, bucketcas.AppendInput{
		Stream:         "s-a",
		Record:         makeRecordJSON("s-a", "rec-1", 1, `{"hop":1}`),
		IdempotencyKey: "k-a-1",
	})
	if err != nil {
		t.Fatalf("Append s-a: %v", err)
	}

	// 1. Simulate crash after stream-seal.json is written on s-a, before s-a head.json is updated.
	// Next retry (with advanced clock) must reuse the persisted stream-seal.json and its digest.
	failFirstHeadCAS := true
	mem.SetPutIfMatchHook(func(key, expectedValidator string, body []byte) (bool, bool, stratum.CommitKnowledge, error) {
		if failFirstHeadCAS && strings.HasSuffix(key, "streams/s-a/head.json") {
			failFirstHeadCAS = false
			mem.SetPutIfMatchHook(nil)
			return true, false, stratum.CommitNotCommitted, errors.New("injected head CAS failure after stream-seal.json")
		}
		return false, false, 0, nil
	})

	compactAB := bucketcas.CompactionInput{
		SourceStream:      "s-a",
		DestinationStream: "s-b",
		Mode:              "single_revision",
		Records: []bucketcas.CompactRecordInput{
			{
				Record: makeRecordJSON("s-b", "rec-1", 1, `{"hop":1}`),
				SourcePin: journal.Pin{
					Stream:   "s-a",
					RecordID: "rec-1",
					Revision: 1,
					Digest:   rA.Digest,
				},
				IdempotencyKey: "k-ab-1",
			},
		},
		ActorKind:          "human",
		Actor:              "alice@example.com",
		AuthRef:            "auth:ab",
		AuthDigest:         "sha256:" + strings.Repeat("4", 64),
		ReclaimPredecessor: true,
	}
	if _, err := bs.SealAndCompactStream(ctx, compactAB); err == nil {
		t.Fatal("expected injected head CAS failure on first SealAndCompactStream(s-a -> s-b)")
	}

	// Retry s-a -> s-b; must succeed and reuse existing stream-seal.json.
	outAB, err := bs.SealAndCompactStream(ctx, compactAB)
	if err != nil {
		t.Fatalf("retry SealAndCompactStream(s-a -> s-b): %v", err)
	}
	if outAB.SealDigest == "" {
		t.Fatalf("expected non-empty SealDigest on s-a -> s-b")
	}

	// Retry s-a -> s-b AFTER s-a has been reclaimed; must succeed idempotently!
	outABRetry, err := bs.SealAndCompactStream(ctx, compactAB)
	if err != nil {
		t.Fatalf("idempotent retry after reclamation (s-a -> s-b): %v", err)
	}
	if outABRetry.SealDigest != outAB.SealDigest || outABRetry.DestinationCutSeq != outAB.DestinationCutSeq {
		t.Fatalf("idempotent retry mismatch: got %+v, want %+v", outABRetry, outAB)
	}

	// 2. Multi-hop compaction s-b -> s-c (where s-a is already reclaimed).
	// Determine imported record digest on s-b via State(s-b).
	stB, _, err := bs.State(ctx, "s-b")
	if err != nil {
		t.Fatalf("State(s-b): %v", err)
	}
	var recBDigest string
	for pin := range stB.Records() {
		if pin.RecordID == "rec-1" {
			recBDigest = pin.Digest
		}
	}
	if recBDigest == "" {
		t.Fatal("rec-1 not found in s-b")
	}

	outBC, err := bs.SealAndCompactStream(ctx, bucketcas.CompactionInput{
		SourceStream:      "s-b",
		DestinationStream: "s-c",
		Mode:              "single_revision",
		Records: []bucketcas.CompactRecordInput{
			{
				Record: makeRecordJSON("s-c", "rec-1", 1, `{"hop":1}`),
				SourcePin: journal.Pin{
					Stream:   "s-b",
					RecordID: "rec-1",
					Revision: 1,
					Digest:   recBDigest,
				},
				IdempotencyKey: "k-bc-1",
			},
		},
		ActorKind:          "human",
		Actor:              "alice@example.com",
		AuthRef:            "auth:bc",
		AuthDigest:         "sha256:" + strings.Repeat("5", 64),
		ReclaimPredecessor: true,
	})
	if err != nil {
		t.Fatalf("SealAndCompactStream(s-b -> s-c): %v", err)
	}
	if outBC.SealDigest == "" {
		t.Fatal("expected non-empty SealDigest on s-b -> s-c")
	}

	// Verify s-c loads cleanly when both s-a and s-b have been reclaimed.
	if _, _, err := bs.State(ctx, "s-c"); err != nil {
		t.Fatalf("State(s-c) after multi-hop reclamation: %v", err)
	}

	// Tamper with s-a's stream-seal.json in object storage and verify State(s-c) fails closed!
	sealKeyA := "multihop/streams/s-a/stream-seal.json"
	origSealA, _, _, err := mem.GetCoherent(ctx, sealKeyA)
	if err != nil {
		t.Fatalf("GetCoherent(%s): %v", sealKeyA, err)
	}
	if _, err := mem.Delete(ctx, sealKeyA); err != nil {
		t.Fatalf("Delete(%s): %v", sealKeyA, err)
	}
	if _, _, _, err := mem.PutIfAbsent(ctx, sealKeyA, []byte(strings.Replace(string(origSealA), `"cut_seq":2`, `"cut_seq":99`, 1))); err != nil {
		t.Fatalf("PutIfAbsent tampered sealKeyA: %v", err)
	}
	if _, _, err := bs.State(ctx, "s-c"); !errors.Is(err, bucketcas.ErrInvalidStore) {
		t.Fatalf("expected ErrInvalidStore on State(s-c) when transitive ancestor s-a seal is tampered, got %v", err)
	}
	// Restore s-a's stream-seal.json.
	_, _ = mem.Delete(ctx, sealKeyA)
	_, _, _, _ = mem.PutIfAbsent(ctx, sealKeyA, origSealA)

	// 3. Unrelated existing destination stream must be refused.
	if err := bs.OpenStream(ctx, "unrelated-dst", "single_revision"); err != nil {
		t.Fatalf("OpenStream unrelated-dst: %v", err)
	}
	if _, err := bs.SealAndCompactStream(ctx, bucketcas.CompactionInput{
		SourceStream:      "s-c",
		DestinationStream: "unrelated-dst",
		Mode:              "single_revision",
		ActorKind:         "human",
		Actor:             "alice@example.com",
		AuthRef:           "auth:bad-dst",
		AuthDigest:        "sha256:" + strings.Repeat("6", 64),
	}); !errors.Is(err, bucketcas.ErrInvalidStore) {
		t.Fatalf("expected ErrInvalidStore when compacting into unrelated existing destination stream, got %v", err)
	}

	// 4. Compacting a record with an unpublished external body must be refused.
	if err := bs.OpenStream(ctx, "s-body-src", "single_revision"); err != nil {
		t.Fatalf("OpenStream s-body-src: %v", err)
	}
	rBodySrc, err := bs.Append(ctx, bucketcas.AppendInput{
		Stream:         "s-body-src",
		Record:         makeRecordJSON("s-body-src", "b-1", 1, `{"inline":true}`),
		IdempotencyKey: "k-b-1",
	})
	if err != nil {
		t.Fatalf("Append s-body-src: %v", err)
	}
	missingBodyDig := "sha256:" + strings.Repeat("e", 64)
	recWithMissingBody := []byte(fmt.Sprintf(
		`{"v":1,"stream":"s-body-dst","record_id":"b-1","revision":1,"payload_contract":"contract: notes/v1","body":{"digest":%q,"length":10,"media_type":"text/plain"},"provenance":{"actor_kind":"process","actor":"test"},"classification":{"retention":"standard","sensitivity":"1-confidential"}}`,
		missingBodyDig,
	))
	if _, err := bs.SealAndCompactStream(ctx, bucketcas.CompactionInput{
		SourceStream:      "s-body-src",
		DestinationStream: "s-body-dst",
		Mode:              "single_revision",
		Records: []bucketcas.CompactRecordInput{
			{
				Record: recWithMissingBody,
				SourcePin: journal.Pin{
					Stream:   "s-body-src",
					RecordID: "b-1",
					Revision: 1,
					Digest:   rBodySrc.Digest,
				},
				IdempotencyKey: "k-imp-b-1",
			},
		},
		ActorKind:  "human",
		Actor:      "alice@example.com",
		AuthRef:    "auth:body",
		AuthDigest: "sha256:" + strings.Repeat("7", 64),
	}); !errors.Is(err, bucketcas.ErrInvalidStore) {
		t.Fatalf("expected ErrInvalidStore when compacting record with unpublished external body, got %v", err)
	}

	// 5. Dropping an external body reference of an unretired source record must be refused.
	bodyBytes := []byte("bucket-external-body-must-be-preserved")
	bDig, bLen, _, err := bs.PutBody(ctx, bodyBytes)
	if err != nil {
		t.Fatalf("PutBody: %v", err)
	}
	if err := bs.OpenStream(ctx, "s-unpres-src", "single_revision"); err != nil {
		t.Fatalf("OpenStream s-unpres-src: %v", err)
	}
	recWithBodySrc := []byte(fmt.Sprintf(
		`{"v":1,"stream":"s-unpres-src","record_id":"ub-1","revision":1,"payload_contract":"contract: notes/v1","body":{"digest":%q,"length":%d,"media_type":"text/plain"},"provenance":{"actor_kind":"process","actor":"test"},"classification":{"retention":"standard","sensitivity":"1-confidential"}}`,
		bDig, bLen,
	))
	rUnpres, err := bs.Append(ctx, bucketcas.AppendInput{
		Stream:         "s-unpres-src",
		Record:         recWithBodySrc,
		IdempotencyKey: "k-ub-1",
	})
	if err != nil {
		t.Fatalf("Append s-unpres-src: %v", err)
	}
	if _, err := bs.SealAndCompactStream(ctx, bucketcas.CompactionInput{
		SourceStream:      "s-unpres-src",
		DestinationStream: "s-unpres-dst",
		Mode:              "single_revision",
		Records: []bucketcas.CompactRecordInput{
			{
				Record: makeRecordJSON("s-unpres-dst", "ub-1", 1, `{"dropped_body":true}`),
				SourcePin: journal.Pin{
					Stream:   "s-unpres-src",
					RecordID: "ub-1",
					Revision: 1,
					Digest:   rUnpres.Digest,
				},
				IdempotencyKey: "k-imp-ub-1",
			},
		},
		ActorKind:  "human",
		Actor:      "alice@example.com",
		AuthRef:    "auth:unpres",
		AuthDigest: "sha256:" + strings.Repeat("8", 64),
	}); !errors.Is(err, bucketcas.ErrInvalidStore) {
		t.Fatalf("expected ErrInvalidStore (body_unpreserved) when unretired source body is omitted in compacted record, got %v", err)
	}

	// 6. Malformed record retry against already-reclaimed s-a must return ErrInvalidStore cleanly (never panic).
	if _, err := bs.SealAndCompactStream(ctx, bucketcas.CompactionInput{
		SourceStream:      "s-a",
		DestinationStream: "s-b",
		Mode:              "single_revision",
		Records: []bucketcas.CompactRecordInput{
			{
				Record: []byte(`{}`),
				SourcePin: journal.Pin{
					Stream:   "s-a",
					RecordID: "rec-1",
					Revision: 1,
					Digest:   rA.Digest,
				},
				IdempotencyKey: "k-malformed",
			},
		},
		ActorKind:  "human",
		Actor:      "alice@example.com",
		AuthRef:    "auth:mal",
		AuthDigest: "sha256:" + strings.Repeat("9", 64),
	}); !errors.Is(err, bucketcas.ErrInvalidStore) {
		t.Fatalf("expected ErrInvalidStore on malformed record retry against reclaimed source, got %v", err)
	}

	// 7. Concurrent Append during SealAndCompactStream source-head CAS must recover cleanly by
	// cleaning up the uncommitted stream-seal.json and retrying (or failing with unretired record dropped).
	if err := bs.OpenStream(ctx, "s-race-src", "single_revision"); err != nil {
		t.Fatalf("OpenStream s-race-src: %v", err)
	}
	rRace1, err := bs.Append(ctx, bucketcas.AppendInput{
		Stream:         "s-race-src",
		Record:         makeRecordJSON("s-race-src", "race-1", 1, `{"v":1}`),
		IdempotencyKey: "k-race-1",
	})
	if err != nil {
		t.Fatalf("Append race-1: %v", err)
	}
	injectedRace := false
	mem.SetPutIfMatchHook(func(key, _ string, _ []byte) (bool, bool, stratum.CommitKnowledge, error) {
		if key == "multihop/streams/s-race-src/head.json" && !injectedRace {
			injectedRace = true
			mem.SetPutIfMatchHook(nil)
			_, aerr := bs.Append(ctx, bucketcas.AppendInput{
				Stream:         "s-race-src",
				Record:         makeRecordJSON("s-race-src", "race-2", 1, `{"v":2}`),
				IdempotencyKey: "k-race-2",
			})
			if aerr != nil {
				t.Errorf("concurrent Append race-2 failed: %v", aerr)
			}
		}
		return false, false, 0, nil
	})
	defer mem.SetPutIfMatchHook(nil)

	outRace, err := bs.SealAndCompactStream(ctx, bucketcas.CompactionInput{
		SourceStream:       "s-race-src",
		DestinationStream:  "s-race-dst",
		Mode:               "single_revision",
		AllowDropUnretired: true,
		Records: []bucketcas.CompactRecordInput{
			{
				Record: makeRecordJSON("s-race-dst", "race-1", 1, `{"v":1}`),
				SourcePin: journal.Pin{
					Stream:   "s-race-src",
					RecordID: "race-1",
					Revision: 1,
					Digest:   rRace1.Digest,
				},
				IdempotencyKey: "k-imp-race-1",
			},
		},
		ActorKind:          "human",
		Actor:              "alice@example.com",
		AuthRef:            "auth:race",
		AuthDigest:         "sha256:" + strings.Repeat("a", 64),
		ReclaimPredecessor: true,
	})
	if err != nil {
		t.Fatalf("SealAndCompactStream after concurrent Append race: %v", err)
	}
	if outRace.SealedCutSeq != 3 {
		t.Fatalf("expected SealedCutSeq=3 (including concurrent Append), got %d", outRace.SealedCutSeq)
	}

	// 8. Competing compactors A and B racing on source head.json CAS: loser A must NOT delete
	// winner B's committed stream-seal.json, and both must converge cleanly!
	if err := bs.OpenStream(ctx, "s-comp-src", "single_revision"); err != nil {
		t.Fatalf("OpenStream s-comp-src: %v", err)
	}
	rComp, err := bs.Append(ctx, bucketcas.AppendInput{
		Stream:         "s-comp-src",
		Record:         makeRecordJSON("s-comp-src", "c-1", 1, `{"v":1}`),
		IdempotencyKey: "k-comp-1",
	})
	if err != nil {
		t.Fatalf("Append c-1: %v", err)
	}
	compReq := bucketcas.CompactionInput{
		SourceStream:      "s-comp-src",
		DestinationStream: "s-comp-dst",
		Mode:              "single_revision",
		Records: []bucketcas.CompactRecordInput{
			{
				Record: makeRecordJSON("s-comp-dst", "c-1", 1, `{"v":1}`),
				SourcePin: journal.Pin{
					Stream:   "s-comp-src",
					RecordID: "c-1",
					Revision: 1,
					Digest:   rComp.Digest,
				},
				IdempotencyKey: "k-imp-comp-1",
			},
		},
		ActorKind:          "human",
		Actor:              "alice@example.com",
		AuthRef:            "auth:comp",
		AuthDigest:         "sha256:" + strings.Repeat("b", 64),
		ReclaimPredecessor: true,
	}
	competingInjected := false
	mem.SetPutIfMatchHook(func(key, _ string, _ []byte) (bool, bool, stratum.CommitKnowledge, error) {
		if key == "multihop/streams/s-comp-src/head.json" && !competingInjected {
			competingInjected = true
			mem.SetPutIfMatchHook(nil)
			// Compactor B runs to completion while Compactor A is paused right before source head CAS!
			if _, berr := bs.SealAndCompactStream(ctx, compReq); berr != nil {
				t.Errorf("competing compactor B failed: %v", berr)
			}
		}
		return false, false, 0, nil
	})
	outCompA, err := bs.SealAndCompactStream(ctx, compReq)
	if err != nil {
		t.Fatalf("competing compactor A failed after B won: %v", err)
	}
	if outCompA.SealDigest == "" {
		t.Fatal("expected non-empty SealDigest from compactor A")
	}
	if _, _, err := bs.State(ctx, "s-comp-dst"); err != nil {
		t.Fatalf("State(s-comp-dst) after competing compactors A & B: %v", err)
	}

	// 9. With MaxCASRetries=1, a concurrent Append during source head CAS must return ErrCASExhausted
	// and leave the source stream unsealed without creating the destination stream.
	bs1, err := bucketcas.Open(ctx, mem, bucketcas.Options{
		Prefix:        "multihop",
		Now:           nowFn,
		MaxCASRetries: 1,
	})
	if err != nil {
		t.Fatalf("Open bs1: %v", err)
	}
	if err := bs1.OpenStream(ctx, "s-exh-src", "single_revision"); err != nil {
		t.Fatalf("OpenStream s-exh-src: %v", err)
	}
	rExh, err := bs1.Append(ctx, bucketcas.AppendInput{
		Stream:         "s-exh-src",
		Record:         makeRecordJSON("s-exh-src", "e-1", 1, `{"v":1}`),
		IdempotencyKey: "k-exh-1",
	})
	if err != nil {
		t.Fatalf("Append e-1: %v", err)
	}
	exhInjected := false
	mem.SetPutIfMatchHook(func(key, _ string, _ []byte) (bool, bool, stratum.CommitKnowledge, error) {
		if key == "multihop/streams/s-exh-src/head.json" && !exhInjected {
			exhInjected = true
			mem.SetPutIfMatchHook(nil)
			if _, aerr := bs1.Append(ctx, bucketcas.AppendInput{
				Stream:         "s-exh-src",
				Record:         makeRecordJSON("s-exh-src", "e-2", 1, `{"v":2}`),
				IdempotencyKey: "k-exh-2",
			}); aerr != nil {
				t.Errorf("concurrent Append e-2 failed: %v", aerr)
			}
		}
		return false, false, 0, nil
	})
	if _, err := bs1.SealAndCompactStream(ctx, bucketcas.CompactionInput{
		SourceStream:      "s-exh-src",
		DestinationStream: "s-exh-dst",
		Mode:              "single_revision",
		Records: []bucketcas.CompactRecordInput{
			{
				Record: makeRecordJSON("s-exh-dst", "e-1", 1, `{"v":1}`),
				SourcePin: journal.Pin{
					Stream:   "s-exh-src",
					RecordID: "e-1",
					Revision: 1,
					Digest:   rExh.Digest,
				},
				IdempotencyKey: "k-imp-exh-1",
			},
		},
		ActorKind:  "human",
		Actor:      "alice@example.com",
		AuthRef:    "auth:exh",
		AuthDigest: "sha256:" + strings.Repeat("c", 64),
	}); !errors.Is(err, bucketcas.ErrCASExhausted) {
		t.Fatalf("expected ErrCASExhausted when MaxCASRetries=1 is beaten by concurrent Append, got %v", err)
	}
	_, exhHead, err := bs1.State(ctx, "s-exh-src")
	if err != nil || exhHead.Sealed {
		t.Fatalf("expected s-exh-src to remain unsealed after CAS exhaustion, head=%+v, err=%v", exhHead, err)
	}

	// 10. Tampering with reclaimed.json to inject an out-of-prefix ReclaimedKeys entry must fail closed
	// on ReclaimSealedAncestor retry and never delete the out-of-prefix object.
	sentinelKey := "other-store/STORE.json"
	if _, _, _, err := mem.PutIfAbsent(ctx, sentinelKey, []byte(`{"sentinel":true}`)); err != nil {
		t.Fatalf("PutIfAbsent sentinelKey: %v", err)
	}
	reclKeyA := "multihop/streams/s-a/reclaimed.json"
	origReclA, _, _, err := mem.GetCoherent(ctx, reclKeyA)
	if err != nil {
		t.Fatalf("GetCoherent(%s): %v", reclKeyA, err)
	}
	_, _ = mem.Delete(ctx, reclKeyA)
	tamperedReclA := strings.Replace(string(origReclA), `"reclaimed_keys":[`, fmt.Sprintf(`"reclaimed_keys":[%q,`, sentinelKey), 1)
	if _, _, _, err := mem.PutIfAbsent(ctx, reclKeyA, []byte(tamperedReclA)); err != nil {
		t.Fatalf("PutIfAbsent tamperedReclA: %v", err)
	}
	if _, _, err := bs.ReclaimSealedAncestor(ctx, "s-a", "s-b"); !errors.Is(err, bucketcas.ErrInvalidStore) {
		t.Fatalf("expected ErrInvalidStore on tampered reclaimed.json during ReclaimSealedAncestor retry, got %v", err)
	}
	if _, _, _, err := mem.GetCoherent(ctx, sentinelKey); err != nil {
		t.Fatalf("sentinel object %q was deleted or unreadable: %v", sentinelKey, err)
	}
	_, _ = mem.Delete(ctx, reclKeyA)
	_, _, _, _ = mem.PutIfAbsent(ctx, reclKeyA, origReclA)

	// 11. Interrupted seal from cut 2 left in place when Append advances head.json to cut 3:
	// validator-fenced PutIfMatch on stream-seal.json replaces the obsolete cut-2 seal without
	// using unconditional Delete.
	if err := bs.OpenStream(ctx, "s-stale-seal-src", "single_revision"); err != nil {
		t.Fatalf("OpenStream s-stale-seal-src: %v", err)
	}
	rStale1, err := bs.Append(ctx, bucketcas.AppendInput{
		Stream:         "s-stale-seal-src",
		Record:         makeRecordJSON("s-stale-seal-src", "st-1", 1, `{"v":1}`),
		IdempotencyKey: "k-stale-1",
	})
	if err != nil {
		t.Fatalf("Append st-1: %v", err)
	}
	// Simulate an interrupted compaction at cut 2 that wrote stream-seal.json and failed on head.json CAS,
	// followed by an Append advancing s-stale-seal-src to cut 3.
	abortCut2 := false
	mem.SetPutIfMatchHook(func(key, _ string, _ []byte) (bool, bool, stratum.CommitKnowledge, error) {
		if key == "multihop/streams/s-stale-seal-src/head.json" && !abortCut2 {
			abortCut2 = true
			mem.SetPutIfMatchHook(nil)
			return true, false, stratum.CommitNotCommitted, errors.New("simulated crash before head CAS at cut 2")
		}
		return false, false, 0, nil
	})
	_, _ = bs.SealAndCompactStream(ctx, bucketcas.CompactionInput{
		SourceStream:      "s-stale-seal-src",
		DestinationStream: "s-stale-seal-dst",
		Mode:              "single_revision",
		Records: []bucketcas.CompactRecordInput{
			{
				Record: makeRecordJSON("s-stale-seal-dst", "st-1", 1, `{"v":1}`),
				SourcePin: journal.Pin{
					Stream:   "s-stale-seal-src",
					RecordID: "st-1",
					Revision: 1,
					Digest:   rStale1.Digest,
				},
				IdempotencyKey: "k-stale-imp-1",
			},
		},
		ActorKind:  "human",
		Actor:      "alice@example.com",
		AuthRef:    "auth:stale",
		AuthDigest: "sha256:" + strings.Repeat("d", 64),
	})
	if _, err := bs.Append(ctx, bucketcas.AppendInput{
		Stream:         "s-stale-seal-src",
		Record:         makeRecordJSON("s-stale-seal-src", "st-2", 1, `{"v":2}`),
		IdempotencyKey: "k-stale-2",
	}); err != nil {
		t.Fatalf("Append st-2 after aborted cut-2 seal: %v", err)
	}
	outStale, err := bs.SealAndCompactStream(ctx, bucketcas.CompactionInput{
		SourceStream:       "s-stale-seal-src",
		DestinationStream:  "s-stale-seal-dst",
		Mode:               "single_revision",
		AllowDropUnretired: true,
		Records: []bucketcas.CompactRecordInput{
			{
				Record: makeRecordJSON("s-stale-seal-dst", "st-1", 1, `{"v":1}`),
				SourcePin: journal.Pin{
					Stream:   "s-stale-seal-src",
					RecordID: "st-1",
					Revision: 1,
					Digest:   rStale1.Digest,
				},
				IdempotencyKey: "k-stale-imp-1",
			},
		},
		ActorKind:  "human",
		Actor:      "alice@example.com",
		AuthRef:    "auth:stale",
		AuthDigest: "sha256:" + strings.Repeat("d", 64),
	})
	if err != nil || outStale.SealedCutSeq != 3 {
		t.Fatalf("expected SealAndCompactStream to replace stale cut-2 seal via PutIfMatch at cut 3, got %+v, err=%v", outStale, err)
	}

	// 12. Interrupted ReclaimSealedAncestor that wrote reclaimed.json for candidate successor A
	// and failed before head.json CAS: subsequent ReclaimSealedAncestor for valid successor B
	// reconciles reclaimed.json via PutIfMatch so head.ReclaimedDigest matches reclaimed.json.
	if _, err := bs.SealAndCompactStream(ctx, bucketcas.CompactionInput{
		SourceStream:       "s-stale-seal-src",
		DestinationStream:  "s-stale-seal-dst-2",
		Mode:               "single_revision",
		AllowDropUnretired: true,
		Records: []bucketcas.CompactRecordInput{
			{
				Record: makeRecordJSON("s-stale-seal-dst-2", "st-1", 1, `{"v":1}`),
				SourcePin: journal.Pin{
					Stream:   "s-stale-seal-src",
					RecordID: "st-1",
					Revision: 1,
					Digest:   rStale1.Digest,
				},
				IdempotencyKey: "k-stale-imp-2",
			},
		},
		ActorKind:  "human",
		Actor:      "alice@example.com",
		AuthRef:    "auth:stale",
		AuthDigest: "sha256:" + strings.Repeat("d", 64),
	}); err != nil {
		t.Fatalf("SealAndCompactStream s-stale-seal-dst-2: %v", err)
	}
	abortReclaimA := false
	mem.SetPutIfMatchHook(func(key, _ string, _ []byte) (bool, bool, stratum.CommitKnowledge, error) {
		if key == "multihop/streams/s-stale-seal-src/head.json" && !abortReclaimA {
			abortReclaimA = true
			mem.SetPutIfMatchHook(nil)
			return true, false, stratum.CommitNotCommitted, errors.New("simulated crash after writing reclaimed.json for dst-1")
		}
		return false, false, 0, nil
	})
	if _, _, err := bs.ReclaimSealedAncestor(ctx, "s-stale-seal-src", "s-stale-seal-dst"); err == nil {
		t.Fatal("expected simulated crash on first ReclaimSealedAncestor")
	}
	// Now reclaim via s-stale-seal-dst-2: reclaimed.json must be reconciled via PutIfMatch!
	if _, _, err := bs.ReclaimSealedAncestor(ctx, "s-stale-seal-src", "s-stale-seal-dst-2"); err != nil {
		t.Fatalf("ReclaimSealedAncestor via s-stale-seal-dst-2 after interrupted dst-1 reclaim: %v", err)
	}
	// Subsequent ReclaimSealedAncestor naming dst-1 must fail closed with ErrInvalidStore!
	if _, _, err := bs.ReclaimSealedAncestor(ctx, "s-stale-seal-src", "s-stale-seal-dst"); !errors.Is(err, bucketcas.ErrInvalidStore) {
		t.Fatalf("expected ErrInvalidStore when reclaiming already-reclaimed predecessor into different successor, got %v", err)
	}
	// Verify both continuation streams load cleanly from the reclaimed predecessor!
	if _, _, err := bs.State(ctx, "s-stale-seal-dst"); err != nil {
		t.Fatalf("State(s-stale-seal-dst) after reconciled reclamation: %v", err)
	}
	if _, _, err := bs.State(ctx, "s-stale-seal-dst-2"); err != nil {
		t.Fatalf("State(s-stale-seal-dst-2) after reconciled reclamation: %v", err)
	}

	// 13. Concurrent ReclaimSealedAncestor callers A and B naming different valid successors:
	// because reclaimed.json is a deterministic function of the sealed predecessor and written
	// strictly once via PutIfAbsent (with the winning successor recorded in head.json's ReclaimedBy),
	// a losing concurrent reclaimer cannot overwrite reclaimed.json underneath a committed head.
	if err := bs.OpenStream(ctx, "s-conc-recl-src", "single_revision"); err != nil {
		t.Fatalf("OpenStream s-conc-recl-src: %v", err)
	}
	for _, dst := range []string{"s-conc-recl-dst-a", "s-conc-recl-dst-b"} {
		if _, err := bs.SealAndCompactStream(ctx, bucketcas.CompactionInput{
			SourceStream:      "s-conc-recl-src",
			DestinationStream: dst,
			Mode:              "single_revision",
			ActorKind:         "human",
			Actor:             "alice@example.com",
			AuthRef:           "auth:conc-recl",
			AuthDigest:        "sha256:" + strings.Repeat("e", 64),
		}); err != nil {
			t.Fatalf("SealAndCompactStream %s: %v", dst, err)
		}
	}
	concHeadKey := "multihop/streams/s-conc-recl-src/head.json"
	concInjected := false
	mem.SetPutIfMatchHook(func(key, expected string, body []byte) (bool, bool, stratum.CommitKnowledge, error) {
		if key != concHeadKey || concInjected {
			return false, false, 0, nil
		}
		concInjected = true
		mem.SetPutIfMatchHook(nil)
		// Commit A's head CAS first, then let B attempt ReclaimSealedAncestor concurrently.
		if _, commit, _, err := mem.PutIfMatch(ctx, concHeadKey, expected, body); err != nil || commit != stratum.CommitCommitted {
			t.Fatalf("commit A head: %v, %v", commit, err)
		}
		if _, _, err := bs.ReclaimSealedAncestor(ctx, "s-conc-recl-src", "s-conc-recl-dst-b"); !errors.Is(err, bucketcas.ErrInvalidStore) {
			t.Fatalf("expected B to be refused with ErrInvalidStore after A committed head, got %v", err)
		}
		return true, false, stratum.CommitCommitted, nil
	})
	if _, _, err := bs.ReclaimSealedAncestor(ctx, "s-conc-recl-src", "s-conc-recl-dst-a"); err != nil {
		t.Fatalf("ReclaimSealedAncestor dst-a: %v", err)
	}
	if !concInjected {
		t.Fatal("expected concurrent reclamation hook to fire")
	}
	for _, dst := range []string{"s-conc-recl-dst-a", "s-conc-recl-dst-b"} {
		if _, _, err := bs.State(ctx, dst); err != nil {
			t.Fatalf("State(%s) after concurrent reclamation: %v", dst, err)
		}
	}
	if _, _, err := bs.ReclaimSealedAncestor(ctx, "s-conc-recl-src", "s-conc-recl-dst-a"); err != nil {
		t.Fatalf("idempotent retry of winning ReclaimSealedAncestor dst-a: %v", err)
	}
}
