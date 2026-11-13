// SPDX-License-Identifier: MIT OR Apache-2.0

//! Content-addressed body store (`PutBody` and `RemoveBody`, matching `body.go`).

use std::collections::HashSet;
use std::io::{self, Read, Write};
use std::panic::{catch_unwind, resume_unwind, AssertUnwindSafe};
use std::sync::atomic::Ordering;
use std::time::Duration;

use sha2::{Digest as _, Sha256};

use crate::error::{DamagedTailKind, Error, IoClass};
use crate::instant::{format_instant, parse_instant, Instant};
use crate::journal::{self, BodyKey, SourceInput};
use crate::shape::{self, REASON_SHAPE};
use crate::store::{
    fill_random, hex_encode, map_jfile_err, read_file_verified, stream_hash, stream_journal_file,
    valid_digest, write_file_durable, BodyRef, Store, DIR_BODIES, DIR_STREAMS,
};
use crate::wire::{self, Kind, Member, Node, MAX_SAFE_INTEGER};

/// Default TTL of a [`PublicationLease`] returned by [`Store::put_body`] (1 hour, matching `DefaultPendingBodyTTL` in `store.go`).
pub const DEFAULT_PENDING_BODY_TTL: Duration = Duration::from_secs(3600);

pub(crate) const DIR_BODY_SHA256: &str = "bodies/sha256";
pub(crate) const DIR_BODY_TMP: &str = "bodies/tmp";
pub(crate) const DIR_BODY_INCARNATION: &str = "bodies/incarnation";
pub(crate) const DIR_BODY_PENDING: &str = "bodies/pending";

const MAX_JOURNAL_BYTES: i64 = 64 << 20;

/// Short-lived publication lease protecting a newly published body until its
/// referencing record is appended (`PublicationLease` in `body.go`).
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct PublicationLease {
    /// Random 128-bit lease identifier (32 lowercase hex characters).
    pub id: String,
    /// `sha256:<64 hex>` of the leased body.
    pub digest: String,
    /// Lease expiration instant.
    pub expires_at: Instant,
}

/// Closed vocabulary of body-removal reasons (`RemoveBodyReason` in `body.go`).
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash)]
pub enum RemoveBodyReason {
    /// Retention expiry (`"retention"`).
    Retention,
    /// Operator removal (`"operator"`).
    Operator,
    /// Rights-restricted removal (`"rights_restricted"`).
    RightsRestricted,
}

impl RemoveBodyReason {
    /// Stable wire string of this reason.
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::Retention => "retention",
            Self::Operator => "operator",
            Self::RightsRestricted => "rights_restricted",
        }
    }

    /// Parse a wire reason string.
    pub fn from_wire(s: &str) -> Option<Self> {
        match s {
            "retention" => Some(Self::Retention),
            "operator" => Some(Self::Operator),
            "rights_restricted" => Some(Self::RightsRestricted),
            _ => None,
        }
    }
}

/// Producer or operator provenance (`Provenance` in `body.go`).
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct Provenance {
    /// `"human"`, `"agent"`, or `"process"`.
    pub actor_kind: String,
    /// Actor identifier (`1..=128` Unicode scalar values).
    pub actor: String,
    /// Optional supervisor identifier (`1..=128` Unicode scalar values when non-empty).
    pub supervisor: String,
    /// Optional model identifier (`1..=128` Unicode scalar values when non-empty).
    pub model: String,
    /// Optional tool identifier (`1..=128` Unicode scalar values when non-empty).
    pub tool: String,
}

/// External authorization artifact reference (`Authorization` in `body.go`).
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct Authorization {
    /// Authorization reference (`^[A-Za-z0-9][A-Za-z0-9._:-]{0,255}$`).
    pub r#ref: String,
    /// `sha256:<64 lowercase hex>` of the authorization artifact.
    pub digest: String,
}

/// Request parameters for [`Store::remove_body`] (`RemoveBodyRequest` in `body.go`).
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct RemoveBodyRequest {
    /// `sha256:<64 lowercase hex>` of the body to remove.
    pub digest: String,
    /// Byte length (`0..=MAX_SAFE_INTEGER`).
    pub length: i64,
    /// Optional incarnation expected to be completed (`1..=MAX_SAFE_INTEGER`).
    pub complete_incarnation: Option<u64>,
    /// Closed removal reason.
    pub reason: RemoveBodyReason,
    /// Actor provenance.
    pub provenance: Provenance,
    /// Optional removal authorization.
    pub authorization: Option<Authorization>,
}

