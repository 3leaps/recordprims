package bucketcas_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

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
