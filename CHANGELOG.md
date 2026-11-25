# Changelog

All notable changes to this project are documented in this file.

Format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).
This project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

- `Stream.ReadServed` / `Stream::read_served`: read one exact pin through the
  serving path. It is a serve release whose selection is that pin, with the
  same hold, retirement and continuation checks at one cut, and it delivers at
  most that revision's frames. A held, retired or restricted revision, a body
  that cannot be delivered under full content, an absent pin and a wrong digest
  all give the one outcome `not_served`; stream-wide refusals keep their
  reason. The result reports the decision cut (absent when none could be
  established), the frames the decision covers and the delivery progress. A
  nil sink evaluates only. A malformed pin is a schema error. Go and Rust tests
  and the differential check cover it
- Example: `Lookup` and `Render` use `ReadServed`
- Contract pin moved to `3leaps/crucible` `v0.2.14` (`38e40aa`). The vendored
  `durable-record/v1` files are unchanged from `v0.2.10`. `record-collection/v0`
  (proposed, experimental) is vendored as a separately pinned family;
  `make contract-check` verifies every family and `make contract-oracle` also
  runs its reference controls. No implementation uses it yet
- Go: a store entry the confined root refuses (symlink, group or other
  permission bits, wrong owner, hardlink, special file, filesystem type) is
  `ErrStoreInvalid` wherever it is met, as in Rust. It was
  `io_error{class=other}` on stream paths. A refusal met after a durable step
  keeps an indeterminate outcome in both: Go `IsIndeterminate` reports it, and
  Rust returns the new `Error::StoreInvalidAfterProgress` (code
  `store_invalid`). Go and Rust tests cover a refused journal and stream
  directory on open, release and append
- Rust crate: packages from its own directory. The shape schemas and license
  texts are copied into `crates/recordprims`, a check keeps each copy equal to
  its source, and `make crate-package` builds the packaged crate

## [0.1.0] - 2026-11-23

- `UseRequest.Selection` documented as it behaves: an explicit selection
  applies to serve, export and reuse; a selected held revision is refused for
  every purpose and a selected retired record is refused for serve. Go and Rust
  tests cover each purpose
- Example: `examples/project-record-writer`, a separate module with no
  compatibility promise, records project decisions and updates through the
  public Go API
- Producer instants: `NormalizeProducerInstant` / `normalize_producer_instant`
  turn a UTC instant observed at whole seconds, milliseconds or microseconds
  into the canonical six-digit spelling, refusing every other spelling;
  `ProducerInstant` / `producer_instant` format a native time value and refuse
  a sub-microsecond remainder instead of truncating it. Go and Rust share the
  vectors in `testdata/producer-instant.json`. The wire spelling and
  `ParseInstant` are unchanged
- Rust crate: verified store root with symlink, owner and permission checks
  (Unix and Windows), the cross-process store lock, content-addressed bodies
  (`put_body`, `remove_body`) and the `durable-record/v1` journal reader
- Rust plain streams: `Store::open_stream`, `Stream::append`, `retire`,
  `protect`, `head`, `revisions`, `reconcile` and `release`, with the derived
  stream indexes.
  Continuation streams and streams carrying compaction state are refused
- Rust conformance driver: the `durable-record/v1` vectors run through the
  Rust journal state, with its own ledger (`conformance/v1-ledger-rust.json`)
- Go/Rust differential check (`make crosscheck`): the same store, body and
  stream operations through both implementations must produce identical bytes,
  and each implementation opens stores written by the other
- Sealed-continuation compaction: `Store.SealStream`, `Store.VerifyStreamSeal`,
  `Store.SealAndCompactStream`, `Store.ReclaimSealedAncestor`,
  `Store.StorageUsage`, `Stream.EstablishRestrictions`,
  `Stream.ContinuationState`, `Stream.ResolveSourcePin` and
  `Stream.StorageUsage`; the bucket stratum gains `SealAndCompactStream` and
  `ReclaimSealedAncestor`, with a detached seal and a write-once reclamation
  summary
- Bucket stratum reclamation returns an indeterminate error on an unknown
  write of its summary or the head, before any segment is deleted
- [docs/storage-strata.md](docs/storage-strata.md): the guarantees a storage
  backend must provide
- Release ceremony and preflight hardening: `make release-github-preflight`
  (and `make release-preflight`) verifies GitHub-hosted workflow token
  permissions, the active `Tag Publish Protection` ruleset on `refs/tags/v*`,
  and immutable releases; tag verification reports the exact workflow-pin or
  signature failure cause without masking it behind a second error line, pins
  explicit OpenPGP `git` invocation flags, and honors `PYTHON` across ceremony
  and doctor scripts

