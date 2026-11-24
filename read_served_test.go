package recordprims

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

type readServedFixture struct {
	dir  string
	s    *Store
	st   *Stream
	clk  *stepClock
	pins map[string]Pin
	body []byte
}

// newReadServedFixture opens a stream with a plain record (two revisions), a
// record with a body, a retired record and a record whose revision is held.
func newReadServedFixture(t *testing.T) readServedFixture {
	t.Helper()
	s, dir, clk := openBodyStore(t)
	st, err := s.OpenStream("rs", ModeRevisioned)
	if err != nil {
		t.Fatal(err)
	}
	f := readServedFixture{dir: dir, s: s, st: st, clk: clk, pins: map[string]Pin{}, body: []byte("served-body-bytes")}
	appendRec := func(name, id string, rev int, sup *Pin, body *BodyRef, lease *PublicationLease) {
		t.Helper()
		clk.t = clk.t.Add(time.Second)
		rc, err := st.Append(ctx, Write{Record: recordJSON("rs", id, rev, sup, `{"n":1}`, body), IdempotencyKey: "k-" + name, Lease: lease})
		if err != nil || rc.Pin == nil {
			t.Fatalf("append %s: %+v, %v", name, rc, err)
		}
		f.pins[name] = *rc.Pin
	}
	appendRec("plain1", "plain", 1, nil, nil, nil)
	p1 := f.pins["plain1"]
	appendRec("plain2", "plain", 2, &p1, nil, nil)
	ref, lease := put(t, s, f.body)
	appendRec("body", "body", 1, nil, &ref, &lease)
	appendRec("retired", "retired", 1, nil, nil, nil)
	appendRec("held", "held", 1, nil, nil, nil)

	clk.t = clk.t.Add(time.Second)
	if _, err := st.Retire(ctx, "retired", RetireWithdrawn); err != nil {
		t.Fatal(err)
	}
	clk.t = clk.t.Add(time.Second)
	held := f.pins["held"]
	hold := fmt.Sprintf(
		`{"v":1,"stream":"rs","idempotency_key":"k-hold","action":"tombstone","target":{"stream":"rs","record_id":"held","revision":1,"digest":%q},"scope":"revision","reason":"operator_hold","evidence":{"ref":"case-1","digest":%q},"observed_at":%q,"provenance":{"actor_kind":"process","actor":"test"}}`,
		held.Digest, digestOf([]byte("ev")), FormatInstant(clk.t))
	if rc, err := st.Protect(ctx, ProtectionRequest{Raw: []byte(hold)}); err != nil || rc.Outcome != "tombstoned" {
		t.Fatalf("hold: %+v, %v", rc, err)
	}
	return f
}

func TestReadServedDeliversExactlyThePin(t *testing.T) {
	f := newReadServedFixture(t)
	for _, c := range []struct {
		name    string
		closure UseClosure
		frames  []FrameKind
	}{
		{"plain1", ClosureMetadataOnly, []FrameKind{FrameKindRecord}},
		{"plain1", ClosureFullContent, []FrameKind{FrameKindRecord}},
		{"plain2", ClosureFullContent, []FrameKind{FrameKindRecord}},
		{"body", ClosureMetadataOnly, []FrameKind{FrameKindRecord}},
		{"body", ClosureFullContent, []FrameKind{FrameKindRecord, FrameKindBody}},
		{"body", "", []FrameKind{FrameKindRecord, FrameKindBody}},
	} {
		pin := f.pins[c.name]
		sink := &collectSink{}
		got, err := f.st.ReadServed(ctx, pin, c.closure, sink)
		if err != nil || got.Outcome != ServedReadServed || got.Reason != "" || got.Cut == nil {
			t.Fatalf("%s/%s: %+v, %v", c.name, c.closure, got, err)
		}
		n := len(c.frames)
		if got.Frames != n || got.Delivery != ReleaseReleased || got.FramesCompleted != n || got.FrameInFlight != nil {
			t.Fatalf("%s/%s: progress %+v", c.name, c.closure, got)
		}
		if len(sink.headers) != n {
			t.Fatalf("%s/%s: %d frames, want %d", c.name, c.closure, len(sink.headers), n)
		}
		for i, h := range sink.headers {
			if h.Pin != pin || h.Kind != c.frames[i] || h.Index != i {
				t.Fatalf("%s/%s: frame %d %+v", c.name, c.closure, i, h)
			}
		}
		if n == 2 && string(sink.payload[1]) != string(f.body) {
			t.Fatalf("%s: body frame %q", c.name, sink.payload[1])
		}
	}
}

