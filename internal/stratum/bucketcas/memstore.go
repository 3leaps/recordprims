package bucketcas

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/3leaps/recordprims/internal/stratum"
)

// PutIfMatchFaultHook intercepts a PutIfMatch call on MemStore for deterministic
// fault injection (412 contention races and CommitUnknown lost-reply scenarios).
//
// When hooked is true, MemStore executes the write if and only if applyWrite is
// true, and returns (overrideCommit, overrideErr) to the caller.
type PutIfMatchFaultHook func(key, expectedValidator string, body []byte) (hooked bool, applyWrite bool, overrideCommit stratum.CommitKnowledge, overrideErr error)

// PutIfAbsentFaultHook intercepts a PutIfAbsent call on MemStore.
type PutIfAbsentFaultHook func(key string, body []byte) (hooked bool, applyWrite bool, overrideCommit stratum.CommitKnowledge, overrideErr error)

type memObject struct {
	body      []byte
	validator string
}

// MemStore is an in-memory, goroutine-safe implementation of
// stratum.ConditionalObjectStore with deterministic fault-injection hooks for
// testing 412 contention races, CommitUnknown lost replies, capability checks,
// and checkpoint cold-start read counts.
type MemStore struct {
	mu          sync.Mutex
	caps        stratum.StoreCaps
	objs        map[string]memObject
	nextVer     uint64
	ifMatchHook PutIfMatchFaultHook
	ifAbsHook   PutIfAbsentFaultHook

	getCalls         atomic.Int64
	getSegmentCalls  atomic.Int64
	putIfAbsentCalls atomic.Int64
	putIfMatchCalls  atomic.Int64
}

// NewMemStore returns a MemStore advertising full conditional-PUT and coherent
// read capabilities.
func NewMemStore() *MemStore {
	return &MemStore{
		caps: stratum.StoreCaps{
			ConditionalPutMustNotExist:   true,
			ConditionalPutValidatorMatch: true,
			CoherentRead:                 true,
		},
		objs: make(map[string]memObject),
	}
}

// SetCapabilities overrides the capabilities advertised by Capabilities().
func (m *MemStore) SetCapabilities(caps stratum.StoreCaps) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.caps = caps
}

// SetPutIfMatchHook installs a fault-injection hook for PutIfMatch.
func (m *MemStore) SetPutIfMatchHook(h PutIfMatchFaultHook) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ifMatchHook = h
}

// SetPutIfAbsentHook installs a fault-injection hook for PutIfAbsent.
func (m *MemStore) SetPutIfAbsentHook(h PutIfAbsentFaultHook) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ifAbsHook = h
}

// ResetCounters zeroes the operation counters used by cold-start tests.
func (m *MemStore) ResetCounters() {
	m.getCalls.Store(0)
	m.getSegmentCalls.Store(0)
	m.putIfAbsentCalls.Store(0)
	m.putIfMatchCalls.Store(0)
}

// SegmentGetCount returns the number of GetCoherent calls targeting a segment
// object since the last ResetCounters call.
func (m *MemStore) SegmentGetCount() int64 {
	return m.getSegmentCalls.Load()
}

// TotalGetCount returns the total number of GetCoherent calls since the last
// ResetCounters call.
func (m *MemStore) TotalGetCount() int64 {
	return m.getCalls.Load()
}

// Capabilities implements stratum.ConditionalObjectStore.
func (m *MemStore) Capabilities() stratum.StoreCaps {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.caps
}

// GetCoherent implements stratum.ConditionalObjectStore.
func (m *MemStore) GetCoherent(ctx context.Context, key string) ([]byte, string, int64, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", 0, err
	}
	m.getCalls.Add(1)
	if strings.Contains(key, "/segments/") {
		m.getSegmentCalls.Add(1)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	obj, ok := m.objs[key]
	if !ok {
		return nil, "", 0, stratum.ErrNotFound
	}
	cp := bytes.Clone(obj.body)
	return cp, obj.validator, int64(len(cp)), nil
}

