package bucketcas_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/3leaps/recordprims/internal/stratum"
	"github.com/3leaps/recordprims/internal/stratum/bucketcas"
)

// nilErrStore wraps MemStore and, for one armed key and operation, returns a
// non-committed outcome with a nil error. The interface permits this shape;
// MemStore's own hooks never produce it.
type nilErrStore struct {
	*bucketcas.MemStore

	mu      sync.Mutex
	op      string // "put_if_absent", "put_if_match", or "delete"
	key     string
	commit  stratum.CommitKnowledge
	landed  bool
	tripped int
}

func (s *nilErrStore) arm(op, key string, commit stratum.CommitKnowledge, landed bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.op, s.key, s.commit, s.landed, s.tripped = op, key, commit, landed, 0
}

func (s *nilErrStore) disarm() { s.arm("", "", 0, false) }

func (s *nilErrStore) hit(op, key string) (stratum.CommitKnowledge, bool, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.op != op || s.key != key {
		return 0, false, false
	}
	s.tripped++
	return s.commit, s.landed, true
}

func (s *nilErrStore) PutIfAbsent(ctx context.Context, key string, body []byte) (string, stratum.CommitKnowledge, stratum.ConflictKind, error) {
	if commit, landed, ok := s.hit("put_if_absent", key); ok {
		if landed {
			_, _, _, _ = s.MemStore.PutIfAbsent(ctx, key, body)
		}
		return "", commit, stratum.ConflictNone, nil
	}
	return s.MemStore.PutIfAbsent(ctx, key, body)
}

func (s *nilErrStore) PutIfMatch(ctx context.Context, key, expected string, body []byte) (string, stratum.CommitKnowledge, stratum.ConflictKind, error) {
	if commit, landed, ok := s.hit("put_if_match", key); ok {
		if landed {
			_, _, _, _ = s.MemStore.PutIfMatch(ctx, key, expected, body)
		}
		return "", commit, stratum.ConflictNone, nil
	}
	return s.MemStore.PutIfMatch(ctx, key, expected, body)
}

func (s *nilErrStore) Delete(ctx context.Context, key string) (stratum.CommitKnowledge, error) {
	if commit, landed, ok := s.hit("delete", key); ok {
		if landed {
			_, _ = s.MemStore.Delete(ctx, key)
		}
		return commit, nil
	}
	return s.MemStore.Delete(ctx, key)
}

type nilErrWorld struct {
	ctx   context.Context
	store *nilErrStore
	bs    *bucketcas.BucketStore
}

func newNilErrWorld(t *testing.T) *nilErrWorld {
	t.Helper()
	ctx := context.Background()
	store := &nilErrStore{MemStore: bucketcas.NewMemStore()}
	bs, err := bucketcas.Init(ctx, store, bucketcas.Options{
		Prefix: "nilerr",
		Now:    fixedClock(time.Date(2026, 3, 29, 12, 0, 0, 0, time.UTC)),
	})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	return &nilErrWorld{ctx: ctx, store: store, bs: bs}
}

func (w *nilErrWorld) key(stream, name string) string {
	return "nilerr/streams/" + stream + "/" + name
}

func (w *nilErrWorld) appendRecord(t *testing.T, stream, id string) {
	t.Helper()
	if _, err := w.bs.Append(w.ctx, bucketcas.AppendInput{
		Stream:         stream,
		Record:         makeRecordJSON(stream, id, 1, `{"v":1}`),
		IdempotencyKey: "k-" + stream + "-" + id,
	}); err != nil {
		t.Fatalf("Append %s/%s: %v", stream, id, err)
	}
}

// sealed returns a predecessor stream with two segments, sealed into dst.
func (w *nilErrWorld) sealed(t *testing.T, src, dst string) {
	t.Helper()
	if err := w.bs.OpenStream(w.ctx, src, "single_revision"); err != nil {
		t.Fatalf("OpenStream %s: %v", src, err)
	}
	w.appendRecord(t, src, "r-1")
	w.appendRecord(t, src, "r-2")
	if _, err := w.bs.SealAndCompactStream(w.ctx, bucketcas.CompactionInput{
		SourceStream:       src,
		DestinationStream:  dst,
		Mode:               "single_revision",
		AllowDropUnretired: true,
		ActorKind:          "human",
		Actor:              "alice@example.com",
		AuthRef:            "auth:nilerr",
		AuthDigest:         "sha256:" + strings.Repeat("a", 64),
	}); err != nil {
		t.Fatalf("SealAndCompactStream %s -> %s: %v", src, dst, err)
	}
}

