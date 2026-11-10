package recordprims

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/3leaps/recordprims/internal/jfile"
	"github.com/3leaps/recordprims/internal/journal"
	"github.com/3leaps/recordprims/internal/shape"
	"github.com/3leaps/recordprims/internal/wire"
)

// BodyRef binds bytes held outside a record by digest and length.
type BodyRef struct {
	Digest    string // "sha256:" + 64 lowercase hex
	Length    int64
	MediaType string
}

// PublicationLease protects a freshly published body from removal until the
// Append that references it consumes the lease, or the lease expires.
type PublicationLease struct {
	ID        string
	Digest    string
	ExpiresAt time.Time
}

// Pin names one revision of one record.
type Pin struct {
	Stream   string `json:"stream"`
	RecordID string `json:"record_id"`
	Revision uint64 `json:"revision"`
	Digest   string `json:"digest"`
}

// Provenance identifies the producer or operator of a record or lifecycle action.
type Provenance struct {
	ActorKind  string `json:"actor_kind"`
	Actor      string `json:"actor"`
	Supervisor string `json:"supervisor,omitempty"`
	Model      string `json:"model,omitempty"`
	Tool       string `json:"tool,omitempty"`
}

// Authorization binds an external authorization artifact by reference and digest.
type Authorization struct {
	Ref    string `json:"ref"`
	Digest string `json:"digest"`
}

// RemoveBodyReason is the closed vocabulary of body-removal reasons.
type RemoveBodyReason string

const (
	RemoveReasonRetention        RemoveBodyReason = "retention"
	RemoveReasonOperator         RemoveBodyReason = "operator"
	RemoveReasonRightsRestricted RemoveBodyReason = "rights_restricted"
)

// RemoveBodyRequest asks the store to remove a body's bytes.
type RemoveBodyRequest struct {
	Digest string
	Length int64
	// CompleteIncarnation, when set, completes an earlier removal of that
	// incarnation only; a newer publication makes the request superseded.
	CompleteIncarnation *uint64
	Reason              RemoveBodyReason
	Provenance          Provenance
	Authorization       *Authorization
}

// RemoveBodyOutcome is the closed set of removal results.
type RemoveBodyOutcome string

const (
	BodyRemoved               RemoveBodyOutcome = "removed"
	BodyAbsent                RemoveBodyOutcome = "absent"
	BodyMissing               RemoveBodyOutcome = "missing"
	BodyPublicationIncomplete RemoveBodyOutcome = "publication_incomplete"
	BodySuperseded            RemoveBodyOutcome = "superseded"
	BodyConflict              RemoveBodyOutcome = "body_conflict"
	BodyProtectionUnknown     RemoveBodyOutcome = "protection_unknown"
	BodyHeld                  RemoveBodyOutcome = "held"
	BodyStillReferenced       RemoveBodyOutcome = "still_referenced"
	BodyPending               RemoveBodyOutcome = "pending"
)

// RemoveBodyResult is the outcome of RemoveBody.
type RemoveBodyResult struct {
	Outcome RemoveBodyOutcome
}

// bodyStateKind is the durable-record/v1 reader state (plus corrupt for failed
// physical byte observation) every body operation derives from.
type bodyStateKind string

const (
	stateAvailable      bodyStateKind = "available"
	stateRemovalPending bodyStateKind = "removal_pending"
	stateRemoved        bodyStateKind = "removed"
	statePublishing     bodyStateKind = "publishing"
	stateUnpublished    bodyStateKind = "unpublished"
	stateIntegrityLoss  bodyStateKind = "integrity_loss"
	stateCorrupt        bodyStateKind = "corrupt"
)

// bodyState is the evaluated state of one (digest, length), the journal bound,
// the journal phase, and the local prewrite incarnation ledger.
type bodyState struct {
	kind        bodyStateKind
	incarnation uint64 // max(journal bound, local counter)
	bound       uint64 // committed journal bound
	phase       string // "", "published", "started", "removed"
}

// Body directory layout, relative to the store root.
const (
	dirBodySHA256      = dirBodies + "/sha256"
	dirBodyTmp         = dirBodies + "/tmp"
	dirBodyIncarnation = dirBodies + "/incarnation"
	dirBodyPending     = dirBodies + "/pending"
)

