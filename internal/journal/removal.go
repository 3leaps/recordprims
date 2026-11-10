package journal

import (
	"slices"
	"time"

	"github.com/3leaps/recordprims/internal/shape"
	"github.com/3leaps/recordprims/internal/wire"
)

var requiredRemovalStepKeys = []string{
	"store",
	"store_streams",
	"observed_streams",
	"body",
	"incarnation",
	"mode",
	"reason",
	"decision_time",
	"expected_policy",
}

var allowedRemovalStepKeys = map[string]bool{
	"store":                          true,
	"store_streams":                  true,
	"observed_streams":               true,
	"body":                           true,
	"incarnation":                    true,
	"mode":                           true,
	"reason":                         true,
	"decision_time":                  true,
	"expected_policy":                true,
	"authorization":                  true,
	"quarantined_designated_streams": true,
}

var requiredAuthKeys = []string{
	"ref",
	"digest",
	"store",
	"body",
	"incarnation",
	"policy",
	"action",
	"scope_verified",
	"not_before",
	"expires_at",
}

var allowedObsKeys = map[string]bool{
	"stream":             true,
	"damaged":            true,
	"journal":            true,
	"raw_journal":        true,
	"source_journal":     true,
	"raw_source_journal": true,
	"ancestors":          true,
	"raw_ancestors":      true,
	"inherited_block":    true,
	"source_seal":        true,
}

func hasExactKeys(n *wire.Node, want []string) bool {
	if n == nil || n.Kind != wire.Object || len(n.Fields) != len(want) {
		return false
	}
	for _, k := range want {
		if n.Get(k) == nil {
			return false
		}
	}
	return true
}

func nonEmptyStr(n *wire.Node, key string) (string, bool) {
	if n == nil || n.Kind != wire.Object {
		return "", false
	}
	v := n.Get(key)
	if v == nil || v.Kind != wire.String || v.Str == "" {
		return "", false
	}
	return v.Str, true
}

func validBodyObj(b *wire.Node) (BodyKey, bool) {
	if !hasExactKeys(b, []string{"digest", "length"}) {
		return BodyKey{}, false
	}
	dig, ok := nonEmptyStr(b, "digest")
	if !ok || !digestPattern.MatchString(dig) {
		return BodyKey{}, false
	}
	length, ok := integral(b.Get("length"))
	if !ok {
		return BodyKey{}, false
	}
	return BodyKey{Digest: dig, Length: length}, true
}

func bodyMatchesTarget(recBody *wire.Node, target BodyKey) bool {
	if recBody == nil || recBody.Kind != wire.Object {
		return false
	}
	dig := recBody.Get("digest")
	if dig == nil || dig.Kind != wire.String || dig.Str != target.Digest {
		return false
	}
	length, ok := integral(recBody.Get("length"))
	return ok && length == target.Length
}

func journalsEqual(a, b []*wire.Node) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		sa, err1 := jcsString(a[i])
		sb, err2 := jcsString(b[i])
		if err1 != nil || err2 != nil || sa != sb {
			return false
		}
	}
	return true
}

func (s *State) removableIncarnationOK(body BodyKey, incarnation int64, mode string) bool {
	bs := s.bodies[body]
	switch mode {
	case "start":
		return bs.phase == "published" && incarnation == bs.bound
	case "retry_unlink":
		return s.openStarts[body][incarnation] && bs.phase == "started" && incarnation == bs.bound
	default:
		return false
	}
}

func validRemovalAuthorization(
	auth, step *wire.Node,
	targetBody BodyKey,
	decisionTs time.Time,
	hasNotBefore bool, notBeforeTs time.Time,
	hasExpiresAt bool, expiresAtTs time.Time,
) bool {
	if !hasExactKeys(auth, requiredAuthKeys) {
		return false
	}
	if _, ok := nonEmptyStr(auth, "ref"); !ok {
		return false
	}
	authDig, ok := nonEmptyStr(auth, "digest")
	if !ok || !digestPattern.MatchString(authDig) {
		return false
	}
	authPolicy := auth.Get("policy")
	if !hasExactKeys(authPolicy, []string{"id", "version"}) {
		return false
	}
	polID, ok1 := nonEmptyStr(authPolicy, "id")
	polVer, ok2 := nonEmptyStr(authPolicy, "version")
	if !ok1 || !ok2 {
		return false
	}
	expPol := step.Get("expected_policy")
	expID, _ := nonEmptyStr(expPol, "id")
	expVer, _ := nonEmptyStr(expPol, "version")
	if polID != expID || polVer != expVer {
		return false
	}
	authStore, ok := nonEmptyStr(auth, "store")
	stepStore, _ := nonEmptyStr(step, "store")
	if !ok || authStore != stepStore {
		return false
	}
	abKey, ok := validBodyObj(auth.Get("body"))
	if !ok || abKey != targetBody {
		return false
	}
	authInc, ok := integral(auth.Get("incarnation"))
	stepInc, okStep := integral(step.Get("incarnation"))
	if !ok || !okStep || authInc < 1 || authInc != stepInc {
		return false
	}
	if act, ok := nonEmptyStr(auth, "action"); !ok || act != "remove_body" {
		return false
	}
	sv := auth.Get("scope_verified")
	if sv == nil || sv.Kind != wire.Bool || !sv.Bool {
		return false
	}
	if !hasNotBefore || !hasExpiresAt {
		return false
	}
	if decisionTs.Before(notBeforeTs) || decisionTs.After(expiresAtTs) {
		return false
	}
	return true
}

