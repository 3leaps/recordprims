# Durable Record v1: Retained Records with Use Restrictions

**Status: proposed, experimental.** Capability: `contract: durable-record/v1`.
Schemas: [`schemas/durable-record/v1`](../../schemas/durable-record/v1/).

## Version boundary

This family adds `tombstoned` and `untombstoned` journal entries. Adding entry
types is a breaking change under the [v0 standard](durable-record-contract.md),
so this is v1. `contract: durable-record/v0` is deprecated (`"status": "deprecated"`,
`"superseded_by": "contract: durable-record/v1"` in
[`schemas/durable-record/v0/contract.json`](../../schemas/durable-record/v0/contract.json))
for new stream production; new durable-record producers MUST target
`contract: durable-record/v1`. The v0 schemas, fixtures, and validator checks
remain in the repository because v1 stream continuations (`continues`,
`imported_from`) and multi-version store readers still verify historical v0
source streams and ancestor journals. Repository release numbers and contract
versions are independent; an experimental v1 contract does not imply a stable
implementation.

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

## Wire numeric domain and evaluation order

All contract-defined integer fields in `durable-record/v1` — and in any observed
v0 or v1 continuation source or ancestor journal — share the safe-integer upper
bound `9007199254740991` ($2^{53} - 1$), keeping each field's `minimum` or
`const`:

- `record.schema.json`: `v` (`1`), `revision`, `supersedes.revision`,
  `body.length`, `links[].revision`, and `$defs.pin.revision`;
- `journal-entry.schema.json`: `v` (`1`), `seq`, `group`, `group_size`,
  `revision`, `presented.revision`, `current.revision`, `destination_cut_seq`,
  `incarnation`, `continues.cut_seq`, `recovery.cut_seq`,
  `recovery.reconciled_bodies[].floor` (`0`..`9007199254740991`),
  `source.cut_seq`, `imported_from.pin.revision`, `body.length`, and embedded
  `record` and `request` integer paths;
- `write-receipt.schema.json`: `receipt.v` (`1`), `receipt.seq`,
  `receipt.pin.revision`, `receipt.current.revision`, `reconciliation.v` (`1`),
  and embedded `receipt` or `protectionReceipt` integer paths;
- `protection-request.schema.json` and `protection-receipt.schema.json`: `v`
  (`1`), `target.revision`, `body.length`, `restores`, and `seq`;
- `export-request.schema.json` and `export-result.schema.json`: `v` (`1`),
  `protection_cut`, `current_cut`, `selection[].revision`, and
  `omitted[].revision`;
- `body-state-request.schema.json`: `body.length`;
- `removal-eligibility-request.schema.json`: `body.length`, `incarnation`,
  `authorization.body.length`, `authorization.incarnation`, and embedded
  `observed_streams[]` journal and source-journal integer paths;
- `stream-seal.schema.json`: `v` (`1`), `cut_seq`, and
  `ancestor_seals[].cut_seq`;
- `stratum-profile.schema.json`: `runtime.clock_skew_bound_ms` and
  `runtime.idempotency_window_ms`.

