# Changelog

All notable changes to this project are documented in this file.

Format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).
This project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

- ADR-0001 records the Profile B language plan: pure-Go and Rust twins under
  one contract, TypeScript (napi-rs) and Python (PyO3) from the Rust tree
- A panic while `PutBody` stages a body (for example from the input reader) or
  holds the store lock no longer leaves the staged temporary file behind
- `LICENSE-APACHE` and `LICENSE-MIT` are the verbatim license texts; the
  dual-license statement and project name notice move to a new `NOTICE` file

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

[Unreleased]: https://github.com/3leaps/recordprims/compare/v0.0.1...HEAD
[0.0.1]: https://github.com/3leaps/recordprims/tree/v0.0.1
