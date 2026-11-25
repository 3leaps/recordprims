# Record Collection (`record-collection/v0`)

**Status:** Proposed, experimental.

**Capability:** `contract: record-collection/v0`

Resolve this contract through a trusted, pinned [`contract.json`](contract.json)
manifest rather than fetching schema URIs over the network.

## Purpose

`record-collection/v0` defines a named, versioned statement that a bounded set
of exact `durable-record/v1` record revisions belong together, optionally in an
explicit order. A collection is carried as the payload of an ordinary
`durable-record/v1` record (`"payload_contract": "contract: record-collection/v0"`)
with no store-layer changes, or wrapped in a standalone
`collection-document.schema.json` envelope when exchanged outside a record
store.

Other contracts that need an ordered list of exact pins can compose the `list`
shape and add their own coverage and publication rules.

## Schemas

| File                                                                 | Role                                                                                                         |
| -------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------ |
| [`collection-document.schema.json`](collection-document.schema.json) | Entry schema (`capabilities` envelope containing `contract: record-collection/v0` and a `collection` object) |
| [`record-collection.schema.json`](record-collection.schema.json)     | Bare payload schema (`object_schemas.record_collection`) for embedding inside a `durable-record/v1` record   |

**Standalone document limit:** Outside a carrying `durable-record/v1` record, a
standalone `collection-document` has no revision number or predecessor link, so
`append_only` extension and correction-by-superseding-revision cannot be
verified from the document alone; a reader needs the carrying records for
history verification.

## Data Model and Invariants

### Kinds

- **`set`**: Unordered membership with canonical storage order. Members MUST be
  sorted ascending by `(pin.digest, pin.stream, pin.record_id, pin.revision)`
  (UTF-8 lexicographic order for strings, numeric order for `revision`).
  Duplicate pins are rejected, and `allow_repeats: true` is forbidden. Two
  writers constructing a `set` with identical membership and identical
  non-member payload fields produce identical canonical JSON payload bytes
  (RFC 8785) and the same canonical payload digest. That payload digest is
  distinct from the enclosing `durable-record/v1` record digest, which also
  covers stream, record identifier, revision, provenance, timestamps, and
  classification.
- **`list`**: Ordered membership where stored array order is authoritative.
  Readers MUST preserve stored order and MUST NOT reorder members by
  timestamps or pin fields. Duplicate pins are rejected by default unless
  `allow_repeats: true` is explicitly declared.

### Members and Exact Pins

Every member in both `set` and `list` carries one exact `durable-record/v1`
revision pin:

```json
{
  "pin": {
    "stream": "evidence.main",
    "record_id": "rec-0001",
    "revision": 1,
    "digest": "sha256:..."
  }
}
```

- `stream`, `record_id`, `revision`, and `digest` reuse the exact types and
  patterns of `durable-record/v1`.
- `members` is bounded (`maxItems: 10000`) and `member_count` MUST equal the
  exact length of `members`.
- Corrections append a new revision pin in a superseding collection revision;
  earlier collection revisions and earlier member revisions remain resolvable.

### Append-Only History (`append_only`)

When `append_only` is `true`, every superseding revision of the collection MUST
keep `append_only: true`, preserve `kind`, and strictly extend its predecessor:

- For `set`: strict superset (`len(next) > len(prev)` and every pin in `prev`
  appears in `next`).
- For `list`: strict prefix extension (`len(next) > len(prev)` and
  `next[0:len(prev)] == prev` in exact order).

### Closure Claims (`closure_claim`) and Coverage Limits

- `"none"` (default): The writer makes no completeness claim about member
  resolution at write time.
- `"complete_at_write"`: A writer claim that every pinned member resolved with
  a matching digest (and, for body-backed records, verified body bytes) when
  the collection was written. Readers verify this claim independently; the
  underlying record store does not enforce it.

Structural validity of a collection payload is separate from closure
verification: a nonempty `complete_at_write` collection evaluated without
resolver evidence MUST NOT be reported as closure-verified and MUST report
absent evidence explicitly (`closure_evidence_absent`). Establishing a
historical `complete_at_write` claim requires an explicit at-write resolver
snapshot (`closure_verified_at_write` when every member verified at write time,
or `closure_refuted_at_write` when any failed). A later read observing current
store state — whether every pin currently verifies or reports
`member_unresolved`, `member_held`, `body_missing`, or `body_held` — reports
the current read outcome and leaves the historical `complete_at_write` claim
unproven (`closure_unproven`).

A collection establishes selected membership only, never complete stream
coverage. Even when every supplied stream event appears in a collection's
members, complete coverage remains unproven (`coverage_unproven`) without
journal/audit history and a declared cut; omitting a supplied event fails
coverage explicitly (`coverage_incomplete`).

### Body-Backed Evidence Members

When a collection pins body-backed `durable-record/v1` records (for example,
records referencing verified files via a `body` object carrying `digest`,
`length`, `media_type`, and optional governed `artifact_id`):

- The collection still pins the exact `durable-record/v1` revision pin.
- The body-first rule is preserved: the canonical record envelope digest
  (`pin.digest`) is distinct from the raw file byte digest (`body.digest`).
- Altering the underlying file bytes while retaining a matching record envelope
  MUST fail byte verification (`body_digest_mismatch`).
- Any integrity failure (`member_digest_mismatch`, `body_digest_mismatch`,
  `record_body_digest_collision`) takes precedence over availability-only
  outcomes (`member_unresolved`, `member_held`, `body_missing`, `body_held`)
  regardless of member order.
- Unresolved member records, held member records, missing body bytes, and held
  body bytes MUST be reported explicitly (`member_unresolved`, `member_held`,
  `body_missing`, `body_held`) rather than silently skipped, subject to the
  reader's authorized visibility.

## Verification Assets

- [`examples/`](examples/): Valid positive fixtures for both schemas and kinds.
- [`rejects/schema/`](rejects/schema/): Paired one-leaf-diff `baseline-*` and
  `reject-*` schema negative controls with [`index.json`](rejects/schema/index.json).
- [`rejects/semantic/`](rejects/semantic/): Semantic negative controls proving
  the reference verifier catches wrong verdicts, wrong refusal codes, empty
  expectations, and digest mismatches.
- [`vectors/`](vectors/): Executable behavioral vectors with
  [`INVENTORY.json`](vectors/INVENTORY.json), verified by
  `scripts/test-record-collection-controls.py`.
