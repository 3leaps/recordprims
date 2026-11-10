package journal

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"slices"

	"github.com/3leaps/recordprims/internal/shape"
	"github.com/3leaps/recordprims/internal/wire"
)

var capabilityByFamily = map[int64]string{
	0: "contract: durable-record/v0",
	1: "contract: durable-record/v1",
}

const emptyManifestDigest = "sha256:5cb72fd9337154074dc353b97f04c8dc84bba51bf1aa1f73db49a14dd890b93c"

type importItem struct {
	seq int64
	pin Pin
}

type streamKey struct {
	capability string
	stream     string
}

type edgeTuple struct {
	store      string
	capability string
	stream     string
	cutSeq     int64
	sealDigest string
}

func cloneJournal(j []*wire.Node) []*wire.Node {
	out := make([]*wire.Node, len(j))
	for i, e := range j {
		out[i] = cloneNode(e)
	}
	return out
}

func completeCut(journal []*wire.Node, cutSeq int64) bool {
	if cutSeq < 1 || cutSeq > int64(len(journal)) {
		return false
	}
	e := journal[cutSeq-1]
	seq, err1 := intOf(e, "seq")
	grp, err2 := intOf(e, "group")
	gsz, err3 := intOf(e, "group_size")
	if err1 != nil || err2 != nil || err3 != nil {
		return false
	}
	return seq == grp+gsz-1
}

func sortByJCS(nodes []*wire.Node) ([]*wire.Node, error) {
	type pair struct {
		jcs  string
		node *wire.Node
	}
	pairs := make([]pair, len(nodes))
	for i, n := range nodes {
		s, err := jcsString(n)
		if err != nil {
			return nil, err
		}
		pairs[i] = pair{jcs: s, node: n}
	}
	slices.SortFunc(pairs, func(a, b pair) int {
		return cmp.Compare(a.jcs, b.jcs)
	})
	out := make([]*wire.Node, len(nodes))
	for i, p := range pairs {
		out[i] = p.node
	}
	return out, nil
}

func manifestDigest(holds, retired, inherited []*wire.Node, includeInherited bool) (string, error) {
	oh, err := sortByJCS(holds)
	if err != nil {
		return "", err
	}
	or, err := sortByJCS(retired)
	if err != nil {
		return "", err
	}
	fields := []wire.Member{
		{Name: "holds", Value: &wire.Node{Kind: wire.Array, Elems: oh}},
		{Name: "retired", Value: &wire.Node{Kind: wire.Array, Elems: or}},
	}
	if includeInherited {
		oi, err := sortByJCS(inherited)
		if err != nil {
			return "", err
		}
		fields = append(fields, wire.Member{Name: "inherited", Value: &wire.Node{Kind: wire.Array, Elems: oi}})
	}
	return recordDigest(&wire.Node{Kind: wire.Object, Fields: fields})
}

func restrictionState(journal []*wire.Node) (holds map[int64]string, retired map[string]bool, review *wire.Node, err error) {
	holds = map[int64]string{}
	retired = map[string]bool{}
	for _, e := range journal {
		typ, err := strOf(e, "type")
		if err != nil {
			return nil, nil, nil, err
		}
		switch typ {
		case "tombstoned":
			seq, err := intOf(e, "seq")
			if err != nil {
				return nil, nil, nil, err
			}
			rd, err := strOf(e, "request_digest")
			if err != nil {
				return nil, nil, nil, err
			}
			holds[seq] = rd
		case "untombstoned":
			req := e.Get("request")
			if req == nil || req.Kind != wire.Object {
				return nil, nil, nil, reject(ReasonShape, "untombstoned.request is not an object")
			}
			rest, err := intOf(req, "restores")
			if err != nil {
				return nil, nil, nil, err
			}
			delete(holds, rest)
		case "retired":
			rid, err := strOf(e, "record_id")
			if err != nil {
				return nil, nil, nil, err
			}
			retired[rid] = true
		case "restrictions_reviewed":
			review = e
		}
	}
	return holds, retired, review, nil
}

func sourceManifest(journal []*wire.Node) (string, error) {
	if len(journal) == 0 {
		return "", reject(ReasonContinuation, "empty source journal")
	}
	v, err := intOf(journal[0], "v")
	if err != nil {
		return "", err
	}
	capability, ok := capabilityByFamily[v]
	if !ok {
		return "", reject(ReasonContinuation, "unsupported capability family %d", v)
	}
	stream, err := strOf(journal[0], "stream")
	if err != nil {
		return "", err
	}
	holds, retired, review, err := restrictionState(journal)
	if err != nil {
		return "", err
	}
	var inherited []*wire.Node
	if review != nil {
		src := review.Get("source")
		if src == nil || src.Kind != wire.Object {
			return "", reject(ReasonContinuation, "review.source is not an object")
		}
		sc, err := strOf(src, "capability")
		if err != nil {
			return "", err
		}
		ss, err := strOf(src, "stream")
		if err != nil {
			return "", err
		}
		ssd, err := strOf(review, "source_state_digest")
		if err != nil {
			return "", err
		}
		inherited = append(inherited, &wire.Node{
			Kind: wire.Object,
			Fields: []wire.Member{
				{Name: "capability", Value: strNode(sc)},
				{Name: "stream", Value: strNode(ss)},
				{Name: "source_state_digest", Value: strNode(ssd)},
			},
		})
	}
	holdNodes := make([]*wire.Node, 0, len(holds))
	for seq, d := range holds {
		holdNodes = append(holdNodes, &wire.Node{
			Kind: wire.Object,
			Fields: []wire.Member{
				{Name: "capability", Value: strNode(capability)},
				{Name: "stream", Value: strNode(stream)},
				{Name: "seq", Value: intNode(seq)},
				{Name: "request_digest", Value: strNode(d)},
			},
		})
	}
	retNodes := make([]*wire.Node, 0, len(retired))
	for r := range retired {
		retNodes = append(retNodes, &wire.Node{
			Kind: wire.Object,
			Fields: []wire.Member{
				{Name: "capability", Value: strNode(capability)},
				{Name: "stream", Value: strNode(stream)},
				{Name: "record_id", Value: strNode(r)},
			},
		})
	}
	return manifestDigest(holdNodes, retNodes, inherited, true)
}

func appliedManifest(journal []*wire.Node) (string, error) {
	if len(journal) == 0 {
		return "", reject(ReasonContinuation, "empty destination journal")
	}
	capability := capabilityByFamily[1]
	stream, err := strOf(journal[0], "stream")
	if err != nil {
		return "", err
	}
	holds, retired, _, err := restrictionState(journal)
	if err != nil {
		return "", err
	}
	holdNodes := make([]*wire.Node, 0, len(holds))
	for seq, d := range holds {
		holdNodes = append(holdNodes, &wire.Node{
			Kind: wire.Object,
			Fields: []wire.Member{
				{Name: "capability", Value: strNode(capability)},
				{Name: "stream", Value: strNode(stream)},
				{Name: "seq", Value: intNode(seq)},
				{Name: "request_digest", Value: strNode(d)},
			},
		})
	}
	retNodes := make([]*wire.Node, 0, len(retired))
	for r := range retired {
		retNodes = append(retNodes, &wire.Node{
			Kind: wire.Object,
			Fields: []wire.Member{
				{Name: "capability", Value: strNode(capability)},
				{Name: "stream", Value: strNode(stream)},
				{Name: "record_id", Value: strNode(r)},
			},
		})
	}
	return manifestDigest(holdNodes, retNodes, nil, false)
}

