// SPDX-License-Identifier: MIT OR Apache-2.0

//! Content streams: open, append, read-back and release.
//!
//! This is the Rust twin of `stream.go`, for plain (non-continuation)
//! streams. Continuation streams and streams carrying compaction state (a
//! seal or a reclaimed-predecessor summary) are refused until compaction is
//! ported: opening one returns [`Error::ContinuationUnsupported`], and
//! replaying a sealed or reclaimed stream returns [`Error::StreamSealed`].

use std::collections::{HashMap, HashSet};
use std::io::{self, Read};
use std::sync::Mutex;

use sha2::{Digest as _, Sha256};

use crate::body::{body_file, BodyStateKind, PublicationLease};
use crate::canon;
use crate::error::{DamagedTailKind, Error, IoClass};
use crate::instant::{format_instant, parse_instant, Instant};
use crate::journal::{self, pin_node, State};
use crate::shape;
use crate::store::{
    create_stream_journal, hex_encode, map_jfile_err, read_file_verified, stream_dir,
    stream_journal_file, valid_stream_id, Pin, Store,
};
use crate::wire::{self, Kind, Member, Node, MAX_SAFE_INTEGER};

const MAX_JOURNAL_BYTES: i64 = 64 << 20;

/// A stream's revision policy, fixed when the stream is opened.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash)]
pub enum Mode {
    /// Each record has exactly one revision.
    SingleRevision,
    /// Records may be revised; each revision supersedes the previous one.
    Revisioned,
}

impl Mode {
    /// The contract spelling of the mode.
    pub fn as_str(self) -> &'static str {
        match self {
            Self::SingleRevision => "single_revision",
            Self::Revisioned => "revisioned",
        }
    }

    fn parse(s: &str) -> Option<Self> {
        match s {
            "single_revision" => Some(Self::SingleRevision),
            "revisioned" => Some(Self::Revisioned),
            _ => None,
        }
    }
}

/// Binds a continuation stream to a specific source stream cut.
#[derive(Debug, Clone, PartialEq, Eq, Hash)]
pub struct Continues {
    /// Capability of the source (`contract: durable-record/v1`).
    pub capability: String,
    /// Source stream identifier.
    pub stream: String,
    /// Sequence number of the source cut.
    pub cut_seq: u64,
    /// JCS SHA-256 digest of the source journal through the cut.
    pub cut_digest: String,
}

/// Identifies a record revision in a continuation's source stream.
#[derive(Debug, Clone, PartialEq, Eq, Hash)]
pub struct SourcePin {
    /// Capability of the source stream.
    pub capability: String,
    /// The source revision.
    pub pin: Pin,
}

/// A content write (append or revise) on a [`Stream`].
#[derive(Debug, Clone, Default)]
pub struct Write {
    /// Raw JSON of the `durable-record/v1` record document.
    pub record: Vec<u8>,
    /// The caller's idempotency key (1 to 256 code points).
    pub idempotency_key: String,
    /// Publication lease for the record's external body, if any.
    pub lease: Option<PublicationLease>,
    /// For a continuation write that re-records source content, the exact
    /// source revision.
    pub imported_from: Option<SourcePin>,
}

/// The contract outcome of a content write.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash)]
#[non_exhaustive]
pub enum WriteOutcome {
    /// A first revision was appended.
    Appended,
    /// A later revision was appended.
    Revised,
    /// The presented revision does not extend the current head.
    RevisionConflict,
    /// The idempotency key is live for a different operation.
    IdempotencyConflict,
    /// A revision above 1 names a record with no head.
    NoHead,
    /// A revision above 1 on a single-revision stream.
    ModeViolation,
    /// The record's external body is not available.
    BodyUnverified,
    /// The record is retired.
    RetiredRecord,
}

impl WriteOutcome {
    /// The contract spelling of the outcome.
    pub fn as_str(self) -> &'static str {
        match self {
            Self::Appended => "appended",
            Self::Revised => "revised",
            Self::RevisionConflict => "revision_conflict",
            Self::IdempotencyConflict => "idempotency_conflict",
            Self::NoHead => "no_head",
            Self::ModeViolation => "mode_violation",
            Self::BodyUnverified => "body_unverified",
            Self::RetiredRecord => "retired_record",
        }
    }

    fn parse(s: &str) -> Option<Self> {
        Some(match s {
            "appended" => Self::Appended,
            "revised" => Self::Revised,
            "revision_conflict" => Self::RevisionConflict,
            "idempotency_conflict" => Self::IdempotencyConflict,
            "no_head" => Self::NoHead,
            "mode_violation" => Self::ModeViolation,
            "body_unverified" => Self::BodyUnverified,
            "retired_record" => Self::RetiredRecord,
            _ => return None,
        })
    }
}

/// The `durable-record/v1` write receipt returned by [`Stream::append`].
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Receipt {
    /// Stream identifier.
    pub stream: String,
    /// The write's idempotency key.
    pub idempotency_key: String,
    /// Contract outcome.
    pub outcome: WriteOutcome,
    /// Journal sequence number of the accepted entry.
    pub seq: Option<u64>,
    /// The accepted revision.
    pub pin: Option<Pin>,
    /// Body verification status (`verified`) for a record with a body.
    pub body_status: Option<String>,
    /// The stream's declared failure model.
    pub failure_model: Option<String>,
    /// The current head, on a revision conflict.
    pub current: Option<Pin>,
}

