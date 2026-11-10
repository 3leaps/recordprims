package journal

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"slices"
	"strings"

	"github.com/3leaps/recordprims/internal/canon"
	"github.com/3leaps/recordprims/internal/shape"
	"github.com/3leaps/recordprims/internal/wire"
)

var digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// BodyKey identifies a content-addressed body by its digest and byte length.
type BodyKey struct {
	Digest string
	Length int64
}

type bodyState struct {
	bound int64
	phase string // "", "published", "started", "removed", "recovery_publishing", "recovery_integrity_loss"
}

func integral(n *wire.Node) (int64, bool) {
	if n == nil || n.Kind != wire.Number {
		return 0, false
	}
	if n.IsInt {
		return n.Int, n.Int >= 0 && n.Int <= wire.MaxSafeInteger
	}
	v, err := wire.SafeInt(n.Token)
	if err != nil || v < 0 || v > wire.MaxSafeInteger {
		return 0, false
	}
	return v, true
}

func bodyKeyOf(b *wire.Node, where string) (BodyKey, error) {
	if b == nil || b.Kind != wire.Object {
		return BodyKey{}, reject(ReasonShape, "%s: body is not an object", where)
	}
	dig, err := strOf(b, "digest")
	if err != nil {
		return BodyKey{}, err
	}
	length, ok := integral(b.Get("length"))
	if !ok {
		return BodyKey{}, reject(ReasonShape, "%s: body.length is not a nonnegative safe integer", where)
	}
	return BodyKey{Digest: dig, Length: length}, nil
}

func computeRecoveryRow(priorPhase string, priorBound, floor int64, obs string, damaged, referenced bool) string {
	_ = referenced
	switch priorPhase {
	case "":
		if !damaged {
			if obs == "absent" {
				return "unpublished"
			}
			return "publishing"
		}
		if floor == 0 && obs == "absent" {
			return "unpublished"
		}
		if floor > 0 && obs == "absent" {
			return "integrity_loss"
		}
		return "publishing"
	case "published":
		if obs == "present_valid" {
			if !damaged || floor == priorBound {
				return "available"
			}
			return "publishing"
		}
		return "integrity_loss"
	case "started":
		if !damaged {
			return "removal_pending"
		}
		if obs == "present_valid" {
			if floor == priorBound {
				return "available"
			}
			return "publishing"
		}
		if floor == priorBound {
			return "removal_pending"
		}
		return "integrity_loss"
	case "removed":
		if obs == "absent" {
			if !damaged || floor == priorBound {
				return "removed"
			}
			return "integrity_loss"
		}
		return "publishing"
	case "recovery_integrity_loss":
		return "integrity_loss"
	default:
		if obs == "present_valid" {
			return "publishing"
		}
		return "integrity_loss"
	}
}

func applyRecoveryTransition(priorPhase string, priorBound int64, openSet map[int64]bool, floor int64, obs, postState string) (bodyState, map[int64]bool) {
	newOpen := make(map[int64]bool, len(openSet))
	for k, v := range openSet {
		if v {
			newOpen[k] = true
		}
	}
	if priorPhase == "started" {
		if floor == priorBound && obs == "present_valid" {
			return bodyState{bound: floor, phase: "published"}, map[int64]bool{}
		}
		if floor == priorBound && obs == "absent" {
			return bodyState{bound: floor, phase: "started"}, newOpen
		}
		if obs == "present_valid" {
			return bodyState{bound: floor, phase: "recovery_publishing"}, map[int64]bool{}
		}
		return bodyState{bound: floor, phase: "recovery_integrity_loss"}, map[int64]bool{}
	}
	switch postState {
	case "unpublished":
		return bodyState{bound: 0, phase: ""}, map[int64]bool{}
	case "available":
		return bodyState{bound: floor, phase: "published"}, map[int64]bool{}
	case "removed":
		return bodyState{bound: floor, phase: "removed"}, map[int64]bool{}
	case "publishing":
		return bodyState{bound: floor, phase: "recovery_publishing"}, map[int64]bool{}
	default:
		return bodyState{bound: floor, phase: "recovery_integrity_loss"}, map[int64]bool{}
	}
}

