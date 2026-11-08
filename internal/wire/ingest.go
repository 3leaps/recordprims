package wire

import "strings"

// Document kinds accepted by Ingest.
const (
	KindJournalEntry       = "journal_entry"
	KindRecord             = "record"
	KindProtectionRequest  = "protection_request"
	KindProtectionReceipt  = "protection_receipt"
	KindWriteReceipt       = "write_receipt"
	KindExportRequest      = "export_request"
	KindExportResult       = "export_result"
	KindBodyStateRequest   = "body_state_request"
	KindStratumProfile     = "stratum_profile"
	KindStreamSeal         = "stream_seal"
	KindRemovalEligibility = "removal_eligibility"
)

// Position kinds: "int" is a protocol integer, "opaque" is free JSON (a record
// payload, for example), "list:<kind>" is an array of <kind>, and any other
// value names an object spec below.
const (
	posInt    = "int"
	posOpaque = "opaque"
)

// protocolInts maps each object kind to the members that are protocol
// integers or nested protocol objects, across durable-record v0 and v1.
// Members not named are opaque. It mirrors the contract's reference
// interpreter; TestProtocolIntsCoverSchemas (internal/conformance) checks it
// against the vendored schemas.
var protocolInts = map[string]map[string]string{
	"pin":             {"revision": posInt},
	"revision_pin":    {"revision": posInt},
	"body_ref":        {"length": posInt},
	"lifecycle_body":  {"length": posInt},
	"link":            {"revision": posInt},
	"source_cut":      {"cut_seq": posInt},
	"source_pin":      {"pin": "pin"},
	"reconciled_body": {"body": "lifecycle_body", "floor": posInt},
	"stream_recovery": {"cut_seq": posInt, "reconciled_bodies": "list:reconciled_body"},
	"ancestor_seal":   {"cut_seq": posInt},
	KindStreamSeal: {
		"v": posInt, "cut_seq": posInt, "ancestor_seals": "list:ancestor_seal",
	},
	"seal_context": {"seal": KindStreamSeal, "ancestor_seal_contexts": "list:seal_context"},
	KindRecord: {
		"v": posInt, "revision": posInt, "supersedes": "revision_pin",
		"body": "body_ref", "links": "list:link", "payload": posOpaque,
	},
	KindProtectionRequest: {
		"v": posInt, "target": "pin", "body": "lifecycle_body", "restores": posInt,
	},
	KindJournalEntry: {
		"v": posInt, "seq": posInt, "group": posInt, "group_size": posInt,
		"revision": posInt, "destination_cut_seq": posInt, "incarnation": posInt,
		"record": KindRecord, "presented": "revision_pin", "current": "revision_pin",
		"request": KindProtectionRequest, "continues": "source_cut",
		"source": "source_cut", "imported_from": "source_pin", "body": "lifecycle_body",
		"recovery": "stream_recovery",
	},
	KindProtectionReceipt: {"v": posInt, "seq": posInt},
	KindWriteReceipt: {
		"v": posInt, "seq": posInt, "pin": "pin", "current": "pin",
		"receipt": KindWriteReceipt,
	},
	KindExportRequest: {
		"v": posInt, "protection_cut": posInt, "current_cut": posInt,
		"selection": "list:pin", "omitted": "list:pin",
	},
	KindExportResult: {
		"protection_cut": posInt, "selection": "list:pin", "omitted": "list:pin",
	},
	KindBodyStateRequest: {"body": "lifecycle_body"},
	"observed_sharing_stream": {
		"journal": "list:" + KindJournalEntry, "source_journal": "list:" + KindJournalEntry,
		"ancestors": "list:list:" + KindJournalEntry, "source_seal": "seal_context",
	},
	"removal_authorization": {"body": "lifecycle_body", "incarnation": posInt},
	KindRemovalEligibility: {
		"body": "lifecycle_body", "incarnation": posInt,
		"observed_streams": "list:observed_sharing_stream", "authorization": "removal_authorization",
	},
	"profile_runtime": {
		"clock_skew_bound_ms": posInt, "idempotency_window_ms": posInt,
	},
	KindStratumProfile: {"runtime": "profile_runtime"},
}

