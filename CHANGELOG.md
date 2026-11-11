# Changelog

All notable changes to this project are documented in this file.

Format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).
This project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

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

[Unreleased]: https://github.com/3leaps/recordprims/compare/v0.0.2...HEAD
[0.0.2]: https://github.com/3leaps/recordprims/compare/v0.0.1...v0.0.2
[0.0.1]: https://github.com/3leaps/recordprims/tree/v0.0.1
