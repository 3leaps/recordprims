package journal

import (
	"math/big"
	"regexp"
	"strconv"
	"time"

	"github.com/3leaps/recordprims/internal/wire"
)

// lifecycle are the body lifecycle entry types, which appear only on the
// store's designated body-removal stream.
var lifecycle = map[string]bool{
	"body_published": true, "body_removal_started": true, "body_removed": true,
}

// Pin identifies one revision of one record on a stream.
type Pin struct {
	Stream   string
	RecordID string
	Revision int64
	Digest   string
}

type keyIdentity struct {
	kind  string // "content", "protection", or "retirement"
	value string // entry digest or request digest
}

type liveEntry struct {
	id       keyIdentity
	at       time.Time
	recordID string
	receipt  *wire.Node
}

// State replays one v1 stream entry by entry, in the reference interpreter's
// order.
type State struct {
	window        int64
	journal       []*wire.Node
	stream        string
	failureModel  string
	records       map[Pin]*wire.Node
	keys          map[string]liveEntry
	retired       map[string]bool
	removalStream string
	hasRemoval    bool
	partial       bool
}

var integerToken = regexp.MustCompile(`^-?(0|[1-9][0-9]*)$`)

// New starts a stream replay with the idempotency window in milliseconds,
// given as the JSON number it was read from. The window must be a positive
// integer written without fraction or exponent (else idempotency_window) and
// a safe integer (else numeric_domain).
func New(window *wire.Node) (*State, error) {
	if window == nil || window.Kind != wire.Number || !integerToken.MatchString(window.Token) || window.Token[0] == '-' || window.Token == "0" {
		return nil, reject(ReasonIdempotencyWindow, "the idempotency window is not a positive integer")
	}
	v, ok := new(big.Int).SetString(window.Token, 10)
	if !ok || v.Cmp(big.NewInt(wire.MaxSafeInteger)) > 0 {
		return nil, reject(wire.ReasonNumericDomain, "the idempotency window exceeds %d", int64(wire.MaxSafeInteger))
	}
	return &State{
		window:  v.Int64(),
		records: map[Pin]*wire.Node{},
		keys:    map[string]liveEntry{},
		retired: map[string]bool{},
	}, nil
}

// SetRemovalStream names the store's designated body-removal stream, from
// the profile's runtime.body_removal_stream.
func (s *State) SetRemovalStream(name string) {
	s.removalStream, s.hasRemoval = name, true
}

// Partial reports whether an ingested entry needed semantics this package
// does not implement yet (content, protection, retirement, body lifecycle,
// continuation or recovery processing). A State that is partial has not
// checked the stream completely. Partial is set by the entry itself (its
// type, or a continues or recovery on stream_opened), whether or not its
// group has closed, and an open group does not set it: the journal invariants
// run only when an entry closes its group, so a State that is not partial may
// still hold an open last group it has not checked. The caller runs
// CheckJournal on the whole journal for that. Schema constraints (group_size
// at least 1, required members such as failure_model) belong to the shape
// stage and are not rechecked here.
func (s *State) Partial() bool { return s.partial }

// Len is the number of entries ingested.
func (s *State) Len() int { return len(s.journal) }

// Journal returns the entries ingested, in order. The caller must not modify
// them.
func (s *State) Journal() []*wire.Node { return s.journal }

// Records returns a shallow copy of the stream's accepted record revisions
// keyed by Pin. The caller must not modify the returned *wire.Node values.
func (s *State) Records() map[Pin]*wire.Node {
	out := make(map[Pin]*wire.Node, len(s.records))
	for k, v := range s.records {
		out[k] = v
	}
	return out
}

func (s *State) remember(key string, id keyIdentity, at time.Time, recordID string, receipt *wire.Node) {
	s.keys[key] = liveEntry{
		id:       id,
		at:       at,
		recordID: recordID,
		receipt:  cloneNode(receipt),
	}
}

func (s *State) live(key string, at time.Time) (liveEntry, bool) {
	prior, ok := s.keys[key]
	if !ok {
		return liveEntry{}, false
	}
	delta := at.UnixMicro() - prior.at.UnixMicro()
	if delta >= 0 && delta < s.window*1000 {
		return prior, true
	}
	return liveEntry{}, false
}

func pinOf(e *wire.Node, where string) (Pin, error) {
	rec := e.Get("record")
	if rec == nil || rec.Kind != wire.Object {
		return Pin{}, reject(ReasonShape, "%s: record is not an object", where)
	}
	stream, err := strOf(rec, "stream")
	if err != nil {
		return Pin{}, err
	}
	rid, err := strOf(rec, "record_id")
	if err != nil {
		return Pin{}, err
	}
	rev, err := intOf(rec, "revision")
	if err != nil {
		return Pin{}, err
	}
	dig, err := strOf(e, "digest")
	if err != nil {
		return Pin{}, err
	}
	return Pin{Stream: stream, RecordID: rid, Revision: rev, Digest: dig}, nil
}

