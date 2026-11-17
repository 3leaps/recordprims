package bucketcas

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/3leaps/recordprims/internal/canon"
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

func (s *BucketStore) sealKey(stream string) string {
	return s.objKey(fmt.Sprintf("streams/%s/stream-seal.json", stream))
}

func (s *BucketStore) reclaimedKey(stream string) string {
	return s.objKey(fmt.Sprintf("streams/%s/reclaimed.json", stream))
}

type bucketReclaimedDoc struct {
	V                                  int                     `json:"v"`
	PredecessorStream                  string                  `json:"predecessor_stream"`
	CutSeq                             int64                   `json:"cut_seq"`
	CutDigest                          string                  `json:"cut_digest"`
	SealDigest                         string                  `json:"seal_digest"`
	EffectiveRestrictionManifestDigest string                  `json:"effective_restriction_manifest_digest"`
	Manifest                           json.RawMessage         `json:"manifest"`
	ContinuesStream                    string                  `json:"continues_stream,omitempty"`
	KnownPins                          []journal.Pin           `json:"known_pins"`
	KnownPinSeqs                       []int64                 `json:"known_pin_seqs,omitempty"`
	HistoricalCuts                     []journal.SealedCutInfo `json:"historical_cuts,omitempty"`
	ReclaimedKeys                      []string                `json:"reclaimed_keys,omitempty"`
	ReclaimedBytes                     int64                   `json:"reclaimed_bytes,omitempty"`
}

type bucketAncestorSealRef struct {
	Store      string `json:"store"`
	Capability string `json:"capability"`
	Stream     string `json:"stream"`
	CutSeq     int64  `json:"cut_seq"`
	SealDigest string `json:"seal_digest"`
}

func copyBucketSeen(seen map[string]bool) map[string]bool {
	cp := make(map[string]bool, len(seen)+1)
	for k, v := range seen {
		cp[k] = v
	}
	return cp
}

func (s *BucketStore) loadAndVerifyBucketSealDoc(ctx context.Context, stream string) (*wire.Node, string, []bucketAncestorSealRef, error) {
	sealRaw, _, _, err := s.obj.GetCoherent(ctx, s.sealKey(stream))
	if err != nil {
		return nil, "", nil, fmt.Errorf("%w: missing stream-seal.json for %q: %v", ErrInvalidStore, stream, err)
	}
	sealTrim := bytes.TrimSuffix(sealRaw, []byte{'\n'})
	sealNode, err := wire.Ingest(sealTrim, wire.KindStreamSeal)
	if err != nil || shape.Validate("v1/stream-seal.schema.json", sealNode) != nil {
		return nil, "", nil, fmt.Errorf("%w: invalid stream-seal.json for %q", ErrInvalidStore, stream)
	}
	if sealNode.Get("store").Str != s.storeID ||
		sealNode.Get("capability").Str != "contract: durable-record/v1" ||
		sealNode.Get("stream").Str != stream {
		return nil, "", nil, fmt.Errorf("%w: stream-seal.json header mismatch for %q", ErrInvalidStore, stream)
	}
	tp := sealNode.Get("trust_profile")
	if tp == nil || tp.Get("id").Str != "recordprims-cas" || tp.Get("version").Str != "v1" {
		return nil, "", nil, fmt.Errorf("%w: stream-seal.json trust_profile mismatch for %q", ErrInvalidStore, stream)
	}
	if _, ok := journal.ParseInstant(sealNode.Get("sealed_at").Str); !ok {
		return nil, "", nil, fmt.Errorf("%w: stream-seal.json invalid sealed_at for %q", ErrInvalidStore, stream)
	}
	canBytes, err := canon.Canonicalize(sealTrim)
	if err != nil || !bytes.Equal(canBytes, sealTrim) {
		return nil, "", nil, fmt.Errorf("%w: non-canonical stream-seal.json for %q", ErrInvalidStore, stream)
	}
	ancSeq := sealNode.Get("ancestor_seals")
	refs := make([]bucketAncestorSealRef, 0, len(ancSeq.Elems))
	var jcsOrder []string
	seenAnc := map[string]bool{}
	for _, el := range ancSeq.Elems {
		r := bucketAncestorSealRef{
			Store:      el.Get("store").Str,
			Capability: el.Get("capability").Str,
			Stream:     el.Get("stream").Str,
			CutSeq:     el.Get("cut_seq").Int,
			SealDigest: el.Get("seal_digest").Str,
		}
		if r.Store != s.storeID || r.Capability != "contract: durable-record/v1" || seenAnc[r.Stream] {
			return nil, "", nil, fmt.Errorf("%w: invalid ancestor_seal entry in %q", ErrInvalidStore, stream)
		}
		seenAnc[r.Stream] = true
		jb, jerr := canon.Canonicalize(wire.Encode(el))
		if jerr != nil {
			return nil, "", nil, fmt.Errorf("%w: ancestor_seal JCS failed in %q", ErrInvalidStore, stream)
		}
		jcsOrder = append(jcsOrder, string(jb))
		refs = append(refs, r)
	}
	if !slices.IsSorted(jcsOrder) {
		return nil, "", nil, fmt.Errorf("%w: ancestor_seals not JCS-sorted in %q", ErrInvalidStore, stream)
	}
	return sealNode, sha256Prefixed(sealTrim), refs, nil
}

func (s *BucketStore) verifyBucketAncestorRefs(ctx context.Context, directContStream string, ancRefs []bucketAncestorSealRef, seen map[string]bool) ([]*wire.Node, error) {
	if directContStream == "" {
		if len(ancRefs) != 0 {
			return nil, fmt.Errorf("%w: unexpected ancestor_seals on root stream", ErrInvalidStore)
		}
		return []*wire.Node{}, nil
	}
	byStream := make(map[string]bucketAncestorSealRef, len(ancRefs))
	for _, ar := range ancRefs {
		byStream[ar.Stream] = ar
	}
	if _, ok := byStream[directContStream]; !ok {
		return nil, fmt.Errorf("%w: direct ancestor %q missing from ancestor_seals", ErrInvalidStore, directContStream)
	}
	upstreamOf := make(map[string]string, len(ancRefs))
	ancCtxNodes := make([]*wire.Node, 0, len(ancRefs))
	for _, ar := range ancRefs {
		if seen[ar.Stream] || len(seen) > 16 {
			return nil, fmt.Errorf("%w: cycle or depth exceeded in ancestor_seals", ErrInvalidStore)
		}
		ancSeen := copyBucketSeen(seen)
		ancSeen[ar.Stream] = true

		hBytes, _, _, err := s.obj.GetCoherent(ctx, s.headKey(ar.Stream))
		if err != nil {
			return nil, fmt.Errorf("%w: ancestor %q head missing: %v", ErrInvalidStore, ar.Stream, err)
		}
		var ancHead HeadDoc
		if err := json.Unmarshal(hBytes, &ancHead); err != nil {
			return nil, fmt.Errorf("%w: ancestor %q head decode: %v", ErrInvalidStore, ar.Stream, err)
		}
		if !ancHead.Sealed || ancHead.CutSeq != ar.CutSeq || ancHead.SealDigest != ar.SealDigest {
			return nil, fmt.Errorf("%w: ancestor %q head does not match ancestor_seal reference", ErrInvalidStore, ar.Stream)
		}
		ancSealNode, ancSealDig, _, err := s.loadAndVerifyBucketSealDoc(ctx, ar.Stream)
		if err != nil || ancSealDig != ar.SealDigest {
			return nil, fmt.Errorf("%w: ancestor %q seal invalid", ErrInvalidStore, ar.Stream)
		}
		var ancManifest *wire.Node
		if ancHead.Reclaimed {
			rdoc, _, err := s.loadAndVerifyBucketReclaimed(ctx, ar.Stream, ancHead, ancSeen)
			if err != nil {
				return nil, err
			}
			mNode, merr := wire.ParseJSON(rdoc.Manifest)
			if merr != nil {
				return nil, merr
			}
			ancManifest = mNode
			if rdoc.ContinuesStream != "" {
				if _, ok := byStream[rdoc.ContinuesStream]; !ok {
					return nil, fmt.Errorf("%w: transitive ancestor %q omitted from ancestor_seals", ErrInvalidStore, rdoc.ContinuesStream)
				}
				upstreamOf[ar.Stream] = rdoc.ContinuesStream
			}
		} else {
			ancLS, _, exists, err := s.loadStreamWithSeen(ctx, ar.Stream, ancSeen)
			if err != nil || !exists {
				return nil, fmt.Errorf("%w: load sealed ancestor %q: %v", ErrInvalidStore, ar.Stream, err)
			}
			if len(ancLS.st.ActiveHolds()) > 0 || ancLS.st.HasAncestorHold() {
				return nil, fmt.Errorf("%w: sealed ancestor %q has active hold", ErrInvalidStore, ar.Stream)
			}
			mNode, _, merr := ancLS.st.EffectiveRestrictionManifest()
			if merr != nil {
				return nil, merr
			}
			ancManifest = mNode
			if cn := ancLS.st.ContinuesNode(); cn != nil {
				upStream := cn.Get("stream").Str
				if _, ok := byStream[upStream]; !ok {
					return nil, fmt.Errorf("%w: transitive ancestor %q omitted from ancestor_seals", ErrInvalidStore, upStream)
				}
				upstreamOf[ar.Stream] = upStream
			}
		}
		ancCtxNodes = append(ancCtxNodes, &wire.Node{
			Kind: wire.Object,
			Fields: []wire.Member{
				{Name: "seal", Value: ancSealNode},
				{Name: "closer_authenticated", Value: &wire.Node{Kind: wire.Bool, Bool: true}},
				{Name: "source_write_fenced", Value: &wire.Node{Kind: wire.Bool, Bool: true}},
				{Name: "expected_store", Value: &wire.Node{Kind: wire.String, Str: s.storeID}},
				{Name: "accepted_trust_profile", Value: &wire.Node{
					Kind: wire.Object,
					Fields: []wire.Member{
						{Name: "id", Value: &wire.Node{Kind: wire.String, Str: "recordprims-cas"}},
						{Name: "version", Value: &wire.Node{Kind: wire.String, Str: "v1"}},
					},
				}},
				{Name: "manifest", Value: ancManifest},
			},
		})
	}
	reachable := make(map[string]bool, len(ancRefs))
	cur := directContStream
	for cur != "" {
		if reachable[cur] {
			return nil, fmt.Errorf("%w: cycle in ancestor_seals", ErrInvalidStore)
		}
		reachable[cur] = true
		cur = upstreamOf[cur]
	}
	if len(reachable) != len(ancRefs) {
		return nil, fmt.Errorf("%w: ancestor_seals contains unreachable stream", ErrInvalidStore)
	}
	return ancCtxNodes, nil
}

