package recordprims

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func recordJSON(stream, recordID string, rev int, supersedes *Pin, payload string, body *BodyRef) json.RawMessage {
	supField := ""
	if supersedes != nil {
		supField = fmt.Sprintf(`,"supersedes":{"revision":%d,"digest":%q}`, supersedes.Revision, supersedes.Digest)
	}
	contentField := fmt.Sprintf(`,"payload":%s`, payload)
	if body != nil {
		contentField = fmt.Sprintf(`,"body":{"digest":%q,"length":%d,"media_type":%q}`, body.Digest, body.Length, body.MediaType)
	}
	s := fmt.Sprintf(
		`{"v":1,"stream":%q,"record_id":%q,"revision":%d%s,"payload_contract":"contract: notes/v1"%s,"provenance":{"actor_kind":"process","actor":"test"},"classification":{"retention":"standard","sensitivity":"1-confidential"}}`,
		stream, recordID, rev, supField, contentField,
	)
	return json.RawMessage(s)
}

type collectSink struct {
	headers []FrameHeader
	payload [][]byte
	fn      func(ctx context.Context, h FrameHeader, r io.Reader) error
}

func (cs *collectSink) Frame(ctx context.Context, h FrameHeader, r io.Reader) error {
	if cs.fn != nil {
		return cs.fn(ctx, h, r)
	}
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	cs.headers = append(cs.headers, h)
	cs.payload = append(cs.payload, b)
	return nil
}

func TestOpenStreamRefusesDesignatedBodyRemovalStream(t *testing.T) {
	s, _, _ := openBodyStore(t)
	if _, err := s.OpenStream(DefaultBodyRemovalStream, ModeSingleRevision); !errors.Is(err, ErrSchema) {
		t.Fatalf("want ErrSchema when opening designated stream, got %v", err)
	}
}

func TestStreamAppendReviseDeduplicateAndConflict(t *testing.T) {
	s, _, clk := openBodyStore(t)
	st, err := s.OpenStream("notes", ModeRevisioned)
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}
	// Reopen with same mode is idempotent; different mode fails.
	if _, err := s.OpenStream("notes", ModeRevisioned); err != nil {
		t.Fatalf("reopen same mode: %v", err)
	}
	if _, err := s.OpenStream("notes", ModeSingleRevision); !errors.Is(err, ErrSchema) {
		t.Fatalf("reopen wrong mode: want ErrSchema, got %v", err)
	}

	r1 := recordJSON("notes", "rec-1", 1, nil, `{"msg":"v1"}`, nil)
	rc1, err := st.Append(ctx, Write{Record: r1, IdempotencyKey: "k-1"})
	if err != nil || rc1.Outcome != OutcomeAppended || rc1.Seq != 2 || rc1.Pin == nil || rc1.Pin.Revision != 1 {
		t.Fatalf("append r1: %+v, err=%v", rc1, err)
	}

	// Deduplicated retry with same key and identical record returns original receipt.
	clk.t = clk.t.Add(time.Second)
	rc1Dup, err := st.Append(ctx, Write{Record: r1, IdempotencyKey: "k-1"})
	if err != nil || rc1Dup.Outcome != OutcomeAppended || rc1Dup.Seq != rc1.Seq || rc1Dup.Pin.Digest != rc1.Pin.Digest {
		t.Fatalf("deduplicated r1: %+v, err=%v", rc1Dup, err)
	}

	// Same key with different content yields idempotency_conflict.
	r1Other := recordJSON("notes", "rec-1", 1, nil, `{"msg":"different"}`, nil)
	rcConflictKey, err := st.Append(ctx, Write{Record: r1Other, IdempotencyKey: "k-1"})
	if err != nil || rcConflictKey.Outcome != OutcomeIdempotencyConflict {
		t.Fatalf("key conflict: %+v, err=%v", rcConflictKey, err)
	}

	// Appending revision 1 again with a new key yields revision_conflict.
	rcRevConflict, err := st.Append(ctx, Write{Record: r1Other, IdempotencyKey: "k-2"})
	if err != nil || rcRevConflict.Outcome != OutcomeRevisionConflict || rcRevConflict.Current == nil || rcRevConflict.Current.Digest != rc1.Pin.Digest {
		t.Fatalf("revision conflict: %+v, err=%v", rcRevConflict, err)
	}

	// Revision 2 superseding r1 succeeds.
	clk.t = clk.t.Add(time.Second)
	r2 := recordJSON("notes", "rec-1", 2, rc1.Pin, `{"msg":"v2"}`, nil)
	rc2, err := st.Append(ctx, Write{Record: r2, IdempotencyKey: "k-3"})
	if err != nil || rc2.Outcome != OutcomeRevised || rc2.Pin == nil || rc2.Pin.Revision != 2 {
		t.Fatalf("revise r2: %+v, err=%v", rc2, err)
	}

	head, ok, err := st.Head("rec-1")
	if err != nil || !ok || head.Pin.Revision != 2 || head.Retired {
		t.Fatalf("Head: %+v, ok=%v, err=%v", head, ok, err)
	}
	revs, err := st.Revisions("rec-1")
	if err != nil || len(revs) != 2 || revs[1].Supersedes == nil || revs[1].Supersedes.Revision != 1 {
		t.Fatalf("Revisions: %+v, err=%v", revs, err)
	}
}