/// Outcome of [`Store::remove_body`] (`RemoveBodyOutcome` in `body.go`).
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash)]
pub enum RemoveBodyOutcome {
    /// Body bytes were unlinked and `body_removed` was recorded (`"removed"`).
    Removed,
    /// Body was never published, already removed, or completed a pending removal whose bytes were already gone (`"absent"`).
    Absent,
    /// Body is published in the lifecycle journal, but its physical file is missing (`"missing"`).
    Missing,
    /// Body publication was not completed (`"publication_incomplete"`).
    PublicationIncomplete,
    /// `complete_incarnation` does not match the current bound (`"superseded"`).
    Superseded,
    /// Physical file bytes do not match `digest` and `length` (`"body_conflict"`).
    Conflict,
    /// A sharing stream in the store is sealed, unreadable, invalid, or has pending restriction review (`"protection_unknown"`).
    ProtectionUnknown,
    /// Body is held by an active protection hold (`"held"`).
    Held,
    /// Body is referenced by an accepted record revision on a content stream (`"still_referenced"`).
    StillReferenced,
    /// Body is currently publishing or covered by an unexpired publication lease (`"pending"`).
    Pending,
}

impl RemoveBodyOutcome {
    /// Stable wire string of this outcome.
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::Removed => "removed",
            Self::Absent => "absent",
            Self::Missing => "missing",
            Self::PublicationIncomplete => "publication_incomplete",
            Self::Superseded => "superseded",
            Self::Conflict => "body_conflict",
            Self::ProtectionUnknown => "protection_unknown",
            Self::Held => "held",
            Self::StillReferenced => "still_referenced",
            Self::Pending => "pending",
        }
    }
}

/// Result returned by [`Store::remove_body`].
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct RemoveBodyResult {
    /// Outcome of the removal evaluation.
    pub outcome: RemoveBodyOutcome,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub(crate) enum BodyStateKind {
    Available,
    RemovalPending,
    Removed,
    Publishing,
    Unpublished,
    IntegrityLoss,
    Corrupt,
}

impl BodyStateKind {
    pub(crate) const fn as_str(self) -> &'static str {
        match self {
            Self::Available => "available",
            Self::RemovalPending => "removal_pending",
            Self::Removed => "removed",
            Self::Publishing => "publishing",
            Self::Unpublished => "unpublished",
            Self::IntegrityLoss => "integrity_loss",
            Self::Corrupt => "corrupt",
        }
    }
    pub(crate) fn from_wire(s: &str) -> Self {
        match s {
            "available" => Self::Available,
            "removal_pending" => Self::RemovalPending,
            "removed" => Self::Removed,
            "publishing" => Self::Publishing,
            "unpublished" => Self::Unpublished,
            "integrity_loss" => Self::IntegrityLoss,
            _ => Self::Corrupt,
        }
    }
}

#[derive(Debug, Clone, Copy)]
pub(crate) struct BodyState {
    pub(crate) kind: BodyStateKind,
    pub(crate) incarnation: u64,
    pub(crate) bound: u64,
}

pub(crate) struct DesignatedJournal {
    st: journal::State,
    jpath: String,
    cut_offset: u64,
    last_seq: i64,
    last_at: Instant,
}

impl Store {
    /// Stage, verify, and publish a content-addressed body into the store,
    /// returning its [`BodyRef`] and a [`PublicationLease`].
    pub fn put_body<R: Read>(
        &self,
        r: R,
        media_type: &str,
    ) -> Result<(BodyRef, PublicationLease), Error> {
        if !valid_media_type(media_type) {
            return Err(Error::schema("media type"));
        }
        self.with_lock(|| self.ensure_body_dirs())?;

        let (tmp, h, length) = self.stage_body(r)?;
        let digest = format!("sha256:{h}");

        let mut staged = true;
        let mut progressed = false;
        let mut lease_out: Option<PublicationLease> = None;

        let lock_res = catch_unwind(AssertUnwindSafe(|| {
            self.with_lock(|| {
                let mut desig = self.read_designated_state()?;
                let st = self.eval_body_state(&desig, &h, length)?;
                if st.kind == BodyStateKind::Corrupt {
                    return Err(Error::store_invalid("body_conflict"));
                }

                match st.kind {
                    BodyStateKind::Available => {
                        progressed = true;
                        let lease = self.write_lease(&h, st.bound)?;
                        lease_out = Some(lease);
                        Ok(())
                    }
                    BodyStateKind::Publishing => {
                        let mut n = st.incarnation;
                        if n <= st.bound {
                            if st.bound >= (MAX_SAFE_INTEGER as u64) {
                                return Err(Error::schema("body incarnation exhausted"));
                            }
                            n = st.bound + 1;
                            progressed = true;
                            self.write_incarnation(&h, n)?;
                        }
                        progressed = true;
                        self.sync_body_dir(&h, "body.dir_fsynced")?;
                        self.append_lifecycle_entry(
                            &mut desig,
                            "body_published",
                            &h,
                            length,
                            n,
                            None,
                        )?;
                        let lease = self.write_lease(&h, n)?;
                        lease_out = Some(lease);
                        Ok(())
                    }
                    _ => {
                        if st.incarnation >= (MAX_SAFE_INTEGER as u64) {
                            return Err(Error::schema("body incarnation exhausted"));
                        }
                        let now = self.clock.now();
                        if now < desig.last_at {
                            return Err(Error::ClockRegression);
                        }
                        progressed = true;
                        let n = st.incarnation + 1;
                        self.write_incarnation(&h, n)?;
                        self.link_body(&tmp, &h, length)?;
                        staged = false;
                        self.append_lifecycle_entry(
                            &mut desig,
                            "body_published",
                            &h,
                            length,
                            n,
                            None,
                        )?;
                        let lease = self.write_lease(&h, n)?;
                        lease_out = Some(lease);
                        Ok(())
                    }
                }
            })
        }));

        match lock_res {
            Ok(Ok(())) => {
                if staged {
                    let _ = self.root.remove(&tmp, self.fso);
                }
                Ok((
                    BodyRef {
                        digest,
                        length,
                        media_type: media_type.to_owned(),
                    },
                    lease_out.expect("lease written"),
                ))
            }
            Ok(Err(err)) => {
                let mut err = self.body_err(err, progressed, &h, length);
                if staged {
                    err = self.discard_staged(&tmp, err);
                }
                Err(err)
            }
            Err(payload) => {
                if staged {
                    let _ = self.root.remove(&tmp, self.fso);
                }
                resume_unwind(payload)
            }
        }
    }

