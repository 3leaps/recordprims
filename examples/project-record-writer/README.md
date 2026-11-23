# Record a project decision or update

This example records decisions and project updates against a project ID, lets
you recover the accepted revision after a lost reply, and reads the same
content back as structured data or as a Markdown page. It is built only on the
public recordprims Go API.

It is an example. It carries no compatibility promise, is not a release
artifact, and is not part of the recordprims API. The project, people and
content in it are synthetic.

## Quick start

From this directory (Go 1.26 or later):

```bash
go build -o .local/project-record ./cmd/project-record
P=project/harbor-demo.json

.local/project-record --project $P init          # create the local store
.local/project-record --project $P kinds         # the kinds this project enables
.local/project-record --project $P inspect decision
```

Write a request (`.local/decision.json`):

```json
{
  "project": "harbor-demo",
  "kind": "decision",
  "record_id": "dec-intake-queue",
  "operation_key": "2026-11-23-intake-queue",
  "entry": {
    "title": "Use one intake queue for berth requests",
    "decision": "All berth requests enter through the shared intake queue.",
    "rationale": "Two intake paths caused double bookings in the October trial.",
    "authority": {"name": "Rowan Example", "role": "operations lead"},
    "evidence_absent": {"reason": "Trial notes are still being written up."},
    "decided_at": "2026-11-20T15:00:00Z"
  }
}
```

Then check it, record it and read it back:

```bash
.local/project-record --project $P --as rowan validate .local/decision.json
.local/project-record --project $P --as rowan record .local/decision.json > .local/result.json
.local/project-record --project $P resolve .local/result.json   # structured entry
.local/project-record --project $P render .local/result.json    # Markdown page
.local/project-record --project $P history decision dec-intake-queue
```

Every command runs in its own process, so `resolve` reopens the store the
`record` command closed. Running `record` again with the same request and
operation key returns the original receipt. The store and the search view live
under `.local/`; delete that directory to start over.

Exit status is 0 on success, 3 for a refused, conflicting or indeterminate
outcome (the JSON explains it and says what to do next), 2 for usage errors and
1 for other errors.

## The operation

`projectwriter.Writer.Record` takes a request with a project, a kind, a record
ID, an operation key, the entry, and, for a revision, the exact predecessor pin.
It returns one of:

| Outcome | Meaning |
| --- | --- |
| `accepted` | The store accepted the entry. The result carries the project, kind, authority, the exact pin and the store receipt. A retry with the same key and request returns the same receipt. |
| `conflict` | `idempotency_conflict`: the key was used for different content. `revision_conflict`: another revision of the predecessor won; `current` is its pin. |
| `refused` | A check failed before anything was written: `unknown_kind`, `invalid_payload`, `untrusted_descriptor`, `unauthorized_writer`, `classification_below_floor`, `git_authority` and others. |
| `indeterminate` | The store could not report the outcome. `Reconcile` with the same operation key resolves it while the key is inside the store's idempotency window; outside it, the result stays `indeterminate` (`coverage_unprovable`). |

The CLI is a thin layer over the same functions, so a later control-plane
caller can expose the same request, outcomes and reason codes.

## Who supplies what

| Supplied by | |
| --- | --- |
| recordprims | Idempotent append with a receipt; conflict on key reuse; reconciliation inside the idempotency window and an explicit unknown outside it; predecessor-pinned revisions with one winner; revision history and head; holds enforced on the serving path; a confined local store with a single-writer lock that a second process can reopen; canonical instants. |
| This example (demonstrated, not qualified) | Project configuration and stream mapping; the type registry and descriptor lookup; payload validation; writer policy; binding the project and descriptor revision inside the record's payload; receipt presentation; lookup, history and Markdown rendering tied to a pin; a rebuildable search view. |
| You, before production use | Authentication of the caller: the example takes the writer's name from local configuration and does not authenticate it. A trusted source for descriptors: here the registry is a local file pinned by sha256 in the project file. A live Git or forge integration, and any durability claim for Git. Qualification of the storage backend beyond this local demonstration. Signing and key management. Retention and classification policy beyond the floors shown. |

## Projects, kinds and descriptors

`project/harbor-demo.json` names the project, pins the registry by sha256,
maps each enabled kind to a stream and one authority, and lists the admitted
writers and their classes. The `runbook-decision` kind uses the decision
descriptor but is accepted by a reviewed Git merge instead of the store, so
`record` refuses it with `git_authority`; see `gitreview` below.

