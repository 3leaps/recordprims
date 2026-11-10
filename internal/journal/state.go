package journal

import (
	"math/big"
	"regexp"
	"slices"
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
	window                  int64
	journal                 []*wire.Node
	stream                  string
	failureModel            string
	records                 map[Pin]*wire.Node
	holds                   map[int64]*wire.Node
	active                  map[int64]bool
	keys                    map[string]liveEntry
	retired                 map[string]bool
	restrictedAt            []int64
	source                  []*wire.Node
	continues               *wire.Node
	imports                 []importItem
	review                  *wire.Node
	upstream                *State
	upstreamUnobserved      bool
	upstreamUncurrentSealed bool
	sourceSealFinal         bool
	sealedAncestorHold      bool
	removalStream           string
	hasRemoval              bool
	incarnationAllocation   string
	quarantinedDesignated   []string
	expectedQuarantined     []string
	referencedBodies        map[BodyKey]bool
	bodies                  map[BodyKey]bodyState
	openStarts              map[BodyKey]map[int64]bool
	partial                 bool
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
		window:           v.Int64(),
		records:          map[Pin]*wire.Node{},
		holds:            map[int64]*wire.Node{},
		active:           map[int64]bool{},
		keys:             map[string]liveEntry{},
		retired:          map[string]bool{},
		referencedBodies: map[BodyKey]bool{},
		bodies:           map[BodyKey]bodyState{},
		openStarts:       map[BodyKey]map[int64]bool{},
	}, nil
}

// SetRemovalStream names the store's designated body-removal stream, from
// the profile's runtime.body_removal_stream.
func (s *State) SetRemovalStream(name string) {
	s.removalStream, s.hasRemoval = name, true
}