func TestStreamRetireKeyedAndUnkeyedAndReconcile(t *testing.T) {
	s, _, clk := openBodyStore(t)
	st, err := s.OpenStream("items", ModeSingleRevision)
	if err != nil {
		t.Fatal(err)
	}
	r1 := recordJSON("items", "item-1", 1, nil, `{"x":1}`, nil)
	if _, err := st.Append(ctx, Write{Record: r1, IdempotencyKey: "k-append-1"}); err != nil {
		t.Fatal(err)
	}

	// G13: Key lookup precedes target_not_found, and missing record with fresh key returns target_not_found.
	clk.t = clk.t.Add(time.Second)
	miss, err := st.Retire(ctx, "no-such-record", RetireWithdrawn, WithRetireKey("k-ret-miss"))
	if err != nil || miss.Outcome != RetireOutcomeTargetNotFound {
		t.Fatalf("keyed retire missing: %+v, err=%v", miss, err)
	}

	// Keyed retirement on item-1 succeeds and replays idempotently.
	ret1, err := st.Retire(ctx, "item-1", RetireWithdrawn, WithRetireKey("k-ret-1"))
	if err != nil || ret1.Outcome != RetireOutcomeRetired || ret1.Seq == 0 {
		t.Fatalf("keyed retire: %+v, err=%v", ret1, err)
	}
	clk.t = clk.t.Add(time.Second)
	ret1Replay, err := st.Retire(ctx, "item-1", RetireWithdrawn, WithRetireKey("k-ret-1"))
	if err != nil || ret1Replay.Outcome != RetireOutcomeRetired || ret1Replay.Seq != ret1.Seq || ret1Replay.At != ret1.At {
		t.Fatalf("keyed retire replay: %+v, err=%v", ret1Replay, err)
	}

	// Unkeyed repeat on already-retired record appends nothing and returns original Seq and At.
	clk.t = clk.t.Add(time.Second)
	unkeyedRepeat, err := st.Retire(ctx, "item-1", RetireWithdrawn)
	if err != nil || unkeyedRepeat.Outcome != RetireOutcomeAlreadyRetired || unkeyedRepeat.Seq != ret1.Seq || unkeyedRepeat.At != ret1.At {
		t.Fatalf("unkeyed repeat: %+v, err=%v", unkeyedRepeat, err)
	}

	// Reconcile finds both content and retirement keys, and refuses sub-microsecond attemptedAt (G10).
	if _, err := st.Reconcile("k-ret-1", clk.t.Add(123*time.Nanosecond)); !errors.Is(err, ErrSchema) {
		t.Fatalf("want ErrSchema on sub-microsecond attemptedAt, got %v", err)
	}
	recFound, err := st.Reconcile("k-ret-1", clk.t)
	if err != nil || recFound.Result != "found" || len(recFound.Receipt) == 0 {
		t.Fatalf("Reconcile found: %+v, err=%v", recFound, err)
	}
	recUnknown, err := st.Reconcile("never-seen", clk.t)
	if err != nil || recUnknown.Result != "unknown" {
		t.Fatalf("Reconcile unknown: %+v, err=%v", recUnknown, err)
	}
}

