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

	"github.com/3leaps/recordprims/internal/wire"
)

// Refusal reasons, as the contract names them.
const (
	ReasonEmpty                 = "empty"
	ReasonSeqGap                = "seq_gap"
	ReasonStreamMismatch        = "stream_mismatch"
	ReasonStreamOpenedPosition  = "stream_opened_position"
	ReasonGroup                 = "group"
	ReasonInconsistentCut       = "inconsistent_cut"
	ReasonDigest                = "digest"
	ReasonSupersedesRevision    = "supersedes_revision"
	ReasonRevisionChain         = "revision_chain"
	ReasonModeViolation         = "mode_violation"
	ReasonConflictCurrent       = "conflict_current"
	ReasonJournalVersion        = "journal_version"
	ReasonRecordVersion         = "record_version"
	ReasonInstantSpelling       = "instant_spelling"
	ReasonJournalTime           = "journal_time"
	ReasonBodyRemovalStream     = "body_removal_stream"
	ReasonIdempotencyWindow     = "idempotency_window"
	ReasonCapabilityNotCompiled = "capability_not_compiled"
	ReasonStratumNotCompiled    = "stratum_not_compiled"
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