func (w *nilErrWorld) raw(t *testing.T, key string) []byte {
	t.Helper()
	b, _, _, err := w.store.MemStore.GetCoherent(w.ctx, key)
	if err != nil {
		t.Fatalf("read %s: %v", key, err)
	}
	return b
}

func (w *nilErrWorld) segments(t *testing.T, stream string) []string {
	t.Helper()
	keys, err := w.store.MemStore.ListPrefix(w.ctx, w.key(stream, "segments/"))
	if err != nil {
		t.Fatalf("ListPrefix segments %s: %v", stream, err)
	}
	return keys
}

func (w *nilErrWorld) readable(t *testing.T, streams ...string) {
	t.Helper()
	for _, s := range streams {
		if _, _, err := w.bs.State(w.ctx, s); err != nil {
			t.Fatalf("State(%s): %v", s, err)
		}
	}
}

// A reclaimed.json publication that reports (CommitUnknown, nil), whether or
// not the write landed, must stop before the head CAS and before any segment
// delete. A retry then completes reclamation.
func TestReclaimDocUnknownWithoutErrorMakesNoDestructiveProgress(t *testing.T) {
	for _, landed := range []bool{false, true} {
		name := "not-landed"
		if landed {
			name = "landed"
		}
		t.Run(name, func(t *testing.T) {
			w := newNilErrWorld(t)
			w.sealed(t, "src", "dst")
			headKey, reclKey := w.key("src", "head.json"), w.key("src", "reclaimed.json")
			headBefore := w.raw(t, headKey)
			segsBefore := w.segments(t, "src")
			if len(segsBefore) == 0 {
				t.Fatal("expected predecessor segments before reclamation")
			}

			w.store.arm("put_if_absent", reclKey, stratum.CommitUnknown, landed)
			n, _, err := w.bs.ReclaimSealedAncestor(w.ctx, "src", "dst")
			tripped := w.store.tripped
			w.store.disarm()
			if tripped != 1 {
				t.Fatalf("expected the reclaimed.json publication to be intercepted once, got %d", tripped)
			}
			var ind *stratum.IndeterminateError
			if !errors.As(err, &ind) || n != 0 {
				t.Fatalf("got n=%d err=%v, want 0 and *stratum.IndeterminateError", n, err)
			}
			if !bytes.Equal(headBefore, w.raw(t, headKey)) {
				t.Fatal("head.json changed after an indeterminate reclaimed.json publication")
			}
			if got := w.segments(t, "src"); len(got) != len(segsBefore) {
				t.Fatalf("segments deleted after an indeterminate publication: before=%v after=%v", segsBefore, got)
			}
			w.readable(t, "src", "dst")

			n, _, err = w.bs.ReclaimSealedAncestor(w.ctx, "src", "dst")
			if err != nil || n == 0 {
				t.Fatalf("retry: n=%d err=%v", n, err)
			}
			if got := w.segments(t, "src"); len(got) != 0 {
				t.Fatalf("segments remain after retry: %v", got)
			}
			w.readable(t, "src", "dst")
		})
	}
}

// (CommitNotCommitted, nil) on the reclaimed.json publication is a backend
// contract violation; it must refuse without destructive progress.
func TestReclaimDocNotCommittedWithoutErrorRefuses(t *testing.T) {
	w := newNilErrWorld(t)
	w.sealed(t, "src", "dst")
	headKey, reclKey := w.key("src", "head.json"), w.key("src", "reclaimed.json")
	headBefore := w.raw(t, headKey)
	segsBefore := w.segments(t, "src")

	w.store.arm("put_if_absent", reclKey, stratum.CommitNotCommitted, false)
	_, _, err := w.bs.ReclaimSealedAncestor(w.ctx, "src", "dst")
	w.store.disarm()
	if err == nil {
		t.Fatal("expected refusal on (CommitNotCommitted, nil)")
	}
	if !bytes.Equal(headBefore, w.raw(t, headKey)) || len(w.segments(t, "src")) != len(segsBefore) {
		t.Fatal("destructive progress after (CommitNotCommitted, nil)")
	}
	w.readable(t, "src", "dst")
}

