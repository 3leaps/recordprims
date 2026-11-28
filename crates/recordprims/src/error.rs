// SPDX-License-Identifier: MIT OR Apache-2.0

//! Error taxonomy for recordprims, matching the published contract refusal and
//! failure classifications in the pure-Go tree (`errors.go`).
//!
//! No `Error` value ever embeds a filesystem path or raw operating-system
//! message text.

use std::fmt;
use std::io;

/// Closed identifier for each published error class.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash)]
#[non_exhaustive]
pub enum ErrorCode {
    /// Store root or metadata failed structural/permission verification (`store_invalid`).
    StoreInvalid,
    /// Another handle or process holds the store lock (`locked`).
    Locked,
    /// A caller-supplied argument violates the contract schema or domain (`schema`).
    Schema,
    /// The configured authorizer, or same-owner default, refused the operation (`unauthorized`).
    Unauthorized,
    /// A journal failed contract verification (`journal_invalid`).
    JournalInvalid,
    /// A body publication or removal stopped between durable steps (`body_incomplete`).
    BodyIncomplete,
    /// The clock returned an instant earlier than a stream's latest entry (`clock_regression`).
    ClockRegression,
    /// The stream has a `SEALED` marker and refuses ordinary opens (`stream_sealed`).
    StreamSealed,
    /// A continuation names an unsupported capability or missing source stream (`continuation_unsupported`).
    ContinuationUnsupported,
    /// The source or destination state digest in a restriction review is stale (`stale_review`).
    StaleReview,
    /// An operation named a cut that is no longer current (`stale_cut`).
    StaleCut,
    /// The store or stream handle was closed before the operation started (`closed`).
    Closed,
    /// A stream's final line or multi-entry atomic group is incomplete (`damaged_tail`).
    DamagedTail,
    /// A `Release` frame reader was read after its `Frame` callback returned (`reader_revoked`).
    ReaderRevoked,
    /// A `Release` callback re-entered the store (`sink_reentered`).
    SinkReentered,
    /// A `Release` callback returned an error (`sink_error`).
    SinkError,
    /// A classified filesystem/I/O error (`io_error`).
    Io,
}

impl ErrorCode {
    /// Stable wire name of this error code.
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::StoreInvalid => "store_invalid",
            Self::Locked => "locked",
            Self::Schema => "schema",
            Self::Unauthorized => "unauthorized",
            Self::JournalInvalid => "journal_invalid",
            Self::BodyIncomplete => "body_incomplete",
            Self::ClockRegression => "clock_regression",
            Self::StreamSealed => "stream_sealed",
            Self::ContinuationUnsupported => "continuation_unsupported",
            Self::StaleReview => "stale_review",
            Self::StaleCut => "stale_cut",
            Self::Closed => "closed",
            Self::DamagedTail => "damaged_tail",
            Self::ReaderRevoked => "reader_revoked",
            Self::SinkReentered => "sink_reentered",
            Self::SinkError => "sink_error",
            Self::Io => "io_error",
        }
    }
}

impl fmt::Display for ErrorCode {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(self.as_str())
    }
}

/// Operating-system independent classification of an I/O failure.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash)]
#[non_exhaustive]
pub enum IoClass {
    /// Device or quota refused more bytes (`ENOSPC`, `EDQUOT`, `ERROR_DISK_FULL`).
    NoSpace,
    /// Permission denied (`EACCES`, `EPERM`, `ERROR_ACCESS_DENIED`).
    Permission,
    /// Entry not found (`ENOENT`).
    NotFound,
    /// Entry already exists (`EEXIST`).
    Exists,
    /// Read-only filesystem (`EROFS`, `ERROR_WRITE_PROTECT`).
    ReadOnly,
    /// Interrupted system call (`EINTR`).
    Interrupted,
    /// Device I/O error (`EIO`, `ERROR_IO_DEVICE`, `ERROR_CRC`).
    Device,
    /// Any other I/O error.
    Other,
}

impl IoClass {
    /// Stable wire name of this I/O class.
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::NoSpace => "no_space",
            Self::Permission => "permission",
            Self::NotFound => "not_found",
            Self::Exists => "exists",
            Self::ReadOnly => "read_only",
            Self::Interrupted => "interrupted",
            Self::Device => "device",
            Self::Other => "other",
        }
    }
}

impl fmt::Display for IoClass {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(self.as_str())
    }
}

