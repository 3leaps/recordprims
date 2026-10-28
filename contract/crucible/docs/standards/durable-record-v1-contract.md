# Durable Record v1: Retained Records with Use Restrictions

**Status: proposed, experimental.** Capability: `contract: durable-record/v1`.
Schemas: [`schemas/durable-record/v1`](../../schemas/durable-record/v1/).

## Version boundary

This family adds `tombstoned` and `untombstoned` journal entries. Adding entry
types is a breaking change under the [v0 standard](durable-record-contract.md),
so this is v1. The v0 schemas and semantics remain unchanged. Repository release
numbers and contract versions are independent; an experimental v1 contract does
not imply a stable implementation.

The record's `v` denotes its envelope version, not the contract-family
version. A v1 journal carries **record envelope `v: 1`**: the v0 envelope with
`v` changed from `0` to `1` and one optional field, `produced_at`, added. Its
canonicalization, body digest and revision pin rules are those of v0. The
controls enforce that the v1 record schema equals the v0 record schema after
those two changes and no others. A `v: 0` record inside a v1 journal entry is
refused as `record_version`. A record does not become a new revision when a
restriction changes. Journals, write receipts, reconciliation results and
protection requests use `v: 1`.

**Amendment, 2026-10-25.** The record envelope version bump, `produced_at`,
the canonical instant spelling and keyed retirement were made to v1 in place,
before any implementation of v1 had been released. They are not additive:
v1 records written before this amendment are `v: 0` records and are refused by
an amended reader. The v0 family is unchanged. Stratum profiles advertise the v1
capability and require `tombstoning` in both compiled and enabled capabilities.
A profile's optional `commit_strategy` is one of `fsync_file_and_dir`,
`fsync_file`, `backend_acknowledged` or `none`, with free-text detail in
`commit_note`. `backend_acknowledged` means the configured backend acknowledged
the operation, without implying which failure it survives; `none` means no
explicit commit step, not that data cannot persist. Both fields are
information only; `failure_model` remains the sole durability guarantee.
Readers MUST negotiate the journal capability and MUST refuse unknown entry
types; recognizing the record envelope alone is insufficient.

All v0 rules for content writes, atomic groups, consistent cuts, receipts,
classification, failure models and reconciliation continue to apply unless
explicitly changed here. This family defines no automatic journal conversion
or migration of outstanding receipts and live keys. Relabeling a v0 journal as
v1 is not a migration protocol. A v1 reader MUST refuse v0 journals and
receipts. One implementation MAY serve both capabilities through distinct
version-aware entry points; it MUST NOT silently reinterpret one as the other.

A live v0 stream remains v0. An adopter needing restrictions opens a separately
addressed v1 stream and re-records content under v1 acceptance rules; v0 keys,
receipts and journal history MUST NOT carry over, and retry safety for outstanding
v0 keys stays with the v0 stream. Content re-recorded into a v1 stream becomes
a `v: 1` record, so its digest and pin change even when the logical `stream`
identifier is kept; choosing a new identifier changes them as well. A v0 pin
never identifies a v1 record. Re-recording MUST satisfy the ordinary revision-chain rules, including any
required predecessors. It produces new v1 journal entries and receipts, not
copies of v0 acknowledgments. The adopter MUST establish applicable retirement
state, restrictions and inherited protection before making the content available for
ordinary serving, export or reuse. A retained v0 copy cannot authorize release
on behalf of the v1 store.
Once content is re-recorded under the same stream identifier, the v0 stream
MUST accept no further content writes for that identifier; its retained records
are copies outside v1 management, and ordinary serving, export or reuse of those
pins MUST resolve through the v1 store.

## Production time, instants and ordering

A record MAY carry `produced_at`, the producer's claim of when the revision was
produced: `instant`, `source` (`producer_clock` or `trusted_time`) and, required
when `source` is `trusted_time`, `evidence` (`kind`, opaque `ref`, `digest`).
The field is inside the record digest, so it survives re-recording when the
record is carried unchanged. It is a claim: `producer_clock` is attribution,
like provenance, and `trusted_time` evidence is opaque to the store and
unverified until an adopter verifies it. Neither makes the time authoritative.
A store does not assign `produced_at`: the producer computes the digest before
acceptance. Store acceptance time is the journal entry's `at`, outside the
record digest.