func TestBodyReferenceAndHoldPreventRemoveBodyAndRelease(t *testing.T) {
	s, dir, clk := openBodyStore(t)
	st, err := s.OpenStream("docs", ModeSingleRevision)
	if err != nil {
		t.Fatal(err)
	}

	bodyBytes := []byte("confidential report body")
	ref, lease := put(t, s, bodyBytes)

	rec := recordJSON("docs", "doc-1", 1, nil, "", &ref)
	rc, err := st.Append(ctx, Write{
		Record:         rec,
		IdempotencyKey: "k-doc-1",
		Lease:          &lease,
	})
	if err != nil || rc.Outcome != OutcomeAppended || rc.BodyStatus != "verified" {
		t.Fatalf("Append with body: %+v, err=%v", rc, err)
	}

	// Lease was consumed by Append; RemoveBody now sees still_referenced (A6/G4b).
	expire(clk)
	if got := remove(t, s, bodyBytes); got != BodyStillReferenced {
		t.Fatalf("RemoveBody on referenced body = %s, want still_referenced", got)
	}

	// Release(serve) delivers both the record frame and the body frame.
	sink := &collectSink{}
	relRes, err := st.Release(ctx, UseRequest{Purpose: PurposeServe, Closure: ClosureFullContent}, sink)
	if err != nil || relRes.Outcome != ReleaseReleased || relRes.FramesCompleted != 2 || len(sink.payload) != 2 {
		t.Fatalf("Release(serve): %+v, err=%v", relRes, err)
	}
	if !bytes.Equal(sink.payload[1], bodyBytes) {
		t.Fatalf("body frame payload = %q", sink.payload[1])
	}

	// Place a protection hold on doc-1; RemoveBody returns held, and Release(export) of doc-1 refuses protected_use.
	clk.t = clk.t.Add(time.Second)
	protRaw := fmt.Sprintf(
		`{"v":1,"stream":"docs","idempotency_key":"k-hold-1","action":"tombstone","target":{"stream":"docs","record_id":"doc-1","revision":1,"digest":%q},"scope":"record","reason":"security_hold","evidence":{"ref":"ticket-1","digest":%q},"observed_at":%q,"provenance":{"actor_kind":"process","actor":"test"}}`,
		rc.Pin.Digest, digestOf([]byte("ev")), FormatInstant(clk.t),
	)
	protRc, err := st.Protect(ctx, ProtectionRequest{Raw: json.RawMessage(protRaw)})
	if err != nil || protRc.Outcome != "tombstoned" {
		t.Fatalf("Protect: %+v, err=%v", protRc, err)
	}
	if got := remove(t, s, bodyBytes); got != BodyHeld {
		t.Fatalf("RemoveBody while held = %s, want held", got)
	}
	relHeld, err := st.Release(ctx, UseRequest{
		Purpose:   PurposeExport,
		Selection: []Pin{*rc.Pin},
		Closure:   ClosureFullContent,
	}, &collectSink{})
	if err != nil || relHeld.Outcome != ReleaseRefused || relHeld.RefusalReason != "protected_use" {
		t.Fatalf("Release of held record: %+v, err=%v", relHeld, err)
	}

	// Restore the hold, then retire doc-1: A6/G4b rules that a retired record's
	// historical reference still prevents physical byte removal (still_referenced).
	clk.t = clk.t.Add(time.Second)
	restRaw := fmt.Sprintf(
		`{"v":1,"stream":"docs","idempotency_key":"k-rest-1","action":"restore","restores":%d,"target":{"stream":"docs","record_id":"doc-1","revision":1,"digest":%q},"scope":"record","reason":"hold_released","evidence":{"ref":"ticket-1","digest":%q},"observed_at":%q,"provenance":{"actor_kind":"process","actor":"test"}}`,
		protRc.Seq, rc.Pin.Digest, digestOf([]byte("ev")), FormatInstant(clk.t),
	)
	restRc, err := st.Protect(ctx, ProtectionRequest{Raw: json.RawMessage(restRaw)})
	if err != nil || restRc.Outcome != "untombstoned" {
		t.Fatalf("Protect restore: %+v, err=%v", restRc, err)
	}
	if _, err := st.Retire(ctx, "doc-1", RetireRetention); err != nil {
		t.Fatal(err)
	}
	if got := remove(t, s, bodyBytes); got != BodyStillReferenced {
		t.Fatalf("RemoveBody after retire = %s, want still_referenced", got)
	}

	// G17: If the designated stream is missing, Release with full_content returns
	// determinate ErrJournalInvalid, while metadata_only succeeds.
	jpath := filepath.Join(dir, "streams", streamHash(DefaultBodyRemovalStream), "journal.ndjson")
	os.Remove(jpath)
	var ji *ErrJournalInvalid
	if _, err := st.Release(ctx, UseRequest{Purpose: PurposeReuse, Closure: ClosureFullContent}, &collectSink{}); !errors.As(err, &ji) {
		t.Fatalf("Release full_content with missing designated stream: want ErrJournalInvalid, got %v", err)
	}
	if relMeta, err := st.Release(ctx, UseRequest{Purpose: PurposeReuse, Closure: ClosureMetadataOnly}, &collectSink{}); err != nil || relMeta.Outcome != ReleaseReleased {
		t.Fatalf("Release metadata_only with missing designated stream: %+v, err=%v", relMeta, err)
	}
}