func (s *BucketStore) verifyBucketSealAgainstLoaded(ctx context.Context, stream string, head HeadDoc, st *journal.State, seen map[string]bool) (string, error) {
	sealNode, sealDig, ancRefs, err := s.loadAndVerifyBucketSealDoc(ctx, stream)
	if err != nil {
		return "", err
	}
	if head.SealDigest != "" && sealDig != head.SealDigest {
		return "", fmt.Errorf("%w: head seal_digest mismatch for %q", ErrInvalidStore, stream)
	}
	if sealNode.Get("cut_seq").Int != head.CutSeq || sealNode.Get("cut_digest").Str != head.CutDigest {
		return "", fmt.Errorf("%w: stream-seal.json cut mismatch for %q", ErrInvalidStore, stream)
	}
	mDoc, effDig, err := st.EffectiveRestrictionManifest()
	if err != nil || sealNode.Get("effective_restriction_manifest_digest").Str != effDig {
		return "", fmt.Errorf("%w: stream-seal.json manifest digest mismatch for %q", ErrInvalidStore, stream)
	}
	var directCont string
	if cn := st.ContinuesNode(); cn != nil {
		directCont = cn.Get("stream").Str
	}
	ancCtxNodes, err := s.verifyBucketAncestorRefs(ctx, directCont, ancRefs, seen)
	if err != nil {
		return "", err
	}
	stepNode := &wire.Node{
		Kind: wire.Object,
		Fields: []wire.Member{
			{Name: "seal", Value: sealNode},
			{Name: "closer_authenticated", Value: &wire.Node{Kind: wire.Bool, Bool: true}},
			{Name: "source_write_fenced", Value: &wire.Node{Kind: wire.Bool, Bool: true}},
			{Name: "expected_store", Value: &wire.Node{Kind: wire.String, Str: s.storeID}},
			{Name: "accepted_trust_profile", Value: &wire.Node{
				Kind: wire.Object,
				Fields: []wire.Member{
					{Name: "id", Value: &wire.Node{Kind: wire.String, Str: "recordprims-cas"}},
					{Name: "version", Value: &wire.Node{Kind: wire.String, Str: "v1"}},
				},
			}},
			{Name: "manifest", Value: mDoc},
			{Name: "journal", Value: &wire.Node{Kind: wire.Array, Elems: st.Journal()}},
			{Name: "ancestor_seal_contexts", Value: &wire.Node{Kind: wire.Array, Elems: ancCtxNodes}},
		},
	}
	outNode, err := st.VerifySealStep(stepNode)
	if err != nil || outNode.Get("outcome").Str != "seal_verified" || outNode.Get("final") == nil || !outNode.Get("final").Bool {
		return "", fmt.Errorf("%w: stream-seal.json verification failed for %q", ErrInvalidStore, stream)
	}
	return sealDig, nil
}

func (s *BucketStore) loadAndVerifyBucketReclaimed(ctx context.Context, stream string, srcHead HeadDoc, seen map[string]bool) (bucketReclaimedDoc, *journal.SealedSourceSummary, error) {
	if len(seen) > 16 {
		return bucketReclaimedDoc{}, nil, fmt.Errorf("%w: continuation depth exceeded", ErrInvalidStore)
	}
	sealNode, sealDig, ancRefs, err := s.loadAndVerifyBucketSealDoc(ctx, stream)
	if err != nil {
		return bucketReclaimedDoc{}, nil, err
	}
	if sealDig != srcHead.SealDigest ||
		sealNode.Get("cut_seq").Int != srcHead.CutSeq ||
		sealNode.Get("cut_digest").Str != srcHead.CutDigest {
		return bucketReclaimedDoc{}, nil, fmt.Errorf("%w: reclaimed source seal mismatch for %q", ErrInvalidStore, stream)
	}
	recRaw, _, _, err := s.obj.GetCoherent(ctx, s.reclaimedKey(stream))
	if err != nil {
		return bucketReclaimedDoc{}, nil, fmt.Errorf("%w: reclaimed source missing reclaimed.json: %v", ErrInvalidStore, err)
	}
	recTrim := bytes.TrimSuffix(recRaw, []byte{'\n'})
	if srcHead.ReclaimedDigest == "" || sha256Prefixed(recTrim) != srcHead.ReclaimedDigest {
		return bucketReclaimedDoc{}, nil, fmt.Errorf("%w: reclaimed.json digest mismatch for %q", ErrInvalidStore, stream)
	}
	var rdoc bucketReclaimedDoc
	if err := json.Unmarshal(recRaw, &rdoc); err != nil {
		return bucketReclaimedDoc{}, nil, fmt.Errorf("%w: reclaimed.json decode: %v", ErrInvalidStore, err)
	}
	if rdoc.V != 1 ||
		rdoc.PredecessorStream != stream ||
		rdoc.CutSeq != srcHead.CutSeq ||
		rdoc.CutDigest != srcHead.CutDigest ||
		rdoc.SealDigest != srcHead.SealDigest ||
		rdoc.EffectiveRestrictionManifestDigest != sealNode.Get("effective_restriction_manifest_digest").Str {
		return bucketReclaimedDoc{}, nil, fmt.Errorf("%w: reclaimed.json header mismatch", ErrInvalidStore)
	}
	segPrefix := s.objKey(fmt.Sprintf("streams/%s/segments/", stream))
	ckptPrefix := s.objKey(fmt.Sprintf("streams/%s/checkpoints/", stream))
	for _, k := range rdoc.ReclaimedKeys {
		if strings.Contains(k, "..") || (!strings.HasPrefix(k, segPrefix) && !strings.HasPrefix(k, ckptPrefix)) {
			return bucketReclaimedDoc{}, nil, fmt.Errorf("%w: reclaimed.json contains out-of-scope key %q", ErrInvalidStore, k)
		}
	}
	mNode, err := wire.ParseJSON(rdoc.Manifest)
	if err != nil {
		return bucketReclaimedDoc{}, nil, fmt.Errorf("%w: reclaimed manifest ingest: %v", ErrInvalidStore, err)
	}
	hasHolds, err := journal.VerifyManifestPreimage(mNode, rdoc.EffectiveRestrictionManifestDigest)
	if err != nil || hasHolds {
		return bucketReclaimedDoc{}, nil, fmt.Errorf("%w: reclaimed manifest verification failed", ErrInvalidStore)
	}
	if _, err := s.verifyBucketAncestorRefs(ctx, rdoc.ContinuesStream, ancRefs, seen); err != nil {
		return bucketReclaimedDoc{}, nil, err
	}
	if len(rdoc.KnownPinSeqs) != 0 && len(rdoc.KnownPinSeqs) != len(rdoc.KnownPins) {
		return bucketReclaimedDoc{}, nil, fmt.Errorf("%w: reclaimed.json known_pin_seqs length mismatch", ErrInvalidStore)
	}
	known := make(map[journal.Pin]bool, len(rdoc.KnownPins))
	knownSeq := make(map[journal.Pin]int64, len(rdoc.KnownPinSeqs))
	for i, p := range rdoc.KnownPins {
		known[p] = true
		if i < len(rdoc.KnownPinSeqs) {
			knownSeq[p] = rdoc.KnownPinSeqs[i]
		}
	}
	return rdoc, &journal.SealedSourceSummary{
		Capability:                         "contract: durable-record/v1",
		Stream:                             stream,
		CutSeq:                             rdoc.CutSeq,
		CutDigest:                          rdoc.CutDigest,
		EffectiveRestrictionManifestDigest: rdoc.EffectiveRestrictionManifestDigest,
		KnownPins:                          known,
		KnownPinSeq:                        knownSeq,
		HistoricalCuts:                     rdoc.HistoricalCuts,
		HasAncestorHold:                    false,
	}, nil
}