func sourcePins(journal []*wire.Node) (map[Pin]bool, error) {
	out := map[Pin]bool{}
	for _, e := range journal {
		typ, err := strOf(e, "type")
		if err != nil {
			return nil, err
		}
		if typ == "appended" || typ == "revised" {
			p, err := pinOf(e, "source")
			if err != nil {
				return nil, err
			}
			out[p] = true
		}
	}
	return out, nil
}

func (s *State) sourceCut(cut *wire.Node) ([]*wire.Node, error) {
	if len(s.source) == 0 || cut == nil || cut.Kind != wire.Object {
		return nil, reject(ReasonContinuation, "missing source journal or invalid cut")
	}
	v0, err := intOf(s.source[0], "v")
	if err != nil {
		return nil, reject(ReasonContinuation, "source[0].v invalid")
	}
	cap0, ok := capabilityByFamily[v0]
	if !ok {
		return nil, reject(ReasonContinuation, "unsupported source family %d", v0)
	}
	cutCap, err := strOf(cut, "capability")
	if err != nil || cutCap != cap0 {
		return nil, reject(ReasonContinuation, "cut capability mismatch")
	}
	for _, e := range s.source {
		ev, err := intOf(e, "v")
		if err != nil || ev != v0 {
			return nil, reject(ReasonContinuation, "inconsistent source journal version")
		}
	}
	cutStream, err := strOf(cut, "stream")
	if err != nil {
		return nil, reject(ReasonContinuation, "cut.stream invalid")
	}
	srcStream, err := strOf(s.source[0], "stream")
	if err != nil || cutStream != srcStream {
		return nil, reject(ReasonContinuation, "cut stream mismatch")
	}
	cutSeq, err := intOf(cut, "cut_seq")
	if err != nil || !completeCut(s.source, cutSeq) {
		return nil, reject(ReasonContinuation, "cut_seq %d is not a complete cut", cutSeq)
	}
	prefix := s.source[:cutSeq]
	gotDig, err := sliceDigest(prefix)
	if err != nil {
		return nil, reject(ReasonContinuation, "cut digest calculation failed")
	}
	wantDig, err := strOf(cut, "cut_digest")
	if err != nil || gotDig != wantDig {
		return nil, reject(ReasonContinuation, "cut_digest mismatch")
	}
	return prefix, nil
}

func (s *State) openContinuation(cut *wire.Node) error {
	if _, err := s.sourceCut(cut); err != nil {
		return err
	}
	s.continues = cloneNode(cut)
	return nil
}

func (s *State) importRecord(e *wire.Node, seq int64) error {
	if s.continues == nil {
		return reject(ReasonContinuation, "imported_from on non-continuation stream")
	}
	imp := e.Get("imported_from")
	if imp == nil || imp.Kind != wire.Object {
		return reject(ReasonContinuation, "imported_from is not an object")
	}
	impCap, err := strOf(imp, "capability")
	if err != nil {
		return reject(ReasonContinuation, "imported_from.capability invalid")
	}
	contCap, _ := strOf(s.continues, "capability")
	contStream, _ := strOf(s.continues, "stream")
	p, err := pinFromNode(imp.Get("pin"), "imported_from.pin")
	if err != nil {
		return reject(ReasonContinuation, "imported_from.pin invalid")
	}
	if impCap != contCap || p.Stream != contStream {
		return reject(ReasonContinuation, "imported_from does not match continues")
	}
	s.imports = append(s.imports, importItem{seq: seq, pin: p})
	return nil
}

func (s *State) checkReview(e *wire.Node, seq int64) error {
	if s.continues == nil {
		return reject(ReasonContinuation, "restrictions_reviewed on non-continuation stream")
	}
	src := e.Get("source")
	if src == nil || src.Kind != wire.Object {
		return reject(ReasonContinuation, "review.source is not an object")
	}
	srcCap, err := strOf(src, "capability")
	if err != nil {
		return reject(ReasonContinuation, "review.source.capability invalid")
	}
	srcStream, err := strOf(src, "stream")
	if err != nil {
		return reject(ReasonContinuation, "review.source.stream invalid")
	}
	contCap, _ := strOf(s.continues, "capability")
	contStream, _ := strOf(s.continues, "stream")
	if srcCap != contCap || srcStream != contStream {
		return reject(ReasonContinuation, "review.source does not match continues")
	}
	grp, err1 := intOf(e, "group")
	gsz, err2 := intOf(e, "group_size")
	destCut, err3 := intOf(e, "destination_cut_seq")
	if err1 != nil || err2 != nil || err3 != nil || grp != seq || gsz != 1 || destCut != seq-1 {
		return reject(ReasonContinuation, "review group/destination_cut_seq invalid")
	}
	prefix, err := s.sourceCut(src)
	if err != nil {
		return err
	}
	srcCutSeq, _ := intOf(src, "cut_seq")
	contCutSeq, _ := intOf(s.continues, "cut_seq")
	if srcCutSeq < contCutSeq {
		return reject(ReasonContinuation, "review cut_seq precedes opening cut")
	}
	openDig, err := sliceDigest(prefix[:contCutSeq])
	if err != nil {
		return reject(ReasonContinuation, "opening prefix digest failed")
	}
	contDig, _ := strOf(s.continues, "cut_digest")
	if openDig != contDig {
		return reject(ReasonContinuation, "opening prefix digest mismatch")
	}
	smDig, err := sourceManifest(prefix)
	if err != nil {
		return reject(ReasonContinuation, "source manifest calculation failed")
	}
	wantSrcDig, err := strOf(e, "source_state_digest")
	if err != nil || smDig != wantSrcDig {
		return reject(ReasonContinuation, "source_state_digest mismatch")
	}
	known, err := sourcePins(prefix)
	if err != nil {
		return reject(ReasonContinuation, "source pins failed")
	}
	for _, imp := range s.imports {
		if !known[imp.pin] {
			return reject(ReasonContinuation, "imported pin not in source prefix")
		}
	}
	amDig, err := appliedManifest(s.journal)
	if err != nil {
		return reject(ReasonContinuation, "applied manifest calculation failed")
	}
	wantAppDig, err := strOf(e, "destination_applied_state_digest")
	if err != nil || amDig != wantAppDig {
		return reject(ReasonContinuation, "destination_applied_state_digest mismatch")
	}
	s.review = cloneNode(e)
	return nil
}

func (s *State) reviewPending(r *wire.Node) (bool, error) {
	if s.continues == nil {
		return present(r, "source_state_digest"), nil
	}
	if s.upstreamUnobserved {
		return true, nil
	}
	smDig, err := sourceManifest(s.source)
	if err != nil {
		return true, err
	}
	if !present(r, "source_state_digest") {
		return true, nil
	}
	rDig, err := strOf(r, "source_state_digest")
	if err != nil || rDig != smDig {
		return true, nil
	}
	return false, nil
}

func (s *State) reviewCurrent() (bool, error) {
	if s.continues == nil {
		return true, nil
	}
	if s.review == nil {
		return false, nil
	}
	after, err := intOf(s.review, "seq")
	if err != nil {
		return false, err
	}
	for _, imp := range s.imports {
		if imp.seq > after {
			return false, nil
		}
	}
	for _, sq := range s.restrictedAt {
		if sq > after {
			return false, nil
		}
	}
	if s.sourceSealFinal {
		return true, nil
	}
	smDig, err := sourceManifest(s.source)
	if err != nil {
		return false, err
	}
	revSrcDig, err := strOf(s.review, "source_state_digest")
	if err != nil || smDig != revSrcDig {
		return false, nil
	}
	if s.upstreamUnobserved || s.upstreamUncurrentSealed {
		return false, nil
	}
	if s.upstream == nil {
		return true, nil
	}
	return s.upstream.reviewCurrent()
}

