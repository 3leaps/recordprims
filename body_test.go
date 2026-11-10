package recordprims

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var ctx = context.Background()

var testProv = Provenance{ActorKind: "process", Actor: "test"}

type stepClock struct{ t time.Time }

func (c *stepClock) Now() time.Time { return c.t }

func openBodyStore(t *testing.T) (*Store, string, *stepClock) {
	t.Helper()
	dir := newStoreDir(t)
	clk := &stepClock{t: time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)}
	if err := Init(dir, InitOptions{Clock: clk, fs: testFS}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	s, err := Open(dir, OpenOptions{Clock: clk, fs: testFS})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, dir, clk
}

func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func put(t *testing.T, s *Store, b []byte) (BodyRef, PublicationLease) {
	t.Helper()
	ref, l, err := s.PutBody(ctx, bytes.NewReader(b), "application/octet-stream")
	if err != nil {
		t.Fatalf("PutBody: %v", err)
	}
	return ref, l
}

func state(t *testing.T, s *Store, b []byte) bodyStateKind {
	t.Helper()
	var st bodyState
	err := s.withLock(func() error {
		var err error
		st, err = s.bodyState(digestHex(digestOf(b)), int64(len(b)))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return st.kind
}

func removeReq(b []byte) RemoveBodyRequest {
	return RemoveBodyRequest{
		Digest:     digestOf(b),
		Length:     int64(len(b)),
		Reason:     RemoveReasonRetention,
		Provenance: testProv,
	}
}

func remove(t *testing.T, s *Store, b []byte) RemoveBodyOutcome {
	t.Helper()
	res, err := s.RemoveBody(ctx, removeReq(b))
	if err != nil {
		t.Fatalf("RemoveBody: %v", err)
	}
	return res.Outcome
}

// expire moves the clock past every lease.
func expire(clk *stepClock) { clk.t = clk.t.Add(DefaultPendingBodyTTL + time.Second) }

// failAt makes the store fail once at phase.
func failAt(s *Store, phase string) {
	fired := false
	s.hook = func(p string) error {
		if p == phase && !fired {
			fired = true
			return errors.New("injected")
		}
		return nil
	}
}

func TestPutBodyPublishesAvailableWithLease(t *testing.T) {
	s, dir, _ := openBodyStore(t)
	b := []byte("hello body")
	ref, l := put(t, s, b)
	if ref.Digest != digestOf(b) || ref.Length != int64(len(b)) || ref.MediaType != "application/octet-stream" {
		t.Fatalf("ref %+v", ref)
	}
	if l.Digest != ref.Digest || len(l.ID) != 32 {
		t.Fatalf("lease %+v", l)
	}
	if got := state(t, s, b); got != stateAvailable {
		t.Fatalf("state %s, want available", got)
	}
	h := digestHex(ref.Digest)
	got, err := os.ReadFile(filepath.Join(dir, "bodies", "sha256", h[:2], h))
	if err != nil || !bytes.Equal(got, b) {
		t.Fatalf("stored bytes %q %v", got, err)
	}
	if left, _ := os.ReadDir(filepath.Join(dir, "bodies", "tmp")); len(left) != 0 {
		t.Fatalf("tmp leftovers: %d", len(left))
	}
}

func TestPutBodyEmptyBody(t *testing.T) {
	s, _, _ := openBodyStore(t)
	ref, _ := put(t, s, nil)
	if ref.Length != 0 || ref.Digest != digestOf(nil) {
		t.Fatalf("ref %+v", ref)
	}
}

func TestPutBodyRefusesBadMediaType(t *testing.T) {
	s, _, _ := openBodyStore(t)
	for _, mt := range []string{"", "text", "Text/plain", "text/plain; charset=utf-8"} {
		if _, _, err := s.PutBody(ctx, strings.NewReader("x"), mt); !errors.Is(err, ErrSchema) {
			t.Errorf("%q: want ErrSchema, got %v", mt, err)
		}
	}
}

func TestPutBodyTwiceKeepsIncarnationAndIssuesSecondLease(t *testing.T) {
	s, _, _ := openBodyStore(t)
	b := []byte("same")
	_, l1 := put(t, s, b)
	_, l2 := put(t, s, b)
	if l1.ID == l2.ID {
		t.Fatal("concurrent producers share a lease")
	}
	n, _, _ := s.readIncarnation(digestHex(digestOf(b)))
	if n != 1 {
		t.Fatalf("incarnation %d, want 1", n)
	}
}

func TestRemoveBodyNeverPublishedIsAbsent(t *testing.T) {
	s, _, _ := openBodyStore(t)
	if got := remove(t, s, []byte("never")); got != BodyAbsent {
		t.Fatalf("got %s, want absent", got)
	}
	inc := uint64(1)
	req := removeReq([]byte("never"))
	req.CompleteIncarnation = &inc
	res, err := s.RemoveBody(ctx, req)
	if err != nil || res.Outcome != BodySuperseded {
		t.Fatalf("with complete_incarnation: %v %v", res, err)
	}
}

func TestRemoveBodyPendingWhileLeased(t *testing.T) {
	s, _, clk := openBodyStore(t)
	b := []byte("leased")
	put(t, s, b)
	if got := remove(t, s, b); got != BodyPending {
		t.Fatalf("got %s, want pending", got)
	}
	expire(clk)
	if got := remove(t, s, b); got != BodyRemoved {
		t.Fatalf("after expiry got %s, want removed", got)
	}
	if got := state(t, s, b); got != stateRemoved {
		t.Fatalf("state %s, want removed", got)
	}
	if got := remove(t, s, b); got != BodyAbsent {
		t.Fatalf("repeat got %s, want absent", got)
	}
}

func TestRemoveBodyProtectionUnknownWithStreams(t *testing.T) {
	s, dir, clk := openBodyStore(t)
	b := []byte("referenced?")
	put(t, s, b)
	expire(clk)
	os.Mkdir(filepath.Join(dir, "streams", "0123"), 0o700)
	if got := remove(t, s, b); got != BodyProtectionUnknown {
		t.Fatalf("got %s, want protection_unknown", got)
	}
}

func TestRemoveBodySupersededByNewerIncarnation(t *testing.T) {
	s, _, clk := openBodyStore(t)
	b := []byte("cycle")
	put(t, s, b)
	expire(clk)
	remove(t, s, b)
	put(t, s, b) // incarnation 2
	expire(clk)
	old := uint64(1)
	req := removeReq(b)
	req.CompleteIncarnation = &old
	res, err := s.RemoveBody(ctx, req)
	if err != nil || res.Outcome != BodySuperseded {
		t.Fatalf("got %v %v, want superseded", res, err)
	}
	// Republish after removal, then a fresh removal succeeds.
	if got := remove(t, s, b); got != BodyRemoved {
		t.Fatalf("fresh removal got %s", got)
	}
}

func TestCorruptBodyIsConflict(t *testing.T) {
	s, dir, clk := openBodyStore(t)
	b := []byte("pristine")
	ref, _ := put(t, s, b)
	expire(clk)
	h := digestHex(ref.Digest)
	p := filepath.Join(dir, "bodies", "sha256", h[:2], h)
	os.WriteFile(p, []byte("tampered"), 0o600)
	if got := state(t, s, b); got != stateCorrupt {
		t.Fatalf("state %s, want corrupt", got)
	}
	if got := remove(t, s, b); got != BodyConflict {
		t.Fatalf("remove got %s, want body_conflict", got)
	}
	if _, _, err := s.PutBody(ctx, bytes.NewReader(b), "text/plain"); err == nil {
		t.Fatal("PutBody over a corrupt body must fail")
	}
}

func TestMissingBodyIsIntegrityAlarm(t *testing.T) {
	s, dir, clk := openBodyStore(t)
	b := []byte("vanishes")
	ref, _ := put(t, s, b)
	expire(clk)
	h := digestHex(ref.Digest)
	os.Remove(filepath.Join(dir, "bodies", "sha256", h[:2], h))
	if got := state(t, s, b); got != stateIntegrityLoss {
		t.Fatalf("state %s, want integrity_loss", got)
	}
	if got := remove(t, s, b); got != BodyMissing {
		t.Fatalf("got %s, want missing", got)
	}
}

// A crash after body_removal_started and before unlink leaves
// "removal_pending"; a retry completes it.
func TestRemoveRetryCompletesRemovalPending(t *testing.T) {
	s, _, clk := openBodyStore(t)
	b := []byte("half removed")
	put(t, s, b)
	expire(clk)
	failAt(s, "remove.tombstone_fsynced")
	_, err := s.RemoveBody(ctx, removeReq(b))
	var bi *ErrBodyIncomplete
	if !errors.As(err, &bi) || bi.State != string(stateRemovalPending) || !IsIndeterminate(err) {
		t.Fatalf("want ErrBodyIncomplete{removal_pending}, got %v", err)
	}
	if got := remove(t, s, b); got != BodyRemoved {
		t.Fatalf("retry got %s, want removed", got)
	}
}

// After body_removal_started, a PutBody of identical bytes advances the
// incarnation rather than reusing the open-removal one; a later file loss is
// "integrity_loss".
func TestPutAfterRemovalStartedAdvancesIncarnation(t *testing.T) {
	s, dir, clk := openBodyStore(t)
	b := []byte("resurrected")
	put(t, s, b)
	expire(clk)
	failAt(s, "remove.tombstone_fsynced")
	s.RemoveBody(ctx, removeReq(b))
	s.hook = nil
	put(t, s, b)
	h := digestHex(digestOf(b))
	if n, _, _ := s.readIncarnation(h); n != 2 {
		t.Fatalf("incarnation %d, want 2", n)
	}
	if got := state(t, s, b); got != stateAvailable {
		t.Fatalf("state %s, want available", got)
	}
	os.Remove(filepath.Join(dir, "bodies", "sha256", h[:2], h))
	if got := state(t, s, b); got != stateIntegrityLoss {
		t.Fatalf("state %s, want integrity_loss", got)
	}
}

// G2: With the local incarnation counter deleted or lowered, PutBody still
// appends a valid body_published(n) using the journal's high-water mark + 1.
func TestPutBodyWithDeletedOrLoweredCounterUsesJournalBound(t *testing.T) {
	s, dir, clk := openBodyStore(t)
	b := []byte("counter-resilience")
	put(t, s, b)
	expire(clk)
	if got := remove(t, s, b); got != BodyRemoved {
		t.Fatalf("remove: %s", got)
	}
	h := digestHex(digestOf(b))
	// Delete the counter file entirely; journal has bound 1, so next is 2.
	os.Remove(filepath.Join(dir, "bodies", "incarnation", h))
	put(t, s, b)
	if n, _, _ := s.readIncarnation(h); n != 2 {
		t.Fatalf("incarnation after deleted counter = %d, want 2", n)
	}
}

// A crash between rename and body_published leaves "publishing"; a PutBody
// retry commits body_published before the lease.
func TestPutRetryCompletesPublishing(t *testing.T) {
	s, _, _ := openBodyStore(t)
	b := []byte("half published")
	failAt(s, "body.dir_fsynced")
	_, _, err := s.PutBody(ctx, bytes.NewReader(b), "text/plain")
	var bi *ErrBodyIncomplete
	if !errors.As(err, &bi) || bi.State != string(statePublishing) {
		t.Fatalf("want ErrBodyIncomplete{publishing}, got %v", err)
	}
	if got := remove(t, s, b); got != BodyPending {
		t.Fatalf("remove during publishing got %s, want pending", got)
	}
	var phases []string
	s.hook = func(p string) error { phases = append(phases, p); return nil }
	put(t, s, b)
	if got := state(t, s, b); got != stateAvailable {
		t.Fatalf("state %s, want available", got)
	}
	if n, _, _ := s.readIncarnation(digestHex(digestOf(b))); n != 1 {
		t.Fatalf("incarnation %d, want 1 (publishing resumes, no advance)", n)
	}
	pub, lease := index(phases, "body.published_written"), index(phases, "body.lease_written")
	if pub < 0 || lease < 0 || pub > lease {
		t.Fatalf("phases %v: published record must precede the lease", phases)
	}
}

// A crash between incarnation and link leaves unpublished with local counter 1;
// the retry advances to incarnation 2.
func TestPublicationIncompleteRetry(t *testing.T) {
	s, _, _ := openBodyStore(t)
	b := []byte("never linked")
	failAt(s, "body.incarnation_fsynced")
	_, _, err := s.PutBody(ctx, bytes.NewReader(b), "text/plain")
	var bi *ErrBodyIncomplete
	if !errors.As(err, &bi) || bi.State != string(stateUnpublished) {
		t.Fatalf("want ErrBodyIncomplete{unpublished}, got %v", err)
	}
	if got := remove(t, s, b); got != BodyAbsent {
		t.Fatalf("remove got %s, want absent", got)
	}
	put(t, s, b)
	if n, _, _ := s.readIncarnation(digestHex(digestOf(b))); n != 2 {
		t.Fatalf("incarnation %d, want 2", n)
	}
}

// A failure before the first durable write is determinate and changes
// nothing.
func TestFailureBeforeDurableWriteIsDeterminate(t *testing.T) {
	s, _, _ := openBodyStore(t)
	b := []byte("early")
	failAt(s, "body.tmp_fsynced")
	_, _, err := s.PutBody(ctx, bytes.NewReader(b), "text/plain")
	if err == nil || IsIndeterminate(err) {
		t.Fatalf("want a determinate error, got %v", err)
	}
	if got := state(t, s, b); got != stateUnpublished {
		t.Fatalf("state %s, want unpublished", got)
	}
}

// A directory-fsync failure that persists across retries is never
// acknowledged.
func TestPersistentDirFsyncFailureNeverAcknowledged(t *testing.T) {
	s, _, clk := openBodyStore(t)
	b := []byte("stubborn")
	s.hook = func(p string) error {
		if p == "body.dir_fsynced" {
			return errors.New("injected")
		}
		return nil
	}
	for i := 0; i < 3; i++ {
		if _, _, err := s.PutBody(ctx, bytes.NewReader(b), "text/plain"); err == nil {
			t.Fatalf("attempt %d acknowledged a failing fsync", i)
		}
	}
	s.hook = nil
	put(t, s, b)
	expire(clk)
	s.hook = func(p string) error {
		if p == "remove.dir_fsynced" {
			return errors.New("injected")
		}
		return nil
	}
	for i := 0; i < 3; i++ {
		if _, err := s.RemoveBody(ctx, removeReq(b)); err == nil {
			t.Fatalf("removal attempt %d acknowledged a failing fsync", i)
		}
	}
	s.hook = nil
	if got := remove(t, s, b); got != BodyAbsent {
		t.Fatalf("got %s, want absent after completing", got)
	}
}

// G7, G9: A missing or damaged designated body-removal stream returns
// determinate ErrJournalInvalid on body operations.
func TestDamagedOrMissingDesignatedStreamReturnsErrJournalInvalid(t *testing.T) {
	s, dir, _ := openBodyStore(t)
	b := []byte("desig-check")
	jpath := filepath.Join(dir, "streams", streamHash(DefaultBodyRemovalStream), "journal.ndjson")
	os.Remove(jpath)

	var ji *ErrJournalInvalid
	if _, _, err := s.PutBody(ctx, bytes.NewReader(b), "text/plain"); !errors.As(err, &ji) || IsIndeterminate(err) {
		t.Fatalf("PutBody with missing designated stream: want determinate ErrJournalInvalid, got %v", err)
	}
	if _, err := s.RemoveBody(ctx, removeReq(b)); !errors.As(err, &ji) || IsIndeterminate(err) {
		t.Fatalf("RemoveBody with missing designated stream: want determinate ErrJournalInvalid, got %v", err)
	}
}

// G4f: Reason never changes eligibility across retention, operator, and rights_restricted.
func TestRemoveBodyReasonDoesNotChangeEligibility(t *testing.T) {
	for _, reason := range []RemoveBodyReason{RemoveReasonRetention, RemoveReasonOperator, RemoveReasonRightsRestricted} {
		s, _, clk := openBodyStore(t)
		b := []byte("reason-test")
		put(t, s, b)
		req := removeReq(b)
		req.Reason = reason
		res, err := s.RemoveBody(ctx, req)
		if err != nil || res.Outcome != BodyPending {
			t.Fatalf("reason %s while leased: got %v %v", reason, res, err)
		}
		expire(clk)
		res, err = s.RemoveBody(ctx, req)
		if err != nil || res.Outcome != BodyRemoved {
			t.Fatalf("reason %s after lease expiry: got %v %v", reason, res, err)
		}
	}
}

func TestRemoveBodyValidatesRequest(t *testing.T) {
	s, _, _ := openBodyStore(t)
	for _, req := range []RemoveBodyRequest{
		{Digest: "sha256:XYZ", Length: 1, Reason: RemoveReasonRetention, Provenance: testProv},
		{Digest: digestOf(nil), Length: -1, Reason: RemoveReasonRetention, Provenance: testProv},
		{Digest: digestOf(nil), Length: 0, Reason: "unknown", Provenance: testProv},
		{Digest: digestOf(nil), Length: 0, Reason: RemoveReasonRetention, Provenance: Provenance{ActorKind: "invalid", Actor: "x"}},
		{Digest: digestOf(nil), Length: 0, Reason: RemoveReasonRetention, Provenance: Provenance{ActorKind: "process", Actor: ""}},
	} {
		if _, err := s.RemoveBody(ctx, req); !errors.Is(err, ErrSchema) {
			t.Errorf("%+v: want ErrSchema, got %v", req, err)
		}
	}
}

func TestConcurrentProducersInProcess(t *testing.T) {
	s, _, _ := openBodyStore(t)
	b := []byte("contended")
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		go func() {
			_, _, err := s.PutBody(ctx, bytes.NewReader(b), "text/plain")
			errs <- err
		}()
	}
	for i := 0; i < 8; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("producer: %v", err)
		}
	}
	if got := state(t, s, b); got != stateAvailable {
		t.Fatalf("state %s", got)
	}
}