func (s *BucketStore) sourceRetiredRecordIDs(ctx context.Context, stream string, seen map[string]bool) (map[string]bool, error) {
	if seen[stream] || len(seen) > 16 {
		return nil, fmt.Errorf("%w: continuation cycle or depth exceeded", ErrInvalidStore)
	}
	nextSeen := copyBucketSeen(seen)
	nextSeen[stream] = true
	out := map[string]bool{}

	hBytes, _, _, err := s.obj.GetCoherent(ctx, s.headKey(stream))
	if err != nil {
		return nil, fmt.Errorf("%w: ancestor %q head missing: %v", ErrInvalidStore, stream, err)
	}
	var head HeadDoc
	if err := json.Unmarshal(hBytes, &head); err != nil {
		return nil, fmt.Errorf("%w: ancestor %q head decode: %v", ErrInvalidStore, stream, err)
	}
	if head.Reclaimed {
		rdoc, _, err := s.loadAndVerifyBucketReclaimed(ctx, stream, head, copyBucketSeen(nextSeen))
		if err != nil {
			return nil, err
		}
		mNode, merr := wire.ParseJSON(rdoc.Manifest)
		if merr != nil {
			return nil, merr
		}
		if retArr := mNode.Get("retired"); retArr != nil && retArr.Kind == wire.Array {
			for _, el := range retArr.Elems {
				if el != nil && el.Kind == wire.Object && el.Get("record_id") != nil {
					out[el.Get("record_id").Str] = true
				}
			}
		}
		if rdoc.ContinuesStream != "" {
			upRet, uerr := s.sourceRetiredRecordIDs(ctx, rdoc.ContinuesStream, nextSeen)
			if uerr != nil {
				return nil, uerr
			}
			for k := range upRet {
				out[k] = true
			}
		}
		return out, nil
	}
	ls, _, exists, err := s.loadStreamWithSeen(ctx, stream, copyBucketSeen(nextSeen))
	if err != nil || !exists {
		return nil, fmt.Errorf("%w: load ancestor %q: %v", ErrInvalidStore, stream, err)
	}
	for pk := range ls.st.Records() {
		if ls.st.IsRetired(pk.RecordID) {
			out[pk.RecordID] = true
		}
	}
	if cn := ls.st.ContinuesNode(); cn != nil {
		upRet, uerr := s.sourceRetiredRecordIDs(ctx, cn.Get("stream").Str, nextSeen)
		if uerr != nil {
			return nil, uerr
		}
		for k := range upRet {
			out[k] = true
		}
	}
	return out, nil
}

func (s *BucketStore) collectBucketAncestorSeals(ctx context.Context, srcStream string, seen map[string]bool) ([]bucketAncestorSealRef, []*wire.Node, error) {
	byStream := map[string]bucketAncestorSealRef{}
	cur := srcStream
	for cur != "" {
		if seen[cur] || len(seen) > 16 {
			return nil, nil, fmt.Errorf("%w: cycle or depth exceeded in ancestor seals", ErrInvalidStore)
		}
		seen[cur] = true
		hBytes, _, _, err := s.obj.GetCoherent(ctx, s.headKey(cur))
		if err != nil {
			return nil, nil, fmt.Errorf("%w: ancestor %q head missing: %v", ErrInvalidStore, cur, err)
		}
		var head HeadDoc
		if err := json.Unmarshal(hBytes, &head); err != nil || !head.Sealed {
			return nil, nil, fmt.Errorf("%w: ancestor %q is not sealed", ErrInvalidStore, cur)
		}
		_, sealDig, ancRefs, err := s.loadAndVerifyBucketSealDoc(ctx, cur)
		if err != nil || sealDig != head.SealDigest {
			return nil, nil, fmt.Errorf("%w: ancestor %q seal invalid", ErrInvalidStore, cur)
		}
		byStream[cur] = bucketAncestorSealRef{
			Store:      s.storeID,
			Capability: "contract: durable-record/v1",
			Stream:     cur,
			CutSeq:     head.CutSeq,
			SealDigest: sealDig,
		}
		for _, ar := range ancRefs {
			byStream[ar.Stream] = ar
		}
		if head.Reclaimed {
			rdoc, _, err := s.loadAndVerifyBucketReclaimed(ctx, cur, head, copyBucketSeen(seen))
			if err != nil {
				return nil, nil, err
			}
			cur = rdoc.ContinuesStream
			break
		}
		ls, _, exists, err := s.loadStreamWithSeen(ctx, cur, copyBucketSeen(seen))
		if err != nil || !exists {
			return nil, nil, fmt.Errorf("%w: load ancestor %q: %v", ErrInvalidStore, cur, err)
		}
		if cn := ls.st.ContinuesNode(); cn != nil {
			cur = cn.Get("stream").Str
		} else {
			cur = ""
		}
	}

	type pair struct {
		jcs  string
		ref  bucketAncestorSealRef
		node *wire.Node
	}
	pairs := make([]pair, 0, len(byStream))
	for _, r := range byStream {
		cutTok := strconv.FormatInt(r.CutSeq, 10)
		n := &wire.Node{
			Kind: wire.Object,
			Fields: []wire.Member{
				{Name: "store", Value: &wire.Node{Kind: wire.String, Str: r.Store}},
				{Name: "capability", Value: &wire.Node{Kind: wire.String, Str: r.Capability}},
				{Name: "stream", Value: &wire.Node{Kind: wire.String, Str: r.Stream}},
				{Name: "cut_seq", Value: &wire.Node{Kind: wire.Number, Token: cutTok, IsInt: true, Int: r.CutSeq}},
				{Name: "seal_digest", Value: &wire.Node{Kind: wire.String, Str: r.SealDigest}},
			},
		}
		jb, err := canon.Canonicalize(wire.Encode(n))
		if err != nil {
			return nil, nil, err
		}
		pairs = append(pairs, pair{jcs: string(jb), ref: r, node: n})
	}
	slices.SortFunc(pairs, func(a, b pair) int {
		return strings.Compare(a.jcs, b.jcs)
	})
	refs := make([]bucketAncestorSealRef, len(pairs))
	nodes := make([]*wire.Node, len(pairs))
	for i, p := range pairs {
		refs[i] = p.ref
		nodes[i] = p.node
	}
	return refs, nodes, nil
}

