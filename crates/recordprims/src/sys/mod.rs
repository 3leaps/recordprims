// SPDX-License-Identifier: MIT OR Apache-2.0

//! Platform-specific filesystem confinement, ownership/mode/DACL verification,
//! filesystem allowlist checks, and advisory file locking.
//!
//! - On Unix (`unix.rs`), all operations use safe `rustix` interfaces with zero `unsafe`.
//! - On Windows (`windows.rs`), `windows-sys` FFI calls are confined to that
//!   single module under an audited `#![allow(unsafe_code)]` boundary.

#[cfg(unix)]
pub(crate) mod unix;
#[cfg(unix)]
pub(crate) use unix as imp;

#[cfg(windows)]
pub(crate) mod windows;
#[cfg(windows)]
pub(crate) use windows as imp;