func replayObservedSharingStream(obs *wire.Node, windowMs int64) (st *State, damaged bool, unobs bool, chainJournals [][]*wire.Node) {
	if obs == nil || obs.Kind != wire.Object || obs.Get("stream") == nil {
		return nil, true, false, nil
	}
	obsStreamNode := obs.Get("stream")
	if obsStreamNode.Kind != wire.String {
		return nil, true, false, nil
	}
	obsStream := obsStreamNode.Str
	for _, m := range obs.Fields {
		if !allowedObsKeys[m.Name] {
			return nil, true, false, nil
		}
	}
	if dmg := obs.Get("damaged"); dmg != nil {
		if dmg.Kind != wire.Bool || dmg.Bool {
			return nil, true, false, nil
		}
	}
	if (obs.Get("journal") != nil && obs.Get("raw_journal") != nil) ||
		(obs.Get("source_journal") != nil && obs.Get("raw_source_journal") != nil) ||
		(obs.Get("ancestors") != nil && obs.Get("raw_ancestors") != nil) {
		return nil, true, false, nil
	}
	hasAnc := obs.Get("ancestors") != nil || obs.Get("raw_ancestors") != nil
	hasSrc := obs.Get("source_journal") != nil || obs.Get("raw_source_journal") != nil
	if hasAnc && !hasSrc {
		return nil, false, true, nil
	}
	var journal []*wire.Node
	if rj := obs.Get("raw_journal"); rj != nil {
		if rj.Kind != wire.Array || len(rj.Elems) == 0 {
			return nil, true, false, nil
		}
		journal = make([]*wire.Node, len(rj.Elems))
		for i, raw := range rj.Elems {
			if raw == nil || raw.Kind != wire.String {
				return nil, true, false, nil
			}
			n, err := wire.Ingest([]byte(raw.Str), wire.KindJournalEntry)
			if err != nil {
				return nil, true, false, nil
			}
			journal[i] = n
		}
	} else if j := obs.Get("journal"); j != nil && j.Kind == wire.Array && len(j.Elems) > 0 {
		journal = j.Elems
	} else {
		return nil, true, false, nil
	}

	for _, entry := range journal {
		if err := shape.Validate("v1/journal-entry.schema.json", entry); err != nil {
			return nil, true, false, nil
		}
	}

	st, err := New(intNode(windowMs))
	if err != nil {
		return nil, true, false, nil
	}
	if hasSrc {
		firstSpec := obs.Get("source_journal")
		if obs.Get("raw_source_journal") != nil {
			firstSpec = obs.Get("raw_source_journal")
		}
		ancSpecs := obs.Get("ancestors")
		if obs.Get("raw_ancestors") != nil {
			ancSpecs = obs.Get("raw_ancestors")
		}
		src, up, upUnobs, cj, err := chainWithJournals(firstSpec, ancSpecs, windowMs)
		if err != nil {
			return nil, false, true, nil
		}
		st.source = src
		st.upstream = up
		st.upstreamUnobserved = upUnobs
		chainJournals = cj
		if ss := obs.Get("source_seal"); ss != nil {
			if len(chainJournals) == 0 && len(st.source) > 0 {
				chainJournals = [][]*wire.Node{cloneJournal(st.source)}
			}
			if err := st.applyCaseSourceSeal(ss); err != nil {
				return nil, false, true, nil
			}
		}
		if st.upstreamUnobserved {
			return nil, false, true, nil
		}
	} else if obs.Get("source_seal") != nil {
		return nil, false, true, nil
	}

	replayErr := CheckJournal(journal)
	if replayErr == nil {
		for _, entry := range journal {
			if err := st.Ingest(entry); err != nil {
				replayErr = err
				break
			}
		}
	}
	if replayErr != nil {
		if len(journal) > 0 && journal[0] != nil && journal[0].Kind == wire.Object && journal[0].Get("continues") != nil && len(st.source) == 0 {
			return nil, false, true, nil
		}
		return nil, true, false, nil
	}

	if st.stream != obsStream {
		return nil, true, false, nil
	}
	if st.continues != nil {
		ib := obs.Get("inherited_block")
		if ib == nil || ib.Kind != wire.Bool {
			return nil, false, true, nil
		}
	} else {
		if obs.Get("inherited_block") != nil ||
			obs.Get("source_journal") != nil ||
			obs.Get("raw_source_journal") != nil ||
			obs.Get("ancestors") != nil ||
			obs.Get("raw_ancestors") != nil ||
			obs.Get("source_seal") != nil {
			return nil, false, true, nil
		}
	}
	return st, false, false, chainJournals
}

