# recordprims

Durable records for programs that must outlive their writers.

recordprims is a library-first store that implements the
[`durable-record/v1`](https://github.com/3leaps/crucible) contract: stable
record identity, exact revisions bound to digests, keyed idempotent writes,
typed receipts, and use restrictions (holds) that block serving, export and
reuse while keeping records readable for authorized audit. The records it
writes are meant to stay readable by anyone who reads the contract, without
reading the program.

There will be two implementations, in pure Go and Rust, under one contract,
one version and one shared conformance suite. The first stratum is the local
filesystem.

## Implementations and bindings

| Language | Package | Status |
| --- | --- | --- |
| Go | `github.com/3leaps/recordprims` | planned; the first implementation (operating tree while bootstrapping) |
| Rust | `recordprims` (crates.io) | planned; held to the same conformance suite |
| TypeScript | `recordprims` (npm), from the Rust implementation | planned |
| Python | `recordprims` (PyPI), from the Rust implementation | planned |

The Go module is cgo-free.

## Status

Pre-release. Nothing is published yet, and the API will change before the
first tag.