/// Kind of damaged tail on a stream journal.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash)]
pub enum DamagedTailKind {
    /// The final NDJSON line lacks its terminating LF or is malformed JSON.
    TornLine,
    /// The final entry is valid JSON, but its multi-entry atomic group is incomplete.
    IncompleteGroup,
}

impl DamagedTailKind {
    /// Stable wire name (`torn_line` or `incomplete_group`).
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::TornLine => "torn_line",
            Self::IncompleteGroup => "incomplete_group",
        }
    }
}

impl fmt::Display for DamagedTailKind {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(self.as_str())
    }
}

/// Errors returned by recordprims store, body, and stream operations.
#[derive(Debug, Clone, PartialEq, Eq)]
#[non_exhaustive]
pub enum Error {
    /// Store root or metadata is missing, malformed, or fails ownership/mode/filesystem checks.
    StoreInvalid(String),
    /// `store_invalid` met after the operation made a durable step: a store
    /// entry the root refuses, found mid-operation. The outcome is
    /// indeterminate; reconcile or re-observe before retrying.
    StoreInvalidAfterProgress(String),
    /// Another process or handle holds the store lock.
    Locked,
    /// Caller input failed contract schema or domain validation.
    Schema(String),
    /// The configured authorizer, or same-owner default, refused the operation.
    Unauthorized,
    /// A journal failed contract verification (`reason` is the closed contract refusal code).
    JournalInvalid {
        /// Contract refusal reason code (for example `ingest`, `shape`, `seq_gap`).
        reason: String,
    },
    /// A body publication or removal stopped between durable steps.
    BodyIncomplete {
        /// Observed reader state after the failure (`unpublished`, `publishing`, `available`, `removal_pending`, `removed`, `integrity_loss`, `corrupt`, or `unknown`).
        state: String,
    },
    /// The clock returned an instant earlier than the stream's latest entry.
    ClockRegression,
    /// The stream has a `SEALED` marker and refuses ordinary opens.
    StreamSealed,
    /// A continuation names an unsupported capability or a source stream not in this store.
    ContinuationUnsupported,
    /// The source or destination state digest in a restriction review does not match current state.
    StaleReview,
    /// An operation named a cut that is no longer current.
    StaleCut,
    /// The store or stream handle was closed before the operation started.
    Closed,
    /// A stream's final line or multi-entry atomic group is incomplete.
    DamagedTail {
        /// Whether the tail is a torn line or an incomplete group.
        kind: DamagedTailKind,
        /// Sequence number of the last complete atomic group (`0` if none).
        last_cut_seq: u64,
        /// Byte offset of the end of the last complete atomic group.
        last_cut_offset: i64,
        /// Byte count after `last_cut_offset`.
        damaged_bytes_count: i64,
    },
    /// A `Release` frame reader was read after its callback returned.
    ReaderRevoked,
    /// A `Release` callback re-entered the store.
    SinkReentered,
    /// A `Release` callback returned an error.
    SinkError,
    /// A filesystem or I/O error, without disclosing any path.
    Io {
        /// Operation name (`open`, `create`, `read`, `write`, `close`, `fsync`, `fsync_dir`, `rename`, `unlink`, `mkdir`, `stat`, `truncate`, `lock`, `append`).
        op: &'static str,
        /// Operating-system independent error class.
        class: IoClass,
        /// Whether durable store state may have changed before the failure.
        indeterminate: bool,
    },
    /// A primary operation error joined with a staging-cleanup I/O failure (`errors.Join(err, ioErr("unlink", ...))` parity).
    WithCleanupIo {
        /// Primary error returned by the operation.
        primary: Box<Error>,
        /// Cleanup operation name (for example `"unlink"`).
        op: &'static str,
        /// Operating-system independent error class of the cleanup failure.
        class: IoClass,
    },
}

impl Error {
    /// Construct a `StoreInvalid` error.
    pub fn store_invalid(detail: impl Into<String>) -> Self {
        Self::StoreInvalid(detail.into())
    }

    /// Construct a `Schema` error.
    pub fn schema(detail: impl Into<String>) -> Self {
        Self::Schema(detail.into())
    }

    /// Construct a `JournalInvalid` error with a contract refusal reason.
    pub fn journal_invalid(reason: impl Into<String>) -> Self {
        Self::JournalInvalid {
            reason: reason.into(),
        }
    }

