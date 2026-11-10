package journal

import (
	"strconv"

	"github.com/3leaps/recordprims/internal/canon"
	"github.com/3leaps/recordprims/internal/wire"
)

// recordDigest is "sha256:<hex>" of the RFC 8785 form of rec. A record
// outside the canonicalisable domain is refused as ingest.
func recordDigest(rec *wire.Node) (string, error) {
	d, err := canon.Digest(wire.Encode(rec))
	if err != nil {
		return "", reject(wire.ReasonIngest, "record digest canonicalization failed: %v", err)
	}
	return d, nil
}

type head struct {
	revision int64
	digest   string
}

type headKey struct {
	recordID string
	revision int64
	digest   string
}

type journalFacts struct {
	stream        string
	failureModel  string
	accepted      map[int64]*wire.Node
	acceptedKeys  map[string]bool
	acceptedHeads map[headKey]bool
}

// isHead reports whether n is exactly {"revision": h.revision, "digest":
// h.digest}, the reference's dictionary equality with a head.
func isHead(n *wire.Node, h head) bool {
	if n == nil || n.Kind != wire.Object || len(n.Fields) != 2 {
		return false
	}
	r, d := n.Get("revision"), n.Get("digest")
	return r != nil && r.Kind == wire.Number && r.IsInt && r.Int == h.revision &&
		d != nil && d.Kind == wire.String && d.Str == h.digest
}

// closingSeq returns the seq that closes e's atomic group, refusing a group
// whose closing seq leaves the safe-integer domain.
func closingSeq(e *wire.Node, where string) (int64, error) {
	group, err := intOf(e, "group")
	if err != nil {
		return 0, err
	}
	size, err := intOf(e, "group_size")
	if err != nil {
		return 0, err
	}
	// Both are safe integers, so the sum cannot overflow int64.
	end := group + size - 1
	if end > wire.MaxSafeInteger {
		return 0, reject(wire.ReasonNumericDomain, "%s: atomic group closing seq exceeds %d", where, int64(wire.MaxSafeInteger))
	}
	return end, nil
}

// checkEntry is the per-entry part of CheckJournal: the group span, and for
// content entries the record's stream, digest and supersedes revision. v1
// instants are checked by State, not here.
func checkEntry(e *wire.Node, where string) error {
	if _, err := closingSeq(e, where); err != nil {
		return err
	}
	typ, err := strOf(e, "type")
	if err != nil {
		return err
	}
	if typ != "appended" && typ != "revised" {
		return nil
	}
	rec := e.Get("record")
	if rec == nil || rec.Kind != wire.Object {
		return reject(ReasonShape, "%s: record is not an object", where)
	}
	rs, es := rec.Get("stream"), e.Get("stream")
	if rs == nil || es == nil || rs.Kind != wire.String || es.Kind != wire.String || rs.Str != es.Str {
		return reject(ReasonStreamMismatch, "%s", where)
	}
	got, err := recordDigest(rec)
	if err != nil {
		return err
	}
	if d := e.Get("digest"); d == nil || d.Kind != wire.String || d.Str != got {
		return reject(ReasonDigest, "%s: recorded digest does not match the record (computed %s)", where, got)
	}
	return checkSupersedes(rec, where)
}

func checkSupersedes(rec *wire.Node, where string) error {
	if !present(rec, "supersedes") {
		return nil
	}
	rev, err := intOf(rec, "revision")
	if err != nil {
		return err
	}
	srev, err := intOf(rec.Get("supersedes"), "revision")
	if err != nil {
		return err
	}
	if srev != rev-1 {
		return reject(ReasonSupersedesRevision, "%s", where)
	}
	return nil
}

// CheckJournal validates a complete v1 journal: seq runs 1..n with no gap,
// one stream, stream_opened first and only first, atomic groups start at
// their own seq and close before another begins, content entries carry their
// record's digest and extend the record's revision chain, conflicted entries
// name the head at that point, and the final group is complete.
func CheckJournal(lines []*wire.Node) error {
	_, err := checkJournalFacts(lines)
	return err
}

