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
version and one shared conformance suite. Go is the first, and the tree under
active development; the Rust implementation is held to the same suite before
0.1.0. The first stratum is the local filesystem.

## Implementations and bindings

| Language | Package | Status |
| --- | --- | --- |
| Go | `github.com/3leaps/recordprims` | in development; the first implementation |
| Rust | `recordprims` (crates.io) | planned; held to the same conformance suite |
| TypeScript | `@3leaps/recordprims` (npm), from the Rust implementation | planned |
| Python | `recordprims` (PyPI), from the Rust implementation | planned |

The Go module is cgo-free and needs Go 1.26 or newer.

## Status

Pre-release (0.0.x). The Go module has the store foundation: a verified,
confined store root, the store lock, and content-addressed bodies with
publication leases and removal. Records, the journal, restrictions and release
are not yet implemented, and the API will change before 0.1.0.

## Releases

Each release is one signed tag, `vX.Y.Z`, covering the Go module and the Rust
crate. See [RELEASE_NOTES.md](RELEASE_NOTES.md) for recent releases,
[CHANGELOG.md](CHANGELOG.md) for the full history, and
[docs/security/README.md](docs/security/README.md) for how to verify a release.

## License

Licensed under either of [Apache License, Version 2.0](LICENSE-APACHE) or
[MIT license](LICENSE-MIT) at your option. See [NOTICE](NOTICE) for project
name and trademark notices.