func pinNode(p Pin) *wire.Node {
	return &wire.Node{
		Kind: wire.Object,
		Fields: []wire.Member{
			{Name: "stream", Value: strNode(p.Stream)},
			{Name: "record_id", Value: strNode(p.RecordID)},
			{Name: "revision", Value: intNode(p.Revision)},
			{Name: "digest", Value: strNode(p.Digest)},
		},
	}
}

func contentReceipt(stream, key, outcome string, p Pin, seq int64, failureModel, bodyStatus string) *wire.Node {
	fields := []wire.Member{
		{Name: "v", Value: intNode(1)},
		{Name: "kind", Value: strNode("receipt")},
		{Name: "stream", Value: strNode(stream)},
		{Name: "idempotency_key", Value: strNode(key)},
		{Name: "outcome", Value: strNode(outcome)},
		{Name: "pin", Value: pinNode(p)},
		{Name: "seq", Value: intNode(seq)},
		{Name: "failure_model", Value: strNode(failureModel)},
	}
	if bodyStatus != "" {
		fields = append(fields, wire.Member{Name: "body_status", Value: strNode(bodyStatus)})
	}
	return &wire.Node{Kind: wire.Object, Fields: fields}
}

func retirementRequest(e *wire.Node) *wire.Node {
	var fields []wire.Member
	for _, k := range []string{"stream", "record_id", "reason", "idempotency_key"} {
		if v := e.Get(k); v != nil {
			fields = append(fields, wire.Member{Name: k, Value: cloneNode(v)})
		}
	}
	return &wire.Node{Kind: wire.Object, Fields: fields}
}

func (s *State) protectionReceipt(r *wire.Node, seq int64) (*wire.Node, error) {
	if r == nil || r.Kind != wire.Object {
		return nil, reject(ReasonShape, "protection request is not an object")
	}
	stream, err := strOf(r, "stream")
	if err != nil {
		return nil, err
	}
	key, err := strOf(r, "idempotency_key")
	if err != nil {
		return nil, err
	}
	action, err := strOf(r, "action")
	if err != nil {
		return nil, err
	}
	outcome := "untombstoned"
	if action == "tombstone" {
		outcome = "tombstoned"
	}
	rdig, err := recordDigest(r)
	if err != nil {
		return nil, err
	}
	return &wire.Node{
		Kind: wire.Object,
		Fields: []wire.Member{
			{Name: "v", Value: intNode(1)},
			{Name: "stream", Value: strNode(stream)},
			{Name: "idempotency_key", Value: strNode(key)},
			{Name: "outcome", Value: strNode(outcome)},
			{Name: "seq", Value: intNode(seq)},
			{Name: "request_digest", Value: strNode(rdig)},
			{Name: "failure_model", Value: strNode(s.failureModel)},
		},
	}, nil
}

func (s *State) retirementReceipt(r *wire.Node, seq int64) (*wire.Node, error) {
	if r == nil || r.Kind != wire.Object {
		return nil, reject(ReasonShape, "retirement request is not an object")
	}
	stream, err := strOf(r, "stream")
	if err != nil {
		return nil, err
	}
	key, err := strOf(r, "idempotency_key")
	if err != nil {
		return nil, err
	}
	rid, err := strOf(r, "record_id")
	if err != nil {
		return nil, err
	}
	rdig, err := recordDigest(r)
	if err != nil {
		return nil, err
	}
	return &wire.Node{
		Kind: wire.Object,
		Fields: []wire.Member{
			{Name: "v", Value: intNode(1)},
			{Name: "kind", Value: strNode("receipt")},
			{Name: "stream", Value: strNode(stream)},
			{Name: "idempotency_key", Value: strNode(key)},
			{Name: "outcome", Value: strNode("retired")},
			{Name: "seq", Value: intNode(seq)},
			{Name: "record_id", Value: strNode(rid)},
			{Name: "request_digest", Value: strNode(rdig)},
			{Name: "failure_model", Value: strNode(s.failureModel)},
		},
	}, nil
}

