package bucketcas

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/3leaps/recordprims/internal/canon"
	"github.com/3leaps/recordprims/internal/journal"
	"github.com/3leaps/recordprims/internal/shape"
	"github.com/3leaps/recordprims/internal/stratum"
	"github.com/3leaps/recordprims/internal/wire"
)

const (
	// Format is the store metadata format string for the Segmented NDJSON +
	// Conditional-PUT Bucket Stratum.
	Format = "recordprims-cas/v1"

	// StratumID is the runtime.stratum identifier in profile.json.
	StratumID = "ndjson-cas"

	// DefaultBodyRemovalStream is the designated body lifecycle stream name.
	DefaultBodyRemovalStream = "body-removal"

	// MaxHeadBytes is the hard ceiling on serialized streams/<stream>/head.json
	// payloads enforced before PutIfAbsent or PutIfMatch.
	MaxHeadBytes = 16 * 1024

	defaultIdempotencyWindowMs       int64 = 7 * 24 * 60 * 60 * 1000
	defaultCheckpointIntervalEntries int   = 32
	defaultCheckpointIntervalBytes   int64 = 32 * 1024
	defaultMaxCASRetries             int   = 16
)

var (
	// ErrCapabilities is returned at Init or Open when the backing
	// ConditionalObjectStore does not advertise MustNotExist, ValidatorMatch,
	// and CoherentRead. The bucket stratum never degrades to unconditional PUT.
	ErrCapabilities = errors.New("bucketcas: backing store lacks required conditional-put or coherent-read capabilities")

	// ErrStoreExists is returned by Init when STORE.json or profile.json already exists.
	ErrStoreExists = errors.New("bucketcas: store already exists at prefix")

	// ErrInvalidStore is returned when store metadata, head.json, checkpoint, or
	// segment objects fail verification.
	ErrInvalidStore = errors.New("bucketcas: invalid store state")

	// ErrClockRegression is returned when the clock regresses before the stream tail.
	ErrClockRegression = errors.New("bucketcas: clock precedes last journal entry")

	// ErrCASExhausted is returned when bounded 412 contention retries are exhausted.
	ErrCASExhausted = errors.New("bucketcas: conditional-put contention retries exhausted")
)

// Options configures Init and Open on a BucketStore.
type Options struct {
	Prefix                    string
	IdempotencyWindowMs       int64
	Now                       func() time.Time
	CheckpointIntervalEntries int
	CheckpointIntervalBytes   int64
	MaxCASRetries             int
}

func (o *Options) applyDefaults() {
	o.Prefix = strings.Trim(o.Prefix, "/")
	if o.IdempotencyWindowMs <= 0 {
		o.IdempotencyWindowMs = defaultIdempotencyWindowMs
	}
	if o.Now == nil {
		o.Now = func() time.Time { return time.Now().UTC() }
	}
	if o.CheckpointIntervalEntries <= 0 {
		o.CheckpointIntervalEntries = defaultCheckpointIntervalEntries
	}
	if o.CheckpointIntervalBytes <= 0 {
		o.CheckpointIntervalBytes = defaultCheckpointIntervalBytes
	}
	if o.MaxCASRetries <= 0 {
		o.MaxCASRetries = defaultMaxCASRetries
	}
}

func (s *BucketStore) objKey(rel string) string {
	if s.opts.Prefix == "" {
		return rel
	}
	return s.opts.Prefix + "/" + rel
}

// SegmentRef identifies an immutable NDJSON segment object in head.json.
type SegmentRef struct {
	FirstSeq int64  `json:"first_seq"`
	LastSeq  int64  `json:"last_seq"`
	Key      string `json:"key"`
	Digest   string `json:"digest"`
	Length   int64  `json:"length"`
}

// CheckpointRef identifies the latest compacted state checkpoint in head.json.
type CheckpointRef struct {
	CutSeq    int64  `json:"cut_seq"`
	CutDigest string `json:"cut_digest"`
	Key       string `json:"key"`
	Digest    string `json:"digest"`
	Length    int64  `json:"length"`
}

// HeadDoc is the JSON representation of streams/<stream>/head.json, the single
// CAS linearization point per stream.
type HeadDoc struct {
	V                                  int            `json:"v"`
	Stream                             string         `json:"stream"`
	Mode                               string         `json:"mode"`
	CutSeq                             int64          `json:"cut_seq"`
	CutDigest                          string         `json:"cut_digest"`
	SegmentsTail                       []SegmentRef   `json:"segments_tail"`
	LatestCheckpoint                   *CheckpointRef `json:"latest_checkpoint,omitempty"`
	EffectiveRestrictionManifestDigest string         `json:"effective_restriction_manifest_digest"`
}

// CheckpointRecord summarizes a live revision in a compacted checkpoint.
type CheckpointRecord struct {
	Stream   string `json:"stream"`
	RecordID string `json:"record_id"`
	Revision int64  `json:"revision"`
	Digest   string `json:"digest"`
}

// CheckpointDoc is the immutable compacted state snapshot written to
// streams/<stream>/checkpoints/<cut_seq_zero_padded>-<state_sha256>.json.
type CheckpointDoc struct {
	V           int                `json:"v"`
	Stream      string             `json:"stream"`
	Mode        string             `json:"mode"`
	CutSeq      int64              `json:"cut_seq"`
	CutDigest   string             `json:"cut_digest"`
	LastAt      string             `json:"last_at"`
	LiveRecords []CheckpointRecord `json:"live_records"`
	Retired     []string           `json:"retired"`
	JournalRaw  string             `json:"journal_raw"`
}