func (s *BucketStore) attachBucketContinuation(ctx context.Context, dst *journal.State, cont *wire.Node, seen map[string]bool) error {
	if cont == nil || cont.Kind != wire.Object || len(seen) > 16 {
		return fmt.Errorf("%w: invalid continuation", ErrInvalidStore)
	}
	capNode, streamNode := cont.Get("capability"), cont.Get("stream")
	if capNode == nil || capNode.Str != "contract: durable-record/v1" || streamNode == nil {
		return fmt.Errorf("%w: unsupported continuation capability", ErrInvalidStore)
	}
	srcStream := streamNode.Str
	if seen[srcStream] || srcStream == s.bodyRemovalStream {
		return fmt.Errorf("%w: continuation cycle or invalid source stream", ErrInvalidStore)
	}
	nextSeen := copyBucketSeen(seen)
	nextSeen[srcStream] = true

	// Check if source head is marked reclaimed.
	hBytes, _, _, err := s.obj.GetCoherent(ctx, s.headKey(srcStream))
	if err != nil {
		return fmt.Errorf("%w: continuation source head %q: %v", ErrInvalidStore, srcStream, err)
	}
	var srcHead HeadDoc
	if err := json.Unmarshal(hBytes, &srcHead); err != nil {
		return fmt.Errorf("%w: continuation source head decode: %v", ErrInvalidStore, err)
	}
	if srcHead.Reclaimed {
		_, summary, err := s.loadAndVerifyBucketReclaimed(ctx, srcStream, srcHead, nextSeen)
		if err != nil {
			return err
		}
		return dst.SetSealedSource(summary)
	}

	srcLS, _, exists, err := s.loadStreamWithSeen(ctx, srcStream, nextSeen)
	if err != nil || !exists {
		return fmt.Errorf("%w: load continuation source %q: %v", ErrInvalidStore, srcStream, err)
	}
	return dst.AttachVerifiedSourceState(srcLS.st, srcLS.head.Sealed)
}

// CompactRecordInput specifies one retained record to import into the
// continuation stream during BucketStore.SealAndCompactStream.
type CompactRecordInput struct {
	Record         []byte
	SourcePin      journal.Pin
	IdempotencyKey string
}

// CompactionInput configures BucketStore.SealAndCompactStream.
type CompactionInput struct {
	SourceStream       string
	DestinationStream  string
	Mode               string
	Records            []CompactRecordInput
	AllowDropUnretired bool
	ActorKind          string
	Actor              string
	AuthRef            string
	AuthDigest         string
	ReclaimPredecessor bool
}

// CompactionOutput summarizes the result of BucketStore.SealAndCompactStream.
type CompactionOutput struct {
	SourceStream          string
	DestinationStream     string
	SealedCutSeq          int64
	SealedCutDigest       string
	SealDigest            string
	DestinationCutSeq     int64
	ReclaimedSegmentCount int
	ReclaimedSegmentBytes int64
}

func (s *BucketStore) verifyExistingBucketCompactionDestination(ctx context.Context, in CompactionInput, wantSrcCutSeq int64, wantSrcCutDigest string) (*loadedStream, error) {
	dstLS, _, dstExists, err := s.loadStream(ctx, in.DestinationStream)
	if err != nil {
		return nil, err
	}
	if !dstExists {
		return nil, fmt.Errorf("%w: destination stream %q missing", ErrInvalidStore, in.DestinationStream)
	}
	if dstLS.head.Mode != in.Mode || dstLS.st.ReviewPending() {
		return nil, fmt.Errorf("%w: existing destination stream %q mode or review state mismatch", ErrInvalidStore, in.DestinationStream)
	}
	cn := dstLS.st.ContinuesNode()
	if cn == nil ||
		cn.Get("capability").Str != "contract: durable-record/v1" ||
		cn.Get("stream").Str != in.SourceStream ||
		cn.Get("cut_seq").Int != wantSrcCutSeq ||
		cn.Get("cut_digest").Str != wantSrcCutDigest {
		return nil, fmt.Errorf("%w: existing destination stream %q continuation mismatch", ErrInvalidStore, in.DestinationStream)
	}
	dstRecs := dstLS.st.Records()
	if len(dstRecs) != len(in.Records) {
		return nil, fmt.Errorf("%w: existing destination stream %q record count mismatch", ErrInvalidStore, in.DestinationStream)
	}
	matchedDstPins := make(map[journal.Pin]bool, len(in.Records))
	for _, r := range in.Records {
		rn, err := wire.Ingest(r.Record, wire.KindRecord)
		if err != nil {
			return nil, fmt.Errorf("%w: record ingest: %v", ErrInvalidStore, err)
		}
		if err := shape.Validate("v1/record.schema.json", rn); err != nil {
			return nil, fmt.Errorf("%w: record schema: %v", ErrInvalidStore, err)
		}
		if rn.Get("stream").Str != in.DestinationStream || rn.Get("record_id").Str != r.SourcePin.RecordID {
			return nil, fmt.Errorf("%w: compacted record stream or record_id mismatch", ErrInvalidStore)
		}
		canRec, err := canon.Canonicalize(wire.Encode(rn))
		if err != nil {
			return nil, err
		}
		dstPin := journal.Pin{
			Stream:   in.DestinationStream,
			RecordID: rn.Get("record_id").Str,
			Revision: rn.Get("revision").Int,
			Digest:   sha256Prefixed(canRec),
		}
		if dstRecs[dstPin] == nil || matchedDstPins[dstPin] {
			return nil, fmt.Errorf("%w: existing destination stream %q missing or duplicate compacted record %q", ErrInvalidStore, in.DestinationStream, dstPin.RecordID)
		}
		matchedDstPins[dstPin] = true
		impSrc, ok := dstLS.st.ImportedFromForPin(dstPin)
		if !ok || impSrc.SourceCapability != "contract: durable-record/v1" || impSrc.SourcePin != r.SourcePin {
			return nil, fmt.Errorf("%w: existing destination stream %q missing imported_from binding for %q", ErrInvalidStore, in.DestinationStream, dstPin.RecordID)
		}
	}
	if len(matchedDstPins) != len(dstRecs) {
		return nil, fmt.Errorf("%w: existing destination stream %q record set mismatch", ErrInvalidStore, in.DestinationStream)
	}
	return dstLS, nil
}

