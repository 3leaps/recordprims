package recordprims

import (
	"context"
	"fmt"

	"github.com/3leaps/recordprims/internal/journal"
)

// ServedReadOutcome is the outcome of Stream.ReadServed.
type ServedReadOutcome string

const (
	// ServedReadServed: the revision is served at the cut. Delivery progress
	// is in ServedRead.Delivery and the frame counters.
	ServedReadServed ServedReadOutcome = "served"
	// ServedReadNotServed: the revision is not served at the cut. The same
	// outcome covers a held, retired or restricted revision, a revision whose
	// body cannot be delivered under full_content, a pin that names no
	// revision on the stream and a pin whose digest does not match. No frame
	// is delivered.
	ServedReadNotServed ServedReadOutcome = "not_served"
	// ServedReadRefused: the stream cannot be evaluated, independent of the
	// pin. ServedRead.Reason is protection_unavailable or
	// restrictions_pending. No frame is delivered.
	ServedReadRefused ServedReadOutcome = "refused"
)

// ServedRead is the result of Stream.ReadServed.
type ServedRead struct {
	Outcome ServedReadOutcome `json:"outcome"`
	// Reason is set only when Outcome is ServedReadRefused.
	Reason string `json:"reason,omitempty"`
	// Cut is the decision cut. It is nil when no trustworthy cut could be
	// established because the stream could not be replayed.
	Cut *uint64 `json:"cut,omitempty"`
	// Frames is the number of frames the served decision covers: the record
	// frame, plus the body frame under full_content when the revision has a
	// body. It is zero unless Outcome is ServedReadServed.
	Frames int `json:"frames"`
	// Delivery is empty when nothing was attempted: the outcome is not
	// served, or the sink is nil (a nil sink evaluates the decision only).
	// Otherwise it is ReleaseReleased when every frame was delivered,
	// ReleasePartial when delivery stopped between frames, or
	// ReleaseUncertain when a frame's delivery is uncertain.
	Delivery        ReleaseOutcome `json:"delivery,omitempty"`
	FramesCompleted int            `json:"frames_completed"`
	FrameInFlight   *int           `json:"frame_in_flight,omitempty"`
}

// ReadServed reads one revision through the serving path. It is
// Release with PurposeServe and a selection of exactly pin, evaluated at the
// current cut under the store lock with the same hold, retirement and
// continuation checks, and it delivers at most that revision's frames to
// sink. It never falls back to another revision of the record.
//
// Unlike Release it reports no omitted revisions, and every member-level
// denial, including a pin that names no revision, is the single outcome
// ServedReadNotServed. Stream-wide refusals that do not depend on the pin are
// reported as ServedReadRefused with their reason.
//
// A malformed pin returns ErrSchema before the store is read. A sink callback
// error returns ErrSink together with the delivery progress, as Release does.
func (st *Stream) ReadServed(ctx context.Context, pin Pin, closure UseClosure, sink ReleaseSink) (ServedRead, error) {
	if !reStream.MatchString(pin.Stream) || !reRecordID.MatchString(pin.RecordID) ||
		pin.Revision < 1 || pin.Revision > maxSafeInteger || !reDigest.MatchString(pin.Digest) {
		return ServedRead{}, fmt.Errorf("%w: invalid pin", ErrSchema)
	}
	if closure == "" {
		closure = ClosureFullContent
	}
	if closure != ClosureFullContent && closure != ClosureMetadataOnly {
		return ServedRead{}, fmt.Errorf("%w: invalid release closure", ErrSchema)
	}
	res, meta, err := st.release(ctx, UseRequest{Purpose: PurposeServe, Selection: []Pin{pin}, Closure: closure}, sink)
	if err != nil && res.Outcome == "" {
		return ServedRead{}, err
	}
	out, merr := servedReadFrom(res, meta, sink != nil)
	if merr != nil {
		return ServedRead{}, merr
	}
	return out, err
}

func servedReadFrom(res ReleaseResult, meta releaseMeta, delivered bool) (ServedRead, error) {
	cut := res.Cut
	out := ServedRead{Cut: &cut}
	switch res.Outcome {
	case ReleaseRefused:
		switch res.RefusalReason {
		case journal.ReasonProtectionUnavailable, journal.ReasonRestrictionsPending:
			out.Outcome = ServedReadRefused
			out.Reason = res.RefusalReason
			if meta.noCut {
				out.Cut = nil
			}
		case journal.ReasonProtectedUse, journal.ReasonTargetNotFound, journal.ReasonBodyClosure:
			// Each describes the selected revision; they share one outward
			// outcome.
			out.Outcome = ServedReadNotServed
		default:
			return ServedRead{}, fmt.Errorf("recordprims: unexpected serve refusal %q", res.RefusalReason)
		}
		return out, nil
	}
	out.Outcome = ServedReadServed
	out.Frames = meta.frames
	if delivered {
		out.Delivery = res.Outcome
		out.FramesCompleted = res.FramesCompleted
		out.FrameInFlight = res.FrameInFlight
	}
	return out, nil
}
