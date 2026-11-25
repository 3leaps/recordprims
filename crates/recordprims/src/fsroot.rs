// SPDX-License-Identifier: MIT OR Apache-2.0

//! Confined store root directory handle (`internal/fsroot` parity).
//!
//! Every relative path is validated to prevent escaping the root (`..`, leading
//! slashes, backslashes, drive prefixes, and symlinks are refused), and every
//! directory/file opened through [`Root`] is checked for ownership and
//! owner-only permissions.

use std::fs::File;
use std::io;
use std::path::Path;

use crate::error::Error;
use crate::sys::imp;

/// Filesystem verification options.
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq)]
pub struct Options {
    /// Admit `tmpfs` on Linux (used in test harnesses).
    pub allow_tmpfs: bool,
    /// Admit `overlayfs` on Linux (used in container test harnesses).
    pub allow_overlay: bool,
}

/// Internal classification of `fsroot` failures before mapping to [`Error`].
#[derive(Debug)]
pub(crate) enum FsRootError {
    NotDir,
    NotRegular,
    MultipleLink,
    Symlink,
    Owner,
    Perm,
    FsType,
    Path,
    Io(&'static str, io::Error),
}

impl FsRootError {
    /// Maps to the public error. A refusal keeps the outcome marker: met after
    /// a durable step (`indeterminate`), it is `store_invalid` with an
    /// indeterminate outcome.
    pub(crate) fn into_store_error(self, default_op: &'static str, indeterminate: bool) -> Error {
        let refused = |detail: &str| {
            if indeterminate {
                Error::StoreInvalidAfterProgress(detail.to_owned())
            } else {
                Error::store_invalid(detail)
            }
        };
        match self {
            Self::NotDir => refused("not a directory"),
            Self::NotRegular => refused("store entry is not a regular file"),
            Self::MultipleLink => refused("hardlinked file refused"),
            Self::Symlink => refused("symlink"),
            Self::Owner => refused("wrong owner"),
            Self::Perm => refused("group or other permissions"),
            Self::FsType => refused("filesystem type not allowed"),
            Self::Path => refused("path is outside the store root"),
            Self::Io(op, e) => Error::io(
                if op.is_empty() { default_op } else { op },
                &e,
                indeterminate,
            ),
        }
    }

    pub(crate) fn into_open_root_error(self) -> Error {
        if self.is_not_found() {
            return Error::store_invalid("not found");
        }
        self.into_store_error("open", false)
    }

    pub(crate) fn is_not_found(&self) -> bool {
        matches!(self, Self::Io(_, e) if e.kind() == io::ErrorKind::NotFound)
    }

    #[allow(dead_code)]
    pub(crate) fn is_structural(&self) -> bool {
        matches!(
            self,
            Self::NotDir
                | Self::NotRegular
                | Self::MultipleLink
                | Self::Symlink
                | Self::Owner
                | Self::Perm
                | Self::FsType
                | Self::Path
        )
    }
}

/// Confined handle to a verified store root directory.
pub(crate) struct Root {
    #[cfg(unix)]
    inner: std::os::fd::OwnedFd,
    #[cfg(windows)]
    inner: imp::WinRoot,
}

impl Root {
    /// Open `dir` as a confined store root after verifying ownership, mode/DACL,
    /// and filesystem type.
    pub(crate) fn open(dir: &Path, opts: Options) -> Result<Self, FsRootError> {
        let meta = std::fs::symlink_metadata(dir).map_err(|e| FsRootError::Io("stat", e))?;
        if meta.file_type().is_symlink() {
            return Err(FsRootError::Symlink);
        }
        if !meta.is_dir() {
            return Err(FsRootError::NotDir);
        }
        let inner = imp::open_root_dir(dir, opts)?;
        Ok(Self { inner })
    }

    /// Verify a relative entry (`want_dir == true` for directory, `false` for regular file)
    /// and return its byte size.
    pub(crate) fn stat_verified(
        &self,
        rel: &str,
        want_dir: bool,
        opts: Options,
    ) -> Result<u64, FsRootError> {
        if rel == "." && want_dir {
            return imp::stat_verified(&self.inner, &[], true, opts);
        }
        let parts = clean_parts(rel)?;
        imp::stat_verified(&self.inner, &parts, want_dir, opts)
    }