// RemovalEligibility evaluates a body removal eligibility request against the
// designated removal stream and observed sharing streams, matching
// State.removal_eligibility in the reference interpreter.
func (s *State) RemovalEligibility(step *wire.Node) (*wire.Node, error) {
	if step != nil {
		var err error
		step, err = wire.Ingest(wire.Encode(step), wire.KindRemovalEligibility)
		if err != nil {
			return nil, err
		}
	}
	if err := CheckJournal(s.journal); err != nil {
		return nil, err
	}
	var decisionStr string
	if step != nil && step.Kind == wire.Object {
		if dt := step.Get("decision_time"); dt != nil && dt.Kind == wire.String {
			decisionStr = dt.Str
		}
	}
	decisionTs, err := instantOf(decisionStr, "removal_eligibility.decision_time")
	if err != nil {
		return nil, err
	}

	var auth *wire.Node
	if step != nil && step.Kind == wire.Object {
		auth = step.Get("authorization")
	}
	var authNotBeforeTs, authExpiresAtTs time.Time
	var hasNotBefore, hasExpiresAt bool
	if auth != nil && auth.Kind == wire.Object {
		if nb := auth.Get("not_before"); nb != nil {
			var nbStr string
			if nb.Kind == wire.String {
				nbStr = nb.Str
			}
			t, err := instantOf(nbStr, "authorization.not_before")
			if err != nil {
				return nil, err
			}
			authNotBeforeTs, hasNotBefore = t, true
		}
		if ea := auth.Get("expires_at"); ea != nil {
			var eaStr string
			if ea.Kind == wire.String {
				eaStr = ea.Str
			}
			t, err := instantOf(eaStr, "authorization.expires_at")
			if err != nil {
				return nil, err
			}
			authExpiresAtTs, hasExpiresAt = t, true
		}
	}

	removalRefused := func(reason string) *wire.Node {
		return &wire.Node{
			Kind: wire.Object,
			Fields: []wire.Member{
				{Name: "outcome", Value: strNode("removal_eligibility")},
				{Name: "verdict", Value: strNode("refused")},
				{Name: "reason", Value: strNode(reason)},
			},
		}
	}

	var targetBodyNode, expPolNode, storeStreamsNode, observedListNode *wire.Node
	if step != nil && step.Kind == wire.Object {
		targetBodyNode = step.Get("body")
		expPolNode = step.Get("expected_policy")
		storeStreamsNode = step.Get("store_streams")
		observedListNode = step.Get("observed_streams")
	}

	targetBody, targetBodyOK := validBodyObj(targetBodyNode)
	stepShapeOK := func() bool {
		if step == nil || step.Kind != wire.Object {
			return false
		}
		for _, req := range requiredRemovalStepKeys {
			if step.Get(req) == nil {
				return false
			}
		}
		for _, m := range step.Fields {
			if !allowedRemovalStepKeys[m.Name] {
				return false
			}
		}
		if _, ok := nonEmptyStr(step, "store"); !ok {
			return false
		}
		if storeStreamsNode == nil || storeStreamsNode.Kind != wire.Array || len(storeStreamsNode.Elems) < 1 {
			return false
		}
		for _, el := range storeStreamsNode.Elems {
			if el == nil || el.Kind != wire.String || el.Str == "" {
				return false
			}
		}
		if observedListNode == nil || observedListNode.Kind != wire.Array {
			return false
		}
		if !targetBodyOK {
			return false
		}
		if !hasExactKeys(expPolNode, []string{"id", "version"}) {
			return false
		}
		if _, ok := nonEmptyStr(expPolNode, "id"); !ok {
			return false
		}
		if _, ok := nonEmptyStr(expPolNode, "version"); !ok {
			return false
		}
		return true
	}()

	duplicateStoreStreams := false
	duplicateObservedStreams := false
	coverageMissing := false
	streamDamaged := false
	unobservableSource := false
	sameStoreSourceMismatch := false
	quarantinedInStoreStreams := false
	quarantinedChainMismatch := false
	localHold := false
	inheritedHold := false
	sourceChainHold := false
	pendingReviewHold := false
	sealedAncestorHold := false

	type refPair struct {
		stream string
		pin    Pin
	}
	var allRefs, activeRefs []refPair

	type replayedEntry struct {
		obs           *wire.Node
		st            *State
		chainJournals [][]*wire.Node
	}
	replayedByName := map[string]replayedEntry{}
	var replayedOrder []string

	var effQuarantined []string
	effQuarantinedIsList := false
	if step != nil && step.Kind == wire.Object && step.Get("quarantined_designated_streams") != nil {
		qds := step.Get("quarantined_designated_streams")
		if qds.Kind == wire.Array {
			effQuarantinedIsList = true
			allStr := true
			for _, el := range qds.Elems {
				if el != nil && el.Kind == wire.String {
					effQuarantined = append(effQuarantined, el.Str)
				} else {
					allStr = false
				}
			}
			if !allStr || !slices.Equal(effQuarantined, s.expectedQuarantined) {
				quarantinedChainMismatch = true
			}
		} else {
			quarantinedChainMismatch = true
		}
	} else {
		effQuarantinedIsList = true
		effQuarantined = s.quarantinedDesignated
		if !slices.Equal(effQuarantined, s.expectedQuarantined) {
			quarantinedChainMismatch = true
		}
	}

	if storeStreamsNode != nil && storeStreamsNode.Kind == wire.Array && observedListNode != nil && observedListNode.Kind == wire.Array {
		storeStreamNames := make([]string, 0, len(storeStreamsNode.Elems))
		storeStreamSet := map[string]bool{}
		for _, el := range storeStreamsNode.Elems {
			if el != nil && el.Kind == wire.String {
				if storeStreamSet[el.Str] {
					duplicateStoreStreams = true
				}
				storeStreamSet[el.Str] = true
				storeStreamNames = append(storeStreamNames, el.Str)
			}
		}
		if effQuarantinedIsList {
			for _, ss := range storeStreamNames {
				if slices.Contains(effQuarantined, ss) {
					quarantinedInStoreStreams = true
					break
				}
			}
		}
		var expectedSharing []string
		for _, ss := range storeStreamNames {
			if !s.hasRemoval || ss != s.removalStream {
				expectedSharing = append(expectedSharing, ss)
			}
		}
		var obsNames []string
		obsNameSet := map[string]bool{}
		observedByName := map[string]*wire.Node{}
		for _, obs := range observedListNode.Elems {
			if obs != nil && obs.Kind == wire.Object {
				if sn := obs.Get("stream"); sn != nil && sn.Kind == wire.String {
					if obsNameSet[sn.Str] {
						duplicateObservedStreams = true
					}
					obsNameSet[sn.Str] = true
					obsNames = append(obsNames, sn.Str)
					observedByName[sn.Str] = obs
				}
			}
		}
		if (s.hasRemoval && !storeStreamSet[s.removalStream]) || len(obsNames) != len(observedListNode.Elems) {
			coverageMissing = true
		}
		for _, name := range obsNames {
			if !storeStreamSet[name] || (s.hasRemoval && name == s.removalStream) {
				coverageMissing = true
			}
		}

		for _, sName := range expectedSharing {
			obs, ok := observedByName[sName]
			if !ok {
				coverageMissing = true
				continue
			}
			st, damaged, unobs, cj := replayObservedSharingStream(obs, s.window)
			if damaged {
				streamDamaged = true
				continue
			}
			if unobs {
				unobservableSource = true
				continue
			}
			replayedByName[sName] = replayedEntry{obs: obs, st: st, chainJournals: cj}
			replayedOrder = append(replayedOrder, sName)
			if ib := obs.Get("inherited_block"); ib != nil && ib.Kind == wire.Bool && ib.Bool {
				inheritedHold = true
			}
			if st.sealedAncestorHold {
				sealedAncestorHold = true
			}
			for _, ancJ := range cj {
				ancHolds, _, _, err := restrictionState(ancJ)
				if err == nil && len(ancHolds) > 0 {
					sourceChainHold = true
				}
			}
			revCur, err := st.reviewCurrent()
			if err != nil || !revCur {
				pendingReviewHold = true
			}
			if targetBodyNode != nil && hasExactKeys(targetBodyNode, []string{"digest", "length"}) {
				if tLen, ok := integral(targetBodyNode.Get("length")); ok {
					tDigNode := targetBodyNode.Get("digest")
					var tDig string
					if tDigNode != nil && tDigNode.Kind == wire.String {
						tDig = tDigNode.Str
					}
					tb := BodyKey{Digest: tDig, Length: tLen}
					for pk, rec := range st.records {
						if bodyMatchesTarget(rec.Get("body"), tb) {
							allRefs = append(allRefs, refPair{stream: sName, pin: pk})
							if blk, err := st.Blocked(pk); err != nil || blk {
								localHold = true
							}
							if !st.retired[pk.RecordID] {
								activeRefs = append(activeRefs, refPair{stream: sName, pin: pk})
							}
						}
					}
				}
			}
		}

		for _, sName := range replayedOrder {
			rep := replayedByName[sName]
			if rep.st.continues != nil {
				contStream, _ := strOf(rep.st.continues, "stream")
				if storeStreamSet[contStream] {
					srcRep, ok := replayedByName[contStream]
					if !ok {
						unobservableSource = true
					} else if !journalsEqual(rep.st.source, srcRep.st.journal) {
						sameStoreSourceMismatch = true
					}
				}
			}
		}
	}

	// P1: designated stream & body lifecycle preconditions.
	if !s.hasRemoval || s.stream != s.removalStream {
		return removalRefused(ReasonBodyRemovalStream), nil
	}
	if !stepShapeOK {
		return removalRefused(ReasonBodyLifecycle), nil
	}
	mode, _ := nonEmptyStr(step, "mode")
	if mode != "start" && mode != "retry_unlink" {
		return removalRefused(ReasonBodyLifecycle), nil
	}
	reason, _ := nonEmptyStr(step, "reason")
	if reason != "retention" && reason != "operator" && reason != "rights_restricted" {
		return removalRefused(ReasonBodyLifecycle), nil
	}
	inc, ok := integral(step.Get("incarnation"))
	if !ok || inc < 1 {
		return removalRefused(ReasonBodyLifecycle), nil
	}
	if !s.removableIncarnationOK(targetBody, inc, mode) {
		return removalRefused(ReasonBodyLifecycle), nil
	}

	// P2: store & sharing-stream protection coverage.
	if duplicateStoreStreams ||
		duplicateObservedStreams ||
		coverageMissing ||
		streamDamaged ||
		unobservableSource ||
		sameStoreSourceMismatch ||
		quarantinedInStoreStreams ||
		quarantinedChainMismatch {
		return removalRefused(ReasonProtectionUnavailable), nil
	}

	// P2b: decision_time monotonicity against designated & sharing streams.
	lastAtStr, _ := strOf(s.journal[len(s.journal)-1], "at")
	lastAt, _ := ParseInstant(lastAtStr)
	if decisionTs.Before(lastAt) {
		return removalRefused(ReasonJournalTime), nil
	}
	for _, sName := range replayedOrder {
		st := replayedByName[sName].st
		stLastAtStr, _ := strOf(st.journal[len(st.journal)-1], "at")
		stLastAt, _ := ParseInstant(stLastAtStr)
		if decisionTs.Before(stLastAt) {
			return removalRefused(ReasonJournalTime), nil
		}
	}

	// P3: active holds, inherited blocks, source-chain holds, pending review, sealed ancestor holds.
	if localHold || inheritedHold || sourceChainHold || pendingReviewHold || sealedAncestorHold {
		return removalRefused(ReasonProtectedUse), nil
	}

	// P4: non-retired references remain.
	if len(activeRefs) > 0 {
		return removalRefused(ReasonStillReferenced), nil
	}

	// P5: removal authorization required when any historical reference exists.
	if len(allRefs) > 0 && !validRemovalAuthorization(auth, step, targetBody, decisionTs, hasNotBefore, authNotBeforeTs, hasExpiresAt, authExpiresAtTs) {
		return removalRefused(ReasonRemovalAuthorization), nil
	}

	return &wire.Node{
		Kind: wire.Object,
		Fields: []wire.Member{
			{Name: "outcome", Value: strNode("removal_eligibility")},
			{Name: "verdict", Value: strNode("eligible")},
		},
	}, nil
}