    /// Construct a path-free `Io` error by classifying `err`.
    pub fn io(op: &'static str, err: &io::Error, indeterminate: bool) -> Self {
        Self::Io {
            op,
            class: classify_io(err),
            indeterminate,
        }
    }

    /// Attach a cleanup I/O error while preserving the primary error's code and determinacy.
    pub fn with_cleanup_io(self, op: &'static str, err: &io::Error) -> Self {
        Self::WithCleanupIo {
            primary: Box::new(self),
            op,
            class: classify_io(err),
        }
    }

    /// Closed `ErrorCode` of this error.
    pub fn code(&self) -> ErrorCode {
        match self {
            Self::StoreInvalid(_) | Self::StoreInvalidAfterProgress(_) => ErrorCode::StoreInvalid,
            Self::Locked => ErrorCode::Locked,
            Self::Schema(_) => ErrorCode::Schema,
            Self::Unauthorized => ErrorCode::Unauthorized,
            Self::JournalInvalid { .. } => ErrorCode::JournalInvalid,
            Self::BodyIncomplete { .. } => ErrorCode::BodyIncomplete,
            Self::ClockRegression => ErrorCode::ClockRegression,
            Self::StreamSealed => ErrorCode::StreamSealed,
            Self::ContinuationUnsupported => ErrorCode::ContinuationUnsupported,
            Self::StaleReview => ErrorCode::StaleReview,
            Self::StaleCut => ErrorCode::StaleCut,
            Self::Closed => ErrorCode::Closed,
            Self::DamagedTail { .. } => ErrorCode::DamagedTail,
            Self::ReaderRevoked => ErrorCode::ReaderRevoked,
            Self::SinkReentered => ErrorCode::SinkReentered,
            Self::SinkError => ErrorCode::SinkError,
            Self::Io { .. } => ErrorCode::Io,
            Self::WithCleanupIo { primary, .. } => primary.code(),
        }
    }

    /// Reports whether `self` left the operation's outcome unknown: the write
    /// may or may not have taken effect, and the caller must reconcile or
    /// re-observe before retrying.
    pub fn is_indeterminate(&self) -> bool {
        match self {
            Self::Io { indeterminate, .. } => *indeterminate,
            Self::BodyIncomplete { .. } | Self::StoreInvalidAfterProgress(_) => true,
            Self::WithCleanupIo { primary, .. } => primary.is_indeterminate(),
            _ => false,
        }
    }

    /// Returns the I/O operation and class if this error is (or carries) an `Io` error.
    pub fn io_error(&self) -> Option<(&'static str, IoClass)> {
        match self {
            Self::Io { op, class, .. } | Self::WithCleanupIo { op, class, .. } => {
                Some((*op, *class))
            }
            _ => None,
        }
    }

    /// Returns the observed body state if this error is (or wraps) `Error::BodyIncomplete`.
    pub fn body_incomplete_state(&self) -> Option<&str> {
        match self {
            Self::BodyIncomplete { state } => Some(state.as_str()),
            Self::WithCleanupIo { primary, .. } => primary.body_incomplete_state(),
            _ => None,
        }
    }
}

/// Reports whether `err` left the operation's outcome unknown.
pub fn is_indeterminate(err: &Error) -> bool {
    err.is_indeterminate()
}

