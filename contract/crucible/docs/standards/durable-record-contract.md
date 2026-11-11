---
title: "Portable Durable Record Contract"
description: "Proposed engine-neutral record and journal semantics: stable record identity, exact revisions, digest-bound bodies, provenance, retention intent, keyed idempotent writes, revision conflict detection, typed receipts, and a stratum profile that declares durability and supported deletion scope"
category: "standards"
status: "deprecated"
version: "0.0.0"
lastUpdated: "2026-11-06"
maintainer: "core-standards"
reviewers: ["architecture", "security", "data-engineering"]
approvers: ["lead-maintainer"]
tags: ["records", "journal", "storage", "contract", "provenance", "retention", "interoperability"]
content_license: "CC0"
relatedDocs:
  - "docs/standards/durable-record-v1-contract.md"
  - "docs/standards/data-artifact-contract.md"
  - "docs/standards/segmented-snapshot-contract.md"
  - "docs/standards/durable-queue-contract.md"
  - "docs/standards/retention-lifecycle-classification.md"
  - "docs/standards/data-sensitivity-classification.md"
  - "docs/decisions/ADR-0013-durable-record-contract.md"
audience: "implementers"
---

# Portable Durable Record Contract

## Maturity and scope

> **Status: Deprecated for new stream production** (superseded by
> [`contract: durable-record/v1`](durable-record-v1-contract.md)). New
> producers and new streams MUST use `contract: durable-record/v1`. The `v0`
> schemas, fixtures, and validation rules remain normative for reading and
> verifying historical `v0` streams and for `v1` continuation chains whose
> `continues.capability` or `imported_from.capability` names
> `contract: durable-record/v0`.

This document specifies `contract: durable-record/v0`.
It defines what a durable record store promises to writers and readers: how a
record is identified, how revisions are bound to exact bytes, how a body held
outside the record is referenced, what an append receipt means, and how a
store declares which durability and deletion guarantees it actually provides.

It does **not** define a storage engine, a file layout, a table schema, a query
language, a sync protocol or a retrieval API. An embedded database, a journal
of files, a Git repository and an object-storage bucket can each sit behind it
and declare their guarantees through a stratum profile.

Deferred from v0 and not advertised in its vocabulary: **redaction of journal
history** (successor-generation semantics and completion receipts), shared
stream-level metadata inherited by records, multi-writer ownership of one
stream across hosts, automatic merge of divergent revisions, a wire binding for
remote operations, and behavioral conformance vectors. A v0 journal is never
mutated after append.

## Design stance

- **Records, not only events.** The unit is a record with a stable identity
  and one or more exact revisions. An event or measurement is a record with
  exactly one revision. A note or a reusable procedure is a record with many.
  A stream declares which of these it holds.
- **One thin envelope, typed payloads.** The envelope governs identity,
  revision, provenance, classification and body binding. Meaning lives in the
  payload, named by a host-less `payload_contract` token. The store never
  interprets, validates or rewrites a payload.
- **Profiles narrow, never widen.** A domain profile (for example agent
  session state, pipeline stage results or telemetry) may require fields,
  close the set of payload contracts, or require scope attributes. A profile
  may not add envelope fields, relax a rule, or change a meaning.
- **Identity is not location.** A body is bound by digest and length. Where
  the bytes can be fetched is resolved outside the record under the caller's
  access rules. Retrieval hints never appear in a record or its digest.
- **A receipt means persisted.** An `appended` or `revised` receipt means the
  entry is durable to the stream's declared failure model. Best-effort
  delivery is a stream declaration, never an inherited default.
- **Silence is not failure.** A writer that did not observe a receipt holds an
  indeterminate outcome and reconciles by idempotency key. It never infers
  that the write failed.
- **Conflicts are detected, never merged.** A revision presents the revision
  it supersedes. A mismatch is a conflict outcome; nothing is written as the
  new head.
- **Deletion is scoped.** A profile declares which deletion operations a store
  supports, per surface: the live head, retained history, managed replicas,
  external bodies, or decryption capability. A profile never states that
  anything _was_ deleted; completion belongs to a per-operation receipt.
  Copies outside management are outside every claim.
