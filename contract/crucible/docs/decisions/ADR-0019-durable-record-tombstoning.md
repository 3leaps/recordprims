---
id: "ADR-0019"
title: "Durable record tombstoning with retained audit history"
status: "proposed"
date: "2026-10-20"
last_updated: "2026-10-21"
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

## Context

A retained record can remain necessary for authorized audit while its use,
export or reuse must stop. Retirement withdraws an individual record from
current reads; it does not express this restriction. Byte removal belongs to
the separately reserved managed-redaction capability.

## Decision

Add `tombstoned` and `untombstoned` journal operations in v1. Retain the v0
record envelope and revision digests unchanged. V1 readers refuse v0 journals
and receipts. Implementations may expose both capabilities through distinct
version-aware entry points. No automatic migration or relabeling is defined.
A live v0 stream stays v0; an adopter needing restrictions opens a separately
addressed v1 stream and re-records content under the standard's acceptance rules.
Unchanged envelopes and pins require retaining the same logical stream
identifier; a new identifier requires new digests and pins. V0 keys, receipts
and journal history do not carry over; outstanding v0 retry safety remains
with v0. Retirement and protection state must be established before releasing
re-recorded content.
Once content is re-recorded under the same stream identifier, the v0 stream
accepts no further content writes for that identifier; its retained records are
copies outside v1 management, and ordinary serving, export or reuse of those
pins must resolve through the v1 store.

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
