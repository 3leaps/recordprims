# recordprims

Durable records for programs that must outlive their writers.

recordprims is a library-first store that implements the
[`durable-record/v1`](https://github.com/3leaps/crucible) contract: stable
record identity, exact revisions bound to digests, keyed idempotent writes,
typed receipts, and use restrictions (holds) that block serving, export and
reuse while keeping records readable for authorized audit. The records it
writes are meant to stay readable by anyone who reads the contract, without
reading the program.

There are two implementations, in pure Go and Rust, under one contract, one
version and one shared conformance suite. The Go module is the supported
implementation; the Rust crate is a preview. The first stratum is the local
filesystem.

## Implementations and bindings

| Language | Package | Status |
| --- | --- | --- |
| Go | `github.com/3leaps/recordprims` | supported; the first implementation |
| Rust | `recordprims` (crates.io) | preview, not yet published: store root, lock, bodies and plain streams (open, append, retire, protect, head, revisions, reconcile, release); continuations and compaction to follow |
| TypeScript | `@3leaps/recordprims` (npm), from the Rust implementation | planned |
| Python | `recordprims` (PyPI), from the Rust implementation | planned |

The Go module is cgo-free and needs Go 1.26 or newer. The language and binding
plan is recorded in [ADR-0001](docs/decisions/ADR-0001-profile-b-twin-and-bindings.md).

## Status

0.1.0. What each surface promises:

| Surface | Status |
| --- | --- |
| `durable-record/v1` records written by the Go module | Conform to `durable-record/v1` as pinned, and later releases keep reading them. The contract itself is proposed and experimental upstream. |
| Go module | Supported. Any breaking API change before 1.0 comes in a new minor release and is listed in the changelog. |
| Local filesystem store format (`recordprims-fs/v2`) | Later 0.x releases open a store written by 0.1.0, or a store-format change ships with a documented migration and a changelog entry. |
| Rust crate | Preview, not published. Plain streams only. |
| Bucket stratum (`internal/stratum/bucketcas`) | Experimental and internal; no public API, not tested against a cloud storage provider. |
| TypeScript and Python bindings | Planned. |

Not claimed: use of one store from more than one host, network or cloud
storage, or survival of power loss.

The Go module implements the `durable-record/v1` store (`recordprims-fs/v2`),
passing all 400 vectors of the pinned `3leaps/crucible` `v0.2.10` contract: a
verified, confined store root, the store lock, content-addressed bodies with
designated-stream lifecycle and removal eligibility, streams (`OpenStream`,
`Append`, `Retire`, `Protect`, `Reconcile`, `Head`, `Revisions`, `Release`),
continuations, detached stream seals, sealed-continuation compaction with
predecessor reclamation, and watermarked local stream indexes.

The Rust crate implements the verified store root, the store lock,
content-addressed bodies and plain streams: open, append, retire, protect,
head, revisions, reconcile and release. It runs the `durable-record/v1`
vectors with its own ledger. A differential check runs the same operations
through both implementations and requires identical bytes, and each opens
stores the other wrote. Rust continuations and compaction, and the TypeScript
and Python bindings, come in later releases.

Storage backends sit behind an internal seam; the guarantees a backend must
provide are described in [docs/storage-strata.md](docs/storage-strata.md).

## Example: record a project decision or update

[`examples/project-record-writer`](examples/project-record-writer/) records
project decisions and updates through the public Go API and reads them back as
structured data or Markdown. It is a separate module with no compatibility
promise, and is not part of a release.

## Releases

Each release is one signed tag, `vX.Y.Z`, covering the Go module (and the Rust
crate once it is published). See [RELEASE_NOTES.md](RELEASE_NOTES.md) for recent releases,
[CHANGELOG.md](CHANGELOG.md) for the full history, and
[docs/security/README.md](docs/security/README.md) for how to verify a release.

## License

Licensed under either of [Apache License, Version 2.0](LICENSE-APACHE) or
[MIT license](LICENSE-MIT) at your option. See [NOTICE](NOTICE) for project
name and trademark notices.