func TestReleaseSinkRevocationShortReadAndReentry(t *testing.T) {
	s, _, _ := openBodyStore(t)
	st, err := s.OpenStream("sink-test", ModeSingleRevision)
	if err != nil {
		t.Fatal(err)
	}
	r1 := recordJSON("sink-test", "r1", 1, nil, `{"a":1}`, nil)
	if _, err := st.Append(ctx, Write{Record: r1, IdempotencyKey: "k1"}); err != nil {
		t.Fatal(err)
	}

	// 1. Reader retained after Frame returns yields ErrReaderRevoked.
	var retained io.Reader
	res, err := st.Release(ctx, UseRequest{Purpose: PurposeServe}, &collectSink{
		fn: func(_ context.Context, _ FrameHeader, r io.Reader) error {
			retained = r
			_, err := io.ReadAll(r)
			return err
		},
	})
	if err != nil || res.Outcome != ReleaseReleased {
		t.Fatalf("Release: %+v, err=%v", res, err)
	}
	buf := make([]byte, 8)
	if n, rerr := retained.Read(buf); n != 0 || !errors.Is(rerr, ErrReaderRevoked) {
		t.Fatalf("retained Read = (%d, %v), want (0, ErrReaderRevoked)", n, rerr)
	}

	// 2. Short read with nil return marks frame uncertain.
	resShort, err := st.Release(ctx, UseRequest{Purpose: PurposeServe}, &collectSink{
		fn: func(_ context.Context, _ FrameHeader, r io.Reader) error {
			var one [1]byte
			_, _ = r.Read(one[:])
			return nil
		},
	})
	if err != nil || resShort.Outcome != ReleaseUncertain || resShort.FrameInFlight == nil || *resShort.FrameInFlight != 0 {
		t.Fatalf("short read: %+v, err=%v", resShort, err)
	}

	// 3. Same-goroutine reentry inside Frame (even with context.Background()) returns ErrSinkReentered.
	var reentryErr error
	_, _ = st.Release(ctx, UseRequest{Purpose: PurposeServe}, &collectSink{
		fn: func(_ context.Context, _ FrameHeader, r io.Reader) error {
			_, _, reentryErr = st.Head("r1")
			_, _ = io.ReadAll(r)
			return nil
		},
	})
	if !errors.Is(reentryErr, ErrSinkReentered) {
		t.Fatalf("same-goroutine reentry: want ErrSinkReentered, got %v", reentryErr)
	}

	// 4. Panic inside Frame still revokes the retained reader and clears the reentry guard.
	var panickedReader io.Reader
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("expected sink panic")
			}
		}()
		_, _ = st.Release(ctx, UseRequest{Purpose: PurposeServe}, &collectSink{
			fn: func(_ context.Context, _ FrameHeader, r io.Reader) error {
				panickedReader = r
				panic("sink panic")
			},
		})
	}()
	if n, rerr := panickedReader.Read(buf); n != 0 || !errors.Is(rerr, ErrReaderRevoked) {
		t.Fatalf("retained Read after panic = (%d, %v), want (0, ErrReaderRevoked)", n, rerr)
	}
	if _, _, herr := st.Head("r1"); errors.Is(herr, ErrSinkReentered) {
		t.Fatalf("Head after sink panic returned ErrSinkReentered")
	}
}

func TestAppendRefusesInvalidUTF8IdempotencyKeyBeforeWrite(t *testing.T) {
	s, _, _ := openBodyStore(t)
	st, err := s.OpenStream("notes", ModeSingleRevision)
	if err != nil {
		t.Fatal(err)
	}
	w := Write{
		Record:         recordJSON("notes", "r1", 1, nil, `{"a":1}`, nil),
		IdempotencyKey: string([]byte{'k', 0xff}),
	}
	rcpt, err := st.Append(ctx, w)
	if !errors.Is(err, ErrSchema) || IsIndeterminate(err) {
		t.Fatalf("Append with invalid UTF-8 key: rcpt=%+v err=%v, want determinate ErrSchema", rcpt, err)
	}
	// Stream journal remains valid for subsequent writes.
	if _, err := st.Append(ctx, Write{
		Record:         recordJSON("notes", "r1", 1, nil, `{"a":1}`, nil),
		IdempotencyKey: "k-valid",
	}); err != nil {
		t.Fatalf("subsequent valid Append failed: %v", err)
	}
}
