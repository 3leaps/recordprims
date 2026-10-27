---
id: "ADR-0019"
title: "Durable record tombstoning with retained audit history"
status: "proposed"
date: "2026-10-20"
last_updated: "2026-10-25"
deciders:
  - "@3leapsdave"
  - "cxotech"
  - "entarch"
scope: "Crucible foundation / portable storage contracts"
tags: ["records", "journal", "retention", "interchange-contract"]
---

# ADR-0019: Durable Record Tombstoning with Retained Audit History

## Status

Cxotech assents to the version boundary and adoption scope, subject to the
promotion gate and v0-adopter path stated here. Other listed deciders remain
prospective; this scoped assent does not promote the family to accepted.

Proposed. Defines an experimental `contract: durable-record/v1` family.

**Amended 2026-10-25, before any v1 implementation was released.** The v1
journal now carries record envelope `v: 1` (the v0 envelope with `v` changed
to `1` and an optional `produced_at` added), every v1 wire instant has one
canonical spelling, and `retired` gains an optional idempotency key with an
accepted receipt. This replaces the original decision to keep the v0 record
envelope unchanged in v1; the original text is kept below, marked. The change
is not additive.

## Context

A retained record can remain necessary for authorized audit while its use,
export or reuse must stop. Retirement withdraws an individual record from
current reads; it does not express this restriction. Byte removal belongs to
the separately reserved managed-redaction capability.

## Decision

Add `tombstoned` and `untombstoned` journal operations in v1. *(Original,
2026-10-20: retain the v0 record envelope and revision digests unchanged.
Amended 2026-10-25: v1 carries record envelope `v: 1`; see below.)* V1 readers refuse v0 journals
and receipts. Implementations may expose both capabilities through distinct
version-aware entry points. No automatic migration or relabeling is defined.
A live v0 stream stays v0; an adopter needing restrictions opens a separately
addressed v1 stream and re-records content under the standard's acceptance rules.
*(Original, 2026-10-20: unchanged envelopes and pins require retaining the
same logical stream identifier; a new identifier requires new digests and
pins. Amended 2026-10-25: content re-recorded into v1 becomes a `v: 1` record,
so its digest and pin always change.)* V0 keys, receipts
and journal history do not carry over; outstanding v0 retry safety remains
with v0. Retirement and protection state must be established before releasing
re-recorded content.
Once content is re-recorded under the same stream identifier, the v0 stream
accepts no further content writes for that identifier; its retained records are
copies outside v1 management, and ordinary serving, export or reuse of those
pins must resolve through the v1 store.

**Amendment (2026-10-25).** Building the first implementation showed three
gaps in the envelope that only an in-place change can close while v1 has no
released implementation. The record has no production time, so records cannot
carry a pinned time and re-recording loses it; v1 records therefore carry
envelope `v: 1` with an optional `produced_at` claim inside the digest, and a
`v: 0` record in a v1 journal is refused. Two correct implementations could
emit different bytes for one instant; every v1 wire instant is therefore
spelled with exactly six fractional digits in UTC, and readers refuse other
spellings rather than normalizing them under a digest. Retirement was the only
unkeyed, unacknowledged v1 operation; it gains an optional key and an accepted
receipt bound to a digest of `{stream, record_id, reason, idempotency_key}`.
The v0 family is unchanged.

Require exact targets, attributed evidence, observations, independent holds
and targeted restoration.
Use one key namespace across content and protection operations, with original
receipt replay preceding mutable-state checks.

Require authoritative current protection before ordinary serving, export or
reuse. Declare omissions explicitly and keep closure independent of permission.
Inherited restrictions remain binding. Audit access requires separate
authorization and cannot be selected as a public export bypass.

The [standard](../standards/durable-record-v1-contract.md), schemas and logical
vectors define the candidate. Physical migration, erasure and adopter
qualification remain outside this decision. This repository follows the
[3 Leaps OSS Sensitive Local Data Policy](https://github.com/3leaps/oss-policies/blob/main/SENSITIVE-LOCAL-DATA.md);
all examples are synthetic.

## Consequences

Readers must negotiate v1 before interpreting its journal. Existing v0 data
and contracts remain valid. Held content and its immutable pins remain available
for authorized audit. Consumers must enforce restrictions through derived
artifacts; schema validation alone cannot establish release authorization or
revoke previously released copies.

V1 remains proposed and experimental until (a) an independently authored reader
agrees with the logical vectors with no known disagreements, as required by
[PDR-0006 Rule 2c](PDR-0006-shipping-charter.md), and (b) at least one adopter
qualifies a store against the contract. It MAY ship in a tagged release under
the proposed/experimental label before those promotion gates are met. The
current checker reuses v0 helpers and does not satisfy the independent-reader
gate. Schema validation and logical-vector agreement alone do not qualify a
production store.
