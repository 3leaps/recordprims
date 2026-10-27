// Package recordprims is a durable record store implementing the
// durable-record/v1 contract on the local filesystem.
//
// It is pre-release (0.0.x). The package provides the store foundation:
// Init and Open create and verify a confined store root, a store-wide lock
// serializes mutations, and PutBody and RemoveBody manage content-addressed
// bodies with publication leases. Records, the journal, restrictions and
// release land in later versions, and the API will change before 0.1.0. A
// Rust implementation is held to the same conformance suite.
package recordprims