func TestReadServedNilSinkEvaluatesOnly(t *testing.T) {
	f := newReadServedFixture(t)
	got, err := f.st.ReadServed(ctx, f.pins["body"], ClosureFullContent, nil)
	if err != nil || got.Outcome != ServedReadServed || got.Frames != 2 || got.Delivery != "" ||
		got.FramesCompleted != 0 || got.FrameInFlight != nil || got.Cut == nil {
		t.Fatalf("nil sink: %+v, %v", got, err)
	}
	got, err = f.st.ReadServed(ctx, f.pins["held"], ClosureFullContent, nil)
	if err != nil || got.Outcome != ServedReadNotServed || got.Frames != 0 || got.Delivery != "" {
		t.Fatalf("nil sink held: %+v, %v", got, err)
	}
}

// Held, retired, absent and digest-mismatched pins give one public result at
// the same cut and never call the sink, under both closures.
func TestReadServedDenialsAreIndistinguishable(t *testing.T) {
	f := newReadServedFixture(t)
	wrongDigest := f.pins["plain2"]
	wrongDigest.Digest = f.pins["plain1"].Digest
	otherDigest := f.pins["plain1"]
	otherDigest.Digest = digestOf([]byte("nothing"))
	absentRev := f.pins["plain2"]
	absentRev.Revision = 3
	absentRec := f.pins["plain1"]
	absentRec.RecordID = "never-written"
	otherStream := f.pins["plain1"]
	otherStream.Stream = "elsewhere"
	retiredLatest := f.pins["retired"]

	cases := map[string]Pin{
		"held":         f.pins["held"],
		"retired":      retiredLatest,
		"wrong digest": wrongDigest,
		"other digest": otherDigest,
		"absent rev":   absentRev,
		"absent rec":   absentRec,
		"other stream": otherStream,
	}
	for _, closure := range []UseClosure{ClosureMetadataOnly, ClosureFullContent} {
		var first *ServedRead
		for name, pin := range cases {
			calls := 0
			sink := &collectSink{fn: func(context.Context, FrameHeader, io.Reader) error { calls++; return nil }}
			got, err := f.st.ReadServed(ctx, pin, closure, sink)
			if err != nil || got.Outcome != ServedReadNotServed || got.Reason != "" || got.Cut == nil || calls != 0 {
				t.Fatalf("%s/%s: %+v, %v, %d sink calls", name, closure, got, err, calls)
			}
			if first == nil {
				first = &got
			} else if !reflect.DeepEqual(*first, got) {
				t.Fatalf("%s/%s: %+v differs from %+v", name, closure, got, *first)
			}
		}
	}
}

// Under full_content a revision whose body cannot be delivered reads the same
// as an absent pin; under metadata_only it is served.
func TestReadServedUndeliverableBodyIsNotServed(t *testing.T) {
	f := newReadServedFixture(t)
	if err := os.Remove(filepath.Join(f.dir, bodyFile(digestHex(digestOf(f.body))))); err != nil {
		t.Fatal(err)
	}
	absent := f.pins["body"]
	absent.RecordID = "never-written"
	var results []ServedRead
	for _, pin := range []Pin{f.pins["body"], absent} {
		calls := 0
		sink := &collectSink{fn: func(context.Context, FrameHeader, io.Reader) error { calls++; return nil }}
		got, err := f.st.ReadServed(ctx, pin, ClosureFullContent, sink)
		if err != nil || got.Outcome != ServedReadNotServed || calls != 0 {
			t.Fatalf("%s: %+v, %v, %d calls", pin.RecordID, got, err, calls)
		}
		results = append(results, got)
	}
	if !reflect.DeepEqual(results[0], results[1]) {
		t.Fatalf("undeliverable body distinguishable: %+v vs %+v", results[0], results[1])
	}
	sink := &collectSink{}
	got, err := f.st.ReadServed(ctx, f.pins["body"], ClosureMetadataOnly, sink)
	if err != nil || got.Outcome != ServedReadServed || got.Frames != 1 || len(sink.headers) != 1 {
		t.Fatalf("metadata_only: %+v, %v", got, err)
	}
}