Every instant on the v1 wire is spelled `YYYY-MM-DDTHH:MM:SS.ffffffZ`: UTC,
`Z`, exactly six fractional digits, and a valid RFC 3339 date and time (no
February 30, no hour 24, no leap second). This covers journal `at`, `observed_at`,
`produced_at.instant`, reconciliation `attempted_at` and coverage endpoints.
Emitters MUST use this spelling. Readers MUST refuse any other spelling, and any
canonical-looking instant that is not a valid date and time, as
`instant_spelling` and MUST NOT normalize it: a digest covers the bytes the
producer wrote, so a store that rewrote `observed_at` or `produced_at.instant`
would invalidate the caller's digest. An instant that is not exactly
representable at microseconds is refused, never rounded.

`seq` orders one stream's journal. It is a store artifact and is not portable:
it changes when content is re-recorded into another stream. Portable ordering
uses `produced_at` or an explicit order carried by the payload. Ordering
records from several streams by `produced_at` is a presentation order, not a
causal or authoritative total order: ties break by `instant`, `stream`,
`record_id`, then `revision`, and records without `produced_at` follow those
with it in the same order. A consumer presenting such an order MUST state that
`producer_clock` times carry unknown skew; only `trusted_time` with verified
evidence supports a claim about when something happened.

## Meaning of restriction

| Operation         | Current ordinary reads        | Export and reuse               | Authorized audit history                        | Bytes                |
| ----------------- | ----------------------------- | ------------------------------ | ----------------------------------------------- | -------------------- |
| `retired`         | Withdraws one record          | Existing selection rules apply | Retained                                        | Retained             |
| `tombstoned`      | Blocks matching records       | Blocks matching records        | Retained and readable under audit authorization | Retained             |
| `untombstoned`    | Removes one identified hold   | Removes that same hold         | Both operations remain                          | Retained             |
| Managed redaction | Separate, reserved capability | Separate policy                | May remove content                              | Not implemented here |

Retirement is per record, not stream closure. Repeated retirement entries are
permitted audit observations; their effect remains withdrawal, not a new state. Restoration does not unretire a
record, undo classification, clear an inherited restriction, or clear another
hold. Tombstoning is not deletion, cryptographic erasure, or recall of a copy
already released. Releasing to a consumer that cannot negotiate v1 produces a
copy outside management; a later hold does not reach that copy. Storage strata
retain their declared deletion limits.

An audit path MUST require separate authorization. A caller-supplied `audit`
purpose MUST NOT bypass ordinary serving, export or reuse checks. Provenance
records attribution; it does not establish authority. Authentication, evidence
verification and authorization are adopter duties, required before accepting
an operation. The logical fixtures assume authorized actors.

## Requests and scope

A protection request names an exact existing `target` pin and includes:

- `stream`, `idempotency_key`, `action` (`tombstone` or `restore`);
- `scope`, `reason`, opaque evidence reference and evidence digest;
- `observed_at` and provenance;
- `body` digest and byte length only for body scope;
- `restores`, the accepted hold's journal sequence, only for restoration.

The request is closed by schema. Evidence references are opaque identifiers;
portable requests MUST NOT embed source content, credentials or fetchable
locator credentials. Evidence is retained in the authorized evidence domain,
not presumed safe for export because its digest appears here.

| Scope      | Exact effect within the named stream                                                               |
| ---------- | -------------------------------------------------------------------------------------------------- |
| `record`   | All revisions of the target record, including subsequent revisions                                 |
| `revision` | Only the exact target revision pin                                                                 |
| `body`     | Every record referencing the target body's digest and byte length, including subsequent references |