func checkJournalSource(journal []*wire.Node) error {
	if err := CheckJournal(journal); err != nil {
		return reject(ReasonContinuation, "source journal invalid: %v", err)
	}
	for _, e := range journal {
		atStr, err := strOf(e, "at")
		if err != nil {
			return reject(ReasonContinuation, "source entry missing at")
		}
		if _, ok := parseV0Instant(atStr); !ok {
			return reject(ReasonContinuation, "source entry invalid at timestamp")
		}
	}
	return nil
}

func parseSourceSpec(spec *wire.Node) (parsed []*wire.Node, rawStrings []string, err error) {
	if spec == nil || spec.Kind != wire.Array || len(spec.Elems) == 0 {
		return nil, nil, reject(ReasonContinuation, "invalid source journal container")
	}
	if spec.Elems[0] != nil && spec.Elems[0].Kind == wire.String {
		rawStrings = make([]string, len(spec.Elems))
		parsed = make([]*wire.Node, len(spec.Elems))
		for i, el := range spec.Elems {
			if el == nil || el.Kind != wire.String {
				return nil, nil, reject(ReasonContinuation, "mixed source journal container")
			}
			rawStrings[i] = el.Str
			n, err := wire.Ingest([]byte(el.Str), wire.KindJournalEntry)
			if err != nil {
				return nil, nil, reject(ReasonContinuation, "source raw entry failed wire ingest")
			}
			parsed[i] = n
		}
		if parsed[0] == nil || parsed[0].Kind != wire.Object || parsed[0].Get("v") == nil {
			return nil, nil, reject(ReasonContinuation, "source[0] missing v")
		}
		return parsed, rawStrings, nil
	}
	parsed = make([]*wire.Node, len(spec.Elems))
	for i, el := range spec.Elems {
		if el == nil || el.Kind != wire.Object {
			return nil, nil, reject(ReasonContinuation, "source entry is not an object")
		}
		n, err := wire.Ingest(wire.Encode(el), wire.KindJournalEntry)
		if err != nil {
			return nil, nil, reject(ReasonContinuation, "source entry failed wire domain")
		}
		parsed[i] = n
	}
	if parsed[0].Get("v") == nil {
		return nil, nil, reject(ReasonContinuation, "source[0] missing v")
	}
	return parsed, nil, nil
}

func observed(journal []*wire.Node, window int64, source []*wire.Node, upstream *State, upstreamUnobs bool) (*State, error) {
	if len(journal) == 0 {
		return nil, reject(ReasonContinuation, "empty observed journal")
	}
	family, err := intOf(journal[0], "v")
	if err != nil {
		return nil, reject(ReasonContinuation, "observed[0].v invalid")
	}
	var schemaName string
	switch family {
	case 0:
		schemaName = "v0/journal-entry.schema.json"
	case 1:
		schemaName = "v1/journal-entry.schema.json"
	default:
		return nil, reject(ReasonContinuation, "unsupported family %d", family)
	}
	for _, e := range journal {
		ev, err := intOf(e, "v")
		if err != nil || ev != family {
			return nil, reject(ReasonContinuation, "observed entry family mismatch")
		}
		if err := shape.Validate(schemaName, e); err != nil {
			return nil, reject(ReasonContinuation, "observed entry failed shape")
		}
	}
	if err := checkJournalSource(journal); err != nil {
		return nil, err
	}
	if family == 0 {
		return nil, nil
	}
	st, err := New(intNode(window))
	if err != nil {
		return nil, reject(ReasonContinuation, "window invalid")
	}
	st.source = source
	st.upstream = upstream
	st.upstreamUnobserved = upstreamUnobs
	for _, e := range journal {
		if err := st.Ingest(e); err != nil {
			return nil, reject(ReasonContinuation, "observed v1 ingest failed: %v", err)
		}
	}
	return st, nil
}

func chainWithJournals(firstSpec, ancSpecs *wire.Node, window int64) (source []*wire.Node, upstream *State, upstreamUnobs bool, chainJournals [][]*wire.Node, err error) {
	specs := []*wire.Node{firstSpec}
	if ancSpecs != nil {
		if ancSpecs.Kind != wire.Array {
			return nil, nil, false, nil, reject(ReasonContinuation, "ancestors is not an array")
		}
		specs = append(specs, ancSpecs.Elems...)
	}
	var curSource []*wire.Node
	var curUpstream *State
	var curUnobs bool
	hasUpstream := false
	var validatedRev [][]*wire.Node

	for i := len(specs) - 1; i >= 0; i-- {
		journal, _, err := parseSourceSpec(specs[i])
		if err != nil {
			return nil, nil, false, nil, err
		}
		hasContinues := journal[0].Get("continues") != nil
		if hasContinues && !hasUpstream {
			firstJournal, _, err := parseSourceSpec(specs[0])
			if err != nil {
				return nil, nil, false, nil, err
			}
			return cloneJournal(firstJournal), nil, true, nil, nil
		}
		if !hasContinues && hasUpstream {
			return nil, nil, false, nil, reject(ReasonContinuation, "non-continuation stream has ancestor")
		}
		st, err := observed(journal, window, curSource, curUpstream, curUnobs)
		if err != nil {
			return nil, nil, false, nil, err
		}
		isCont := st != nil && st.continues != nil
		curSource = cloneJournal(journal)
		if isCont {
			curUpstream = st
		} else {
			curUpstream = nil
		}
		curUnobs = false
		hasUpstream = true
		validatedRev = append(validatedRev, cloneJournal(journal))
	}
	chainJournals = make([][]*wire.Node, len(validatedRev))
	for i := range validatedRev {
		chainJournals[i] = validatedRev[len(validatedRev)-1-i]
	}
	return curSource, curUpstream, curUnobs, chainJournals, nil
}

// SourceInput carries the source journal, ancestor journals, and optional
// source seal for a continuation stream.
type SourceInput struct {
	SourceJournal    *wire.Node
	RawSourceJournal *wire.Node
	Ancestors        *wire.Node
	RawAncestors     *wire.Node
	SourceSeal       *wire.Node
}

// SetSourceChain validates and attaches the observed source chain and optional
// source seal to State, matching chain(case) and _apply_case_source_seal in
// the reference interpreter.
func (s *State) SetSourceChain(in SourceInput) error {
	firstSpec := in.SourceJournal
	if in.RawSourceJournal != nil {
		firstSpec = in.RawSourceJournal
	}
	if firstSpec == nil {
		return nil
	}
	ancSpecs := in.Ancestors
	if in.RawAncestors != nil {
		ancSpecs = in.RawAncestors
	}
	src, up, unobs, _, err := chainWithJournals(firstSpec, ancSpecs, s.window)
	if err != nil {
		return err
	}
	s.source = src
	s.upstream = up
	s.upstreamUnobserved = unobs
	if in.SourceSeal != nil {
		if err := s.applyCaseSourceSeal(in.SourceSeal); err != nil {
			return err
		}
	}
	return nil
}

var allowedSealCtxKeys = map[string]bool{
	"seal":                     true,
	"raw_seal":                 true,
	"expected_store":           true,
	"accepted_trust_profile":   true,
	"closer_authenticated":     true,
	"source_write_fenced":      true,
	"trailing_bytes":           true,
	"live_source_unobservable": true,
	"journal":                  true,
	"raw_journal":              true,
	"expected_manifest":        true,
	"expected_manifest_jcs":    true,
	"manifest":                 true,
	"ancestor_seal_contexts":   true,
}