type storeDoc struct {
	Format            string `json:"format"`
	StoreID           string `json:"store_id"`
	CreatedAt         string `json:"created_at"`
	ProfileDigest     string `json:"profile_digest"`
	BodyRemovalStream string `json:"body_removal_stream"`
}

// BucketStore implements the Segmented NDJSON + Conditional-PUT Bucket Stratum
// (ndjson-cas) over stratum.ConditionalObjectStore.
type BucketStore struct {
	obj               stratum.ConditionalObjectStore
	opts              Options
	storeID           string
	profileDigest     string
	idempotencyWindow int64
	bodyRemovalStream string
}

func checkCaps(obj stratum.ConditionalObjectStore) error {
	if obj == nil {
		return ErrCapabilities
	}
	caps := obj.Capabilities()
	if !caps.ConditionalPutMustNotExist || !caps.ConditionalPutValidatorMatch || !caps.CoherentRead {
		return ErrCapabilities
	}
	return nil
}

func formatInstant(t time.Time) string {
	u := t.UTC().Truncate(time.Microsecond)
	const layout = "2006-01-02T15:04:05"
	return fmt.Sprintf("%s.%06dZ", u.Format(layout), u.Nanosecond()/1000)
}

func sha256Prefixed(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func jcsCanonicalBytes(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	out, err := canon.Canonicalize(raw)
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}

// Init initializes a new ndjson-cas store at opts.Prefix using PutIfAbsent.
// It refuses immediately if the backend lacks required conditional-PUT or
// coherent-read capabilities.
func Init(ctx context.Context, obj stratum.ConditionalObjectStore, opts Options) (*BucketStore, error) {
	if err := checkCaps(obj); err != nil {
		return nil, err
	}
	opts.applyDefaults()

	var idBytes [16]byte
	if _, err := rand.Read(idBytes[:]); err != nil {
		return nil, err
	}
	storeID := hex.EncodeToString(idBytes[:])
	nowStr := formatInstant(opts.Now())

	profMap := map[string]any{
		"capabilities": []string{"contract: durable-record/v1"},
		"origin":       "declared",
		"conformance":  "none",
		"build": map[string]any{
			"name":                  "recordprims",
			"version":               "0.0.1",
			"target":                runtime.GOARCH + "-" + runtime.GOOS,
			"native_dependencies":   []string{},
			"compiled_strata":       []string{"journal-files", StratumID},
			"compiled_capabilities": []string{"revisioned", "body_ref", "tombstoning"},
		},
		"runtime": map[string]any{
			"stratum":                StratumID,
			"failure_model":          "survives_process_loss",
			"commit_strategy":        "backend_acknowledged",
			"commit_note":            "conditional PUT on head.json after segment PutIfAbsent",
			"history_policy":         "append_only",
			"enabled_capabilities":   []string{"revisioned", "body_ref", "tombstoning"},
			"idempotency_window_ms":  opts.IdempotencyWindowMs,
			"clock_skew_bound_ms":    int64(0),
			"body_removal_stream":    DefaultBodyRemovalStream,
			"incarnation_allocation": "prewrite_durable_ledger",
			"deletion": map[string]any{
				"live_head":        "supported",
				"retained_history": "unsupported",
				"managed_replicas": "not_applicable",
				"external_bodies":  "supported",
				"decryption":       "not_applicable",
				"unmanaged_copies": "outside_claim",
			},
		},
	}
	profBytes, err := jcsCanonicalBytes(profMap)
	if err != nil {
		return nil, err
	}
	profNode, err := wire.Ingest(profBytes[:len(profBytes)-1], wire.KindStratumProfile)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidStore, err)
	}
	if err := shape.Validate("v1/stratum-profile.schema.json", profNode); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidStore, err)
	}
	if err := journal.CheckProfile(profNode); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidStore, err)
	}
	profDigest := sha256Prefixed(profBytes[:len(profBytes)-1])

	bs := &BucketStore{
		obj:               obj,
		opts:              opts,
		storeID:           storeID,
		profileDigest:     profDigest,
		idempotencyWindow: opts.IdempotencyWindowMs,
		bodyRemovalStream: DefaultBodyRemovalStream,
	}

	if _, commit, conflict, err := obj.PutIfAbsent(ctx, bs.objKey("profile.json"), profBytes); err != nil || commit != stratum.CommitCommitted {
		if conflict == stratum.ConflictAlreadyExists {
			return nil, ErrStoreExists
		}
		if commit == stratum.CommitUnknown {
			return nil, &stratum.IndeterminateError{Op: "init_put_profile", Key: "profile.json", Err: err}
		}
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("%w: profile.json unexpected commit state %s", ErrInvalidStore, commit)
	}

	// Open the designated body-removal stream before STORE.json is committed.
	if err := bs.OpenStream(ctx, DefaultBodyRemovalStream, "single_revision"); err != nil {
		return nil, err
	}

	sd := storeDoc{
		Format:            Format,
		StoreID:           storeID,
		CreatedAt:         nowStr,
		ProfileDigest:     profDigest,
		BodyRemovalStream: DefaultBodyRemovalStream,
	}
	sdBytes, err := jcsCanonicalBytes(sd)
	if err != nil {
		return nil, err
	}
	if _, commit, conflict, err := obj.PutIfAbsent(ctx, bs.objKey("STORE.json"), sdBytes); err != nil || commit != stratum.CommitCommitted {
		if conflict == stratum.ConflictAlreadyExists {
			return nil, ErrStoreExists
		}
		if commit == stratum.CommitUnknown {
			return nil, &stratum.IndeterminateError{Op: "init_put_store", Key: "STORE.json", Err: err}
		}
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("%w: STORE.json unexpected commit state %s", ErrInvalidStore, commit)
	}
	return bs, nil
}