func TestReadServedRejectsMalformedRequests(t *testing.T) {
	f := newReadServedFixture(t)
	good := f.pins["plain1"]
	mutate := func(fn func(*Pin)) Pin { p := good; fn(&p); return p }
	for name, pin := range map[string]Pin{
		"empty stream":    mutate(func(p *Pin) { p.Stream = "" }),
		"bad stream":      mutate(func(p *Pin) { p.Stream = "Upper" }),
		"empty record":    mutate(func(p *Pin) { p.RecordID = "" }),
		"revision 0":      mutate(func(p *Pin) { p.Revision = 0 }),
		"revision > 2^53": mutate(func(p *Pin) { p.Revision = maxSafeInteger + 1 }),
		"revision max":    mutate(func(p *Pin) { p.Revision = ^uint64(0) }),
		"short digest":    mutate(func(p *Pin) { p.Digest = "sha256:abc" }),
		"upper digest":    mutate(func(p *Pin) { p.Digest = strings.ToUpper(p.Digest) }),
	} {
		calls := 0
		sink := &collectSink{fn: func(context.Context, FrameHeader, io.Reader) error { calls++; return nil }}
		if _, err := f.st.ReadServed(ctx, pin, ClosureMetadataOnly, sink); !errors.Is(err, ErrSchema) || calls != 0 {
			t.Fatalf("%s: %v, %d calls", name, err, calls)
		}
	}
	if _, err := f.st.ReadServed(ctx, good, UseClosure("partial"), nil); !errors.Is(err, ErrSchema) {
		t.Fatalf("bad closure: %v", err)
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := f.st.ReadServed(cctx, good, ClosureMetadataOnly, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled: %v", err)
	}
}

func TestReadServedSinkFailureReportsProgress(t *testing.T) {
	f := newReadServedFixture(t)
	pin := f.pins["body"]
	for _, failAt := range []int{0, 1} {
		var seen []FrameKind
		sink := &collectSink{fn: func(_ context.Context, h FrameHeader, r io.Reader) error {
			seen = append(seen, h.Kind)
			if h.Index == failAt {
				return errors.New("caller detail must not leak")
			}
			_, err := io.ReadAll(r)
			return err
		}}
		got, err := f.st.ReadServed(ctx, pin, ClosureFullContent, sink)
		if !errors.Is(err, ErrSink) || strings.Contains(err.Error(), "leak") {
			t.Fatalf("fail at %d: err %v", failAt, err)
		}
		if got.Outcome != ServedReadServed || got.Frames != 2 || got.Delivery != ReleaseUncertain ||
			got.FramesCompleted != failAt || got.FrameInFlight == nil || *got.FrameInFlight != failAt {
			t.Fatalf("fail at %d: %+v", failAt, got)
		}
		if len(seen) != failAt+1 {
			t.Fatalf("fail at %d: sink saw %v", failAt, seen)
		}
	}
}

// A stream whose journal cannot be replayed is refused for every pin with
// protection_unavailable and no cut.
func TestReadServedInvalidJournalHasNoCut(t *testing.T) {
	s, dir, _ := openBodyStore(t)
	st, err := s.OpenStream("rs-bad", ModeSingleRevision)
	if err != nil {
		t.Fatal(err)
	}
	rc, err := st.Append(ctx, Write{Record: recordJSON("rs-bad", "doc-1", 1, nil, `{"ok":true}`, nil), IdempotencyKey: "k-1"})
	if err != nil || rc.Pin == nil {
		t.Fatalf("append: %+v, %v", rc, err)
	}
	jf, err := os.OpenFile(filepath.Join(dir, streamJournalFile("rs-bad")), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := jf.WriteString("{\"not\":\"an entry\"}\n"); err != nil {
		t.Fatal(err)
	}
	jf.Close()
	for _, pin := range []Pin{*rc.Pin, {Stream: "rs-bad", RecordID: "never-written", Revision: 1, Digest: digestOf(nil)}} {
		calls := 0
		sink := &collectSink{fn: func(context.Context, FrameHeader, io.Reader) error { calls++; return nil }}
		got, err := st.ReadServed(ctx, pin, ClosureMetadataOnly, sink)
		if err != nil || got.Outcome != ServedReadRefused || got.Reason != "protection_unavailable" || got.Cut != nil || got.Frames != 0 || calls != 0 {
			t.Fatalf("invalid journal %s: %+v, %v, %d calls", pin.RecordID, got, err, calls)
		}
	}
}

// A continuation is refused with restrictions_pending before its review, for
// any pin; after the review, a hold inherited from the source makes every pin
// not_served, indistinguishable from an absent pin.
func TestReadServedContinuationRestrictions(t *testing.T) {
	s, _, clk := openBodyStore(t)
	src, err := s.OpenStream("rs-src", ModeSingleRevision)
	if err != nil {
		t.Fatal(err)
	}
	r1, err := src.Append(ctx, Write{Record: recordJSON("rs-src", "doc-1", 1, nil, `{"ok":true}`, nil), IdempotencyKey: "k-1"})
	if err != nil || r1.Pin == nil {
		t.Fatalf("append: %+v, %v", r1, err)
	}
	clk.t = clk.t.Add(time.Second)
	hold := fmt.Sprintf(
		`{"v":1,"stream":"rs-src","idempotency_key":"k-hold","action":"tombstone","target":{"stream":"rs-src","record_id":"doc-1","revision":1,"digest":%q},"scope":"revision","reason":"operator_hold","evidence":{"ref":"case-1","digest":%q},"observed_at":%q,"provenance":{"actor_kind":"process","actor":"test"}}`,
		r1.Pin.Digest, digestOf([]byte("ev")), FormatInstant(clk.t))
	hr, err := src.Protect(ctx, ProtectionRequest{Raw: []byte(hold)})
	if err != nil || hr.Outcome != "tombstoned" {
		t.Fatalf("hold: %+v, %v", hr, err)
	}
	cont := Continues{Capability: "contract: durable-record/v1", Stream: "rs-src", CutSeq: hr.Seq, CutDigest: mustCutDigest(t, s, "rs-src", hr.Seq)}
	dst, err := s.OpenStream("rs-dst", ModeSingleRevision, WithContinues(cont))
	if err != nil {
		t.Fatal(err)
	}
	rc, err := dst.Append(ctx, Write{Record: recordJSON("rs-dst", "doc-2", 1, nil, `{"ok":true}`, nil), IdempotencyKey: "k-2"})
	if err != nil || rc.Pin == nil {
		t.Fatalf("dst append: %+v, %v", rc, err)
	}
	absent := Pin{Stream: "rs-dst", RecordID: "never-written", Revision: 1, Digest: digestOf(nil)}

	for _, pin := range []Pin{*rc.Pin, absent} {
		got, err := dst.ReadServed(ctx, pin, ClosureMetadataOnly, &collectSink{})
		if err != nil || got.Outcome != ServedReadRefused || got.Reason != "restrictions_pending" || got.Cut == nil {
			t.Fatalf("pending %s: %+v, %v", pin.RecordID, got, err)
		}
	}

	cs, err := dst.ContinuationState()
	if err != nil {
		t.Fatal(err)
	}
	if rr, err := dst.EstablishRestrictions(ctx, RestrictionReview{
		SourceCutSeq:                  cont.CutSeq,
		SourceCutDigest:               cont.CutDigest,
		SourceStateDigest:             cs.SourceStateDigest,
		DestinationCutSeq:             cs.DestinationCutSeq,
		DestinationAppliedStateDigest: cs.DestinationAppliedStateDigest,
		Reviewer:                      testReviewer,
		Authorization:                 testAuth,
	}); err != nil || rr.Outcome != RestrictionReviewCompleted {
		t.Fatalf("review: %+v, %v", rr, err)
	}

	for _, closure := range []UseClosure{ClosureMetadataOnly, ClosureFullContent} {
		var results []ServedRead
		for _, pin := range []Pin{*rc.Pin, absent} {
			calls := 0
			sink := &collectSink{fn: func(context.Context, FrameHeader, io.Reader) error { calls++; return nil }}
			got, err := dst.ReadServed(ctx, pin, closure, sink)
			if err != nil || got.Outcome != ServedReadNotServed || got.Cut == nil || calls != 0 {
				t.Fatalf("inherited %s/%s: %+v, %v, %d calls", pin.RecordID, closure, got, err, calls)
			}
			results = append(results, got)
		}
		if !reflect.DeepEqual(results[0], results[1]) {
			t.Fatalf("inherited hold distinguishable: %+v vs %+v", results[0], results[1])
		}
	}
}