func validateSealCtxEnvelope(ctx *wire.Node, checkTop bool) (*wire.Node, error) {
	if ctx == nil || ctx.Kind != wire.Object {
		return nil, reject(ReasonContinuation, "seal context is not an object")
	}
	if ctx.Get("seal") == nil || ctx.Get("expected_store") == nil || ctx.Get("accepted_trust_profile") == nil {
		return nil, reject(ReasonContinuation, "seal context missing required keys")
	}
	for _, m := range ctx.Fields {
		if !allowedSealCtxKeys[m.Name] {
			return nil, reject(ReasonContinuation, "unexpected key %q in seal context", m.Name)
		}
	}
	seal := ctx.Get("seal")
	typedSeal, err := wire.Ingest(wire.Encode(seal), wire.KindStreamSeal)
	if err != nil {
		return nil, err
	}
	if !checkTop {
		return typedSeal, nil
	}
	var sealedAt string
	if typedSeal.Kind == wire.Object {
		if sa := typedSeal.Get("sealed_at"); sa != nil && sa.Kind == wire.String {
			sealedAt = sa.Str
		}
	}
	if _, err := instantOf(sealedAt, "stream_seal.sealed_at"); err != nil {
		return nil, err
	}
	if rawSeal := ctx.Get("raw_seal"); rawSeal != nil {
		if rawSeal.Kind != wire.String {
			return nil, reject(ReasonContinuation, "raw_seal is not a string")
		}
		parsedRaw, err := wire.Ingest([]byte(rawSeal.Str), wire.KindStreamSeal)
		if err != nil {
			return nil, reject(ReasonContinuation, "raw_seal failed wire ingest")
		}
		if err := shape.Validate("v1/stream-seal.schema.json", parsedRaw); err != nil {
			return nil, reject(ReasonContinuation, "raw_seal failed shape")
		}
	} else {
		if err := shape.Validate("v1/stream-seal.schema.json", typedSeal); err != nil {
			return nil, reject(ReasonContinuation, "seal failed shape")
		}
	}
	return typedSeal, nil
}

func verifyManifestPreimage(mDoc *wire.Node, expectedDigest string) error {
	if mDoc == nil || mDoc.Kind != wire.Object || len(mDoc.Fields) != 3 {
		return reject(ReasonContinuation, "manifest preimage invalid")
	}
	for _, k := range []string{"holds", "retired", "inherited"} {
		v := mDoc.Get(k)
		if v == nil || v.Kind != wire.Array {
			return reject(ReasonContinuation, "manifest.%s is not an array", k)
		}
	}
	dig, err := recordDigest(mDoc)
	if err != nil || dig != expectedDigest {
		return reject(ReasonContinuation, "manifest digest mismatch")
	}
	return nil
}

func nodesEqual(a, b *wire.Node) bool {
	if a == nil || b == nil {
		return false
	}
	sa, err1 := jcsString(a)
	sb, err2 := jcsString(b)
	return err1 == nil && err2 == nil && sa == sb
}

func verifySealAgainstJournal(sealCtx, seal *wire.Node, sourceJournal []*wire.Node) ([]*wire.Node, error) {
	v, err := intOf(seal, "v")
	if err != nil || v != 1 {
		return nil, reject(ReasonContinuation, "seal.v != 1")
	}
	store, err := strOf(seal, "store")
	if err != nil {
		return nil, reject(ReasonContinuation, "seal.store invalid")
	}
	expStore, err := strOf(sealCtx, "expected_store")
	if err != nil || store != expStore {
		return nil, reject(ReasonContinuation, "seal.store != expected_store")
	}
	if !nodesEqual(seal.Get("trust_profile"), sealCtx.Get("accepted_trust_profile")) {
		return nil, reject(ReasonContinuation, "seal.trust_profile != accepted_trust_profile")
	}
	sjV, err := intOf(sourceJournal[0], "v")
	if err != nil {
		return nil, reject(ReasonContinuation, "sourceJournal[0].v invalid")
	}
	sjStream, err := strOf(sourceJournal[0], "stream")
	if err != nil {
		return nil, reject(ReasonContinuation, "sourceJournal[0].stream invalid")
	}
	sealCap, _ := strOf(seal, "capability")
	sealStream, _ := strOf(seal, "stream")
	if sealCap != capabilityByFamily[sjV] || sealStream != sjStream {
		return nil, reject(ReasonContinuation, "seal capability/stream mismatch")
	}
	cutSeq, err := intOf(seal, "cut_seq")
	if err != nil || !completeCut(sourceJournal, cutSeq) {
		return nil, reject(ReasonContinuation, "seal cut_seq is not a complete cut")
	}
	prefix := sourceJournal[:cutSeq]
	prefDig, err := sliceDigest(prefix)
	if err != nil {
		return nil, reject(ReasonContinuation, "prefix digest failed")
	}
	sealCutDig, _ := strOf(seal, "cut_digest")
	if prefDig != sealCutDig {
		return nil, reject(ReasonContinuation, "seal cut_digest mismatch")
	}
	if tb := sealCtx.Get("trailing_bytes"); tb != nil {
		if tb.Kind != wire.String || tb.Str != "" {
			return nil, reject(ReasonContinuation, "non-empty trailing_bytes")
		}
	}
	if swf := sealCtx.Get("source_write_fenced"); swf != nil && swf.Kind == wire.Bool && swf.Bool {
		if int64(len(sourceJournal)) != cutSeq {
			return nil, reject(ReasonContinuation, "source_write_fenced but journal continues past cut_seq")
		}
	}
	smDig, err := sourceManifest(prefix)
	if err != nil {
		return nil, err
	}
	effDig, _ := strOf(seal, "effective_restriction_manifest_digest")
	if smDig != effDig {
		return nil, reject(ReasonContinuation, "effective_restriction_manifest_digest mismatch")
	}
	ancSeals := seal.Get("ancestor_seals")
	if ancSeals == nil || ancSeals.Kind != wire.Array {
		return nil, reject(ReasonContinuation, "ancestor_seals is not an array")
	}
	jcsAnc := make([]string, len(ancSeals.Elems))
	seenKeys := map[streamKey]bool{}
	for i, m := range ancSeals.Elems {
		s, err := jcsString(m)
		if err != nil {
			return nil, reject(ReasonContinuation, "ancestor_seal JCS failed")
		}
		jcsAnc[i] = s
		mc, _ := strOf(m, "capability")
		ms, _ := strOf(m, "stream")
		sk := streamKey{capability: mc, stream: ms}
		if seenKeys[sk] {
			return nil, reject(ReasonContinuation, "duplicate ancestor_seal stream")
		}
		seenKeys[sk] = true
	}
	if !slices.IsSorted(jcsAnc) {
		return nil, reject(ReasonContinuation, "ancestor_seals not JCS-sorted")
	}
	return prefix, nil
}