// Open opens and validates an existing ndjson-cas store at opts.Prefix.
// It refuses immediately if the backend lacks required conditional-PUT or
// coherent-read capabilities.
func Open(ctx context.Context, obj stratum.ConditionalObjectStore, opts Options) (*BucketStore, error) {
	if err := checkCaps(obj); err != nil {
		return nil, err
	}
	opts.applyDefaults()
	bs := &BucketStore{
		obj:  obj,
		opts: opts,
	}

	sdRaw, _, _, err := obj.GetCoherent(ctx, bs.objKey("STORE.json"))
	if err != nil {
		return nil, fmt.Errorf("%w: STORE.json: %v", ErrInvalidStore, err)
	}
	var sd storeDoc
	if err := json.Unmarshal(sdRaw, &sd); err != nil {
		return nil, fmt.Errorf("%w: STORE.json decode: %v", ErrInvalidStore, err)
	}
	if sd.Format != Format {
		return nil, fmt.Errorf("%w: unsupported format %q", ErrInvalidStore, sd.Format)
	}

	profRaw, _, _, err := obj.GetCoherent(ctx, bs.objKey("profile.json"))
	if err != nil {
		return nil, fmt.Errorf("%w: profile.json: %v", ErrInvalidStore, err)
	}
	profTrim := bytes.TrimSuffix(profRaw, []byte{'\n'})
	if gotDig := sha256Prefixed(profTrim); gotDig != sd.ProfileDigest {
		return nil, fmt.Errorf("%w: profile_digest mismatch", ErrInvalidStore)
	}
	profNode, err := wire.Ingest(profTrim, wire.KindStratumProfile)
	if err != nil {
		return nil, fmt.Errorf("%w: profile ingest: %v", ErrInvalidStore, err)
	}
	if err := shape.Validate("v1/stratum-profile.schema.json", profNode); err != nil {
		return nil, fmt.Errorf("%w: profile schema: %v", ErrInvalidStore, err)
	}
	if err := journal.CheckProfile(profNode); err != nil {
		return nil, fmt.Errorf("%w: profile semantics: %v", ErrInvalidStore, err)
	}
	winMs := profNode.Get("runtime").Get("idempotency_window_ms").Int
	remStream := profNode.Get("runtime").Get("body_removal_stream").Str
	if remStream == "" || remStream != sd.BodyRemovalStream {
		return nil, fmt.Errorf("%w: body_removal_stream mismatch", ErrInvalidStore)
	}

	bs.storeID = sd.StoreID
	bs.profileDigest = sd.ProfileDigest
	bs.idempotencyWindow = winMs
	bs.bodyRemovalStream = remStream

	// Verify the designated body-removal stream is present and intact.
	if _, _, exists, err := bs.loadStream(ctx, remStream); err != nil {
		return nil, err
	} else if !exists {
		return nil, fmt.Errorf("%w: designated stream %q missing", ErrInvalidStore, remStream)
	}
	return bs, nil
}

func (s *BucketStore) headKey(stream string) string {
	return s.objKey(fmt.Sprintf("streams/%s/head.json", stream))
}

func (s *BucketStore) segmentKey(stream string, firstSeq, lastSeq int64, segHex string) string {
	return s.objKey(fmt.Sprintf("streams/%s/segments/%016d-%016d-%s.ndjson", stream, firstSeq, lastSeq, segHex))
}

func (s *BucketStore) checkpointKey(stream string, cutSeq int64, stateHex string) string {
	return s.objKey(fmt.Sprintf("streams/%s/checkpoints/%016d-%s.json", stream, cutSeq, stateHex))
}

func (s *BucketStore) bodyKey(digHex string) string {
	return s.objKey(fmt.Sprintf("bodies/sha256/%s", digHex))
}

func (s *BucketStore) ledgerKey(digHex string, length int64) string {
	return s.objKey(fmt.Sprintf("bodies/ledger/%s-%d.json", digHex, length))
}

type loadedStream struct {
	head          HeadDoc
	headValidator string
	st            *journal.State
	rawLines      [][]byte
	lastAt        time.Time
}

func restrictionManifestDigest(st *journal.State) string {
	var items []string
	for _, e := range st.Journal() {
		typ := e.Get("type").Str
		if typ == "tombstoned" || typ == "untombstoned" || typ == "retired" {
			items = append(items, fmt.Sprintf("%d:%s", e.Get("seq").Int, typ))
		}
	}
	raw, _ := json.Marshal(items)
	return sha256Prefixed(raw)
}

func encodeHead(h HeadDoc) ([]byte, error) {
	b, err := jcsCanonicalBytes(h)
	if err != nil {
		return nil, err
	}
	if len(b) > MaxHeadBytes {
		return nil, fmt.Errorf("%w: head.json length %d exceeds %d-byte limit", ErrInvalidStore, len(b), MaxHeadBytes)
	}
	return b, nil
}

