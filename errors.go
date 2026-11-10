package recordprims

import (
	"errors"
	"fmt"
)

// Errors fall into two published sets. A caller decides how to recover from the
// set, not from the message.
//
// Determinate errors mean nothing was journaled or changed: the caller must not
// reconcile, and retrying is safe only after fixing the cause. Indeterminate
// errors mean an operation may have taken effect: the caller reconciles (by key
// or attempt handle) or, for body operations, retries the same call.
//
// Contract outcomes (a rejected write, a refused release) are values in results,
// never errors.

// Determinate errors.
var (
	// ErrLocked: the store lock is held by another process and the lock timeout
	// elapsed (or was zero).
	ErrLocked = errors.New("recordprims: store is locked")

	// ErrClockRegression: the acceptance time would precede the stream tail's
	// acceptance time. Acceptance times are nondecreasing; the store refuses,
	// never clamps.
	ErrClockRegression = errors.New("recordprims: clock regression")

	// ErrStreamSealed: the stream was sealed by quarantine and accepts no writes.
	ErrStreamSealed = errors.New("recordprims: stream is sealed")

	// ErrSchema: a record, request or document failed validation before
	// acceptance.
	ErrSchema = errors.New("recordprims: schema violation")

	// ErrUnauthorized: the configured Authorizer, or the same-owner default,
	// refused the operation.
	ErrUnauthorized = errors.New("recordprims: unauthorized")

	// ErrSinkReentered: a release sink called back into the store on the same
	// goroutine.
	ErrSinkReentered = errors.New("recordprims: release sink reentered the store")

	// ErrReaderRevoked: a release frame reader was used after its callback
	// returned.
	ErrReaderRevoked = errors.New("recordprims: release reader revoked")

	// ErrStaleCut: an operation named a cut that is no longer current.
	ErrStaleCut = errors.New("recordprims: stale cut")

	// ErrClosed: the store or stream was closed before the operation started.
	ErrClosed = errors.New("recordprims: closed")

	// ErrStoreInvalid: the store root failed validation on open (ownership,
	// permissions, object type, filesystem type, or format). Never repaired.
	ErrStoreInvalid = errors.New("recordprims: invalid store")

	// ErrContinuationUnsupported: the continuation source uses an unsupported
	// capability or store (same-store durable-record/v1 sources only).
	ErrContinuationUnsupported = errors.New("recordprims: continuation source unsupported")

	// ErrStaleReview: the source or destination state digest in a restriction
	// review does not match the current state under the lock.
	ErrStaleReview = errors.New("recordprims: stale restriction review")
)

// ErrJournalInvalid is returned, determinately, when a stream journal (such as
// the store's designated body-removal stream) is missing or fails contract
// validation. Reason carries the closed durable-record/v1 refusal code.
type ErrJournalInvalid struct {
	Reason string
}

func (e *ErrJournalInvalid) Error() string {
	return fmt.Sprintf("recordprims: invalid journal (%s)", e.Reason)
}

// ErrDamagedTail is returned, determinately, by operations on a stream whose
// journal tail is a torn line or an incomplete atomic group. Other streams are
// unaffected. The fields are offsets and counts only, never content.
type ErrDamagedTail struct {
	Kind              DamagedTailKind
	LastCutSeq        uint64
	LastCutOffset     int64
	DamagedBytesCount int64
}

// DamagedTailKind distinguishes a torn final line from complete lines that end
// inside an incomplete atomic group.
type DamagedTailKind string

const (
	DamagedTornLine        DamagedTailKind = "torn_line"
	DamagedIncompleteGroup DamagedTailKind = "incomplete_group"
)

func (e *ErrDamagedTail) Error() string {
	return fmt.Sprintf("recordprims: damaged tail (%s) after seq %d at offset %d (%d bytes)",
		e.Kind, e.LastCutSeq, e.LastCutOffset, e.DamagedBytesCount)
}

// IOError reports a filesystem failure by operation and error class only. It
// never carries a path or the underlying error's text, so diagnostics cannot
// disclose local paths. Whether it is determinate depends on where it occurred;
// see IsIndeterminate.
type IOError struct {
	Op    string // e.g. "open", "write", "fsync", "rename"
	Class string // e.g. "not_found", "permission", "exists", "no_space", "other"
	// indeterminate is true when the failure happened at or after a durable
	// write began.
	indeterminate bool
}

func (e *IOError) Error() string {
	return fmt.Sprintf("recordprims: io_error{op=%s, class=%s}", e.Op, e.Class)
}

// ErrBodyIncomplete is returned, indeterminately, when a body operation failed
// after a durable step. Retry the same call; it resumes from the durable state.
type ErrBodyIncomplete struct {
	State string // the body state after the failure, e.g. "removing", "publishing"
}

func (e *ErrBodyIncomplete) Error() string {
	return fmt.Sprintf("recordprims: body operation incomplete (state %s); retry the same call", e.State)
}

// IsIndeterminate reports whether err means an operation may have taken effect.
// A false result for a non-nil error means nothing changed.
func IsIndeterminate(err error) bool {
	if err == nil {
		return false
	}
	var io *IOError
	if errors.As(err, &io) {
		return io.indeterminate
	}
	var bi *ErrBodyIncomplete
	return errors.As(err, &bi)
}