func checkSealedIntermediateJournal(journal []*wire.Node, ancSeal *wire.Node, windowMs int64) (bool, error) {
	if len(journal) == 0 {
		return false, reject(ReasonContinuation, "empty intermediate journal")
	}
	family, err := intOf(journal[0], "v")
	if err != nil {
		return false, reject(ReasonContinuation, "intermediate[0].v invalid")
	}
	var schemaName string
	switch family {
	case 0:
		schemaName = "v0/journal-entry.schema.json"
	case 1:
		schemaName = "v1/journal-entry.schema.json"
	default:
		return false, reject(ReasonContinuation, "unsupported family %d", family)
	}
	for _, e := range journal {
		ev, err := intOf(e, "v")
		if err != nil || ev != family {
			return false, reject(ReasonContinuation, "intermediate family mismatch")
		}
		if err := shape.Validate(schemaName, e); err != nil {
			return false, reject(ReasonContinuation, "intermediate entry failed shape")
		}
	}
	if err := checkJournalSource(journal); err != nil {
		return false, err
	}
	continues := journal[0].Get("continues")
	if continues == nil || continues.Kind != wire.Object {
		return false, reject(ReasonContinuation, "intermediate journal missing continues")
	}
	contCap, _ := strOf(continues, "capability")
	contStream, _ := strOf(continues, "stream")
	contCutSeq, _ := intOf(continues, "cut_seq")
	contCutDig, _ := strOf(continues, "cut_digest")
	if ancSeal != nil {
		ancCap, _ := strOf(ancSeal, "capability")
		ancStream, _ := strOf(ancSeal, "stream")
		ancCutSeq, _ := intOf(ancSeal, "cut_seq")
		ancCutDig, _ := strOf(ancSeal, "cut_digest")
		if contCap != ancCap || contStream != ancStream || contCutSeq > ancCutSeq {
			return false, reject(ReasonContinuation, "intermediate continues does not match ancestor seal")
		}
		if contCutSeq == ancCutSeq && contCutDig != ancCutDig {
			return false, reject(ReasonContinuation, "intermediate continues cut_digest mismatch")
		}
	}
	if family == 0 {
		return true, nil
	}
	st, err := New(intNode(windowMs))
	if err != nil {
		return false, reject(ReasonContinuation, "window invalid")
	}
	var latestRev *wire.Node
	var importsSeq []int64
	var restrictedSeq []int64

	for idx, e := range journal {
		atStr, err := strOf(e, "at")
		if err != nil {
			return false, reject(ReasonContinuation, "missing at")
		}
		at, err := instantOf(atStr, "intermediate.at")
		if err != nil {
			return false, reject(ReasonContinuation, "invalid at")
		}
		if idx > 0 {
			prevAtStr, _ := strOf(journal[idx-1], "at")
			prevAt, _ := ParseInstant(prevAtStr)
			if at.Before(prevAt) {
				return false, reject(ReasonContinuation, "backwards time in intermediate journal")
			}
		}
		if rec := e.Get("record"); rec != nil {
			if rv, err := intOf(rec, "v"); err != nil || rv != 1 {
				return false, reject(ReasonContinuation, "intermediate record.v != 1")
			}
			if pa := rec.Get("produced_at"); pa != nil {
				ps, err := strOf(pa, "instant")
				if err != nil {
					return false, reject(ReasonContinuation, "invalid produced_at")
				}
				if _, err := instantOf(ps, "produced_at.instant"); err != nil {
					return false, reject(ReasonContinuation, "invalid produced_at.instant")
				}
			}
		}
		if req := e.Get("request"); req != nil {
			obs, err := strOf(req, "observed_at")
			if err != nil {
				return false, reject(ReasonContinuation, "invalid observed_at")
			}
			if _, err := instantOf(obs, "observed_at"); err != nil {
				return false, reject(ReasonContinuation, "invalid observed_at")
			}
		}
		typ, _ := strOf(e, "type")
		seq, _ := intOf(e, "seq")
		switch typ {
		case "stream_opened":
			st.stream, _ = strOf(e, "stream")
			st.failureModel, _ = strOf(e, "failure_model")
			st.continues = cloneNode(continues)
		case "appended", "revised":
			p, err := pinOf(e, "intermediate")
			if err != nil || st.retired[p.RecordID] {
				return false, reject(ReasonContinuation, "retired record in intermediate journal")
			}
			if present(e, "imported_from") {
				imp := e.Get("imported_from")
				ic, _ := strOf(imp, "capability")
				ip, err := pinFromNode(imp.Get("pin"), "imported_from.pin")
				if err != nil || ic != contCap || ip.Stream != contStream {
					return false, reject(ReasonContinuation, "intermediate imported_from mismatch")
				}
				importsSeq = append(importsSeq, seq)
			}
			st.records[p] = e.Get("record")
		case "tombstoned", "untombstoned":
			r := e.Get("request")
			rd, _ := strOf(e, "request_digest")
			gotDig, err := recordDigest(r)
			if err != nil || rd != gotDig {
				return false, reject(ReasonContinuation, "intermediate request_digest mismatch")
			}
			act, _ := strOf(r, "action")
			wantTyp := "untombstoned"
			if act == "tombstone" {
				wantTyp = "tombstoned"
			}
			if typ != wantTyp {
				return false, reject(ReasonContinuation, "intermediate operation_type mismatch")
			}
			reason, err := st.refusal(r, at)
			if err != nil || reason != "" {
				return false, reject(ReasonContinuation, "intermediate protection refused")
			}
			if err := st.apply(r, seq); err != nil {
				return false, reject(ReasonContinuation, "intermediate apply failed")
			}
			restrictedSeq = append(restrictedSeq, seq)
		case "retired":
			rid, _ := strOf(e, "record_id")
			if !st.hasRecordID(rid) {
				return false, reject(ReasonContinuation, "intermediate retired target not found")
			}
			if present(e, "idempotency_key") {
				r := retirementRequest(e)
				rd, _ := strOf(e, "request_digest")
				gotDig, err := recordDigest(r)
				if err != nil || rd != gotDig {
					return false, reject(ReasonContinuation, "intermediate retirement digest mismatch")
				}
			}
			st.retired[rid] = true
			restrictedSeq = append(restrictedSeq, seq)
		case "restrictions_reviewed":
			src := e.Get("source")
			sc, _ := strOf(src, "capability")
			ss, _ := strOf(src, "stream")
			sCutSeq, _ := intOf(src, "cut_seq")
			sCutDig, _ := strOf(src, "cut_digest")
			grp, _ := intOf(e, "group")
			gsz, _ := intOf(e, "group_size")
			dCutSeq, _ := intOf(e, "destination_cut_seq")
			amDig, err := appliedManifest(journal[:idx])
			if err != nil {
				return false, err
			}
			wantAppDig, _ := strOf(e, "destination_applied_state_digest")
			if sc != contCap || ss != contStream || grp != seq || gsz != 1 || dCutSeq != seq-1 || sCutSeq < contCutSeq || amDig != wantAppDig {
				return false, reject(ReasonContinuation, "intermediate review invalid")
			}
			if ancSeal != nil {
				ancCutSeq, _ := intOf(ancSeal, "cut_seq")
				ancCutDig, _ := strOf(ancSeal, "cut_digest")
				ancEffDig, _ := strOf(ancSeal, "effective_restriction_manifest_digest")
				revSrcDig, _ := strOf(e, "source_state_digest")
				if sCutSeq != ancCutSeq || sCutDig != ancCutDig || revSrcDig != ancEffDig {
					return false, reject(ReasonContinuation, "intermediate review does not match ancestor seal")
				}
			}
			latestRev = e
		}
	}
	if latestRev == nil {
		return false, nil
	}
	after, _ := intOf(latestRev, "seq")
	for _, sq := range importsSeq {
		if sq > after {
			return false, nil
		}
	}
	for _, sq := range restrictedSeq {
		if sq > after {
			return false, nil
		}
	}
	return true, nil
}

func sealEstablishesFinality(sealCtx *wire.Node, ancestorsFinal bool) bool {
	ca := sealCtx.Get("closer_authenticated")
	if ca == nil || ca.Kind != wire.Bool || !ca.Bool {
		return false
	}
	swf := sealCtx.Get("source_write_fenced")
	if swf == nil || swf.Kind != wire.Bool || !swf.Bool {
		return false
	}
	return ancestorsFinal
}