// loadStream loads head.json + at most 1 checkpoint + tail segments in O(1)
// requests, verifying all digests and cut invariants.
func (s *BucketStore) loadStream(ctx context.Context, stream string) (*loadedStream, string, bool, error) {
	hKey := s.headKey(stream)
	hBytes, hVal, _, err := s.obj.GetCoherent(ctx, hKey)
	if err != nil {
		if errors.Is(err, stratum.ErrNotFound) {
			return nil, "", false, nil
		}
		return nil, "", false, err
	}
	if len(hBytes) > MaxHeadBytes {
		return nil, "", false, fmt.Errorf("%w: head.json exceeds %d bytes", ErrInvalidStore, MaxHeadBytes)
	}
	var head HeadDoc
	if err := json.Unmarshal(hBytes, &head); err != nil {
		return nil, "", false, fmt.Errorf("%w: head.json decode: %v", ErrInvalidStore, err)
	}
	if head.V != 1 || head.Stream != stream || head.CutSeq < 1 {
		return nil, "", false, fmt.Errorf("%w: invalid head.json header", ErrInvalidStore)
	}

	winTok := strconv.FormatInt(s.idempotencyWindow, 10)
	st, err := journal.New(&wire.Node{Kind: wire.Number, Token: winTok, IsInt: true, Int: s.idempotencyWindow})
	if err != nil {
		return nil, "", false, err
	}
	st.SetRemovalStream(s.bodyRemovalStream)

	var rawLines [][]byte
	var expectedNextSeq int64 = 1
	var lastAt time.Time

	if head.LatestCheckpoint != nil {
		cpRef := head.LatestCheckpoint
		cpBytes, _, cpSize, err := s.obj.GetCoherent(ctx, cpRef.Key)
		if err != nil {
			return nil, "", false, fmt.Errorf("%w: checkpoint %s: %v", ErrInvalidStore, cpRef.Key, err)
		}
		if cpSize != cpRef.Length || sha256Prefixed(cpBytes) != cpRef.Digest {
			return nil, "", false, fmt.Errorf("%w: checkpoint %s digest or length mismatch", ErrInvalidStore, cpRef.Key)
		}
		var cpDoc CheckpointDoc
		if err := json.Unmarshal(cpBytes, &cpDoc); err != nil {
			return nil, "", false, fmt.Errorf("%w: checkpoint decode: %v", ErrInvalidStore, err)
		}
		if cpDoc.V != 1 || cpDoc.Stream != stream || cpDoc.CutSeq != cpRef.CutSeq || cpDoc.CutDigest != cpRef.CutDigest {
			return nil, "", false, fmt.Errorf("%w: checkpoint header mismatch", ErrInvalidStore)
		}
		entries, err := wire.IngestLines([]byte(cpDoc.JournalRaw), wire.KindJournalEntry)
		if err != nil {
			return nil, "", false, fmt.Errorf("%w: checkpoint journal ingest: %v", ErrInvalidStore, err)
		}
		for _, e := range entries {
			if err := shape.Validate("v1/journal-entry.schema.json", e); err != nil {
				return nil, "", false, fmt.Errorf("%w: checkpoint entry schema: %v", ErrInvalidStore, err)
			}
			if err := st.Ingest(e); err != nil {
				return nil, "", false, fmt.Errorf("%w: checkpoint entry ingest: %v", ErrInvalidStore, err)
			}
			rawLines = append(rawLines, append(wire.Encode(e), '\n'))
			lastAt, _ = journal.ParseInstant(e.Get("at").Str)
		}
		if int64(st.Len()) != cpRef.CutSeq {
			return nil, "", false, fmt.Errorf("%w: checkpoint seq count mismatch", ErrInvalidStore)
		}
		gotCutDig, err := st.CutDigest(cpRef.CutSeq)
		if err != nil || gotCutDig != cpRef.CutDigest {
			return nil, "", false, fmt.Errorf("%w: checkpoint cut_digest mismatch", ErrInvalidStore)
		}
		expectedNextSeq = cpRef.CutSeq + 1
	}

	for _, seg := range head.SegmentsTail {
		if seg.FirstSeq != expectedNextSeq || seg.LastSeq < seg.FirstSeq {
			return nil, "", false, fmt.Errorf("%w: segment tail seq discontinuity at %d", ErrInvalidStore, seg.FirstSeq)
		}
		segBytes, _, segSize, err := s.obj.GetCoherent(ctx, seg.Key)
		if err != nil {
			return nil, "", false, fmt.Errorf("%w: segment %s: %v", ErrInvalidStore, seg.Key, err)
		}
		if segSize != seg.Length || sha256Prefixed(segBytes) != seg.Digest {
			return nil, "", false, fmt.Errorf("%w: segment %s digest or length mismatch", ErrInvalidStore, seg.Key)
		}
		entries, err := wire.IngestLines(segBytes, wire.KindJournalEntry)
		if err != nil {
			return nil, "", false, fmt.Errorf("%w: segment ingest: %v", ErrInvalidStore, err)
		}
		for _, e := range entries {
			if err := shape.Validate("v1/journal-entry.schema.json", e); err != nil {
				return nil, "", false, fmt.Errorf("%w: segment entry schema: %v", ErrInvalidStore, err)
			}
			if err := st.Ingest(e); err != nil {
				return nil, "", false, fmt.Errorf("%w: segment entry ingest: %v", ErrInvalidStore, err)
			}
			rawLines = append(rawLines, append(wire.Encode(e), '\n'))
			lastAt, _ = journal.ParseInstant(e.Get("at").Str)
		}
		expectedNextSeq = seg.LastSeq + 1
	}

	if int64(st.Len()) != head.CutSeq {
		return nil, "", false, fmt.Errorf("%w: head cut_seq %d != replayed len %d", ErrInvalidStore, head.CutSeq, st.Len())
	}
	if err := journal.CheckJournal(st.Journal()); err != nil {
		return nil, "", false, fmt.Errorf("%w: journal check: %v", ErrInvalidStore, err)
	}
	gotCutDig, err := st.CutDigest(head.CutSeq)
	if err != nil || gotCutDig != head.CutDigest {
		return nil, "", false, fmt.Errorf("%w: head cut_digest mismatch", ErrInvalidStore)
	}

	return &loadedStream{
		head:          head,
		headValidator: hVal,
		st:            st,
		rawLines:      rawLines,
		lastAt:        lastAt,
	}, hVal, true, nil
}