    /// Evaluate and execute a body removal under the store lock.
    pub fn remove_body(&self, req: &RemoveBodyRequest) -> Result<RemoveBodyResult, Error> {
        if !valid_digest(&req.digest) || req.length < 0 || req.length > MAX_SAFE_INTEGER {
            return Err(Error::schema("digest or length"));
        }
        if let Some(ci) = req.complete_incarnation {
            if ci < 1 || ci > (MAX_SAFE_INTEGER as u64) {
                return Err(Error::schema("complete incarnation"));
            }
        }
        if !valid_provenance(&req.provenance) {
            return Err(Error::schema("removal provenance"));
        }
        if !valid_authorization(req.authorization.as_ref()) {
            return Err(Error::schema("removal authorization"));
        }

        let h = digest_hex(&req.digest);
        let mut out = RemoveBodyOutcome::Absent;
        let mut progressed = false;

        let res = self.with_lock(|| {
            self.ensure_body_dirs()?;
            let mut desig = self.read_designated_state()?;
            let st = self.eval_body_state(&desig, h, req.length)?;

            if st.kind == BodyStateKind::Corrupt {
                out = RemoveBodyOutcome::Conflict;
                return Ok(());
            }
            if let Some(ci) = req.complete_incarnation {
                if st.bound == 0 || ci != st.bound || st.incarnation != st.bound {
                    out = RemoveBodyOutcome::Superseded;
                    return Ok(());
                }
            }
            match st.kind {
                BodyStateKind::IntegrityLoss => {
                    out = RemoveBodyOutcome::Missing;
                    return Ok(());
                }
                BodyStateKind::Unpublished | BodyStateKind::Removed => {
                    out = RemoveBodyOutcome::Absent;
                    return Ok(());
                }
                BodyStateKind::Publishing => {
                    out = RemoveBodyOutcome::Pending;
                    return Ok(());
                }
                BodyStateKind::RemovalPending | BodyStateKind::Available => {}
                BodyStateKind::Corrupt => unreachable!(),
            }

            if st.kind == BodyStateKind::RemovalPending {
                let (present, _) = self.body_file_present(h, req.length)?;
                if !present {
                    if st.incarnation > st.bound {
                        out = RemoveBodyOutcome::Pending;
                        return Ok(());
                    }
                    progressed = true;
                    self.sync_body_dir(h, "remove.dir_fsynced")?;
                    self.append_lifecycle_entry(
                        &mut desig,
                        "body_removed",
                        h,
                        req.length,
                        st.bound,
                        None,
                    )?;
                    out = RemoveBodyOutcome::Absent;
                    return Ok(());
                }
            }

            if st.incarnation > st.bound {
                out = RemoveBodyOutcome::Pending;
                return Ok(());
            }
            if let Some(elig_out) = self.check_removal_eligibility(h, req.length)? {
                out = elig_out;
                return Ok(());
            }
            self.fault("remove.checked")?;
            self.fault("remove.decided")?;

            if st.kind == BodyStateKind::Available {
                if let Err(err) = self.append_lifecycle_entry(
                    &mut desig,
                    "body_removal_started",
                    h,
                    req.length,
                    st.bound,
                    Some(req),
                ) {
                    if matches!(
                        err,
                        Error::Io {
                            op: "write",
                            class: IoClass::NoSpace,
                            indeterminate: false,
                        }
                    ) {
                        return Err(err);
                    }
                    progressed = true;
                    return Err(err);
                }
            }
            progressed = true;
            if let Err(e) = self.root.remove(&body_file(h), self.fso) {
                if !e.is_not_found() {
                    return Err(e.into_store_error("unlink", true));
                }
            }
            self.fault("remove.unlinked")?;
            self.sync_body_dir(h, "remove.dir_fsynced")?;
            self.append_lifecycle_entry(&mut desig, "body_removed", h, req.length, st.bound, None)?;
            out = RemoveBodyOutcome::Removed;
            Ok(())
        });

        match res {
            Ok(()) => Ok(RemoveBodyResult { outcome: out }),
            Err(err) => Err(self.body_err(err, progressed, h, req.length)),
        }
    }

