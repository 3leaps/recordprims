// SPDX-License-Identifier: MIT OR Apache-2.0

//! Durable record store implementing the `durable-record/v1` contract.
//!
//! This crate is the Rust twin of the pure-Go implementation at the repository
//! root (`github.com/3leaps/recordprims`). Both trees operate over the same
//! `recordprims-fs/v2` on-disk layout and are gated against the same
//! conformance and differential test suites.

mod body;
mod canon;
mod error;
pub mod fsroot;
mod instant;
mod jfile;
mod journal;
mod lock;
mod shape;
mod store;
mod sys;
#[doc(hidden)]
pub mod wire;

#[cfg(test)]
mod tests;

pub use body::{
    Authorization, Provenance, PublicationLease, RemoveBodyOutcome, RemoveBodyReason,
    RemoveBodyRequest, RemoveBodyResult, DEFAULT_PENDING_BODY_TTL,
};
pub use error::{is_indeterminate, DamagedTailKind, Error, ErrorCode, IoClass};
pub use instant::{format_instant, parse_instant, Clock, Instant, SystemClock};
#[doc(hidden)]
pub use lock::FileLock;
pub use store::{
    init, open, BodyRef, InitOptions, OpenOptions, Pin, Profile, ProfileBuild, ProfileDeletion,
    ProfileRuntime, Store, DEFAULT_BODY_REMOVAL_STREAM, DEFAULT_IDEMPOTENCY_WINDOW,
};