// stageSegmentAndAdvanceHead writes an immutable segment via PutIfAbsent,
// optionally writes a compacted checkpoint via PutIfAbsent when the tail
// reaches the configured threshold, and advances head.json via PutIfMatch.
func (s *BucketStore) stageSegmentAndAdvanceHead(ctx context.Context, ls *loadedStream, newEntries []*wire.Node) (string, stratum.CommitKnowledge, stratum.ConflictKind, error) {
	firstSeq := ls.head.CutSeq + 1
	lastSeq := ls.head.CutSeq + int64(len(newEntries))

	var segBuf bytes.Buffer
	for _, e := range newEntries {
		can, err := canon.Canonicalize(wire.Encode(e))
		if err != nil {
			return "", stratum.CommitNotCommitted, stratum.ConflictNone, err
		}
		ingested, err := wire.Ingest(can, wire.KindJournalEntry)
		if err != nil {
			return "", stratum.CommitNotCommitted, stratum.ConflictNone, err
		}
		if err := shape.Validate("v1/journal-entry.schema.json", ingested); err != nil {
			return "", stratum.CommitNotCommitted, stratum.ConflictNone, err
		}
		segBuf.Write(can)
		segBuf.WriteByte('\n')
	}
	segBytes := segBuf.Bytes()
	segHex := sha256Hex(segBytes)
	segDig := "sha256:" + segHex
	segKey := s.segmentKey(ls.head.Stream, firstSeq, lastSeq, segHex)

	if _, commit, conflict, err := s.obj.PutIfAbsent(ctx, segKey, segBytes); err != nil {
		if conflict != stratum.ConflictAlreadyExists {
			if commit == stratum.CommitUnknown {
				return "", stratum.CommitUnknown, stratum.ConflictNone, err
			}
			return "", stratum.CommitNotCommitted, conflict, err
		}
	}

	newTail := append(slices.Clone(ls.head.SegmentsTail), SegmentRef{
		FirstSeq: firstSeq,
		LastSeq:  lastSeq,
		Key:      segKey,
		Digest:   segDig,
		Length:   int64(len(segBytes)),
	})

	cutDig, err := ls.st.CutDigest(lastSeq)
	if err != nil {
		return "", stratum.CommitNotCommitted, stratum.ConflictNone, err
	}

	var tailEntries int64
	var tailBytes int64
	for _, sr := range newTail {
		tailEntries += sr.LastSeq - sr.FirstSeq + 1
		tailBytes += sr.Length
	}

	latestCP := ls.head.LatestCheckpoint
	if int(tailEntries) >= s.opts.CheckpointIntervalEntries || tailBytes >= s.opts.CheckpointIntervalBytes {
		cpRef, err := s.writeCheckpoint(ctx, ls.head.Stream, ls.head.Mode, lastSeq, cutDig, ls.st)
		if err != nil {
			return "", stratum.CommitNotCommitted, stratum.ConflictNone, err
		}
		latestCP = cpRef
		newTail = nil
	}

	nextHead := HeadDoc{
		V:                                  1,
		Stream:                             ls.head.Stream,
		Mode:                               ls.head.Mode,
		CutSeq:                             lastSeq,
		CutDigest:                          cutDig,
		SegmentsTail:                       newTail,
		LatestCheckpoint:                   latestCP,
		EffectiveRestrictionManifestDigest: restrictionManifestDigest(ls.st),
	}
	headBytes, err := encodeHead(nextHead)
	if err != nil {
		// If head.json would exceed 16 KiB because of tail segments, force a checkpoint now.
		cpRef, cpErr := s.writeCheckpoint(ctx, ls.head.Stream, ls.head.Mode, lastSeq, cutDig, ls.st)
		if cpErr != nil {
			return "", stratum.CommitNotCommitted, stratum.ConflictNone, err
		}
		nextHead.LatestCheckpoint = cpRef
		nextHead.SegmentsTail = nil
		headBytes, err = encodeHead(nextHead)
		if err != nil {
			return "", stratum.CommitNotCommitted, stratum.ConflictNone, err
		}
	}

	return s.obj.PutIfMatch(ctx, s.headKey(ls.head.Stream), ls.headValidator, headBytes)
}