/// The `durable-record/v1` reconciliation result.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Reconciliation {
    /// `found` or `unknown`.
    pub result: String,
    /// Why the result is `unknown` (`coverage_unprovable`).
    pub reason: Option<String>,
    /// Stream identifier.
    pub stream: String,
    /// The key looked up.
    pub idempotency_key: String,
    /// The caller's attempt instant.
    pub attempted_at: String,
    /// The stored receipt (canonical JSON), when found.
    pub receipt: Option<Vec<u8>>,
}

/// The latest revision of a record on a stream.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct HeadInfo {
    /// The head revision.
    pub pin: Pin,
    /// Whether the record is retired.
    pub retired: bool,
}

/// Redacted metadata of one record revision.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct RevisionView {
    /// The revision.
    pub pin: Pin,
    /// The revision it supersedes.
    pub supersedes: Option<Pin>,
    /// The record's payload contract.
    pub payload_contract: String,
    /// Whether the record references an external body.
    pub has_body: bool,
    /// Whether the record is retired.
    pub retired: bool,
    /// Journal instant of the revision.
    pub at: String,
}

/// The purpose of a [`Stream::release`] call.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum UsePurpose {
    /// Serve current content; retired records are excluded.
    Serve,
    /// Export a selection.
    Export,
    /// Reuse content.
    Reuse,
}

impl UsePurpose {
    fn as_str(self) -> &'static str {
        match self {
            Self::Serve => "serve",
            Self::Export => "export",
            Self::Reuse => "reuse",
        }
    }
}

/// Whether external body bytes are required and delivered.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Default)]
pub enum UseClosure {
    /// Deliver record and body frames; every referenced body is required.
    #[default]
    FullContent,
    /// Deliver record frames only.
    MetadataOnly,
}

impl UseClosure {
    fn as_str(self) -> &'static str {
        match self {
            Self::FullContent => "full_content",
            Self::MetadataOnly => "metadata_only",
        }
    }
}

/// A release request evaluated at the current cut.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct UseRequest {
    /// Purpose of the release.
    pub purpose: UsePurpose,
    /// Pins to release; empty selects every eligible revision.
    pub selection: Vec<Pin>,
    /// Body closure.
    pub closure: UseClosure,
}

/// Outcome of [`Stream::release`].
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum ReleaseOutcome {
    /// Every frame was delivered.
    Released,
    /// Delivery stopped between frames.
    Partial,
    /// A frame's delivery is uncertain.
    Uncertain,
    /// The release was refused.
    Refused,
}

/// Result of [`Stream::release`].
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ReleaseResult {
    /// Outcome.
    pub outcome: ReleaseOutcome,
    /// Contract refusal reason when refused.
    pub refusal_reason: Option<String>,
    /// The cut the release was evaluated at.
    pub cut: u64,
    /// The released selection, in frame order.
    pub selection: Vec<Pin>,
    /// Frames delivered.
    pub frames_completed: usize,
    /// Index of the frame whose delivery is uncertain.
    pub frame_in_flight: Option<usize>,
    /// The pin of the last fully delivered frame.
    pub released_through_pin: Option<Pin>,
    /// Revisions not selected.
    pub omitted: Vec<Pin>,
    /// [`Error::SinkError`] when a sink callback returned an error. Delivery
    /// stopped there: the outcome is `Uncertain`, `frame_in_flight` names the
    /// failed frame, and the progress fields describe the frames delivered
    /// before it. The callback error's text is not disclosed.
    pub sink_error: Option<Error>,
}

/// Kind of a released frame.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum FrameKind {
    /// Canonical record bytes.
    Record,
    /// External body bytes.
    Body,
}

/// Describes one frame delivered to a [`ReleaseSink`].
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct FrameHeader {
    /// Frame index.
    pub index: usize,
    /// The revision the frame belongs to.
    pub pin: Pin,
    /// Frame kind.
    pub kind: FrameKind,
    /// Frame length in bytes.
    pub length: u64,
    /// `sha256:<hex>` of the frame bytes.
    pub digest: String,
}

/// Receives frames during [`Stream::release`] while the store lock is held.
/// The reader is revoked when `frame` returns; calling back into the store
/// from `frame` fails with [`Error::SinkReentered`].
pub trait ReleaseSink {
    /// Consumes one frame.
    fn frame(&mut self, header: &FrameHeader, reader: &mut FrameReader<'_>) -> Result<(), Error>;
}

/// Reader over one released frame; it hashes what it delivers.
pub struct FrameReader<'a> {
    data: &'a [u8],
    pos: usize,
    hasher: Sha256,
}

impl Read for FrameReader<'_> {
    fn read(&mut self, buf: &mut [u8]) -> io::Result<usize> {
        let n = buf.len().min(self.data.len() - self.pos);
        buf[..n].copy_from_slice(&self.data[self.pos..self.pos + n]);
        self.hasher.update(&buf[..n]);
        self.pos += n;
        Ok(n)
    }
}