// SealAndCompactStream seals in.SourceStream at its current head via
// stream-seal.json PutIfAbsent + head.json PutIfMatch, opens or updates
// in.DestinationStream with imported_from records and restrictions_reviewed in
// a single segment + head.json CAS, and optionally reclaims the predecessor's
// segment and checkpoint objects.
func (s *BucketStore) SealAndCompactStream(ctx context.Context, in CompactionInput) (CompactionOutput, error) {
	if in.SourceStream == "" || in.DestinationStream == "" || in.SourceStream == in.DestinationStream ||
		in.SourceStream == s.bodyRemovalStream || in.DestinationStream == s.bodyRemovalStream {
		return CompactionOutput{}, fmt.Errorf("%w: invalid source or destination stream", ErrInvalidStore)
	}
	if in.Mode != "single_revision" && in.Mode != "revisioned" {
		return CompactionOutput{}, fmt.Errorf("%w: invalid mode %q", ErrInvalidStore, in.Mode)
	}

	type dstIDRev struct {
		rid string
		rev int64
	}
	seenKeys := make(map[string]bool, len(in.Records))
	seenSrcPins := make(map[journal.Pin]bool, len(in.Records))
	seenDstIDRevs := make(map[dstIDRev]bool, len(in.Records))
	preservedBodies := make(map[journal.BodyKey]bool)
	for _, r := range in.Records {
		if r.IdempotencyKey == "" || seenKeys[r.IdempotencyKey] {
			return CompactionOutput{}, fmt.Errorf("%w: invalid or duplicate compaction idempotency key", ErrInvalidStore)
		}
		seenKeys[r.IdempotencyKey] = true
		if r.SourcePin.Stream != in.SourceStream || r.SourcePin.RecordID == "" || r.SourcePin.Revision < 1 || r.SourcePin.Digest == "" || seenSrcPins[r.SourcePin] {
			return CompactionOutput{}, fmt.Errorf("%w: invalid or duplicate compaction source pin", ErrInvalidStore)
		}
		seenSrcPins[r.SourcePin] = true
		rn, err := wire.Ingest(r.Record, wire.KindRecord)
		if err != nil {
			return CompactionOutput{}, fmt.Errorf("%w: record ingest: %v", ErrInvalidStore, err)
		}
		if err := shape.Validate("v1/record.schema.json", rn); err != nil {
			return CompactionOutput{}, fmt.Errorf("%w: record schema: %v", ErrInvalidStore, err)
		}
		if rn.Get("stream").Str != in.DestinationStream || rn.Get("record_id").Str != r.SourcePin.RecordID {
			return CompactionOutput{}, fmt.Errorf("%w: compacted record stream or record_id mismatch", ErrInvalidStore)
		}
		dk := dstIDRev{rid: rn.Get("record_id").Str, rev: rn.Get("revision").Int}
		if seenDstIDRevs[dk] {
			return CompactionOutput{}, fmt.Errorf("%w: duplicate destination record revision", ErrInvalidStore)
		}
		seenDstIDRevs[dk] = true
		if bNode := rn.Get("body"); bNode != nil {
			bDig := bNode.Get("digest").Str
			bLen := bNode.Get("length").Int
			bst, err := s.BodyState(ctx, bDig, bLen)
			if err != nil {
				return CompactionOutput{}, err
			}
			if bst != "available" {
				return CompactionOutput{}, fmt.Errorf("%w: body %s is %s, not available", ErrInvalidStore, bDig, bst)
			}
			preservedBodies[journal.BodyKey{Digest: bDig, Length: bLen}] = true
		}
	}

	var srcLS *loadedStream
	var sealDig string
	var effDig string
	var atStr string
	var cutTok string
	sealedConfirmed := false

	for attempt := 0; attempt < s.opts.MaxCASRetries; attempt++ {
		var exists bool
		var err error
		srcLS, _, exists, err = s.loadStream(ctx, in.SourceStream)
		if err != nil {
			return CompactionOutput{}, err
		}
		if !exists {
			return CompactionOutput{}, fmt.Errorf("%w: source stream %q not found", ErrInvalidStore, in.SourceStream)
		}
		srcRetired, err := s.sourceRetiredRecordIDs(ctx, in.SourceStream, map[string]bool{})
		if err != nil {
			return CompactionOutput{}, err
		}
		for _, r := range in.Records {
			if srcRetired[r.SourcePin.RecordID] {
				return CompactionOutput{}, fmt.Errorf("%w: unimported_retired_record_id %q", ErrInvalidStore, r.SourcePin.RecordID)
			}
		}

		// Handle idempotent retry after predecessor reclamation.
		if srcLS.head.Reclaimed {
			rdoc, summary, err := s.loadAndVerifyBucketReclaimed(ctx, in.SourceStream, srcLS.head, map[string]bool{in.SourceStream: true})
			if err != nil {
				return CompactionOutput{}, err
			}
			for _, r := range in.Records {
				if !summary.KnownPins[r.SourcePin] {
					return CompactionOutput{}, fmt.Errorf("%w: source pin not found in reclaimed source stream", ErrInvalidStore)
				}
			}
			dstLS, err := s.verifyExistingBucketCompactionDestination(ctx, in, rdoc.CutSeq, rdoc.CutDigest)
			if err != nil {
				return CompactionOutput{}, err
			}
			out := CompactionOutput{
				SourceStream:      in.SourceStream,
				DestinationStream: in.DestinationStream,
				SealedCutSeq:      rdoc.CutSeq,
				SealedCutDigest:   rdoc.CutDigest,
				SealDigest:        rdoc.SealDigest,
				DestinationCutSeq: dstLS.head.CutSeq,
			}
			if in.ReclaimPredecessor {
				cnt, bytesReclaimed, rerr := s.ReclaimSealedAncestor(ctx, in.SourceStream, in.DestinationStream)
				if rerr != nil {
					return CompactionOutput{}, rerr
				}
				out.ReclaimedSegmentCount = cnt
				out.ReclaimedSegmentBytes = bytesReclaimed
			}
			return out, nil
		}

		if len(srcLS.st.ActiveHolds()) > 0 || srcLS.st.HasAncestorHold() {
			return CompactionOutput{}, fmt.Errorf("%w: source stream has active hold", ErrInvalidStore)
		}
		if srcLS.st.ReviewPending() {
			return CompactionOutput{}, fmt.Errorf("%w: source stream has pending restriction review", ErrInvalidStore)
		}

		srcRecords := srcLS.st.Records()
		importedIDs := map[string]bool{}
		for _, r := range in.Records {
			if srcRecords[r.SourcePin] == nil {
				return CompactionOutput{}, fmt.Errorf("%w: source pin not found in source stream", ErrInvalidStore)
			}
			importedIDs[r.SourcePin.RecordID] = true
		}
		if !in.AllowDropUnretired {
			for pk := range srcRecords {
				if !srcLS.st.IsRetired(pk.RecordID) && !importedIDs[pk.RecordID] {
					return CompactionOutput{}, fmt.Errorf("%w: unretired record %q dropped without AllowDropUnretired", ErrInvalidStore, pk.RecordID)
				}
			}
		}
		for pk, recNode := range srcRecords {
			if srcLS.st.IsRetired(pk.RecordID) {
				continue
			}
			if bNode := recNode.Get("body"); bNode != nil {
				bk := journal.BodyKey{
					Digest: bNode.Get("digest").Str,
					Length: bNode.Get("length").Int,
				}
				if !preservedBodies[bk] {
					return CompactionOutput{}, fmt.Errorf("%w: body_unpreserved %s", ErrInvalidStore, bk.Digest)
				}
			}
		}

		// If destination stream already exists before compaction, verify it is an
		// idempotent retry of this compaction; refuse unrelated destinations.
		if _, _, dstExists, err := s.loadStream(ctx, in.DestinationStream); err != nil {
			return CompactionOutput{}, err
		} else if dstExists {
			if _, err := s.verifyExistingBucketCompactionDestination(ctx, in, srcLS.head.CutSeq, srcLS.head.CutDigest); err != nil {
				return CompactionOutput{}, err
			}
		}

		effDig, err = srcLS.st.EffectiveRestrictionManifestDigest()
		if err != nil {
			return CompactionOutput{}, err
		}
		var ancNodes []*wire.Node
		if cn := srcLS.st.ContinuesNode(); cn != nil {
			_, nodes, err := s.collectBucketAncestorSeals(ctx, cn.Get("stream").Str, map[string]bool{in.SourceStream: true})
			if err != nil {
				return CompactionOutput{}, err
			}
			ancNodes = nodes
		} else {
			ancNodes = []*wire.Node{}
		}

		now := s.opts.Now().UTC().Truncate(time.Microsecond)
		if now.Before(srcLS.lastAt) {
			return CompactionOutput{}, ErrClockRegression
		}
		atStr = formatInstant(now)
		cutTok = strconv.FormatInt(srcLS.head.CutSeq, 10)

		if srcLS.head.Sealed {
			sealDig, err = s.verifyBucketSealAgainstLoaded(ctx, in.SourceStream, srcLS.head, srcLS.st, map[string]bool{in.SourceStream: true})
			if err != nil {
				return CompactionOutput{}, err
			}
			sealedConfirmed = true
			break
		}

		sealNode := &wire.Node{
			Kind: wire.Object,
			Fields: []wire.Member{
				{Name: "v", Value: &wire.Node{Kind: wire.Number, Token: "1", IsInt: true, Int: 1}},
				{Name: "store", Value: &wire.Node{Kind: wire.String, Str: s.storeID}},
				{Name: "capability", Value: &wire.Node{Kind: wire.String, Str: "contract: durable-record/v1"}},
				{Name: "stream", Value: &wire.Node{Kind: wire.String, Str: in.SourceStream}},
				{Name: "cut_seq", Value: &wire.Node{Kind: wire.Number, Token: cutTok, IsInt: true, Int: srcLS.head.CutSeq}},
				{Name: "cut_digest", Value: &wire.Node{Kind: wire.String, Str: srcLS.head.CutDigest}},
				{Name: "effective_restriction_manifest_digest", Value: &wire.Node{Kind: wire.String, Str: effDig}},
				{Name: "ancestor_seals", Value: &wire.Node{Kind: wire.Array, Elems: ancNodes}},
				{Name: "trust_profile", Value: &wire.Node{
					Kind: wire.Object,
					Fields: []wire.Member{
						{Name: "id", Value: &wire.Node{Kind: wire.String, Str: "recordprims-cas"}},
						{Name: "version", Value: &wire.Node{Kind: wire.String, Str: "v1"}},
					},
				}},
				{Name: "sealed_at", Value: &wire.Node{Kind: wire.String, Str: atStr}},
				{Name: "provenance", Value: &wire.Node{
					Kind: wire.Object,
					Fields: []wire.Member{
						{Name: "actor_kind", Value: &wire.Node{Kind: wire.String, Str: in.ActorKind}},
						{Name: "actor", Value: &wire.Node{Kind: wire.String, Str: in.Actor}},
					},
				}},
				{Name: "authorization", Value: &wire.Node{
					Kind: wire.Object,
					Fields: []wire.Member{
						{Name: "ref", Value: &wire.Node{Kind: wire.String, Str: in.AuthRef}},
						{Name: "digest", Value: &wire.Node{Kind: wire.String, Str: in.AuthDigest}},
					},
				}},
			},
		}
		if err := shape.Validate("v1/stream-seal.schema.json", sealNode); err != nil {
			return CompactionOutput{}, err
		}
		canonSeal, err := canon.Canonicalize(wire.Encode(sealNode))
		if err != nil {
			return CompactionOutput{}, err
		}
		sealDig = sha256Prefixed(canonSeal)
		sealBytes := append(canonSeal, '\n')
		if _, commit, conflict, err := s.obj.PutIfAbsent(ctx, s.sealKey(in.SourceStream), sealBytes); err != nil {
			if conflict == stratum.ConflictAlreadyExists {
				_, sealVal, _, gerr := s.obj.GetCoherent(ctx, s.sealKey(in.SourceStream))
				if gerr != nil {
					if errors.Is(gerr, stratum.ErrNotFound) {
						continue
					}
					return CompactionOutput{}, gerr
				}
				existingDig, verr := s.verifyBucketSealAgainstLoaded(ctx, in.SourceStream, srcLS.head, srcLS.st, map[string]bool{in.SourceStream: true})
				if verr == nil {
					sealDig = existingDig
				} else {
					// Existing stream-seal.json does not match srcLS.head; only replace it via
					// validator-fenced PutIfMatch while head.json is still unsealed at srcLS.headValidator.
					hBytes, headVal, _, herr := s.obj.GetCoherent(ctx, s.headKey(in.SourceStream))
					if herr != nil {
						return CompactionOutput{}, herr
					}
					var curHead HeadDoc
					if err := json.Unmarshal(hBytes, &curHead); err != nil {
						return CompactionOutput{}, err
					}
					if curHead.Sealed || curHead.Reclaimed || headVal != srcLS.headValidator {
						continue
					}
					_, scommit, sconf, serr := s.obj.PutIfMatch(ctx, s.sealKey(in.SourceStream), sealVal, sealBytes)
					if serr != nil || scommit != stratum.CommitCommitted {
						if sconf == stratum.ConflictTokenMismatch {
							continue
						}
						if scommit == stratum.CommitUnknown {
							return CompactionOutput{}, &stratum.IndeterminateError{Op: "put_stream_seal", Key: in.SourceStream, Err: serr}
						}
						return CompactionOutput{}, serr
					}
				}
			} else if commit == stratum.CommitUnknown {
				return CompactionOutput{}, &stratum.IndeterminateError{Op: "put_stream_seal", Key: in.SourceStream, Err: err}
			} else {
				return CompactionOutput{}, err
			}
		}

		sealedHead := srcLS.head
		sealedHead.Sealed = true
		sealedHead.SealDigest = sealDig
		hb, err := encodeHead(sealedHead)
		if err != nil {
			return CompactionOutput{}, err
		}
		_, commit, conflict, err := s.obj.PutIfMatch(ctx, s.headKey(in.SourceStream), srcLS.headValidator, hb)
		if err == nil && commit == stratum.CommitCommitted {
			sealedConfirmed = true
			break
		}
		if conflict == stratum.ConflictTokenMismatch {
			if hBytes, _, _, herr := s.obj.GetCoherent(ctx, s.headKey(in.SourceStream)); herr == nil {
				var curHead HeadDoc
				if json.Unmarshal(hBytes, &curHead) == nil {
					if curHead.Reclaimed {
						rdoc, summary, rerr := s.loadAndVerifyBucketReclaimed(ctx, in.SourceStream, curHead, map[string]bool{in.SourceStream: true})
						if rerr != nil {
							return CompactionOutput{}, rerr
						}
						for _, r := range in.Records {
							if !summary.KnownPins[r.SourcePin] {
								return CompactionOutput{}, fmt.Errorf("%w: source pin not found in reclaimed source stream", ErrInvalidStore)
							}
						}
						dstLS, verr := s.verifyExistingBucketCompactionDestination(ctx, in, rdoc.CutSeq, rdoc.CutDigest)
						if verr != nil {
							return CompactionOutput{}, verr
						}
						out := CompactionOutput{
							SourceStream:      in.SourceStream,
							DestinationStream: in.DestinationStream,
							SealedCutSeq:      rdoc.CutSeq,
							SealedCutDigest:   rdoc.CutDigest,
							SealDigest:        rdoc.SealDigest,
							DestinationCutSeq: dstLS.head.CutSeq,
						}
						if in.ReclaimPredecessor {
							cnt, bytesReclaimed, rcerr := s.ReclaimSealedAncestor(ctx, in.SourceStream, in.DestinationStream)
							if rcerr != nil {
								return CompactionOutput{}, rcerr
							}
							out.ReclaimedSegmentCount = cnt
							out.ReclaimedSegmentBytes = bytesReclaimed
						}
						return out, nil
					}
					if curHead.Sealed && curHead.CutSeq == srcLS.head.CutSeq && curHead.CutDigest == srcLS.head.CutDigest {
						// Another compactor sealed the exact same cut; verify its committed seal and adopt it.
						if vDig, verr := s.verifyBucketSealAgainstLoaded(ctx, in.SourceStream, curHead, srcLS.st, map[string]bool{in.SourceStream: true}); verr == nil {
							sealDig = vDig
							sealedConfirmed = true
							break
						}
					}
				}
			}
			continue
		}
		if commit == stratum.CommitUnknown {
			return CompactionOutput{}, &stratum.IndeterminateError{Op: "put_sealed_head", Key: in.SourceStream, Err: err}
		}
		return CompactionOutput{}, fmt.Errorf("%w: seal head CAS failed: %v", ErrInvalidStore, err)
	}
	if !sealedConfirmed {
		return CompactionOutput{}, fmt.Errorf("%w: max CAS retries exceeded sealing %q", ErrCASExhausted, in.SourceStream)
	}

	// Build destination stream entries: stream_opened (with continues) + imported records + restrictions_reviewed.
	winTok := strconv.FormatInt(s.idempotencyWindow, 10)
	dstSt, err := journal.New(&wire.Node{Kind: wire.Number, Token: winTok, IsInt: true, Int: s.idempotencyWindow})
	if err != nil {
		return CompactionOutput{}, err
	}
	dstSt.SetRemovalStream(s.bodyRemovalStream)
	if err := dstSt.AttachVerifiedSourceState(srcLS.st, true); err != nil {
		return CompactionOutput{}, err
	}

	contNode := &wire.Node{
		Kind: wire.Object,
		Fields: []wire.Member{
			{Name: "capability", Value: &wire.Node{Kind: wire.String, Str: "contract: durable-record/v1"}},
			{Name: "stream", Value: &wire.Node{Kind: wire.String, Str: in.SourceStream}},
			{Name: "cut_seq", Value: &wire.Node{Kind: wire.Number, Token: cutTok, IsInt: true, Int: srcLS.head.CutSeq}},
			{Name: "cut_digest", Value: &wire.Node{Kind: wire.String, Str: srcLS.head.CutDigest}},
		},
	}
	openEntry := &wire.Node{
		Kind: wire.Object,
		Fields: []wire.Member{
			{Name: "v", Value: &wire.Node{Kind: wire.Number, Token: "1", IsInt: true, Int: 1}},
			{Name: "seq", Value: &wire.Node{Kind: wire.Number, Token: "1", IsInt: true, Int: 1}},
			{Name: "group", Value: &wire.Node{Kind: wire.Number, Token: "1", IsInt: true, Int: 1}},
			{Name: "group_size", Value: &wire.Node{Kind: wire.Number, Token: "1", IsInt: true, Int: 1}},
			{Name: "stream", Value: &wire.Node{Kind: wire.String, Str: in.DestinationStream}},
			{Name: "type", Value: &wire.Node{Kind: wire.String, Str: "stream_opened"}},
			{Name: "at", Value: &wire.Node{Kind: wire.String, Str: atStr}},
			{Name: "mode", Value: &wire.Node{Kind: wire.String, Str: in.Mode}},
			{Name: "failure_model", Value: &wire.Node{Kind: wire.String, Str: "survives_process_loss"}},
			{Name: "continues", Value: contNode},
		},
	}
	entries := []*wire.Node{openEntry}
	if err := dstSt.Ingest(openEntry); err != nil {
		return CompactionOutput{}, err
	}

	seq := int64(1)
	for _, r := range in.Records {
		seq++
		seqTok := strconv.FormatInt(seq, 10)
		rn, err := wire.Ingest(r.Record, wire.KindRecord)
		if err != nil {
			return CompactionOutput{}, err
		}
		canRec, err := canon.Canonicalize(wire.Encode(rn))
		if err != nil {
			return CompactionOutput{}, err
		}
		recDig := sha256Prefixed(canRec)
		entryType := "appended"
		if rn.Get("revision").Int > 1 {
			entryType = "revised"
		}
		srcRevTok := strconv.FormatInt(r.SourcePin.Revision, 10)
		fields := []wire.Member{
			{Name: "v", Value: &wire.Node{Kind: wire.Number, Token: "1", IsInt: true, Int: 1}},
			{Name: "seq", Value: &wire.Node{Kind: wire.Number, Token: seqTok, IsInt: true, Int: seq}},
			{Name: "group", Value: &wire.Node{Kind: wire.Number, Token: seqTok, IsInt: true, Int: seq}},
			{Name: "group_size", Value: &wire.Node{Kind: wire.Number, Token: "1", IsInt: true, Int: 1}},
			{Name: "stream", Value: &wire.Node{Kind: wire.String, Str: in.DestinationStream}},
			{Name: "type", Value: &wire.Node{Kind: wire.String, Str: entryType}},
			{Name: "at", Value: &wire.Node{Kind: wire.String, Str: atStr}},
			{Name: "record", Value: rn},
			{Name: "digest", Value: &wire.Node{Kind: wire.String, Str: recDig}},
			{Name: "idempotency_key", Value: &wire.Node{Kind: wire.String, Str: r.IdempotencyKey}},
			{Name: "imported_from", Value: &wire.Node{
				Kind: wire.Object,
				Fields: []wire.Member{
					{Name: "capability", Value: &wire.Node{Kind: wire.String, Str: "contract: durable-record/v1"}},
					{Name: "pin", Value: &wire.Node{
						Kind: wire.Object,
						Fields: []wire.Member{
							{Name: "stream", Value: &wire.Node{Kind: wire.String, Str: r.SourcePin.Stream}},
							{Name: "record_id", Value: &wire.Node{Kind: wire.String, Str: r.SourcePin.RecordID}},
							{Name: "revision", Value: &wire.Node{Kind: wire.Number, Token: srcRevTok, IsInt: true, Int: r.SourcePin.Revision}},
							{Name: "digest", Value: &wire.Node{Kind: wire.String, Str: r.SourcePin.Digest}},
						},
					}},
				},
			}},
		}
		if rn.Get("body") != nil {
			fields = append(fields, wire.Member{Name: "body_status", Value: &wire.Node{Kind: wire.String, Str: "verified"}})
		}
		ie := &wire.Node{Kind: wire.Object, Fields: fields}
		if err := dstSt.Ingest(ie); err != nil {
			return CompactionOutput{}, err
		}
		entries = append(entries, ie)
	}

	appDig, err := dstSt.AppliedRestrictionManifestDigest()
	if err != nil {
		return CompactionOutput{}, err
	}
	dstCutBeforeRev := seq
	dstCutBeforeTok := strconv.FormatInt(dstCutBeforeRev, 10)
	seq++
	revSeqTok := strconv.FormatInt(seq, 10)
	revEntry := &wire.Node{
		Kind: wire.Object,
		Fields: []wire.Member{
			{Name: "v", Value: &wire.Node{Kind: wire.Number, Token: "1", IsInt: true, Int: 1}},
			{Name: "seq", Value: &wire.Node{Kind: wire.Number, Token: revSeqTok, IsInt: true, Int: seq}},
			{Name: "group", Value: &wire.Node{Kind: wire.Number, Token: revSeqTok, IsInt: true, Int: seq}},
			{Name: "group_size", Value: &wire.Node{Kind: wire.Number, Token: "1", IsInt: true, Int: 1}},
			{Name: "stream", Value: &wire.Node{Kind: wire.String, Str: in.DestinationStream}},
			{Name: "type", Value: &wire.Node{Kind: wire.String, Str: "restrictions_reviewed"}},
			{Name: "at", Value: &wire.Node{Kind: wire.String, Str: atStr}},
			{Name: "source", Value: contNode},
			{Name: "source_state_digest", Value: &wire.Node{Kind: wire.String, Str: effDig}},
			{Name: "destination_cut_seq", Value: &wire.Node{Kind: wire.Number, Token: dstCutBeforeTok, IsInt: true, Int: dstCutBeforeRev}},
			{Name: "destination_applied_state_digest", Value: &wire.Node{Kind: wire.String, Str: appDig}},
			{Name: "provenance", Value: &wire.Node{
				Kind: wire.Object,
				Fields: []wire.Member{
					{Name: "actor_kind", Value: &wire.Node{Kind: wire.String, Str: in.ActorKind}},
					{Name: "actor", Value: &wire.Node{Kind: wire.String, Str: in.Actor}},
				},
			}},
			{Name: "authorization", Value: &wire.Node{
				Kind: wire.Object,
				Fields: []wire.Member{
					{Name: "ref", Value: &wire.Node{Kind: wire.String, Str: in.AuthRef}},
					{Name: "digest", Value: &wire.Node{Kind: wire.String, Str: in.AuthDigest}},
				},
			}},
		},
	}
	if err := dstSt.Ingest(revEntry); err != nil {
		return CompactionOutput{}, err
	}
	entries = append(entries, revEntry)

	var segBuf bytes.Buffer
	for _, e := range entries {
		can, err := canon.Canonicalize(wire.Encode(e))
		if err != nil {
			return CompactionOutput{}, err
		}
		segBuf.Write(can)
		segBuf.WriteByte('\n')
	}
	segBytes := segBuf.Bytes()
	segHex := sha256Hex(segBytes)
	segKey := s.segmentKey(in.DestinationStream, 1, seq, segHex)
	if _, commit, conflict, err := s.obj.PutIfAbsent(ctx, segKey, segBytes); err != nil && conflict != stratum.ConflictAlreadyExists {
		if commit == stratum.CommitUnknown {
			return CompactionOutput{}, &stratum.IndeterminateError{Op: "put_compacted_segment", Key: segKey, Err: err}
		}
		return CompactionOutput{}, err
	}
	dstCutDig, err := dstSt.CutDigest(seq)
	if err != nil {
		return CompactionOutput{}, err
	}
	dstHead := HeadDoc{
		V:         1,
		Stream:    in.DestinationStream,
		Mode:      in.Mode,
		CutSeq:    seq,
		CutDigest: dstCutDig,
		SegmentsTail: []SegmentRef{
			{
				FirstSeq: 1,
				LastSeq:  seq,
				Key:      segKey,
				Digest:   "sha256:" + segHex,
				Length:   int64(len(segBytes)),
			},
		},
		EffectiveRestrictionManifestDigest: restrictionManifestDigest(dstSt),
	}
	dstHeadBytes, err := encodeHead(dstHead)
	if err != nil {
		return CompactionOutput{}, err
	}
	if _, commit, conflict, err := s.obj.PutIfAbsent(ctx, s.headKey(in.DestinationStream), dstHeadBytes); err != nil {
		if conflict == stratum.ConflictAlreadyExists {
			dstLS, verr := s.verifyExistingBucketCompactionDestination(ctx, in, srcLS.head.CutSeq, srcLS.head.CutDigest)
			if verr != nil {
				return CompactionOutput{}, verr
			}
			seq = dstLS.head.CutSeq
		} else if commit == stratum.CommitUnknown {
			return CompactionOutput{}, &stratum.IndeterminateError{Op: "put_compacted_head", Key: in.DestinationStream, Err: err}
		} else {
			return CompactionOutput{}, err
		}
	}

	out := CompactionOutput{
		SourceStream:      in.SourceStream,
		DestinationStream: in.DestinationStream,
		SealedCutSeq:      srcLS.head.CutSeq,
		SealedCutDigest:   srcLS.head.CutDigest,
		SealDigest:        sealDig,
		DestinationCutSeq: seq,
	}
	if in.ReclaimPredecessor {
		cnt, bytesReclaimed, err := s.ReclaimSealedAncestor(ctx, in.SourceStream, in.DestinationStream)
		if err != nil {
			return CompactionOutput{}, err
		}
		out.ReclaimedSegmentCount = cnt
		out.ReclaimedSegmentBytes = bytesReclaimed
	}
	return out, nil
}