func checkJournalFacts(lines []*wire.Node) (journalFacts, error) {
	if len(lines) == 0 {
		return journalFacts{}, reject(ReasonEmpty, "journal has no entries")
	}
	heads := map[string]head{}
	accepted := map[int64]*wire.Node{}
	acceptedKeys := map[string]bool{}
	acceptedHeads := map[headKey]bool{}
	var mode, failureModel, stream string
	var openGroup, openSize int64
	open := false
	for idx, e := range lines {
		i := int64(idx + 1)
		where := "journal:" + strconv.FormatInt(i, 10)
		if _, err := closingSeq(e, where); err != nil {
			return journalFacts{}, err
		}
		seq, err := intOf(e, "seq")
		if err != nil {
			return journalFacts{}, err
		}
		if seq != i {
			return journalFacts{}, reject(ReasonSeqGap, "%s: expected seq %d, found %d", where, i, seq)
		}
		s, err := strOf(e, "stream")
		if err != nil {
			return journalFacts{}, err
		}
		if idx == 0 {
			stream = s
		} else if s != stream {
			return journalFacts{}, reject(ReasonStreamMismatch, "%s", where)
		}
		typ, err := strOf(e, "type")
		if err != nil {
			return journalFacts{}, err
		}
		if (typ == "stream_opened") != (i == 1) {
			return journalFacts{}, reject(ReasonStreamOpenedPosition, "%s", where)
		}
		if typ == "stream_opened" {
			if mode, err = strOf(e, "mode"); err != nil {
				return journalFacts{}, err
			}
			if failureModel, err = strOf(e, "failure_model"); err != nil {
				return journalFacts{}, err
			}
		}
		group, _ := intOf(e, "group")
		size, _ := intOf(e, "group_size")
		if open && i < openGroup+openSize {
			if group != openGroup || size != openSize {
				return journalFacts{}, reject(ReasonGroup, "%s: group (%d, %d) abandoned or changed before it closed", where, openGroup, openSize)
			}
		} else if group != i {
			return journalFacts{}, reject(ReasonGroup, "%s: a new group must start at its own seq", where)
		}
		openGroup, openSize, open = group, size, true
		if err := checkEntry(e, where); err != nil {
			return journalFacts{}, err
		}
		switch typ {
		case "appended", "revised":
			rec := e.Get("record")
			rid, err := strOf(rec, "record_id")
			if err != nil {
				return journalFacts{}, err
			}
			rev, err := intOf(rec, "revision")
			if err != nil {
				return journalFacts{}, err
			}
			key, err := strOf(e, "idempotency_key")
			if err != nil {
				return journalFacts{}, err
			}
			h, exists := heads[rid]
			if typ == "appended" {
				if exists || rev != 1 {
					return journalFacts{}, reject(ReasonRevisionChain, "%s: appended over an existing head", where)
				}
			} else {
				if mode == "single_revision" {
					return journalFacts{}, reject(ReasonModeViolation, "%s", where)
				}
				if !exists || rev != h.revision+1 || !isHead(rec.Get("supersedes"), h) {
					return journalFacts{}, reject(ReasonRevisionChain, "%s: supersedes is not the current head", where)
				}
			}
			dig := e.Get("digest").Str
			heads[rid] = head{revision: rev, digest: dig}
			accepted[i] = e
			acceptedKeys[key] = true
			acceptedHeads[headKey{recordID: rid, revision: rev, digest: dig}] = true
		case "conflicted":
			rid, err := strOf(e, "record_id")
			if err != nil {
				return journalFacts{}, err
			}
			h, exists := heads[rid]
			if present(e, "current") {
				if !exists || !isHead(e.Get("current"), h) {
					return journalFacts{}, reject(ReasonConflictCurrent, "%s: current is not the head at this point", where)
				}
			} else if exists {
				return journalFacts{}, reject(ReasonConflictCurrent, "%s: current omitted while a head exists", where)
			}
		}
	}
	if open && int64(len(lines)) < openGroup+openSize-1 {
		return journalFacts{}, reject(ReasonInconsistentCut, "final group truncated")
	}
	return journalFacts{
		stream:        stream,
		failureModel:  failureModel,
		accepted:      accepted,
		acceptedKeys:  acceptedKeys,
		acceptedHeads: acceptedHeads,
	}, nil
}