    pub(crate) fn ensure_body_dirs(&self) -> Result<(), Error> {
        if self.body_dirs_ready.load(Ordering::SeqCst) {
            return Ok(());
        }
        for d in [
            DIR_BODIES,
            DIR_BODY_SHA256,
            DIR_BODY_TMP,
            DIR_BODY_INCARNATION,
            DIR_BODY_PENDING,
        ] {
            self.root
                .mkdir(d, self.fso)
                .map_err(|e| e.into_store_error("mkdir", false))?;
        }
        self.root
            .sync_dir(DIR_BODIES, self.fso)
            .map_err(|e| e.into_store_error("fsync_dir", false))?;
        self.body_dirs_ready.store(true, Ordering::SeqCst);
        Ok(())
    }

    fn discard_staged(&self, name: &str, err: Error) -> Error {
        match self.root.remove(name, self.fso) {
            Ok(()) => err,
            Err(e) if e.is_not_found() => err,
            Err(crate::fsroot::FsRootError::Io(_, ioe)) => err.with_cleanup_io("unlink", &ioe),
            Err(_) => {
                let ioe = io::Error::from(io::ErrorKind::PermissionDenied);
                err.with_cleanup_io("unlink", &ioe)
            }
        }
    }

    fn stage_body<R: Read>(&self, mut r: R) -> Result<(String, String, i64), Error> {
        let mut id = [0u8; 16];
        fill_random(&mut id)?;
        let tmp = format!("{DIR_BODY_TMP}/{}", hex_encode(&id));
        let mut f = self
            .root
            .create_exclusive(&tmp, 0o600, self.fso)
            .map_err(|e| e.into_store_error("create", false))?;

        let max_len = self.max_body_length.load(Ordering::SeqCst);
        let stage_res = catch_unwind(AssertUnwindSafe(|| -> Result<(String, i64), Error> {
            self.fault("body.tmp_created")?;
            let mut hasher = Sha256::new();
            let mut total: i64 = 0;
            let mut buf = vec![0u8; 64 * 1024];
            loop {
                let n = r.read(&mut buf).map_err(|e| Error::io("read", &e, false))?;
                if n == 0 {
                    break;
                }
                let next = total
                    .checked_add(n as i64)
                    .filter(|&t| t <= max_len)
                    .ok_or_else(|| Error::schema(format!("body length exceeds {max_len} bytes")))?;
                hasher.update(&buf[..n]);
                f.write_all(&buf[..n])
                    .map_err(|e| Error::io("write", &e, false))?;
                total = next;
            }
            self.fault("body.tmp_written")?;
            f.sync_all().map_err(|e| Error::io("fsync", &e, false))?;
            drop(f);
            self.fault("body.tmp_fsynced")?;
            let sum = hasher.finalize();
            Ok((hex_encode(&sum), total))
        }));

        match stage_res {
            Ok(Ok((h, total))) => Ok((tmp, h, total)),
            Ok(Err(err)) => Err(self.discard_staged(&tmp, err)),
            Err(payload) => {
                let _ = self.root.remove(&tmp, self.fso);
                resume_unwind(payload)
            }
        }
    }

    pub(crate) fn read_designated_state(&self) -> Result<DesignatedJournal, Error> {
        let stream_id = &self.profile.runtime.body_removal_stream;
        if stream_id.is_empty() {
            return Err(Error::journal_invalid(journal::REASON_BODY_REMOVAL_STREAM));
        }
        let jpath = stream_journal_file(stream_id);
        let raw = match read_file_verified(&self.root, &jpath, self.fso, MAX_JOURNAL_BYTES) {
            Ok(b) => b,
            Err(Error::Io {
                class: IoClass::NotFound,
                ..
            }) => return Err(Error::journal_invalid(journal::REASON_BODY_REMOVAL_STREAM)),
            Err(e) => return Err(e),
        };
        let nodes = wire::ingest_lines(&raw, wire::KIND_JOURNAL_ENTRY)
            .map_err(|r| Error::journal_invalid(r.reason))?;
        let mut st = journal::State::new(&self.window_node())
            .map_err(|r| Error::journal_invalid(r.reason))?;
        st.set_profile(&self.profile_node);

        let mut last_at: Option<Instant> = None;
        for node in nodes {
            shape::validate("v1/journal-entry.schema.json", &node)
                .map_err(|_| Error::journal_invalid(REASON_SHAPE))?;
            if let Some(at_node) = node.get("at").filter(|x| x.kind == Kind::String) {
                if let Ok(parsed_at) = parse_instant(&at_node.str_val) {
                    last_at = Some(parsed_at);
                }
            }
            st.ingest(node)
                .map_err(|r| Error::journal_invalid(r.reason))?;
        }
        journal::check_journal(st.journal()).map_err(|r| Error::journal_invalid(r.reason))?;
        if st.stream() != stream_id {
            return Err(Error::journal_invalid(journal::REASON_BODY_REMOVAL_STREAM));
        }
        let last_seq = st.len() as i64;
        Ok(DesignatedJournal {
            st,
            jpath,
            cut_offset: raw.len() as u64,
            last_seq,
            last_at: last_at.expect("non-empty designated journal"),
        })
    }