## [0.0.2] - 2026-11-11

The pure-Go `durable-record/v1` store (`recordprims-fs/v2`), passing all 400
vectors of the pinned `3leaps/crucible` `v0.2.10` contract. Pre-release; the
Rust twin and bindings land before 0.1.0.

- Contract pin moved to `3leaps/crucible` `v0.2.10` (`24c8920`), with strict
  wire ingest (`numeric_domain` safe integers, duplicate-key and lone-surrogate
  refusal), bounded Draft 2020-12 schema validation (`strict-rfc` and
  `structural-leap/v1`), and the full 400-vector `durable-record/v1`
  conformance suite passing
- `durable-record/v1` journal engine: stream opening and continuations
  (`continues`, `imported_from`, `restrictions_reviewed`, detached stream
  seals and ancestor closure), content writes (`appended`, `revised`),
  stream-scoped idempotency windows (`live` and `remember`, `deduplicated` and
  `invalid_deduplication`, `accepted_key_reuse`), protection (`tombstoned` and
  `untombstoned` holds), keyed and unkeyed retirement, designated
  `body_removal_stream` lifecycle (`body_published`, `body_removal_started`,
  `body_removed`), removal eligibility (`P1`–`P5`), designated-stream recovery
  (`R1`–`R19`), and bounded out-of-space partial-write abort
- Public Go `Store` and `Stream` API (`recordprims-fs/v2`): `OpenStream`
  (`WithContinues`), `Append`, `Retire`, `Protect`, `Reconcile`, `Head`,
  `Revisions`, and `Release` (with callback-scoped `Frame` reader revocation
  and reentry refusal), designated-stream commit points for `PutBody` and
  `RemoveBody`, and watermarked local stream indexes (`heads.idx`, `keys.idx`,
  `records.idx`)
- Internal `ndjson-cas` conditional-PUT bucket stratum (`internal/stratum/bucketcas`):
  segmented NDJSON journal groups, `head.json` conditional-PUT linearization,
  journal checkpoints (bounding cold-start object reads while retaining full
  journal history), and indeterminate-outcome recovery
- `ParseInstant` accepts only the canonical v1 spelling,
  `YYYY-MM-DDTHH:MM:SS.ffffffZ`, and refuses offsets, other fraction lengths,
  a comma fraction and impossible dates and times
- A panic while `PutBody` stages a body (for example from the input reader) or
  holds the store lock no longer leaves the staged temporary file behind
- Content-addressed body APIs enforce the `durable-record` safe-integer domain
- ADR-0001 records the Profile B language plan: pure-Go and Rust twins under
  one contract, TypeScript (napi-rs) and Python (PyO3) from the Rust tree;
  Go `canonprims` dependency updated to `v0.1.4`
- `LICENSE-APACHE` and `LICENSE-MIT` are the verbatim license texts; the
  dual-license statement and project name notice move to a new `NOTICE` file
- Release and CI updates: generated one-line tag messages (`recordprims X.Y.Z`),
  printed anchor inputs on tag push, and Linux CI gates running inside the
  digest-pinned `goneat-tools-runner-glibc` `v0.5.15` container (`goneat v0.6.8`)

## [0.0.1] - 2026-10-27

The first tag. Pre-release; the API will change before 0.1.0.

- Go store foundation: `Init` and `Open` with a confined, verified store root
  (owner, permissions or DACL, no symlinks, special files or hard links), and
  a filesystem-type allowlist on macOS, Linux and Windows
- A cross-process store lock taken through the confined root
- Content-addressed bodies: `PutBody` and `RemoveBody` with publication leases,
  incarnation records and retention tombstones, idempotent by state, with
  `ErrBodyIncomplete` for retryable incomplete operations
- Published determinate and indeterminate error sets that carry no paths
- Release process: one signed tag per release (`vX.Y.Z`) for the Go module and
  the Rust crate, committed public signing pins and fingerprint anchors read
  from the tagged commit
- Repository checks: the vendored `durable-record/v1` contract pin and its
  oracle, sibling-prim allow-list, exported-API scan, workflow digest pins, a
  vocabulary-free content check, and a maintainer public-surface scan

[Unreleased]: https://github.com/3leaps/recordprims/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/3leaps/recordprims/compare/v0.0.2...v0.1.0
[0.0.2]: https://github.com/3leaps/recordprims/compare/v0.0.1...v0.0.2
[0.0.1]: https://github.com/3leaps/recordprims/tree/v0.0.1