/// A handle to one content stream inside a [`Store`].
pub struct Stream<'s> {
    store: &'s Store,
    id: String,
    mode: Mode,
}

impl std::fmt::Debug for Stream<'_> {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("Stream")
            .field("id", &self.id)
            .field("mode", &self.mode)
            .finish()
    }
}

pub(crate) struct Replayed {
    pub(crate) st: State,
    jpath: String,
    sdir: String,
    raw: Vec<u8>,
    last_seq: i64,
    last_at: Option<Instant>,
    mode: Option<Mode>,
}

fn reject_reason(r: &wire::Reject) -> String {
    r.reason.to_owned()
}

fn valid_wire_string(s: &str, min: usize, max: usize) -> bool {
    let n = s.chars().count();
    n >= min && n <= max
}

fn valid_record_id(s: &str) -> bool {
    let b = s.as_bytes();
    !b.is_empty()
        && b.len() <= 128
        && b[0].is_ascii_alphanumeric()
        && b.iter()
            .all(|&c| c.is_ascii_alphanumeric() || matches!(c, b'.' | b'_' | b':' | b'-'))
}

fn digest_hex(d: &str) -> &str {
    d.strip_prefix("sha256:").unwrap_or(d)
}

fn m(name: &str, value: Node) -> Member {
    Member {
        name: name.to_owned(),
        value,
    }
}

fn s(v: &str) -> Node {
    Node::string(v)
}

fn str_at<'a>(n: &'a Node, name: &str) -> &'a str {
    n.get(name).map_or("", |v| v.str_val.as_str())
}

fn int_at(n: &Node, name: &str) -> i64 {
    n.get(name).map_or(0, |v| v.int_val)
}

fn to_pin(p: &journal::Pin) -> Pin {
    Pin {
        stream: p.stream.clone(),
        record_id: p.record_id.clone(),
        revision: u64::try_from(p.revision).unwrap_or(0),
        digest: p.digest.clone(),
    }
}

fn pin_from_wire(n: Option<&Node>) -> Option<Pin> {
    let n = n?;
    Some(Pin {
        stream: str_at(n, "stream").to_owned(),
        record_id: str_at(n, "record_id").to_owned(),
        revision: u64::try_from(int_at(n, "revision")).ok()?,
        digest: str_at(n, "digest").to_owned(),
    })
}

fn receipt_from_node(n: &Node) -> Result<Receipt, Error> {
    let outcome = WriteOutcome::parse(str_at(n, "outcome"))
        .ok_or_else(|| Error::schema("write receipt outcome"))?;
    let opt = |k: &str| n.get(k).map(|v| v.str_val.clone());
    Ok(Receipt {
        stream: str_at(n, "stream").to_owned(),
        idempotency_key: str_at(n, "idempotency_key").to_owned(),
        outcome,
        seq: n.get("seq").and_then(|v| u64::try_from(v.int_val).ok()),
        pin: pin_from_wire(n.get("pin")),
        body_status: opt("body_status"),
        failure_model: opt("failure_model"),
        current: pin_from_wire(n.get("current")),
    })
}

fn pub_pin_node(p: &Pin) -> Node {
    Node::object(vec![
        m("stream", s(&p.stream)),
        m("record_id", s(&p.record_id)),
        m("revision", Node::uint(p.revision)),
        m("digest", s(&p.digest)),
    ])
}

fn receipt_to_node(r: &Receipt) -> Node {
    let mut f = vec![
        m("v", Node::int(1)),
        m("kind", s("receipt")),
        m("stream", s(&r.stream)),
        m("idempotency_key", s(&r.idempotency_key)),
        m("outcome", s(r.outcome.as_str())),
    ];
    if let Some(seq) = r.seq.filter(|v| *v != 0) {
        f.push(m("seq", Node::uint(seq)));
    }
    if let Some(p) = &r.pin {
        f.push(m("pin", pub_pin_node(p)));
    }
    if let Some(b) = r.body_status.as_deref().filter(|b| !b.is_empty()) {
        f.push(m("body_status", s(b)));
    }
    if let Some(fm) = r.failure_model.as_deref().filter(|b| !b.is_empty()) {
        f.push(m("failure_model", s(fm)));
    }
    if let Some(p) = &r.current {
        f.push(m("current", pub_pin_node(p)));
    }
    Node::object(f)
}

fn simple_receipt(stream: &str, key: &str, outcome: WriteOutcome) -> Receipt {
    Receipt {
        stream: stream.to_owned(),
        idempotency_key: key.to_owned(),
        outcome,
        seq: None,
        pin: None,
        body_status: None,
        failure_model: None,
        current: None,
    }
}

fn entry_head(seq: i64, stream: &str, typ: &str, at: &str) -> Vec<Member> {
    vec![
        m("v", Node::int(1)),
        m("seq", Node::int(seq)),
        m("group", Node::int(seq)),
        m("group_size", Node::int(1)),
        m("stream", s(stream)),
        m("type", s(typ)),
        m("at", s(at)),
    ]
}

