package bucketcas

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/3leaps/recordprims/internal/journal"
	"github.com/3leaps/recordprims/internal/shape"
	"github.com/3leaps/recordprims/internal/stratum"
	"github.com/3leaps/recordprims/internal/wire"
)

// PutBody uploads content-addressed external body bytes to bodies/sha256/<hex>
// via PutIfAbsent (treating AlreadyExists as idempotent success after digest
// and length verification) and commits body_published on the designated
// body-removal stream via segment PutIfAbsent + head.json PutIfMatch.
func (s *BucketStore) PutBody(ctx context.Context, raw []byte) (digest string, length int64, incarnation int64, err error) {
	sum := sha256.Sum256(raw)
	digHex := hex.EncodeToString(sum[:])
	digest = "sha256:" + digHex
	length = int64(len(raw))
	bKey := s.bodyKey(digHex)

	_, commit, conflict, err := s.obj.PutIfAbsent(ctx, bKey, raw)
	if err != nil {
		if conflict == stratum.ConflictAlreadyExists {
			// Verify existing object matches digest and length before treating as idempotent.
			gotBytes, _, gotLen, gerr := s.obj.GetCoherent(ctx, bKey)
			if gerr != nil {
				return "", 0, 0, gerr
			}
			gotSum := sha256.Sum256(gotBytes)
			if gotLen != length || hex.EncodeToString(gotSum[:]) != digHex {
				return "", 0, 0, fmt.Errorf("%w: existing body object %s failed integrity check", ErrInvalidStore, bKey)
			}
		} else {
			if commit == stratum.CommitUnknown {
				return "", 0, 0, &stratum.IndeterminateError{Op: "put_body_object", Key: digest, Err: err}
			}
			return "", 0, 0, err
		}
	} else if commit != stratum.CommitCommitted {
		return "", 0, 0, &stratum.IndeterminateError{Op: "put_body_object", Key: digest, Err: fmt.Errorf("%w: body object %s not committed", ErrInvalidStore, bKey)}
	}

	jKey := journal.BodyKey{Digest: digest, Length: length}
	for attempt := 0; attempt < s.opts.MaxCASRetries; attempt++ {
		ls, _, exists, err := s.loadStream(ctx, s.bodyRemovalStream)
		if err != nil {
			return "", 0, 0, err
		}
		if !exists {
			return "", 0, 0, fmt.Errorf("%w: designated stream %q missing", ErrInvalidStore, s.bodyRemovalStream)
		}

		readerSt, bound, _ := ls.st.BodyLifecycle(jKey, true)
		if readerSt == "available" {
			return digest, length, bound, nil
		}

		nextInc := bound + 1
		now := s.opts.Now().UTC().Truncate(time.Microsecond)
		if now.Before(ls.lastAt) {
			return "", 0, 0, ErrClockRegression
		}
		atStr := formatInstant(now)
		nextSeq := ls.head.CutSeq + 1
		seqTok := strconv.FormatInt(nextSeq, 10)
		incTok := strconv.FormatInt(nextInc, 10)
		lenTok := strconv.FormatInt(length, 10)

		entry := &wire.Node{
			Kind: wire.Object,
			Fields: []wire.Member{
				{Name: "v", Value: &wire.Node{Kind: wire.Number, Token: "1", IsInt: true, Int: 1}},
				{Name: "seq", Value: &wire.Node{Kind: wire.Number, Token: seqTok, IsInt: true, Int: nextSeq}},
				{Name: "group", Value: &wire.Node{Kind: wire.Number, Token: seqTok, IsInt: true, Int: nextSeq}},
				{Name: "group_size", Value: &wire.Node{Kind: wire.Number, Token: "1", IsInt: true, Int: 1}},
				{Name: "stream", Value: &wire.Node{Kind: wire.String, Str: s.bodyRemovalStream}},
				{Name: "type", Value: &wire.Node{Kind: wire.String, Str: "body_published"}},
				{Name: "at", Value: &wire.Node{Kind: wire.String, Str: atStr}},
				{Name: "body", Value: &wire.Node{
					Kind: wire.Object,
					Fields: []wire.Member{
						{Name: "digest", Value: &wire.Node{Kind: wire.String, Str: digest}},
						{Name: "length", Value: &wire.Node{Kind: wire.Number, Token: lenTok, IsInt: true, Int: length}},
					},
				}},
				{Name: "incarnation", Value: &wire.Node{Kind: wire.Number, Token: incTok, IsInt: true, Int: nextInc}},
			},
		}
		if err := shape.Validate("v1/journal-entry.schema.json", entry); err != nil {
			return "", 0, 0, err
		}
		if err := ls.st.Ingest(entry); err != nil {
			return "", 0, 0, err
		}

		_, commit, conflict, err := s.stageSegmentAndAdvanceHead(ctx, ls, []*wire.Node{entry})
		if err == nil && commit == stratum.CommitCommitted {
			return digest, length, nextInc, nil
		}
		if conflict == stratum.ConflictTokenMismatch {
			continue
		}
		if commit == stratum.CommitUnknown {
			lsAfter, _, okAfter, errAfter := s.loadStream(ctx, s.bodyRemovalStream)
			if errAfter == nil && okAfter {
				if rst, bnd, _ := lsAfter.st.BodyLifecycle(jKey, true); rst == "available" {
					return digest, length, bnd, nil
				}
			}
			return "", 0, 0, &stratum.IndeterminateError{Op: "put_body_head_cas", Key: digest, Err: err}
		}
		return "", 0, 0, err
	}
	return "", 0, 0, ErrCASExhausted
}

// BodyState evaluates the durable-record/v1 reader state for (digest, length)
// by inspecting bodies/sha256/<hex> and the designated body-removal stream.
func (s *BucketStore) BodyState(ctx context.Context, digest string, length int64) (string, error) {
	if len(digest) != 71 || digest[:7] != "sha256:" {
		return "", fmt.Errorf("%w: invalid digest %q", ErrInvalidStore, digest)
	}
	digHex := digest[7:]
	bKey := s.bodyKey(digHex)

	present := false
	raw, _, gotLen, err := s.obj.GetCoherent(ctx, bKey)
	if err != nil {
		if !errors.Is(err, stratum.ErrNotFound) {
			return "", err
		}
	} else {
		sum := sha256.Sum256(raw)
		if gotLen != length || !bytes.Equal([]byte(hex.EncodeToString(sum[:])), []byte(digHex)) {
			return "corrupt", nil
		}
		present = true
	}

	ls, _, exists, err := s.loadStream(ctx, s.bodyRemovalStream)
	if err != nil {
		return "", err
	}
	if !exists {
		return "", fmt.Errorf("%w: designated stream %q missing", ErrInvalidStore, s.bodyRemovalStream)
	}
	st, _, _ := ls.st.BodyLifecycle(journal.BodyKey{Digest: digest, Length: length}, present)
	return st, nil
}