- **Closed vocabulary.** Unknown entry types, outcomes, capabilities and
  stream modes fail closed. There is no `x-` extension prefix in v0.

## Capability and versioning

A stratum profile MUST carry the capability string
`contract: durable-record/v0`. Consumers resolve it to a trusted, pinned
`contract.json`, verify `capability`, and load the entry schema
(`stratum-profile.schema.json`). Records and journal entries do not repeat the
token.

Additive changes within v0 are permitted when older consumers ignore the field
or fail closed safely. Changing the meaning of a field, weakening the receipt,
conflict or deletion-scope rules, adding or removing an entry type, and
renaming fields without an alias are breaking.

## Wire numeric domain and evaluation order

Every contract-defined integer field in `durable-record/v0` has an upper bound
of `9007199254740991` ($2^{53} - 1$), preserving each field's `minimum` or
`const`:

- `record.schema.json`: `v` (`0`), `revision`, `supersedes.revision`,
  `body.length`, `links[].revision`, and shared `$defs.pin.revision`;
- `journal-entry.schema.json`: `v` (`0`), `seq`, `group`, `group_size`,
  `revision`, `presented.revision`, `current.revision`, and embedded `record`
  integer paths;
- `write-receipt.schema.json`: `receipt.seq`, `receipt.pin.revision`,
  `receipt.current.revision`, and embedded `reconciliation.receipt` integer
  paths;
- `stratum-profile.schema.json`: `runtime.clock_skew_bound_ms` and numeric
  `runtime.idempotency_window_ms`.

