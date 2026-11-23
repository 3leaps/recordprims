// Package recordprims is a durable record store implementing the
// durable-record/v1 contract on the local filesystem.
//
// Streams hold exact, digest-pinned revisions of records with keyed
// idempotent writes, typed receipts, protection holds, continuations and
// sealed-continuation compaction; Release delivers records for serve, export
// or reuse under those holds. Init and Open create and verify a confined store
// root, a store-wide lock serializes mutations, and PutBody and RemoveBody
// manage content-addressed bodies. A Rust implementation is held to the same
// conformance suite.
package recordprims
