# Storage strata

A *stratum* is the physical storage beneath a recordprims store. Journal
semantics (streams, revisions, receipts, holds, continuations) are defined by
the `durable-record/v1` contract and are the same on every stratum. What
changes is how bytes reach durable storage and how a writer learns whether a
write took effect.

Two strata exist today:

| Stratum | Store format | Status |
| --- | --- | --- |
| `journal-files` | `recordprims-fs/v2` | The local filesystem store behind the public Go API. |
| `ndjson-cas` | bucket CAS | Internal. A conditional-write object store layout; not yet reachable through the public API. |

The stratum seam is internal and may change before 0.1.0. This document
records the guarantees a backend must give, so that backend implementations and
test harnesses can be built against them.

## Filesystem stratum

The filesystem stratum runs inside a verified store root: paths are confined to
the root, symlinks are refused, and files and directories must be owned by the
running user with no group or other access. On Windows the ACL may grant only
the owner, SYSTEM, the Administrators group, CREATOR OWNER and OWNER RIGHTS. A
cross-process advisory lock admits one writer per store. Journal appends are
written and synced before a receipt is returned; bodies are staged in a
temporary file, synced and then linked into place before any record refers to
them.

The declared failure model is `survives_process_loss`: an accepted write with a
receipt survives the writer process being killed. Power loss is not claimed.

## Object-store stratum (`ndjson-cas`)

The object-store stratum keeps each stream as segmented NDJSON objects plus a
small `head.json` that names the committed segments. Every state change is a
conditional write of `head.json`, so concurrent writers are linearized by the
backend rather than by a lock.

### Required backend capabilities

A backend must provide all three of the following. A store refuses to
initialize or open on a backend that does not advertise them, and never falls
back to unconditional writes.

| Capability | Meaning | S3-style request |
| --- | --- | --- |
| Create-only write | Write a key only if it does not exist. | `PUT` with `If-None-Match: *` |
| Compare-and-swap write | Write a key only if its current validator matches. | `PUT` with `If-Match: <etag>` |
| Coherent read | One read returns the full bytes and the validator of those same bytes, with read-after-write consistency. | `GET` returning body and `ETag` |

The backend must also list keys under a prefix in byte order and delete keys.

### Write outcomes

Every conditional write and every delete reports one of four outcomes:

| Outcome | Meaning |
| --- | --- |
| `not_started` | The request failed before it was sent. Nothing changed. |
| `not_committed` | The backend rejected the write without changing state, for example a failed precondition (HTTP 412). The rejection names its kind: `already_exists`, `token_mismatch` or `other`. |
| `committed` | The backend acknowledged the write as durable. |
| `unknown` | The request was sent and the outcome is not known, for example a dropped connection or a timeout after sending. |

The store holds backends to one rule: **a write succeeded if and only if it
reports `committed` with no error.**

- Any other outcome reported without an error is treated as a failure, and
  the outcome is kept.
- An error reported together with `committed` is treated as `unknown`.

### How the store uses outcomes

State advances only through a `committed` write of a stream's `head.json`.

Other writes are handled by kind:

- **Segments and checkpoints** are content-addressed: the key embeds the
  SHA-256 of the bytes. A create-only write that reports `already_exists` is
  accepted as the same content without reading it back, whatever commit
  outcome accompanies it, and the operation continues to the head write. Any
  other failure, including `unknown` without `already_exists`, stops the
  operation before the head is written.
- **Bodies** are compared byte for byte. A create-only write that reports
  `already_exists` is accepted only when the stored bytes are identical;
  otherwise the operation is refused.
- **The reclamation summary** returns an indeterminate error on `unknown`
  before any comparison. A determinate `already_exists` is accepted only when
  the stored bytes are identical.
- **Head writes** that report `unknown` during an append are followed by a
  read of the head. If the record is found committed, the original receipt is
  returned; otherwise the caller receives an indeterminate error.
- **Reclamation** returns an indeterminate error on an `unknown` write of the
  head, as for the summary, before any segment is deleted.

Retrying an operation after an indeterminate error converges: it either finds
the earlier write in place and continues, or performs it.

### Compaction and reclamation

A stream can be sealed and continued into a compacted successor stream. Once
the successor is established, the sealed predecessor's segments can be
reclaimed. Reclamation first publishes a write-once summary of the predecessor
(its historical cuts and pin sequences, so continuations opened at earlier cuts
remain verifiable), then records the reclaiming successor in the predecessor's
head through a compare-and-swap, and only then deletes segments. A second
reclaimer that finds the head already reclaimed by the same successor re-issues
the segment deletes; it never rewrites the summary or the head. A reclaimer
naming a different successor is refused.

## Testing a backend

The bucket stratum is tested against an in-memory backend that can inject each
outcome on any key, including `unknown` with and without the write having
landed. The append, compaction and reclamation paths have tests showing that an
unknown or contract-violating outcome causes no destructive progress and that a
retry converges. Tests against live S3-compatible services are not yet part of
the suite.
