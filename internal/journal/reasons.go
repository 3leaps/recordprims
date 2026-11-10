// Package journal is the durable-record/v1 reader's semantic stage: the
// journal invariants, envelope versions, instants and entry order that the
// contract's reference interpreter checks after the wire and shape stages.
//
// It is ported family by family. State.Partial reports whether an entry
// needed semantics this package does not implement yet; until it never does,
// no store relies on a State.
package journal

import (
	"fmt"
	"strconv"

	"github.com/3leaps/recordprims/internal/wire"
)

// Refusal reasons, as the contract names them.
const (
	ReasonEmpty                  = "empty"
	ReasonSeqGap                 = "seq_gap"
	ReasonStreamMismatch         = "stream_mismatch"
	ReasonStreamOpenedPosition   = "stream_opened_position"
	ReasonGroup                  = "group"
	ReasonInconsistentCut        = "inconsistent_cut"
	ReasonDigest                 = "digest"
	ReasonSupersedesRevision     = "supersedes_revision"
	ReasonRevisionChain          = "revision_chain"
	ReasonModeViolation          = "mode_violation"
	ReasonConflictCurrent        = "conflict_current"
	ReasonJournalVersion         = "journal_version"
	ReasonRecordVersion          = "record_version"
	ReasonInstantSpelling        = "instant_spelling"
	ReasonJournalTime            = "journal_time"
	ReasonBodyRemovalStream      = "body_removal_stream"
	ReasonIdempotencyWindow      = "idempotency_window"
	ReasonCapabilityNotCompiled  = "capability_not_compiled"
	ReasonStratumNotCompiled     = "stratum_not_compiled"
	ReasonAcceptedKeyReuse       = "accepted_key_reuse"
	ReasonInvalidDeduplication   = "invalid_deduplication"
	ReasonReceiptMismatch        = "receipt_mismatch"
	ReasonReceiptNotAccepted     = "receipt_not_accepted"
	ReasonReceiptSeq             = "receipt_seq"
	ReasonReconciliationMismatch = "reconciliation_mismatch"
	ReasonCoverage               = "coverage"
	ReasonRetiredRecord          = "retired_record"
	ReasonRequestDigest          = "request_digest"
	ReasonOperationType          = "operation_type"
	ReasonIdempotencyConflict    = "idempotency_conflict"
	ReasonTargetNotFound         = "target_not_found"
	ReasonBodyMismatch           = "body_mismatch"
	ReasonHoldNotActive          = "hold_not_active"
	ReasonHoldMismatch           = "hold_mismatch"
	ReasonObservationOrder       = "observation_order"
	// ReasonShape is the shape stage's refusal. This package reaches it only
	// when a document that should have passed shape lacks a member or has
	// the wrong type for one, so it fails closed instead of guessing.
	ReasonShape = "shape"
)

func reject(reason, format string, a ...any) error {
	return &wire.Reject{Reason: reason, Detail: fmt.Sprintf(format, a...)}
}

// intOf returns a protocol integer member that passed numeric_domain.
func intOf(n *wire.Node, name string) (int64, error) {
	v := n.Get(name)
	if v == nil || v.Kind != wire.Number || !v.IsInt {
		return 0, reject(ReasonShape, "%s is not a protocol integer", name)
	}
	return v.Int, nil
}

// strOf returns a string member.
func strOf(n *wire.Node, name string) (string, error) {
	v := n.Get(name)
	if v == nil || v.Kind != wire.String {
		return "", reject(ReasonShape, "%s is not a string", name)
	}
	return v.Str, nil
}

// present reports whether member name is present and not JSON null.
func present(n *wire.Node, name string) bool {
	v := n.Get(name)
	return v != nil && v.Kind != wire.Null
}

// Equal reports whether a and b are structurally equal JSON values, ignoring
// object member order and comparing protocol integers by value.
func Equal(a, b *wire.Node) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a.Kind != b.Kind {
		return false
	}
	switch a.Kind {
	case wire.Null:
		return true
	case wire.Bool:
		return a.Bool == b.Bool
	case wire.String:
		return a.Str == b.Str
	case wire.Number:
		if a.IsInt && b.IsInt {
			return a.Int == b.Int
		}
		return !a.IsInt && !b.IsInt && a.Token == b.Token
	case wire.Array:
		if len(a.Elems) != len(b.Elems) {
			return false
		}
		for i := range a.Elems {
			if !Equal(a.Elems[i], b.Elems[i]) {
				return false
			}
		}
		return true
	case wire.Object:
		if len(a.Fields) != len(b.Fields) {
			return false
		}
		for _, m := range a.Fields {
			if !Equal(m.Value, b.Get(m.Name)) {
				return false
			}
		}
		return true
	}
	return false
}

func strNode(s string) *wire.Node {
	return &wire.Node{Kind: wire.String, Str: s}
}

func intNode(v int64) *wire.Node {
	return &wire.Node{Kind: wire.Number, Token: strconv.FormatInt(v, 10), Int: v, IsInt: true}
}

func cloneNode(n *wire.Node) *wire.Node {
	if n == nil {
		return nil
	}
	cp := *n
	if n.Elems != nil {
		cp.Elems = make([]*wire.Node, len(n.Elems))
		for i, e := range n.Elems {
			cp.Elems[i] = cloneNode(e)
		}
	}
	if n.Fields != nil {
		cp.Fields = make([]wire.Member, len(n.Fields))
		for i, m := range n.Fields {
			cp.Fields[i] = wire.Member{Name: m.Name, Value: cloneNode(m.Value)}
		}
	}
	return &cp
}
