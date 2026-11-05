---
id: "ADR-0001"
title: "Profile B: pure-Go and Rust twins, with TypeScript and Python bound to the Rust tree"
status: "accepted"
date: "2026-11-05"
---

# ADR-0001: Profile B: Pure-Go and Rust Twins, with TypeScript and Python Bound to the Rust Tree

## Context

recordprims is a prim: a library with a commitment to Go, Rust, TypeScript and
Python consumers. The Go consumers must not need cgo. A Rust core with a C ABI
consumed from Go through cgo would break that, so recordprims follows Profile B
of the crucible
[repository-classes standard](https://github.com/3leaps/crucible/blob/main/docs/standards/repository-classes.md):
two native implementations, one in pure Go and one in Rust, in one repository.

Unlike prims whose work is a pure function of bytes in and bytes out,
recordprims is a filesystem store. Its contract depends on the operating
system: path resolution confined to the store root, owner and permission (or
DACL) verification of that root, an advisory cross-process lock, ordered
fsync, and publication by rename. Any binding has to keep those guarantees.

## Decision

1. **Two native trees, one contract.** The pure-Go tree at the repository root
   (`github.com/3leaps/recordprims`) and the Rust tree under `crates/` implement
   `durable-record/v1` as pinned in `contract/PIN.md`. They share one version
   and one signed `vX.Y.Z` tag. The Go tree builds with `CGO_ENABLED=0`, and
   `scripts/deps-check.sh` refuses cgo. No C ABI or cgo binding is planned.

2. **Bootstrap order.** Go is the operating tree while the repository is
   `profile_status: bootstrapping`; today it is exercised by `make check`, and
   once the shared conformance suite exists only it is graded against it. The
   Rust tree is a scaffold. The Rust implementation follows once the Go journal and
   content writes have landed. The repository flips to `profile_status: locked`
   only when both trees pass the shared conformance suite. The flip gets a
   release-notes entry.

3. **What is normative.** Neither implementation is the reference. Expected
   behavior is defined by the pinned contract and by conformance vectors
   authored as data, never snapshotted from either tree. A differential
   check between the trees (identical operation scripts producing
   byte-identical journals and bodies, each tree opening and verifying a store
   the other wrote, one error-set vector per published error) proves that the
   trees agree, not that they are right: output both trees share must still
   pass the vectors. Byte parity does not qualify crash, durability, locking or
   ownership behavior. The platform and failure-model cells both trees are
   held to will be declared alongside the conformance suite.

4. **TypeScript comes from the Rust tree through napi-rs.** It ships as
   `@3leaps/recordprims` on npm, from an isolated `bindings/typescript`
   workspace, so napi-rs stays out of the published crates' dependency graph.
   A WebAssembly module, as other prims use, cannot meet this store's contract.
   `wasm32-unknown-unknown` has no filesystem. Node's WASI host is documented
   as not a security sandbox and cannot demonstrate root confinement, owner
   verification or advisory locking. A read-only WebAssembly verifier may be
   added later if a consumer needs one. It will never write to a store.

5. **Python comes from the Rust tree through PyO3.** It ships as `recordprims`
   on PyPI, an `abi3` wheel built with maturin from an isolated
   `bindings/python` workspace.

6. **Callbacks across a binding boundary.** The contract has the store hand
   frame readers to a caller's callback, revoke them when the callback
   returns, and refuse reentry from inside the callback. The Go API for this
   has not landed yet; only its error values (`ErrReaderRevoked`,
   `ErrSinkReentered`) exist. Before a binding's API is fixed, its
   design must state where callbacks run and how reentry is detected there.
   A thread-local marker set on a Rust worker thread does not follow a callback
   dispatched onto the JavaScript main thread or into Python. Each binding is
   qualified with tests for retained readers, in-flight reads,
   exceptions or cancellation, and callback reentry.

7. **Unsafe Rust.** The Rust workspace forbids unsafe code today. When the
   Windows ownership, DACL and `LockFileEx` support lands, the crate moves to
   denying unsafe code, and that support lives in one module that calls
   `windows-sys`. That module carries the only allowance, has safe
   internal wrappers and stated handle and buffer ownership invariants, and is
   security-reviewed. A check added with that module will refuse any other
   allowance. The module is an
   audit boundary, not permission for unsafe code in its callers. Unix support
   uses safe interfaces.

## Consequences

- `.fulmen/repo.yaml` keeps `profile: B`, `profile_status: bootstrapping` and
  `leaf: false` (recordprims depends on canonprims). Each binding stays
  `planned` until its implementation, conformance and differential receipts
  exist and its package is published.
- Each tree pins the same canonprims release.
- The shared conformance suite, the declared platform and failure-model
  cells, and the differential check are built before the Rust implementation,
  so the second tree is graded from its first commit.
- The check that refuses any unsafe-code allowance outside the Windows module
  lands with that module.
- A capability only one tree can provide is a declared extension with an
  explicit `unsupported` vector, never a silent difference.