func index(xs []string, x string) int {
	for i, v := range xs {
		if v == x {
			return i
		}
	}
	return -1
}

// setIncarnation durably writes the incarnation record for b.
func setIncarnation(t *testing.T, s *Store, b []byte, n uint64) {
	t.Helper()
	err := s.withLock(func() error {
		if err := s.ensureBodyDirs(); err != nil {
			return err
		}
		return s.writeIncarnation(digestHex(digestOf(b)), n)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// RemoveBody refuses every integer outside the durable-record domain and
// accepts the ceiling itself.
func TestRemoveBodySafeIntegerDomain(t *testing.T) {
	s, _, _ := openBodyStore(t)
	d := digestOf([]byte("domain"))
	zero, max, over := uint64(0), uint64(maxSafeInteger), uint64(maxSafeInteger+1)
	for name, req := range map[string]RemoveBodyRequest{
		"length over":               {Digest: d, Length: maxSafeInteger + 1, Reason: RemoveReasonRetention, Provenance: testProv},
		"length max int64":          {Digest: d, Length: 1<<63 - 1, Reason: RemoveReasonRetention, Provenance: testProv},
		"complete incarnation 0":    {Digest: d, Length: 6, CompleteIncarnation: &zero, Reason: RemoveReasonRetention, Provenance: testProv},
		"complete incarnation over": {Digest: d, Length: 6, CompleteIncarnation: &over, Reason: RemoveReasonRetention, Provenance: testProv},
	} {
		if _, err := s.RemoveBody(ctx, req); !errors.Is(err, ErrSchema) || IsIndeterminate(err) {
			t.Errorf("%s: want determinate ErrSchema, got %v", name, err)
		}
	}
	for name, req := range map[string]RemoveBodyRequest{
		"length max":               {Digest: d, Length: maxSafeInteger, Reason: RemoveReasonRetention, Provenance: testProv},
		"complete incarnation max": {Digest: d, Length: 6, CompleteIncarnation: &max, Reason: RemoveReasonRetention, Provenance: testProv},
	} {
		if _, err := s.RemoveBody(ctx, req); err != nil {
			t.Errorf("%s: want a result, got %v", name, err)
		}
	}
}

// An interrupted publication that already holds the last incarnation leaves
// nothing to publish into: PutBody refuses before any durable write.
func TestPutBodyRefusesIncarnationExhaustion(t *testing.T) {
	s, _, _ := openBodyStore(t)
	b := []byte("exhausted")
	setIncarnation(t, s, b, maxSafeInteger)
	if got := state(t, s, b); got != stateUnpublished {
		t.Fatalf("setup state %s", got)
	}
	_, _, err := s.PutBody(ctx, bytes.NewReader(b), "text/plain")
	if !errors.Is(err, ErrSchema) || IsIndeterminate(err) {
		t.Fatalf("want determinate ErrSchema, got %v", err)
	}
	if n, _, _ := s.readIncarnation(digestHex(digestOf(b))); n != maxSafeInteger {
		t.Fatalf("incarnation %d, want unchanged %d", n, uint64(maxSafeInteger))
	}
	if got := state(t, s, b); got != stateUnpublished {
		t.Fatalf("state %s, want unchanged", got)
	}
	if names, _ := s.root.ReadDirNames(dirBodyTmp, s.fso); len(names) != 0 {
		t.Fatalf("staged files left: %v", names)
	}
}

// The last incarnation is usable: a publication may take it, and a live body
// at the ceiling republishes without an increment.
func TestPutBodyUsesLastIncarnation(t *testing.T) {
	s, _, _ := openBodyStore(t)
	b := []byte("last")
	setIncarnation(t, s, b, maxSafeInteger-1)
	put(t, s, b)
	if n, _, _ := s.readIncarnation(digestHex(digestOf(b))); n != maxSafeInteger {
		t.Fatalf("incarnation %d, want %d", n, uint64(maxSafeInteger))
	}
	put(t, s, b)
	if got := state(t, s, b); got != stateAvailable {
		t.Fatalf("state %s, want available", got)
	}
	if names, _ := s.root.ReadDirNames(dirBodyTmp, s.fso); len(names) != 0 {
		t.Fatalf("republishing a live body left staged files: %v", names)
	}
}

// An incarnation record above the ceiling is not something this store
// writes: the store is invalid.
func TestIncarnationAboveCeilingIsInvalidStore(t *testing.T) {
	s, _, _ := openBodyStore(t)
	b := []byte("tampered")
	setIncarnation(t, s, b, maxSafeInteger+1)
	if _, _, err := s.PutBody(ctx, bytes.NewReader(b), "text/plain"); !errors.Is(err, ErrStoreInvalid) {
		t.Fatalf("PutBody: want ErrStoreInvalid, got %v", err)
	}
	if _, err := s.RemoveBody(ctx, removeReq(b)); !errors.Is(err, ErrStoreInvalid) {
		t.Fatalf("RemoveBody: want ErrStoreInvalid, got %v", err)
	}
}

// A body longer than the ceiling is refused while staging and leaves nothing.
func TestPutBodyRefusesLengthAboveCeiling(t *testing.T) {
	s, _, _ := openBodyStore(t)
	old := maxBodyLength
	maxBodyLength = 4
	t.Cleanup(func() { maxBodyLength = old })
	if _, _, err := s.PutBody(ctx, bytes.NewReader([]byte("1234")), "text/plain"); err != nil {
		t.Fatalf("at the ceiling: %v", err)
	}
	b := []byte("12345")
	_, _, err := s.PutBody(ctx, bytes.NewReader(b), "text/plain")
	if !errors.Is(err, ErrSchema) || IsIndeterminate(err) {
		t.Fatalf("want determinate ErrSchema, got %v", err)
	}
	if got := state(t, s, b); got != stateUnpublished {
		t.Fatalf("state %s, want unpublished", got)
	}
	if names, _ := s.root.ReadDirNames(dirBodyTmp, s.fso); len(names) != 0 {
		t.Fatalf("staged files left: %v", names)
	}
}

// panicReader returns n bytes and then panics.
type panicReader struct{ n int }

func (r *panicReader) Read(p []byte) (int, error) {
	if r.n <= 0 {
		panic("reader panic")
	}
	k := min(len(p), r.n)
	for i := range p[:k] {
		p[i] = 'x'
	}
	r.n -= k
	return k, nil
}

// putRecover runs PutBody and returns the value it panicked with, if any.
func putRecover(s *Store, r io.Reader) (v any) {
	defer func() { v = recover() }()
	s.PutBody(ctx, r, "application/octet-stream")
	return nil
}

// A panic while staging or under the lock propagates unchanged, leaves no
// staged file behind, and leaves the store usable.
func TestPutBodyPanicRemovesStagedFile(t *testing.T) {
	b := []byte("panic body")
	panicAt := func(phase string) func(s *Store) {
		return func(s *Store) {
			s.hook = func(p string) error {
				if p == phase {
					panic("hook panic")
				}
				return nil
			}
		}
	}
	cases := []struct {
		name  string
		setup func(s *Store)
		input func() io.Reader
		want  any
	}{
		{"reader after a full buffer", nil, func() io.Reader { return &panicReader{n: 100 << 10} }, "reader panic"},
		{"tmp_written", panicAt("body.tmp_written"), func() io.Reader { return bytes.NewReader(b) }, "hook panic"},
		{"tmp_fsynced, file closed", panicAt("body.tmp_fsynced"), func() io.Reader { return bytes.NewReader(b) }, "hook panic"},
		{"under the lock, before link", panicAt("body.incarnation_written"), func() io.Reader { return bytes.NewReader(b) }, "hook panic"},
		{"under the lock, after link", panicAt("body.renamed"), func() io.Reader { return bytes.NewReader(b) }, "hook panic"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, _, _ := openBodyStore(t)
			if c.setup != nil {
				c.setup(s)
			}
			if got := putRecover(s, c.input()); got != c.want {
				t.Fatalf("panic value %v, want %v", got, c.want)
			}
			s.hook = nil
			names, err := s.root.ReadDirNames(dirBodyTmp, s.fso)
			if err != nil {
				t.Fatal(err)
			}
			if len(names) != 0 {
				t.Fatalf("staged files left after panic: %v", names)
			}
			put(t, s, b)
			if got := state(t, s, b); got != stateAvailable {
				t.Fatalf("state %s after retry, want available", got)
			}
		})
	}
}

func TestRemoveBodyRefusesInvalidUTF8Provenance(t *testing.T) {
	s, _, clk := openBodyStore(t)
	b := []byte("orphan")
	put(t, s, b)
	expire(clk)
	req := removeReq(b)
	req.Provenance.Actor = string([]byte{'x', 0xff})
	res, err := s.RemoveBody(ctx, req)
	if !errors.Is(err, ErrSchema) || IsIndeterminate(err) {
		t.Fatalf("RemoveBody with invalid UTF-8 provenance: res=%+v err=%v, want determinate ErrSchema", res, err)
	}
	if _, _, err := s.PutBody(ctx, bytes.NewReader([]byte("different")), "text/plain"); err != nil {
		t.Fatalf("subsequent PutBody after rejected RemoveBody failed: %v", err)
	}
}