    /// Open a verified regular file for reading and return `(File, size)`.
    pub(crate) fn open_verified(
        &self,
        rel: &str,
        opts: Options,
    ) -> Result<(File, u64), FsRootError> {
        let parts = clean_parts(rel)?;
        imp::open_verified_read(&self.inner, &parts, opts)
    }

    /// Create a new regular file (`O_CREAT | O_EXCL`) with owner-only `mode`.
    pub(crate) fn create_exclusive(
        &self,
        rel: &str,
        mode: u32,
        opts: Options,
    ) -> Result<File, FsRootError> {
        let parts = clean_parts(rel)?;
        imp::create_exclusive(&self.inner, &parts, mode, opts)
    }

    /// Open an existing verified regular file for appending (`O_WRONLY | O_APPEND`).
    pub(crate) fn open_append(&self, rel: &str, opts: Options) -> Result<File, FsRootError> {
        let parts = clean_parts(rel)?;
        imp::open_append(&self.inner, &parts, opts)
    }

    /// Open an existing verified regular file for writing without `O_APPEND` (used for rollback truncate).
    pub(crate) fn open_write(&self, rel: &str, opts: Options) -> Result<File, FsRootError> {
        let parts = clean_parts(rel)?;
        imp::open_write(&self.inner, &parts, opts)
    }

    /// Open or create the store `LOCK` file (`0o600`) inside the confined root.
    pub(crate) fn open_lock_file(&self, rel: &str, opts: Options) -> Result<File, FsRootError> {
        let parts = clean_parts(rel)?;
        imp::open_lock_file(&self.inner, &parts, opts)
    }

    /// Create or verify a subdirectory with mode `0o700`.
    pub(crate) fn mkdir(&self, rel: &str, opts: Options) -> Result<(), FsRootError> {
        let parts = clean_parts(rel)?;
        imp::mkdir_verified(&self.inner, &parts, opts)
    }

    /// Rename a relative file inside the root.
    pub(crate) fn rename(
        &self,
        old_rel: &str,
        new_rel: &str,
        opts: Options,
    ) -> Result<(), FsRootError> {
        let old_parts = clean_parts(old_rel)?;
        let new_parts = clean_parts(new_rel)?;
        imp::rename_in_root(&self.inner, &old_parts, &new_parts, opts)
    }

    /// Unlink a relative file inside the root.
    pub(crate) fn remove(&self, rel: &str, opts: Options) -> Result<(), FsRootError> {
        let parts = clean_parts(rel)?;
        imp::remove_in_root(&self.inner, &parts, opts)
    }

    /// `fsync` a relative directory inside the root (`"."` for the root itself).
    pub(crate) fn sync_dir(&self, rel: &str, opts: Options) -> Result<(), FsRootError> {
        if rel == "." {
            return imp::sync_dir_in_root(&self.inner, &[], opts);
        }
        let parts = clean_parts(rel)?;
        imp::sync_dir_in_root(&self.inner, &parts, opts)
    }

    /// `fsync` the root directory itself.
    #[allow(dead_code)]
    pub(crate) fn sync(&self, opts: Options) -> Result<(), FsRootError> {
        self.sync_dir(".", opts)
    }

    /// Read sorted entry names in a relative directory (`"."` for the root itself).
    pub(crate) fn read_dir_names(
        &self,
        rel: &str,
        opts: Options,
    ) -> Result<Vec<String>, FsRootError> {
        if rel == "." {
            return imp::read_dir_names_in_root(&self.inner, &[], opts);
        }
        let parts = clean_parts(rel)?;
        imp::read_dir_names_in_root(&self.inner, &parts, opts)
    }
}

fn clean_parts(rel: &str) -> Result<Vec<&str>, FsRootError> {
    if rel.is_empty()
        || rel.starts_with('/')
        || rel.starts_with('\\')
        || rel.contains('\\')
        || rel.contains(':')
        || rel.contains('\0')
    {
        return Err(FsRootError::Path);
    }
    let mut parts = Vec::new();
    for seg in rel.split('/') {
        if seg.is_empty() || seg == "." || seg == ".." {
            return Err(FsRootError::Path);
        }
        parts.push(seg);
    }
    if parts.is_empty() {
        return Err(FsRootError::Path);
    }
    Ok(parts)
}