`registry/registry.json` lists descriptors. Each names its owner, revision,
payload contract, pinned schema (path and sha256), revision policy, writer
class, classification floor and an example. `risk` is a project-defined kind
(owner `example.org/harbor-demo`); it is written through the same store and
envelope as the shared kinds.

A record's payload is `{project, descriptor: {kind, owner, revision, digest},
entry}`. The descriptor digest is the RFC 8785 sha256 of the descriptor as
listed in the registry. To check a historical record, find the descriptor with
that kind, revision and digest (the registry keeps earlier revisions), then the
schema by its pinned digest. A schema file that no longer matches its pin marks
its descriptor untrusted; a registry that no longer matches the project's pin
is refused. The typed checks in `projectwriter` name the schema revisions they
implement; a descriptor that pins any other schema is refused with
`unsupported_schema`, so a changed schema cannot drift from the checks.

Writers: a `maintainer` may write any kind and place holds; an `agent` or
`external` writer may write only kinds whose descriptor names its class.

### Fields shared with an agent content kind registry

The descriptor reuses field names from an existing agent content kind registry
where the meaning is the same, rather than adding a new vocabulary:

| This example | Kind registry | Note |
| --- | --- | --- |
| `kind` | `kind` | Same pattern. |
| `payload_contract` | `payload_contract` | Same host-less token form. |
| `writer_class` | `writer_class` | Same values (`agent`, `maintainer`, `external`). |
| `revision_policy` | `revision_policy` | `revisioned` and `single`; `append` is not used here. |
| `carrier` | `carrier` | Only `payload`. |
| `classification.sensitivity`, `classification.retention-lifecycle` | `classification` | The floor; `access-tier` has no field in a durable-record/v1 record, so it is omitted. |
| `stream_volatility` | `stream_volatility` | Same values. |
| `owner`, `descriptor_revision`, `schema {path, digest}`, `example` | none | Project-specific: who owns the kind, which revision a record used, and where its pinned schema and example are. |

## Revisions, corrections and reversals

A correction is a new revision of the same record whose request names the exact
predecessor pin. The original revision stays resolvable. A reversal is a new
decision whose entry names the reversed decision's pin in `reverses`. The record
also carries a `links` entry with `rel: "reverses"`, and the search view finds
it with `ReversalsOf`.

## Reads and holds

`Lookup`, `Render` and the search view read through the store's serving path
(`Release` with purpose `serve`). A held revision, or a retired record, is not
served, so it does not appear in any of them. `Hold` places an operator hold on
a revision; its request carries the operator's `observed_at`, so a retry with
the same fields and key returns the original receipt. The search view is
derived only from served records: `reindex` rebuilds it, and deleting it loses
nothing. `search` also drops any hit the store no longer serves, so a hold
placed after the last `reindex` takes effect at once.

## Producer time

Entry instants (`decided_at`, `observed_at`) and the optional request
`produced_at` may be given at whole seconds, milliseconds or microseconds. They
are converted to the canonical six-digit spelling with
`recordprims.NormalizeProducerInstant` before the record digest is computed, so
`...00Z` and `...00.000000Z` produce the same record. The schemas describe the
stored, canonical form.

## Git-reviewed decisions (`gitreview`)

`gitreview` shows the other acceptance point with a local repository driven by
the `git` binary:

- `Propose` commits the validated entry on a proposal branch and returns a
  proposal reference: the branch, the proposal commit and its base.
- `Revalidate` checks the proposal against the current head of the designated
  branch, which a merge would use as its first parent. A proposal whose base
  has moved is stale; it is still accepted if the new head did not change its
  file, and refused as a competing edit if it did. The result is bound to the
  proposal commit, its file and the digest of the validated entry.
- `Accept` takes a revalidation of that proposal only. It builds the merge
  commit, checks its parents and the accepted file's digest, and only then
  advances the designated branch, and only if the branch still points at the
  validated parent. It returns the acceptance reference: the merge commit, its
  validated parent, the proposal commit and the digest of the accepted file.

This is a fixture. It makes no durability claim for Git, uses no forge, and its
references carry no signature.

## Tests

```bash
go test ./...
```

From the repository root, `make example-check` runs these tests and validates
the registry's examples against its schemas (`make example-schemas`).