    pub(crate) fn eval_body_state(
        &self,
        desig: &DesignatedJournal,
        h: &str,
        want_length: i64,
    ) -> Result<BodyState, Error> {
        let (local_n, has_local) = self.read_incarnation(h)?;
        let (present, intact) = self.body_file_present(h, want_length)?;
        let key = BodyKey {
            digest: format!("sha256:{h}"),
            length: want_length,
        };
        let (rs, bound_i64, _phase) = desig.st.body_lifecycle(&key, present && intact);
        let bound = bound_i64 as u64;
        let mut max_inc = bound;
        if has_local && local_n > max_inc {
            max_inc = local_n;
        }
        if present && !intact {
            return Ok(BodyState {
                kind: BodyStateKind::Corrupt,
                incarnation: max_inc,
                bound,
            });
        }
        Ok(BodyState {
            kind: BodyStateKind::from_wire(rs),
            incarnation: max_inc,
            bound,
        })
    }

    pub(crate) fn read_incarnation(&self, h: &str) -> Result<(u64, bool), Error> {
        let path = format!("{DIR_BODY_INCARNATION}/{h}");
        let b = match read_file_verified(&self.root, &path, self.fso, 32) {
            Ok(b) => b,
            Err(Error::Io {
                class: IoClass::NotFound,
                ..
            }) => return Ok((0, false)),
            Err(e) => return Err(e),
        };
        let s = std::str::from_utf8(&b)
            .map_err(|_| Error::store_invalid("incarnation record invalid"))?;
        if s.is_empty() || !s.bytes().all(|c| c.is_ascii_digit()) {
            return Err(Error::store_invalid("incarnation record invalid"));
        }
        let n: u64 = s
            .parse()
            .map_err(|_| Error::store_invalid("incarnation record invalid"))?;
        if n == 0 || n > (MAX_SAFE_INTEGER as u64) {
            return Err(Error::store_invalid("incarnation record invalid"));
        }
        Ok((n, true))
    }

    pub(crate) fn write_incarnation(&self, h: &str, n: u64) -> Result<(), Error> {
        let dst = format!("{DIR_BODY_INCARNATION}/{h}");
        self.replace_file(&dst, n.to_string().as_bytes(), "body.incarnation_written")?;
        self.fault("body.incarnation_fsynced")
    }

    fn replace_file(&self, name: &str, data: &[u8], phase: &str) -> Result<(), Error> {
        let mut id = [0u8; 16];
        fill_random(&mut id)?;
        let tmp = format!("{DIR_BODY_TMP}/{}", hex_encode(&id));
        write_file_durable(&self.root, &tmp, data, self.fso)?;
        if let Err(e) = self.root.rename(&tmp, name, self.fso) {
            let _ = self.root.remove(&tmp, self.fso);
            return Err(e.into_store_error("rename", true));
        }
        self.fault(phase)?;
        let parent = match name.rsplit_once('/') {
            Some((dir, _)) if !dir.is_empty() => dir,
            _ => ".",
        };
        self.root
            .sync_dir(parent, self.fso)
            .map_err(|e| e.into_store_error("fsync_dir", true))
    }

    fn write_lease(&self, h: &str, n: u64) -> Result<PublicationLease, Error> {
        let mut raw = [0u8; 16];
        fill_random(&mut raw)?;
        let id = hex_encode(&raw);
        let dir = format!("{DIR_BODY_PENDING}/{h}");
        self.root
            .mkdir(&dir, self.fso)
            .map_err(|e| e.into_store_error("mkdir", true))?;
        let exp = self.clock.now().saturating_add(self.pending_body_ttl);
        let lf = Node::object(vec![
            Member {
                name: "incarnation".into(),
                value: Node::uint(n),
            },
            Member {
                name: "expires_at".into(),
                value: Node::string(format_instant(exp)),
            },
        ]);
        let dst = format!("{dir}/{id}");
        let bytes = wire::encode(&lf);
        write_file_durable(&self.root, &dst, &bytes, self.fso)?;
        self.root
            .sync_dir(DIR_BODY_PENDING, self.fso)
            .map_err(|e| e.into_store_error("fsync_dir", true))?;
        self.fault("body.lease_written")?;
        Ok(PublicationLease {
            id,
            digest: format!("sha256:{h}"),
            expires_at: exp,
        })
    }