func (s *State) trackBody(e *wire.Node, where string) error {
	key, err := bodyKeyOf(e.Get("body"), where)
	if err != nil {
		return err
	}
	n, err := intOf(e, "incarnation")
	if err != nil {
		return err
	}
	typ, err := strOf(e, "type")
	if err != nil {
		return err
	}
	cur := s.bodies[key]
	switch typ {
	case "body_published":
		if n <= cur.bound {
			return reject(ReasonBodyLifecycle, "%s: body_published incarnation %d <= %d", where, n, cur.bound)
		}
		s.bodies[key] = bodyState{bound: n, phase: "published"}
	case "body_removal_started":
		if n != cur.bound || cur.phase != "published" {
			return reject(ReasonBodyLifecycle, "%s: body_removal_started requires published incarnation %d", where, cur.bound)
		}
		s.bodies[key] = bodyState{bound: n, phase: "started"}
		if s.openStarts[key] == nil {
			s.openStarts[key] = map[int64]bool{}
		}
		s.openStarts[key][n] = true
	default:
		if n != cur.bound || cur.phase != "started" {
			return reject(ReasonBodyLifecycle, "%s: body_removed requires started incarnation %d", where, cur.bound)
		}
		s.bodies[key] = bodyState{bound: n, phase: "removed"}
		if s.openStarts[key] != nil {
			delete(s.openStarts[key], n)
		}
	}
	return nil
}

// BodyState evaluates a body_state request step against the designated
// body-removal stream's replayed journal and returns the reader state.
func (s *State) BodyState(step *wire.Node) (*wire.Node, error) {
	if step == nil || step.Kind != wire.Object || len(step.Fields) != 2 {
		return nil, reject(ReasonBodyObservation, "body_state: expected object with body and present")
	}
	pres := step.Get("present")
	body := step.Get("body")
	if pres == nil || pres.Kind != wire.Bool || body == nil || body.Kind != wire.Object || len(body.Fields) != 2 {
		return nil, reject(ReasonBodyObservation, "body_state: invalid present or body")
	}
	dig := body.Get("digest")
	lengthNode := body.Get("length")
	if dig == nil || dig.Kind != wire.String || !digestPattern.MatchString(dig.Str) {
		return nil, reject(ReasonBodyObservation, "body_state: invalid body.digest")
	}
	length, ok := integral(lengthNode)
	if !ok {
		return nil, reject(ReasonBodyObservation, "body_state: invalid body.length")
	}
	if err := CheckJournal(s.journal); err != nil {
		return nil, err
	}
	if !s.hasRemoval || s.removalStream != s.stream {
		return nil, reject(ReasonBodyRemovalStream, "body_state: stream %q is not the designated body-removal stream", s.stream)
	}
	key := BodyKey{Digest: dig.Str, Length: length}
	bs := s.bodies[key]
	obs := "absent"
	if pres.Bool {
		obs = "present_valid"
	}
	st := computeRecoveryRow(bs.phase, bs.bound, bs.bound, obs, false, s.referencedBodies[key])
	return &wire.Node{
		Kind: wire.Object,
		Fields: []wire.Member{
			{Name: "outcome", Value: strNode("body_state")},
			{Name: "state", Value: strNode(st)},
		},
	}, nil
}

// BodyLifecycle returns the durable-record/v1 reader state, current
// incarnation high-water bound, and internal lifecycle phase for key given a
// verified physical observation (present == true for intact bytes, false for
// absent bytes).
func (s *State) BodyLifecycle(key BodyKey, present bool) (readerState string, bound int64, phase string) {
	bs := s.bodies[key]
	obs := "absent"
	if present {
		obs = "present_valid"
	}
	st := computeRecoveryRow(bs.phase, bs.bound, bs.bound, obs, false, s.referencedBodies[key])
	return st, bs.bound, bs.phase
}

// BodyHeld reports whether any active hold on s covers key (either directly via
// a body-scoped hold or via a record/revision hold on a revision referencing key).
func (s *State) BodyHeld(key BodyKey) bool {
	for hSeq := range s.active {
		h := s.holds[hSeq]
		if h == nil || h.Kind != wire.Object {
			continue
		}
		if sc, _ := strOf(h, "scope"); sc == "body" && bodyMatchesTarget(h.Get("body"), key) {
			return true
		}
	}
	for pk, rec := range s.records {
		if bodyMatchesTarget(rec.Get("body"), key) {
			if blk, err := s.Blocked(pk); err != nil || blk {
				return true
			}
		}
	}
	return false
}

// BodyReferenced reports whether any accepted revision on s references key
// (whether or not the record was subsequently retired).
func (s *State) BodyReferenced(key BodyKey) bool {
	for _, rec := range s.records {
		if bodyMatchesTarget(rec.Get("body"), key) {
			return true
		}
	}
	return false
}