A body request MUST match the body's digest and length in the pinned record.
Its scope is stream-local, not a global assertion of ownership of bytes. An
inline payload is protected by record or revision scope. Several holds may
coexist, even for the same target and reason; eligibility requires that none
matches. A content revision may be recorded while held, but the hold still
blocks its use when its scope matches.

Tombstone reasons are `source_removed`, `source_corrected`, `rights_restricted`,
`security_hold`, and `operator_hold`. Restore reasons are `source_restored` and
`hold_released`. A source change is an observation requiring an authorized
operation, not an automatic inference from failed retrieval or an index
listing. In particular, a durable-index absence tombstone is an inventory
statement, not a durable-record rights restriction.

`observed_at` MUST be no later than acceptance time. Restoration MUST identify
an active hold and repeat its exact target, scope and body selector; its
observation MUST be no earlier than the hold's observation. An observation of
restoration does not supersede any other hold. Timestamps use the canonical
spelling above. Journal acceptance times MUST be nondecreasing. The evidence
observation can precede acceptance.

## Acceptance, journal and replay

The accepted journal entry carries the complete `request` and `request_digest`.
The digest is SHA-256 of the request's RFC 8785 JCS bytes, with a `sha256:` prefix.
It binds all fields, including provenance, evidence and the key. It is not a
journal hash chain or a signature; this family adds neither. No content
record, revision pin, or body bytes are changed. The normal sequence and atomic
group rules apply; a visible cut MUST end at a complete group.

An accepted protection receipt contains `v`, `stream`, `idempotency_key`,
`outcome` (`tombstoned` or `untombstoned`), `seq`, `request_digest`, and the
stream's acknowledged `failure_model`. A refusal contains only version,
stream, key and refusal outcome. Refusals accept no operation and reserve no
key. A journal line with an invalid request digest, operation kind, target,
restore relation or already-live accepted key MUST be refused.

Content writes and protection operations share one stream-scoped key namespace
and one positive `idempotency_window_ms`. Unlike v0's benchmark exception, v1
does not permit `none`. The window begins at original acceptance and does not
slide on retry. Its upper boundary is exclusive. Key identity distinguishes
content digests from protection request digests.

Within the window, key lookup precedes checks against current target or hold
state. An identical protection retry returns the exact original receipt,
including its sequence and failure model, even after restoration. It does not
reapply a hold or append another accepted operation. A changed request or a
collision with a content-write key yields `idempotency_conflict`. Protection
replays append no journal entry; v0 content replay audit rules still apply.
A `deduplicated` content audit entry MUST name a live accepted content-write
key and the same record id. A protection key, absent key, expired key or a
different record id MUST be refused as `invalid_deduplication`. The audit
entry does not refresh the key window.
After expiry, a request is evaluated anew: retry safety is no longer promised.

A `retired` entry MAY carry an `idempotency_key`; it then also carries
`request_digest`, the `sha256:`-prefixed SHA-256 of the RFC 8785 JCS bytes of
exactly `{stream, record_id, reason, idempotency_key}`. Journal-assigned `seq`,
`at`, `group` and `type` are not part of it. A keyed retirement shares the
stream's key namespace and window with content writes and protection
operations, and is acknowledged by an accepted write receipt with outcome
`retired`: `v`, `kind`, `stream`, `idempotency_key`, `seq`, `record_id`,
`request_digest` and `failure_model`, with no `pin` or `body_status`. Within the
window, the same key and the same retirement request return the exact original
receipt, including its sequence, and append nothing; the same key with a
changed request, or a key already used by another kind of operation, yields
`idempotency_conflict`. A distinct key MAY append a further retirement audit
observation with its own receipt and sequence. Reconciliation `found` MAY carry
a retirement receipt. An unkeyed `retired` entry remains a permitted audit
observation with no receipt and no replay promise. `retired_record` remains the
refusal of a content write that would revive a retired record; it is not the
accepted retirement outcome.

A missing receipt remains indeterminate. The v1 reconciliation schema permits
`found` to carry either the original accepted content receipt or the original
accepted protection receipt. Stream, key, sequence, digest and failure model
MUST agree with the accepted operation. The v0 skew-bound coverage rules for
`not_found` apply to the shared namespace, including every accepted protection
operation. Without complete coverage, return `unknown`, never inferred absence.
The logical interpreter demonstrates `found` and conservative `unknown`; it
has no authoritative coverage provider and never manufactures `not_found`.