// evaluateSealContext returns finality ("true", "false", or "uncurrent_final"),
// unobservedAncestorHold, and the verified prefix.
func evaluateSealContext(sealCtx *wire.Node, sourceJournal []*wire.Node, windowMs int64, observedUpstream bool, checkTopSeal bool) (finality string, unobservedAncHold bool, prefix []*wire.Node, err error) {
	seal, err := validateSealCtxEnvelope(sealCtx, checkTopSeal)
	if err != nil {
		return "", false, nil, err
	}
	if mDoc := sealCtx.Get("manifest"); mDoc != nil {
		effDig, _ := strOf(seal, "effective_restriction_manifest_digest")
		if err := verifyManifestPreimage(mDoc, effDig); err != nil {
			return "", false, nil, err
		}
	}

	isCont := sourceJournal[0].Get("continues") != nil
	sealCap, _ := strOf(seal, "capability")
	sealStream, _ := strOf(seal, "stream")
	sealKey := streamKey{capability: sealCap, stream: sealStream}

	type ancJournalInfo struct {
		ac      *wire.Node
		seal    *wire.Node
		journal []*wire.Node
	}
	ctxByKey := map[streamKey]*wire.Node{}
	sealByKey := map[streamKey]*wire.Node{}
	ancJournalsByKey := map[streamKey]ancJournalInfo{}

	var ancCtxs []*wire.Node
	if acNode := sealCtx.Get("ancestor_seal_contexts"); acNode != nil {
		if acNode.Kind != wire.Array {
			return "", false, nil, reject(ReasonContinuation, "ancestor_seal_contexts is not an array")
		}
		ancCtxs = acNode.Elems
	}
	for _, ac := range ancCtxs {
		acSeal, err := validateSealCtxEnvelope(ac, true)
		if err != nil {
			return "", false, nil, err
		}
		acV, err := intOf(acSeal, "v")
		if err != nil || acV != 1 {
			return "", false, nil, reject(ReasonContinuation, "ancestor seal.v != 1")
		}
		acStore, _ := strOf(acSeal, "store")
		acExpStore, err := strOf(ac, "expected_store")
		if err != nil || acStore != acExpStore {
			return "", false, nil, reject(ReasonContinuation, "ancestor seal.store != expected_store")
		}
		if !nodesEqual(acSeal.Get("trust_profile"), ac.Get("accepted_trust_profile")) {
			return "", false, nil, reject(ReasonContinuation, "ancestor seal.trust_profile != accepted_trust_profile")
		}
		if tb := ac.Get("trailing_bytes"); tb != nil {
			if tb.Kind != wire.String || tb.Str != "" {
				return "", false, nil, reject(ReasonContinuation, "ancestor non-empty trailing_bytes")
			}
		}
		acAncSeals := acSeal.Get("ancestor_seals")
		if acAncSeals == nil || acAncSeals.Kind != wire.Array {
			return "", false, nil, reject(ReasonContinuation, "ancestor_seals is not an array")
		}
		acJCS := make([]string, len(acAncSeals.Elems))
		acJCSSet := map[string]bool{}
		for i, m := range acAncSeals.Elems {
			s, err := jcsString(m)
			if err != nil {
				return "", false, nil, err
			}
			acJCS[i] = s
			acJCSSet[s] = true
		}
		if !slices.IsSorted(acJCS) || len(acJCSSet) != len(acJCS) {
			return "", false, nil, reject(ReasonContinuation, "ancestor_seals not sorted or has duplicates")
		}
		acCap, _ := strOf(acSeal, "capability")
		acStream, _ := strOf(acSeal, "stream")
		acKey := streamKey{capability: acCap, stream: acStream}
		if acKey == sealKey || ctxByKey[acKey] != nil {
			return "", false, nil, reject(ReasonContinuation, "duplicate or self ancestor_seal_context")
		}
		subKeys := map[streamKey]bool{}
		for _, m := range acAncSeals.Elems {
			mc, _ := strOf(m, "capability")
			ms, _ := strOf(m, "stream")
			sk := streamKey{capability: mc, stream: ms}
			if sk == acKey || subKeys[sk] {
				return "", false, nil, reject(ReasonContinuation, "invalid sub-key in ancestor_seals")
			}
			subKeys[sk] = true
		}
		if mDoc := ac.Get("manifest"); mDoc != nil {
			effDig, _ := strOf(acSeal, "effective_restriction_manifest_digest")
			if err := verifyManifestPreimage(mDoc, effDig); err != nil {
				return "", false, nil, err
			}
		}
		if ac.Get("journal") != nil || ac.Get("raw_journal") != nil {
			jSpec := ac.Get("journal")
			if ac.Get("raw_journal") != nil {
				jSpec = ac.Get("raw_journal")
			}
			aj, _, err := parseSourceSpec(jSpec)
			if err != nil {
				return "", false, nil, err
			}
			ancJournalsByKey[acKey] = ancJournalInfo{ac: ac, seal: acSeal, journal: aj}
		}
		ctxByKey[acKey] = ac
		sealByKey[acKey] = acSeal
	}

	ancJournalUncurrent := map[streamKey]bool{}
	ancJournalUpstreams := map[streamKey]streamKey{}
	for acKey, info := range ancJournalsByKey {
		aj := info.journal
		acSeal := info.seal
		if aj[0].Get("continues") == nil {
			if _, err := observed(aj, windowMs, nil, nil, false); err != nil {
				return "", false, nil, err
			}
			if _, err := verifySealAgainstJournal(info.ac, acSeal, aj); err != nil {
				return "", false, nil, err
			}
			if len(acSeal.Get("ancestor_seals").Elems) != 0 {
				return "", false, nil, reject(ReasonContinuation, "non-continuation ancestor has ancestor_seals")
			}
		} else {
			ajPrefix, err := verifySealAgainstJournal(info.ac, acSeal, aj)
			if err != nil {
				return "", false, nil, err
			}
			_, _, ajRev, err := restrictionState(ajPrefix)
			if err != nil {
				return "", false, nil, err
			}
			var ajUpKey streamKey
			if ajRev != nil {
				src := ajRev.Get("source")
				c, _ := strOf(src, "capability")
				s, _ := strOf(src, "stream")
				ajUpKey = streamKey{capability: c, stream: s}
			} else {
				cont := ajPrefix[0].Get("continues")
				c, _ := strOf(cont, "capability")
				s, _ := strOf(cont, "stream")
				ajUpKey = streamKey{capability: c, stream: s}
			}
			ancJournalUpstreams[acKey] = ajUpKey
			upSeal := sealByKey[ajUpKey]
			ajCur, err := checkSealedIntermediateJournal(aj, upSeal, windowMs)
			if err != nil {
				return "", false, nil, err
			}
			hasUpInSeal := false
			for _, m := range acSeal.Get("ancestor_seals").Elems {
				mc, _ := strOf(m, "capability")
				ms, _ := strOf(m, "stream")
				if (streamKey{capability: mc, stream: ms}) == ajUpKey {
					hasUpInSeal = true
					break
				}
			}
			if !ajCur || !hasUpInSeal {
				ancJournalUncurrent[acKey] = true
			}
		}
	}

	if !isCont {
		if !observedUpstream {
			if _, err := observed(sourceJournal, windowMs, nil, nil, false); err != nil {
				return "", false, nil, err
			}
		}
		prefix, err := verifySealAgainstJournal(sealCtx, seal, sourceJournal)
		if err != nil {
			return "", false, nil, err
		}
		if len(seal.Get("ancestor_seals").Elems) != 0 || len(ancCtxs) != 0 {
			return "", false, nil, reject(ReasonContinuation, "non-continuation source seal has ancestors")
		}
		if sealEstablishesFinality(sealCtx, true) {
			return "true", false, prefix, nil
		}
		return "false", false, prefix, nil
	}

	prefix, err = verifySealAgainstJournal(sealCtx, seal, sourceJournal)
	if err != nil {
		return "", false, nil, err
	}
	topAncByKey := map[streamKey]*wire.Node{}
	for _, entry := range seal.Get("ancestor_seals").Elems {
		ec, _ := strOf(entry, "capability")
		es, _ := strOf(entry, "stream")
		key := streamKey{capability: ec, stream: es}
		if key == sealKey {
			return "", false, nil, reject(ReasonContinuation, "self key in ancestor_seals")
		}
		topAncByKey[key] = entry
		if acSeal := sealByKey[key]; acSeal != nil {
			acDig, err := recordDigest(acSeal)
			if err != nil {
				return "", false, nil, err
			}
			eSealDig, _ := strOf(entry, "seal_digest")
			eStore, _ := strOf(entry, "store")
			eCutSeq, _ := intOf(entry, "cut_seq")
			acStore, _ := strOf(acSeal, "store")
			acCutSeq, _ := intOf(acSeal, "cut_seq")
			if acDig != eSealDig || acStore != eStore || acCutSeq != eCutSeq {
				return "", false, nil, reject(ReasonContinuation, "ancestor_seal entry mismatch")
			}
		}
	}

	_, _, midRev, err := restrictionState(prefix)
	if err != nil {
		return "", false, nil, err
	}
	var neededKey streamKey
	if midRev != nil {
		src := midRev.Get("source")
		c, _ := strOf(src, "capability")
		s, _ := strOf(src, "stream")
		neededKey = streamKey{capability: c, stream: s}
	} else {
		cont := prefix[0].Get("continues")
		c, _ := strOf(cont, "capability")
		s, _ := strOf(cont, "stream")
		neededKey = streamKey{capability: c, stream: s}
	}
	matchingSeal := sealByKey[neededKey]
	midCurrent, err := checkSealedIntermediateJournal(sourceJournal, matchingSeal, windowMs)
	if err != nil {
		return "", false, nil, err
	}

	ancestorsFinal := true
	unobservedAncHold = false
	reachableKeys := map[streamKey]bool{}
	canonicalEdgeByKey := map[streamKey]edgeTuple{}

	bindEdge := func(key streamKey, edge edgeTuple) error {
		if prev, ok := canonicalEdgeByKey[key]; ok {
			if prev != edge {
				return reject(ReasonContinuation, "conflicting ancestor seal edge")
			}
		} else {
			canonicalEdgeByKey[key] = edge
		}
		return nil
	}

	var walkAncestor func(key streamKey, expectedEntry *wire.Node, visiting map[streamKey]bool) error
	walkAncestor = func(key streamKey, expectedEntry *wire.Node, visiting map[streamKey]bool) error {
		if visiting[key] {
			return reject(ReasonContinuation, "cycle in ancestor seals")
		}
		if expectedEntry != nil {
			st, _ := strOf(expectedEntry, "store")
			cp, _ := strOf(expectedEntry, "capability")
			sm, _ := strOf(expectedEntry, "stream")
			cs, _ := intOf(expectedEntry, "cut_seq")
			sd, _ := strOf(expectedEntry, "seal_digest")
			if err := bindEdge(key, edgeTuple{store: st, capability: cp, stream: sm, cutSeq: cs, sealDigest: sd}); err != nil {
				return err
			}
		}
		reachableKeys[key] = true
		if topE, ok := topAncByKey[key]; ok {
			st, _ := strOf(topE, "store")
			cp, _ := strOf(topE, "capability")
			sm, _ := strOf(topE, "stream")
			cs, _ := intOf(topE, "cut_seq")
			sd, _ := strOf(topE, "seal_digest")
			if err := bindEdge(key, edgeTuple{store: st, capability: cp, stream: sm, cutSeq: cs, sealDigest: sd}); err != nil {
				return err
			}
		} else {
			ancestorsFinal = false
			unobservedAncHold = true
		}
		ac := ctxByKey[key]
		if ac == nil {
			ancestorsFinal = false
			unobservedAncHold = true
			return nil
		}
		acSeal := sealByKey[key]
		acDig, err := recordDigest(acSeal)
		if err != nil {
			return err
		}
		st, _ := strOf(acSeal, "store")
		cp, _ := strOf(acSeal, "capability")
		sm, _ := strOf(acSeal, "stream")
		cs, _ := intOf(acSeal, "cut_seq")
		if err := bindEdge(key, edgeTuple{store: st, capability: cp, stream: sm, cutSeq: cs, sealDigest: acDig}); err != nil {
			return err
		}
		ca := ac.Get("closer_authenticated")
		swf := ac.Get("source_write_fenced")
		if ca == nil || ca.Kind != wire.Bool || !ca.Bool || swf == nil || swf.Kind != wire.Bool || !swf.Bool || ancJournalUncurrent[key] {
			ancestorsFinal = false
		}
		mDigest, _ := strOf(acSeal, "effective_restriction_manifest_digest")
		if mDoc := ac.Get("manifest"); mDoc != nil {
			if holds := mDoc.Get("holds"); holds != nil && len(holds.Elems) > 0 {
				unobservedAncHold = true
			}
		} else if mDigest != emptyManifestDigest {
			unobservedAncHold = true
		}
		nextVisiting := make(map[streamKey]bool, len(visiting)+1)
		for k, v := range visiting {
			nextVisiting[k] = v
		}
		nextVisiting[key] = true
		subKeysPresent := map[streamKey]bool{}
		for _, subE := range acSeal.Get("ancestor_seals").Elems {
			sc, _ := strOf(subE, "capability")
			ss, _ := strOf(subE, "stream")
			subK := streamKey{capability: sc, stream: ss}
			subKeysPresent[subK] = true
			if err := walkAncestor(subK, subE, nextVisiting); err != nil {
				return err
			}
		}
		if upK, ok := ancJournalUpstreams[key]; ok && !subKeysPresent[upK] {
			if err := walkAncestor(upK, nil, nextVisiting); err != nil {
				return err
			}
		}
		return nil
	}

	if err := walkAncestor(neededKey, nil, map[streamKey]bool{sealKey: true}); err != nil {
		return "", false, nil, err
	}
	for k := range topAncByKey {
		if !reachableKeys[k] {
			return "", false, nil, reject(ReasonContinuation, "unreachable entry in ancestor_seals")
		}
	}
	isFinal := sealEstablishesFinality(sealCtx, ancestorsFinal)
	if !midCurrent && isFinal {
		return "uncurrent_final", unobservedAncHold, prefix, nil
	}
	if isFinal {
		return "true", unobservedAncHold, prefix, nil
	}
	return "false", unobservedAncHold, prefix, nil
}

