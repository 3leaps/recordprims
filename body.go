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
	"sort"
	"strconv"
	"strings"
	"time"
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
	Stream   string
	RecordID string
	Revision uint64
	Digest   string
}

// RemoveBodyRequest asks the store to remove a body's bytes.
type RemoveBodyRequest struct {
	Digest string
	Length int64
	// DropSet lists the pins whose references the caller is retiring along
	// with the body.
	DropSet []Pin
	// CompleteIncarnation, when set, completes an earlier removal of that
	// incarnation only; a newer publication makes the request superseded.
	CompleteIncarnation *uint64
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

// bodyStateKind is the single body state every body operation derives from.
type bodyStateKind string

const (
	stateLive                  bodyStateKind = "live"
	stateRemoving              bodyStateKind = "removing"
	statePublishing            bodyStateKind = "publishing"
	statePublicationIncomplete bodyStateKind = "publication_incomplete"
	stateRemoved               bodyStateKind = "removed"
	stateMissing               bodyStateKind = "missing"
	stateInconsistent          bodyStateKind = "inconsistent"
	stateUnknownDigest         bodyStateKind = "unknown_digest"
	stateCorrupt               bodyStateKind = "corrupt"
)

// bodyState is the state of one (digest, length) and the incarnation it was
// evaluated at.
type bodyState struct {
	kind        bodyStateKind
	incarnation uint64
}

// Body directory layout, relative to the store root.
const (
	dirBodySHA256      = dirBodies + "/sha256"
	dirBodyTmp         = dirBodies + "/tmp"
	dirBodyIncarnation = dirBodies + "/incarnation"
	dirBodyPublished   = dirBodies + "/published"
	dirBodyPending     = dirBodies + "/pending"
	dirBodyRemoved     = dirBodies + "/removed"
)

var (
	reDigest    = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	reMediaType = regexp.MustCompile(`^[a-z0-9][a-z0-9!#$&^_.+-]{0,126}/[a-z0-9][a-z0-9!#$&^_.+-]{0,126}$`)
	reStream    = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)
	reRecordID  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
)

// errBodyConflict is returned by PutBody when the stored bytes for the digest
// are corrupt or the body's records are inconsistent.
var errBodyConflict = fmt.Errorf("%w: body_conflict", ErrStoreInvalid)

// digestHex returns the 64-hex part of a validated digest. Names on disk use
// the bare hex: the "sha256:" prefix is not portable in file names.
func digestHex(digest string) string { return strings.TrimPrefix(digest, "sha256:") }

func bodyFile(h string) string        { return dirBodySHA256 + "/" + h[:2] + "/" + h }
func incarnationFile(h string) string { return dirBodyIncarnation + "/" + h }
func publishedFile(h string, n uint64) string {
	return dirBodyPublished + "/" + h + "-" + strconv.FormatUint(n, 10)
}
func tombstoneFile(h string, length int64) string {
	return dirBodyRemoved + "/" + h + "-" + strconv.FormatInt(length, 10) + ".json"
}