// SetProfile configures runtime fields from a validated stratum profile
// (body_removal_stream, incarnation_allocation, quarantined_designated_streams).
func (s *State) SetProfile(prof *wire.Node) {
	if prof == nil || prof.Kind != wire.Object {
		return
	}
	rt := prof.Get("runtime")
	if rt == nil || rt.Kind != wire.Object {
		return
	}
	if rs := rt.Get("body_removal_stream"); rs != nil && rs.Kind == wire.String {
		s.SetRemovalStream(rs.Str)
	}
	if ia := rt.Get("incarnation_allocation"); ia != nil && ia.Kind == wire.String {
		s.incarnationAllocation = ia.Str
	}
	if qds := rt.Get("quarantined_designated_streams"); qds != nil && qds.Kind == wire.Array {
		s.quarantinedDesignated = make([]string, 0, len(qds.Elems))
		for _, el := range qds.Elems {
			if el != nil && el.Kind == wire.String {
				s.quarantinedDesignated = append(s.quarantinedDesignated, el.Str)
			}
		}
	}
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

// ActiveHolds returns the sequence numbers of active protection holds in
// ascending order.
func (s *State) ActiveHolds() []int64 {
	out := make([]int64, 0, len(s.active))
	for seq := range s.active {
		out = append(out, seq)
	}
	slices.Sort(out)
	return out
}

// LiveKey reports whether key is live in the stream's idempotency window at at,
// returning its operation kind ("content", "protection", or "retirement"),
// identity value (entry digest or request digest), record_id, and stored receipt.
func (s *State) LiveKey(key string, at time.Time) (kind, value, recordID string, receipt *wire.Node, ok bool) {
	prior, live := s.live(key, at)
	if !live {
		return "", "", "", nil, false
	}
	return prior.id.kind, prior.id.value, prior.recordID, cloneNode(prior.receipt), true
}

// HeadPin returns the highest accepted revision Pin for recordID on the stream.
func (s *State) HeadPin(recordID string) (Pin, bool) {
	var best Pin
	found := false
	for p := range s.records {
		if p.RecordID == recordID && (!found || p.Revision > best.Revision) {
			best = p
			found = true
		}
	}
	return best, found
}

// FirstRetired returns the seq and at timestamp of the first retirement entry
// for recordID on the stream.
func (s *State) FirstRetired(recordID string) (seq int64, at string, ok bool) {
	if !s.retired[recordID] {
		return 0, "", false
	}
	for _, e := range s.journal {
		if typ, _ := strOf(e, "type"); typ == "retired" {
			if rid, _ := strOf(e, "record_id"); rid == recordID {
				sq, _ := intOf(e, "seq")
				atStr, _ := strOf(e, "at")
				return sq, atStr, true
			}
		}
	}
	return 0, "", false
}

// CutDigest computes the RFC 8785 JCS sha256 digest over entries 1..cutSeq.
func (s *State) CutDigest(cutSeq int64) (string, error) {
	if cutSeq < 1 || cutSeq > int64(len(s.journal)) {
		return "", reject(ReasonContinuation, "cut_seq %d out of range", cutSeq)
	}
	return sliceDigest(s.journal[:cutSeq])
}

func (s *State) hasRecordID(recordID string) bool {
	for p := range s.records {
		if p.RecordID == recordID {
			return true
		}
	}
	return false
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

func pinFromNode(n *wire.Node, where string) (Pin, error) {
	if n == nil || n.Kind != wire.Object {
		return Pin{}, reject(ReasonShape, "%s is not an object", where)
	}
	stream, err := strOf(n, "stream")
	if err != nil {
		return Pin{}, err
	}
	rid, err := strOf(n, "record_id")
	if err != nil {
		return Pin{}, err
	}
	rev, err := intOf(n, "revision")
	if err != nil {
		return Pin{}, err
	}
	dig, err := strOf(n, "digest")
	if err != nil {
		return Pin{}, err
	}
	return Pin{Stream: stream, RecordID: rid, Revision: rev, Digest: dig}, nil
}

func bodyMatches(recBody, targetBody *wire.Node) (bool, error) {
	if recBody == nil || recBody.Kind != wire.Object {
		return false, nil
	}
	if targetBody == nil || targetBody.Kind != wire.Object {
		return false, reject(ReasonShape, "body is not an object")
	}
	d1, err := strOf(recBody, "digest")
	if err != nil {
		return false, err
	}
	l1, err := intOf(recBody, "length")
	if err != nil {
		return false, err
	}
	d2, err := strOf(targetBody, "digest")
	if err != nil {
		return false, err
	}
	l2, err := intOf(targetBody, "length")
	if err != nil {
		return false, err
	}
	return d1 == d2 && l1 == l2, nil
}

func (s *State) refusal(r *wire.Node, at time.Time) (string, error) {
	if r == nil || r.Kind != wire.Object {
		return "", reject(ReasonShape, "protection request is not an object")
	}
	rStream, err := strOf(r, "stream")
	if err != nil {
		return "", err
	}
	targetPin, err := pinFromNode(r.Get("target"), "request.target")
	if err != nil {
		return "", err
	}
	if rStream != s.stream || targetPin.Stream != s.stream {
		return ReasonTargetNotFound, nil
	}
	record, ok := s.records[targetPin]
	if !ok {
		return ReasonTargetNotFound, nil
	}
	scope, err := strOf(r, "scope")
	if err != nil {
		return "", err
	}
	if scope == "body" {
		match, err := bodyMatches(record.Get("body"), r.Get("body"))
		if err != nil {
			return "", err
		}
		if !match {
			return ReasonBodyMismatch, nil
		}
	}
	obsStr, err := strOf(r, "observed_at")
	if err != nil {
		return "", err
	}
	obsAt, err := instantOf(obsStr, "request.observed_at")
	if err != nil {
		return "", err
	}
	if obsAt.After(at) {
		return ReasonObservationOrder, nil
	}
	action, err := strOf(r, "action")
	if err != nil {
		return "", err
	}
	if action == "restore" {
		restores, err := intOf(r, "restores")
		if err != nil {
			return "", err
		}
		if !s.active[restores] {
			return ReasonHoldNotActive, nil
		}
		hold := s.holds[restores]
		if !Equal(r.Get("target"), hold.Get("target")) ||
			!Equal(r.Get("scope"), hold.Get("scope")) ||
			!Equal(r.Get("body"), hold.Get("body")) {
			return ReasonHoldMismatch, nil
		}
		holdObsStr, err := strOf(hold, "observed_at")
		if err != nil {
			return "", err
		}
		holdObsAt, err := instantOf(holdObsStr, "hold.observed_at")
		if err != nil {
			return "", err
		}
		if obsAt.Before(holdObsAt) {
			return ReasonObservationOrder, nil
		}
	}
	return "", nil
}

func (s *State) apply(r *wire.Node, seq int64) error {
	action, err := strOf(r, "action")
	if err != nil {
		return err
	}
	if action == "tombstone" {
		s.holds[seq] = cloneNode(r)
		s.active[seq] = true
		return nil
	}
	restores, err := intOf(r, "restores")
	if err != nil {
		return err
	}
	delete(s.active, restores)
	return nil
}

// Blocked reports whether any active hold on the stream blocks Pin p (by
// record scope, revision scope, or body scope).
func (s *State) Blocked(p Pin) (bool, error) {
	record, ok := s.records[p]
	if !ok {
		return false, nil
	}
	for seq := range s.active {
		h := s.holds[seq]
		scope, err := strOf(h, "scope")
		if err != nil {
			return false, err
		}
		target, err := pinFromNode(h.Get("target"), "hold.target")
		if err != nil {
			return false, err
		}
		switch scope {
		case "record":
			if p.RecordID == target.RecordID {
				return true, nil
			}
		case "revision":
			if p == target {
				return true, nil
			}
		case "body":
			match, err := bodyMatches(record.Get("body"), h.Get("body"))
			if err != nil {
				return false, err
			}
			if match {
				return true, nil
			}
		}
	}
	return false, nil
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
	case lifecycle[typ]:
		if err := s.trackBody(e, where); err != nil {
			return err
		}
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
		if cont := e.Get("continues"); cont != nil {
			if err := s.openContinuation(cont); err != nil {
				return err
			}
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
			if err := s.importRecord(e, seq); err != nil {
				return err
			}
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
	case typ == "tombstoned" || typ == "untombstoned":
		req := e.Get("request")
		if req == nil || req.Kind != wire.Object {
			return reject(ReasonShape, "%s: request is not an object", where)
		}
		wantDig, err := strOf(e, "request_digest")
		if err != nil {
			return err
		}
		gotDig, err := recordDigest(req)
		if err != nil {
			return err
		}
		if wantDig != gotDig {
			return reject(ReasonRequestDigest, "%s: request_digest mismatch", where)
		}
		action, err := strOf(req, "action")
		if err != nil {
			return err
		}
		wantTyp := "untombstoned"
		if action == "tombstone" {
			wantTyp = "tombstoned"
		}
		if typ != wantTyp {
			return reject(ReasonOperationType, "%s: entry type %q does not match request action %q", where, typ, action)
		}
		key, err := strOf(req, "idempotency_key")
		if err != nil {
			return err
		}
		if _, ok := s.live(key, at); ok {
			return reject(ReasonAcceptedKeyReuse, "%s: idempotency key %q is still live", where, key)
		}
		reason, err := s.refusal(req, at)
		if err != nil {
			return err
		}
		if reason != "" {
			return reject(reason, "%s: protection journal operation refused", where)
		}
		rcpt, err := s.protectionReceipt(req, seq)
		if err != nil {
			return err
		}
		if err := s.apply(req, seq); err != nil {
			return err
		}
		s.remember(key, keyIdentity{kind: "protection", value: gotDig}, at, "", rcpt)
		s.restrictedAt = append(s.restrictedAt, seq)
	case typ == "retired":
		rid, err := strOf(e, "record_id")
		if err != nil {
			return err
		}
		if !s.hasRecordID(rid) {
			return reject(ReasonTargetNotFound, "%s: record %q not found", where, rid)
		}
		if present(e, "idempotency_key") {
			key, err := strOf(e, "idempotency_key")
			if err != nil {
				return err
			}
			r := retirementRequest(e)
			wantDig, err := strOf(e, "request_digest")
			if err != nil {
				return err
			}
			gotDig, err := recordDigest(r)
			if err != nil {
				return err
			}
			if wantDig != gotDig {
				return reject(ReasonRequestDigest, "%s: request_digest mismatch", where)
			}
			if _, ok := s.live(key, at); ok {
				return reject(ReasonAcceptedKeyReuse, "%s: idempotency key %q is still live", where, key)
			}
			rcpt, err := s.retirementReceipt(r, seq)
			if err != nil {
				return err
			}
			s.remember(key, keyIdentity{kind: "retirement", value: gotDig}, at, rid, rcpt)
		}
		s.retired[rid] = true
		s.restrictedAt = append(s.restrictedAt, seq)
	case typ == "restrictions_reviewed":
		if err := s.checkReview(e, seq); err != nil {
			return err
		}
	case typ == "conflicted":
	default:
		s.partial = true
	}
	s.journal = append(s.journal, e)
	return nil
}

// Request evaluates a protection request r at decision time atStr, appending
// a tombstoned or untombstoned entry when accepted and returning the
// protection receipt. Stream-scoped live-key replay precedes target and
// current-state checks, matching the reference interpreter's request method.
func (s *State) Request(r *wire.Node, atStr string) (*wire.Node, error) {
	if r == nil || r.Kind != wire.Object {
		return nil, reject(ReasonShape, "request: not an object")
	}
	if err := CheckJournal(s.journal); err != nil {
		return nil, err
	}
	at, err := instantOf(atStr, "request at")
	if err != nil {
		return nil, err
	}
	lastAt, _ := ParseInstant(s.journal[len(s.journal)-1].Get("at").Str)
	if at.Before(lastAt) {
		return nil, reject(ReasonJournalTime, "request: at precedes the journal")
	}
	obsStr, err := strOf(r, "observed_at")
	if err != nil {
		return nil, err
	}
	if _, err := instantOf(obsStr, "request.observed_at"); err != nil {
		return nil, err
	}
	rStream, err := strOf(r, "stream")
	if err != nil {
		return nil, err
	}
	key, err := strOf(r, "idempotency_key")
	if err != nil {
		return nil, err
	}
	rdig, err := recordDigest(r)
	if err != nil {
		return nil, err
	}
	var reason string
	if rStream == s.stream {
		if prior, ok := s.live(key, at); ok {
			if prior.id.kind == "protection" && prior.id.value == rdig {
				return cloneNode(prior.receipt), nil
			}
			reason = ReasonIdempotencyConflict
		}
	}
	if reason == "" {
		reason, err = s.refusal(r, at)
		if err != nil {
			return nil, err
		}
	}
	if reason != "" {
		return &wire.Node{
			Kind: wire.Object,
			Fields: []wire.Member{
				{Name: "v", Value: intNode(1)},
				{Name: "stream", Value: strNode(rStream)},
				{Name: "idempotency_key", Value: strNode(key)},
				{Name: "outcome", Value: strNode(reason)},
			},
		}, nil
	}
	seq := int64(len(s.journal)) + 1
	if seq > wire.MaxSafeInteger {
		return nil, reject(wire.ReasonNumericDomain, "request: seq exceeds %d", int64(wire.MaxSafeInteger))
	}
	rcpt, err := s.protectionReceipt(r, seq)
	if err != nil {
		return nil, err
	}
	outcome, _ := strOf(rcpt, "outcome")
	entry := &wire.Node{
		Kind: wire.Object,
		Fields: []wire.Member{
			{Name: "v", Value: intNode(1)},
			{Name: "seq", Value: intNode(seq)},
			{Name: "group", Value: intNode(seq)},
			{Name: "group_size", Value: intNode(1)},
			{Name: "stream", Value: strNode(s.stream)},
			{Name: "type", Value: strNode(outcome)},
			{Name: "at", Value: strNode(atStr)},
			{Name: "request", Value: cloneNode(r)},
			{Name: "request_digest", Value: strNode(rdig)},
		},
	}
	if err := s.Ingest(entry); err != nil {
		return nil, err
	}
	return rcpt, nil
}

// Retire evaluates a keyed retirement request r at decision time atStr,
// appending a retired entry when accepted and returning the write receipt.
// Stream-scoped live-key replay precedes target and current-state checks,
// matching the reference interpreter's retire method.
func (s *State) Retire(r *wire.Node, atStr string) (*wire.Node, error) {
	if r == nil || r.Kind != wire.Object {
		return nil, reject(ReasonShape, "retire: not an object")
	}
	if err := CheckJournal(s.journal); err != nil {
		return nil, err
	}
	at, err := instantOf(atStr, "retire at")
	if err != nil {
		return nil, err
	}
	lastAt, _ := ParseInstant(s.journal[len(s.journal)-1].Get("at").Str)
	if at.Before(lastAt) {
		return nil, reject(ReasonJournalTime, "retire: at precedes the journal")
	}
	rStream, err := strOf(r, "stream")
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
	retireReason, err := strOf(r, "reason")
	if err != nil {
		return nil, err
	}
	rdig, err := recordDigest(r)
	if err != nil {
		return nil, err
	}
	var reason string
	if rStream == s.stream {
		if prior, ok := s.live(key, at); ok {
			if prior.id.kind == "retirement" && prior.id.value == rdig {
				return cloneNode(prior.receipt), nil
			}
			reason = ReasonIdempotencyConflict
		}
	}
	if reason == "" && (rStream != s.stream || !s.hasRecordID(rid)) {
		reason = ReasonTargetNotFound
	}
	if reason != "" {
		return &wire.Node{
			Kind: wire.Object,
			Fields: []wire.Member{
				{Name: "v", Value: intNode(1)},
				{Name: "kind", Value: strNode("receipt")},
				{Name: "stream", Value: strNode(rStream)},
				{Name: "idempotency_key", Value: strNode(key)},
				{Name: "outcome", Value: strNode(reason)},
			},
		}, nil
	}
	seq := int64(len(s.journal)) + 1
	if seq > wire.MaxSafeInteger {
		return nil, reject(wire.ReasonNumericDomain, "retire: seq exceeds %d", int64(wire.MaxSafeInteger))
	}
	entry := &wire.Node{
		Kind: wire.Object,
		Fields: []wire.Member{
			{Name: "v", Value: intNode(1)},
			{Name: "seq", Value: intNode(seq)},
			{Name: "group", Value: intNode(seq)},
			{Name: "group_size", Value: intNode(1)},
			{Name: "stream", Value: strNode(s.stream)},
			{Name: "type", Value: strNode("retired")},
			{Name: "at", Value: strNode(atStr)},
			{Name: "record_id", Value: strNode(rid)},
			{Name: "reason", Value: strNode(retireReason)},
			{Name: "idempotency_key", Value: strNode(key)},
			{Name: "request_digest", Value: strNode(rdig)},
		},
	}
	if err := s.Ingest(entry); err != nil {
		return nil, err
	}
	return s.retirementReceipt(r, seq)
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