As in [v0](durable-record-contract.md#wire-numeric-domain-and-evaluation-order),
this ceiling is an explicit `durable-record` interoperability tightening per
[RFC 7493 Section 2.2](https://www.rfc-editor.org/rfc/rfc7493#section-2.2) and
[RFC 8785 Appendix B](https://www.rfc-editor.org/rfc/rfc8785#appendix-B), not a
claim that RFC 8785 forbids larger binary64 values in general. Arbitrary
application `record.payload` numbers are not restricted to integers or to this
ceiling. Implementations with `int64` or `uint64` internals MUST enforce the
wire ceiling at API, import and journal boundaries before emitting, hashing or
incrementing; counter or atomic-group exhaustion (`seq`, `revision`,
`incarnation`, `group + group_size - 1`) refuses without wraparound.

Evaluation follows the four-stage order: strict JSON ingestion (`ingest`,
including NDJSON line framing where every serialized entry line of byte length
`L` including its terminating LF `0x0A` MUST end with a single `0x0A` without
CR `0x0D`; any partial line write `0 <= k < L` — including `k = L - 1` missing
only the trailing `0x0A` — or any blank line refuses as `ingest`; forbidding
`0x0D` across all `v0` and `v1` NDJSON inputs is an explicit framing tightening
in this revision) ->
exact protocol-integer raw-token preflight (`numeric_domain`, bounded without
expanding huge exponents; accepting `12`, `12.0`, `1.2e1`, `0.0`, `-0.0` where
`minimum` permits; rejecting fractional tails such as `12.0000000000000001`,
underflow `1e-400`, and magnitudes above `9007199254740991`; passing non-numeric
types and safe negative integers through to field `type`/`minimum` checks) ->
schema validation (`shape`) -> logical semantics. Direct typed APIs enforce the
same permitted range, while raw-byte checks certify wire tokens before binary64
conversion. An observed v0 or v1 source or ancestor journal failing any of these
four stages (including v0 Stage 4 `timestamp` validation on every entry `at` in
the full observed journal, whether at or beyond `cut_seq`) supplies no
restriction state and refuses the relying continuation as `continuation`.

Because the pinned schema validator (`goneat`) does not implement arbitrary
JSON Schema Draft 2020-12 vocabularies or sibling assertions alongside `$ref`,
`durable-record` schemas are restricted to a finite Draft 2020-12 keyword subset
enforced at schema qualification: `$ref` targets only local `#/$defs/<name>`
definitions and carries no sibling assertion keywords (`pattern`, `minLength`,
`maxLength`, `format`, `enum`, `const`, `minimum`, `maximum`, etc. alongside
`$ref` are forbidden; constraints belong on the `$defs` target or inside
`allOf`), and `format` is restricted to `"date-time"`. This subset admission
qualifies schema structure; exact `format: "date-time"` and Stage 4 timestamp
staging are pinned by the per-position stage trace table below.

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
uppercase `T` and `Z`, ASCII digits `0`–`9` (`0x30`–`0x39`) only, four-digit
year `0001` through `9999` inclusive (`0000` is refused), month `01` through
`12`, proleptic Gregorian day-of-month valid for `(year, month)` (February `29`
valid only when `year % 4 == 0 and (year % 100 != 0 or year % 400 == 0)`), hour
`00` through `23`, minute `00` through `59`, second `00` through `59` (`:60`
leap seconds are unconditionally refused), period `.`, and exactly six
fractional digits (`000000` through `999999`). This covers journal `at`,
`observed_at` (in both `protection-request.schema.json` and embedded
`journal-entry.schema.json`), `produced_at.instant` (in both
`record.schema.json` and embedded `journal-entry.schema.json`), reconciliation
`attempted_at`, reconciliation coverage endpoints (`window_covers_from`,
`window_covers_until`), removal-eligibility instants
(`removal-eligibility-request.decision_time`,
`removal-eligibility-request.authorization.not_before`, and
`removal-eligibility-request.authorization.expires_at`), and detached stream
seal `sealed_at` (`stream-seal.schema.json`). Emitters MUST use this
spelling, and readers MUST NOT normalize non-canonical spellings: a digest
covers the bytes the producer wrote, so a store that rewrote `observed_at` or
`produced_at.instant` would invalidate the caller's digest. An instant that is
not exactly representable at microseconds is refused, never rounded. In
protection request, removal-eligibility, and standalone stream-seal evaluation,
canonical instant spelling (`observed_at`, `decision_time`, `not_before`,
`expires_at`, `sealed_at`) is checked at Stage 4 before operation-level target,
hold, sequence, Precedence 1–5, or seal finality checks.

Under the repository's published validator profile (`goneat` `v0.6.8`,
Draft 2020-12 with `$defs/instant` carrying both
`pattern: "^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-5][0-9]\\.[0-9]{6}Z$"`
and `format: "date-time"`, verified on every run by
`scripts/validate-durable-record-fixtures.py` and
`scripts/test-durable-record-v1-controls.py`, followed by the Stage 4 semantic
evaluator), each timestamp position has a deterministic refusal stage:

| Timestamp position                                                                                                                                                                                                                                                                                                                                                                                                       | Non-string, wrong length/fraction width, lowercase `t`/`z`, numeric offset (`+00:00`/`-00:00`), or non-ASCII digits (`２０２６-...`)                                                                                                                                                                                               | Impossible calendar date (`2026-02-30T12:00:00.000000Z`) or leap second (`2016-12-31T23:59:60.000000Z`)                                                                                                                          | Year zero (`0000-01-01T00:00:00.000000Z`)                                           |
| ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------- |
| Destination `v1` schema-gated input (`journal-entry.at`, `record.produced_at.instant`, `protection-request.observed_at`, `reconciliation.attempted_at`, `reconciliation.window_covers_from`, `reconciliation.window_covers_until`, `removal-eligibility-request.decision_time`, `removal-eligibility-request.authorization.not_before`, `removal-eligibility-request.authorization.expires_at`, `stream-seal.sealed_at`) | Stage 3 `shape` (`pattern` and `format: "date-time"`; Stage 4 `instant_spelling` as defense-in-depth)                                                                                                                                                                                                                              | Stage 3 `shape` (current seconds pattern for `:60`; qualified `date-time` format for calendar validity); Stage 4 `instant_spelling` for direct semantic checks and calendar checks when format is disabled                       | Passes qualified `goneat` Stage 3 `shape`; refused at Stage 4 as `instant_spelling` |
| Observed `v1` continuation source, ancestor journal, or detached stream seal (`at`, `record.produced_at.instant`, `request.observed_at`, `seal.sealed_at`)                                                                                                                                                                                                                                                               | Refuses relying continuation as `continuation` (Stage 3 `shape`; Stage 4 `instant_spelling` as defense-in-depth)                                                                                                                                                                                                                   | Refuses relying continuation as `continuation` (Stage 3 `shape` with the current seconds pattern for `:60` or qualified format for calendar validity; Stage 4 `instant_spelling` for direct and format-disabled calendar checks) | Refuses relying continuation as `continuation` (Stage 4 `instant_spelling`)         |
| Observed `v0` continuation source or ancestor journal (`at` across `1..end`, including entries beyond `cut_seq`)                                                                                                                                                                                                                                                                                                         | Refuses relying continuation as `continuation` (Stage 3 `shape` for non-string, non-ASCII digits, bare date, comma fraction, 1-digit hour, or bad offset under `v0.6.8` `format: "date-time"`; Stage 4 `timestamp` for `>6` nonzero fractional digits and UTC year underflow/overflow, with spelling checks as defense-in-depth) | Refuses relying continuation as `continuation` (Stage 3 `shape` / Stage 4 `timestamp`)                                                                                                                                           | Refuses relying continuation as `continuation` (Stage 4 `timestamp`)                |

Stage 4 independently enforces the complete ASCII-only `instant_spelling`
grammar, proleptic Gregorian calendar, year `0001`..`9999`, and `:60` refusal on
every `v1` position (including received reconciliation `attempted_at`,
`window_covers_from`, and `window_covers_until`) as defense-in-depth, and the
control suites include explicit Stage 3 + Stage 4 fallback controls proving
that `:60` and non-ASCII digits are rejected by the current pattern even when
`format: "date-time"` is disabled. Impossible calendar dates still require the
format evaluator or the Stage 4 calendar check; the pattern alone cannot
validate the calendar. The historic schema relied on the qualified formatter to
refuse `:60` at Stage 3. The earlier qualified `v0.6.5` formatter refused `:60`.
The separate trial module `v0.6.8-0.20261110123243-512d7e6a2bbd` reported
`v0.6.7`, but its formatter accepted `:60`; the amended seconds pattern refused
it. The published `v0.6.8` formatter also admits `:60`, and the current seconds
constraint preserves Stage 3 refusal. Under `v0.6.5`, the v0 bare-date, comma,
one-digit-hour and bad-offset cases reached Stage 4. The published build's
identity, reported engine profile and schema-byte digests are recorded in the
[v0 timestamp profile](durable-record-contract.md#timestamp-validation-profile-v0).

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
current again if no release occurred while the state differed. A sealed source,
read-only at the reviewed cut, makes the source-state check final (and bounds
upstream ancestor walks) only when verified under the detached stream-seal
rules below.

**Verified stream seals and source finality (`stream-seal.schema.json`).** This
contract standardizes one authoritative stream-seal representation: a detached
JSON artifact validated by `schemas/durable-record/v1/stream-seal.schema.json`
(registered as `"stream_seal": "stream-seal.schema.json"` in `contract.json`)
with a canonical RFC 8785 JCS payload. There is no dual authoritative in-stream
and sidecar seal form; a sealed stream's journal lines remain unmodified `v0`
or `v1` journal entries.

The canonical seal object binds:

- `v: 1`, `store` (non-empty store identifier preventing cross-store seal
  replay), `capability` (`contract: durable-record/v0` or
  `contract: durable-record/v1`), `stream`, `cut_seq`, and `cut_digest`;
- `effective_restriction_manifest_digest` at `1..cut_seq`;
- `ancestor_seals`: array of `{store, capability, stream, cut_seq,
  seal_digest}` ordered strictly by the RFC 8785 JCS bytes of its member
  objects with no duplicates (`seal_digest` is `sha256:` of the RFC 8785 JCS
  bytes of that ancestor's `stream-seal` object), covering every upstream
  continuation boundary when `stream` is itself a continuation, or `[]` when
  `stream` is not a continuation;
- `trust_profile: {id, version}`, `sealed_at` (canonical `v1` UTC microsecond
  instant), `provenance`, and `authorization: {ref, digest}`.

`effective_restriction_manifest_digest` uses the exact derivation defined above
for `source_state_digest`: `sha256:` followed by lowercase hex SHA-256 of the
RFC 8785 JCS bytes of `{holds, retired, inherited}` at `1..cut_seq`, with each
array sorted by the RFC 8785 JCS bytes of its member objects without
duplicates:

- Empty manifest `{"holds":[],"inherited":[],"retired":[]}` ->
  `sha256:5cb72fd9337154074dc353b97f04c8dc84bba51bf1aa1f73db49a14dd890b93c`;
- Holds-only `{"holds":[{"capability":"contract: durable-record/v1","request_digest":"sha256:df554395d9e891a5696d72a0ca02b8b8fb4ad601908d800ca346d7d1dd6aa895","seq":3,"stream":"records.src"}],"inherited":[],"retired":[]}` ->
  `sha256:35c34957ca90009b9620e891ca531b83889a0c34157366d2a58f910b6971e717`;
- Retired-only `{"holds":[],"inherited":[],"retired":[{"capability":"contract: durable-record/v1","record_id":"rec-a","stream":"records.src"},{"capability":"contract: durable-record/v1","record_id":"rec-b","stream":"records.src"}]}` ->
  `sha256:c32ec233b1b9b4c7dacbbfa2acd61b89e2ea3d562e6c0671be95642490d419e6`;
- Continuation with `inherited` `{"holds":[{"capability":"contract: durable-record/v1","request_digest":"sha256:03d65999e6f577305caab25264a500a4e1c756c4d74235cc3ae00eb02d3c2930","seq":4,"stream":"records.mid"}],"inherited":[{"capability":"contract: durable-record/v1","source_state_digest":"sha256:5cb72fd9337154074dc353b97f04c8dc84bba51bf1aa1f73db49a14dd890b93c","stream":"records.root"}],"retired":[{"capability":"contract: durable-record/v1","record_id":"rec-m1","stream":"records.mid"}]}` ->
  `sha256:9658ed015fa868e3f17f5f034d006c6102f6ff98d3f9dcbe109b908d235ddee1`.

A JSON file on disk, timestamp, filesystem read-only bit, or consumer-pinned
`cut_digest` / seal digest alone NEVER proves finality: a consumer pin
authenticates historical prefix bytes `1..cut_seq`, not that no still-authorized
writer has appended a later hold or retirement to the source or an upstream
ancestor beyond the consumer's snapshot. A seal makes a source's restriction
state at `cut_seq` final (and bounds an ancestor walk above an unobserved live
upstream journal) only when **all** of the following hold:

1. **Authenticated closer authority (`closer_authenticated: true`), trust
   profile match & `store` identity match:** The verifier authenticates the
   closer's `authorization` under an accepted `trust_profile: {id, version}`,
   and `seal.store` equals the expected store identity for the source stream
   (mismatch refuses as `continuation`).
2. **Independently verified permanent source write and protection fencing
   (`source_write_fenced: true`):** The source stream is permanently closed and
   fenced against any subsequent content or protection writes (`tombstoned`,
   `untombstoned`, `retired`) by verified store or authority closure bound to
   the seal's trust profile. When `closer_authenticated` is `true` and
   `cut_digest` matches the historical prefix `1..cut_seq`, `source_write_fenced:
   false` establishes **no** finality exemption (unobservable live source or
   ancestor state fails closed as `protection_unavailable`, and live state
   changes fail closed as `restrictions_pending`).
3. **Exact cut match, manifest match & zero trailing bytes:** The observed
   source journal validates under its declared capability, has a complete
   atomic group at `1..cut_seq` hashing to `cut_digest`, has derived effective
   restriction manifest digest for `1..cut_seq` equal to
   `effective_restriction_manifest_digest`, and has **zero raw trailing bytes**
   (`trailing_bytes == ""`, refusing any unterminated or damaged tail as
   `continuation`). When `source_write_fenced` is `true`, the observed source
   journal MUST have exact length `cut_seq` with zero entries beyond `cut_seq`
   (any entry beyond `cut_seq` violates the claimed write fence and refuses as
   `continuation`); when `source_write_fenced` is `false`, valid committed
   entries beyond `cut_seq` leave the seal non-final under condition 2 so live
   source changes fail closed as `restrictions_pending`.
4. **Complete transitive ancestor closure:** When the sealed source is itself a
   continuation, `ancestor_seals` MUST record the **exact transitive closure**
   of upstream boundaries above it (strict JCS byte order, unique `(capability,
   stream)` targets, no cycles, no conflicting boundary tuples, and no
   unrelated stream entries; structural, conflict, or padding violations refuse
   as `continuation`), and every upstream boundary at every depth MUST be
   permanently closed and fenced (`closer_authenticated: true` and
   `source_write_fenced: true`) by a verified seal (`cut_seq ==
   review.source.cut_seq` and `effective_restriction_manifest_digest ==
   review.source_state_digest` at each sealed continuation hop). When a
   manifest preimage is supplied for a sealed boundary, it MUST have the closed
   `{holds, retired, inherited}` object shape and canonical JCS digest matching
   `effective_restriction_manifest_digest` (mismatch refuses as `continuation`).
   A local or intermediate seal MUST NOT hide an unsealed, missing, or unfenced
   (`source_write_fenced: false`) deeper upstream ancestor.

In the logical vector form, seal verification supplies the detached `seal`
object alongside explicit trusted verifier inputs (`expected_store`,
`accepted_trust_profile: {id, version}`, `closer_authenticated`,
`source_write_fenced`, optional raw `trailing_bytes`, and `ancestor_seals`
contexts) and grades every Stage 1–4 rule deterministically.

The source-state check and the destination's current-cut release decision
MUST be held together through release, serialized with protection operations
on both streams: a source hold accepted between the check and the release
blocks it. The logical interpreter decides at one observation and does not
qualify that serialization.

## Body lifecycle and removal

A store whose profile declares `deletion.external_bodies: supported` records the
lifecycle of the bytes held by body references in one ordinary v1 stream, its
**body-removal stream**, named by the profile's `runtime.body_removal_stream`.
The profile MUST name that stream when `external_bodies` is `supported` and
MUST NOT name one otherwise. Bytes are shared by every stream that references
them, so their lifecycle is recorded once for the store, not in each
referencing stream. The identifier is an ordinary stream identifier chosen by
the store; no literal is reserved.

The body-removal stream carries only `stream_opened` and the three lifecycle
entry types below, and is not an ordinary content continuation (`continues` on
its `stream_opened` is refused as `body_removal_stream`). Its `stream_opened`
MAY carry `recovery` only when activating a designated-stream recovery successor
under `runtime.incarnation_allocation: "prewrite_durable_ledger"`; `recovery` on
any non-designated stream, or when `incarnation_allocation` is omitted or
`"effect_dependent"`, is refused as `body_removal_stream`, and any lifecycle
entry in a non-designated stream or non-lifecycle entry in the designated stream
is refused as `body_removal_stream`. Every lifecycle entry is its own atomic
group (`group` equals `seq`, `group_size` 1).

A lifecycle entry names its body as `body: {digest, length}`, the digest and
byte length of the body reference, with no media type, encryption or retrieval
hint, and an `incarnation`. An **incarnation** is one publication of those
bytes: a positive integer, strictly increasing per `{digest, length}` within
the store. Gaps are permitted, and the first durable publication need not be
`1`, because a store may advance its counter before a publication that is then
interrupted. `at` is the store's acceptance time under the ordinary v1 rules.

| Entry                  | Written                                                | Carries                                                                 | Means                                                                |
| ---------------------- | ------------------------------------------------------ | ----------------------------------------------------------------------- | -------------------------------------------------------------------- |
| `body_published`       | after the bytes are durable                            | `body`, `incarnation`                                                   | Incarnation `n` is published; this is publication's commit point     |
| `body_removal_started` | durable before any byte of incarnation `n` is unlinked | `body`, `incarnation`, `reason`, `provenance`; optional `authorization` | The store decided to remove incarnation `n`; not evidence of removal |
| `body_removed`         | after the store observed the bytes absent              | `body`, `incarnation`                                                   | Incarnation `n` is removed; `at` is completion time                  |

`reason` on `body_removal_started` is one of `retention`, `operator` or
`rights_restricted`; it attributes a decision and is never a bypass of any
rule. `authorization` is an opaque reference and digest, the same shape as a
restriction review's. `reason`, `provenance` and `authorization` appear only on
`body_removal_started`.

**Transitions.** For one `{digest, length}`, the latest incarnation is the one
named by its latest `body_published`. A reader MUST refuse as `body_lifecycle`:

- a `body_published` whose incarnation is not greater than every incarnation
  already named for that body;
- a `body_removal_started` that does not name the latest incarnation, or whose
  latest incarnation already has a removal started or completed: removal after
  completion requires a new publication;
- a `body_removed` that does not name the latest incarnation and its open
  `body_removal_started`.

A `body_published` for a body whose latest incarnation has an open removal
closes that removal as **abandoned**: the store republished the bytes before
completing the removal. Abandonment is not an error. A completion, or a retry,
for an older or abandoned incarnation is refused and cannot act on the newer
one. A publication retry that finds the latest publication committed, its
bytes verified available and no removal pending appends nothing, and so does
repeating a completed removal: that is the store's operation idempotency. A
publication that restores lost bytes is a new publication with a greater
incarnation, not a retry. A duplicate lifecycle entry in the journal is an
invalid transition, not the same as a fresh publication. Lifecycle entries
carry no idempotency key.

**Fencing.** Publication, removal and republication of the same body MUST be
serialized or fenced by incarnation through their physical effects and their
lifecycle commit points. After a later incarnation is published, an older or
abandoned removal MUST NOT unlink its bytes or append a completion. The
eligibility check and the unlink MUST be protected against intervening
references and protection operations. A retry MUST re-establish eligibility
and the current incarnation before any further physical removal. A
`body_removal_started` entry alone authorizes neither action. An absence the
store has already observed MAY be recorded as completion of the matching open
removal without a fresh unlink; recording it authorizes no further removal.

**Eligibility.** Lifecycle entries establish neither the authority for a
removal nor its eligibility. Physical removal of `body: {digest, length}` is
evaluated per exact `{digest, length}` pair (never by record pin identity alone;
the same SHA-256 digest with a different byte length is isolated) at a
**consistent store-wide decision cut**, under the store-wide reference and
protection exclusion fence held continuously across the eligibility check and
the physical unlink.

- **Scope boundary (all-retired-record reclamation):** Because `retired` in `v1`
  applies to an entire `record_id` rather than individual revisions, reclaiming
  referenced bytes applies strictly when **every** referring record across
  **every** sharing stream in the store is `retired`. An older (superseded)
  revision of an unretired (active) record remains part of retained history and
  continues to reference its body bytes even when a newer revision of that same
  record references a different body or no external body; per-revision
  reference release (`keep-N` revisions of an active record) and destruction of
  held bytes while preservation holds remain active are outside this contract.
  When a metadata-only revision reuses an earlier revision's `{digest, length}`,
  retiring the record retires all of its revisions together. A store's local
  policy admitting only same-store, `v1`-only continuation sources is a
  stream-open admission rule, not a modification to `v1` continuation syntax.
- **All-retired-record eligibility conditions:** After completing the four-stage
  wire/numeric-domain/shape (`removal-eligibility-request.schema.json`) and
  canonical `instant_spelling` preflight (`decision_time` and, when
  `authorization` is supplied, `authorization.not_before` and
  `authorization.expires_at`), physical removal of `body: {digest, length}` MAY
  proceed only when, at a consistent store-wide decision cut:
  1. Target `incarnation` is the current removable incarnation for
     `body: {digest, length}` in the valid designated
     `runtime.body_removal_stream` (with protocol integers validated in the
     exact safe-integer domain excluding booleans): for an initial start
     (`mode: "start"`), the latest entry for `body` is `body_published(n)` with
     `n == incarnation`; for a retry unlink with bytes still present
     (`mode: "retry_unlink"`), the latest entry for `body` is
     `body_removal_started(n)` with `n == incarnation` (not completed and not
     abandoned/superseded by a later `body_published(n')`).
  2. Complete store-wide stream membership (`store_streams`, with no duplicate
     stream names and disjoint from `runtime.quarantined_designated_streams`),
     exact `runtime.quarantined_designated_streams` predecessor-chain equality
     when recovery is active, and valid consistent cuts are established for
     every stream in the store, excluding **only** the exact designated
     `runtime.body_removal_stream` named in the store profile, and full
     continuation ancestry (`source_journal` plus all deeper `ancestors`, unless
     bounded above a verified sealed boundary) is validated under each hop's
     declared `v0`/`v1` family. On each observed sharing stream,
     `inherited_block` is a required boolean when that stream's validated
     `stream_opened` carries `continues` and is forbidden on non-continuation
     streams (absent required or non-boolean values are never treated as
     `false`). Any stream omitted from the observed cuts, duplicated, overlapping
     or mismatching `quarantined_designated_streams`, unopened, at a damaged or
     incomplete cut, disagreeing with a same-store source journal, missing or
     mis-typed on `inherited_block`, or having unobservable, damaged, or
     incomplete continuation source/ancestor state is **not** an empty reference
     set and fails closed as `protection_unavailable`.
  2b. **Decision-time cut consistency (`journal_time`):** Across the validated
     designated `runtime.body_removal_stream` cut and every validated same-store
     sharing stream cut established at Precedence 2 (for both `start` and
     `retry_unlink`), `decision_time` MUST be greater than or equal to every
     accepted journal entry `at` in those cuts (equivalently each cut's final
     entry `at`, since accepted per-stream `at` is nondecreasing; equality is
     valid, and producer `record.produced_at` or cross-store external source
     clocks are not compared here). A `decision_time` predating any accepted
     same-store cut entry refuses as `journal_time` before protection,
     active-reference, or authorization checks; a cut containing a retirement or
     state transition stamped after `decision_time` cannot justify reclamation at
     that earlier instant.
  3. No active hold (`tombstoned` without `untombstoned` covering any
     referencing record, revision, or the body itself) and no active or pending
     inherited continuation restriction applies in any sharing stream
     (`protected_use`). Because `restrictions_reviewed` verifies manifest digests
     rather than proving transitive pin-mapping completeness, this rule is
     deliberately **store-wide conservative** (including for unreferenced orphan
     cleanup): `inherited_block: true`, a missing or non-current
     `restrictions_reviewed` anywhere in the continuation chain, or **any**
     still-active `tombstoned` hold (not lifted by `untombstoned`, across
     `record`, `revision`, or `body` scopes) in any observed `source_journal` or
     deeper `ancestors` journal of any sharing stream in the store refuses
     `protected_use` even when `inherited_block` is `false`, `restrictions_reviewed`
     is current, or that continuation stream has no local target-body reference
     (when an upstream ancestor is bounded by a verified `stream-seal` rather
     than observed live, zero active holds on that ancestor must be proven by
     the empty `effective_restriction_manifest_digest` or a verified manifest
     preimage with `holds: []`, refusing `protected_use` otherwise; source
     retirement alone is not a hold, and `use`/`serve` semantics on unimported
     source holds remain unchanged).
  4. **Every** record (`record_id`) that has **any** revision (current head or
     superseded historical revision) referencing `body: {digest, length}` across
     **every** sharing stream in the store is `retired` at that stream's
     decision cut; if any unretired record in any sharing stream references
     `body: {digest, length}` in any revision, removal is refused as
     `still_referenced`. Re-recording a source record via `imported_from` in a
     continuation stream neither transfers ownership nor extinguishes the
     source reference: when a continuation stream and its source/ancestor share
     the store's physical bodies, every referencing source/ancestor record and
     every re-recording destination record must be `retired`. An external
     continuation source that does not share the store's physical bodies does
     not create a local body reference, but its observable ancestry and
     conservative active-hold / currency checks on any local continuation stream
     still dominate (`protection_unavailable` at Precedence 2 when unobservable
     or malformed; `protected_use` at Precedence 3 when any source/ancestor hold
     is active, `inherited_block` is `true`, or review is pending).
  5. Whenever the referenced record set across the store is non-empty (all
     referencing records retired), a verified removal `authorization` covering
     `{store, body: {digest, length}, incarnation, policy: {id, version},
     action: "remove_body", scope_verified: true, not_before, expires_at}` is
     present, matches the decision context's `expected_policy: {id, version}` on
     both `id` and `version`, and is temporally valid (`not_before <=
     decision_time <= expires_at`), failing closed as `removal_authorization`
     otherwise. Unreferenced orphan bytes—where complete store-wide coverage and
     conservative protection gates at Precedence 2–4 prove zero records have
     ever referenced `{digest, length}`—are eligible without triggering
     Precedence 5 (`authorization` remains optional on `body_removal_started` at
     the JSON Schema layer and required by this decision rule whenever bytes
     have ever been referenced).
- **Reason independence:** Eligibility is independent of `reason` (`retention`,
  `operator`, or `rights_restricted`); neither `operator` nor
  `rights_restricted` bypasses incomplete coverage (`protection_unavailable`),
  decision-time cut ordering (`journal_time`), holds or inherited restrictions
  (`protected_use`), active references (`still_referenced`), or required
  `authorization` (`removal_authorization`).
- **Deterministic refusal precedence (Precedence 1 -> 2 -> 2b -> 3 -> 4 -> 5):**
  After Stage 1–3 wire/numeric-domain/shape and Stage 4 `instant_spelling`
  preflight, both initial start and retry re-verification before unlink MUST
  evaluate simultaneous faults in the following deterministic order:

  | Precedence | Stage / Check | Refusal Code |
  | --- | --- | --- |
  | 1 | Designated-stream placement and journal validity, closed request container/domain checks, plus target `incarnation` transition check against the designated stream's current state (`body_lifecycle` when not published, already removed, or abandoned by `body_published(n')`) | `ingest` -> `numeric_domain` -> `shape` -> `instant_spelling` -> `body_removal_stream` -> `body_lifecycle` |
  | 2 | Store-wide stream membership completeness (no duplicates, disjoint from and matching `quarantined_designated_streams`), valid per-stream cuts on every non-designated stream, valid continuation `inherited_block` boolean presence/absence, and complete observable `v0`/`v1` continuation `source_journal` + `ancestors` chain | `protection_unavailable` |
  | 2b | Decision-time consistency over the validated cuts: `decision_time >=` accepted journal `at` on the designated stream and every observed same-store sharing stream | `journal_time` |
  | 3 | Active local `record`, `revision`, or `body` holds (`tombstoned`), explicit `inherited_block: true`, non-current `restrictions_reviewed`, or any still-active `tombstoned` hold in any observed `source_journal` or deeper `ancestors` journal of any sharing stream | `protected_use` |
  | 4 | Any unretired record referencing `body: {digest, length}` in any revision (latest or superseded) across any sharing stream (including same-store continuation sources) | `still_referenced` |
  | 5 | Non-empty referenced set (all referencing records retired) with missing, malformed, wrong-`store`, wrong-`body`, wrong-`incarnation`, wrong-`policy` (`id` or `version` vs `expected_policy`), wrong-`action`, unverified-scope, not-yet-valid, or expired `authorization` at `decision_time` | `removal_authorization` |

- **Authorization verifier obligations & retry re-verification:** Presence of
  `{ref, digest}` alone establishes nothing. The store or verifier MUST validate
  the authorization against target `store`, `body: {digest, length}`, and
  `incarnation`, retention `policy: {id, version}` matching the decision
  context's `expected_policy: {id, version}` and permitted `action:
  "remove_body"`, authenticated scope over the decision cuts (re-evaluating
  broader policy scopes against the actual consistent store-wide decision cut at
  execution time), and temporal validity `[not_before, expires_at]` at
  `decision_time`. An existing `body_removal_started(n)` entry never freezes
  eligibility: any retry with bytes present (`retry_unlink`) MUST re-run
  Precedence 1 -> 2 -> 2b -> 3 -> 4 -> 5 in full under exclusion (and
  implementations MUST test the check-to-unlink race window via a dedicated
  `remove.checked` faultpoint seam). In the logical vector form
  (`removal-eligibility-request.schema.json`, where nested ancestry is supplied
  ordered from immediate `source_journal` through `ancestors[0], ancestors[1],
  ...` to the root stream and validated from root downward), the
  `removal_eligibility` step grades this precedence over trusted store-derived
  inputs and decomposed `authorization` claims at a pinned `decision_time`; it
  does not certify physical exclusion.

**Reader states.** A body's state is read from the designated stream's
complete, valid journal at a consistent cut, together with an observation of
the bytes that agrees with that cut, made under the store's exclusion or an
equivalent consistent protocol. Present means the bytes were read and verified
against both digest and length; absent means the bytes were observed not to be
there. A failed read, corrupt bytes or unknown availability is neither, and
fails the observation; in the logical form a malformed observation is refused
as `body_observation` before any state is derived. A stale cut combined with a newer observation
establishes no state.

| Latest lifecycle entry for the body | Bytes present     | Bytes absent      |
| ----------------------------------- | ----------------- | ----------------- |
| none                                | `publishing`      | `unpublished`     |
| `body_published(n)`                 | `available`       | `integrity_loss`  |
| `body_removal_started(n)`           | `removal_pending` | `removal_pending` |
| `body_removed(n)`                   | `publishing`      | `removed`         |

`publishing` means bytes are present whose publication has no committed
`body_published`; it is not evidence that a publication is in progress.
`unpublished` requires a valid designated stream at a complete cut with no
lifecycle entry for the body: a missing, unavailable or invalid journal is not
an empty history. It is an observation, not permission to serve missing bytes,
and it does not waive an earlier verified-write or durability promise.
`removal_pending` with the bytes absent is a removal the store has not
confirmed; the store completes it by appending `body_removed`. It is neither
integrity loss nor proof of removal. A `body_removed` explains absence only for
its own incarnation: absence after a later `body_published` is
`integrity_loss`.

Ordinary schema, journal and group refusals precede lifecycle refusals: a
damaged or incomplete designated journal is refused for that defect and yields
no state, never an empty history. Placement (`body_removal_stream`) is checked
before transition (`body_lifecycle`).

No cross-stream order is required between `body_published` and the content
entries that reference the body. Lifecycle does not change any restriction: a
removed body does not retire, release or restrict a record, and no
`full_content` export may succeed without every required body.

**Incomplete-append abort and torn-tail boundary.** A single-writer store MAY
abort a provably incomplete lifecycle append in-process only when every
requirement of the completion gate below holds:

1. **NDJSON trailing-newline completeness rule:** Every serialized NDJSON
   journal entry line MUST be terminated by `\n` (`0x0A`) with no `\r` (`0x0D`).
   Let `k` be the number of bytes written in an append attempt and `L` be the
   serialized entry's byte length including its mandatory trailing `\n`. Stage 1
   (`ingest`) owns raw NDJSON line framing as well as strict RFC 8259 JSON
   syntax. A final line without a trailing `\n` — including a write of
   `k = L - 1` bytes that wrote the closing `}` of the JSON object but omitted
   `\n` — is an **incomplete entry** and MUST be refused as `ingest` on reopen
   if not truncated (a reader MUST NOT accept an unterminated final line as
   `cut_seq + 1`).
2. **Continuous reader-and-writer exclusion & prior-cut recording:** The store
   MUST hold continuous exclusion across **all** observers (concurrent readers
   as well as writers), caches, and dependent actions from before the append
   attempt through the abort decision, the truncate, the file and namespace
   sync, the `fstat` size check, and the re-validation of the prior complete
   cut. A concurrent reader MUST observe only the verified prior cut or
   refuse — never uncommitted bytes. A lost lock or broken exclusion at any
   point during the attempt makes the outcome indeterminate and poisons the
   stream. Before writing, the writer records the exact byte offset and
   `cut_seq` of the **verified complete prior cut** (`1..cut_seq`).
3. **Closed `(bytes_written k, errno)` abortability rule:**
   - **Abortable (provably incomplete write):** `0 <= k < L` (including
     `k = L - 1` missing only the trailing `\n`) where the append call failed
     before writing the complete `\n`-terminated entry (for example `ENOSPC`,
     `EDQUOT`, or a short write with known byte count `k`) and no media/device
     corruption error (`EIO`) or lost exclusion occurred.
   - **Must poison / stay fail-closed (indeterminate commit — MUST NOT
     truncate):**
     - A **full-length write (`k == L`) followed by a failed `fsync`** (an
       indeterminate commit: lack of a returned receipt does not establish
       abortability, and the store MUST poison without truncating);
     - An **uncertain write count**, an I/O error (`EIO`) where underlying media
       state is indeterminate, or **lost exclusion**;
     - Any damaged or incomplete tail (including `k = L - 1` without trailing
       `\n`) discovered on **reopen / restart** (an in-memory prior offset does
       not survive restart and MUST NOT be used as repair authority; no
       in-place reopen tail-truncation is permitted, and recovering a damaged
       designated stream requires activating a separately typed recovery
       successor under `stream_opened.recovery` below).
4. **Full abort completion gate:**
   - **Same-file truncate identity:** The truncate MUST act on the journal file
     itself, proven at abort time (when an OS such as Windows denies `truncate`
     on an `O_APPEND` descriptor and requires a second non-append handle, the
     store MUST verify `SameFile` identity between the append handle and the
     truncate handle before truncating, writing nothing on the second handle).
   - **Truncate and durable sync:** Truncate **only** this attempt's `k` bytes
     back to the recorded prior-cut byte offset, `fsync` the journal file, and
     `fsync` the parent directory if the namespace changed.
   - **Same-file re-stat:** Verify via `fstat` on the journal handle that the
     post-truncate byte length equals the recorded prior-cut offset; any
     mismatch immediately poisons the stream (never attempt a second truncate).
   - **Re-establish and validate the prior complete cut:** Before accepting any
     subsequent operation or unlocking into normal service, re-read and
     re-validate the prior complete cut (`1..cut_seq`). Failed re-validation
     poisons the stream just as a failed `truncate` or `fsync` does.
5. **In-process poison latch & restart semantics:**
   - **In-process poison latch:** Any indeterminate commit (`k == L` + failed
     `fsync`) or any failure during the abort completion gate MUST latch the
     journal as poisoned in-process; the store MUST refuse **all** further
     appends and dependent body operations (reads, state derivations, unlinks,
     and exports) until reopen/recovery.
   - **Restart semantics:** On restart, ordinary journal validation applies: if
     an in-process rollback completed and synced cleanly before a later crash,
     the resulting clean verified prefix (`1..cut_seq`) validates normally on
     reopen and is not permanently poisoned; if a crash occurred before the
     rollback's truncate and sync completed, any remaining damaged or
     unterminated tail cannot use the lost in-memory offset as repair authority
     and fails closed (`ingest`) on reopen. Consequently, kill points around
     `truncate` and its `fsync` satisfy the **disjunctive oracle**: on reopen,
     the store MUST either accept the exact verified prior cut (`seq == cut_seq`)
     or refuse the journal as damaged (`ingest`) — never any intermediate or
     `cut_seq + 1` state.

**Incarnation allocation policy (`runtime.incarnation_allocation`).** Never
unlink bytes on an aborted `body_removal_started(n)`. Pre-existing durable body
placement on disk before an aborted `body_published(n)` remains a separately
reconciled physical state (`publishing`), never proof of publication, and no
existing bytes are deleted. Only `body_published` allocates a new incarnation
`n` in the per-body `(digest, length)` store-wide domain (`1 .. 2^53 - 1`);
both `body_removal_started(n)` and `body_removed(n)` reference the existing
published incarnation `n`, so aborting a short write of `body_removal_started(n)`
or `body_removed(n)` neither allocates nor burns an incarnation, and a retry
acts on the same published incarnation `n`.

When `deletion.external_bodies` is `"supported"`, a stratum profile MAY declare
`runtime.incarnation_allocation` as either `"effect_dependent"` or
`"prewrite_durable_ledger"` (the field is forbidden when
`deletion.external_bodies != "supported"`; when omitted on an existing profile
with `deletion.external_bodies == "supported"`, the baseline
`"effect_dependent"` rule applies):

- `"effect_dependent"` (baseline `v1` without designated-stream recovery): an
  aborted `body_published(n)` incarnation number `n` MUST NOT be reused if
  **any physical effect, reversible or irreversible durable body placement, or
  external handle/fence** could rely on `n`. Reuse of `n` after an aborted
  `body_published(n)` is permissible **only** when the store proves under
  continuous exclusion that zero physical effects or handles ever relied on
  `n`. Across `abort -> clean restart -> republish`, the new incarnation `n'`
  MUST satisfy `n' > max(committed_max, effect_relied_on_max)`.
- `"prewrite_durable_ledger"` (unconditional burn): no incarnation `n` for
  which a `body_published(n)` write was attempted is **ever** reused, even on a
  zero-byte or short write with no prior physical effect. Because a completed
  rollback leaves no trace of `n` in the journal bytes, a volatile counter
  cannot satisfy this rule: the store **MUST** persist and sync `n` in its
  pre-write durable allocation ledger before attempting the `body_published(n)`
  journal write. Across `abort -> clean restart -> republish`, the new
  incarnation `n'` MUST satisfy `n' > burned_ledger_max` (`>= n + 1`).
  Declaring `"prewrite_durable_ledger"` states the store's pre-write
  incarnation burn invariant and is a mandatory prerequisite for activating a
  designated-stream recovery successor (`stream_opened.recovery`).

In both modes, incarnation sequences permit gaps (`n' > bound` does not mandate
`prior + 1`), and counter exhaustion at `2^53 - 1` (`9007199254740991`) MUST
refuse as `numeric_domain` without wraparound.

| Faultpoint (designated stream with `stream_opened` committed at `seq = 1`) | Pre-fault committed cut (`1..cut_seq`) | Physical body bytes | Required post-fault journal, poison & reader state | Next retry / reopen rule |
| --- | --- | --- | --- | --- |
| Short write (`0 < k < L - 1`) or zero-byte write (`k == 0`, `ENOSPC`/`EDQUOT`) on `body_published(n)` under continuous exclusion | `1..cut_seq` (e.g. `stream_opened` at `cut_seq = 1`) | present | Completion gate truncates `k` bytes, syncs, `fstat`-verifies size, and re-validates `1..cut_seq`; reader observes `publishing` | Next accepted append has `seq = cut_seq + 1` (`2`); retried `body_published(n')` satisfies mode bound (`n' > max(committed_max, effect_relied_on_max)` in `effect_dependent`, `n' > burned_ledger_max` in `prewrite_durable_ledger`; gaps permitted) |
| Unterminated final line (`k = L - 1`, missing only trailing `\n`): (a) in-process `ENOSPC` under continuous exclusion; (b) process kill before truncate | `1..cut_seq` | present | (a) Clean rollback to `1..cut_seq` (`publishing`); (b) on reopen, unterminated final line refuses at Stage 1 as `ingest` (never accepted as `cut_seq + 1`) | (a) Retry commits at `seq = cut_seq + 1`; (b) stream remains fail-closed on reopen (in-memory prior offset is never used as reopen repair authority) |
| Short write (`0 <= k < L`) on `body_removal_started(n)` (no unlink performed) or on `body_removed(n)` (after committed `body_removal_started(n)` and completed unlink) | `body_published(n)` (`cut_seq = 2`) or `body_removal_started(n)` (`cut_seq = 3`) | present (`body_removal_started` abort) or absent (`body_removed` abort) | Clean rollback to `1..cut_seq`; reader observes `available` (`cut_seq = 2`) or `removal_pending` (`cut_seq = 3`) | Retry commits at `seq = cut_seq + 1` (`3` or `4`) referencing the **same** published incarnation `n` |
| Physical body placement or handle/fence relying on `n` -> aborted `body_published(n)` -> clean restart -> republish | `1..cut_seq` (`committed_max < n`) | present | Clean rollback to `1..cut_seq` before restart; `effect_relied_on_max >= n` persists across restart | Republish requires `n' > max(committed_max, effect_relied_on_max)` with `max(committed_max, effect_relied_on_max) >= n` (therefore `n' >= n + 1`) in **both** modes (`n` is never reused); exhaustion at `2^53 - 1` refuses as `numeric_domain` |
| Attempted `body_published(n)` with zero physical effect (`k == 0`) -> aborted -> clean restart -> republish | `1..cut_seq` (`committed_max < n`) | present | Clean rollback to `1..cut_seq` before restart | In `prewrite_durable_ledger` mode, durable pre-write ledger forces `n' > max(committed_max, burned_ledger_max)` with `max(committed_max, burned_ledger_max) >= n` (therefore `n' >= n + 1`; reusing `n` is refused); in `effect_dependent` mode with proven zero effect/handle reliance under continuous exclusion, reuse is permitted and gaps `n' > n` are also valid |
| Zero-byte or short write with `EIO`, uncertain byte count, or lost exclusion | `1..cut_seq` | any | Indeterminate state: MUST NOT truncate; latches in-process poison | All subsequent appends and dependent body operations (`body_state`, `use`, unlink) are refused |
| Full write (`k == L`) followed by failed `fsync` | `1..cut_seq` | any | Indeterminate commit: MUST NOT truncate; latches in-process poison | All subsequent appends and dependent body operations are refused |
| Abort completion-gate failure (`SameFile` mismatch on truncate handle, `truncate` error, post-truncate `fstat` size mismatch, `fsync` error, or failed prior-cut `1..cut_seq` re-validation) | `1..cut_seq` | any | Latches in-process poison (never attempts a second truncate) | All subsequent appends and dependent body operations are refused |
| Kill points immediately before and after `truncate` and its `fsync` | `1..cut_seq` | any | Disjunctive reopen oracle: reopen either accepts exact verified prior cut `1..cut_seq` or refuses damaged/unterminated tail at Stage 1 as `ingest` | Never yields an intermediate cut, `cut_seq + 1`, lost earlier receipt, or stale removal action |

The repository's logical control suite (`scripts/test-durable-record-v1-controls.py`)
verifies the Stage 1 NDJSON `k = L - 1` framing refusal, the
`runtime.incarnation_allocation` schema and mode bounds (`effect_dependent` vs.
`prewrite_durable_ledger`, including `effect_relied_on_max`, `burned_ledger_max`,
permitted gaps, and `numeric_domain` exhaustion), and the logical abort,
completion-gate, poison-latch, and disjunctive-reopen state transitions with AST
mutation controls; OS/storage-level fault injection (`O_APPEND` `SameFile`
descriptor identity, physical `fsync`/`fstat` syscalls, and concurrent lock
exclusion) is an implementation-level store qualification obligation.

**Designated body-removal stream recovery (`stream_opened.recovery`).** A
damaged or torn designated `body_removal_stream` MUST NOT be repaired by
in-place rewriting or reopen tail-truncation. Instead, a store declaring
`deletion.external_bodies: "supported"` and
`runtime.incarnation_allocation: "prewrite_durable_ledger"` MAY recover a
damaged designated stream by activating a **separately typed recovery successor
stream** whose `stream_opened` entry carries `recovery`:

- **Profile prerequisites & quarantined-predecessor chain
  (`runtime.quarantined_designated_streams`):**
  - Recovery requires `runtime.incarnation_allocation: "prewrite_durable_ledger"`.
    Any `stream_opened.recovery` entry, or any non-empty
    `runtime.quarantined_designated_streams` array, when
    `runtime.incarnation_allocation` is omitted or `"effect_dependent"` MUST be
    refused as `body_removal_stream`.
  - Walking backward from the active `runtime.body_removal_stream` (`S_k`) via
    `stream_opened.recovery.previous_stream` (`S_k -> S_{k-1} -> ... -> S_0`,
    where `S_0` has no `recovery` header) defines the exact ordered chain of
    quarantined designated predecessors `[S_{k-1}, ..., S_0]`.
  - `runtime.quarantined_designated_streams` MUST equal `[S_{k-1}, ..., S_0]`
    with no cycles, no forks (no two streams naming the same `previous_stream`),
    no stale profile (where `runtime.body_removal_stream` still names a
    quarantined predecessor that has a successor), and no extra or unlinked
    streams (for example, placing a content stream or an unlinked stream into
    `quarantined_designated_streams`). Active `store_streams` MUST be disjoint
    from `quarantined_designated_streams`. Violating any of these chain
    invariants refuses designated-stream validation as `body_removal_stream`
    (and refuses `removal_eligibility` at Precedence 2 as
    `protection_unavailable`).
- **Recovery header binding (`stream_opened.recovery`):** Because reconciliation
  outcomes `R12` and `R15` depend on physical byte presence **at the moment of
  recovery activation**, a later physical byte loss MUST NOT retroactively
  rewrite an earlier recovery transition. `stream_opened.recovery` therefore
  durably records:
  - `store` (store identifier matching the active store);
  - `previous_stream` (quarantined designated predecessor stream identifier);
  - `cut_seq` (`>= 1`) and `cut_digest` (`sha256:` of RFC 8785 JCS bytes of the
    prefix array `previous_stream[1..cut_seq]`, which MUST end a complete group
    and MUST be the **longest valid complete-group prefix** of the quarantined
    raw byte image; naming a shorter prefix refuses as `recovery_incomplete`);
  - `quarantined_image_digest` (`sha256:` of the complete preserved damaged raw
    byte image of `previous_stream`, which MUST actually have a damaged tail or
    trailing invalid bytes beyond `cut_seq`—recovering an undamaged clean
    complete image without a damaged tail refuses as `recovery_incomplete`);
  - `reconciled_bodies`: array of `{body: {digest, length}, floor,
    activation_observation, post_recovery_state}`, sorted strictly by the RFC
    8785 JCS bytes of `body` with no duplicate `{digest, length}` entries,
    covering every body in `previous_stream[1..cut_seq]` or the durable
    allocation ledger (`floor > 0`) at activation:
    - `floor` is in `0 .. 2^53 - 1` (`0` is an explicit authoritative ledger
      attestation that zero incarnations were ever allocated for `{digest,
      length}`, whereas lifecycle `incarnation` remains `1 .. 2^53 - 1`);
    - `activation_observation` is `"present_valid"` or `"absent"` (a corrupt or
      unreadable byte observation `"present_corrupt"` cannot appear in an
      accepted recovery header because `R3` refuses activation as
      `body_observation`);
    - `post_recovery_state` is the exact deterministic state mandated by
      `R4`–`R19`;
  - `authorization: {ref, digest}` and `provenance`.
- **Atomic activation under continuous store-wide exclusion:** Before releasing
  exclusion or accepting any body operation on the successor stream, the store
  MUST: (1) permanently fence/invalidate all writer, remover, and lease handles
  from `previous_stream`; (2) preserve the complete damaged raw image of
  `previous_stream` matching `quarantined_image_digest`; (3) verify that
  `{cut_seq, cut_digest}` is the longest valid complete-group prefix of that raw
  image; (4) evaluate `R1`–`R19` across every body in prefix history or durable
  allocation ledger; and (5) write and `fsync` `S_k`'s `stream_opened` entry,
  move `previous_stream` out of active `store_streams` into
  `runtime.quarantined_designated_streams`, and atomically persist and `fsync`
  the updated profile (`runtime.body_removal_stream = S_k`) and directory.
- **Sequential multi-hop replay (`S_0 -> S_1 -> ... -> S_k`):** Evaluation MUST
  replay each hop sequentially rather than concatenating entries:
  1. Replay `S_0[1..cut_0]` from `(prior_state = none, prior_bound = 0)`.
  2. At each recovery boundary `S_{i-1} -> S_i`, validate `S_i`'s `recovery`
     header against `R1`–`R19` using `prior_state`, `prior_bound`, `floor`, and
     `activation_observation`:
     - Under `R12` (`prior_state == body_removal_started(n)`, damaged tail,
       `floor == n`, `activation_observation == "present_valid"`), the open
       removal is **durably closed as abandoned** and `post_recovery_state` is
       `"available"` (effective lifecycle state entering `S_i` becomes
       `body_published(n)`, so if bytes are later lost in `S_i` or at `S_{i+1}`,
       state transitions to `"integrity_loss"` under `R10` and `body_removed(n)`
       or `retry_unlink` is refused as `body_lifecycle`—never reverting to `R15`
       `"removal_pending"`). Re-initiating removal after an abandoned `R12` or
       `R13` transition—including when the abandoned `body_removal_started`
       carried `reason: "rights_restricted"`—requires fresh
       `removal_eligibility` evaluation (and fresh `authorization` when
       referenced) plus a new `body_removal_started` entry on the current
       published incarnation (`n` after `R12`, or `n' > floor` after `R13` and
       `body_published(n')`).
     - Under `R15` (`prior_state == body_removal_started(n)`, damaged tail,
       `floor == n`, `activation_observation == "absent"`), the open removal is
       preserved (`post_recovery_state == "removal_pending"`), allowing
       `body_removed(n)` in `S_i`.
     - Under `"publishing"` (`R7`, `R9`, `R13`, `R19`), any open removal is
       closed, `prior_bound` advances to `floor`, and the body enters a durable
       un-committed recovery-publishing state: until a new `body_published(n')`
       with `n' > floor` commits, subsequent clean-cut `body_state` reads and
       later recovery hops `S_{i+1}` yield `"publishing"` when bytes are
       `present_valid` and `"integrity_loss"` when bytes are `absent` (never
       reverting to `"removed"` after `R19` at `floor == n` or `"unpublished"`
       after `R7` at `floor == 0`).
     - Under `"integrity_loss"` (`R6`, `R10`, `R16`, `R18`), any open removal is
       closed, `prior_bound` advances to `floor`, and `"integrity_loss"` remains
       sticky across both `absent` and `present_valid` observations (at clean
       cuts and across later recovery hops `S_{i+1}`) until a new
       `body_published(n')` with `n' > floor` commits; un-republished byte
       reappearance never downgrades `"integrity_loss"` to `"publishing"` or
       `"available"`.
  3. Replay `S_i`'s subsequent lifecycle entries from that post-recovery
     effective state and `prior_bound := floor`, requiring every new
     `body_published(n')` in `S_i` to satisfy `n' > prior_bound`.
  4. At the active stream `S_k`'s clean current cut (`floor := prior_bound`),
     combine the effective lifecycle state with the live physical byte
     observation (`present_valid`, `present_corrupt`, or `absent`) under the
     table below.
- **Deterministic reconciliation cross-product (`R1`–`R19`):** Evaluated in
  strict precedence order `R1 -> R2 -> R3 -> R4..R19`; rows `R4`–`R19` apply
  identically whether cross-stream `referenced` is `false` or `true`:

| Row | Precedence / Pre-Header `prior_state` | Tail & Floor Relation (`prior_bound` vs `floor`) | Observation (`activation_observation` or clean live bytes) | Cross-Stream Referenced? | Exact Verdict (Refusal Code or State) |
| --- | --- | --- | --- | --- | --- |
| R1 | **Precedence 1:** Cut / floor / state / sort check | `cut_seq` not longest valid complete-group prefix of quarantined image, undamaged predecessor image, missing/unsorted/duplicate `reconciled_bodies` entry, `floor < prior_bound`, or `post_recovery_state` disagrees with table | any | `false` & `true` | Refuse: `recovery_incomplete` |
| R2 | **Precedence 2:** Counter ceiling check | `floor >= 2^53 - 1` (`9007199254740991`, leaving no safe integer `n' > floor`) | any | `false` & `true` | Refuse: `numeric_domain` |
| R3 | **Precedence 3:** Corrupt observation check | valid cut & `floor < 2^53 - 1` | `present_corrupt` (read error or digest/length mismatch) | `false` & `true` | Refuse: `body_observation` |
| R4 | `none` (`prior_bound == 0`) | clean (`cut_seq == end`, `floor := 0`) | `absent` | `false` & `true` | State: `unpublished` |
| R5 | `none` (`prior_bound == 0`) | damaged tail + valid `floor == 0` | `absent` | `false` & `true` | State: `unpublished` |
| R6 | `none` (`prior_bound == 0`) | damaged tail + valid `floor > 0` | `absent` | `false` & `true` | State: `integrity_loss` |
| R7 | `none` (`prior_bound == 0`) | clean (`floor := 0`) or damaged tail (`floor >= 0`) | `present_valid` | `false` & `true` | State: `publishing` (requires `body_published(n')` with `n' > floor`) |
| R8 | `body_published(n)` (`prior_bound == n`) | clean (`floor := n`) or damaged tail + valid `floor == n` | `present_valid` | `false` & `true` | State: `available` |
| R9 | `body_published(n)` (`prior_bound == n`) | damaged tail + valid `floor > n` (burned pre-write or lost-tail allocation) | `present_valid` | `false` & `true` | State: `publishing` (fenced; requires `body_published(n')` with `n' > floor`) |
| R10 | `body_published(n)` (`prior_bound == n`) | clean (`floor := n`) or damaged tail + valid `floor >= n` | `absent` | `false` & `true` | State: `integrity_loss` |
| R11 | `body_removal_started(n)` (`prior_bound == n`) | clean (`cut_seq == end`, `floor := n`) | `present_valid` | `false` & `true` | State: `removal_pending` |
| R12 | `body_removal_started(n)` (`prior_bound == n`) | damaged tail + valid `floor == n` | `present_valid` | `false` & `true` | Open removal durably closed as abandoned -> `available` (effective state becomes `body_published(n)`; `body_removed(n)` or `retry_unlink` without fresh `body_removal_started(n)` refused as `body_lifecycle`) |
| R13 | `body_removal_started(n)` (`prior_bound == n`) | damaged tail + valid `floor > n` | `present_valid` | `false` & `true` | Open removal durably closed as abandoned -> `publishing` (requires `body_published(n')` with `n' > floor`; `body_removed(n)` refused as `body_lifecycle`) |
| R14 | `body_removal_started(n)` (`prior_bound == n`) | clean (`cut_seq == end`, `floor := n`) | `absent` | `false` & `true` | State: `removal_pending` (may complete via `body_removed(n)`) |
| R15 | `body_removal_started(n)` (`prior_bound == n`) | damaged tail + valid `floor == n` | `absent` | `false` & `true` | Open removal preserved -> `removal_pending` (may complete via `body_removed(n)` in successor) |
| R16 | `body_removal_started(n)` (`prior_bound == n`) | damaged tail + valid `floor > n` | `absent` | `false` & `true` | State: `integrity_loss` (`body_removed(n)` refused as `body_lifecycle`) |
| R17 | `body_removed(n)` (`prior_bound == n`) | clean (`floor := n`) or damaged tail + valid `floor == n` | `absent` | `false` & `true` | State: `removed` |
| R18 | `body_removed(n)` (`prior_bound == n`) | damaged tail + valid `floor > n` | `absent` | `false` & `true` | State: `integrity_loss` |
| R19 | `body_removed(n)` (`prior_bound == n`) | clean (`floor := n`) or damaged tail + valid `floor >= n` | `present_valid` | `false` & `true` | State: `publishing` |

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

Existing v0 ingestion (`ingest`), shape, journal, receipt and reconciliation
refusals remain. Evaluation proceeds through strict JSON ingestion (`ingest`),
exact protocol-integer preflight (`numeric_domain`), schema validation
(`shape`), and logical semantics. The following are protection-operation
outcomes or v1 validation failures.

| Reason                   | Meaning                                                                                           |
| ------------------------ | ------------------------------------------------------------------------------------------------- |
| `numeric_domain`         | A protocol integer field is non-integral, exceeds the safe-integer ceiling, or exhausts a counter |
| `idempotency_conflict`   | A live key identifies another request or content operation                                        |
| `target_not_found`       | No exact target pin in this stream                                                                |
| `body_mismatch`          | The requested body selector disagrees with its target                                             |
| `hold_not_active`        | Restoration does not identify an active hold                                                      |
| `hold_mismatch`          | Restoration changes the hold's target or scope                                                    |
| `observation_order`      | Observation follows acceptance or restoration predates the hold                                   |
| `protection_unavailable` | Protection or source state is unavailable, from another stream, or not current                    |
| `protected_use`          | Matching or inherited hold, or a retired record selected for serving                              |
| `export_selection`       | Selection and omissions do not partition the inventory                                            |
| `body_closure`           | Missing required bytes, wrong bytes/length, duplicate or surplus body                             |
| `invalid_purpose`        | Use purpose is not serve, export or reuse                                                         |
| `invalid_deduplication`  | Content audit does not identify a live accepted content key and its record                        |
| `receipt_mismatch`       | Received protection or retirement receipt disagrees with the journal                              |
| `request_digest`         | Journal request does not match its canonical digest                                               |
| `operation_type`         | Journal type disagrees with request action                                                        |
| `accepted_key_reuse`     | A journal accepts a live key again                                                                |
| `journal_version`        | A v1 journal contains another wire version                                                        |
| `record_version`         | A v1 journal entry carries a record envelope other than `v: 1`                                    |
| `instant_spelling`       | An instant is not a valid date and time in the canonical six-digit UTC form                       |
| `journal_time`           | Acceptance or decision time precedes the accepted journal                                         |
| `idempotency_window`     | Missing positive key-retention window                                                             |
| `retired_record`         | A content write attempts to revive a retired record                                               |
| `restrictions_pending`   | A continuation has no current restriction review                                                  |
| `continuation`           | An import, review or manifest disagrees with the stream's continuation                            |
| `body_removal_stream`    | A lifecycle entry, type or continuation is outside the designated stream                          |
| `body_lifecycle`         | A lifecycle entry is not a valid transition for its body's incarnation                            |
| `body_observation`       | A body state observation is not an exact body and a verified present or absent                    |
| `still_referenced`       | An unretired record references the target body in a current or superseded revision                |
| `removal_authorization`  | Referenced retired-record bytes lack a valid authorization for store, body, incarnation and time  |
| `recovery_incomplete`    | A designated-stream recovery cut, quarantined image, floor bound or reconciled state is invalid   |

## Conformance and limits

Run `python3 scripts/test-durable-record-v1-controls.py`. File-backed vectors
cover scopes, independent holds, replay, expiration, reconciliation, atomic
cuts, keyed and unkeyed retirement, production time, instant spelling, record
envelope version, export omissions, inherited restrictions, body closure,
continuation review (cut and manifest digests, imports, currency), detached
stream seals (golden manifest digests, closer authentication, permanent
source/ancestor write fencing, store binding, `ancestor_seals` ordering, and
trailing-byte refusals), body lifecycle (every reader-state cell, incarnation
gaps, abandonment, republication, invalid transitions, placement and
designation), designated-stream recovery (`R1`–`R19` reconciliation across
unreferenced and referenced bodies, multi-hop replay, `R12` abandonment
durability, `quarantined_designated_streams` chain equality, and longest-prefix
quarantine checks), retention removal eligibility (orphan vs.
all-retired-record reclamation, active superseded-revision blocking, same-store
continuation references, store-wide coverage, Precedence 1–5 pairwise ordering,
authorization binding and temporal bounds, reason invariance, and
`retry_unlink` re-verification), and wire ingestion and numeric-domain
boundaries (safe-integer ceiling, integral decimal and exponent spellings,
precision-aliasing fractions, underflow/overflow exponents, counter exhaustion,
arbitrary payload numbers, and continuation source/import paths).
Schema validation batches cases through `goneat validate suite --format json`
with exact file and schema membership, separating valid/invalid schema verdicts
from parser, range or tool errors.
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