func (s *State) applyCaseSourceSeal(sealCtx *wire.Node) error {
	observedUp := s.upstream != nil || s.upstreamUnobserved
	finality, ancHold, prefix, err := evaluateSealContext(sealCtx, s.source, s.window, observedUp, true)
	if err != nil {
		return err
	}
	switch finality {
	case "uncurrent_final":
		s.source = cloneJournal(prefix)
		s.upstream = nil
		s.upstreamUnobserved = false
		s.upstreamUncurrentSealed = true
		s.sourceSealFinal = false
		s.sealedAncestorHold = ancHold
	case "true":
		s.upstream = nil
		s.upstreamUnobserved = false
		s.upstreamUncurrentSealed = false
		s.sourceSealFinal = true
		s.sealedAncestorHold = ancHold
	default:
		s.sourceSealFinal = false
		if lsu := sealCtx.Get("live_source_unobservable"); lsu != nil && lsu.Kind == wire.Bool && lsu.Bool {
			s.upstream = nil
			s.upstreamUnobserved = true
		}
	}
	return nil
}

// VerifySealStep evaluates a verify_seal step context, matching
// State.verify_seal_step in the reference interpreter.
func (s *State) VerifySealStep(stepCtx *wire.Node) (*wire.Node, error) {
	seal, err := validateSealCtxEnvelope(stepCtx, true)
	if err != nil {
		return nil, err
	}
	jSpec := stepCtx.Get("journal")
	if stepCtx.Get("raw_journal") != nil {
		jSpec = stepCtx.Get("raw_journal")
	}
	journal, _, err := parseSourceSpec(jSpec)
	if err != nil {
		return nil, err
	}
	obsUp := journal[0].Get("continues") != nil
	finality, _, _, err := evaluateSealContext(stepCtx, journal, s.window, obsUp, false)
	if err != nil {
		return nil, err
	}
	effDig, _ := strOf(seal, "effective_restriction_manifest_digest")
	if expM := stepCtx.Get("expected_manifest"); expM != nil {
		if expJCS := stepCtx.Get("expected_manifest_jcs"); expJCS != nil {
			if expJCS.Kind != wire.String {
				return nil, reject(ReasonContinuation, "expected_manifest_jcs is not a string")
			}
			gotJCS, err := jcsString(expM)
			if err != nil || gotJCS != expJCS.Str {
				return nil, reject(ReasonContinuation, "expected_manifest_jcs mismatch")
			}
		}
		gotDig, err := recordDigest(expM)
		if err != nil || gotDig != effDig {
			return nil, reject(ReasonContinuation, "expected_manifest digest mismatch")
		}
	}
	isFinal := finality == "true"
	return &wire.Node{
		Kind: wire.Object,
		Fields: []wire.Member{
			{Name: "outcome", Value: strNode("seal_verified")},
			{Name: "final", Value: &wire.Node{Kind: wire.Bool, Bool: isFinal}},
			{Name: "effective_restriction_manifest_digest", Value: strNode(effDig)},
		},
	}, nil
}

