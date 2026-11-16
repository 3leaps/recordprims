package bucketcas

import (
	"context"
	"errors"
	"fmt"

	"github.com/3leaps/recordprims/internal/stratum"
)

// errCommitWithoutError reports a backend write that returned a nil error
// without a committed outcome.
var errCommitWithoutError = errors.New("bucketcas: backend reported a non-committed outcome without an error")

// commitContractStore normalizes backend write results so that a nil error
// always means CommitCommitted. Every caller in this package gates progress
// on err != nil, so a backend that returns (CommitUnknown, nil) or
// (CommitNotCommitted, nil) must not be able to look like a durable write.
// A non-nil error paired with CommitCommitted is contradictory and is
// reported as CommitUnknown.
type commitContractStore struct {
	inner stratum.ConditionalObjectStore
}

func enforceCommitContract(obj stratum.ConditionalObjectStore) stratum.ConditionalObjectStore {
	if obj == nil {
		return nil
	}
	if c, ok := obj.(*commitContractStore); ok {
		return c
	}
	return &commitContractStore{inner: obj}
}

func normalizeCommit(op, key string, commit stratum.CommitKnowledge, err error) (stratum.CommitKnowledge, error) {
	switch {
	case err == nil && commit == stratum.CommitCommitted:
		return commit, nil
	case err == nil:
		return commit, fmt.Errorf("%w: %s %s returned %v", errCommitWithoutError, op, key, commit)
	case commit == stratum.CommitCommitted:
		return stratum.CommitUnknown, err
	default:
		return commit, err
	}
}

func (c *commitContractStore) Capabilities() stratum.StoreCaps { return c.inner.Capabilities() }

func (c *commitContractStore) GetCoherent(ctx context.Context, key string) ([]byte, string, int64, error) {
	return c.inner.GetCoherent(ctx, key)
}

func (c *commitContractStore) PutIfAbsent(ctx context.Context, key string, body []byte) (string, stratum.CommitKnowledge, stratum.ConflictKind, error) {
	v, commit, conflict, err := c.inner.PutIfAbsent(ctx, key, body)
	commit, err = normalizeCommit("put_if_absent", key, commit, err)
	return v, commit, conflict, err
}

func (c *commitContractStore) PutIfMatch(ctx context.Context, key, expected string, body []byte) (string, stratum.CommitKnowledge, stratum.ConflictKind, error) {
	v, commit, conflict, err := c.inner.PutIfMatch(ctx, key, expected, body)
	commit, err = normalizeCommit("put_if_match", key, commit, err)
	return v, commit, conflict, err
}

func (c *commitContractStore) ListPrefix(ctx context.Context, prefix string) ([]string, error) {
	return c.inner.ListPrefix(ctx, prefix)
}

func (c *commitContractStore) Delete(ctx context.Context, key string) (stratum.CommitKnowledge, error) {
	commit, err := c.inner.Delete(ctx, key)
	return normalizeCommit("delete", key, commit, err)
}