// The head CAS and segment deletes get the same treatment: (CommitUnknown, nil)
// surfaces as indeterminate, and the same successor's retry converges.
func TestReclaimHeadAndDeleteUnknownWithoutErrorConverge(t *testing.T) {
	t.Run("head-cas-landed", func(t *testing.T) {
		w := newNilErrWorld(t)
		w.sealed(t, "src", "dst")
		w.store.arm("put_if_match", w.key("src", "head.json"), stratum.CommitUnknown, true)
		_, _, err := w.bs.ReclaimSealedAncestor(w.ctx, "src", "dst")
		w.store.disarm()
		var ind *stratum.IndeterminateError
		if !errors.As(err, &ind) {
			t.Fatalf("head CAS (CommitUnknown, nil): got %v, want *stratum.IndeterminateError", err)
		}
		if len(w.segments(t, "src")) == 0 {
			t.Fatal("segments deleted before the head commit was known")
		}
		if n, _, err := w.bs.ReclaimSealedAncestor(w.ctx, "src", "dst"); err != nil || n == 0 {
			t.Fatalf("retry: n=%d err=%v", n, err)
		}
		if got := w.segments(t, "src"); len(got) != 0 {
			t.Fatalf("segments remain after retry: %v", got)
		}
		w.readable(t, "src", "dst")
	})
	t.Run("delete-not-landed", func(t *testing.T) {
		w := newNilErrWorld(t)
		w.sealed(t, "src", "dst")
		segs := w.segments(t, "src")
		w.store.arm("delete", segs[0], stratum.CommitUnknown, false)
		_, _, err := w.bs.ReclaimSealedAncestor(w.ctx, "src", "dst")
		w.store.disarm()
		if err == nil {
			t.Fatal("expected an error when a segment delete reports (CommitUnknown, nil)")
		}
		if n, _, err := w.bs.ReclaimSealedAncestor(w.ctx, "src", "dst"); err != nil || n == 0 {
			t.Fatalf("retry: n=%d err=%v", n, err)
		}
		if got := w.segments(t, "src"); len(got) != 0 {
			t.Fatalf("segments remain after retry: %v", got)
		}
		w.readable(t, "src", "dst")
	})
}

// The contract applies store-wide: an Append whose segment publication
// reports (CommitUnknown, nil) without landing must not commit a head that
// references the missing segment.
func TestAppendSegmentUnknownWithoutErrorDoesNotCommitHead(t *testing.T) {
	w := newNilErrWorld(t)
	if err := w.bs.OpenStream(w.ctx, "s", "single_revision"); err != nil {
		t.Fatalf("OpenStream: %v", err)
	}
	w.appendRecord(t, "s", "r-1")
	headKey := w.key("s", "head.json")
	headBefore := w.raw(t, headKey)

	// Intercept every segment PutIfAbsent for this stream.
	interceptSegments := &prefixNilErrStore{nilErrStore: w.store, prefix: w.key("s", "segments/")}
	bs, err := bucketcas.Open(w.ctx, interceptSegments, bucketcas.Options{Prefix: "nilerr"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	_, err = bs.Append(w.ctx, bucketcas.AppendInput{
		Stream:         "s",
		Record:         makeRecordJSON("s", "r-2", 1, `{"v":2}`),
		IdempotencyKey: "k-s-r-2",
	})
	if err == nil {
		t.Fatal("expected Append to fail when the segment publication is (CommitUnknown, nil)")
	}
	if interceptSegments.tripped == 0 {
		t.Fatal("segment publication was not intercepted")
	}
	if !bytes.Equal(headBefore, w.raw(t, headKey)) {
		t.Fatal("head.json committed a segment whose publication was not known to have landed")
	}
	w.readable(t, "s")
}

type prefixNilErrStore struct {
	*nilErrStore
	prefix  string
	tripped int
}

func (p *prefixNilErrStore) PutIfAbsent(ctx context.Context, key string, body []byte) (string, stratum.CommitKnowledge, stratum.ConflictKind, error) {
	if strings.HasPrefix(key, p.prefix) {
		p.tripped++
		return "", stratum.CommitUnknown, stratum.ConflictNone, nil
	}
	return p.nilErrStore.PutIfAbsent(ctx, key, body)
}