    fn link_body(&self, tmp: &str, h: &str, _want_len: i64) -> Result<(), Error> {
        let dst = body_file(h);
        let present = self.exists(&dst)?;
        if present {
            let _ = self.root.remove(tmp, self.fso);
        } else {
            let shard = body_dir(h);
            self.root
                .mkdir(&shard, self.fso)
                .map_err(|e| e.into_store_error("mkdir", true))?;
            self.root
                .sync_dir(DIR_BODY_SHA256, self.fso)
                .map_err(|e| e.into_store_error("fsync_dir", true))?;
            self.root
                .rename(tmp, &dst, self.fso)
                .map_err(|e| e.into_store_error("rename", true))?;
        }
        self.fault("body.renamed")?;
        self.sync_body_dir(h, "body.dir_fsynced")
    }

    fn sync_body_dir(&self, h: &str, fault_phase: &str) -> Result<(), Error> {
        self.root
            .sync_dir(&body_dir(h), self.fso)
            .map_err(|e| e.into_store_error("fsync_dir", true))?;
        self.fault(fault_phase)
    }

    fn body_file_present(&self, h: &str, want_length: i64) -> Result<(bool, bool), Error> {
        let (mut f, size) = match self.root.open_verified(&body_file(h), self.fso) {
            Ok(pair) => pair,
            Err(e) if e.is_not_found() => return Ok((false, false)),
            Err(e) => return Err(e.into_store_error("open", false)),
        };
        if (size as i64) != want_length {
            return Ok((true, false));
        }
        let mut hasher = Sha256::new();
        let mut copied: u64 = 0;
        let mut buf = [0u8; 32 * 1024];
        loop {
            let n = f.read(&mut buf).map_err(|e| Error::io("read", &e, false))?;
            if n == 0 {
                break;
            }
            copied += n as u64;
            if copied > size {
                return Ok((true, false));
            }
            hasher.update(&buf[..n]);
        }
        if copied != size {
            return Ok((true, false));
        }
        let got = hex_encode(&hasher.finalize());
        Ok((true, got == h))
    }

