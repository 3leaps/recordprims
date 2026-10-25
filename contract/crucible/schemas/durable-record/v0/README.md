# Durable record schema family v0

**Proposed / experimental.** The capability is `contract: durable-record/v0`.
Resolve it through a trusted, pinned `contract.json`; never treat
instance-provided retrieval addresses as a schema registry. The normative
companion is the
[durable record standard](../../../docs/standards/durable-record-contract.md).
Decision record: [ADR-0013](../../../docs/decisions/ADR-0013-durable-record-contract.md).

The contract is logical only. Storage engines, file layouts, query languages,
sync protocols and retrieval APIs belong to adopting repositories. A v0 journal
is never rewritten; redaction of history is deferred.

| Artifact | Responsibility |
| --- | --- |
| `contract.json` | Capability manifest; entry is the stratum profile |
| `stratum-profile.schema.json` | Discovery root: build facts, runtime facts, failure model, keyed-dedup window, supported deletion scope |
| `record.schema.json` | One revision of a record; its RFC 8785 digest is its identity |
| `journal-entry.schema.json` | One line of the append-only NDJSON journal; closed entry vocabulary |
| `write-receipt.schema.json` | A write receipt, or the result of reconciling an indeterminate write |
| `examples/` | Session-handoff and pipeline stage-result records, a journal, receipts, reconciliations, a profile |
| `rejects/schema/` | Baseline and reject pairs differing in exactly one field |
| `rejects/semantic/` | Journals the fixture-integrity checker must reject, named `reject-<reason>--<case>` |

## Controls

`scripts/test-durable-record-controls.sh` (run by `make check`) validates every
golden and every schema pair, checks that shared definitions are identical
across schemas, and runs `scripts/validate-durable-record-fixtures.py`. That
checker recomputes each revision digest over RFC 8785 bytes, and requires:
- gap-free `seq` and a consistent final cut;
- a linear revision chain whose `supersedes` is the current head;
- no `revised` entry in a `single_revision` stream;
- `conflicted.current` equal to the head at that point;
- receipt pins that match their journal line.

Each semantic reject must fail with the reason named in its file name.

**Assurance boundary.** These controls check fixtures and the structural
rules. They do not exercise a store. Behavioral vectors and harness-produced
profiles are deferred; no v0 profile claims deterministic conformance.
