---
title: "Portable Durable Record Contract"
description: "Proposed engine-neutral record and journal semantics: stable record identity, exact revisions, digest-bound bodies, provenance, retention intent, keyed idempotent writes, revision conflict detection, typed receipts, and a stratum profile that declares durability and supported deletion scope"
category: "standards"
status: "draft"
version: "0.0.0"
lastUpdated: "2026-10-12"
maintainer: "core-standards"
reviewers: ["architecture", "security", "data-engineering"]
approvers: ["lead-maintainer"]
tags: ["records", "journal", "storage", "contract", "provenance", "retention", "interoperability"]
content_license: "CC0"
relatedDocs:
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

This is the **proposed experimental contract** `contract: durable-record/v0`.
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
  anything *was* deleted; completion belongs to a per-operation receipt.
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

## Governed objects

### Record

Schema: `record.schema.json`.

| Field | Rule |
| --- | --- |
| `v` | `0`. |
| `stream` | Stream identifier; scope of record ids, sequence numbers and idempotency keys. |
| `record_id` | Stable for the life of the record, unique per stream, opaque. |
| `revision` | Positive integer. `1` creates the record. Each revision is exactly one greater than the revision it supersedes. |
| `supersedes` | Required when `revision > 1`: `{revision, digest}` of the revision being replaced in the same stream and record. Forbidden when `revision` is `1`. |
| `payload_contract` | Host-less token naming the payload's contract. Carried, never validated by the store. |
| `payload` / `body` | Exactly one. `payload` is inline I-JSON. `body` is a body reference. |
| `provenance` | Who produced this revision: `actor_kind` (`human`, `agent`, `process`), `actor`, optional `supervisor`, `model`, `tool`. Model and tool describe the producer; they do not own the record. |
| `classification` | `retention` (retention-lifecycle classifier key) and `sensitivity` (sensitivity classifier key). Both required; `unknown` is valid and is a decision deferred, not a default. |
| `labels` | Optional free tags. Never interpreted by the store. |
| `links` | Optional soft references to other `record_id` values in the same stream, optionally pinned to a revision. Not a graph contract. |

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

| Field | Rule |
| --- | --- |
| `digest` | Required, `sha256:` + 64 lowercase hex, over the exact bytes. |
| `length` | Required, byte count. |
| `media_type` | Required. |
| `artifact_id` | Optional `contract: data-artifact/v0` artifact token when the body is governed as an artifact. |
| `encryption` | Optional `{scheme, key_ref}`. `key_ref` is an opaque token naming the key-custody record, never key material. |

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

| Type | Appended when | Carries |
| --- | --- | --- |
| `stream_opened` | A stream is created; always `seq` 1 | `mode`, `failure_model` |
| `appended` | Revision 1 of a new record is accepted | `record`, `digest`, `idempotency_key`, `body_status` if the record has a body |
| `revised` | A later revision is accepted against the current head | `record`, `digest`, `idempotency_key`, `body_status` if the record has a body |
| `deduplicated` | A write repeats a live key with the same revision digest (audit only; the caller receives the original receipt) | `record_id`, `idempotency_key` |
| `conflicted` | A write repeats a live key with a different digest, or presents a stale `supersedes` (audit) | `record_id`, `reason`, `presented` pin, `current` pin when a head exists |
| `retired` | A record is withdrawn from current reads; history retained | `record_id`, `reason` |

Unlike the queue ledger, `appended` and `revised` carry the record: a complete
journal is the portable form of the stream. `deduplicated` and `conflicted`
change no state and may follow `retired`. A v0 journal is never rewritten.

### Stream modes

| Mode | Rule |
| --- | --- |
| `single_revision` | Only `appended`. `revised` is a `mode_violation` outcome. Fits events, measurements, stage results. |
| `revisioned` | `appended` then any number of `revised`. Fits notes, procedures, session state. |

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

| Field | Rule |
| --- | --- |
| `outcome` | Accepted: `appended`, `revised`. Rejected: `idempotency_conflict`, `revision_conflict`, `no_head`, `mode_violation`, `body_unverified`, `retired_record`. |
| `stream`, `idempotency_key` | Always. |
| `pin`, `seq` | For accepted outcomes: the record's pin and the journal `seq` of its entry. |
| `failure_model` | For accepted outcomes: the stream's model the entry was acknowledged under. |
| `body_status` | For accepted outcomes, exactly when the record has a body: `verified` or `unverified`, as journaled. |
| `current` | For `revision_conflict`: the current head pin. |

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

| Surface | Operation scope |
| --- | --- |
| `live_head` | The current revision can be made to stop resolving. |
| `retained_history` | Earlier revisions' bytes can be removed from the store's history. |
| `managed_replicas` | Copies the store manages (mirrors, remotes it publishes to, backups it owns) can be cleaned. |
| `external_bodies` | Bytes held by body references can be removed. |
| `decryption` | Decryption capability for retained ciphertext can be retired by key retirement. |

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