// Ingest runs the contract's first two stages on one document of the given
// kind: stage 1 (ingest) parses strictly and checks every opaque number across
// the whole document, then stage 2 (numeric_domain) checks every protocol
// integer token. A refusal is a *Reject with reason "ingest" or
// "numeric_domain". Shape and semantics are later stages.
func Ingest(raw []byte, kind string) (*Node, error) {
	if _, ok := protocolInts[kind]; !ok {
		panic("wire: unknown document kind " + kind)
	}
	n, err := ParseJSON(raw)
	if err != nil {
		return nil, err
	}
	if err := opaquePass(n, kind); err != nil {
		return nil, err
	}
	if err := intPass(n, kind); err != nil {
		return nil, err
	}
	return n, nil
}

// opaquePass is the rest of stage 1: every number outside a protocol-integer
// position must be a finite binary64 value. Protocol-integer tokens are left
// for stage 2.
func opaquePass(n *Node, kind string) error {
	switch {
	case kind == posInt:
		if n.Kind == Number {
			return nil
		}
		return opaquePass(n, posOpaque)
	case kind == posOpaque:
		switch n.Kind {
		case Number:
			return checkOpaque(n.Token)
		case Array:
			for _, e := range n.Elems {
				if err := opaquePass(e, posOpaque); err != nil {
					return err
				}
			}
		case Object:
			for _, m := range n.Fields {
				if err := opaquePass(m.Value, posOpaque); err != nil {
					return err
				}
			}
		}
		return nil
	case strings.HasPrefix(kind, "list:"):
		if n.Kind != Array {
			return opaquePass(n, posOpaque)
		}
		for _, e := range n.Elems {
			if err := opaquePass(e, strings.TrimPrefix(kind, "list:")); err != nil {
				return err
			}
		}
		return nil
	}
	if n.Kind != Object {
		return opaquePass(n, posOpaque)
	}
	spec := protocolInts[kind]
	for _, m := range n.Fields {
		k, ok := spec[m.Name]
		if !ok {
			k = posOpaque
		}
		if err := opaquePass(m.Value, k); err != nil {
			return err
		}
	}
	return nil
}

// intPass is stage 2: every number at a protocol-integer position must be an
// exact safe integer. Other types pass through to the shape stage.
func intPass(n *Node, kind string) error {
	switch {
	case kind == posInt:
		if n.Kind != Number {
			return nil
		}
		v, err := SafeInt(n.Token)
		if err != nil {
			return err
		}
		n.Int, n.IsInt = v, true
		return nil
	case kind == posOpaque:
		return nil
	case strings.HasPrefix(kind, "list:"):
		if n.Kind != Array {
			return nil
		}
		for _, e := range n.Elems {
			if err := intPass(e, strings.TrimPrefix(kind, "list:")); err != nil {
				return err
			}
		}
		return nil
	}
	if n.Kind != Object {
		return nil
	}
	spec := protocolInts[kind]
	for _, m := range n.Fields {
		k, ok := spec[m.Name]
		if !ok {
			continue
		}
		if err := intPass(m.Value, k); err != nil {
			return err
		}
	}
	return nil
}

// MemberKind reports how stage 2 treats member name of an object of the given
// kind: "int" for a protocol integer, "opaque", "list:<kind>", or a nested
// object kind. The conformance suite checks it against the vendored schemas.
func MemberKind(kind, name string) string {
	if k, ok := protocolInts[kind][name]; ok {
		return k
	}
	return posOpaque
}

// IsObjectKind reports whether kind names an object spec.
func IsObjectKind(kind string) bool {
	_, ok := protocolInts[kind]
	return ok
}