var (
	reDigest    = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	reMediaType = regexp.MustCompile(`^[a-z0-9][a-z0-9!#$&^_.+-]{0,126}/[a-z0-9][a-z0-9!#$&^_.+-]{0,126}$`)
	reStream    = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)
	reRecordID  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
	reAuthRef   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,255}$`)
)

// errBodyConflict is returned by PutBody when the stored bytes for the digest
// are corrupt.
var errBodyConflict = fmt.Errorf("%w: body_conflict", ErrStoreInvalid)

// digestHex returns the 64-hex part of a validated digest. Names on disk use
// the bare hex: the "sha256:" prefix is not portable in file names.
func digestHex(digest string) string { return strings.TrimPrefix(digest, "sha256:") }

func bodyFile(h string) string        { return dirBodySHA256 + "/" + h[:2] + "/" + h }
func incarnationFile(h string) string { return dirBodyIncarnation + "/" + h }

type leaseFile struct {
	Incarnation uint64 `json:"incarnation"`
	ExpiresAt   string `json:"expires_at"`
}

// fault reports a test-injected failure at a named phase. Production stores
// have no hook.
func (s *Store) fault(phase string) error {
	if s.hook == nil {
		return nil
	}
	return s.hook(phase)
}

// ensureBodyDirs creates the body directories once per handle. Callers hold
// the store lock.
func (s *Store) ensureBodyDirs() error {
	if s.bodyDirsReady {
		return nil
	}
	for _, d := range []string{dirBodies, dirBodySHA256, dirBodyTmp, dirBodyIncarnation, dirBodyPending} {
		if err := s.root.Mkdir(d, s.fso); err != nil {
			return ioErr("mkdir", err, false)
		}
	}
	if err := s.root.SyncDir(dirBodies); err != nil {
		return ioErr("fsync_dir", err, false)
	}
	s.bodyDirsReady = true
	return nil
}

type designatedJournal struct {
	st        *journal.State
	jpath     string
	sdir      string
	cutOffset int64
	lastSeq   int64
	lastAt    time.Time
}

func rejectReason(err error) string {
	var r *wire.Reject
	if errors.As(err, &r) {
		return r.Reason
	}
	return journal.ReasonShape
}

// readDesignatedState replays and validates the store's designated body-removal
// stream under the store lock. A missing, torn, or invalid designated stream
// returns ErrJournalInvalid (G7, G9, item 21).
func (s *Store) readDesignatedState() (*designatedJournal, error) {
	streamID := s.profile.Runtime.BodyRemovalStream
	if streamID == "" {
		return nil, &ErrJournalInvalid{Reason: journal.ReasonBodyRemovalStream}
	}
	jpath := streamJournalFile(streamID)
	raw, err := readFileVerified(s.root, jpath, s.fso, maxJournalBytes)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, &ErrJournalInvalid{Reason: journal.ReasonBodyRemovalStream}
	}
	if err != nil {
		return nil, ioErr("read", err, false)
	}
	nodes, err := wire.IngestLines(raw, wire.KindJournalEntry)
	if err != nil {
		return nil, &ErrJournalInvalid{Reason: rejectReason(err)}
	}
	st, err := journal.New(s.windowNode())
	if err != nil {
		return nil, &ErrJournalInvalid{Reason: rejectReason(err)}
	}
	st.SetProfile(s.profileNode)
	var lastAt time.Time
	for _, node := range nodes {
		if err := shape.Validate("v1/journal-entry.schema.json", node); err != nil {
			return nil, &ErrJournalInvalid{Reason: journal.ReasonShape}
		}
		if err := st.Ingest(node); err != nil {
			return nil, &ErrJournalInvalid{Reason: rejectReason(err)}
		}
		if atNode := node.Get("at"); atNode != nil && atNode.Kind == wire.String {
			lastAt, _ = journal.ParseInstant(atNode.Str)
		}
	}
	if err := journal.CheckJournal(st.Journal()); err != nil {
		return nil, &ErrJournalInvalid{Reason: rejectReason(err)}
	}
	if st.Stream() != streamID {
		return nil, &ErrJournalInvalid{Reason: journal.ReasonBodyRemovalStream}
	}
	return &designatedJournal{
		st:        st,
		jpath:     jpath,
		sdir:      streamDir(streamID),
		cutOffset: int64(len(raw)),
		lastSeq:   int64(st.Len()),
		lastAt:    lastAt,
	}, nil
}

// bodyState evaluates the state of (digest, length) under the store lock.
func (s *Store) bodyState(h string, length int64) (bodyState, error) {
	desig, err := s.readDesignatedState()
	if err != nil {
		return bodyState{}, err
	}
	return s.evalBodyState(desig, h, length)
}

func (s *Store) evalBodyState(desig *designatedJournal, h string, length int64) (bodyState, error) {
	localN, hasLocal, err := s.readIncarnation(h)
	if err != nil {
		return bodyState{}, err
	}
	present, intact, err := s.bodyFilePresent(h, length)
	if err != nil {
		return bodyState{}, err
	}
	key := journal.BodyKey{Digest: "sha256:" + h, Length: length}
	rs, bound, phase := desig.st.BodyLifecycle(key, present && intact)
	maxInc := uint64(bound)
	if hasLocal && localN > maxInc {
		maxInc = localN
	}
	// A digest or length mismatch overrides every other state (G15 step 0).
	if present && !intact {
		return bodyState{
			kind:        stateCorrupt,
			incarnation: maxInc,
			bound:       uint64(bound),
			phase:       phase,
		}, nil
	}
	return bodyState{
		kind:        bodyStateKind(rs),
		incarnation: maxInc,
		bound:       uint64(bound),
		phase:       phase,
	}, nil
}

func (s *Store) readIncarnation(h string) (uint64, bool, error) {
	b, err := readFileVerified(s.root, incarnationFile(h), s.fso, 32)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, ioErr("read", err, false)
	}
	n, err := strconv.ParseUint(string(b), 10, 64)
	if err != nil || n == 0 || n > maxSafeInteger {
		return 0, false, fmt.Errorf("%w: incarnation record invalid", ErrStoreInvalid)
	}
	return n, true, nil
}

// bodyFilePresent reports whether the body file exists and, if so, whether
// its bytes hash to the digest with the expected length.
func (s *Store) bodyFilePresent(h string, length int64) (present, intact bool, err error) {
	f, err := s.root.OpenRead(bodyFile(h), s.fso)
	if errors.Is(err, fs.ErrNotExist) {
		return false, false, nil
	}
	if err != nil {
		return false, false, ioErr("open", err, false)
	}
	defer f.Close()
	sum := sha256.New()
	got, err := io.Copy(sum, f)
	if err != nil {
		return false, false, ioErr("read", err, false)
	}
	return true, got == length && hex.EncodeToString(sum.Sum(nil)) == h, nil
}

func (s *Store) exists(name string) (bool, error) {
	_, err := s.root.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, ioErr("stat", err, false)
	}
	return true, nil
}

func (s *Store) appendLifecycleEntry(desig *designatedJournal, entryType, h string, length int64, incarnation uint64, req *RemoveBodyRequest) error {
	if desig.lastSeq >= maxSafeInteger {
		return fmt.Errorf("%w: designated stream seq exhausted", ErrSchema)
	}
	now := s.opts.Clock.Now().UTC().Truncate(time.Microsecond)
	if now.Before(desig.lastAt) {
		return ErrClockRegression
	}
	seq := desig.lastSeq + 1
	seqTok := strconv.FormatInt(seq, 10)
	lenTok := strconv.FormatInt(length, 10)
	incTok := strconv.FormatUint(incarnation, 10)

	fields := []wire.Member{
		{Name: "v", Value: &wire.Node{Kind: wire.Number, Token: "1", IsInt: true, Int: 1}},
		{Name: "seq", Value: &wire.Node{Kind: wire.Number, Token: seqTok, IsInt: true, Int: seq}},
		{Name: "group", Value: &wire.Node{Kind: wire.Number, Token: seqTok, IsInt: true, Int: seq}},
		{Name: "group_size", Value: &wire.Node{Kind: wire.Number, Token: "1", IsInt: true, Int: 1}},
		{Name: "stream", Value: &wire.Node{Kind: wire.String, Str: s.profile.Runtime.BodyRemovalStream}},
		{Name: "type", Value: &wire.Node{Kind: wire.String, Str: entryType}},
		{Name: "at", Value: &wire.Node{Kind: wire.String, Str: FormatInstant(now)}},
		{Name: "body", Value: &wire.Node{
			Kind: wire.Object,
			Fields: []wire.Member{
				{Name: "digest", Value: &wire.Node{Kind: wire.String, Str: "sha256:" + h}},
				{Name: "length", Value: &wire.Node{Kind: wire.Number, Token: lenTok, IsInt: true, Int: length}},
			},
		}},
		{Name: "incarnation", Value: &wire.Node{Kind: wire.Number, Token: incTok, IsInt: true, Int: int64(incarnation)}},
	}
	if entryType == "body_removal_started" && req != nil {
		fields = append(fields,
			wire.Member{Name: "reason", Value: &wire.Node{Kind: wire.String, Str: string(req.Reason)}},
			wire.Member{Name: "provenance", Value: provenanceNode(req.Provenance)},
		)
		if req.Authorization != nil {
			fields = append(fields, wire.Member{
				Name: "authorization",
				Value: &wire.Node{
					Kind: wire.Object,
					Fields: []wire.Member{
						{Name: "ref", Value: &wire.Node{Kind: wire.String, Str: req.Authorization.Ref}},
						{Name: "digest", Value: &wire.Node{Kind: wire.String, Str: req.Authorization.Digest}},
					},
				},
			})
		}
	}
	entry := &wire.Node{Kind: wire.Object, Fields: fields}

	// G2: pre-validate against shape and designated-stream state before writing.
	if err := shape.Validate("v1/journal-entry.schema.json", entry); err != nil {
		return fmt.Errorf("%w: lifecycle entry shape invalid", ErrSchema)
	}
	if err := desig.st.Ingest(entry); err != nil {
		return &ErrJournalInvalid{Reason: rejectReason(err)}
	}

	line := append(wire.Encode(entry), '\n')
	if err := s.appender.Append(desig.jpath, desig.cutOffset, line, true); err != nil {
		return mapJFileErr(err)
	}
	desig.cutOffset += int64(len(line))
	desig.lastSeq = seq
	desig.lastAt = now

	switch entryType {
	case "body_published":
		if err := s.fault("journal.published_written"); err != nil {
			return err
		}
		if err := s.fault("journal.published_fsynced"); err != nil {
			return err
		}
		return s.fault("body.published_written")
	case "body_removal_started":
		if err := s.fault("journal.removal_started_written"); err != nil {
			return err
		}
		if err := s.fault("remove.tombstone_written"); err != nil {
			return err
		}
		if err := s.fault("journal.removal_started_fsynced"); err != nil {
			return err
		}
		return s.fault("remove.tombstone_fsynced")
	case "body_removed":
		if err := s.fault("journal.removed_written"); err != nil {
			return err
		}
		return s.fault("journal.removed_fsynced")
	}
	return nil
}

func provenanceNode(p Provenance) *wire.Node {
	fields := []wire.Member{
		{Name: "actor_kind", Value: &wire.Node{Kind: wire.String, Str: p.ActorKind}},
		{Name: "actor", Value: &wire.Node{Kind: wire.String, Str: p.Actor}},
	}
	if p.Supervisor != "" {
		fields = append(fields, wire.Member{Name: "supervisor", Value: &wire.Node{Kind: wire.String, Str: p.Supervisor}})
	}
	if p.Model != "" {
		fields = append(fields, wire.Member{Name: "model", Value: &wire.Node{Kind: wire.String, Str: p.Model}})
	}
	if p.Tool != "" {
		fields = append(fields, wire.Member{Name: "tool", Value: &wire.Node{Kind: wire.String, Str: p.Tool}})
	}
	return &wire.Node{Kind: wire.Object, Fields: fields}
}

func validProvenance(p Provenance) bool {
	if p.ActorKind != "human" && p.ActorKind != "agent" && p.ActorKind != "process" {
		return false
	}
	if n := utf8.RuneCountInString(p.Actor); n < 1 || n > 128 {
		return false
	}
	for _, opt := range []string{p.Supervisor, p.Model, p.Tool} {
		if opt != "" && utf8.RuneCountInString(opt) > 128 {
			return false
		}
	}
	return true
}

func validAuthorization(a *Authorization) bool {
	if a == nil {
		return true
	}
	return reAuthRef.MatchString(a.Ref) && reDigest.MatchString(a.Digest)
}

// PutBody streams r into the store as a content-addressed body and returns
// its reference and a publication lease. It is idempotent by state: after any
// error, retry the same call. The input is first staged durably in a
// temporary file, which is not body state. A body longer than 2^53 - 1 bytes
// is refused while staging; a publication whose incarnation would pass
// 2^53 - 1 is refused after staging. Both refusals are a determinate ErrSchema
// that changes no body state, and the staged file is removed.
func (s *Store) PutBody(ctx context.Context, r io.Reader, mediaType string) (BodyRef, PublicationLease, error) {
	if !reMediaType.MatchString(mediaType) {
		return BodyRef{}, PublicationLease{}, fmt.Errorf("%w: media type", ErrSchema)
	}
	if err := s.withLock(s.ensureBodyDirs); err != nil {
		return BodyRef{}, PublicationLease{}, err
	}
	tmp, h, length, err := s.stage(ctx, r)
	if err != nil {
		return BodyRef{}, PublicationLease{}, err
	}
	staged := true
	completed := false
	defer func() {
		if !completed && staged {
			s.root.Remove(tmp)
		}
	}()

	var lease PublicationLease
	progressed := false
	err = s.withLock(func() error {
		desig, err := s.readDesignatedState()
		if err != nil {
			return err
		}
		st, err := s.evalBodyState(desig, h, length)
		if err != nil {
			return err
		}
		if st.kind == stateCorrupt {
			return errBodyConflict
		}
		switch st.kind {
		case stateAvailable:
			// Bytes are already verified in place and committed at st.bound.
			progressed = true
			lease, err = s.writeLease(h, st.bound)
			return err
		case statePublishing:
			// Bytes are verified in place, awaiting body_published commit.
			n := st.incarnation
			if n <= st.bound {
				if st.bound >= maxSafeInteger {
					return fmt.Errorf("%w: body incarnation exhausted", ErrSchema)
				}
				n = st.bound + 1
				progressed = true
				if err := s.writeIncarnation(h, n); err != nil {
					return err
				}
			}
			progressed = true
			if err := s.syncBodyDir(h, "body.dir_fsynced"); err != nil {
				return err
			}
			if err := s.appendLifecycleEntry(desig, "body_published", h, length, n, nil); err != nil {
				return err
			}
			lease, err = s.writeLease(h, n)
			return err
		default:
			if st.incarnation >= maxSafeInteger {
				return fmt.Errorf("%w: body incarnation exhausted", ErrSchema)
			}
			now := s.opts.Clock.Now().UTC().Truncate(time.Microsecond)
			if now.Before(desig.lastAt) {
				return ErrClockRegression
			}
			progressed = true
			n := st.incarnation + 1
			if err := s.writeIncarnation(h, n); err != nil {
				return err
			}
			if err := s.linkBody(tmp, h, length); err != nil {
				return err
			}
			staged = false
			if err := s.appendLifecycleEntry(desig, "body_published", h, length, n, nil); err != nil {
				return err
			}
			lease, err = s.writeLease(h, n)
			return err
		}
	})
	completed = true
	if err != nil {
		err = s.bodyErr(err, progressed, h, length)
		if staged {
			err = s.discardStaged(tmp, err)
		}
		return BodyRef{}, PublicationLease{}, err
	}
	if staged {
		s.root.Remove(tmp)
	}
	return BodyRef{Digest: "sha256:" + h, Length: length, MediaType: mediaType}, lease, nil
}

// discardStaged removes a staged temporary file after err. A removal failure
// is joined to err as an unlink IOError with err's own determinacy, and the
// file stays as a tmp leftover that Verify reports.
func (s *Store) discardStaged(name string, err error) error {
	if rerr := s.root.Remove(name); rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
		return errors.Join(err, ioErr("unlink", rerr, IsIndeterminate(err)))
	}
	return err
}

// stage streams r into an exclusive temporary file, computing its digest and
// length, and fsyncs it.
func (s *Store) stage(ctx context.Context, r io.Reader) (name, h string, length int64, err error) {
	id := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, id); err != nil {
		return "", "", 0, fmt.Errorf("recordprims: entropy unavailable: %w", err)
	}
	name = dirBodyTmp + "/" + hex.EncodeToString(id)
	f, err := s.root.CreateExclusive(name)
	if err != nil {
		return "", "", 0, ioErr("create", err, false)
	}
	finished := false
	defer func() {
		if !finished {
			f.Close()
			s.root.Remove(name)
		}
	}()
	fail := func(e error) (string, string, int64, error) {
		finished = true
		f.Close()
		return "", "", 0, s.discardStaged(name, e)
	}
	if err := s.fault("body.tmp_created"); err != nil {
		return fail(err)
	}
	sum := sha256.New()
	buf := make([]byte, 64<<10)
	for {
		if err := ctx.Err(); err != nil {
			return fail(err)
		}
		k, rerr := r.Read(buf)
		if k > 0 {
			if length+int64(k) > maxBodyLength {
				return fail(fmt.Errorf("%w: body length exceeds %d bytes", ErrSchema, maxBodyLength))
			}
			sum.Write(buf[:k])
			if _, err := f.Write(buf[:k]); err != nil {
				return fail(ioErr("write", err, false))
			}
			length += int64(k)
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return fail(fmt.Errorf("recordprims: reading body input: %w", rerr))
		}
	}
	if err := s.fault("body.tmp_written"); err != nil {
		return fail(err)
	}
	if err := f.Sync(); err != nil {
		return fail(ioErr("fsync", err, false))
	}
	if err := f.Close(); err != nil {
		finished = true
		return "", "", 0, s.discardStaged(name, ioErr("close", err, false))
	}
	if err := s.fault("body.tmp_fsynced"); err != nil {
		finished = true
		return "", "", 0, s.discardStaged(name, err)
	}
	finished = true
	return name, hex.EncodeToString(sum.Sum(nil)), length, nil
}

// writeIncarnation durably advances the digest's incarnation in the prewrite
// durable ledger before attempting body_published(n).
func (s *Store) writeIncarnation(h string, n uint64) error {
	if err := s.replaceFile(incarnationFile(h), []byte(strconv.FormatUint(n, 10)), "body.incarnation_written"); err != nil {
		return err
	}
	return s.fault("body.incarnation_fsynced")
}

// linkBody moves the staged file into place. A present file was verified
// intact by bodyState, so it is kept and the staged copy discarded.
func (s *Store) linkBody(tmp, h string, length int64) error {
	dst := bodyFile(h)
	present, err := s.exists(dst)
	if err != nil {
		return err
	}
	if present {
		s.root.Remove(tmp)
	} else {
		shard := path.Dir(dst)
		if err := s.root.Mkdir(shard, s.fso); err != nil {
			return ioErr("mkdir", err, true)
		}
		if err := s.root.SyncDir(dirBodySHA256); err != nil {
			return ioErr("fsync_dir", err, true)
		}
		if err := s.root.Rename(tmp, dst); err != nil {
			return ioErr("rename", err, true)
		}
	}
	if err := s.fault("body.renamed"); err != nil {
		return err
	}
	return s.syncBodyDir(h, "body.dir_fsynced")
}

// syncBodyDir fsyncs the directory holding the body file. A failure is never
// acknowledged as completion.
func (s *Store) syncBodyDir(h, phase string) error {
	if err := s.root.SyncDir(path.Dir(bodyFile(h))); err != nil {
		return ioErr("fsync_dir", err, true)
	}
	return s.fault(phase)
}

func (s *Store) writeLease(h string, n uint64) (PublicationLease, error) {
	id := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, id); err != nil {
		return PublicationLease{}, fmt.Errorf("recordprims: entropy unavailable: %w", err)
	}
	dir := dirBodyPending + "/" + h
	if err := s.root.Mkdir(dir, s.fso); err != nil {
		return PublicationLease{}, ioErr("mkdir", err, true)
	}
	exp := s.opts.Clock.Now().Add(s.opts.PendingBodyTTL)
	b, _ := json.Marshal(leaseFile{Incarnation: n, ExpiresAt: FormatInstant(exp)})
	l := PublicationLease{ID: hex.EncodeToString(id), Digest: "sha256:" + h, ExpiresAt: exp.UTC().Truncate(time.Microsecond)}
	if err := writeFileDurable(s.root, dir+"/"+l.ID, b); err != nil {
		return PublicationLease{}, err
	}
	if err := s.root.SyncDir(dirBodyPending); err != nil {
		return PublicationLease{}, ioErr("fsync_dir", err, true)
	}
	if err := s.fault("body.lease_written"); err != nil {
		return PublicationLease{}, err
	}
	return l, nil
}

// replaceFile durably writes name through an exclusive temporary file and an
// atomic rename, then fsyncs the containing directory.
func (s *Store) replaceFile(name string, data []byte, phase string) error {
	id := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, id); err != nil {
		return fmt.Errorf("recordprims: entropy unavailable: %w", err)
	}
	tmp := dirBodyTmp + "/" + hex.EncodeToString(id)
	if err := writeFileDurable(s.root, tmp, data); err != nil {
		return err
	}
	if err := s.root.Rename(tmp, name); err != nil {
		s.root.Remove(tmp)
		return ioErr("rename", err, true)
	}
	if err := s.fault(phase); err != nil {
		return err
	}
	if err := s.root.SyncDir(path.Dir(name)); err != nil {
		return ioErr("fsync_dir", err, true)
	}
	return nil
}

// bodyErr turns a failure inside a body operation into the published error:
// the error itself when nothing durable changed, otherwise ErrBodyIncomplete
// with the reader state after the failure.
func (s *Store) bodyErr(err error, progressed bool, h string, length int64) error {
	if !progressed {
		return err
	}
	state := "unknown"
	s.withLock(func() error {
		if st, e := s.bodyState(h, length); e == nil {
			state = string(st.kind)
		}
		return nil
	})
	return &ErrBodyIncomplete{State: state}
}

// RemoveBody removes a body's bytes when nothing protects them. Every outcome
// is a result; incomplete durable progress returns ErrBodyIncomplete and is
// completed by retrying the same call.
func (s *Store) RemoveBody(ctx context.Context, req RemoveBodyRequest) (RemoveBodyResult, error) {
	if !reDigest.MatchString(req.Digest) || req.Length < 0 || req.Length > maxSafeInteger {
		return RemoveBodyResult{}, fmt.Errorf("%w: digest or length", ErrSchema)
	}
	if c := req.CompleteIncarnation; c != nil && (*c < 1 || *c > maxSafeInteger) {
		return RemoveBodyResult{}, fmt.Errorf("%w: complete incarnation", ErrSchema)
	}
	if req.Reason != RemoveReasonRetention && req.Reason != RemoveReasonOperator && req.Reason != RemoveReasonRightsRestricted {
		return RemoveBodyResult{}, fmt.Errorf("%w: removal reason", ErrSchema)
	}
	if !validProvenance(req.Provenance) {
		return RemoveBodyResult{}, fmt.Errorf("%w: removal provenance", ErrSchema)
	}
	if !validAuthorization(req.Authorization) {
		return RemoveBodyResult{}, fmt.Errorf("%w: removal authorization", ErrSchema)
	}
	if err := ctx.Err(); err != nil {
		return RemoveBodyResult{}, err
	}
	h := digestHex(req.Digest)
	var out RemoveBodyOutcome
	progressed := false
	err := s.withLock(func() error {
		if err := s.ensureBodyDirs(); err != nil {
			return err
		}
		// G4c step 1: pre-validate designated stream at a complete valid cut.
		desig, err := s.readDesignatedState()
		if err != nil {
			return err
		}
		// G15 step 0: observe the bytes under the lock.
		st, err := s.evalBodyState(desig, h, req.Length)
		if err != nil {
			return err
		}
		if st.kind == stateCorrupt {
			out = BodyConflict
			return nil
		}
		// G4c step 2: CompleteIncarnation check.
		if req.CompleteIncarnation != nil {
			if st.bound == 0 || *req.CompleteIncarnation != st.bound || st.incarnation != st.bound {
				out = BodySuperseded
				return nil
			}
		}
		// G15: states with no state change before steps 3-5.
		switch st.kind {
		case stateIntegrityLoss:
			out = BodyMissing
			return nil
		case stateUnpublished, stateRemoved:
			out = BodyAbsent
			return nil
		case statePublishing:
			out = BodyPending
			return nil
		}

		// G4c step 3: removal_pending with bytes already absent completes by
		// syncing the directory and appending body_removed(bound).
		if st.kind == stateRemovalPending {
			present, _, err := s.bodyFilePresent(h, req.Length)
			if err != nil {
				return err
			}
			if !present {
				if st.incarnation > st.bound {
					// A newer PutBody incremented the prewrite ledger; do not
					// append body_removed if a new publication was initiated.
					out = BodyPending
					return nil
				}
				progressed = true
				if err := s.syncBodyDir(h, "remove.dir_fsynced"); err != nil {
					return err
				}
				if err := s.appendLifecycleEntry(desig, "body_removed", h, req.Length, st.bound, nil); err != nil {
					return err
				}
				out = BodyAbsent
				return nil
			}
		}

		// G4c steps 4 & 5: removal_pending with bytes present, or available
		// with bytes present. Re-verify incarnation and full eligibility.
		if st.incarnation > st.bound {
			out = BodyPending
			return nil
		}
		eligOut, err := s.checkRemovalEligibility(h, req.Length)
		if err != nil || eligOut != "" {
			out = eligOut
			return err
		}
		if err := s.fault("remove.checked"); err != nil {
			return err
		}
		if err := s.fault("remove.decided"); err != nil {
			return err
		}

		if st.kind == stateAvailable {
			if err := s.appendLifecycleEntry(desig, "body_removal_started", h, req.Length, st.bound, &req); err != nil {
				var ioe *IOError
				if errors.As(err, &ioe) && !ioe.indeterminate {
					// G3: partial write on designated stream was aborted back
					// to the prior cut; nothing durable changed.
					return err
				}
				progressed = true
				return err
			}
		}
		progressed = true
		if err := s.root.Remove(bodyFile(h)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return ioErr("unlink", err, true)
		}
		if err := s.fault("remove.unlinked"); err != nil {
			return err
		}
		if err := s.syncBodyDir(h, "remove.dir_fsynced"); err != nil {
			return err
		}
		if err := s.appendLifecycleEntry(desig, "body_removed", h, req.Length, st.bound, nil); err != nil {
			return err
		}
		out = BodyRemoved
		return nil
	})
	if err != nil {
		return RemoveBodyResult{}, s.bodyErr(err, progressed, h, req.Length)
	}
	return RemoveBodyResult{Outcome: out}, nil
}

// checkRemovalEligibility evaluates G4c step 5 eligibility across all non-designated
// streams in the store, plus active leases.
func (s *Store) checkRemovalEligibility(h string, length int64) (RemoveBodyOutcome, error) {
	streams, err := s.root.ReadDirNames(dirStreams, s.fso)
	if err != nil {
		return BodyProtectionUnknown, nil
	}
	desigHash := streamHash(s.profile.Runtime.BodyRemovalStream)
	target := journal.BodyKey{Digest: "sha256:" + h, Length: length}

	var anyHeld, anyReferenced bool
	for _, sh := range streams {
		if sh == desigHash {
			continue
		}
		st, ok := s.replaySharingStreamDir(sh)
		if !ok {
			return BodyProtectionUnknown, nil
		}
		if st.BodyHeld(target) {
			anyHeld = true
		}
		if st.BodyReferenced(target) {
			anyReferenced = true
		}
	}
	if anyHeld {
		return BodyHeld, nil
	}
	leased, err := s.unexpiredLease(h)
	if err != nil {
		return "", err
	}
	if leased {
		return BodyPending, nil
	}
	if anyReferenced {
		return BodyStillReferenced, nil
	}
	return "", nil
}

func (s *Store) replaySharingStreamDir(sh string) (*journal.State, bool) {
	sdir := dirStreams + "/" + sh
	if sealed, err := s.exists(sdir + "/SEALED"); err != nil || sealed {
		return nil, false
	}
	raw, err := readFileVerified(s.root, sdir+"/journal.ndjson", s.fso, maxJournalBytes)
	if err != nil {
		return nil, false
	}
	nodes, err := wire.IngestLines(raw, wire.KindJournalEntry)
	if err != nil {
		return nil, false
	}
	st, err := journal.New(s.windowNode())
	if err != nil {
		return nil, false
	}
	st.SetProfile(s.profileNode)
	for i, node := range nodes {
		if err := shape.Validate("v1/journal-entry.schema.json", node); err != nil {
			return nil, false
		}
		if i == 0 && node.Get("continues") != nil {
			if !s.attachContinuationChain(st, node.Get("continues"), map[string]bool{sh: true}) {
				return nil, false
			}
		}
		if err := st.Ingest(node); err != nil {
			return nil, false
		}
	}
	if err := journal.CheckJournal(st.Journal()); err != nil {
		return nil, false
	}
	if streamHash(st.Stream()) != sh {
		return nil, false
	}
	if st.ReviewPending() {
		return nil, false
	}
	return st, true
}

func (s *Store) attachContinuationChain(dst *journal.State, cont *wire.Node, seen map[string]bool) bool {
	if cont == nil || cont.Kind != wire.Object || len(seen) > 16 {
		return false
	}
	capNode, streamNode := cont.Get("capability"), cont.Get("stream")
	if capNode == nil || capNode.Kind != wire.String || capNode.Str != "contract: durable-record/v1" ||
		streamNode == nil || streamNode.Kind != wire.String {
		return false
	}
	srcHash := streamHash(streamNode.Str)
	if seen[srcHash] || srcHash == streamHash(s.profile.Runtime.BodyRemovalStream) {
		return false
	}
	seen[srcHash] = true
	srcSt, ok := s.replaySharingStreamDir(srcHash)
	if !ok || srcSt.Stream() != streamNode.Str {
		return false
	}
	srcArr := &wire.Node{Kind: wire.Array, Elems: srcSt.Journal()}
	if err := dst.SetSourceChain(journal.SourceInput{SourceJournal: srcArr}); err != nil {
		return false
	}
	return true
}

// unexpiredLease reports whether any publication lease for the digest is
// still live. An unreadable lease counts as live (fail closed).
func (s *Store) unexpiredLease(h string) (bool, error) {
	dir := dirBodyPending + "/" + h
	names, err := s.root.ReadDirNames(dir, s.fso)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return true, nil
	}
	now := s.opts.Clock.Now()
	for _, n := range names {
		b, err := readFileVerified(s.root, dir+"/"+n, s.fso, 1<<12)
		if err != nil {
			return true, nil
		}
		var l leaseFile
		if json.Unmarshal(b, &l) != nil {
			return true, nil
		}
		exp, err := ParseInstant(l.ExpiresAt)
		if err != nil || now.Before(exp) {
			return true, nil
		}
	}
	return false, nil
}

// consumeLease removes a valid, unexpired publication lease for digest.
func (s *Store) consumeLease(h string, lease PublicationLease) error {
	if lease.ID == "" || lease.Digest != "sha256:"+h {
		return fmt.Errorf("%w: invalid publication lease", ErrSchema)
	}
	name := dirBodyPending + "/" + h + "/" + lease.ID
	b, err := readFileVerified(s.root, name, s.fso, 1<<12)
	if err != nil {
		return fmt.Errorf("%w: publication lease not found", ErrSchema)
	}
	var l leaseFile
	if json.Unmarshal(b, &l) != nil {
		return fmt.Errorf("%w: publication lease invalid", ErrStoreInvalid)
	}
	exp, err := ParseInstant(l.ExpiresAt)
	if err != nil || !s.opts.Clock.Now().Before(exp) {
		return fmt.Errorf("%w: publication lease expired", ErrSchema)
	}
	if err := s.root.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return ioErr("unlink", err, true)
	}
	return s.root.SyncDir(dirBodyPending + "/" + h)
}

var _ = jfile.ErrAborted