func (s *BucketStore) writeCheckpoint(ctx context.Context, stream, mode string, cutSeq int64, cutDigest string, st *journal.State) (*CheckpointRef, error) {
	var rawBuf bytes.Buffer
	var lastAt string
	for _, e := range st.Journal() {
		can, err := canon.Canonicalize(wire.Encode(e))
		if err != nil {
			return nil, err
		}
		rawBuf.Write(can)
		rawBuf.WriteByte('\n')
		lastAt = e.Get("at").Str
	}

	recs := st.Records()
	liveRecs := make([]CheckpointRecord, 0, len(recs))
	var retiredSet []string
	seenRID := map[string]bool{}
	for pk := range recs {
		liveRecs = append(liveRecs, CheckpointRecord{
			Stream:   pk.Stream,
			RecordID: pk.RecordID,
			Revision: pk.Revision,
			Digest:   pk.Digest,
		})
		if !seenRID[pk.RecordID] {
			seenRID[pk.RecordID] = true
			if st.IsRetired(pk.RecordID) {
				retiredSet = append(retiredSet, pk.RecordID)
			}
		}
	}
	slices.SortFunc(liveRecs, func(a, b CheckpointRecord) int {
		if a.RecordID != b.RecordID {
			return strings.Compare(a.RecordID, b.RecordID)
		}
		if a.Revision < b.Revision {
			return -1
		}
		if a.Revision > b.Revision {
			return 1
		}
		return 0
	})
	slices.Sort(retiredSet)

	cpDoc := CheckpointDoc{
		V:           1,
		Stream:      stream,
		Mode:        mode,
		CutSeq:      cutSeq,
		CutDigest:   cutDigest,
		LastAt:      lastAt,
		LiveRecords: liveRecs,
		Retired:     retiredSet,
		JournalRaw:  rawBuf.String(),
	}
	cpBytes, err := jcsCanonicalBytes(cpDoc)
	if err != nil {
		return nil, err
	}
	cpHex := sha256Hex(cpBytes)
	cpDig := "sha256:" + cpHex
	cpKey := s.checkpointKey(stream, cutSeq, cpHex)

	if _, commit, conflict, err := s.obj.PutIfAbsent(ctx, cpKey, cpBytes); err != nil {
		if conflict != stratum.ConflictAlreadyExists {
			if commit == stratum.CommitUnknown {
				return nil, &stratum.IndeterminateError{Op: "put_checkpoint", Key: cpKey, Err: err}
			}
			return nil, err
		}
	}
	return &CheckpointRef{
		CutSeq:    cutSeq,
		CutDigest: cutDigest,
		Key:       cpKey,
		Digest:    cpDig,
		Length:    int64(len(cpBytes)),
	}, nil
}

// OpenStream opens or creates stream with mode ("single_revision" or
// "revisioned"), writing the initial segment and head.json via PutIfAbsent.
func (s *BucketStore) OpenStream(ctx context.Context, stream, mode string) error {
	if mode != "single_revision" && mode != "revisioned" {
		return fmt.Errorf("%w: invalid mode %q", ErrInvalidStore, mode)
	}
	ls, _, exists, err := s.loadStream(ctx, stream)
	if err != nil {
		return err
	}
	if exists {
		if ls.head.Mode != mode {
			return fmt.Errorf("%w: stream %q already open with mode %q", ErrInvalidStore, stream, ls.head.Mode)
		}
		return nil
	}

	atStr := formatInstant(s.opts.Now())
	openEntry := &wire.Node{
		Kind: wire.Object,
		Fields: []wire.Member{
			{Name: "v", Value: &wire.Node{Kind: wire.Number, Token: "1", IsInt: true, Int: 1}},
			{Name: "seq", Value: &wire.Node{Kind: wire.Number, Token: "1", IsInt: true, Int: 1}},
			{Name: "group", Value: &wire.Node{Kind: wire.Number, Token: "1", IsInt: true, Int: 1}},
			{Name: "group_size", Value: &wire.Node{Kind: wire.Number, Token: "1", IsInt: true, Int: 1}},
			{Name: "stream", Value: &wire.Node{Kind: wire.String, Str: stream}},
			{Name: "type", Value: &wire.Node{Kind: wire.String, Str: "stream_opened"}},
			{Name: "at", Value: &wire.Node{Kind: wire.String, Str: atStr}},
			{Name: "mode", Value: &wire.Node{Kind: wire.String, Str: mode}},
			{Name: "failure_model", Value: &wire.Node{Kind: wire.String, Str: "survives_process_loss"}},
		},
	}
	if err := shape.Validate("v1/journal-entry.schema.json", openEntry); err != nil {
		return err
	}
	winTok := strconv.FormatInt(s.idempotencyWindow, 10)
	st, err := journal.New(&wire.Node{Kind: wire.Number, Token: winTok, IsInt: true, Int: s.idempotencyWindow})
	if err != nil {
		return err
	}
	st.SetRemovalStream(s.bodyRemovalStream)
	if err := st.Ingest(openEntry); err != nil {
		return err
	}
	cutDig, err := st.CutDigest(1)
	if err != nil {
		return err
	}

	canEntry, err := canon.Canonicalize(wire.Encode(openEntry))
	if err != nil {
		return err
	}
	segBytes := append(canEntry, '\n')
	segHex := sha256Hex(segBytes)
	segDig := "sha256:" + segHex
	segKey := s.segmentKey(stream, 1, 1, segHex)

	if _, commit, conflict, err := s.obj.PutIfAbsent(ctx, segKey, segBytes); err != nil {
		if conflict != stratum.ConflictAlreadyExists {
			if commit == stratum.CommitUnknown {
				return &stratum.IndeterminateError{Op: "put_segment", Key: segKey, Err: err}
			}
			return err
		}
	}

	head := HeadDoc{
		V:         1,
		Stream:    stream,
		Mode:      mode,
		CutSeq:    1,
		CutDigest: cutDig,
		SegmentsTail: []SegmentRef{
			{
				FirstSeq: 1,
				LastSeq:  1,
				Key:      segKey,
				Digest:   segDig,
				Length:   int64(len(segBytes)),
			},
		},
		EffectiveRestrictionManifestDigest: restrictionManifestDigest(st),
	}
	headBytes, err := encodeHead(head)
	if err != nil {
		return err
	}
	_, commit, conflict, err := s.obj.PutIfAbsent(ctx, s.headKey(stream), headBytes)
	if err != nil {
		if conflict == stratum.ConflictAlreadyExists {
			// Another worker created head.json concurrently; verify mode matches.
			ls2, _, ok2, err2 := s.loadStream(ctx, stream)
			if err2 != nil {
				return err2
			}
			if ok2 && ls2.head.Mode == mode {
				return nil
			}
			return fmt.Errorf("%w: concurrent stream creation mode conflict", ErrInvalidStore)
		}
		if commit == stratum.CommitUnknown {
			ls2, _, ok2, err2 := s.loadStream(ctx, stream)
			if err2 == nil && ok2 && ls2.head.Mode == mode {
				return nil
			}
			return &stratum.IndeterminateError{Op: "put_head", Key: stream, Err: err}
		}
		return err
	}
	return nil
}