/// Splits `raw` into entries up to the last complete atomic group, reporting
/// a torn final line or an incomplete final group as damage.
fn parse_stream_prefix(raw: &[u8]) -> Result<(Vec<Node>, Option<Error>), Error> {
    if raw.is_empty() {
        return Err(Error::journal_invalid("empty"));
    }
    if raw.contains(&b'\r') {
        return Err(Error::journal_invalid(wire::REASON_INGEST));
    }
    let mut nodes = Vec::new();
    let (mut offset, mut last_cut_seq, mut last_cut_off) = (0i64, 0u64, 0i64);
    let (mut group_start, mut group_size, mut open) = (0i64, 0i64, false);
    let total = i64::try_from(raw.len()).unwrap_or(i64::MAX);
    let damaged = |kind, seq, off: i64| Error::DamagedTail {
        kind,
        last_cut_seq: seq,
        last_cut_offset: off,
        damaged_bytes_count: total - off,
    };
    let mut rest = raw;
    while !rest.is_empty() {
        let Some(nl) = rest.iter().position(|&b| b == b'\n') else {
            return Ok((
                nodes,
                Some(damaged(
                    DamagedTailKind::TornLine,
                    last_cut_seq,
                    last_cut_off,
                )),
            ));
        };
        let line = &rest[..nl];
        rest = &rest[nl + 1..];
        let n = match wire::ingest(line, wire::KIND_JOURNAL_ENTRY) {
            Ok(n) => n,
            Err(_) if rest.is_empty() => {
                return Ok((
                    nodes,
                    Some(damaged(
                        DamagedTailKind::TornLine,
                        last_cut_seq,
                        last_cut_off,
                    )),
                ));
            }
            Err(r) => return Err(Error::journal_invalid(reject_reason(&r))),
        };
        let ints = ["seq", "group", "group_size"]
            .map(|k| n.get(k).filter(|v| v.is_int).map(|v| v.int_val));
        let [Some(seq), Some(grp), Some(sz)] = ints else {
            return Err(Error::journal_invalid(shape::REASON_SHAPE));
        };
        if !open || seq == grp {
            group_start = grp;
            group_size = sz;
            open = true;
        }
        nodes.push(n);
        offset += i64::try_from(nl + 1).unwrap_or(i64::MAX);
        if seq - group_start + 1 == group_size {
            last_cut_seq = u64::try_from(seq).unwrap_or(0);
            last_cut_off = offset;
            open = false;
        }
    }
    if open {
        return Ok((
            nodes,
            Some(damaged(
                DamagedTailKind::IncompleteGroup,
                last_cut_seq,
                last_cut_off,
            )),
        ));
    }
    Ok((nodes, None))
}

impl Store {
    /// Opens or creates the content stream `id` in `mode`. Continuations
    /// are not supported yet. The designated body-removal stream is refused.
    pub fn open_stream(
        &self,
        id: &str,
        mode: Mode,
        continues: Option<Continues>,
    ) -> Result<Stream<'_>, Error> {
        if !valid_stream_id(id) {
            return Err(Error::schema("invalid stream id"));
        }
        if id == self.profile.runtime.body_removal_stream {
            return Err(Error::schema(format!(
                "{id} is the designated body-removal stream"
            )));
        }
        if continues.is_some() {
            return Err(Error::ContinuationUnsupported);
        }
        self.with_lock(|| {
            let sdir = stream_dir(id);
            if !self.exists(&sdir)? {
                let now = format_instant(self.clock.now());
                return create_stream_journal(
                    &self.root,
                    self.fso,
                    &self.appender,
                    id,
                    mode.as_str(),
                    &self.profile.runtime.failure_model,
                    &now,
                    None,
                    false,
                );
            }
            let rep = self.replay_stream(id)?;
            if rep.mode != Some(mode) {
                return Err(Error::schema("stream mode mismatch"));
            }
            Ok(())
        })?;
        Ok(Stream {
            store: self,
            id: id.to_owned(),
            mode,
        })
    }

    /// Reads and validates a content stream's journal under the store lock.
    pub(crate) fn replay_stream(&self, id: &str) -> Result<Replayed, Error> {
        let sdir = stream_dir(id);
        for marker in ["SEALED", "reclaimed.json", "stream-seal.json"] {
            if self.exists(&format!("{sdir}/{marker}"))? {
                return Err(Error::StreamSealed);
            }
        }
        let jpath = stream_journal_file(id);
        let raw = read_file_verified(&self.root, &jpath, self.fso, MAX_JOURNAL_BYTES)?;
        let (nodes, damage) = parse_stream_prefix(&raw)?;
        if let Some(d) = damage {
            return Err(d);
        }
        let mut st = State::new(&self.window_node())
            .map_err(|r| Error::journal_invalid(reject_reason(&r)))?;
        st.set_profile(&self.profile_node);
        let mut mode = None;
        let mut last_at = None;
        for (i, node) in nodes.into_iter().enumerate() {
            shape::validate("v1/journal-entry.schema.json", &node)
                .map_err(|_| Error::journal_invalid(shape::REASON_SHAPE))?;
            if i == 0 {
                mode = node.get("mode").and_then(|v| Mode::parse(&v.str_val));
                if node.get("continues").is_some() {
                    return Err(Error::ContinuationUnsupported);
                }
            }
            if let Some(a) = node.get("at").filter(|a| a.kind == Kind::String) {
                last_at = parse_instant(&a.str_val).ok();
            }
            st.ingest(node)
                .map_err(|r| Error::journal_invalid(reject_reason(&r)))?;
        }
        journal::check_journal(st.journal())
            .map_err(|r| Error::journal_invalid(reject_reason(&r)))?;
        if st.stream() != id {
            return Err(Error::journal_invalid("stream_mismatch"));
        }
        self.sync_stream_indexes(&sdir, &st, &raw)?;
        Ok(Replayed {
            last_seq: i64::try_from(st.len()).unwrap_or(i64::MAX),
            st,
            jpath,
            sdir,
            raw,
            last_at,
            mode,
        })
    }

    fn append_line(&self, rep: &mut Replayed, line: &[u8]) -> Result<(), Error> {
        let raw_entry = line.strip_suffix(b"\n").unwrap_or(line);
        let ingested = wire::ingest(raw_entry, wire::KIND_JOURNAL_ENTRY)
            .map_err(|r| Error::schema(format!("entry wire invalid: {}", r.reason)))?;
        shape::validate("v1/journal-entry.schema.json", &ingested)
            .map_err(|_| Error::schema("entry shape invalid"))?;
        self.appender
            .append(&self.root, &rep.jpath, rep.raw.len() as u64, line, false)
            .map_err(map_jfile_err)?;
        rep.raw.extend_from_slice(line);
        rep.last_seq = i64::try_from(rep.st.len()).unwrap_or(i64::MAX);
        self.sync_stream_indexes(&rep.sdir, &rep.st, &rep.raw)
            .map_err(|e| match e {
                Error::Io { op, class, .. } => Error::Io {
                    op,
                    class,
                    indeterminate: true,
                },
                _ => Error::Io {
                    op: "index",
                    class: IoClass::Other,
                    indeterminate: true,
                },
            })
    }

    fn now_micros(&self) -> Instant {
        self.clock.now()
    }
}