// tombstone is the retention record written before a body's bytes are
// unlinked.
type tombstone struct {
	Digest      string `json:"digest"`
	Length      int64  `json:"length"`
	RemovedAt   string `json:"removed_at"`
	DropSetHash string `json:"drop_set_hash"`
	Incarnation uint64 `json:"incarnation"`
}

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
	for _, d := range []string{dirBodies, dirBodySHA256, dirBodyTmp, dirBodyIncarnation, dirBodyPublished, dirBodyPending, dirBodyRemoved} {
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

// bodyState evaluates the state of (digest, length) under the store lock.
func (s *Store) bodyState(h string, length int64) (bodyState, error) {
	n, ok, err := s.readIncarnation(h)
	if err != nil {
		return bodyState{}, err
	}
	present, intact, err := s.bodyFilePresent(h, length)
	if err != nil {
		return bodyState{}, err
	}
	// A digest or length mismatch overrides every other state.
	if present && !intact {
		return bodyState{kind: stateCorrupt, incarnation: n}, nil
	}
	if !ok {
		if present {
			// Bytes with no recorded incarnation cannot arise from the
			// protocols: an integrity failure.
			return bodyState{kind: stateInconsistent}, nil
		}
		return bodyState{kind: stateUnknownDigest}, nil
	}
	pub, err := s.exists(publishedFile(h, n))
	if err != nil {
		return bodyState{}, err
	}
	tomb, err := s.tombstoneFor(h, length, n)
	if err != nil {
		return bodyState{}, err
	}
	var k bodyStateKind
	switch {
	case present && pub && !tomb:
		k = stateLive
	case present && pub && tomb:
		k = stateRemoving
	case present && !pub && !tomb:
		k = statePublishing
	case present && !pub && tomb:
		k = stateInconsistent
	case !present && !pub:
		k = statePublicationIncomplete
	case !present && pub && tomb:
		k = stateRemoved
	default:
		k = stateMissing
	}
	return bodyState{kind: k, incarnation: n}, nil
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
	if err != nil || n == 0 {
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

// tombstoneFor reports a tombstone for (digest, length) at incarnation n. A
// tombstone for any other incarnation is stale and ignored.
func (s *Store) tombstoneFor(h string, length int64, n uint64) (bool, error) {
	b, err := readFileVerified(s.root, tombstoneFile(h, length), s.fso, 1<<12)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, ioErr("read", err, false)
	}
	var t tombstone
	if err := json.Unmarshal(b, &t); err != nil {
		return false, fmt.Errorf("%w: tombstone invalid", ErrStoreInvalid)
	}
	return t.Incarnation == n, nil
}

// PutBody streams r into the store as a content-addressed body and returns
// its reference and a publication lease. It is idempotent by state: after any
// error, retry the same call.
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
	defer func() {
		if staged {
			s.root.Remove(tmp)
		}
	}()

	var lease PublicationLease
	// progressed is set once the operation may have changed durable state;
	// any later failure is indeterminate.
	progressed := false
	err = s.withLock(func() error {
		st, err := s.bodyState(h, length)
		if err != nil {
			return err
		}
		if st.kind == stateCorrupt || st.kind == stateInconsistent {
			return errBodyConflict
		}
		progressed = true
		n := st.incarnation
		switch st.kind {
		case stateLive:
		case statePublishing:
			if err := s.syncBodyDir(h, "body.dir_fsynced"); err != nil {
				return err
			}
			if err := s.publish(h, n); err != nil {
				return err
			}
		default:
			n++
			if err := s.writeIncarnation(h, n); err != nil {
				return err
			}
			if err := s.linkBody(tmp, h, length); err != nil {
				return err
			}
			staged = false
			if err := s.publish(h, n); err != nil {
				return err
			}
		}
		lease, err = s.writeLease(h, n)
		return err
	})
	if err != nil {
		return BodyRef{}, PublicationLease{}, s.bodyErr(err, progressed, h, length)
	}
	return BodyRef{Digest: "sha256:" + h, Length: length, MediaType: mediaType}, lease, nil
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
	fail := func(e error) (string, string, int64, error) {
		f.Close()
		s.root.Remove(name)
		return "", "", 0, e
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
		s.root.Remove(name)
		return "", "", 0, ioErr("close", err, false)
	}
	if err := s.fault("body.tmp_fsynced"); err != nil {
		s.root.Remove(name)
		return "", "", 0, err
	}
	return name, hex.EncodeToString(sum.Sum(nil)), length, nil
}

// writeIncarnation durably advances the digest's incarnation, which makes
// every tombstone for an earlier incarnation stale.
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

func (s *Store) publish(h string, n uint64) error {
	if err := writeFileDurable(s.root, publishedFile(h, n), []byte("{}")); err != nil {
		return err
	}
	return s.fault("body.published_written")
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
// with the body state after the failure.
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
	if !reDigest.MatchString(req.Digest) || req.Length < 0 {
		return RemoveBodyResult{}, fmt.Errorf("%w: digest or length", ErrSchema)
	}
	dropHash, err := dropSetHash(req.DropSet)
	if err != nil {
		return RemoveBodyResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return RemoveBodyResult{}, err
	}
	h := digestHex(req.Digest)
	var out RemoveBodyOutcome
	progressed := false
	err = s.withLock(func() error {
		if err := s.ensureBodyDirs(); err != nil {
			return err
		}
		st, err := s.bodyState(h, req.Length)
		if err != nil {
			return err
		}
		if st.kind == stateRemoved {
			progressed = true
		}
		out, err = s.removeDecision(st, h, req)
		if err != nil || out != "" {
			return err
		}
		if err := s.fault("remove.decided"); err != nil {
			return err
		}
		progressed = true
		if st.kind == stateLive {
			t := tombstone{
				Digest:      req.Digest,
				Length:      req.Length,
				RemovedAt:   FormatInstant(s.opts.Clock.Now()),
				DropSetHash: dropHash,
				Incarnation: st.incarnation,
			}
			b, _ := json.Marshal(t)
			if err := s.replaceFile(tombstoneFile(h, req.Length), b, "remove.tombstone_written"); err != nil {
				return err
			}
			if err := s.fault("remove.tombstone_fsynced"); err != nil {
				return err
			}
		}
		if err := s.root.Remove(bodyFile(h)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return ioErr("unlink", err, true)
		}
		if err := s.fault("remove.unlinked"); err != nil {
			return err
		}
		if err := s.syncBodyDir(h, "remove.dir_fsynced"); err != nil {
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

// removeDecision applies the removal table in order. An empty outcome with a
// nil error means "perform the removal" (state live or removing).
func (s *Store) removeDecision(st bodyState, h string, req RemoveBodyRequest) (RemoveBodyOutcome, error) {
	switch {
	case st.kind == stateUnknownDigest:
		if req.CompleteIncarnation != nil {
			return BodySuperseded, nil
		}
		return BodyAbsent, nil
	case req.CompleteIncarnation != nil && *req.CompleteIncarnation != st.incarnation:
		return BodySuperseded, nil
	case st.kind == stateCorrupt || st.kind == stateInconsistent:
		return BodyConflict, nil
	case st.kind == statePublicationIncomplete:
		return BodyPublicationIncomplete, nil
	case st.kind == stateRemoved:
		// Complete a removal whose directory fsync may have failed.
		if err := s.syncBodyDir(h, "remove.dir_fsynced"); err != nil {
			return "", err
		}
		return BodyAbsent, nil
	case st.kind == stateMissing:
		return BodyMissing, nil
	case st.kind == statePublishing:
		return BodyPending, nil
	}
	// Protection: holds and references live in stream journals. Until every
	// stream can be enumerated and checked, any stream present leaves the
	// body's protection unknown.
	streams, err := s.root.ReadDirNames(dirStreams, s.fso)
	if err != nil {
		return BodyProtectionUnknown, nil
	}
	if len(streams) > 0 {
		return BodyProtectionUnknown, nil
	}
	leased, err := s.unexpiredLease(h)
	if err != nil {
		return "", err
	}
	if leased {
		return BodyPending, nil
	}
	return "", nil
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

// dropSetHash validates the drop set and returns the sha256 of its sorted,
// canonical JSON encoding. Every field is restricted ASCII, so the encoding
// is canonical by construction.
func dropSetHash(pins []Pin) (string, error) {
	type wirePin struct {
		Digest   string `json:"digest"`
		RecordID string `json:"record_id"`
		Revision uint64 `json:"revision"`
		Stream   string `json:"stream"`
	}
	w := make([]wirePin, 0, len(pins))
	for _, p := range pins {
		if !reStream.MatchString(p.Stream) || !reRecordID.MatchString(p.RecordID) || p.Revision < 1 || !reDigest.MatchString(p.Digest) {
			return "", fmt.Errorf("%w: drop set pin", ErrSchema)
		}
		w = append(w, wirePin{p.Digest, p.RecordID, p.Revision, p.Stream})
	}
	sort.Slice(w, func(i, j int) bool {
		a, b := w[i], w[j]
		if a.Stream != b.Stream {
			return a.Stream < b.Stream
		}
		if a.RecordID != b.RecordID {
			return a.RecordID < b.RecordID
		}
		if a.Revision != b.Revision {
			return a.Revision < b.Revision
		}
		return a.Digest < b.Digest
	})
	b, err := json.Marshal(w)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}