// ReviewPending reports whether s is a continuation stream whose restriction
// review is currently pending.
func (s *State) ReviewPending() bool {
	p, err := s.reviewPending(&wire.Node{Kind: wire.Object})
	return err != nil || p
}

// Stream returns the stream identifier established by stream_opened.
func (s *State) Stream() string {
	return s.stream
}

// IsRetired reports whether recordID has been retired on s.
func (s *State) IsRetired(recordID string) bool {
	return s.retired[recordID]
}

// ObserveSharingStream replays an observed sharing stream to record its
// referenced bodies, matching the reference interpreter's sharing_streams
// setup.
func (s *State) ObserveSharingStream(entries []*wire.Node) error {
	if err := CheckJournal(entries); err != nil {
		return err
	}
	sh, err := New(intNode(s.window))
	if err != nil {
		return err
	}
	for _, se := range entries {
		if err := sh.Ingest(se); err != nil {
			return err
		}
	}
	for _, rec := range sh.records {
		if b := rec.Get("body"); b != nil && b.Kind == wire.Object {
			k, err := bodyKeyOf(b, "sharing_stream.body")
			if err != nil {
				return err
			}
			s.referencedBodies[k] = true
		}
	}
	return nil
}

// RecoveryInput carries the store-level inputs for designated-stream recovery
// chain validation and replay.
type RecoveryInput struct {
	FirstEntry         *wire.Node
	QuarantinedStreams *wire.Node
	CandidateStreams   *wire.Node
	ExpectedStore      *wire.Node
	RecoveryContexts   *wire.Node
}

func firstLine(raw string) (string, bool) {
	idx := strings.IndexByte(raw, '\n')
	if idx < 0 {
		return "", false
	}
	ln := raw[:idx+1]
	if strings.ContainsRune(ln, '\r') {
		return "", false
	}
	return ln, true
}