impl Stream<'_> {
    /// The stream's identifier.
    pub fn id(&self) -> &str {
        &self.id
    }

    /// The stream's revision mode.
    pub fn mode(&self) -> Mode {
        self.mode
    }

    /// Writes a content record (appended or revised).
    pub fn append(&self, w: &Write) -> Result<Receipt, Error> {
        if !valid_wire_string(&w.idempotency_key, 1, 256) {
            return Err(Error::schema("idempotency key invalid"));
        }
        let rec = wire::ingest(&w.record, "record")
            .map_err(|r| Error::schema(format!("record ingest: {}", r.reason)))?;
        shape::validate("v1/record.schema.json", &rec)
            .map_err(|_| Error::schema("record shape"))?;
        if str_at(&rec, "stream") != self.id {
            return Err(Error::schema("record stream mismatch"));
        }
        let rid = str_at(&rec, "record_id").to_owned();
        let rev = int_at(&rec, "revision");
        if rev > 1
            && rec
                .get("supersedes")
                .is_none_or(|sup| int_at(sup, "revision") != rev - 1)
        {
            return Err(Error::schema("supersedes_revision"));
        }
        let rec_digest =
            canon::digest(&wire::encode(&rec)).map_err(|_| Error::schema("record digest"))?;
        let key = w.idempotency_key.as_str();
        let store = self.store;
        let out = store.with_lock(|| {
            let mut rep = store.replay_stream(&self.id)?;
            if rep.last_seq >= MAX_SAFE_INTEGER {
                return Err(Error::schema("stream seq exhausted"));
            }
            let now = store.now_micros();
            if rep.last_at.is_some_and(|la| now < la) {
                return Err(Error::ClockRegression);
            }
            let at = format_instant(now);
            let seq = rep.last_seq + 1;

            if let Some(live) = rep.st.live_key(key, now) {
                if live.kind != "content" || live.value != rec_digest {
                    return Ok(simple_receipt(
                        &self.id,
                        key,
                        WriteOutcome::IdempotencyConflict,
                    ));
                }
                let mut f = entry_head(seq, &self.id, "deduplicated", &at);
                f.push(m("record_id", s(&rid)));
                f.push(m("idempotency_key", s(key)));
                let entry = Node::object(f);
                rep.st
                    .ingest(entry.clone())
                    .map_err(|r| Error::journal_invalid(reject_reason(&r)))?;
                let mut line = wire::encode(&entry);
                line.push(b'\n');
                store.append_line(&mut rep, &line)?;
                return receipt_from_node(&live.receipt);
            }

            let body = rec.get("body");
            let body_hex = body.map(|b| digest_hex(str_at(b, "digest")).to_owned());
            if let (Some(b), Some(h)) = (body, &body_hex) {
                let desig = store.read_designated_state()?;
                let bst = store.eval_body_state(&desig, h, int_at(b, "length"))?;
                if bst.kind != BodyStateKind::Available {
                    return Ok(simple_receipt(&self.id, key, WriteOutcome::BodyUnverified));
                }
                if let Some(lease) = &w.lease {
                    if lease.digest != str_at(b, "digest")
                        || lease.id.is_empty()
                        || now >= lease.expires_at
                    {
                        return Err(Error::schema("invalid or expired publication lease"));
                    }
                }
            }
            if rep.st.is_retired(&rid) {
                return Ok(simple_receipt(&self.id, key, WriteOutcome::RetiredRecord));
            }
            let head = rep.st.head_pin(&rid);
            if rev > 1 && rep.mode == Some(Mode::SingleRevision) {
                return Ok(simple_receipt(&self.id, key, WriteOutcome::ModeViolation));
            }
            if rev > 1 && head.is_none() {
                return Ok(simple_receipt(&self.id, key, WriteOutcome::NoHead));
            }
            let conflict = match (&head, rev) {
                (Some(_), 1) => true,
                (None, _) => false,
                (Some(h), _) => {
                    h.revision != rev - 1
                        || rec.get("supersedes").map(|sup| str_at(sup, "digest"))
                            != Some(h.digest.as_str())
                }
            };
            if conflict {
                let h = head.expect("conflict implies a head");
                let mut f = entry_head(seq, &self.id, "conflicted", &at);
                f.push(m("record_id", s(&rid)));
                f.push(m("reason", s("revision_conflict")));
                f.push(m(
                    "presented",
                    Node::object(vec![
                        m("revision", Node::int(rev)),
                        m("digest", s(&rec_digest)),
                    ]),
                ));
                f.push(m(
                    "current",
                    Node::object(vec![
                        m("revision", Node::int(h.revision)),
                        m("digest", s(&h.digest)),
                    ]),
                ));
                let entry = Node::object(f);
                rep.st
                    .ingest(entry.clone())
                    .map_err(|r| Error::journal_invalid(reject_reason(&r)))?;
                let mut line = wire::encode(&entry);
                line.push(b'\n');
                store.append_line(&mut rep, &line)?;
                let mut out = simple_receipt(&self.id, key, WriteOutcome::RevisionConflict);
                out.current = Some(to_pin(&h));
                return Ok(out);
            }

            let typ = if rev > 1 { "revised" } else { "appended" };
            let mut f = entry_head(seq, &self.id, typ, &at);
            f.push(m("record", rec.clone()));
            f.push(m("digest", s(&rec_digest)));
            f.push(m("idempotency_key", s(key)));
            if body.is_some() {
                f.push(m("body_status", s("verified")));
            }
            if let Some(imp) = &w.imported_from {
                f.push(m(
                    "imported_from",
                    Node::object(vec![
                        m("capability", s(&imp.capability)),
                        m("pin", pub_pin_node(&imp.pin)),
                    ]),
                ));
            }
            let entry = Node::object(f);
            shape::validate("v1/journal-entry.schema.json", &entry)
                .map_err(|_| Error::schema("journal entry shape"))?;
            rep.st
                .ingest(entry.clone())
                .map_err(|r| Error::schema(reject_reason(&r)))?;
            let mut line = wire::encode(&entry);
            line.push(b'\n');
            store.append_line(&mut rep, &line)?;
            if let (Some(h), Some(lease)) = (&body_hex, &w.lease) {
                let _ = store.consume_lease(h, lease);
            }
            let live = rep
                .st
                .live_key(key, now)
                .ok_or_else(|| Error::journal_invalid("missing_accepted_receipt"))?;
            receipt_from_node(&live.receipt)
        })?;
        let n = receipt_to_node(&out);
        let checked = wire::ingest(&wire::encode(&n), "write_receipt")
            .map_err(|_| Error::schema("write receipt wire invalid"))?;
        shape::validate("v1/write-receipt.schema.json", &checked)
            .map_err(|_| Error::schema("write receipt shape invalid"))?;
        Ok(out)
    }

    /// Looks up `key` in the stream's idempotency window for an attempt at
    /// `attempted_at`, returning `found` or `unknown`.
    pub fn reconcile(&self, key: &str, attempted_at: Instant) -> Result<Reconciliation, Error> {
        if !valid_wire_string(key, 1, 256) {
            return Err(Error::schema("idempotency key invalid"));
        }
        let att = format_instant(attempted_at);
        parse_instant(&att)?;
        let store = self.store;
        let node = store.with_lock(|| {
            let rep = store.replay_stream(&self.id)?;
            let now = store.now_micros();
            if rep.last_at.is_some_and(|la| now < la) {
                return Err(Error::ClockRegression);
            }
            let n = rep
                .st
                .reconcile(key, &format_instant(now), &att)
                .map_err(|r| Error::schema(reject_reason(&r)))?;
            shape::validate("v1/write-receipt.schema.json", &n)
                .map_err(|_| Error::schema("reconciliation shape invalid"))?;
            Ok(n)
        })?;
        let receipt = match node.get("receipt") {
            Some(r) => Some(canon::canonicalize(&wire::encode(r))?),
            None => None,
        };
        Ok(Reconciliation {
            result: str_at(&node, "result").to_owned(),
            reason: node.get("reason").map(|v| v.str_val.clone()),
            stream: str_at(&node, "stream").to_owned(),
            idempotency_key: str_at(&node, "idempotency_key").to_owned(),
            attempted_at: str_at(&node, "attempted_at").to_owned(),
            receipt,
        })
    }

    /// The head revision and retirement status of `record_id`, if any.
    pub fn head(&self, record_id: &str) -> Result<Option<HeadInfo>, Error> {
        if !valid_record_id(record_id) {
            return Err(Error::schema("invalid record_id"));
        }
        let store = self.store;
        store.with_lock(|| {
            let rep = store.replay_stream(&self.id)?;
            Ok(rep.st.head_pin(record_id).map(|hp| HeadInfo {
                pin: to_pin(&hp),
                retired: rep.st.is_retired(record_id),
            }))
        })
    }

    /// The accepted revisions of `record_id`, in journal order.
    pub fn revisions(&self, record_id: &str) -> Result<Vec<RevisionView>, Error> {
        if !valid_record_id(record_id) {
            return Err(Error::schema("invalid record_id"));
        }
        let store = self.store;
        store.with_lock(|| {
            let rep = store.replay_stream(&self.id)?;
            let retired = rep.st.is_retired(record_id);
            let mut out = Vec::new();
            for e in rep.st.journal() {
                if !matches!(str_at(e, "type"), "appended" | "revised") {
                    continue;
                }
                let Some(rec) = e.get("record") else { continue };
                if str_at(rec, "record_id") != record_id {
                    continue;
                }
                let supersedes = rec
                    .get("supersedes")
                    .filter(|v| v.kind == Kind::Object)
                    .map(|sup| Pin {
                        stream: self.id.clone(),
                        record_id: record_id.to_owned(),
                        revision: u64::try_from(int_at(sup, "revision")).unwrap_or(0),
                        digest: str_at(sup, "digest").to_owned(),
                    });
                out.push(RevisionView {
                    pin: Pin {
                        stream: self.id.clone(),
                        record_id: record_id.to_owned(),
                        revision: u64::try_from(int_at(rec, "revision")).unwrap_or(0),
                        digest: str_at(e, "digest").to_owned(),
                    },
                    supersedes,
                    payload_contract: str_at(rec, "payload_contract").to_owned(),
                    has_body: rec.get("body").is_some(),
                    retired,
                    at: str_at(e, "at").to_owned(),
                });
            }
            Ok(out)
        })
    }

    /// Evaluates `req` at the current cut while holding the store lock and
    /// delivers eligible frames to `sink` (record frames, each followed by
    /// its body frame under full-content closure). With no sink, it only
    /// evaluates. A sink callback error is reported in
    /// [`ReleaseResult::sink_error`], together with the delivery progress.
    pub fn release(
        &self,
        req: &UseRequest,
        sink: Option<&mut dyn ReleaseSink>,
    ) -> Result<ReleaseResult, Error> {
        let store = self.store;
        let refused = |reason: &str, cut: u64| ReleaseResult {
            outcome: ReleaseOutcome::Refused,
            refusal_reason: Some(reason.to_owned()),
            cut,
            selection: Vec::new(),
            frames_completed: 0,
            frame_in_flight: None,
            released_through_pin: None,
            omitted: Vec::new(),
            sink_error: None,
        };
        let res = store.with_lock(|| {
            let desig = match req.closure {
                UseClosure::FullContent => Some(store.read_designated_state()?),
                UseClosure::MetadataOnly => None,
            };
            let rep = match store.replay_stream(&self.id) {
                Ok(r) => r,
                Err(Error::StreamSealed | Error::JournalInvalid { .. }) => {
                    return Ok(refused(journal::REASON_PROTECTION_UNAVAILABLE, 0));
                }
                Err(e) => return Err(e),
            };
            let cut = u64::try_from(rep.last_seq).unwrap_or(0);
            let records = rep.st.records();
            let mut all: Vec<&journal::Pin> = records.keys().collect();
            all.sort_by(|a, b| {
                a.record_id
                    .cmp(&b.record_id)
                    .then(a.revision.cmp(&b.revision))
            });

            let sel: Vec<journal::Pin> = if req.selection.is_empty() {
                all.iter()
                    .filter(|p| {
                        !(req.purpose == UsePurpose::Serve && rep.st.is_retired(&p.record_id))
                    })
                    .filter(|p| matches!(rep.st.blocked(p), Ok(false)))
                    .map(|p| (*p).clone())
                    .collect()
            } else {
                req.selection
                    .iter()
                    .map(|p| journal::Pin {
                        stream: p.stream.clone(),
                        record_id: p.record_id.clone(),
                        revision: i64::try_from(p.revision).unwrap_or(i64::MAX),
                        digest: p.digest.clone(),
                    })
                    .collect()
            };
            let sel_set: HashSet<&journal::Pin> = sel.iter().collect();
            let omitted: Vec<&journal::Pin> = all
                .iter()
                .copied()
                .filter(|p| !sel_set.contains(p))
                .collect();

            let mut body_nodes = Vec::new();
            let mut seen = HashSet::new();
            let mut body_by_pin: HashMap<&journal::Pin, (String, Vec<u8>)> = HashMap::new();
            if let Some(desig) = &desig {
                for p in &sel {
                    let Some(b) = records.get(p).and_then(|r| r.get("body")) else {
                        continue;
                    };
                    let dig = str_at(b, "digest").to_owned();
                    let h = digest_hex(&dig).to_owned();
                    match store.eval_body_state(desig, &h, int_at(b, "length")) {
                        Ok(bst) if bst.kind == BodyStateKind::Available => {}
                        _ => continue,
                    }
                    let Ok(bytes) = read_file_verified(
                        &store.root,
                        &body_file(&h),
                        store.fso,
                        MAX_SAFE_INTEGER,
                    ) else {
                        continue;
                    };
                    if seen.insert(dig.clone()) {
                        body_nodes.push(Node::object(vec![
                            m("digest", s(&dig)),
                            m("hex", s(&hex_encode(&bytes))),
                        ]));
                    }
                    body_by_pin.insert(p, (dig, bytes));
                }
            }

            let cut_node = Node::int(rep.last_seq);
            let use_step = Node::object(vec![
                m("stream", s(&self.id)),
                m("purpose", s(req.purpose.as_str())),
                m("protection_state", s("verified")),
                m("protection_cut", cut_node.clone()),
                m("current_cut", cut_node),
                m("inherited_block", Node::boolean(false)),
                m("selection", Node::array(sel.iter().map(pin_node).collect())),
                m(
                    "omitted",
                    Node::array(omitted.iter().map(|p| pin_node(p)).collect()),
                ),
                m("closure", s(req.closure.as_str())),
                m("bodies", Node::array(body_nodes)),
            ]);
            let use_out = match rep.st.use_request(&use_step) {
                Ok(o) => o,
                Err(r) => return Ok(refused(r.reason, cut)),
            };
            if str_at(&use_out, "outcome") == "refused" {
                return Ok(refused(str_at(&use_out, "reason"), cut));
            }

            let mut frames: Vec<(FrameHeader, Vec<u8>)> = Vec::new();
            let mut selection = Vec::new();
            for p in &sel {
                let rec = &records[p];
                let bytes = canon::canonicalize(&wire::encode(rec))?;
                let pin = to_pin(p);
                selection.push(pin.clone());
                frames.push((
                    FrameHeader {
                        index: frames.len(),
                        pin: pin.clone(),
                        kind: FrameKind::Record,
                        length: bytes.len() as u64,
                        digest: p.digest.clone(),
                    },
                    bytes,
                ));
                if let Some((dig, bytes)) = body_by_pin.get(p) {
                    frames.push((
                        FrameHeader {
                            index: frames.len(),
                            pin,
                            kind: FrameKind::Body,
                            length: bytes.len() as u64,
                            digest: dig.clone(),
                        },
                        bytes.clone(),
                    ));
                }
            }
            let mut res = ReleaseResult {
                outcome: ReleaseOutcome::Released,
                refusal_reason: None,
                cut,
                selection,
                frames_completed: 0,
                frame_in_flight: None,
                released_through_pin: None,
                omitted: omitted.iter().map(|p| to_pin(p)).collect(),
                sink_error: None,
            };
            let Some(sink) = sink else {
                return Ok(res);
            };
            for (i, (header, data)) in frames.iter().enumerate() {
                let mut reader = FrameReader {
                    data,
                    pos: 0,
                    hasher: Sha256::new(),
                };
                let cb = {
                    let _g = ReleaseGuard::enter(store);
                    sink.frame(header, &mut reader)
                };
                let got = format!("sha256:{}", hex_encode(&reader.hasher.finalize()));
                if cb.is_err() || reader.pos as u64 != header.length || got != header.digest {
                    res.outcome = ReleaseOutcome::Uncertain;
                    res.frame_in_flight = Some(i);
                    if cb.is_err() {
                        res.sink_error = Some(Error::SinkError);
                    }
                    return Ok(res);
                }
                res.frames_completed += 1;
                res.released_through_pin = Some(header.pin.clone());
            }
            Ok(res)
        })?;
        Ok(res)
    }
}

/// Marks the current thread as inside a release sink callback, so a
/// re-entrant store call fails with [`Error::SinkReentered`].
pub(crate) struct ReleaseGuard<'a> {
    slot: &'a Mutex<Option<std::thread::ThreadId>>,
}

impl<'a> ReleaseGuard<'a> {
    fn enter(store: &'a Store) -> Self {
        *store
            .release_thread
            .lock()
            .unwrap_or_else(|e| e.into_inner()) = Some(std::thread::current().id());
        Self {
            slot: &store.release_thread,
        }
    }
}

impl Drop for ReleaseGuard<'_> {
    fn drop(&mut self) {
        *self.slot.lock().unwrap_or_else(|e| e.into_inner()) = None;
    }
}