// Ingest checks one entry against the stream so far: the group span, the
// journal and record versions, every instant, the journal invariants when
// the entry closes its group, nondecreasing time, and the placement of body
// lifecycle entries. The entry must have passed the wire stages.
func (s *State) Ingest(e *wire.Node) error {
	where := "entry"
	if seq, err := intOf(e, "seq"); err == nil {
		where = "entry " + strconv.FormatInt(seq, 10)
	}
	end, err := closingSeq(e, where)
	if err != nil {
		return err
	}
	if v, err := intOf(e, "v"); err != nil || v != 1 {
		return reject(ReasonJournalVersion, "%s: not a v1 journal entry", where)
	}
	atStr, err := strOf(e, "at")
	if err != nil {
		return err
	}
	at, err := instantOf(atStr, where+" at")
	if err != nil {
		return err
	}
	if rec := e.Get("record"); rec != nil {
		if v, err := intOf(rec, "v"); err != nil || v != 1 {
			return reject(ReasonRecordVersion, "%s: the record is not a v1 record", where)
		}
		if pa := rec.Get("produced_at"); pa != nil {
			ps, err := strOf(pa, "instant")
			if err != nil {
				return err
			}
			if _, err := instantOf(ps, where+" record.produced_at.instant"); err != nil {
				return err
			}
		}
	}
	if req := e.Get("request"); req != nil {
		obs, err := strOf(req, "observed_at")
		if err != nil {
			return err
		}
		if _, err := instantOf(obs, where+" request.observed_at"); err != nil {
			return err
		}
	}
	// Complete groups are checked as a journal; a partial group cannot be
	// observed by a request.
	seq, err := intOf(e, "seq")
	if err != nil {
		return err
	}
	if seq == end {
		next := append(append([]*wire.Node(nil), s.journal...), e)
		if err := CheckJournal(next); err != nil {
			return err
		}
	}
	if n := len(s.journal); n > 0 {
		prev, _ := ParseInstant(s.journal[n-1].Get("at").Str)
		if at.Before(prev) {
			return reject(ReasonJournalTime, "%s: at is earlier than the previous entry", where)
		}
	}
	typ, err := strOf(e, "type")
	if err != nil {
		return err
	}
	if s.hasRemoval || lifecycle[typ] {
		// The designated stream carries only stream_opened and lifecycle
		// entries, and lifecycle entries appear nowhere else.
		stream, err := strOf(e, "stream")
		if err != nil {
			return err
		}
		if s.hasRemoval && stream == s.removalStream {
			if !(lifecycle[typ] || typ == "stream_opened") || e.Get("continues") != nil {
				return reject(ReasonBodyRemovalStream, "%s: only stream_opened and body lifecycle entries belong on the body-removal stream", where)
			}
		} else if lifecycle[typ] {
			return reject(ReasonBodyRemovalStream, "%s: a body lifecycle entry outside the body-removal stream", where)
		}
	}
	switch {
	case typ == "stream_opened":
		stream, err := strOf(e, "stream")
		if err != nil {
			return err
		}
		fm, err := strOf(e, "failure_model")
		if err != nil {
			return err
		}
		s.stream, s.failureModel = stream, fm
		if e.Get("continues") != nil || e.Get("recovery") != nil {
			s.partial = true
		}
	case typ == "appended" || typ == "revised":
		p, err := pinOf(e, where)
		if err != nil {
			return err
		}
		if s.retired[p.RecordID] {
			return reject(ReasonRetiredRecord, "%s: record %q is retired", where, p.RecordID)
		}
		if present(e, "imported_from") {
			s.partial = true
		}
		key, err := strOf(e, "idempotency_key")
		if err != nil {
			return err
		}
		if _, ok := s.live(key, at); ok {
			return reject(ReasonAcceptedKeyReuse, "%s: idempotency key %q is still live", where, key)
		}
		var bodyStatus string
		if present(e, "body_status") {
			if bodyStatus, err = strOf(e, "body_status"); err != nil {
				return err
			}
		}
		s.records[p] = e.Get("record")
		rcpt := contentReceipt(s.stream, key, typ, p, seq, s.failureModel, bodyStatus)
		s.remember(key, keyIdentity{kind: "content", value: p.Digest}, at, p.RecordID, rcpt)
	case typ == "deduplicated":
		key, err := strOf(e, "idempotency_key")
		if err != nil {
			return err
		}
		rid, err := strOf(e, "record_id")
		if err != nil {
			return err
		}
		prior, ok := s.live(key, at)
		if !ok || prior.id.kind != "content" || prior.recordID != rid {
			return reject(ReasonInvalidDeduplication, "%s: key %q does not name a live content write of record %q", where, key, rid)
		}
	case typ == "conflicted":
	default:
		s.partial = true
	}
	s.journal = append(s.journal, e)
	return nil
}

