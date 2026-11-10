// Package stratum defines the internal persistence seam beneath recordprims.Store
// and recordprims.Stream, decoupling durable-record/v1 journal semantics
// (internal/journal) from physical storage media:
//
//   - internal/stratum/fs ("journal-files", store format "recordprims-fs/v2"):
//     confined *fsroot.Root + cross-process advisory file lock + *jfile.Appender
//     (with designated-stream partial-write abort) + watermarked local indexes.
//   - internal/stratum/bucketcas ("ndjson-cas"):
//     segmented NDJSON + conditional-PUT head.json CAS + periodic compacted
//     state checkpoints over a sibling-type-free ConditionalObjectStore that
//     maps 1-for-1 onto 3leaps/storageprims ReadResponseProvider and
//     AdmittedPutProvider.
package stratum

import (
	"context"
	"errors"
	"fmt"
)

// CommitKnowledge reports whether a conditional write took effect on the
// underlying storage medium, matching storageprims 4-state CommitKnowledge.
type CommitKnowledge uint8

const (
	// CommitNotStarted means the request failed before dispatch (determinate non-commit).
	CommitNotStarted CommitKnowledge = iota
	// CommitNotCommitted means the storage medium determinately rejected the write
	// without mutating state (for example HTTP 412 Precondition Failed).
	CommitNotCommitted
	// CommitCommitted means the storage medium acknowledged the write as durable.
	CommitCommitted
	// CommitUnknown means the request was dispatched and may or may not have
	// taken effect (for example a transport drop or timeout after send under
	// max_attempts = 1).
	CommitUnknown
)

func (k CommitKnowledge) String() string {
	switch k {
	case CommitNotStarted:
		return "not_started"
	case CommitNotCommitted:
		return "not_committed"
	case CommitCommitted:
		return "committed"
	case CommitUnknown:
		return "unknown"
	default:
		return "invalid"
	}
}

// ConflictKind classifies a determinate conditional-PUT rejection when
// CommitKnowledge == CommitNotCommitted.
type ConflictKind uint8

const (
	ConflictNone ConflictKind = iota
	// ConflictAlreadyExists means If-None-Match: * failed because the object exists.
	ConflictAlreadyExists
	// ConflictTokenMismatch means If-Match: <validator> failed because the validator changed.
	ConflictTokenMismatch
	// ConflictOther means another determinate rejection occurred.
	ConflictOther
)

func (c ConflictKind) String() string {
	switch c {
	case ConflictNone:
		return "none"
	case ConflictAlreadyExists:
		return "already_exists"
	case ConflictTokenMismatch:
		return "token_mismatch"
	case ConflictOther:
		return "other"
	default:
		return "invalid"
	}
}

// StoreCaps advertises the mandatory conditional-write and coherent-read
// capabilities of a ConditionalObjectStore.
type StoreCaps struct {
	// ConditionalPutMustNotExist indicates support for atomic create-only PUT (If-None-Match: *).
	ConditionalPutMustNotExist bool
	// ConditionalPutValidatorMatch indicates support for validator-guarded CAS PUT (If-Match: <token>).
	ConditionalPutValidatorMatch bool
	// CoherentRead indicates single-GET strong read-after-write coherence returning
	// payload bytes and their opaque validator token from the same response.
	CoherentRead bool
}

// Valid reports whether all capabilities required by the ndjson-cas bucket
// stratum are present. A bucket stratum must refuse to initialize or open if
// Valid() is false and must never degrade to unconditional PUT.
func (c StoreCaps) Valid() bool {
	return c.ConditionalPutMustNotExist && c.ConditionalPutValidatorMatch && c.CoherentRead
}

var (
	// ErrNotFound is returned by GetCoherent when the key does not exist.
	ErrNotFound = errors.New("stratum: object not found")
	// ErrUnsupportedCapabilities is returned when a ConditionalObjectStore does
	// not advertise the required conditional-PUT and coherent-read capabilities.
	ErrUnsupportedCapabilities = errors.New("stratum: backend lacks required conditional-put or coherent-read capabilities")
)

// IndeterminateError wraps a failure whose commit outcome is CommitUnknown.
type IndeterminateError struct {
	Op  string
	Err error
}

func (e *IndeterminateError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("stratum: indeterminate %s: %v", e.Op, e.Err)
	}
	return fmt.Sprintf("stratum: indeterminate %s", e.Op)
}

func (e *IndeterminateError) Unwrap() error { return e.Err }

// ConditionalObjectStore is the sibling-type-free 5-method internal interface
// for conditional-PUT object storage backends, mapping 1-for-1 onto
// storageprims ReadResponseProvider + AdmittedPutProvider (DDR-0013).
type ConditionalObjectStore interface {
	// Capabilities returns the backend's truthful capability set.
	Capabilities() StoreCaps

	// GetCoherent performs a single-GET coherent read returning the complete
	// object bytes, the opaque validator token (ETag) from that same response,
	// and the object size. Returns ErrNotFound when the key does not exist.
	GetCoherent(ctx context.Context, key string) (data []byte, validator string, size int64, err error)

	// PutIfAbsent writes body to key only if key does not exist (If-None-Match: *).
	// On success, commit == CommitCommitted and validator is the new object's token.
	// On precondition failure, commit == CommitNotCommitted and conflict == ConflictAlreadyExists.
	// On transport drop after dispatch, commit == CommitUnknown.
	PutIfAbsent(ctx context.Context, key string, body []byte) (validator string, commit CommitKnowledge, conflict ConflictKind, err error)

	// PutIfMatch writes body to key only if its current validator matches expectedValidator
	// (If-Match: <expectedValidator>).
	// On success, commit == CommitCommitted and newValidator is the updated object's token.
	// On precondition failure, commit == CommitNotCommitted and conflict == ConflictTokenMismatch.
	// On transport drop after dispatch, commit == CommitUnknown.
	PutIfMatch(ctx context.Context, key string, expectedValidator string, body []byte) (newValidator string, commit CommitKnowledge, conflict ConflictKind, err error)

	// ListPrefix returns keys under prefix in lexicographical UTF-8 order, and
	// Delete removes an unreferenced or temporary object key.
	ListPrefix(ctx context.Context, prefix string) ([]string, error)
	Delete(ctx context.Context, key string) (CommitKnowledge, error)
}