## Continuation streams and restriction review

A v1 stream that re-records content from another stream is a **continuation**.
Its `stream_opened` entry carries `continues`: the source journal's capability
(`contract: durable-record/v0` or `contract: durable-record/v1`), its `stream`,
and a source cut `{cut_seq, cut_digest}`. The capability is required because a
v0 and a v1 journal can retain the same logical stream identifier. A stream
without `continues` is not a continuation, and nothing in this section applies
to it.

**Cut digest.** `cut_digest` is `sha256:` followed by the lowercase hex SHA-256
of the RFC 8785 JCS bytes of the JSON array of the source journal's entries
`1..cut_seq`, in order. `cut_seq` MUST end a complete group. Every entry
carries its wire version `v` and `stream`, so the preimage already
distinguishes a v0 from a v1 journal with the same identifier. A cut digest
binds history; it does not prove that the cut is the source's current
authoritative cut.

**Imports.** A content entry (`appended` or `revised`) in a continuation that
re-records source content carries `imported_from`, the exact source pin it
re-records. `imported_from.stream` MUST equal the stream named by `continues`.
Re-recording changes the record's digest and pin (see Version boundary), so the
source pin is not the destination pin.

**Review entry.** A `restrictions_reviewed` entry, in the continuation stream,
records an authorized review. It carries:

- `source`: capability, `stream`, `cut_seq` and `cut_digest` of the source cut
  reviewed. It has the capability and stream of `continues` and MAY advance
  beyond the opening cut, so a live continuation can import later source
  content: `cut_seq` is at least `continues.cut_seq` and ends a complete group,
  and the source prefix through the opening cut still hashes to
  `continues.cut_digest`;
- `destination_cut_seq`: the last destination entry the review covers. The
  review is its own atomic group (`group` equals `seq`, `group_size` 1), so
  `destination_cut_seq` is `seq - 1`, the complete cut immediately before it;
- `source_state_digest`: the digest of the **effective source restriction
  manifest** at that source cut;
- `destination_applied_state_digest`: the digest of the **destination applied
  manifest** at `destination_cut_seq`;
- `authorization`: opaque reference and digest, the same shape as protection
  evidence;
- `provenance`.

Both digests are `sha256:` of the RFC 8785 JCS bytes of the manifest object.
The **effective source restriction manifest** is
`{holds, retired, inherited}` where `holds` lists every active hold at the
source cut as `{capability, stream, seq, request_digest}`, `retired` lists every
retired record as `{capability, stream, record_id}`, and `inherited` lists the
`source_state_digest` of every review the source itself depends on (empty when
the source is not a continuation), each as `{capability, stream,
source_state_digest}`. The **destination applied manifest** is
`{holds, retired}` in the same member form, qualified by the destination
stream: the holds active and records retired in the destination at
`destination_cut_seq`. Each list is sorted by the JCS bytes of its members and
has no duplicates.

