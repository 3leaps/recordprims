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

## Status

Pre-release. Nothing is published yet, and the API will change before the
first tag.