func checkAcceptedReceipt(r *wire.Node, facts journalFacts, where string) error {
	if r == nil || r.Kind != wire.Object {
		return reject(ReasonShape, "%s: receipt is not an object", where)
	}
	outcome, err := strOf(r, "outcome")
	if err != nil {
		return err
	}
	if outcome != "appended" && outcome != "revised" {
		return reject(ReasonReceiptNotAccepted, "%s: outcome %s is not an accepted write", where, outcome)
	}
	seq, err := intOf(r, "seq")
	if err != nil {
		return err
	}
	e := facts.accepted[seq]
	if e == nil {
		return reject(ReasonReceiptSeq, "%s: seq %d is not an accepted entry", where, seq)
	}
	p, err := pinOf(e, where)
	if err != nil {
		return err
	}
	for _, want := range []struct {
		member string
		val    string
	}{
		{"stream", e.Get("stream").Str},
		{"idempotency_key", e.Get("idempotency_key").Str},
		{"outcome", e.Get("type").Str},
		{"failure_model", facts.failureModel},
	} {
		got, err := strOf(r, want.member)
		if err != nil || got != want.val {
			return reject(ReasonReceiptMismatch, "%s: %s does not match journal", where, want.member)
		}
	}
	if !Equal(r.Get("pin"), pinNode(p)) {
		return reject(ReasonReceiptMismatch, "%s: pin does not match journal", where)
	}
	hasBodyStatus := present(e, "body_status")
	gotBodyStatus := present(r, "body_status")
	if hasBodyStatus != gotBodyStatus {
		return reject(ReasonReceiptMismatch, "%s: body_status presence does not match journal", where)
	}
	if hasBodyStatus {
		bs, err := strOf(r, "body_status")
		if err != nil || bs != e.Get("body_status").Str {
			return reject(ReasonReceiptMismatch, "%s: body_status does not match journal", where)
		}
	}
	return nil
}

