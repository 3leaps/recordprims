// SPDX-License-Identifier: MIT OR Apache-2.0

//! Verified cut-bounded journal line appender (`internal/jfile` parity).
//!
//! Appends complete LF-terminated JSON lines under the store lock at a
//! caller-verified cut offset. On the designated body-removal stream, a
//! provably incomplete write due to out-of-space/quota is rolled back to the
//! cut and reported as `JFileError::Aborted` (determinate); any ambiguous
//! failure latches the journal on the `Appender` until the store is reopened.

use std::collections::HashMap;
use std::io::{self, Write};
use std::sync::Mutex;

use crate::error::out_of_space;
use crate::fsroot::{FsRootError, Options, Root};
use crate::sys::imp::same_file;

/// Classified outcome of [`Appender::append`].
#[derive(Debug, Clone)]
pub(crate) enum JFileError {
    /// Journal file size did not match the caller's verified `cut`.
    CutMismatch,
    /// Partial write on the designated stream was rolled back to `cut` and fsynced (determinate).
    Aborted,
    /// Write or fsync outcome is unknown; journal is latched until reopen.
    Indeterminate,
    /// Rollback failed or journal was already latched by an earlier outcome.
    Poisoned,
    /// Pre-write open/stat failed before touching the journal.
    PreWriteIo(&'static str, io::ErrorKind, Option<i32>),
    /// Structural `fsroot` refusal before touching the journal.
    Structural(String),
}

/// Per-store-handle journal appender with per-journal poison latch.
pub(crate) struct Appender {
    opts: Options,
    latched: Mutex<HashMap<String, JFileError>>,
}

impl Appender {
    pub(crate) fn new(opts: Options) -> Self {
        Self {
            opts,
            latched: Mutex::new(HashMap::new()),
        }
    }

    pub(crate) fn append(
        &self,
        root: &Root,
        name: &str,
        cut: u64,
        line: &[u8],
        designated: bool,
    ) -> Result<(), JFileError> {
        if self
            .latched
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .contains_key(name)
        {
            return Err(JFileError::Poisoned);
        }
        let res = self.append_once(root, name, cut, line, designated);
        if let Err(ref err @ (JFileError::Poisoned | JFileError::Indeterminate)) = res {
            self.latched
                .lock()
                .unwrap_or_else(|e| e.into_inner())
                .insert(name.to_owned(), err.clone());
        }
        res
    }

    fn append_once(
        &self,
        root: &Root,
        name: &str,
        cut: u64,
        line: &[u8],
        designated: bool,
    ) -> Result<(), JFileError> {
        if line.len() < 2 || line.last() != Some(&b'\n') || line[..line.len() - 1].contains(&b'\n')
        {
            return Err(JFileError::Structural(
                "jfile: line must be one non-empty line ending in a newline".into(),
            ));
        }
        let mut f = root.open_append(name, self.opts).map_err(map_open_err)?;
        let meta = f
            .metadata()
            .map_err(|e| JFileError::PreWriteIo("stat", e.kind(), e.raw_os_error()))?;
        if meta.len() != cut {
            return Err(JFileError::CutMismatch);
        }
        match f.write(line) {
            Ok(n) if n == line.len() => {
                f.sync_all().map_err(|_| JFileError::Indeterminate)?;
                Ok(())
            }
            Ok(n) => {
                // Short write with no OS error: provably incomplete.
                self.rollback_or_indeterminate(root, &f, name, cut, n, line.len(), designated)
            }
            Err(werr) => {
                if !out_of_space(&werr) && werr.kind() != io::ErrorKind::WriteZero {
                    return Err(JFileError::Indeterminate);
                }
                self.rollback_or_indeterminate(root, &f, name, cut, 0, line.len(), designated)
            }
        }
    }

    #[allow(clippy::too_many_arguments)]
    fn rollback_or_indeterminate(
        &self,
        root: &Root,
        append_file: &std::fs::File,
        name: &str,
        cut: u64,
        _wrote: usize,
        _total: usize,
        designated: bool,
    ) -> Result<(), JFileError> {
        if !designated {
            return Err(JFileError::Indeterminate);
        }
        let tf = root
            .open_write(name, self.opts)
            .map_err(|_| JFileError::Poisoned)?;
        if !same_file(append_file, &tf) {
            return Err(JFileError::Poisoned);
        }
        tf.set_len(cut).map_err(|_| JFileError::Poisoned)?;
        tf.sync_all().map_err(|_| JFileError::Poisoned)?;
        let meta = append_file.metadata().map_err(|_| JFileError::Poisoned)?;
        if meta.len() != cut {
            return Err(JFileError::Poisoned);
        }
        Err(JFileError::Aborted)
    }
}

fn map_open_err(err: FsRootError) -> JFileError {
    match err {
        FsRootError::Io(op, e) => JFileError::PreWriteIo(op, e.kind(), e.raw_os_error()),
        other => JFileError::Structural(other.into_store_error("open", false).to_string()),
    }
}