Every observed source and ancestor journal is validated under its declared
family before its restriction state is derived: its entries' shape, its v0
journal rules and, for a v1 source, the v1 semantics of every entry (a restore
that changes its hold's target is refused, for example). An invalid source
supplies no restriction state, and a journal relying on it is refused as
`continuation`; the same holds when the source advances.

A reader MUST refuse, as `continuation`, a journal whose review names a source
capability or stream that disagrees with `continues`, a cut before the opening
cut, or a source whose opening prefix no longer hashes to `continues.cut_digest`, whose cut digest or source state
digest does not match the observed source, whose applied state digest does
not match the destination at `destination_cut_seq`, or whose covered
`imported_from` pins do not exist at or before the reviewed source cut. An
`imported_from` that names another source, and a `continues`,
`imported_from` or review in a stream that is not a continuation, are refused
the same way.

The reviewer MUST verify the mapping from each restricted source pin to the
destination pins that re-record it, because re-recording changed their
digests. The logical interpreter checks the manifests' shape and digests, not
that mapping: mapping and authorization are adopter duties.

**Serving.** A continuation's serve, export and reuse decisions fail closed as
`restrictions_pending` unless the latest `restrictions_reviewed` entry is
current:

- it names the same source capability and stream as `continues`;
- no `imported_from` entry follows its `destination_cut_seq`;
- no `tombstoned`, `untombstoned` or `retired` entry follows it in the
  destination, so the applied manifest it verified is still the destination's
  restriction state;
- the source's effective restriction manifest, at the source's current
  authoritative cut, still has the reviewed `source_state_digest`. A source
  whose state cannot be observed fails closed;
- if the source is itself a continuation, its own latest review is current by
  these same rules against its source, and so on through every ancestor. A
  chain with an ancestor that cannot be observed fails closed, unless each
  boundary above it has a verified seal.

In the logical form, the enforcing store supplies that current digest as the
trusted export-request input `source_state_digest`, alongside
`protection_state`. A missing, unobservable or disagreeing source state, or an
unobservable ancestor, is `protection_unavailable`; the input on a stream that is not a continuation is
refused the same way. A current source state that differs from the reviewed
one, a later import, a later destination restriction change, a pending review
anywhere in the source chain, or no review at all is `restrictions_pending`.

A later import from the source, a destination restriction change, or a
change in the effective restrictions of the source or any ancestor makes
review pending again. Ordinary destination content writes after the review do
not; they are governed by the destination's own current-cut hold rules. The
manifests are effective state, not event history: a source hold accepted and
then restored returns the source to its reviewed state, and the review is
current again if no release occurred while the state differed. A sealed source, read-only at the reviewed cut, can make the
source-state check final only with a verified seal of that cut; this bundle
defines no seal format.

The source-state check and the destination's current-cut release decision
MUST be held together through release, serialized with protection operations
on both streams: a source hold accepted between the check and the release
blocks it. The logical interpreter decides at one observation and does not
qualify that serialization.

## Serving, reuse and export

Before releasing selected records or referenced bodies, the consumer MUST
obtain the authoritative protection state at a current consistent cut, apply
all matching holds, and apply inherited restrictions. Unknown, unavailable or
stale protection state fails closed. The cut check and release decision MUST
be serialized with accepted protection operations; an old exported journal or
a caller's asserted cut cannot authorize a new release. This is a decision at
one observation, not a perpetual permission. Distributed enforcement and
revocation of previously released copies are not claimed.

The export-request schema is a logical decision input, not a public API.
`current_cut`, `protection_cut`, `protection_state`, and `inherited_block` are
trusted inputs supplied by the enforcing store and policy layer, not values
an untrusted caller may assert. The fixture evaluator checks the two cuts
against the actual full journal. A production adopter must independently
establish these inputs and prevent races through release.

`purpose` is `serve`, `export`, or `reuse`; all enforce holds and, for a
continuation, the restriction review. `serve` also
excludes retired records. The same selection mechanism is used by the logical
fixtures. There is no `audit` export purpose.

An explicitly filtered selection MAY omit any records, held or unheld; holds
are not a prerequisite for filtering. `selection` and
`omitted` are disjoint exact-pin lists whose union is the revision inventory
at the decision cut. An omitted pin is a declared verification boundary, not
a selected record, and must not cause its record or body to be included.
Selected records keep their original pins and digests. Implementations MUST
NOT silently omit protected records from an allegedly complete export.
Omission metadata itself requires authorization; this contract does not
promise that revealing record identities is safe.

Selection by label is an exact, case-sensitive string match against a
record's `labels`. What a label means is defined by the adopter; the store
never interprets labels, so a label selection is not a store guarantee of
completeness or classification. The resulting selection is still an
exact-pin list subject to every rule above.

Closure remains orthogonal to permission:

- `full_content` requires the bytes for every selected body reference;
- `references_only` may omit selected bodies, but MUST NOT include protected
  records or provide an escape from hold checks;
- any included body MUST have the declared raw-byte SHA-256 and length and
  belong to the selected records. Duplicate or surplus body members are refused.

The logical fixture form carries body bytes as lowercase hex. It does not
standardize a physical archive format. `export-result` records either the
allowed selection, omissions, cut and closure, or a refusal reason. It is not
a durable authorization token.

Downstream artifacts, caches, indexes and analytics tables MUST preserve the
restriction dependency or recheck authoritative state before use. An inherited
block dominates an otherwise allowed local selection. Building a derived table
does not itself remove a restriction. Physical propagation, lineage transport
and enforcement in analytics engines require adopter specifications; these
logical fixtures do not qualify them.

## Refusal vocabulary

Existing v0 shape, journal, receipt and reconciliation refusals remain. The
following are protection-operation outcomes or v1 logical validation failures.
Schema validation precedes semantic evaluation.

| Reason                   | Meaning                                                                    |
| ------------------------ | -------------------------------------------------------------------------- |
| `idempotency_conflict`   | A live key identifies another request or content operation                 |
| `target_not_found`       | No exact target pin in this stream                                         |
| `body_mismatch`          | The requested body selector disagrees with its target                      |
| `hold_not_active`        | Restoration does not identify an active hold                               |
| `hold_mismatch`          | Restoration changes the hold's target or scope                             |
| `observation_order`      | Observation follows acceptance or restoration predates the hold            |
| `protection_unavailable` | Protection or source state is unavailable, from another stream, or not current |
| `protected_use`          | Matching or inherited hold, or a retired record selected for serving       |
| `export_selection`       | Selection and omissions do not partition the inventory                     |
| `body_closure`           | Missing required bytes, wrong bytes/length, duplicate or surplus body      |
| `invalid_purpose`        | Use purpose is not serve, export or reuse                                  |
| `invalid_deduplication`  | Content audit does not identify a live accepted content key and its record |
| `receipt_mismatch`       | Received protection or retirement receipt disagrees with the journal       |
| `request_digest`         | Journal request does not match its canonical digest                        |
| `operation_type`         | Journal type disagrees with request action                                 |
| `accepted_key_reuse`     | A journal accepts a live key again                                         |
| `journal_version`        | A v1 journal contains another wire version                                 |
| `record_version`         | A v1 journal entry carries a record envelope other than `v: 1`             |
| `instant_spelling`       | An instant is not a valid date and time in the canonical six-digit UTC form |
| `journal_time`           | Acceptance or decision time precedes the accepted journal                  |
| `idempotency_window`     | Missing positive key-retention window                                      |
| `retired_record`         | A content write attempts to revive a retired record                        |
| `restrictions_pending`   | A continuation has no current restriction review                           |
| `continuation`           | An import, review or manifest disagrees with the stream's continuation     |

## Conformance and limits

Run `python3 scripts/test-durable-record-v1-controls.py`. File-backed vectors
cover scopes, independent holds, replay, expiration, reconciliation, atomic
cuts, keyed and unkeyed retirement, production time, instant spelling, record
envelope version, export omissions, inherited restrictions, body closure, and
continuation review (cut and manifest digests, imports, currency).
Schema reject fixtures differ from a passing baseline in one field. Mutation
controls deliberately weaken guarded rules. Each must turn a rejected witness
into acceptance or produce a specifically named wrong outcome; an unrelated
refusal or exception is not success. The local refusal vocabulary is extracted
from the checker and must equal the standard table and exercised local reasons.
Inherited v0 reasons remain covered by the v0 controls. The fixture inventory
is checked separately for exact membership.

The checker reuses the v0 canonicalization and journal helpers. It is a logical
fixture interpreter, not an independent decoder, a persistence engine, an
access-control service or a production reference implementation. The v0 suite
continues to cover inherited rules. Production qualification additionally
requires independent interoperability, authorization, current-cut race tests,
crash/restart and key-retention tests, and restriction propagation into actual
exports and derived consumers. No store or analytics backend is qualified by
this bundle.
