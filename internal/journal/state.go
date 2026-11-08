package journal

import (
	"math/big"
	"regexp"
	"strconv"

	"github.com/3leaps/recordprims/internal/wire"
)

// lifecycle are the body lifecycle entry types, which appear only on the
// store's designated body-removal stream.
var lifecycle = map[string]bool{
	"body_published": true, "body_removal_started": true, "body_removed": true,
}

// State replays one v1 stream entry by entry, in the reference interpreter's
// order.
type State struct {
	window        int64
	journal       []*wire.Node
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
	return &State{window: v.Int64()}, nil
}

// SetRemovalStream names the store's designated body-removal stream, from
// the profile's runtime.body_removal_stream.
func (s *State) SetRemovalStream(name string) {
	s.removalStream, s.hasRemoval = name, true
}

// Partial reports whether an ingested entry needed semantics this package
// does not implement yet (content, protection, retirement, body lifecycle or
// continuation processing). A State that is partial has not checked the
// stream completely. Partial says nothing about groups: the journal
// invariants run only when an entry closes its group, so a State whose last
// group is still open is not partial but has not checked that group; the
// caller runs CheckJournal on the whole journal for that. Shape constraints
// (group_size at least 1, required members) are the wire stages' and are not
// rechecked here.
func (s *State) Partial() bool { return s.partial }

// Len is the number of entries ingested.
func (s *State) Len() int { return len(s.journal) }

// Journal returns the entries ingested, in order. The caller must not modify
// them.
func (s *State) Journal() []*wire.Node { return s.journal }

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
		if e.Get("continues") != nil {
			s.partial = true
		}
	case typ == "conflicted":
	default:
		s.partial = true
	}
	s.journal = append(s.journal, e)
	return nil
}
