# recordprims (Rust)

Durable records for programs that must outlive their writers.

This crate is the Rust implementation of recordprims, a library-first store
for the [`durable-record/v1`](https://github.com/3leaps/crucible) contract:
stable record identity, exact revisions bound to digests, keyed idempotent
writes, typed receipts, and use restrictions (holds) that block serving,
export and reuse while keeping records readable for authorized audit.

The crate is a preview. It implements the verified store root, the store
lock, content-addressed bodies and plain streams: open, append, retire,
protect, head, revisions, reconcile, release and read by pin. Continuations
and compaction are implemented in the Go module and come to this crate in
later releases. Stores written by either implementation open in the other.

The project home, the Go module, the changelog and the design records are at
<https://github.com/3leaps/recordprims>.

## License

Licensed under either of [Apache License, Version 2.0](LICENSE-APACHE) or
[MIT license](LICENSE-MIT) at your option. See [NOTICE](NOTICE).
