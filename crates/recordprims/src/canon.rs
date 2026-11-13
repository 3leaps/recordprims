// SPDX-License-Identifier: MIT OR Apache-2.0

//! Internal wrapper over `canonprims` for RFC 8785 JCS canonicalization and
//! SHA-256 digest computation.
//!
//! No `canonprims` type is re-exported from `recordprims`.

use canonprims_core::{DigestAlg, Limits};

use crate::error::Error;

/// Return the RFC 8785 JCS canonical bytes of `input`.
#[allow(dead_code)]
pub(crate) fn canonicalize(input: &[u8]) -> Result<Vec<u8>, Error> {
    canonprims_json::canonicalize(input, &Limits::default())
        .map_err(|r| Error::schema(format!("jcs canonicalize: {}", r.code.as_str())))
}

/// Return `"sha256:<64 lowercase hex>"` of the RFC 8785 JCS canonical bytes of `input`.
pub(crate) fn digest(input: &[u8]) -> Result<String, Error> {
    canonprims_json::digest(input, &Limits::default(), DigestAlg::Sha256)
        .map_err(|r| Error::schema(format!("jcs digest: {}", r.code.as_str())))
}