func cloneBodies(src map[BodyKey]bodyState) map[BodyKey]bodyState {
	out := make(map[BodyKey]bodyState, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out
}

func cloneOpenStarts(src map[BodyKey]map[int64]bool) map[BodyKey]map[int64]bool {
	out := make(map[BodyKey]map[int64]bool, len(src))
	for k, m := range src {
		cp := make(map[int64]bool, len(m))
		for inc, v := range m {
			if v {
				cp[inc] = true
			}
		}
		out[k] = cp
	}
	return out
}

func sliceDigest(entries []*wire.Node) (string, error) {
	return recordDigest(&wire.Node{Kind: wire.Array, Elems: entries})
}

func jcsString(n *wire.Node) (string, error) {
	b, err := canon.Canonicalize(wire.Encode(n))
	if err != nil {
		return "", reject(wire.ReasonIngest, "JCS canonicalization failed: %v", err)
	}
	return string(b), nil
}

func scanQuarantinedDesignatedImage(rawImg *wire.Node, expectedStream string, initBodies map[BodyKey]bodyState, initOpen map[BodyKey]map[int64]bool, windowMs int64) (longestPrefix []*wire.Node, isDamaged bool, imgDigest string, longestBodies map[BodyKey]bodyState, longestOpen map[BodyKey]map[int64]bool, err error) {
	if rawImg == nil || rawImg.Kind != wire.String {
		return nil, false, "", nil, nil, reject(ReasonRecoveryIncomplete, "quarantined image is not a string")
	}
	imgBytes := []byte(rawImg.Str)
	sum := sha256.Sum256(imgBytes)
	imgDigest = "sha256:" + hex.EncodeToString(sum[:])

	hopSt, err := New(intNode(windowMs))
	if err != nil {
		return nil, false, "", nil, nil, err
	}
	hopSt.SetRemovalStream(expectedStream)
	hopSt.incarnationAllocation = "prewrite_durable_ledger"
	hopSt.bodies = cloneBodies(initBodies)
	hopSt.openStarts = cloneOpenStarts(initOpen)

	offset := 0
	longestOffset := 0
	longestBodies = cloneBodies(initBodies)
	longestOpen = cloneOpenStarts(initOpen)

	rest := rawImg.Str
	for len(rest) > 0 {
		idx := strings.IndexByte(rest, '\n')
		if idx < 0 {
			break
		}
		ln := rest[:idx+1]
		rest = rest[idx+1:]
		if strings.ContainsRune(ln, '\r') {
			break
		}
		e, err := wire.Ingest([]byte(ln), wire.KindJournalEntry)
		if err != nil {
			break
		}
		if err := shape.Validate("v1/journal-entry.schema.json", e); err != nil {
			break
		}
		if err := hopSt.Ingest(e); err != nil {
			break
		}
		offset += len(ln)
		seq, _ := intOf(e, "seq")
		grp, _ := intOf(e, "group")
		gsz, _ := intOf(e, "group_size")
		if seq == grp+gsz-1 {
			longestPrefix = append([]*wire.Node(nil), hopSt.journal...)
			longestOffset = offset
			longestBodies = cloneBodies(hopSt.bodies)
			longestOpen = cloneOpenStarts(hopSt.openStarts)
		}
	}
	isDamaged = longestOffset < len(imgBytes)
	return longestPrefix, isDamaged, imgDigest, longestBodies, longestOpen, nil
}

// ValidateRecovery validates and replays the designated body-removal stream's
// recovery chain (rules R1–R19), matching _validate_and_replay_recovery_chain
// in the reference interpreter.
func (s *State) ValidateRecovery(in RecoveryInput) error {
	quarantinedList := s.quarantinedDesignated
	quarantinedMap := in.QuarantinedStreams
	candidates := in.CandidateStreams
	firstEntry := in.FirstEntry
	hasRecoveryHeader := firstEntry != nil && firstEntry.Kind == wire.Object && firstEntry.Get("recovery") != nil
	if hasRecoveryHeader {
		feStream, _ := strOf(firstEntry, "stream")
		if !s.hasRemoval || feStream != s.removalStream {
			return reject(ReasonBodyRemovalStream, "recovery header outside designated body-removal stream")
		}
	}
	if hasRecoveryHeader || len(quarantinedList) > 0 {
		if s.incarnationAllocation != "prewrite_durable_ledger" {
			return reject(ReasonBodyRemovalStream, "recovery requires prewrite_durable_ledger incarnation_allocation")
		}
	}

	seenPrev := map[string]map[string]bool{}
	var openEntries []*wire.Node
	if hasRecoveryHeader {
		openEntries = append(openEntries, firstEntry)
	}
	if candidates != nil && candidates.Kind == wire.Array {
		for _, cand := range candidates.Elems {
			if cand != nil && cand.Kind == wire.Object && cand.Get("recovery") != nil {
				openEntries = append(openEntries, cand)
			}
		}
	}
	if quarantinedMap != nil && quarantinedMap.Kind == wire.Object {
		for _, m := range quarantinedMap.Fields {
			if m.Value != nil && m.Value.Kind == wire.String {
				if ln, ok := firstLine(m.Value.Str); ok {
					if qe, err := wire.Ingest([]byte(ln), wire.KindJournalEntry); err == nil {
						if qe.Kind == wire.Object && qe.Get("recovery") != nil {
							openEntries = append(openEntries, qe)
						}
					}
				}
			}
		}
	}
	for _, oe := range openEntries {
		rec := oe.Get("recovery")
		if rec == nil || rec.Kind != wire.Object {
			return reject(ReasonShape, "recovery is not an object")
		}
		prevS, err := strOf(rec, "previous_stream")
		if err != nil {
			return err
		}
		oeStream, err := strOf(oe, "stream")
		if err != nil {
			return err
		}
		if seenPrev[prevS] == nil {
			seenPrev[prevS] = map[string]bool{}
		}
		seenPrev[prevS][oeStream] = true
	}
	for _, claimants := range seenPrev {
		if len(claimants) > 1 {
			return reject(ReasonBodyRemovalStream, "recovery fork detected")
		}
	}
	if s.hasRemoval && len(seenPrev[s.removalStream]) > 0 {
		return reject(ReasonBodyRemovalStream, "stale profile: designated stream has a recovery successor")
	}

	type recoveryHop struct {
		succOpen *wire.Node
		prevName string
		rawImg   *wire.Node
	}
	var expectedChain []string
	var hops []recoveryHop
	feStream := ""
	if firstEntry != nil && firstEntry.Kind == wire.Object {
		feStream, _ = strOf(firstEntry, "stream")
	}
	if s.hasRemoval && feStream == s.removalStream {
		visited := map[string]bool{feStream: true}
		curOpen := firstEntry
		for curOpen != nil && curOpen.Kind == wire.Object && curOpen.Get("recovery") != nil {
			rec := curOpen.Get("recovery")
			if rec == nil || rec.Kind != wire.Object {
				return reject(ReasonShape, "recovery is not an object")
			}
			prevName, err := strOf(rec, "previous_stream")
			if err != nil {
				return err
			}
			if visited[prevName] {
				return reject(ReasonBodyRemovalStream, "cycle in recovery chain")
			}
			visited[prevName] = true
			expectedChain = append(expectedChain, prevName)
			if quarantinedMap == nil || quarantinedMap.Kind != wire.Object || quarantinedMap.Get(prevName) == nil {
				return reject(ReasonBodyRemovalStream, "missing quarantined stream %q", prevName)
			}
			rawImg := quarantinedMap.Get(prevName)
			hops = append(hops, recoveryHop{succOpen: curOpen, prevName: prevName, rawImg: rawImg})
			var nextOpen *wire.Node
			if rawImg != nil && rawImg.Kind == wire.String {
				if ln, ok := firstLine(rawImg.Str); ok {
					if parsed0, err := wire.Ingest([]byte(ln), wire.KindJournalEntry); err == nil && parsed0.Kind == wire.Object {
						pTyp, _ := strOf(parsed0, "type")
						pStream, _ := strOf(parsed0, "stream")
						if pTyp == "stream_opened" && pStream == prevName {
							nextOpen = parsed0
						}
					}
				}
			}
			curOpen = nextOpen
		}
		if !slices.Equal(quarantinedList, expectedChain) {
			return reject(ReasonBodyRemovalStream, "quarantined_designated_streams does not match recovery chain")
		}
	}
	s.expectedQuarantined = append([]string(nil), expectedChain...)

	if len(hops) == 0 {
		return nil
	}

	bodies := map[BodyKey]bodyState{}
	openStarts := map[BodyKey]map[int64]bool{}

	for i := len(hops) - 1; i >= 0; i-- {
		hop := hops[i]
		rec := hop.succOpen.Get("recovery")
		if in.ExpectedStore != nil && in.ExpectedStore.Kind == wire.String {
			rStore, err := strOf(rec, "store")
			if err != nil {
				return err
			}
			if rStore != in.ExpectedStore.Str {
				return reject(ReasonBodyRemovalStream, "recovery store mismatch")
			}
		}
		longestPrefix, isDamaged, imgDigest, longestBodies, longestOpen, err := scanQuarantinedDesignatedImage(
			hop.rawImg, hop.prevName, bodies, openStarts, s.window,
		)
		if err != nil {
			return err
		}

		succStream, _ := strOf(hop.succOpen, "stream")
		var hctx *wire.Node
		if in.RecoveryContexts != nil && in.RecoveryContexts.Kind == wire.Object {
			hctx = in.RecoveryContexts.Get(succStream)
		}
		ledgerMap := map[BodyKey]int64{}
		liveObsMap := map[BodyKey]string{}
		if hctx != nil && hctx.Kind == wire.Object {
			if lfs := hctx.Get("ledger_floors"); lfs != nil && lfs.Kind == wire.Array {
				for _, b := range lfs.Elems {
					if b != nil && b.Kind == wire.Object {
						bk, err := bodyKeyOf(b.Get("body"), "ledger_floors.body")
						if err != nil {
							return err
						}
						fl, ok := integral(b.Get("floor"))
						if !ok {
							return reject(ReasonShape, "ledger_floors.floor is not a nonnegative safe integer")
						}
						ledgerMap[bk] = fl
					}
				}
			}
			if laos := hctx.Get("live_activation_observations"); laos != nil && laos.Kind == wire.Array {
				for _, b := range laos.Elems {
					if b != nil && b.Kind == wire.Object {
						bk, err := bodyKeyOf(b.Get("body"), "live_activation_observations.body")
						if err != nil {
							return err
						}
						obs, err := strOf(b, "observation")
						if err != nil {
							return err
						}
						liveObsMap[bk] = obs
					}
				}
			}
		}

		itemsNode := rec.Get("reconciled_bodies")
		if itemsNode == nil || itemsNode.Kind != wire.Array {
			return reject(ReasonShape, "recovery.reconciled_bodies is not an array")
		}
		items := itemsNode.Elems
		jcsBodies := make([]string, len(items))
		itemKeys := make([]BodyKey, len(items))
		itemKeySet := make(map[BodyKey]bool, len(items))
		for idx, it := range items {
			if it == nil || it.Kind != wire.Object {
				return reject(ReasonShape, "reconciled_body is not an object")
			}
			jb, err := jcsString(it.Get("body"))
			if err != nil {
				return err
			}
			jcsBodies[idx] = jb
			bk, err := bodyKeyOf(it.Get("body"), "reconciled_body.body")
			if err != nil {
				return err
			}
			itemKeys[idx] = bk
			itemKeySet[bk] = true
		}

		// R1
		if len(longestPrefix) < 1 || !isDamaged {
			return reject(ReasonRecoveryIncomplete, "recovery R1: predecessor image is empty or not damaged")
		}
		qImgDig, err := strOf(rec, "quarantined_image_digest")
		if err != nil {
			return err
		}
		if qImgDig != imgDigest {
			return reject(ReasonRecoveryIncomplete, "recovery R1: quarantined_image_digest mismatch")
		}
		cutSeq, err := intOf(rec, "cut_seq")
		if err != nil {
			return err
		}
		if cutSeq != int64(len(longestPrefix)) {
			return reject(ReasonRecoveryIncomplete, "recovery R1: cut_seq %d != longest prefix %d", cutSeq, len(longestPrefix))
		}
		cutDig, err := strOf(rec, "cut_digest")
		if err != nil {
			return err
		}
		gotCutDig, err := sliceDigest(longestPrefix[:cutSeq])
		if err != nil {
			return err
		}
		if cutDig != gotCutDig {
			return reject(ReasonRecoveryIncomplete, "recovery R1: cut_digest mismatch")
		}
		if !slices.IsSorted(jcsBodies) || len(itemKeySet) != len(itemKeys) {
			return reject(ReasonRecoveryIncomplete, "recovery R1: reconciled_bodies not JCS-sorted or contains duplicates")
		}
		for k := range longestBodies {
			if !itemKeySet[k] {
				return reject(ReasonRecoveryIncomplete, "recovery R1: missing body from predecessor prefix")
			}
		}
		for k := range ledgerMap {
			if !itemKeySet[k] {
				return reject(ReasonRecoveryIncomplete, "recovery R1: missing body from ledger_floors")
			}
		}
		for idx, it := range items {
			k := itemKeys[idx]
			prior := longestBodies[k]
			fl, err := intOf(it, "floor")
			if err != nil {
				return err
			}
			if fl < prior.bound {
				return reject(ReasonRecoveryIncomplete, "recovery R1: floor %d < prior bound %d", fl, prior.bound)
			}
			if lf, ok := ledgerMap[k]; ok && fl != lf {
				return reject(ReasonRecoveryIncomplete, "recovery R1: floor %d != ledger floor %d", fl, lf)
			}
			actObs, err := strOf(it, "activation_observation")
			if err != nil {
				return err
			}
			postState, err := strOf(it, "post_recovery_state")
			if err != nil {
				return err
			}
			expectedState := computeRecoveryRow(prior.phase, prior.bound, fl, actObs, true, s.referencedBodies[k])
			if postState != expectedState {
				return reject(ReasonRecoveryIncomplete, "recovery R1: post_recovery_state %q != %q", postState, expectedState)
			}
		}

		// R2
		for _, it := range items {
			fl, _ := intOf(it, "floor")
			if fl >= wire.MaxSafeInteger {
				return reject(wire.ReasonNumericDomain, "recovery R2: floor %d reaches MaxSafeInteger", fl)
			}
		}

		// R3
		for idx, it := range items {
			k := itemKeys[idx]
			actObs, _ := strOf(it, "activation_observation")
			liveObs := actObs
			if lo, ok := liveObsMap[k]; ok {
				liveObs = lo
			}
			if liveObs == "present_corrupt" {
				return reject(ReasonBodyObservation, "recovery R3: present_corrupt observation")
			}
		}

		// R4–R19
		for idx, it := range items {
			k := itemKeys[idx]
			actObs, _ := strOf(it, "activation_observation")
			liveObs := actObs
			if lo, ok := liveObsMap[k]; ok {
				liveObs = lo
			}
			if liveObs != actObs {
				return reject(ReasonRecoveryIncomplete, "recovery R4-R19: live observation %q != %q", liveObs, actObs)
			}
		}

		nextBodies := cloneBodies(longestBodies)
		nextOpen := cloneOpenStarts(longestOpen)
		for idx, it := range items {
			k := itemKeys[idx]
			prior := longestBodies[k]
			fl, _ := intOf(it, "floor")
			actObs, _ := strOf(it, "activation_observation")
			postState, _ := strOf(it, "post_recovery_state")
			newBS, newOS := applyRecoveryTransition(prior.phase, prior.bound, longestOpen[k], fl, actObs, postState)
			nextBodies[k] = newBS
			nextOpen[k] = newOS
		}
		bodies = nextBodies
		openStarts = nextOpen
	}

	s.bodies = bodies
	s.openStarts = openStarts
	return nil
}