// PutIfAbsent implements stratum.ConditionalObjectStore.
func (m *MemStore) PutIfAbsent(ctx context.Context, key string, body []byte) (string, stratum.CommitKnowledge, stratum.ConflictKind, error) {
	if err := ctx.Err(); err != nil {
		return "", stratum.CommitNotStarted, stratum.ConflictNone, err
	}
	m.putIfAbsentCalls.Add(1)

	m.mu.Lock()
	hook := m.ifAbsHook
	m.mu.Unlock()

	if hook != nil {
		if hooked, applyWrite, commit, err := hook(key, body); hooked {
			var v string
			if applyWrite {
				m.mu.Lock()
				if _, exists := m.objs[key]; !exists {
					m.nextVer++
					v = fmt.Sprintf("etag-%d", m.nextVer)
					m.objs[key] = memObject{body: bytes.Clone(body), validator: v}
				}
				m.mu.Unlock()
			}
			if commit == stratum.CommitUnknown {
				if err == nil {
					err = errors.New("simulated transport drop after dispatch")
				}
				return "", stratum.CommitUnknown, stratum.ConflictNone, err
			}
			return v, commit, stratum.ConflictNone, err
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.objs[key]; exists {
		return "", stratum.CommitNotCommitted, stratum.ConflictAlreadyExists, errors.New("memstore: object already exists")
	}
	m.nextVer++
	v := fmt.Sprintf("etag-%d", m.nextVer)
	m.objs[key] = memObject{
		body:      bytes.Clone(body),
		validator: v,
	}
	return v, stratum.CommitCommitted, stratum.ConflictNone, nil
}

// PutIfMatch implements stratum.ConditionalObjectStore.
func (m *MemStore) PutIfMatch(ctx context.Context, key, expectedValidator string, body []byte) (string, stratum.CommitKnowledge, stratum.ConflictKind, error) {
	if err := ctx.Err(); err != nil {
		return "", stratum.CommitNotStarted, stratum.ConflictNone, err
	}
	m.putIfMatchCalls.Add(1)

	m.mu.Lock()
	hook := m.ifMatchHook
	m.mu.Unlock()

	if hook != nil {
		if hooked, applyWrite, commit, err := hook(key, expectedValidator, body); hooked {
			var v string
			if applyWrite {
				m.mu.Lock()
				cur, exists := m.objs[key]
				if exists && cur.validator == expectedValidator {
					m.nextVer++
					v = fmt.Sprintf("etag-%d", m.nextVer)
					m.objs[key] = memObject{body: bytes.Clone(body), validator: v}
				}
				m.mu.Unlock()
			}
			if commit == stratum.CommitUnknown {
				if err == nil {
					err = errors.New("simulated transport drop after dispatch")
				}
				return "", stratum.CommitUnknown, stratum.ConflictNone, err
			}
			return v, commit, stratum.ConflictNone, err
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	cur, exists := m.objs[key]
	if !exists || cur.validator != expectedValidator {
		return "", stratum.CommitNotCommitted, stratum.ConflictTokenMismatch, errors.New("memstore: validator token mismatch")
	}
	m.nextVer++
	v := fmt.Sprintf("etag-%d", m.nextVer)
	m.objs[key] = memObject{
		body:      bytes.Clone(body),
		validator: v,
	}
	return v, stratum.CommitCommitted, stratum.ConflictNone, nil
}

// ListPrefix implements stratum.ConditionalObjectStore.
func (m *MemStore) ListPrefix(ctx context.Context, prefix string) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var keys []string
	for k := range m.objs {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	return keys, nil
}

// Delete implements stratum.ConditionalObjectStore.
func (m *MemStore) Delete(ctx context.Context, key string) (stratum.CommitKnowledge, error) {
	if err := ctx.Err(); err != nil {
		return stratum.CommitNotStarted, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.objs, key)
	return stratum.CommitCommitted, nil
}