    fn append_lifecycle_entry(
        &self,
        desig: &mut DesignatedJournal,
        typ: &str,
        h: &str,
        length: i64,
        incarnation: u64,
        req: Option<&RemoveBodyRequest>,
    ) -> Result<(), Error> {
        if desig.last_seq >= MAX_SAFE_INTEGER {
            return Err(Error::schema("designated stream seq exhausted"));
        }
        let now_inst = self.clock.now();
        if now_inst < desig.last_at {
            return Err(Error::ClockRegression);
        }
        let now = format_instant(now_inst);
        let seq = desig.last_seq + 1;
        let stream_id = self.profile.runtime.body_removal_stream.clone();
        let digest = format!("sha256:{h}");

        let body_node = Node::object(vec![
            Member {
                name: "digest".into(),
                value: Node::string(digest.clone()),
            },
            Member {
                name: "length".into(),
                value: Node::int(length),
            },
        ]);
        let mut fields = vec![
            Member {
                name: "v".into(),
                value: Node::int(1),
            },
            Member {
                name: "seq".into(),
                value: Node::int(seq),
            },
            Member {
                name: "group".into(),
                value: Node::int(seq),
            },
            Member {
                name: "group_size".into(),
                value: Node::int(1),
            },
            Member {
                name: "stream".into(),
                value: Node::string(stream_id.clone()),
            },
            Member {
                name: "type".into(),
                value: Node::string(typ),
            },
            Member {
                name: "at".into(),
                value: Node::string(now),
            },
            Member {
                name: "body".into(),
                value: body_node,
            },
            Member {
                name: "incarnation".into(),
                value: Node::uint(incarnation),
            },
        ];

        if typ == "body_removal_started" {
            if let Some(r) = req {
                fields.push(Member {
                    name: "reason".into(),
                    value: Node::string(r.reason.as_str()),
                });
                let mut prov_fields = vec![
                    Member {
                        name: "actor_kind".into(),
                        value: Node::string(r.provenance.actor_kind.clone()),
                    },
                    Member {
                        name: "actor".into(),
                        value: Node::string(r.provenance.actor.clone()),
                    },
                ];
                if !r.provenance.supervisor.is_empty() {
                    prov_fields.push(Member {
                        name: "supervisor".into(),
                        value: Node::string(r.provenance.supervisor.clone()),
                    });
                }
                if !r.provenance.model.is_empty() {
                    prov_fields.push(Member {
                        name: "model".into(),
                        value: Node::string(r.provenance.model.clone()),
                    });
                }
                if !r.provenance.tool.is_empty() {
                    prov_fields.push(Member {
                        name: "tool".into(),
                        value: Node::string(r.provenance.tool.clone()),
                    });
                }
                fields.push(Member {
                    name: "provenance".into(),
                    value: Node::object(prov_fields),
                });
                if let Some(ref auth) = r.authorization {
                    fields.push(Member {
                        name: "authorization".into(),
                        value: Node::object(vec![
                            Member {
                                name: "ref".into(),
                                value: Node::string(auth.r#ref.clone()),
                            },
                            Member {
                                name: "digest".into(),
                                value: Node::string(auth.digest.clone()),
                            },
                        ]),
                    });
                }
            }
        }

        let entry = Node::object(fields);
        let raw_entry = wire::encode(&entry);
        let ingested = wire::ingest(&raw_entry, wire::KIND_JOURNAL_ENTRY)
            .map_err(|r| Error::schema(format!("lifecycle entry wire invalid: {}", r.reason)))?;
        shape::validate("v1/journal-entry.schema.json", &ingested)
            .map_err(|_| Error::schema("lifecycle entry shape invalid"))?;
        desig
            .st
            .ingest(ingested)
            .map_err(|r| Error::journal_invalid(r.reason))?;

        let mut line = raw_entry;
        line.push(b'\n');
        self.appender
            .append(&self.root, &desig.jpath, desig.cut_offset, &line, true)
            .map_err(map_jfile_err)?;

        desig.cut_offset += line.len() as u64;
        desig.last_seq = seq;
        desig.last_at = now_inst;
        match typ {
            "body_published" => {
                self.fault("journal.published_written")?;
                self.fault("journal.published_fsynced")?;
                self.fault("body.published_written")?;
            }
            "body_removal_started" => {
                self.fault("journal.removal_started_written")?;
                self.fault("remove.tombstone_written")?;
                self.fault("journal.removal_started_fsynced")?;
                self.fault("remove.tombstone_fsynced")?;
            }
            "body_removed" => {
                self.fault("journal.removed_written")?;
                self.fault("journal.removed_fsynced")?;
            }
            _ => {}
        }
        Ok(())
    }

    fn check_removal_eligibility(
        &self,
        h: &str,
        length: i64,
    ) -> Result<Option<RemoveBodyOutcome>, Error> {
        let streams = match self.root.read_dir_names(DIR_STREAMS, self.fso) {
            Ok(s) => s,
            Err(_) => return Ok(Some(RemoveBodyOutcome::ProtectionUnknown)),
        };
        let desig_hash = stream_hash(&self.profile.runtime.body_removal_stream);
        let target = BodyKey {
            digest: format!("sha256:{h}"),
            length,
        };

        let mut any_held = false;
        let mut any_referenced = false;
        for sh in streams {
            if sh == desig_hash {
                continue;
            }
            let Some(st) = self.replay_sharing_stream_dir(&sh, &mut HashSet::new()) else {
                return Ok(Some(RemoveBodyOutcome::ProtectionUnknown));
            };
            if st.body_held(&target) {
                any_held = true;
            }
            if st.body_referenced(&target) {
                any_referenced = true;
            }
        }
        if any_held {
            return Ok(Some(RemoveBodyOutcome::Held));
        }
        match self.unexpired_lease(h) {
            Ok(true) => return Ok(Some(RemoveBodyOutcome::Pending)),
            Ok(false) => {}
            Err(()) => return Ok(Some(RemoveBodyOutcome::ProtectionUnknown)),
        }
        if any_referenced {
            return Ok(Some(RemoveBodyOutcome::StillReferenced));
        }
        Ok(None)
    }

    fn replay_sharing_stream_dir(
        &self,
        sh: &str,
        seen: &mut HashSet<String>,
    ) -> Option<journal::State> {
        if seen.len() > 16 || !seen.insert(sh.to_owned()) {
            return None;
        }
        let sdir = format!("{DIR_STREAMS}/{sh}");
        if self.exists(&format!("{sdir}/SEALED")).unwrap_or(true) {
            return None;
        }
        let raw = read_file_verified(
            &self.root,
            &format!("{sdir}/journal.ndjson"),
            self.fso,
            MAX_JOURNAL_BYTES,
        )
        .ok()?;
        let nodes = wire::ingest_lines(&raw, wire::KIND_JOURNAL_ENTRY).ok()?;
        let mut st = journal::State::new(&self.window_node()).ok()?;
        st.set_profile(&self.profile_node);

        for (i, node) in nodes.into_iter().enumerate() {
            shape::validate("v1/journal-entry.schema.json", &node).ok()?;
            if i == 0 {
                if let Some(cont) = node.get("continues") {
                    if !self.attach_continuation_chain(&mut st, cont, seen) {
                        return None;
                    }
                }
            }
            st.ingest(node).ok()?;
        }
        journal::check_journal(st.journal()).ok()?;
        if stream_hash(st.stream()) != sh {
            return None;
        }
        if st.review_pending() {
            return None;
        }
        Some(st)
    }

    fn attach_continuation_chain(
        &self,
        dst: &mut journal::State,
        cont: &Node,
        seen: &mut HashSet<String>,
    ) -> bool {
        if cont.kind != Kind::Object || seen.len() > 16 {
            return false;
        }
        let (Some(cap_node), Some(stream_node)) = (cont.get("capability"), cont.get("stream"))
        else {
            return false;
        };
        if cap_node.kind != Kind::String
            || cap_node.str_val != "contract: durable-record/v1"
            || stream_node.kind != Kind::String
        {
            return false;
        }
        let src_hash = stream_hash(&stream_node.str_val);
        if src_hash == stream_hash(&self.profile.runtime.body_removal_stream) {
            return false;
        }
        let Some(src_st) = self.replay_sharing_stream_dir(&src_hash, seen) else {
            return false;
        };
        if src_st.stream() != stream_node.str_val {
            return false;
        }
        let src_arr = Node::array(src_st.journal().to_vec());
        dst.set_source_chain(SourceInput {
            source_journal: Some(src_arr),
            ..SourceInput::default()
        })
        .is_ok()
    }

    fn unexpired_lease(&self, h: &str) -> Result<bool, ()> {
        let dir = format!("{DIR_BODY_PENDING}/{h}");
        let names = match self.root.read_dir_names(&dir, self.fso) {
            Ok(n) => n,
            Err(e) if e.is_not_found() => return Ok(false),
            Err(_) => return Err(()),
        };
        let now = self.clock.now();
        let mut live = false;
        for n in names {
            let Ok(b) = read_file_verified(&self.root, &format!("{dir}/{n}"), self.fso, 1 << 12)
            else {
                return Err(());
            };
            let Ok(node) = wire::parse_json(&b) else {
                return Err(());
            };
            if node.kind != Kind::Object {
                return Err(());
            }
            let Some(inc) = node.get("incarnation") else {
                return Err(());
            };
            if inc.kind != Kind::Number {
                return Err(());
            }
            let Ok(inc_val) = inc.token.parse::<u64>() else {
                return Err(());
            };
            if inc_val < 1 || inc_val > MAX_SAFE_INTEGER as u64 {
                return Err(());
            }
            let Some(exp_str) = node
                .get("expires_at")
                .filter(|x| x.kind == Kind::String)
                .map(|x| x.str_val.as_str())
            else {
                return Err(());
            };
            let Ok(exp) = parse_instant(exp_str) else {
                return Err(());
            };
            if now < exp {
                live = true;
            }
        }
        Ok(live)
    }

    fn body_err(&self, err: Error, progressed: bool, h: &str, length: i64) -> Error {
        if !progressed {
            return err;
        }
        let mut state = "unknown".to_owned();
        let _ = self.with_lock(|| {
            if let Ok(desig) = self.read_designated_state() {
                if let Ok(st) = self.eval_body_state(&desig, h, length) {
                    state = st.kind.as_str().to_owned();
                }
            }
            Ok(())
        });
        Error::BodyIncomplete { state }
    }
}

pub(crate) fn digest_hex(digest: &str) -> &str {
    digest.strip_prefix("sha256:").unwrap_or(digest)
}

pub(crate) fn body_dir(h: &str) -> String {
    format!("{DIR_BODY_SHA256}/{}", &h[..2])
}

pub(crate) fn body_file(h: &str) -> String {
    format!("{DIR_BODY_SHA256}/{}/{h}", &h[..2])
}

fn valid_media_type(mt: &str) -> bool {
    let Some((typ, sub)) = mt.split_once('/') else {
        return false;
    };
    valid_media_token(typ) && valid_media_token(sub)
}

fn valid_media_token(tok: &str) -> bool {
    let b = tok.as_bytes();
    if b.is_empty() || b.len() > 127 {
        return false;
    }
    if !(b[0].is_ascii_lowercase() || b[0].is_ascii_digit()) {
        return false;
    }
    b.iter().all(|&c| {
        c.is_ascii_lowercase()
            || c.is_ascii_digit()
            || matches!(
                c,
                b'!' | b'#' | b'$' | b'&' | b'^' | b'_' | b'.' | b'+' | b'-'
            )
    })
}

fn valid_wire_string(s: &str, min_cp: usize, max_cp: usize) -> bool {
    let n = s.chars().count();
    n >= min_cp && n <= max_cp
}

fn valid_provenance(p: &Provenance) -> bool {
    if !matches!(p.actor_kind.as_str(), "human" | "agent" | "process") {
        return false;
    }
    if !valid_wire_string(&p.actor, 1, 128) {
        return false;
    }
    for opt in [&p.supervisor, &p.model, &p.tool] {
        if !opt.is_empty() && !valid_wire_string(opt, 1, 128) {
            return false;
        }
    }
    true
}

fn valid_auth_ref(s: &str) -> bool {
    let b = s.as_bytes();
    if b.is_empty() || b.len() > 256 {
        return false;
    }
    if !b[0].is_ascii_alphanumeric() {
        return false;
    }
    b.iter()
        .all(|&c| c.is_ascii_alphanumeric() || matches!(c, b'.' | b'_' | b':' | b'-'))
}

fn valid_authorization(a: Option<&Authorization>) -> bool {
    let Some(auth) = a else {
        return true;
    };
    valid_auth_ref(&auth.r#ref) && valid_digest(&auth.digest)
}

#[allow(dead_code)]
fn _keep_damaged_tail_kind(_: DamagedTailKind) {}