// CheckReceipt validates a write receipt or reconciliation result against a
// complete v1 journal, matching the contract's check_receipt_doc rules.
func CheckReceipt(doc *wire.Node, lines []*wire.Node) error {
	facts, err := checkJournalFacts(lines)
	if err != nil {
		return err
	}
	if doc == nil || doc.Kind != wire.Object {
		return reject(ReasonShape, "receipt: not an object")
	}
	kind, err := strOf(doc, "kind")
	if err != nil {
		return err
	}
	stream, err := strOf(doc, "stream")
	if err != nil {
		return err
	}
	if kind == "reconciliation" {
		if stream != facts.stream {
			return reject(ReasonReceiptMismatch, "receipt: lookup stream is not the journal stream")
		}
		attStr, err := strOf(doc, "attempted_at")
		if err != nil {
			return err
		}
		att, err := instantOf(attStr, "receipt attempted_at")
		if err != nil {
			return err
		}
		res, err := strOf(doc, "result")
		if err != nil {
			return err
		}
		switch res {
		case "found":
			inner := doc.Get("receipt")
			if inner == nil || inner.Kind != wire.Object {
				return reject(ReasonShape, "receipt: missing embedded receipt")
			}
			is, err := strOf(inner, "stream")
			if err != nil {
				return err
			}
			ik, err := strOf(inner, "idempotency_key")
			if err != nil {
				return err
			}
			dk, err := strOf(doc, "idempotency_key")
			if err != nil {
				return err
			}
			if is != stream || ik != dk {
				return reject(ReasonReconciliationMismatch, "receipt: embedded receipt is for another stream or key")
			}
			return checkAcceptedReceipt(inner, facts, "receipt")
		case "not_found":
			dk, err := strOf(doc, "idempotency_key")
			if err != nil {
				return err
			}
			if facts.acceptedKeys[dk] {
				return reject(ReasonReconciliationMismatch, "receipt: not_found for a key the journal accepted")
			}
			fromStr, err := strOf(doc, "window_covers_from")
			if err != nil {
				return err
			}
			from, err := instantOf(fromStr, "receipt window_covers_from")
			if err != nil {
				return err
			}
			untilStr, err := strOf(doc, "window_covers_until")
			if err != nil {
				return err
			}
			until, err := instantOf(untilStr, "receipt window_covers_until")
			if err != nil {
				return err
			}
			if att.Before(from) || until.Before(att) {
				return reject(ReasonCoverage, "receipt: coverage interval does not contain attempted_at")
			}
		}
		return nil
	}
	if stream != facts.stream {
		return reject(ReasonReceiptMismatch, "receipt: receipt stream is not the journal stream")
	}
	outcome, err := strOf(doc, "outcome")
	if err != nil {
		return err
	}
	switch outcome {
	case "appended", "revised":
		return checkAcceptedReceipt(doc, facts, "receipt")
	case "revision_conflict":
		cur := doc.Get("current")
		if cur == nil || cur.Kind != wire.Object || len(cur.Fields) != 4 {
			return reject(ReasonReceiptMismatch, "receipt: current is not a valid pin")
		}
		cs, err := strOf(cur, "stream")
		if err != nil {
			return err
		}
		crid, err := strOf(cur, "record_id")
		if err != nil {
			return err
		}
		crev, err := intOf(cur, "revision")
		if err != nil {
			return err
		}
		cdig, err := strOf(cur, "digest")
		if err != nil {
			return err
		}
		if cs != facts.stream || !facts.acceptedHeads[headKey{recordID: crid, revision: crev, digest: cdig}] {
			return reject(ReasonReceiptMismatch, "receipt: current is not a head the journal accepted")
		}
	}
	return nil
}

// CheckProfile checks a stratum profile against its own build: every enabled
// capability is compiled in, and the runtime stratum is a compiled stratum.
func CheckProfile(p *wire.Node) error {
	build, runtime := p.Get("build"), p.Get("runtime")
	if build == nil || runtime == nil {
		return reject(ReasonShape, "profile: build or runtime missing")
	}
	compiled, err := stringSet(build.Get("compiled_capabilities"))
	if err != nil {
		return err
	}
	enabled := runtime.Get("enabled_capabilities")
	if enabled == nil || enabled.Kind != wire.Array {
		return reject(ReasonShape, "profile: enabled_capabilities is not an array")
	}
	for _, c := range enabled.Elems {
		if c.Kind != wire.String || !compiled[c.Str] {
			return reject(ReasonCapabilityNotCompiled, "profile: an enabled capability is not compiled")
		}
	}
	strata, err := stringSet(build.Get("compiled_strata"))
	if err != nil {
		return err
	}
	st, err := strOf(runtime, "stratum")
	if err != nil {
		return err
	}
	if !strata[st] {
		return reject(ReasonStratumNotCompiled, "profile: runtime stratum %q is not compiled", st)
	}
	return nil
}

func stringSet(n *wire.Node) (map[string]bool, error) {
	if n == nil || n.Kind != wire.Array {
		return nil, reject(ReasonShape, "profile: expected an array of strings")
	}
	out := map[string]bool{}
	for _, e := range n.Elems {
		if e.Kind != wire.String {
			return nil, reject(ReasonShape, "profile: expected an array of strings")
		}
		out[e.Str] = true
	}
	return out, nil
}