// Reconcile looks up idempotencyKey at decision time atStr for an attempt at
// attemptedAtStr, returning the reconciliation write-receipt object. The
// journal so far must be complete, attemptedAtStr and atStr must be canonical
// v1 instants, and atStr must not precede the last journal entry.
func (s *State) Reconcile(idempotencyKey, atStr, attemptedAtStr string) (*wire.Node, error) {
	if err := CheckJournal(s.journal); err != nil {
		return nil, err
	}
	if _, err := instantOf(attemptedAtStr, "reconcile attempted_at"); err != nil {
		return nil, err
	}
	at, err := instantOf(atStr, "reconcile at")
	if err != nil {
		return nil, err
	}
	lastAt, _ := ParseInstant(s.journal[len(s.journal)-1].Get("at").Str)
	if at.Before(lastAt) {
		return nil, reject(ReasonJournalTime, "reconcile: at precedes the journal")
	}
	fields := []wire.Member{
		{Name: "v", Value: intNode(1)},
		{Name: "kind", Value: strNode("reconciliation")},
		{Name: "stream", Value: strNode(s.stream)},
		{Name: "idempotency_key", Value: strNode(idempotencyKey)},
		{Name: "attempted_at", Value: strNode(attemptedAtStr)},
	}
	if prior, ok := s.live(idempotencyKey, at); ok {
		fields = append(fields,
			wire.Member{Name: "result", Value: strNode("found")},
			wire.Member{Name: "receipt", Value: cloneNode(prior.receipt)},
		)
	} else {
		fields = append(fields,
			wire.Member{Name: "result", Value: strNode("unknown")},
			wire.Member{Name: "reason", Value: strNode("coverage_unprovable")},
		)
	}
	return &wire.Node{Kind: wire.Object, Fields: fields}, nil
}

// VerifyReceipt checks a received receipt or reconciliation document against
// the stream's journal, matching the reference interpreter's verify_receipt
// and verify_reconciliation methods.
func (s *State) VerifyReceipt(received *wire.Node) (*wire.Node, error) {
	if received == nil || received.Kind != wire.Object {
		return nil, reject(ReasonShape, "receipt: not an object")
	}
	if k := received.Get("kind"); k != nil && k.Kind == wire.String && k.Str == "reconciliation" {
		return s.verifyReconciliation(received)
	}
	outcome, err := strOf(received, "outcome")
	if err != nil {
		return nil, err
	}
	if outcome != "tombstoned" && outcome != "untombstoned" && outcome != "retired" {
		return nil, reject(ReasonReceiptMismatch, "receipt: outcome %q is not an accepted protection or retirement receipt", outcome)
	}
	if v, err := intOf(received, "v"); err != nil || v != 1 {
		return nil, reject(ReasonReceiptMismatch, "receipt: v is not 1")
	}
	seq, err := intOf(received, "seq")
	if err != nil {
		return nil, err
	}
	if seq < 1 || seq > int64(len(s.journal)) {
		return nil, reject(ReasonReceiptMismatch, "receipt: seq %d is out of range", seq)
	}
	entry := s.journal[seq-1]
	etyp, err := strOf(entry, "type")
	if err != nil {
		return nil, err
	}
	var expected *wire.Node
	if outcome == "retired" {
		if etyp != "retired" || !present(entry, "idempotency_key") {
			return nil, reject(ReasonReceiptMismatch, "receipt: seq %d is not a keyed retirement", seq)
		}
		expected, err = s.retirementReceipt(retirementRequest(entry), seq)
		if err != nil {
			return nil, err
		}
	} else {
		if etyp != "tombstoned" && etyp != "untombstoned" {
			return nil, reject(ReasonReceiptMismatch, "receipt: seq %d is not a protection entry", seq)
		}
		expected, err = s.protectionReceipt(entry.Get("request"), seq)
		if err != nil {
			return nil, err
		}
	}
	if !Equal(received, expected) {
		return nil, reject(ReasonReceiptMismatch, "receipt: does not match journal entry %d", seq)
	}
	return &wire.Node{
		Kind:   wire.Object,
		Fields: []wire.Member{{Name: "outcome", Value: strNode("receipt_verified")}},
	}, nil
}

func (s *State) verifyReconciliation(received *wire.Node) (*wire.Node, error) {
	att, err := strOf(received, "attempted_at")
	if err != nil {
		return nil, err
	}
	if _, err := instantOf(att, "reconciliation attempted_at"); err != nil {
		return nil, err
	}
	if present(received, "window_covers_from") {
		from, err := strOf(received, "window_covers_from")
		if err != nil {
			return nil, err
		}
		if _, err := instantOf(from, "reconciliation window_covers_from"); err != nil {
			return nil, err
		}
	}
	if present(received, "window_covers_until") {
		until, err := strOf(received, "window_covers_until")
		if err != nil {
			return nil, err
		}
		if _, err := instantOf(until, "reconciliation window_covers_until"); err != nil {
			return nil, err
		}
	}
	return &wire.Node{
		Kind:   wire.Object,
		Fields: []wire.Member{{Name: "outcome", Value: strNode("receipt_verified")}},
	}, nil
}