// Use evaluates an export/serve/reuse request against the stream's replayed
// journal and source chain, matching State.use in the reference interpreter.
func (s *State) Use(r *wire.Node) (*wire.Node, error) {
	if r == nil || r.Kind != wire.Object {
		return nil, reject(ReasonShape, "use: request is not an object")
	}
	if err := CheckJournal(s.journal); err != nil {
		return nil, err
	}
	refused := func(reason string) *wire.Node {
		return &wire.Node{
			Kind: wire.Object,
			Fields: []wire.Member{
				{Name: "outcome", Value: strNode("refused")},
				{Name: "reason", Value: strNode(reason)},
			},
		}
	}
	purpose, err := strOf(r, "purpose")
	if err != nil {
		return nil, err
	}
	if purpose != "serve" && purpose != "export" && purpose != "reuse" {
		return refused(ReasonInvalidPurpose), nil
	}
	rStream, err := strOf(r, "stream")
	if err != nil {
		return nil, err
	}
	protState, err := strOf(r, "protection_state")
	if err != nil {
		return nil, err
	}
	protCut, err := intOf(r, "protection_cut")
	if err != nil {
		return nil, err
	}
	curCut, err := intOf(r, "current_cut")
	if err != nil {
		return nil, err
	}
	revPend, err := s.reviewPending(r)
	if err != nil {
		return nil, err
	}
	if rStream != s.stream || protState != "verified" || protCut != int64(len(s.journal)) || curCut != int64(len(s.journal)) || revPend {
		return refused(ReasonProtectionUnavailable), nil
	}
	revCur, err := s.reviewCurrent()
	if err != nil {
		return nil, err
	}
	if !revCur {
		return refused(ReasonRestrictionsPending), nil
	}
	selNode := r.Get("selection")
	omNode := r.Get("omitted")
	if selNode == nil || selNode.Kind != wire.Array || omNode == nil || omNode.Kind != wire.Array {
		return nil, reject(ReasonShape, "use: selection or omitted is not an array")
	}
	selected := make(map[Pin]bool, len(selNode.Elems))
	var selectedPins []Pin
	for _, el := range selNode.Elems {
		p, err := pinFromNode(el, "use.selection")
		if err != nil {
			return nil, err
		}
		selected[p] = true
		selectedPins = append(selectedPins, p)
	}
	omitted := make(map[Pin]bool, len(omNode.Elems))
	for _, el := range omNode.Elems {
		p, err := pinFromNode(el, "use.omitted")
		if err != nil {
			return nil, err
		}
		omitted[p] = true
	}
	for p := range selected {
		if _, ok := s.records[p]; !ok {
			return refused(ReasonTargetNotFound), nil
		}
	}
	for p := range omitted {
		if _, ok := s.records[p]; !ok {
			return refused(ReasonTargetNotFound), nil
		}
	}
	for p := range selected {
		if omitted[p] {
			return refused(ReasonExportSelection), nil
		}
	}
	if len(selected)+len(omitted) != len(s.records) {
		return refused(ReasonExportSelection), nil
	}
	inhBlock := r.Get("inherited_block")
	if inhBlock == nil || inhBlock.Kind != wire.Bool {
		return nil, reject(ReasonShape, "use: inherited_block is not a boolean")
	}
	if inhBlock.Bool {
		return refused(ReasonProtectedUse), nil
	}
	for _, p := range selectedPins {
		b, err := s.Blocked(p)
		if err != nil {
			return nil, err
		}
		if b {
			return refused(ReasonProtectedUse), nil
		}
	}
	if purpose == "serve" {
		for _, p := range selectedPins {
			if s.retired[p.RecordID] {
				return refused(ReasonProtectedUse), nil
			}
		}
	}
	bodiesNode := r.Get("bodies")
	if bodiesNode == nil || bodiesNode.Kind != wire.Array {
		return nil, reject(ReasonShape, "use: bodies is not an array")
	}
	supplied := map[string]int64{}
	for _, b := range bodiesNode.Elems {
		if b == nil || b.Kind != wire.Object {
			return nil, reject(ReasonShape, "use: body item is not an object")
		}
		hexStr, err := strOf(b, "hex")
		if err != nil {
			return nil, err
		}
		dig, err := strOf(b, "digest")
		if err != nil {
			return nil, err
		}
		raw, err := hex.DecodeString(hexStr)
		if err != nil {
			return refused(ReasonBodyClosure), nil
		}
		sum := sha256.Sum256(raw)
		if "sha256:"+hex.EncodeToString(sum[:]) != dig {
			return refused(ReasonBodyClosure), nil
		}
		if _, dup := supplied[dig]; dup {
			return refused(ReasonBodyClosure), nil
		}
		supplied[dig] = int64(len(raw))
	}
	needed := map[string]int64{}
	for p := range selected {
		rec := s.records[p]
		if body := rec.Get("body"); body != nil && body.Kind == wire.Object {
			bDig, err := strOf(body, "digest")
			if err != nil {
				return nil, err
			}
			bLen, err := intOf(body, "length")
			if err != nil {
				return nil, err
			}
			if prev, ok := needed[bDig]; ok && prev != bLen {
				return refused(ReasonBodyClosure), nil
			}
			needed[bDig] = bLen
		}
	}
	for dig, sLen := range supplied {
		if nLen, ok := needed[dig]; !ok || nLen != sLen {
			return refused(ReasonBodyClosure), nil
		}
	}
	closure, err := strOf(r, "closure")
	if err != nil {
		return nil, err
	}
	if closure == "full_content" {
		if len(supplied) != len(needed) {
			return refused(ReasonBodyClosure), nil
		}
	}
	return &wire.Node{
		Kind: wire.Object,
		Fields: []wire.Member{
			{Name: "outcome", Value: strNode("allowed")},
			{Name: "closure", Value: strNode(closure)},
			{Name: "selection", Value: cloneNode(selNode)},
			{Name: "omitted", Value: cloneNode(omNode)},
			{Name: "protection_cut", Value: intNode(protCut)},
		},
	}, nil
}