// AppendInput carries a content write for Append.
type AppendInput struct {
	Stream         string
	Record         []byte
	IdempotencyKey string
}

// AppendReceipt is the result of a BucketStore.Append call.
type AppendReceipt struct {
	Outcome        string
	Stream         string
	IdempotencyKey string
	Seq            int64
	Digest         string
	Raw            []byte
}

func validWireString(s string, minRunes, maxRunes int) bool {
	if !utf8.ValidString(s) {
		return false
	}
	n := utf8.RuneCountInString(s)
	return n >= minRunes && n <= maxRunes
}

// Append executes a content write against stream with bounded 412 CAS retry
// and immediate GetCoherent("head.json") reconciliation on CommitUnknown.
func (s *BucketStore) Append(ctx context.Context, in AppendInput) (AppendReceipt, error) {
	if !validWireString(in.IdempotencyKey, 1, 256) {
		return AppendReceipt{}, fmt.Errorf("%w: invalid idempotency_key", ErrInvalidStore)
	}
	if in.Stream == s.bodyRemovalStream {
		return AppendReceipt{}, fmt.Errorf("%w: %q is the designated body-removal stream", ErrInvalidStore, in.Stream)
	}
	recNode, err := wire.Ingest(in.Record, wire.KindRecord)
	if err != nil {
		return AppendReceipt{}, err
	}
	if err := shape.Validate("v1/record.schema.json", recNode); err != nil {
		return AppendReceipt{}, err
	}
	recStream := recNode.Get("stream").Str
	if recStream != in.Stream {
		return AppendReceipt{}, fmt.Errorf("%w: record.stream %q != %q", ErrInvalidStore, recStream, in.Stream)
	}
	recID := recNode.Get("record_id").Str
	rev := recNode.Get("revision").Int
	supNode := recNode.Get("supersedes")

	recCanon, err := canon.Canonicalize(wire.Encode(recNode))
	if err != nil {
		return AppendReceipt{}, err
	}
	recDig := sha256Prefixed(recCanon)

	for attempt := 0; attempt < s.opts.MaxCASRetries; attempt++ {
		ls, _, exists, err := s.loadStream(ctx, in.Stream)
		if err != nil {
			return AppendReceipt{}, err
		}
		if !exists {
			return AppendReceipt{}, fmt.Errorf("%w: stream %q not found", ErrInvalidStore, in.Stream)
		}

		now := s.opts.Now().UTC().Truncate(time.Microsecond)
		if now.Before(ls.lastAt) {
			return AppendReceipt{}, ErrClockRegression
		}
		atStr := formatInstant(now)

		// 1. Check live idempotency window first.
		if kind, val, _, rcpt, ok := ls.st.LiveKey(in.IdempotencyKey, now); ok {
			if kind == "content" && val == recDig {
				return AppendReceipt{
					Outcome:        rcpt.Get("outcome").Str,
					Stream:         in.Stream,
					IdempotencyKey: in.IdempotencyKey,
					Seq:            rcpt.Get("seq").Int,
					Digest:         recDig,
					Raw:            wire.Encode(rcpt),
				}, nil
			}
			return AppendReceipt{
				Outcome:        journal.ReasonIdempotencyConflict,
				Stream:         in.Stream,
				IdempotencyKey: in.IdempotencyKey,
			}, nil
		}

		// 2. Verify external body availability on the designated stream before claiming body_status: verified.
		if bNode := recNode.Get("body"); bNode != nil {
			bDig := bNode.Get("digest").Str
			bLen := bNode.Get("length").Int
			bst, berr := s.BodyState(ctx, bDig, bLen)
			if berr != nil {
				return AppendReceipt{}, berr
			}
			if bst != "available" {
				return AppendReceipt{}, fmt.Errorf("%w: referenced body %s state is %s, not available", ErrInvalidStore, bDig, bst)
			}
		}

		// 2. Check retirement.
		if ls.st.IsRetired(recID) {
			return AppendReceipt{
				Outcome:        journal.ReasonRetiredRecord,
				Stream:         in.Stream,
				IdempotencyKey: in.IdempotencyKey,
			}, nil
		}

		// 3. Check revision / supersedes preconditions.
		headPin, hasHead := ls.st.HeadPin(recID)
		if ls.head.Mode == "single_revision" && (rev > 1 || supNode != nil) {
			return AppendReceipt{
				Outcome:        "revision_conflict",
				Stream:         in.Stream,
				IdempotencyKey: in.IdempotencyKey,
			}, nil
		}
		entryType := "appended"
		if !hasHead {
			if rev != 1 || supNode != nil {
				return AppendReceipt{
					Outcome:        "revision_conflict",
					Stream:         in.Stream,
					IdempotencyKey: in.IdempotencyKey,
				}, nil
			}
		} else {
			if ls.head.Mode == "single_revision" {
				return AppendReceipt{
					Outcome:        "revision_conflict",
					Stream:         in.Stream,
					IdempotencyKey: in.IdempotencyKey,
				}, nil
			}
			if rev != headPin.Revision+1 || supNode == nil {
				return AppendReceipt{
					Outcome:        "revision_conflict",
					Stream:         in.Stream,
					IdempotencyKey: in.IdempotencyKey,
				}, nil
			}
			supRev := supNode.Get("revision").Int
			supDig := supNode.Get("digest").Str
			if supRev != headPin.Revision || supDig != headPin.Digest {
				return AppendReceipt{
					Outcome:        "revision_conflict",
					Stream:         in.Stream,
					IdempotencyKey: in.IdempotencyKey,
				}, nil
			}
			entryType = "revised"
		}

		// 4. Construct and ingest new entry into in-memory State.
		nextSeq := ls.head.CutSeq + 1
		seqTok := strconv.FormatInt(nextSeq, 10)
		fields := []wire.Member{
			{Name: "v", Value: &wire.Node{Kind: wire.Number, Token: "1", IsInt: true, Int: 1}},
			{Name: "seq", Value: &wire.Node{Kind: wire.Number, Token: seqTok, IsInt: true, Int: nextSeq}},
			{Name: "group", Value: &wire.Node{Kind: wire.Number, Token: seqTok, IsInt: true, Int: nextSeq}},
			{Name: "group_size", Value: &wire.Node{Kind: wire.Number, Token: "1", IsInt: true, Int: 1}},
			{Name: "stream", Value: &wire.Node{Kind: wire.String, Str: in.Stream}},
			{Name: "type", Value: &wire.Node{Kind: wire.String, Str: entryType}},
			{Name: "at", Value: &wire.Node{Kind: wire.String, Str: atStr}},
			{Name: "idempotency_key", Value: &wire.Node{Kind: wire.String, Str: in.IdempotencyKey}},
			{Name: "digest", Value: &wire.Node{Kind: wire.String, Str: recDig}},
			{Name: "record", Value: recNode},
		}
		if recNode.Get("body") != nil {
			fields = append(fields, wire.Member{
				Name:  "body_status",
				Value: &wire.Node{Kind: wire.String, Str: "verified"},
			})
		}
		entry := &wire.Node{Kind: wire.Object, Fields: fields}
		if err := shape.Validate("v1/journal-entry.schema.json", entry); err != nil {
			return AppendReceipt{}, err
		}
		if err := ls.st.Ingest(entry); err != nil {
			return AppendReceipt{}, err
		}

		// 5. Stage immutable segment (and optional checkpoint) and CAS head.json.
		_, commit, conflict, err := s.stageSegmentAndAdvanceHead(ctx, ls, []*wire.Node{entry})
		if err == nil && commit == stratum.CommitCommitted {
			_, _, _, rcpt, _ := ls.st.LiveKey(in.IdempotencyKey, now)
			return AppendReceipt{
				Outcome:        entryType,
				Stream:         in.Stream,
				IdempotencyKey: in.IdempotencyKey,
				Seq:            nextSeq,
				Digest:         recDig,
				Raw:            wire.Encode(rcpt),
			}, nil
		}

		if conflict == stratum.ConflictTokenMismatch {
			// 412 Precondition Failed: another writer advanced head.json.
			// Loop to re-read head.json via GetCoherent, ingest delta segments,
			// re-validate preconditions, and stage at new_cut_seq + 1.
			continue
		}

		if commit == stratum.CommitUnknown {
			// Transport drop/timeout after dispatch: immediately probe head.json via GetCoherent.
			lsAfter, _, okAfter, errAfter := s.loadStream(ctx, in.Stream)
			if errAfter == nil && okAfter {
				probeNow := s.opts.Now().UTC().Truncate(time.Microsecond)
				if probeNow.Before(lsAfter.lastAt) {
					probeNow = lsAfter.lastAt
				}
				if kind, val, _, rcpt, ok := lsAfter.st.LiveKey(in.IdempotencyKey, probeNow); ok && kind == "content" && val == recDig {
					return AppendReceipt{
						Outcome:        rcpt.Get("outcome").Str,
						Stream:         in.Stream,
						IdempotencyKey: in.IdempotencyKey,
						Seq:            rcpt.Get("seq").Int,
						Digest:         recDig,
						Raw:            wire.Encode(rcpt),
					}, nil
				}
			}
			return AppendReceipt{}, &stratum.IndeterminateError{
				Op:  "append_head_cas",
				Key: in.IdempotencyKey,
				Err: err,
			}
		}

		return AppendReceipt{}, err
	}
	return AppendReceipt{}, ErrCASExhausted
}

// Reconcile queries the stream's idempotency window at the current coherent head.
func (s *BucketStore) Reconcile(ctx context.Context, stream, key, attemptedAt string) (*wire.Node, error) {
	if !validWireString(key, 1, 256) {
		return nil, fmt.Errorf("%w: invalid idempotency_key", ErrInvalidStore)
	}
	ls, _, exists, err := s.loadStream(ctx, stream)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, fmt.Errorf("%w: stream %q not found", ErrInvalidStore, stream)
	}
	now := s.opts.Now().UTC().Truncate(time.Microsecond)
	if now.Before(ls.lastAt) {
		return nil, ErrClockRegression
	}
	return ls.st.Reconcile(key, formatInstant(now), attemptedAt)
}

// State loads and returns the verified journal.State and HeadDoc at the
// stream's current coherent head.
func (s *BucketStore) State(ctx context.Context, stream string) (*journal.State, HeadDoc, error) {
	ls, _, exists, err := s.loadStream(ctx, stream)
	if err != nil {
		return nil, HeadDoc{}, err
	}
	if !exists {
		return nil, HeadDoc{}, fmt.Errorf("%w: stream %q not found", ErrInvalidStore, stream)
	}
	return ls.st, ls.head, nil
}