// ReclaimSealedAncestor reclaims the immutable segment and checkpoint objects of
// a sealed predecessor stream in the ndjson-cas bucket stratum after verifying
// that successorStream has a current restrictions_reviewed covering the sealed cut.
func (s *BucketStore) ReclaimSealedAncestor(ctx context.Context, predecessorStream, successorStream string) (reclaimedObjects int, reclaimedBytes int64, err error) {
	predLS, predVal, exists, err := s.loadStream(ctx, predecessorStream)
	if err != nil {
		return 0, 0, err
	}
	if !exists || !predLS.head.Sealed {
		return 0, 0, fmt.Errorf("%w: predecessor stream %q is not sealed", ErrInvalidStore, predecessorStream)
	}
	if predLS.head.Reclaimed {
		rdoc, _, err := s.loadAndVerifyBucketReclaimed(ctx, predecessorStream, predLS.head, map[string]bool{predecessorStream: true})
		if err != nil {
			return 0, 0, err
		}
		if predLS.head.ReclaimedBy != "" && predLS.head.ReclaimedBy != successorStream {
			return 0, 0, fmt.Errorf("%w: predecessor %q already reclaimed into %q, not %q", ErrInvalidStore, predecessorStream, predLS.head.ReclaimedBy, successorStream)
		}
		for _, k := range rdoc.ReclaimedKeys {
			if _, derr := s.obj.Delete(ctx, k); derr != nil {
				return reclaimedObjects, rdoc.ReclaimedBytes, derr
			}
			reclaimedObjects++
		}
		return len(rdoc.ReclaimedKeys), rdoc.ReclaimedBytes, nil
	}
	if len(predLS.st.ActiveHolds()) > 0 || predLS.st.HasAncestorHold() {
		return 0, 0, fmt.Errorf("%w: predecessor stream %q has active hold", ErrInvalidStore, predecessorStream)
	}

	succLS, _, succExists, err := s.loadStream(ctx, successorStream)
	if err != nil {
		return 0, 0, err
	}
	if !succExists || succLS.st.ReviewPending() {
		return 0, 0, fmt.Errorf("%w: successor stream %q has pending restriction review", ErrInvalidStore, successorStream)
	}
	cn := succLS.st.ContinuesNode()
	revNode := succLS.st.ReviewNode()
	revCoversSeal := revNode != nil &&
		revNode.Get("source") != nil &&
		revNode.Get("source").Get("cut_seq").Int == predLS.head.CutSeq &&
		revNode.Get("source").Get("cut_digest").Str == predLS.head.CutDigest
	if cn == nil || cn.Get("stream").Str != predecessorStream || cn.Get("cut_seq").Int > predLS.head.CutSeq || !revCoversSeal {
		return 0, 0, fmt.Errorf("%w: successor stream %q does not continue %q at sealed cut", ErrInvalidStore, successorStream, predecessorStream)
	}
	for pk, recNode := range predLS.st.Records() {
		if predLS.st.IsRetired(pk.RecordID) {
			continue
		}
		if bNode := recNode.Get("body"); bNode != nil {
			bk := journal.BodyKey{
				Digest: bNode.Get("digest").Str,
				Length: bNode.Get("length").Int,
			}
			if !succLS.st.BodyReferenced(bk) {
				return 0, 0, fmt.Errorf("%w: body_unpreserved %s in successor stream %q", ErrInvalidStore, bk.Digest, successorStream)
			}
		}
	}

	mDoc, effDig, err := predLS.st.EffectiveRestrictionManifest()
	if err != nil {
		return 0, 0, err
	}
	canonManifest, err := canon.Canonicalize(wire.Encode(mDoc))
	if err != nil {
		return 0, 0, err
	}
	var knownPins []journal.Pin
	for pk := range predLS.st.Records() {
		knownPins = append(knownPins, pk)
	}
	slices.SortFunc(knownPins, func(a, b journal.Pin) int {
		if a.RecordID != b.RecordID {
			return strings.Compare(a.RecordID, b.RecordID)
		}
		if a.Revision < b.Revision {
			return -1
		}
		if a.Revision > b.Revision {
			return 1
		}
		return strings.Compare(a.Digest, b.Digest)
	})
	if knownPins == nil {
		knownPins = []journal.Pin{}
	}
	pinSeqMap := predLS.st.PinSeqs()
	knownPinSeqs := make([]int64, len(knownPins))
	for i, p := range knownPins {
		knownPinSeqs[i] = pinSeqMap[p]
	}
	var historicalCuts []journal.SealedCutInfo
	for seq := int64(1); seq < predLS.head.CutSeq; seq++ {
		cdig, cerr := predLS.st.CutDigest(seq)
		mdig, merr := predLS.st.ManifestDigestAt(seq)
		if cerr == nil && merr == nil {
			historicalCuts = append(historicalCuts, journal.SealedCutInfo{
				CutSeq:         seq,
				CutDigest:      cdig,
				ManifestDigest: mdig,
			})
		}
	}
	var contStream string
	if pcn := predLS.st.ContinuesNode(); pcn != nil {
		contStream = pcn.Get("stream").Str
	}
	var keysToDelete []string
	for _, seg := range predLS.head.SegmentsTail {
		keysToDelete = append(keysToDelete, seg.Key)
		reclaimedBytes += seg.Length
	}
	if predLS.head.LatestCheckpoint != nil {
		keysToDelete = append(keysToDelete, predLS.head.LatestCheckpoint.Key)
		reclaimedBytes += predLS.head.LatestCheckpoint.Length
	}

	rdoc := bucketReclaimedDoc{
		V:                                  1,
		PredecessorStream:                  predecessorStream,
		CutSeq:                             predLS.head.CutSeq,
		CutDigest:                          predLS.head.CutDigest,
		SealDigest:                         predLS.head.SealDigest,
		EffectiveRestrictionManifestDigest: effDig,
		Manifest:                           canonManifest,
		ContinuesStream:                    contStream,
		KnownPins:                          knownPins,
		KnownPinSeqs:                       knownPinSeqs,
		HistoricalCuts:                     historicalCuts,
		ReclaimedKeys:                      keysToDelete,
		ReclaimedBytes:                     reclaimedBytes,
	}
	rdocBytes, err := jcsCanonicalBytes(rdoc)
	if err != nil {
		return 0, 0, err
	}
	// reclaimed.json is the only durable summary of the predecessor once its
	// segments are deleted: proceed only on a fresh CommitCommitted or on a
	// determinate conflict whose existing bytes are identical.
	if _, commit, conflict, err := s.obj.PutIfAbsent(ctx, s.reclaimedKey(predecessorStream), rdocBytes); err != nil || commit != stratum.CommitCommitted {
		if commit == stratum.CommitUnknown {
			return 0, 0, &stratum.IndeterminateError{Op: "put_reclaimed_doc", Key: predecessorStream, Err: err}
		}
		if err == nil {
			return 0, 0, fmt.Errorf("%w: reclaimed.json publication returned %v without an error", ErrInvalidStore, commit)
		}
		if conflict == stratum.ConflictAlreadyExists {
			existingRaw, _, _, gerr := s.obj.GetCoherent(ctx, s.reclaimedKey(predecessorStream))
			if gerr != nil {
				return 0, 0, gerr
			}
			if !bytes.Equal(existingRaw, rdocBytes) {
				return 0, 0, fmt.Errorf("%w: existing reclaimed.json mismatch for %q", ErrInvalidStore, predecessorStream)
			}
		} else {
			return 0, 0, err
		}
	}

	reclaimedHead := predLS.head
	reclaimedHead.Reclaimed = true
	reclaimedHead.ReclaimedDigest = sha256Prefixed(bytes.TrimSuffix(rdocBytes, []byte{'\n'}))
	reclaimedHead.ReclaimedBy = successorStream
	reclaimedHead.SegmentsTail = nil
	reclaimedHead.LatestCheckpoint = nil
	hb, err := encodeHead(reclaimedHead)
	if err != nil {
		return 0, 0, err
	}
	if _, commit, _, err := s.obj.PutIfMatch(ctx, s.headKey(predecessorStream), predVal, hb); err != nil || commit != stratum.CommitCommitted {
		if commit == stratum.CommitUnknown {
			return 0, 0, &stratum.IndeterminateError{Op: "put_reclaimed_head", Key: predecessorStream, Err: err}
		}
		return 0, 0, fmt.Errorf("%w: reclaim head CAS failed: %v", ErrInvalidStore, err)
	}
	for _, k := range keysToDelete {
		if _, derr := s.obj.Delete(ctx, k); derr != nil {
			return reclaimedObjects, reclaimedBytes, derr
		}
		reclaimedObjects++
	}
	return reclaimedObjects, reclaimedBytes, nil
}