This ceiling is an explicit `durable-record` interoperability restriction, not a
claim that RFC 8785 (JCS) forbids larger numbers in general. JCS serializes IEEE
754 binary64 numbers — including exact $2^{53}$ and larger floats — whereas
[RFC 7493 Section 2.2](https://www.rfc-editor.org/rfc/rfc7493#section-2.2) and
[RFC 8785 Appendix B](https://www.rfc-editor.org/rfc/rfc8785#appendix-B)
recommend $[-(2^{53}-1), +(2^{53}-1)]$ for exact integer interchange across JSON
parsers. Arbitrary numbers inside an application `record.payload` are not
governed by this ceiling and remain subject only to general I-JSON / RFC 8785
rules.

Counter and span exhaustion (`seq`, `revision`, or an atomic group whose closing
sequence `group + group_size - 1` would exceed `9007199254740991`) MUST be
refused; wraparound is forbidden. Implementations MAY use wider internal types
such as `int64` or `uint64`, and MUST enforce the wire domain at APIs, imports
and journal boundaries before emitting, canonicalizing or incrementing.

Wire readers and conformance gates evaluate inputs in four ordered stages:

1. **Strict JSON ingestion (`ingest`)** — valid UTF-8 RFC 8259 syntax, unique
   object member names at every nesting depth, no lone surrogates, and no
   non-JSON or non-finite IEEE 754 numbers. For NDJSON journals, let `L` be the
   byte length of a serialized entry line including its terminating `0x0A`
   (`\n`), and `k` be the number of bytes written for that line: every entry
   line (including the final line of the file) MUST be terminated by a single
   `0x0A` (`\n`) with no `0x0D` (`\r`). A non-empty final line missing its
   trailing `0x0A` (including `k = L - 1` bytes that close the JSON object `{...}`
   without `\n`) is an incomplete append and fails at `ingest`. Forbidding
   `0x0D` (`\r`) across all `v0` and `v1` NDJSON inputs is an explicit framing
   tightening in this revision;
2. **Exact numeric-domain preflight (`numeric_domain`)** — at protocol integer
   schema paths only (never inside `record.payload`), raw JSON numeric tokens
   are checked before binary64 conversion using bounded decimal/exponent
   inspection without expanding enormous exponents. Mathematical integers within
   `[-9007199254740991, +9007199254740991]` (`12`, `12.0`, `1.2e1`, `0.0`,
   `-0.0`) pass preflight; fractional values, precision-aliasing tails
   (`12.0000000000000001`, `1.0000000000000001`), underflow-to-zero exponents
   (`1e-400`), and magnitudes above `9007199254740991` (`9007199254740992`,
   wider `int64`/`uint64` values, overflow exponents) fail closed as
   `numeric_domain`. Non-numeric JSON values (`boolean`, `null`, `string`,
   `array`, `object`) pass through to schema validation, and safe negative
   integers (`-1`) pass `numeric_domain` so field `minimum` rules reject them;
3. **Schema validation (`shape`)** — JSON Schema Draft 2020-12 validation
   yielding a structured validity verdict (`true`, or `false` with validation
   diagnostics; parser, range or tool failures are infrastructure errors, never
   schema rejections). Because the pinned validator (`goneat`) does not
   implement arbitrary Draft 2020-12 vocabularies or sibling assertions
   alongside `$ref`, durable-record schemas are restricted to a finite Draft
   2020-12 keyword subset enforced at schema qualification (`$schema`, `$id`,
   `$defs`, `$ref`, `title`, `description`, `type`, `const`, `enum`,
   `properties`, `required`, `additionalProperties`, `items`, `contains`,
   `minItems`, `maxItems`, `uniqueItems`, `minLength`, `maxLength`, `pattern`,
   `format`, `minimum`, `maximum`, `allOf`, `anyOf`, `oneOf`, `not`, `if`,
   `then`, `else`). Every `$ref` targets a local `#/$defs/<name>` definition
   and MUST NOT be combined with sibling assertion keywords (`maxLength`,
   `type`, `pattern`, etc.); `format: "date-time"` is the only admitted
   `format` attribute;
4. **Logical semantics** — stream, group, digest, revision-chain, timestamp,
   receipt, coverage and profile invariants. Direct typed APIs enforce the same
   `[minimum, 9007199254740991]` range, though an already-parsed float cannot
   certify the original wire token.

### Timestamp validation profile (`v0`)

Every `v0` timestamp position (`journal-entry.schema.json#/properties/at` and
`write-receipt.schema.json#/$defs/reconciliation` `attempted_at` across all
three reconciliation results `found`, `not_found`, and `unknown`, plus
`window_covers_from` and `window_covers_until` when `result` is `not_found`)
is validated against the `v0` RFC 3339 profile:

- **Lexical grammar:** `YYYY-MM-DD('T'|'t')HH:MM:SS(.d+)?('Z'|'z'|[+-]HH:MM)`
  (ASCII digits `0`–`9` `0x30`–`0x39` only; Unicode non-ASCII digits such as
  `U+FF10..U+FF19` are refused; period `.` fractional separator when present;
  2-digit fields; offset hours `00`–`23`, minutes `00`–`59`; `-00:00` is
  accepted in `v0` per RFC 3339 Section 4.3 as UTC epoch-equivalent alongside
  `Z`, `z`, and `+00:00`);
- **Calendar & year bounds:** proleptic Gregorian month/day validity (leap year
  iff `year % 4 == 0 and (year % 100 != 0 or year % 400 == 0)`), with both the
  written local year and the UTC-normalized year in `0001`–`9999` (`0000`,
  `0001-01-01T00:00:00+01:00`, and `9999-12-31T23:59:59-01:00` are refused);
- **Unconditional leap-second refusal:** `second == 60` (`:60`) is refused at
  every timestamp position regardless of date, month, or offset;
- **Microsecond precision ceiling:** fractional seconds with any nonzero digit
  beyond the 6th fractional place (sub-microsecond precision) fail closed as
  `timestamp` rather than truncating.

| `v0` position | Non-string / bad type, impossible calendar date (`2026-02-30...`), leap second (`:60`), or non-ASCII digits (`２０２６-...`) | Bare date (`2026-10-12`), comma fraction, 1-digit hour, bad offset (`+24:00`), year `0000`, UTC year underflow/overflow, or `>6` nonzero fractional digits |
| --- | --- | --- |
| `journal-entry.at`, `reconciliation.attempted_at`, `reconciliation.window_covers_from`, `reconciliation.window_covers_until` (end-to-end staged gate under qualified `goneat` `v0.6.5`–`v0.6.7`, Draft 2020-12 with `format: "date-time"` assertion enabled, verified by `scripts/validate-durable-record-fixtures.py`) | Stage 3 `shape` (and Stage 4 `timestamp` if invoked directly or under a Stage 3 engine without `format: "date-time"`) | Passes `goneat` `format: "date-time"`; refused at Stage 4 as `timestamp` (or `continuation` when observed as a `v1` continuation source/ancestor) |

The table describes the earlier schema and qualified validator profile. The
`v0.6.5` formatter rejects lowercase `t`/`z` at Stage 3, although the v0
semantic grammar accepts them. The current seconds guard preserves those
spellings, numeric offsets including `-00:00`, and additional zero fractional
digits. Stage 4 still enforces the complete v0 timestamp profile.

The repository's published validator profile uses [Goneat `v0.6.8`](https://github.com/fulmenhq/goneat/releases/tag/v0.6.8)
from [source revision `3b700f87a11d8760e66882a829631527ff79bd9f`](https://github.com/fulmenhq/goneat/commit/3b700f87a11d8760e66882a829631527ff79bd9f).
The verified `darwin/arm64` binary reports version and module `v0.6.8` and has
SHA-256 `eb9ef352a120ef0910da40f22cc850963133961ed508f84dfebc98dcea1a4967`.
It reports engine `github.com/santhosh-tekuri/jsonschema/v6@v6.0.3`,
Draft 2020-12 with declared dialect, `format_mode: assert` and
`leap_policy: structural-leap/v1`. With the schema bytes pinned below, Stage 3
refuses `:60` via the seconds guard even though this format evaluator admits it.
It also refuses bare dates, comma fractions, one-digit hours and `+24:00` offsets
at Stage 3; year zero, UTC year underflow/overflow and excess nonzero fractional
precision remain Stage 4 checks. It accepts the v0 spelling variants above.
The binary digest and observations here cover the verified native `darwin/arm64`
build; other platforms require their own published-artifact verification and
contract checks. The same schema inventory covers the
[v1 timestamp profile](durable-record-v1-contract.md#production-time-instants-and-ordering).

| Schema                                                      | SHA-256                                                            |
| ----------------------------------------------------------- | ------------------------------------------------------------------ |
| `durable-record/v0/journal-entry.schema.json`               | `d36b5b9684d967972d2b358ff3642251c6cd33b099c07e56c67d0e5723e91620` |
| `durable-record/v0/write-receipt.schema.json`               | `a9d28dd388ba611713b86793243b01ab01d387c5246911281c7d0cfb8943fb5f` |
| `durable-record/v1/journal-entry.schema.json`               | `b8fe8bf56916f73eac403ce7e03fe2ccbf20d8df6e50acf20af6e9de9784e1de` |
| `durable-record/v1/protection-request.schema.json`          | `5ca0459b2e30ed493116adb5b4b2b524a0c6fd3253c0ccd78ded36ca06a3e6e4` |
| `durable-record/v1/record.schema.json`                      | `84c314557c85fe90b554c0be250e0c123e26c9f82613f759b6319681bffe83ed` |
| `durable-record/v1/removal-eligibility-request.schema.json` | `4a0a0da9a7dee85d622fa181cb1e751f11bb5f56dab274ff25049a6bd7800e61` |
| `durable-record/v1/stream-seal.schema.json`                 | `687953021e2e3e449d14c141e9cd3be65927ba9cc94b462d0d8283b7eee9d654` |
| `durable-record/v1/write-receipt.schema.json`               | `5a8a577799f121bef7e70506f916459203d5586ea3c7773375a1013fe9d0834a` |

## Governed objects

### Record

Schema: `record.schema.json`.

| Field              | Rule                                                                                                                                                                                       |
| ------------------ | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `v`                | `0`.                                                                                                                                                                                       |
| `stream`           | Stream identifier; scope of record ids, sequence numbers and idempotency keys.                                                                                                             |
| `record_id`        | Stable for the life of the record, unique per stream, opaque.                                                                                                                              |
| `revision`         | Positive integer. `1` creates the record. Each revision is exactly one greater than the revision it supersedes.                                                                            |
| `supersedes`       | Required when `revision > 1`: `{revision, digest}` of the revision being replaced in the same stream and record. Forbidden when `revision` is `1`.                                         |
| `payload_contract` | Host-less token naming the payload's contract. Carried, never validated by the store.                                                                                                      |
| `payload` / `body` | Exactly one. `payload` is inline I-JSON. `body` is a body reference.                                                                                                                       |
| `provenance`       | Who produced this revision: `actor_kind` (`human`, `agent`, `process`), `actor`, optional `supervisor`, `model`, `tool`. Model and tool describe the producer; they do not own the record. |
| `classification`   | `retention` (retention-lifecycle classifier key) and `sensitivity` (sensitivity classifier key). Both required; `unknown` is valid and is a decision deferred, not a default.              |
| `labels`           | Optional free tags. Never interpreted by the store.                                                                                                                                        |
| `links`            | Optional soft references to other `record_id` values in the same stream, optionally pinned to a revision. Not a graph contract.                                                            |

**Revision digest.** SHA-256 over the RFC 8785 canonical bytes of the whole
record object as written, including `stream`, `record_id`, `revision` and
`supersedes`. Records carry no store-assigned fields and no retrieval hints,
so the digest is the record's semantic identity. Byte-level integrity of a
journal file or export is a property of that bundle, not of the record.

**Pin.** `{stream, record_id, revision, digest}`. Record ids are unique only
within a stream, so a pin always names the stream. A pin resolves to a record
whose recomputed digest equals `digest`, or to nothing. Never to different
bytes.

### Body reference

A body is bytes held outside the record.

| Field         | Rule                                                                                                          |
| ------------- | ------------------------------------------------------------------------------------------------------------- |
| `digest`      | Required, `sha256:` + 64 lowercase hex, over the exact bytes.                                                 |
| `length`      | Required, byte count.                                                                                         |
| `media_type`  | Required.                                                                                                     |
| `artifact_id` | Optional `contract: data-artifact/v0` artifact token when the body is governed as an artifact.                |
| `encryption`  | Optional `{scheme, key_ref}`. `key_ref` is an opaque token naming the key-custody record, never key material. |

**Body first.** A store MUST NOT acknowledge a record whose body it cannot
verify by digest and length at write time, unless capability
`unverified_bodies` is enabled. Every receipt for a record with a body states
`body_status: verified` or `unverified`. `verified` proves only that the
store checked the bytes against digest and length at write time. It is not a
claim of continued recoverability, which rests on the body's retention and
custody or on bytes included in an export. An `unverified` receipt never
satisfies a requirement for verified bytes. Record and body are
not assumed to be published atomically.

### Journal entry

Schema: `journal-entry.schema.json`. One NDJSON line per entry. `seq` is
monotonic per stream and gap-free. `group` and `group_size` follow the atomic
group rule of `contract: durable-queue/v0`: a prefix ending at `seq` N is a
consistent cut iff, for the entry at N, `N - group + 1 == group_size`.

| Type            | Appended when                                                                                                   | Carries                                                                       |
| --------------- | --------------------------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------- |
| `stream_opened` | A stream is created; always `seq` 1                                                                             | `mode`, `failure_model`                                                       |
| `appended`      | Revision 1 of a new record is accepted                                                                          | `record`, `digest`, `idempotency_key`, `body_status` if the record has a body |
| `revised`       | A later revision is accepted against the current head                                                           | `record`, `digest`, `idempotency_key`, `body_status` if the record has a body |
| `deduplicated`  | A write repeats a live key with the same revision digest (audit only; the caller receives the original receipt) | `record_id`, `idempotency_key`                                                |
| `conflicted`    | A write repeats a live key with a different digest, or presents a stale `supersedes` (audit)                    | `record_id`, `reason`, `presented` pin, `current` pin when a head exists      |
| `retired`       | A record is withdrawn from current reads; history retained                                                      | `record_id`, `reason`                                                         |

Unlike the queue ledger, `appended` and `revised` carry the record: a complete
journal is the portable form of the stream. `deduplicated` and `conflicted`
change no state and may follow `retired`. A v0 journal is never rewritten.

### Stream modes

| Mode              | Rule                                                                                                |
| ----------------- | --------------------------------------------------------------------------------------------------- |
| `single_revision` | Only `appended`. `revised` is a `mode_violation` outcome. Fits events, measurements, stage results. |
| `revisioned`      | `appended` then any number of `revised`. Fits notes, procedures, session state.                     |

### Write request and keyed idempotency

Every write presents an `idempotency_key`, scoped to the stream. Keyed
deduplication is core: the key is live for the profile's declared
`idempotency_window_ms`, measured from the original accepted write and
independent of later revisions or retirement of the record. Precedence: a live
key is checked **before** any revision or retirement rule.

- A repeat with the same revision digest returns the **exact original accepted
  receipt**, byte for byte the same outcome, pin, `seq`, failure model and body
  status. The store journals a `deduplicated` audit entry; the caller never sees
  a different outcome for a replay.
- A repeat with a different digest returns `idempotency_conflict` and accepts
  nothing.
- After the window, a repeated key is a new write and is subject to the ordinary
  revision rules.

### Write receipt

Schema: `write-receipt.schema.json`. One receipt per write.

| Field                       | Rule                                                                                                                                                      |
| --------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `outcome`                   | Accepted: `appended`, `revised`. Rejected: `idempotency_conflict`, `revision_conflict`, `no_head`, `mode_violation`, `body_unverified`, `retired_record`. |
| `stream`, `idempotency_key` | Always.                                                                                                                                                   |
| `pin`, `seq`                | For accepted outcomes: the record's pin and the journal `seq` of its entry.                                                                               |
| `failure_model`             | For accepted outcomes: the stream's model the entry was acknowledged under.                                                                               |
| `body_status`               | For accepted outcomes, exactly when the record has a body: `verified` or `unverified`, as journaled.                                                      |
| `current`                   | For `revision_conflict`: the current head pin.                                                                                                            |

`no_head` answers a revision (`revision > 1`) for a record that has no accepted
head in the stream. `revision_conflict` always has a head to report.

Outcomes other than `appended` and `revised` write no new head.

### Indeterminate writes and reconciliation

A writer that sends a write and observes no receipt holds an **indeterminate**
outcome. It reconciles with a lookup by `{stream, idempotency_key, attempted_at}`,
where `attempted_at` is the writer's own clock reading taken before it sent the
write. The lookup returns exactly one of:

- `found` with the **exact original accepted receipt** for that stream and key;
- `not_found`, only when the store can state a **coverage interval**
  `[window_covers_from, window_covers_until]` on the store clock. Inside it, every
  accepted key is retained and no accepted write holds this key. The interval
  must contain `attempted_at`, and `window_covers_from` must be no later than the
  earliest instant the write could have been accepted. Instants are compared as
  parsed RFC 3339 times, never as strings. A comparison that cannot represent a
  timestamp's precision exactly fails closed rather than truncating; the
  fixture checker in this repository resolves microseconds. With the profile's
  `clock_skew_bound_ms` = B, the whole interval `[attempted_at - B, attempted_at + B]`
  must lie within the coverage interval. `not_found` means absence at an authoritative
  observation. It does **not** prove that a delayed or in-flight write cannot
  still be accepted later. The writer may retry only under the **same** live key,
  which keeps the retry safe;
- `unknown`, when the window has elapsed, the key may have been evicted, the
  attempt is older than the retained interval, or coverage cannot be shown.
  `unknown` is never evidence that the write was not accepted.

A store with `idempotency_window_ms: none` always answers `unknown` and states
so in its profile.

### Export closure

An exported journal is `full_content` only if the bytes of every body referenced
by an included record are themselves contained in the export, and verify against
their digest and length. A digest checked earlier against bytes held elsewhere
does not make an export self-contained. Otherwise the export is
`references_only`: portable metadata, not evidence that the bodies are
recoverable. An export always states which.

### Stratum profile

Schema: `stratum-profile.schema.json`. The discovery root. As in the durable
queue, `build` (immutable artifact facts) and `runtime` (effective facts for the
exercised configuration) are distinct, and `runtime.enabled_capabilities` MUST
be a subset of `build.compiled_capabilities`.

`runtime.failure_model` is a guarantee: the strongest failure an acknowledged
write survives (`survives_power_loss`, `survives_process_loss`, `none`).

`runtime.history_policy` is chosen at store creation and is independent of
writer topology. v0 admits `append_only` only; `managed_redaction` is reserved
for the deferred redaction mechanism and is schema-invalid in v0.

`runtime.idempotency_window_ms` is required: keyed deduplication is core.
`runtime.clock_skew_bound_ms` states the bound the store assumes between writer
and store clocks when it answers `not_found`. The
value `none` states that the stratum performs no keyed deduplication and
cannot reconcile indeterminate writes; the schema admits it only when
`conformance` is `benchmark_only`.

`runtime.deletion` declares, per surface, which deletion operation the store
**supports**. It is a standing capability statement, never an outcome:

| Surface            | Operation scope                                                                              |
| ------------------ | -------------------------------------------------------------------------------------------- |
| `live_head`        | The current revision can be made to stop resolving.                                          |
| `retained_history` | Earlier revisions' bytes can be removed from the store's history.                            |
| `managed_replicas` | Copies the store manages (mirrors, remotes it publishes to, backups it owns) can be cleaned. |
| `external_bodies`  | Bytes held by body references can be removed.                                                |
| `decryption`       | Decryption capability for retained ciphertext can be retired by key retirement.              |

Each surface is `supported`, `unsupported` or `not_applicable`. In v0,
`retained_history` is never `supported`. `decryption: supported` is a claim
about key custody only and requires `key_custody_assumptions` (for example
escrow, wrapped keys, every recipient). It is never a claim about byte removal
or about plaintext already extracted. Every profile states
`unmanaged_copies: outside_claim`. Completion of any deletion is evidenced by a
per-operation receipt bound to target and store generation, defined with the
redaction mechanism; v0 profiles make no completion claims.

`origin: harness` means a qualification harness produced the profile from
observed runs. `origin: declared` is an unverified claim and MUST NOT be
presented as conformance evidence. v0 has no deterministic conformance
level; `conformance` is `benchmark_only` or `none`.

Capabilities in v0: `revisioned`, `body_ref`, `unverified_bodies`,
`key_retirement`, `export_bundle`. Conflict detection on `supersedes` and keyed
deduplication are core and have no capability. `managed_redaction` is reserved
and not in the v0 vocabulary.

## Portable invariants

1. **Exact revisions.** A pin resolves to a record whose recomputed digest
   matches, or to nothing. Never to different bytes.
2. **Linear heads.** At most one accepted revision per
   `{stream, record_id, revision}`. A write whose `supersedes` does not match
   the current head is `revision_conflict` and is journaled as `conflicted`.
3. **Keyed retry.** Within the declared window, a repeated key with the same
   digest returns the exact original accepted receipt, and a different digest
   returns `idempotency_conflict`. This holds even after the record was later
   revised or retired. Outside the window, or under
   `idempotency_window_ms: none`, retry safety is not promised.
4. **Receipt honesty.** A receipt states the failure model it was acknowledged
   under and the body status. A missing receipt is indeterminate, never
   failure.
5. **Retirement is not deletion.** `retired` withdraws a record from current
   reads and retains history. Byte removal is deferred from v0.
6. **Selection is declared.** An export of part of a stream (by record, label
   or profile scope attribute) keeps original records, sequence numbers and
   digests, declares omitted predecessors as a verification boundary, and
   states its closure. It is never re-sequenced or re-hashed and presented as
   complete.

## Composition

- **`contract: data-artifact/v0`.** A journal is a `record_stream` grain;
  bodies may be artifacts.
- **`contract: segmented-snapshot/v0`.** Sealed exports of a journal are
  snapshots. Durable-record governs the live journal; segmented-snapshot governs
  the sealed bundle.
- **`contract: durable-queue/v0`.** Shares the atomic group and consistent cut
  idiom and the build/runtime profile split. Delivery, leases and fencing are
  not record semantics.
- **Classifiers.** `classification.retention` and `classification.sensitivity`
  use the retention-lifecycle and sensitivity classifier keys.