impl fmt::Display for Error {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::StoreInvalid(detail) | Self::StoreInvalidAfterProgress(detail) => {
                if detail.is_empty() {
                    write!(f, "recordprims: invalid store")
                } else {
                    write!(f, "recordprims: invalid store: {detail}")
                }
            }
            Self::Locked => write!(f, "recordprims: store is locked"),
            Self::Schema(detail) => {
                if detail.is_empty() {
                    write!(f, "recordprims: schema violation")
                } else {
                    write!(f, "recordprims: schema violation: {detail}")
                }
            }
            Self::Unauthorized => write!(f, "recordprims: unauthorized"),
            Self::JournalInvalid { reason } => {
                write!(f, "recordprims: invalid journal ({reason})")
            }
            Self::BodyIncomplete { state } => {
                write!(
                    f,
                    "recordprims: body operation incomplete (state {state}); retry the same call"
                )
            }
            Self::ClockRegression => write!(f, "recordprims: clock regression"),
            Self::StreamSealed => write!(f, "recordprims: stream is sealed"),
            Self::ContinuationUnsupported => {
                write!(f, "recordprims: continuation source unsupported")
            }
            Self::StaleReview => write!(f, "recordprims: stale restriction review"),
            Self::StaleCut => write!(f, "recordprims: stale cut"),
            Self::Closed => write!(f, "recordprims: closed"),
            Self::DamagedTail {
                kind,
                last_cut_seq,
                last_cut_offset,
                damaged_bytes_count,
            } => {
                write!(
                    f,
                    "recordprims: damaged tail ({kind}) after seq {last_cut_seq} at offset {last_cut_offset} ({damaged_bytes_count} bytes)"
                )
            }
            Self::ReaderRevoked => write!(f, "recordprims: release reader revoked"),
            Self::SinkReentered => write!(f, "recordprims: release sink reentered the store"),
            Self::SinkError => write!(f, "recordprims: sink_error"),
            Self::Io { op, class, .. } => {
                write!(f, "recordprims: io_error{{op={op}, class={class}}}")
            }
            Self::WithCleanupIo { primary, op, class } => {
                write!(
                    f,
                    "{primary}; recordprims: io_error{{op={op}, class={class}}}"
                )
            }
        }
    }
}

impl std::error::Error for Error {
    fn source(&self) -> Option<&(dyn std::error::Error + 'static)> {
        match self {
            Self::WithCleanupIo { primary, .. } => Some(primary.as_ref()),
            _ => None,
        }
    }
}

/// Classify a `std::io::Error` into an `IoClass` without retaining any path or raw OS message.
pub(crate) fn classify_io(err: &io::Error) -> IoClass {
    match err.kind() {
        io::ErrorKind::NotFound => return IoClass::NotFound,
        io::ErrorKind::PermissionDenied => return IoClass::Permission,
        io::ErrorKind::AlreadyExists => return IoClass::Exists,
        io::ErrorKind::Interrupted => return IoClass::Interrupted,
        io::ErrorKind::StorageFull => return IoClass::NoSpace,
        io::ErrorKind::ReadOnlyFilesystem => return IoClass::ReadOnly,
        _ => {}
    }
    if out_of_space(err) {
        return IoClass::NoSpace;
    }
    if let Some(raw) = err.raw_os_error() {
        #[cfg(unix)]
        {
            if raw == rustix::io::Errno::EXIST.raw_os_error() {
                return IoClass::Exists;
            }
            if raw == rustix::io::Errno::ROFS.raw_os_error() {
                return IoClass::ReadOnly;
            }
            if raw == rustix::io::Errno::IO.raw_os_error() {
                return IoClass::Device;
            }
            if raw == rustix::io::Errno::ACCESS.raw_os_error()
                || raw == rustix::io::Errno::PERM.raw_os_error()
            {
                return IoClass::Permission;
            }
            if raw == rustix::io::Errno::NOENT.raw_os_error() {
                return IoClass::NotFound;
            }
            if raw == rustix::io::Errno::INTR.raw_os_error() {
                return IoClass::Interrupted;
            }
        }
        #[cfg(windows)]
        {
            // ERROR_WRITE_PROTECT = 19, ERROR_CRC = 23, ERROR_IO_DEVICE = 1117, ERROR_ALREADY_EXISTS = 183, ERROR_FILE_EXISTS = 80
            match raw as u32 {
                80 | 183 => return IoClass::Exists,
                19 => return IoClass::ReadOnly,
                23 | 1117 => return IoClass::Device,
                5 => return IoClass::Permission,
                2 | 3 => return IoClass::NotFound,
                _ => {}
            }
        }
    }
    IoClass::Other
}

/// Reports whether `err` is the device or quota refusing more bytes.
pub(crate) fn out_of_space(err: &io::Error) -> bool {
    if err.kind() == io::ErrorKind::StorageFull {
        return true;
    }
    if let Some(raw) = err.raw_os_error() {
        #[cfg(unix)]
        {
            return raw == rustix::io::Errno::NOSPC.raw_os_error()
                || raw == rustix::io::Errno::DQUOT.raw_os_error();
        }
        #[cfg(windows)]
        {
            // ERROR_HANDLE_DISK_FULL = 39, ERROR_DISK_FULL = 112, ERROR_DISK_QUOTA_EXCEEDED = 1295
            return matches!(raw as u32, 39 | 112 | 1295);
        }
    }
    false
}
