// SPDX-License-Identifier: MIT OR Apache-2.0

//! Fourth evaluation stage (`journal`) of the durable-record/v1 reader:
//! journal invariants, envelope versions, instants, entry order, continuation
//! chain & restriction-review manifests, protection holds, and body lifecycle
//! state, matching `internal/journal` in the Go implementation.

use std::collections::{BTreeMap, HashMap, HashSet};

use crate::canon;
use crate::instant::{parse_instant, Instant};
use crate::shape::{self, REASON_SHAPE};
use crate::wire::{
    self, Kind, Member, Node, Reject, KIND_JOURNAL_ENTRY, MAX_SAFE_INTEGER, REASON_INGEST,
    REASON_NUMERIC_DOMAIN,
};

pub(crate) const REASON_EMPTY: &str = "empty";
pub(crate) const REASON_SEQ_GAP: &str = "seq_gap";
pub(crate) const REASON_STREAM_MISMATCH: &str = "stream_mismatch";
pub(crate) const REASON_STREAM_OPENED_POSITION: &str = "stream_opened_position";
pub(crate) const REASON_GROUP: &str = "group";
pub(crate) const REASON_INCONSISTENT_CUT: &str = "inconsistent_cut";
pub(crate) const REASON_DIGEST: &str = "digest";
pub(crate) const REASON_SUPERSEDES_REVISION: &str = "supersedes_revision";
pub(crate) const REASON_REVISION_CHAIN: &str = "revision_chain";
pub(crate) const REASON_MODE_VIOLATION: &str = "mode_violation";
pub(crate) const REASON_CONFLICT_CURRENT: &str = "conflict_current";
pub(crate) const REASON_JOURNAL_VERSION: &str = "journal_version";
pub(crate) const REASON_RECORD_VERSION: &str = "record_version";
pub(crate) const REASON_INSTANT_SPELLING: &str = "instant_spelling";
pub(crate) const REASON_JOURNAL_TIME: &str = "journal_time";
pub(crate) const REASON_BODY_REMOVAL_STREAM: &str = "body_removal_stream";
pub(crate) const REASON_IDEMPOTENCY_WINDOW: &str = "idempotency_window";
pub(crate) const REASON_CAPABILITY_NOT_COMPILED: &str = "capability_not_compiled";
pub(crate) const REASON_STRATUM_NOT_COMPILED: &str = "stratum_not_compiled";
pub(crate) const REASON_ACCEPTED_KEY_REUSE: &str = "accepted_key_reuse";
pub(crate) const REASON_INVALID_DEDUPLICATION: &str = "invalid_deduplication";
pub(crate) const REASON_RETIRED_RECORD: &str = "retired_record";
pub(crate) const REASON_REQUEST_DIGEST: &str = "request_digest";
pub(crate) const REASON_OPERATION_TYPE: &str = "operation_type";
pub(crate) const REASON_TARGET_NOT_FOUND: &str = "target_not_found";
pub(crate) const REASON_BODY_MISMATCH: &str = "body_mismatch";
pub(crate) const REASON_HOLD_NOT_ACTIVE: &str = "hold_not_active";
pub(crate) const REASON_HOLD_MISMATCH: &str = "hold_mismatch";
pub(crate) const REASON_OBSERVATION_ORDER: &str = "observation_order";
pub(crate) const REASON_BODY_LIFECYCLE: &str = "body_lifecycle";
pub(crate) const REASON_CONTINUATION: &str = "continuation";
#[cfg_attr(not(test), allow(dead_code))]
pub(crate) const REASON_RECEIPT_MISMATCH: &str = "receipt_mismatch";
#[cfg_attr(not(test), allow(dead_code))]
pub(crate) const REASON_IDEMPOTENCY_CONFLICT: &str = "idempotency_conflict";
pub(crate) const REASON_INVALID_PURPOSE: &str = "invalid_purpose";
pub(crate) const REASON_PROTECTION_UNAVAILABLE: &str = "protection_unavailable";
pub(crate) const REASON_RESTRICTIONS_PENDING: &str = "restrictions_pending";
pub(crate) const REASON_EXPORT_SELECTION: &str = "export_selection";
pub(crate) const REASON_PROTECTED_USE: &str = "protected_use";
pub(crate) const REASON_BODY_CLOSURE: &str = "body_closure";

/// Identifies a content-addressed body by its digest and byte length.
#[derive(Debug, Clone, PartialEq, Eq, Hash)]
pub(crate) struct BodyKey {
    pub digest: String,
    pub length: i64,
}

/// Identifies one revision of one record on a stream.
#[derive(Debug, Clone, PartialEq, Eq, Hash)]
pub(crate) struct Pin {
    pub stream: String,
    pub record_id: String,
    pub revision: i64,
    pub digest: String,
}

fn reject(reason: &'static str, detail: impl Into<String>) -> Reject {
    Reject {
        reason,
        detail: detail.into(),
    }
}

fn int_of(n: &Node, name: &str) -> Result<i64, Reject> {
    let Some(v) = n.get(name) else {
        return Err(reject(
            REASON_SHAPE,
            format!("{name} is not a protocol integer"),
        ));
    };
    if v.kind != Kind::Number || !v.is_int {
        return Err(reject(
            REASON_SHAPE,
            format!("{name} is not a protocol integer"),
        ));
    }
    Ok(v.int_val)
}

fn str_of<'a>(n: &'a Node, name: &str) -> Result<&'a str, Reject> {
    let Some(v) = n.get(name) else {
        return Err(reject(REASON_SHAPE, format!("{name} is not a string")));
    };
    if v.kind != Kind::String {
        return Err(reject(REASON_SHAPE, format!("{name} is not a string")));
    }
    Ok(&v.str_val)
}

fn present(n: &Node, name: &str) -> bool {
    matches!(n.get(name), Some(v) if v.kind != Kind::Null)
}

pub(crate) fn equal_nodes(a: Option<&Node>, b: Option<&Node>) -> bool {
    match (a, b) {
        (None, None) => true,
        (Some(x), Some(y)) => {
            if x.kind != y.kind {
                return false;
            }
            match x.kind {
                Kind::Null => true,
                Kind::Bool => x.bool_val == y.bool_val,
                Kind::String => x.str_val == y.str_val,
                Kind::Number => {
                    if x.is_int && y.is_int {
                        x.int_val == y.int_val
                    } else {
                        !x.is_int && !y.is_int && x.token == y.token
                    }
                }
                Kind::Array => {
                    x.elems.len() == y.elems.len()
                        && x.elems
                            .iter()
                            .zip(&y.elems)
                            .all(|(ea, eb)| equal_nodes(Some(ea), Some(eb)))
                }
                Kind::Object => {
                    x.fields.len() == y.fields.len()
                        && x.fields
                            .iter()
                            .all(|m| equal_nodes(Some(&m.value), y.get(&m.name)))
                }
            }
        }
        _ => false,
    }
}

fn instant_of(s: &str, where_ctx: &str) -> Result<Instant, Reject> {
    parse_instant(s).map_err(|_| {
        reject(
            REASON_INSTANT_SPELLING,
            format!("{where_ctx}: {s:?} is not a canonical v1 instant"),
        )
    })
}

fn days_in_month(y: i32, m: i32) -> i32 {
    match m {
        1 | 3 | 5 | 7 | 8 | 10 | 12 => 31,
        4 | 6 | 9 | 11 => 30,
        2 => {
            if y % 4 == 0 && (y % 100 != 0 || y % 400 == 0) {
                29
            } else {
                28
            }
        }
        _ => 0,
    }
}

fn parse_v0_instant(s: &str) -> bool {
    let b = s.as_bytes();
    if b.len() < 20 {
        return false;
    }
    if !b[0..4].iter().all(u8::is_ascii_digit)
        || b[4] != b'-'
        || !b[5..7].iter().all(u8::is_ascii_digit)
        || b[7] != b'-'
        || !b[8..10].iter().all(u8::is_ascii_digit)
        || !matches!(b[10], b'T' | b't')
        || !b[11..13].iter().all(u8::is_ascii_digit)
        || b[13] != b':'
        || !b[14..16].iter().all(u8::is_ascii_digit)
        || b[16] != b':'
        || !b[17..19].iter().all(u8::is_ascii_digit)
    {
        return false;
    }
    let year: i32 = s[0..4].parse().unwrap_or(-1);
    let month: i32 = s[5..7].parse().unwrap_or(-1);
    let day: i32 = s[8..10].parse().unwrap_or(-1);
    let hour: i32 = s[11..13].parse().unwrap_or(-1);
    let minute: i32 = s[14..16].parse().unwrap_or(-1);
    let second: i32 = s[17..19].parse().unwrap_or(-1);
    if !(1..=9999).contains(&year)
        || !(1..=12).contains(&month)
        || day < 1
        || day > days_in_month(year, month)
        || hour > 23
        || minute > 59
        || second >= 60
    {
        return false;
    }
    let mut i = 19;
    if b.get(i) == Some(&b'.') {
        i += 1;
        let start = i;
        while b.get(i).is_some_and(u8::is_ascii_digit) {
            i += 1;
        }
        if i == start {
            return false;
        }
        let frac = &s[start..i];
        if frac.len() > 6 && !frac[6..].trim_end_matches('0').is_empty() {
            return false;
        }
    }
    let rest = &b[i..];
    let offset_min = if matches!(rest, [b'Z' | b'z']) {
        0
    } else if rest.len() == 6
        && matches!(rest[0], b'+' | b'-')
        && rest[1..3].iter().all(u8::is_ascii_digit)
        && rest[3] == b':'
        && rest[4..6].iter().all(u8::is_ascii_digit)
    {
        let off_h: i32 = s[i + 1..i + 3].parse().unwrap_or(99);
        let off_m: i32 = s[i + 4..i + 6].parse().unwrap_or(99);
        if off_h > 23 || off_m > 59 {
            return false;
        }
        let total = off_h * 60 + off_m;
        if rest[0] == b'-' {
            -total
        } else {
            total
        }
    } else {
        return false;
    };

    let utc_min = hour * 60 + minute - offset_min;
    if year == 1 && month == 1 && day == 1 && utc_min < 0 {
        return false;
    }
    if year == 9999 && month == 12 && day == 31 && utc_min >= 1440 {
        return false;
    }
    true
}

fn record_digest(rec: &Node) -> Result<String, Reject> {
    canon::digest(&wire::encode(rec)).map_err(|e| {
        reject(
            REASON_INGEST,
            format!("record digest canonicalization failed: {e}"),
        )
    })
}

fn jcs_string(n: &Node) -> Result<String, Reject> {
    let bytes = canon::canonicalize(&wire::encode(n))
        .map_err(|e| reject(REASON_INGEST, format!("JCS canonicalization failed: {e}")))?;
    String::from_utf8(bytes).map_err(|e| reject(REASON_INGEST, format!("JCS non-UTF8 output: {e}")))
}

fn slice_digest(entries: &[Node]) -> Result<String, Reject> {
    record_digest(&Node::array(entries.to_vec()))
}

#[derive(Debug, Clone)]
struct Head {
    revision: i64,
    digest: String,
}

fn is_head(n: Option<&Node>, h: &Head) -> bool {
    let Some(n) = n else {
        return false;
    };
    if n.kind != Kind::Object || n.fields.len() != 2 {
        return false;
    }
    let (Some(r), Some(d)) = (n.get("revision"), n.get("digest")) else {
        return false;
    };
    r.kind == Kind::Number
        && r.is_int
        && r.int_val == h.revision
        && d.kind == Kind::String
        && d.str_val == h.digest
}

fn closing_seq(e: &Node, where_ctx: &str) -> Result<i64, Reject> {
    let group = int_of(e, "group")?;
    let size = int_of(e, "group_size")?;
    let end = group + size - 1;
    if end > MAX_SAFE_INTEGER {
        return Err(reject(
            REASON_NUMERIC_DOMAIN,
            format!("{where_ctx}: atomic group closing seq exceeds {MAX_SAFE_INTEGER}"),
        ));
    }
    Ok(end)
}

fn check_supersedes(rec: &Node, where_ctx: &str) -> Result<(), Reject> {
    if !present(rec, "supersedes") {
        return Ok(());
    }
    let rev = int_of(rec, "revision")?;
    let sup = rec
        .get("supersedes")
        .ok_or_else(|| reject(REASON_SHAPE, "missing supersedes"))?;
    let srev = int_of(sup, "revision")?;
    if srev != rev - 1 {
        return Err(reject(REASON_SUPERSEDES_REVISION, where_ctx));
    }
    Ok(())
}

fn check_entry(e: &Node, where_ctx: &str) -> Result<(), Reject> {
    closing_seq(e, where_ctx)?;
    let typ = str_of(e, "type")?;
    if typ != "appended" && typ != "revised" {
        return Ok(());
    }
    let Some(rec) = e.get("record") else {
        return Err(reject(
            REASON_SHAPE,
            format!("{where_ctx}: record is not an object"),
        ));
    };
    if rec.kind != Kind::Object {
        return Err(reject(
            REASON_SHAPE,
            format!("{where_ctx}: record is not an object"),
        ));
    }
    let (Some(rs), Some(es)) = (rec.get("stream"), e.get("stream")) else {
        return Err(reject(REASON_STREAM_MISMATCH, where_ctx));
    };
    if rs.kind != Kind::String || es.kind != Kind::String || rs.str_val != es.str_val {
        return Err(reject(REASON_STREAM_MISMATCH, where_ctx));
    }
    let got = record_digest(rec)?;
    let Some(d) = e.get("digest") else {
        return Err(reject(
            REASON_DIGEST,
            format!("{where_ctx}: recorded digest does not match the record (computed {got})"),
        ));
    };
    if d.kind != Kind::String || d.str_val != got {
        return Err(reject(
            REASON_DIGEST,
            format!("{where_ctx}: recorded digest does not match the record (computed {got})"),
        ));
    }
    check_supersedes(rec, where_ctx)
}

/// Validates a complete v1 journal (`CheckJournal` in Go).
pub(crate) fn check_journal(lines: &[Node]) -> Result<(), Reject> {
    if lines.is_empty() {
        return Err(reject(REASON_EMPTY, "journal has no entries"));
    }
    let mut heads: HashMap<String, Head> = HashMap::new();
    let mut mode = String::new();
    let mut stream = String::new();
    let mut open_group = 0i64;
    let mut open_size = 0i64;
    let mut open = false;

    for (idx, e) in lines.iter().enumerate() {
        let i = (idx + 1) as i64;
        let where_ctx = format!("journal:{i}");
        closing_seq(e, &where_ctx)?;
        let seq = int_of(e, "seq")?;
        if seq != i {
            return Err(reject(
                REASON_SEQ_GAP,
                format!("{where_ctx}: expected seq {i}, found {seq}"),
            ));
        }
        let s = str_of(e, "stream")?;
        if idx == 0 {
            stream = s.to_owned();
        } else if s != stream {
            return Err(reject(REASON_STREAM_MISMATCH, where_ctx));
        }
        let typ = str_of(e, "type")?;
        if (typ == "stream_opened") != (i == 1) {
            return Err(reject(REASON_STREAM_OPENED_POSITION, where_ctx));
        }
        if typ == "stream_opened" {
            mode = str_of(e, "mode")?.to_owned();
            let _ = str_of(e, "failure_model")?;
        }
        let group = int_of(e, "group")?;
        let size = int_of(e, "group_size")?;
        if open && i < open_group + open_size {
            if group != open_group || size != open_size {
                return Err(reject(
                    REASON_GROUP,
                    format!(
                        "{where_ctx}: group ({open_group}, {open_size}) abandoned or changed before it closed"
                    ),
                ));
            }
        } else if group != i {
            return Err(reject(
                REASON_GROUP,
                format!("{where_ctx}: a new group must start at its own seq"),
            ));
        }
        open_group = group;
        open_size = size;
        open = true;

        check_entry(e, &where_ctx)?;
        match typ {
            "appended" | "revised" => {
                let rec = e
                    .get("record")
                    .ok_or_else(|| reject(REASON_SHAPE, "missing record"))?;
                let rid = str_of(rec, "record_id")?.to_owned();
                let rev = int_of(rec, "revision")?;
                let _ = str_of(e, "idempotency_key")?;
                let existing = heads.get(&rid);
                if typ == "appended" {
                    if existing.is_some() || rev != 1 {
                        return Err(reject(
                            REASON_REVISION_CHAIN,
                            format!("{where_ctx}: appended over an existing head"),
                        ));
                    }
                } else {
                    if mode == "single_revision" {
                        return Err(reject(REASON_MODE_VIOLATION, where_ctx));
                    }
                    let Some(h) = existing else {
                        return Err(reject(
                            REASON_REVISION_CHAIN,
                            format!("{where_ctx}: supersedes is not the current head"),
                        ));
                    };
                    if rev != h.revision + 1 || !is_head(rec.get("supersedes"), h) {
                        return Err(reject(
                            REASON_REVISION_CHAIN,
                            format!("{where_ctx}: supersedes is not the current head"),
                        ));
                    }
                }
                let dig = str_of(e, "digest")?.to_owned();
                heads.insert(
                    rid,
                    Head {
                        revision: rev,
                        digest: dig,
                    },
                );
            }
            "conflicted" => {
                let rid = str_of(e, "record_id")?;
                let existing = heads.get(rid);
                if present(e, "current") {
                    let Some(h) = existing else {
                        return Err(reject(
                            REASON_CONFLICT_CURRENT,
                            format!("{where_ctx}: current is not the head at this point"),
                        ));
                    };
                    if !is_head(e.get("current"), h) {
                        return Err(reject(
                            REASON_CONFLICT_CURRENT,
                            format!("{where_ctx}: current is not the head at this point"),
                        ));
                    }
                } else if existing.is_some() {
                    return Err(reject(
                        REASON_CONFLICT_CURRENT,
                        format!("{where_ctx}: current omitted while a head exists"),
                    ));
                }
            }
            _ => {}
        }
    }
    if open && (lines.len() as i64) < open_group + open_size - 1 {
        return Err(reject(REASON_INCONSISTENT_CUT, "final group truncated"));
    }
    Ok(())
}

/// Checks a stratum profile against its own build (`CheckProfile` in Go).
pub(crate) fn check_profile(p: &Node) -> Result<(), Reject> {
    let (Some(build), Some(runtime)) = (p.get("build"), p.get("runtime")) else {
        return Err(reject(REASON_SHAPE, "profile: build or runtime missing"));
    };
    let compiled = string_set(build.get("compiled_capabilities"))?;
    let Some(enabled) = runtime.get("enabled_capabilities") else {
        return Err(reject(
            REASON_SHAPE,
            "profile: enabled_capabilities is not an array",
        ));
    };
    if enabled.kind != Kind::Array {
        return Err(reject(
            REASON_SHAPE,
            "profile: enabled_capabilities is not an array",
        ));
    }
    for c in &enabled.elems {
        if c.kind != Kind::String || !compiled.contains(&c.str_val) {
            return Err(reject(
                REASON_CAPABILITY_NOT_COMPILED,
                "profile: an enabled capability is not compiled",
            ));
        }
    }
    let strata = string_set(build.get("compiled_strata"))?;
    let st = str_of(runtime, "stratum")?;
    if !strata.contains(st) {
        return Err(reject(
            REASON_STRATUM_NOT_COMPILED,
            format!("profile: runtime stratum {st:?} is not compiled"),
        ));
    }
    Ok(())
}

fn string_set(n: Option<&Node>) -> Result<HashSet<String>, Reject> {
    let Some(n) = n else {
        return Err(reject(
            REASON_SHAPE,
            "profile: expected an array of strings",
        ));
    };
    if n.kind != Kind::Array {
        return Err(reject(
            REASON_SHAPE,
            "profile: expected an array of strings",
        ));
    }
    let mut out = HashSet::with_capacity(n.elems.len());
    for e in &n.elems {
        if e.kind != Kind::String {
            return Err(reject(
                REASON_SHAPE,
                "profile: expected an array of strings",
            ));
        }
        out.insert(e.str_val.clone());
    }
    Ok(out)
}

#[derive(Debug, Clone)]
struct KeyIdentity {
    kind: &'static str,
    #[allow(dead_code)]
    value: String,
}

#[derive(Debug, Clone)]
struct LiveEntry {
    id: KeyIdentity,
    at: Instant,
    record_id: String,
    receipt: Node,
}

/// A live idempotency key: its operation kind, identity value and stored
/// receipt.
#[derive(Debug, Clone)]
pub(crate) struct LiveKey {
    pub kind: &'static str,
    pub value: String,
    pub receipt: Node,
}

#[derive(Debug, Clone)]
struct ImportItem {
    seq: i64,
    pin: Pin,
}

#[derive(Debug, Clone, Default)]
struct BodyState {
    bound: i64,
    phase: &'static str,
}

/// Source chain input for continuation streams (`SourceInput` in Go).
#[derive(Debug, Clone, Default)]
pub(crate) struct SourceInput {
    pub source_journal: Option<Node>,
    pub raw_source_journal: Option<Node>,
    pub ancestors: Option<Node>,
    pub raw_ancestors: Option<Node>,
}

/// Replays one v1 stream entry by entry in the reference interpreter's order.
#[derive(Debug, Clone)]
pub(crate) struct State {
    window: i64,
    journal: Vec<Node>,
    stream: String,
    failure_model: String,
    records: HashMap<Pin, Node>,
    holds: HashMap<i64, Node>,
    active: HashSet<i64>,
    keys: HashMap<String, LiveEntry>,
    retired: HashSet<String>,
    restricted_at: Vec<i64>,
    source: Vec<Node>,
    continues: Option<Node>,
    imports: Vec<ImportItem>,
    review: Option<Node>,
    upstream: Option<Box<State>>,
    upstream_unobserved: bool,
    removal_stream: String,
    has_removal: bool,
    incarnation_allocation: String,
    quarantined_designated: Vec<String>,
    referenced_bodies: HashSet<BodyKey>,
    bodies: HashMap<BodyKey, BodyState>,
    open_starts: HashMap<BodyKey, HashSet<i64>>,
    partial: bool,
}

fn is_integer_token(token: &str) -> bool {
    let rest = token.strip_prefix('-').unwrap_or(token);
    if rest.is_empty() {
        return false;
    }
    if rest == "0" {
        return true;
    }
    !rest.starts_with('0') && rest.bytes().all(|b| b.is_ascii_digit())
}

fn is_lifecycle_type(typ: &str) -> bool {
    matches!(
        typ,
        "body_published" | "body_removal_started" | "body_removed"
    )
}

impl State {
    pub(crate) fn new(window: &Node) -> Result<Self, Reject> {
        if window.kind != Kind::Number
            || !is_integer_token(&window.token)
            || window.token.starts_with('-')
            || window.token == "0"
        {
            return Err(reject(
                REASON_IDEMPOTENCY_WINDOW,
                "the idempotency window is not a positive integer",
            ));
        }
        let v = wire::safe_int(&window.token).map_err(|_| {
            reject(
                REASON_NUMERIC_DOMAIN,
                format!("the idempotency window exceeds {MAX_SAFE_INTEGER}"),
            )
        })?;
        if v <= 0 || v > MAX_SAFE_INTEGER {
            return Err(reject(
                REASON_NUMERIC_DOMAIN,
                format!("the idempotency window exceeds {MAX_SAFE_INTEGER}"),
            ));
        }
        Ok(Self {
            window: v,
            journal: Vec::new(),
            stream: String::new(),
            failure_model: String::new(),
            records: HashMap::new(),
            holds: HashMap::new(),
            active: HashSet::new(),
            keys: HashMap::new(),
            retired: HashSet::new(),
            restricted_at: Vec::new(),
            source: Vec::new(),
            continues: None,
            imports: Vec::new(),
            review: None,
            upstream: None,
            upstream_unobserved: false,
            removal_stream: String::new(),
            has_removal: false,
            incarnation_allocation: String::new(),
            quarantined_designated: Vec::new(),
            referenced_bodies: HashSet::new(),
            bodies: HashMap::new(),
            open_starts: HashMap::new(),
            partial: false,
        })
    }

    pub(crate) fn set_removal_stream(&mut self, name: impl Into<String>) {
        self.removal_stream = name.into();
        self.has_removal = true;
    }

    pub(crate) fn set_profile(&mut self, prof: &Node) {
        if prof.kind != Kind::Object {
            return;
        }
        let Some(rt) = prof.get("runtime") else {
            return;
        };
        if rt.kind != Kind::Object {
            return;
        }
        if let Some(rs) = rt.get("body_removal_stream") {
            if rs.kind == Kind::String {
                self.set_removal_stream(rs.str_val.clone());
            }
        }
        if let Some(ia) = rt.get("incarnation_allocation") {
            if ia.kind == Kind::String {
                self.incarnation_allocation = ia.str_val.clone();
            }
        }
        if let Some(qds) = rt.get("quarantined_designated_streams") {
            if qds.kind == Kind::Array {
                self.quarantined_designated.clear();
                for el in &qds.elems {
                    if el.kind == Kind::String {
                        self.quarantined_designated.push(el.str_val.clone());
                    }
                }
            }
        }
    }

    /// Reports whether an ingested entry needed semantics this replay does
    /// not implement, so a later refusal could have been missed.
    #[cfg_attr(not(test), allow(dead_code))]
    pub(crate) fn partial(&self) -> bool {
        self.partial
    }

    /// Sequence numbers of active protection holds, ascending.
    #[cfg_attr(not(test), allow(dead_code))]
    pub(crate) fn active_holds(&self) -> Vec<i64> {
        let mut out: Vec<i64> = self.active.iter().copied().collect();
        out.sort_unstable();
        out
    }

    pub(crate) fn len(&self) -> usize {
        self.journal.len()
    }

    pub(crate) fn journal(&self) -> &[Node] {
        &self.journal
    }

    /// The live key's operation kind, identity value, record id and stored
    /// receipt, when `key` is live in the idempotency window at `at`.
    pub(crate) fn live_key(&self, key: &str, at: Instant) -> Option<LiveKey> {
        self.live(key, at).map(|p| LiveKey {
            kind: p.id.kind,
            value: p.id.value.clone(),
            receipt: p.receipt.clone(),
        })
    }

    /// The highest accepted revision of `record_id` on the stream.
    pub(crate) fn head_pin(&self, record_id: &str) -> Option<Pin> {
        self.records
            .keys()
            .filter(|p| p.record_id == record_id)
            .max_by_key(|p| p.revision)
            .cloned()
    }

    /// Accepted record revisions, keyed by pin.
    pub(crate) fn records(&self) -> &HashMap<Pin, Node> {
        &self.records
    }

    /// Whether `record_id` has been retired on this stream.
    pub(crate) fn is_retired(&self, record_id: &str) -> bool {
        self.retired.contains(record_id)
    }

    pub(crate) fn stream(&self) -> &str {
        &self.stream
    }

    #[cfg_attr(not(test), allow(dead_code))]
    fn last_at(&self) -> Instant {
        self.journal
            .last()
            .and_then(|e| e.get("at"))
            .and_then(|a| parse_instant(&a.str_val).ok())
            .unwrap_or_else(|| Instant::from_unix_micros(i64::MIN / 2).expect("floor instant"))
    }

    #[cfg_attr(not(test), allow(dead_code))]
    fn next_seq(&self, op: &str) -> Result<i64, Reject> {
        let seq = i64::try_from(self.journal.len())
            .unwrap_or(i64::MAX)
            .saturating_add(1);
        if seq > wire::MAX_SAFE_INTEGER {
            return Err(reject(
                wire::REASON_NUMERIC_DOMAIN,
                format!("{op}: seq exceeds {}", wire::MAX_SAFE_INTEGER),
            ));
        }
        Ok(seq)
    }

    fn protection_receipt(&self, r: &Node, seq: i64) -> Result<Node, Reject> {
        if r.kind != Kind::Object {
            return Err(reject(REASON_SHAPE, "protection request is not an object"));
        }
        let stream = str_of(r, "stream")?;
        let key = str_of(r, "idempotency_key")?;
        let action = str_of(r, "action")?;
        let outcome = if action == "tombstone" {
            "tombstoned"
        } else {
            "untombstoned"
        };
        let rdig = record_digest(r)?;
        Ok(Node::object(vec![
            member("v", Node::int(1)),
            member("stream", str_node(stream)),
            member("idempotency_key", str_node(key)),
            member("outcome", str_node(outcome)),
            member("seq", Node::int(seq)),
            member("request_digest", str_node(&rdig)),
            member("failure_model", str_node(&self.failure_model)),
        ]))
    }

    fn retirement_receipt(&self, r: &Node, seq: i64) -> Result<Node, Reject> {
        if r.kind != Kind::Object {
            return Err(reject(REASON_SHAPE, "retirement request is not an object"));
        }
        let stream = str_of(r, "stream")?;
        let key = str_of(r, "idempotency_key")?;
        let rid = str_of(r, "record_id")?;
        let rdig = record_digest(r)?;
        Ok(Node::object(vec![
            member("v", Node::int(1)),
            member("kind", str_node("receipt")),
            member("stream", str_node(stream)),
            member("idempotency_key", str_node(key)),
            member("outcome", str_node("retired")),
            member("seq", Node::int(seq)),
            member("record_id", str_node(rid)),
            member("request_digest", str_node(&rdig)),
            member("failure_model", str_node(&self.failure_model)),
        ]))
    }

    /// Evaluates protection request `r` at decision time `at_str`, appending
    /// a tombstoned or untombstoned entry when accepted and returning the
    /// protection receipt. Live-key replay precedes target and state checks.
    #[cfg_attr(not(test), allow(dead_code))]
    pub(crate) fn request(&mut self, r: &Node, at_str: &str) -> Result<Node, Reject> {
        if r.kind != Kind::Object {
            return Err(reject(REASON_SHAPE, "request: not an object"));
        }
        check_journal(&self.journal)?;
        let at = instant_of(at_str, "request at")?;
        if at < self.last_at() {
            return Err(reject(
                REASON_JOURNAL_TIME,
                "request: at precedes the journal",
            ));
        }
        let obs = str_of(r, "observed_at")?;
        instant_of(obs, "request.observed_at")?;
        let r_stream = str_of(r, "stream")?.to_owned();
        let key = str_of(r, "idempotency_key")?.to_owned();
        let rdig = record_digest(r)?;
        let mut reason = "";
        if r_stream == self.stream {
            if let Some(prior) = self.live(&key, at) {
                if prior.id.kind == "protection" && prior.id.value == rdig {
                    return Ok(prior.receipt.clone());
                }
                reason = REASON_IDEMPOTENCY_CONFLICT;
            }
        }
        if reason.is_empty() {
            reason = self.refusal(r, at)?;
        }
        if !reason.is_empty() {
            return Ok(Node::object(vec![
                member("v", Node::int(1)),
                member("stream", str_node(&r_stream)),
                member("idempotency_key", str_node(&key)),
                member("outcome", str_node(reason)),
            ]));
        }
        let seq = self.next_seq("request")?;
        let rcpt = self.protection_receipt(r, seq)?;
        let outcome = str_of(&rcpt, "outcome")?.to_owned();
        let entry = Node::object(vec![
            member("v", Node::int(1)),
            member("seq", Node::int(seq)),
            member("group", Node::int(seq)),
            member("group_size", Node::int(1)),
            member("stream", str_node(&self.stream.clone())),
            member("type", str_node(&outcome)),
            member("at", str_node(at_str)),
            member("request", r.clone()),
            member("request_digest", str_node(&rdig)),
        ]);
        self.ingest(entry)?;
        Ok(rcpt)
    }

    /// Evaluates keyed retirement request `r` at decision time `at_str`,
    /// appending a retired entry when accepted and returning the receipt.
    #[cfg_attr(not(test), allow(dead_code))]
    pub(crate) fn retire(&mut self, r: &Node, at_str: &str) -> Result<Node, Reject> {
        if r.kind != Kind::Object {
            return Err(reject(REASON_SHAPE, "retire: not an object"));
        }
        check_journal(&self.journal)?;
        let at = instant_of(at_str, "retire at")?;
        if at < self.last_at() {
            return Err(reject(
                REASON_JOURNAL_TIME,
                "retire: at precedes the journal",
            ));
        }
        let r_stream = str_of(r, "stream")?.to_owned();
        let key = str_of(r, "idempotency_key")?.to_owned();
        let rid = str_of(r, "record_id")?.to_owned();
        let retire_reason = str_of(r, "reason")?.to_owned();
        let rdig = record_digest(r)?;
        let mut reason = "";
        if r_stream == self.stream {
            if let Some(prior) = self.live(&key, at) {
                if prior.id.kind == "retirement" && prior.id.value == rdig {
                    return Ok(prior.receipt.clone());
                }
                reason = REASON_IDEMPOTENCY_CONFLICT;
            }
        }
        if reason.is_empty() && (r_stream != self.stream || !self.has_record_id(&rid)) {
            reason = REASON_TARGET_NOT_FOUND;
        }
        if !reason.is_empty() {
            return Ok(Node::object(vec![
                member("v", Node::int(1)),
                member("kind", str_node("receipt")),
                member("stream", str_node(&r_stream)),
                member("idempotency_key", str_node(&key)),
                member("outcome", str_node(reason)),
            ]));
        }
        let seq = self.next_seq("retire")?;
        let entry = Node::object(vec![
            member("v", Node::int(1)),
            member("seq", Node::int(seq)),
            member("group", Node::int(seq)),
            member("group_size", Node::int(1)),
            member("stream", str_node(&self.stream.clone())),
            member("type", str_node("retired")),
            member("at", str_node(at_str)),
            member("record_id", str_node(&rid)),
            member("reason", str_node(&retire_reason)),
            member("idempotency_key", str_node(&key)),
            member("request_digest", str_node(&rdig)),
        ]);
        self.ingest(entry)?;
        self.retirement_receipt(r, seq)
    }

    /// Looks up `key` at decision time `at_str` for an attempt at
    /// `attempted_at_str`, returning the reconciliation document.
    #[cfg_attr(not(test), allow(dead_code))]
    pub(crate) fn reconcile(
        &self,
        key: &str,
        at_str: &str,
        attempted_at_str: &str,
    ) -> Result<Node, Reject> {
        check_journal(&self.journal)?;
        instant_of(attempted_at_str, "reconcile attempted_at")?;
        let at = instant_of(at_str, "reconcile at")?;
        if at < self.last_at() {
            return Err(reject(
                REASON_JOURNAL_TIME,
                "reconcile: at precedes the journal",
            ));
        }
        let mut fields = vec![
            member("v", Node::int(1)),
            member("kind", str_node("reconciliation")),
            member("stream", str_node(&self.stream)),
            member("idempotency_key", str_node(key)),
            member("attempted_at", str_node(attempted_at_str)),
        ];
        if let Some(prior) = self.live(key, at) {
            fields.push(member("result", str_node("found")));
            fields.push(member("receipt", prior.receipt.clone()));
        } else {
            fields.push(member("result", str_node("unknown")));
            fields.push(member("reason", str_node("coverage_unprovable")));
        }
        Ok(Node::object(fields))
    }

    /// Checks a received receipt or reconciliation against the journal.
    #[cfg_attr(not(test), allow(dead_code))]
    pub(crate) fn verify_receipt(&self, received: &Node) -> Result<Node, Reject> {
        if received.kind != Kind::Object {
            return Err(reject(REASON_SHAPE, "receipt: not an object"));
        }
        if received
            .get("kind")
            .is_some_and(|k| k.kind == Kind::String && k.str_val == "reconciliation")
        {
            let att = str_of(received, "attempted_at")?;
            instant_of(att, "reconciliation attempted_at")?;
            for f in ["window_covers_from", "window_covers_until"] {
                if present(received, f) {
                    let v = str_of(received, f)?;
                    instant_of(v, &format!("reconciliation {f}"))?;
                }
            }
            return Ok(verified_outcome());
        }
        let outcome = str_of(received, "outcome")?;
        if !matches!(outcome, "tombstoned" | "untombstoned" | "retired") {
            return Err(reject(
                REASON_RECEIPT_MISMATCH,
                format!("receipt: outcome {outcome:?} is not an accepted protection or retirement receipt"),
            ));
        }
        if int_of(received, "v").ok() != Some(1) {
            return Err(reject(REASON_RECEIPT_MISMATCH, "receipt: v is not 1"));
        }
        let seq = int_of(received, "seq")?;
        let n = i64::try_from(self.journal.len()).unwrap_or(i64::MAX);
        if seq < 1 || seq > n {
            return Err(reject(
                REASON_RECEIPT_MISMATCH,
                format!("receipt: seq {seq} is out of range"),
            ));
        }
        let entry = &self.journal[(seq - 1) as usize];
        let etyp = str_of(entry, "type")?;
        let expected = if outcome == "retired" {
            if etyp != "retired" || !present(entry, "idempotency_key") {
                return Err(reject(
                    REASON_RECEIPT_MISMATCH,
                    format!("receipt: seq {seq} is not a keyed retirement"),
                ));
            }
            self.retirement_receipt(&retirement_request(entry), seq)?
        } else {
            if etyp != "tombstoned" && etyp != "untombstoned" {
                return Err(reject(
                    REASON_RECEIPT_MISMATCH,
                    format!("receipt: seq {seq} is not a protection entry"),
                ));
            }
            let Some(req) = entry.get("request") else {
                return Err(reject(REASON_SHAPE, "protection request is not an object"));
            };
            self.protection_receipt(req, seq)?
        };
        if !equal_nodes(Some(received), Some(&expected)) {
            return Err(reject(
                REASON_RECEIPT_MISMATCH,
                format!("receipt: does not match journal entry {seq}"),
            ));
        }
        Ok(verified_outcome())
    }

    /// Whether a use request must wait for a restriction review: on a plain
    /// stream, when it names a source state; on a continuation, when the
    /// upstream is unobserved or the request's source state digest differs
    /// from the source's effective restriction manifest.
    fn review_pending_for(&self, r: &Node) -> Result<bool, Reject> {
        if self.continues.is_none() {
            return Ok(present(r, "source_state_digest"));
        }
        if self.upstream_unobserved {
            return Ok(true);
        }
        let sm_dig = source_manifest(&self.source)?;
        if !present(r, "source_state_digest") {
            return Ok(true);
        }
        Ok(str_of(r, "source_state_digest").map_or(true, |d| d != sm_dig))
    }

    /// Evaluates an export, serve or reuse request against the replayed
    /// journal and source chain.
    #[cfg_attr(not(test), allow(dead_code))]
    pub(crate) fn use_request(&self, r: &Node) -> Result<Node, Reject> {
        if r.kind != Kind::Object {
            return Err(reject(REASON_SHAPE, "use: request is not an object"));
        }
        check_journal(&self.journal)?;
        let refused = |reason: &str| {
            Ok(Node::object(vec![
                member("outcome", str_node("refused")),
                member("reason", str_node(reason)),
            ]))
        };
        let purpose = str_of(r, "purpose")?;
        if !matches!(purpose, "serve" | "export" | "reuse") {
            return refused(REASON_INVALID_PURPOSE);
        }
        let r_stream = str_of(r, "stream")?;
        let prot_state = str_of(r, "protection_state")?;
        let prot_cut = int_of(r, "protection_cut")?;
        let cur_cut = int_of(r, "current_cut")?;
        let rev_pend = self.review_pending_for(r)?;
        let n = i64::try_from(self.journal.len()).unwrap_or(i64::MAX);
        if r_stream != self.stream
            || prot_state != "verified"
            || prot_cut != n
            || cur_cut != n
            || rev_pend
        {
            return refused(REASON_PROTECTION_UNAVAILABLE);
        }
        if !self.review_current()? {
            return refused(REASON_RESTRICTIONS_PENDING);
        }
        let (Some(sel_node), Some(om_node)) = (r.get("selection"), r.get("omitted")) else {
            return Err(reject(
                REASON_SHAPE,
                "use: selection or omitted is not an array",
            ));
        };
        if sel_node.kind != Kind::Array || om_node.kind != Kind::Array {
            return Err(reject(
                REASON_SHAPE,
                "use: selection or omitted is not an array",
            ));
        }
        let mut selected = HashSet::new();
        let mut selected_pins = Vec::new();
        for el in &sel_node.elems {
            let p = pin_from_node(Some(el), "use.selection")?;
            selected.insert(p.clone());
            selected_pins.push(p);
        }
        let mut omitted = HashSet::new();
        for el in &om_node.elems {
            omitted.insert(pin_from_node(Some(el), "use.omitted")?);
        }
        if selected
            .iter()
            .chain(omitted.iter())
            .any(|p| !self.records.contains_key(p))
        {
            return refused(REASON_TARGET_NOT_FOUND);
        }
        if selected.iter().any(|p| omitted.contains(p)) {
            return refused(REASON_EXPORT_SELECTION);
        }
        if selected.len() + omitted.len() != self.records.len() {
            return refused(REASON_EXPORT_SELECTION);
        }
        match r.get("inherited_block") {
            Some(b) if b.kind == Kind::Bool => {
                if b.bool_val {
                    return refused(REASON_PROTECTED_USE);
                }
            }
            _ => {
                return Err(reject(
                    REASON_SHAPE,
                    "use: inherited_block is not a boolean",
                ))
            }
        }
        for p in &selected_pins {
            if self.blocked(p)? {
                return refused(REASON_PROTECTED_USE);
            }
        }
        if purpose == "serve"
            && selected_pins
                .iter()
                .any(|p| self.retired.contains(&p.record_id))
        {
            return refused(REASON_PROTECTED_USE);
        }
        let Some(bodies) = r.get("bodies").filter(|b| b.kind == Kind::Array) else {
            return Err(reject(REASON_SHAPE, "use: bodies is not an array"));
        };
        let mut supplied: HashMap<String, i64> = HashMap::new();
        for b in &bodies.elems {
            if b.kind != Kind::Object {
                return Err(reject(REASON_SHAPE, "use: body item is not an object"));
            }
            let hex_str = str_of(b, "hex")?;
            let dig = str_of(b, "digest")?;
            let Some(raw) = decode_hex(hex_str) else {
                return refused(REASON_BODY_CLOSURE);
            };
            if sha256_prefixed(&raw) != dig || supplied.contains_key(dig) {
                return refused(REASON_BODY_CLOSURE);
            }
            supplied.insert(dig.to_owned(), i64::try_from(raw.len()).unwrap_or(i64::MAX));
        }
        let mut needed: HashMap<String, i64> = HashMap::new();
        for p in &selected {
            let Some(body) = self.records[p]
                .get("body")
                .filter(|b| b.kind == Kind::Object)
            else {
                continue;
            };
            let b_dig = str_of(body, "digest")?.to_owned();
            let b_len = int_of(body, "length")?;
            if needed.get(&b_dig).is_some_and(|prev| *prev != b_len) {
                return refused(REASON_BODY_CLOSURE);
            }
            needed.insert(b_dig, b_len);
        }
        if supplied.iter().any(|(d, l)| needed.get(d) != Some(l)) {
            return refused(REASON_BODY_CLOSURE);
        }
        let closure = str_of(r, "closure")?;
        if closure == "full_content" && supplied.len() != needed.len() {
            return refused(REASON_BODY_CLOSURE);
        }
        Ok(Node::object(vec![
            member("outcome", str_node("allowed")),
            member("closure", str_node(closure)),
            member("selection", sel_node.clone()),
            member("omitted", om_node.clone()),
            member("protection_cut", Node::int(prot_cut)),
        ]))
    }

    fn has_record_id(&self, record_id: &str) -> bool {
        self.records.keys().any(|p| p.record_id == record_id)
    }

    fn remember(
        &mut self,
        key: String,
        id: KeyIdentity,
        at: Instant,
        record_id: String,
        receipt: Node,
    ) {
        self.keys.insert(
            key,
            LiveEntry {
                id,
                at,
                record_id,
                receipt,
            },
        );
    }

    fn live(&self, key: &str, at: Instant) -> Option<&LiveEntry> {
        let prior = self.keys.get(key)?;
        let delta = at.as_unix_micros() - prior.at.as_unix_micros();
        if delta >= 0 && delta < self.window * 1000 {
            Some(prior)
        } else {
            None
        }
    }

    fn refusal(&self, r: &Node, at: Instant) -> Result<&'static str, Reject> {
        if r.kind != Kind::Object {
            return Err(reject(REASON_SHAPE, "protection request is not an object"));
        }
        let r_stream = str_of(r, "stream")?;
        let target_pin = pin_from_node(r.get("target"), "request.target")?;
        if r_stream != self.stream || target_pin.stream != self.stream {
            return Ok(REASON_TARGET_NOT_FOUND);
        }
        let Some(record) = self.records.get(&target_pin) else {
            return Ok(REASON_TARGET_NOT_FOUND);
        };
        let scope = str_of(r, "scope")?;
        if scope == "body" {
            let matched = body_matches(record.get("body"), r.get("body"))?;
            if !matched {
                return Ok(REASON_BODY_MISMATCH);
            }
        }
        let obs_str = str_of(r, "observed_at")?;
        let obs_at = instant_of(obs_str, "request.observed_at")?;
        if obs_at > at {
            return Ok(REASON_OBSERVATION_ORDER);
        }
        let action = str_of(r, "action")?;
        if action == "restore" {
            let restores = int_of(r, "restores")?;
            if !self.active.contains(&restores) {
                return Ok(REASON_HOLD_NOT_ACTIVE);
            }
            let Some(hold) = self.holds.get(&restores) else {
                return Ok(REASON_HOLD_NOT_ACTIVE);
            };
            if !equal_nodes(r.get("target"), hold.get("target"))
                || !equal_nodes(r.get("scope"), hold.get("scope"))
                || !equal_nodes(r.get("body"), hold.get("body"))
            {
                return Ok(REASON_HOLD_MISMATCH);
            }
            let hold_obs_str = str_of(hold, "observed_at")?;
            let hold_obs_at = instant_of(hold_obs_str, "hold.observed_at")?;
            if obs_at < hold_obs_at {
                return Ok(REASON_OBSERVATION_ORDER);
            }
        }
        Ok("")
    }

    fn apply(&mut self, r: &Node, seq: i64) -> Result<(), Reject> {
        let action = str_of(r, "action")?;
        if action == "tombstone" {
            self.holds.insert(seq, r.clone());
            self.active.insert(seq);
            return Ok(());
        }
        let restores = int_of(r, "restores")?;
        self.active.remove(&restores);
        Ok(())
    }

    pub(crate) fn blocked(&self, p: &Pin) -> Result<bool, Reject> {
        let Some(record) = self.records.get(p) else {
            return Ok(false);
        };
        for seq in &self.active {
            let Some(h) = self.holds.get(seq) else {
                continue;
            };
            let scope = str_of(h, "scope")?;
            let target = pin_from_node(h.get("target"), "hold.target")?;
            match scope {
                "record" => {
                    if p.record_id == target.record_id {
                        return Ok(true);
                    }
                }
                "revision" => {
                    if p == &target {
                        return Ok(true);
                    }
                }
                "body" => {
                    if body_matches(record.get("body"), h.get("body"))? {
                        return Ok(true);
                    }
                }
                _ => {}
            }
        }
        Ok(false)
    }

    pub(crate) fn ingest(&mut self, e: Node) -> Result<(), Reject> {
        let where_ctx = match int_of(&e, "seq") {
            Ok(seq) => format!("entry {seq}"),
            Err(_) => "entry".to_owned(),
        };
        let end = closing_seq(&e, &where_ctx)?;
        let v = int_of(&e, "v").map_err(|_| {
            reject(
                REASON_JOURNAL_VERSION,
                format!("{where_ctx}: not a v1 journal entry"),
            )
        })?;
        if v != 1 {
            return Err(reject(
                REASON_JOURNAL_VERSION,
                format!("{where_ctx}: not a v1 journal entry"),
            ));
        }
        let at_str = str_of(&e, "at")?;
        let at = instant_of(at_str, &format!("{where_ctx} at"))?;
        if let Some(rec) = e.get("record") {
            let rv = int_of(rec, "v").map_err(|_| {
                reject(
                    REASON_RECORD_VERSION,
                    format!("{where_ctx}: the record is not a v1 record"),
                )
            })?;
            if rv != 1 {
                return Err(reject(
                    REASON_RECORD_VERSION,
                    format!("{where_ctx}: the record is not a v1 record"),
                ));
            }
            if let Some(pa) = rec.get("produced_at") {
                let ps = str_of(pa, "instant")?;
                instant_of(ps, &format!("{where_ctx} record.produced_at.instant"))?;
            }
        }
        if let Some(req) = e.get("request") {
            let obs = str_of(req, "observed_at")?;
            instant_of(obs, &format!("{where_ctx} request.observed_at"))?;
        }
        let seq = int_of(&e, "seq")?;
        if seq == end {
            let mut next = Vec::with_capacity(self.journal.len() + 1);
            next.extend_from_slice(&self.journal);
            next.push(e.clone());
            check_journal(&next)?;
        }
        if let Some(last) = self.journal.last() {
            if let Some(prev_at_node) = last.get("at") {
                if let Ok(prev) = parse_instant(&prev_at_node.str_val) {
                    if at < prev {
                        return Err(reject(
                            REASON_JOURNAL_TIME,
                            format!("{where_ctx}: at is earlier than the previous entry"),
                        ));
                    }
                }
            }
        }
        let typ = str_of(&e, "type")?.to_owned();
        if self.has_removal || is_lifecycle_type(&typ) {
            let stream = str_of(&e, "stream")?;
            if self.has_removal && stream == self.removal_stream {
                if !(is_lifecycle_type(&typ) || typ == "stream_opened")
                    || e.get("continues").is_some()
                {
                    return Err(reject(
                        REASON_BODY_REMOVAL_STREAM,
                        format!(
                            "{where_ctx}: only stream_opened and body lifecycle entries belong on the body-removal stream"
                        ),
                    ));
                }
            } else if is_lifecycle_type(&typ) {
                return Err(reject(
                    REASON_BODY_REMOVAL_STREAM,
                    format!("{where_ctx}: a body lifecycle entry outside the body-removal stream"),
                ));
            }
        }

        if is_lifecycle_type(&typ) {
            self.track_body(&e, &where_ctx)?;
        } else {
            match typ.as_str() {
                "stream_opened" => {
                    let stream = str_of(&e, "stream")?.to_owned();
                    let fm = str_of(&e, "failure_model")?.to_owned();
                    self.stream = stream;
                    self.failure_model = fm;
                    if let Some(cont) = e.get("continues") {
                        self.open_continuation(cont)?;
                    }
                }
                "appended" | "revised" => {
                    let p = pin_of(&e, &where_ctx)?;
                    if self.retired.contains(&p.record_id) {
                        return Err(reject(
                            REASON_RETIRED_RECORD,
                            format!("{where_ctx}: record {:?} is retired", p.record_id),
                        ));
                    }
                    if present(&e, "imported_from") {
                        self.import_record(&e, seq)?;
                    }
                    let key = str_of(&e, "idempotency_key")?.to_owned();
                    if self.live(&key, at).is_some() {
                        return Err(reject(
                            REASON_ACCEPTED_KEY_REUSE,
                            format!("{where_ctx}: idempotency key {key:?} is still live"),
                        ));
                    }
                    let body_status = if present(&e, "body_status") {
                        str_of(&e, "body_status")?.to_owned()
                    } else {
                        String::new()
                    };
                    if let Some(rec) = e.get("record") {
                        self.records.insert(p.clone(), rec.clone());
                    }
                    let rcpt = content_receipt(
                        &self.stream,
                        &key,
                        &typ,
                        &p,
                        seq,
                        &self.failure_model,
                        &body_status,
                    );
                    self.remember(
                        key,
                        KeyIdentity {
                            kind: "content",
                            value: p.digest,
                        },
                        at,
                        p.record_id,
                        rcpt,
                    );
                }
                "deduplicated" => {
                    let key = str_of(&e, "idempotency_key")?;
                    let rid = str_of(&e, "record_id")?;
                    let Some(prior) = self.live(key, at) else {
                        return Err(reject(
                            REASON_INVALID_DEDUPLICATION,
                            format!(
                                "{where_ctx}: key {key:?} does not name a live content write of record {rid:?}"
                            ),
                        ));
                    };
                    if prior.id.kind != "content" || prior.record_id != rid {
                        return Err(reject(
                            REASON_INVALID_DEDUPLICATION,
                            format!(
                                "{where_ctx}: key {key:?} does not name a live content write of record {rid:?}"
                            ),
                        ));
                    }
                }
                "tombstoned" | "untombstoned" => {
                    let Some(req) = e.get("request") else {
                        return Err(reject(
                            REASON_SHAPE,
                            format!("{where_ctx}: request is not an object"),
                        ));
                    };
                    if req.kind != Kind::Object {
                        return Err(reject(
                            REASON_SHAPE,
                            format!("{where_ctx}: request is not an object"),
                        ));
                    }
                    let want_dig = str_of(&e, "request_digest")?;
                    let got_dig = record_digest(req)?;
                    if want_dig != got_dig {
                        return Err(reject(
                            REASON_REQUEST_DIGEST,
                            format!("{where_ctx}: request_digest mismatch"),
                        ));
                    }
                    let action = str_of(req, "action")?;
                    let want_typ = if action == "tombstone" {
                        "tombstoned"
                    } else {
                        "untombstoned"
                    };
                    if typ != want_typ {
                        return Err(reject(
                            REASON_OPERATION_TYPE,
                            format!(
                                "{where_ctx}: entry type {typ:?} does not match request action {action:?}"
                            ),
                        ));
                    }
                    let key = str_of(req, "idempotency_key")?.to_owned();
                    if self.live(&key, at).is_some() {
                        return Err(reject(
                            REASON_ACCEPTED_KEY_REUSE,
                            format!("{where_ctx}: idempotency key {key:?} is still live"),
                        ));
                    }
                    let reason = self.refusal(req, at)?;
                    if !reason.is_empty() {
                        return Err(reject(
                            reason,
                            format!("{where_ctx}: protection journal operation refused"),
                        ));
                    }
                    let rcpt = self.protection_receipt(req, seq)?;
                    self.apply(req, seq)?;
                    self.remember(
                        key,
                        KeyIdentity {
                            kind: "protection",
                            value: got_dig,
                        },
                        at,
                        String::new(),
                        rcpt,
                    );
                    self.restricted_at.push(seq);
                }
                "retired" => {
                    let rid = str_of(&e, "record_id")?.to_owned();
                    if !self.has_record_id(&rid) {
                        return Err(reject(
                            REASON_TARGET_NOT_FOUND,
                            format!("{where_ctx}: record {rid:?} not found"),
                        ));
                    }
                    if present(&e, "idempotency_key") {
                        let key = str_of(&e, "idempotency_key")?.to_owned();
                        let r = retirement_request(&e);
                        let want_dig = str_of(&e, "request_digest")?;
                        let got_dig = record_digest(&r)?;
                        if want_dig != got_dig {
                            return Err(reject(
                                REASON_REQUEST_DIGEST,
                                format!("{where_ctx}: request_digest mismatch"),
                            ));
                        }
                        if self.live(&key, at).is_some() {
                            return Err(reject(
                                REASON_ACCEPTED_KEY_REUSE,
                                format!("{where_ctx}: idempotency key {key:?} is still live"),
                            ));
                        }
                        let rcpt = self.retirement_receipt(&r, seq)?;
                        self.remember(
                            key,
                            KeyIdentity {
                                kind: "retirement",
                                value: got_dig,
                            },
                            at,
                            rid.clone(),
                            rcpt,
                        );
                    }
                    self.retired.insert(rid);
                    self.restricted_at.push(seq);
                }
                "restrictions_reviewed" => {
                    self.check_review(&e, seq)?;
                }
                "conflicted" => {}
                _ => self.partial = true,
            }
        }
        self.journal.push(e);
        Ok(())
    }

    fn track_body(&mut self, e: &Node, where_ctx: &str) -> Result<(), Reject> {
        let key = body_key_of(e.get("body"), where_ctx)?;
        let n = int_of(e, "incarnation")?;
        let typ = str_of(e, "type")?;
        let cur = self.bodies.get(&key).cloned().unwrap_or_default();
        match typ {
            "body_published" => {
                if n <= cur.bound {
                    return Err(reject(
                        REASON_BODY_LIFECYCLE,
                        format!(
                            "{where_ctx}: body_published incarnation {n} <= {}",
                            cur.bound
                        ),
                    ));
                }
                self.bodies.insert(
                    key,
                    BodyState {
                        bound: n,
                        phase: "published",
                    },
                );
            }
            "body_removal_started" => {
                if n != cur.bound || cur.phase != "published" {
                    return Err(reject(
                        REASON_BODY_LIFECYCLE,
                        format!(
                            "{where_ctx}: body_removal_started requires published incarnation {}",
                            cur.bound
                        ),
                    ));
                }
                self.bodies.insert(
                    key.clone(),
                    BodyState {
                        bound: n,
                        phase: "started",
                    },
                );
                self.open_starts.entry(key).or_default().insert(n);
            }
            _ => {
                if n != cur.bound || cur.phase != "started" {
                    return Err(reject(
                        REASON_BODY_LIFECYCLE,
                        format!(
                            "{where_ctx}: body_removed requires started incarnation {}",
                            cur.bound
                        ),
                    ));
                }
                self.bodies.insert(
                    key.clone(),
                    BodyState {
                        bound: n,
                        phase: "removed",
                    },
                );
                if let Some(starts) = self.open_starts.get_mut(&key) {
                    starts.remove(&n);
                }
            }
        }
        Ok(())
    }

    pub(crate) fn body_lifecycle(
        &self,
        key: &BodyKey,
        present: bool,
    ) -> (&'static str, i64, &'static str) {
        let bs = self.bodies.get(key).cloned().unwrap_or_default();
        let obs = if present { "present_valid" } else { "absent" };
        let st = compute_recovery_row(
            bs.phase,
            bs.bound,
            bs.bound,
            obs,
            false,
            self.referenced_bodies.contains(key),
        );
        (st, bs.bound, bs.phase)
    }

    pub(crate) fn body_held(&self, key: &BodyKey) -> bool {
        for h_seq in &self.active {
            let Some(h) = self.holds.get(h_seq) else {
                continue;
            };
            if h.kind != Kind::Object {
                continue;
            }
            if str_of(h, "scope") == Ok("body") && body_matches_target(h.get("body"), key) {
                return true;
            }
        }
        for (pk, rec) in &self.records {
            if body_matches_target(rec.get("body"), key) && self.blocked(pk).unwrap_or(true) {
                return true;
            }
        }
        false
    }

    pub(crate) fn body_referenced(&self, key: &BodyKey) -> bool {
        self.records
            .values()
            .any(|rec| body_matches_target(rec.get("body"), key))
    }

    pub(crate) fn review_pending(&self) -> bool {
        !self.review_current().unwrap_or(false)
    }

    fn review_current(&self) -> Result<bool, Reject> {
        if self.continues.is_none() {
            return Ok(true);
        }
        let Some(review) = &self.review else {
            return Ok(false);
        };
        let after = int_of(review, "seq")?;
        for imp in &self.imports {
            if imp.seq > after {
                return Ok(false);
            }
        }
        for &sq in &self.restricted_at {
            if sq > after {
                return Ok(false);
            }
        }
        let sm_dig = source_manifest(&self.source)?;
        let rev_src_dig = str_of(review, "source_state_digest")?;
        if sm_dig != rev_src_dig {
            return Ok(false);
        }
        if self.upstream_unobserved {
            return Ok(false);
        }
        let Some(upstream) = &self.upstream else {
            return Ok(true);
        };
        upstream.review_current()
    }

    fn source_cut(&self, cut: &Node) -> Result<Vec<Node>, Reject> {
        if self.source.is_empty() || cut.kind != Kind::Object {
            return Err(reject(
                REASON_CONTINUATION,
                "missing source journal or invalid cut",
            ));
        }
        let v0 = int_of(&self.source[0], "v")
            .map_err(|_| reject(REASON_CONTINUATION, "source[0].v invalid"))?;
        let Some(cap0) = capability_by_family(v0) else {
            return Err(reject(
                REASON_CONTINUATION,
                format!("unsupported source family {v0}"),
            ));
        };
        let cut_cap = str_of(cut, "capability")
            .map_err(|_| reject(REASON_CONTINUATION, "cut capability mismatch"))?;
        if cut_cap != cap0 {
            return Err(reject(REASON_CONTINUATION, "cut capability mismatch"));
        }
        for e in &self.source {
            let ev = int_of(e, "v")
                .map_err(|_| reject(REASON_CONTINUATION, "inconsistent source journal version"))?;
            if ev != v0 {
                return Err(reject(
                    REASON_CONTINUATION,
                    "inconsistent source journal version",
                ));
            }
        }
        let cut_stream =
            str_of(cut, "stream").map_err(|_| reject(REASON_CONTINUATION, "cut.stream invalid"))?;
        let src_stream = str_of(&self.source[0], "stream")
            .map_err(|_| reject(REASON_CONTINUATION, "cut stream mismatch"))?;
        if cut_stream != src_stream {
            return Err(reject(REASON_CONTINUATION, "cut stream mismatch"));
        }
        let cut_seq = int_of(cut, "cut_seq")
            .map_err(|_| reject(REASON_CONTINUATION, "cut_seq is not a complete cut"))?;
        if !complete_cut(&self.source, cut_seq) {
            return Err(reject(
                REASON_CONTINUATION,
                format!("cut_seq {cut_seq} is not a complete cut"),
            ));
        }
        let prefix = self.source[..(cut_seq as usize)].to_vec();
        let got_dig = slice_digest(&prefix)
            .map_err(|_| reject(REASON_CONTINUATION, "cut digest calculation failed"))?;
        let want_dig = str_of(cut, "cut_digest")
            .map_err(|_| reject(REASON_CONTINUATION, "cut_digest mismatch"))?;
        if got_dig != want_dig {
            return Err(reject(REASON_CONTINUATION, "cut_digest mismatch"));
        }
        Ok(prefix)
    }

    fn open_continuation(&mut self, cut: &Node) -> Result<(), Reject> {
        self.source_cut(cut)?;
        self.continues = Some(cut.clone());
        Ok(())
    }

    fn import_record(&mut self, e: &Node, seq: i64) -> Result<(), Reject> {
        let Some(continues) = &self.continues else {
            return Err(reject(
                REASON_CONTINUATION,
                "imported_from on non-continuation stream",
            ));
        };
        let Some(imp) = e.get("imported_from") else {
            return Err(reject(
                REASON_CONTINUATION,
                "imported_from is not an object",
            ));
        };
        if imp.kind != Kind::Object {
            return Err(reject(
                REASON_CONTINUATION,
                "imported_from is not an object",
            ));
        }
        let imp_cap = str_of(imp, "capability")
            .map_err(|_| reject(REASON_CONTINUATION, "imported_from.capability invalid"))?;
        let cont_cap = str_of(continues, "capability").unwrap_or("");
        let cont_stream = str_of(continues, "stream").unwrap_or("");
        let p = pin_from_node(imp.get("pin"), "imported_from.pin")
            .map_err(|_| reject(REASON_CONTINUATION, "imported_from.pin invalid"))?;
        if imp_cap != cont_cap || p.stream != cont_stream {
            return Err(reject(
                REASON_CONTINUATION,
                "imported_from does not match continues",
            ));
        }
        self.imports.push(ImportItem { seq, pin: p });
        Ok(())
    }

    fn check_review(&mut self, e: &Node, seq: i64) -> Result<(), Reject> {
        let Some(continues) = self.continues.clone() else {
            return Err(reject(
                REASON_CONTINUATION,
                "restrictions_reviewed on non-continuation stream",
            ));
        };
        let Some(src) = e.get("source") else {
            return Err(reject(
                REASON_CONTINUATION,
                "review.source is not an object",
            ));
        };
        if src.kind != Kind::Object {
            return Err(reject(
                REASON_CONTINUATION,
                "review.source is not an object",
            ));
        }
        let src_cap = str_of(src, "capability")
            .map_err(|_| reject(REASON_CONTINUATION, "review.source.capability invalid"))?;
        let src_stream = str_of(src, "stream")
            .map_err(|_| reject(REASON_CONTINUATION, "review.source.stream invalid"))?;
        let cont_cap = str_of(&continues, "capability").unwrap_or("");
        let cont_stream = str_of(&continues, "stream").unwrap_or("");
        if src_cap != cont_cap || src_stream != cont_stream {
            return Err(reject(
                REASON_CONTINUATION,
                "review.source does not match continues",
            ));
        }
        let grp = int_of(e, "group").map_err(|_| {
            reject(
                REASON_CONTINUATION,
                "review group/destination_cut_seq invalid",
            )
        })?;
        let gsz = int_of(e, "group_size").map_err(|_| {
            reject(
                REASON_CONTINUATION,
                "review group/destination_cut_seq invalid",
            )
        })?;
        let dest_cut = int_of(e, "destination_cut_seq").map_err(|_| {
            reject(
                REASON_CONTINUATION,
                "review group/destination_cut_seq invalid",
            )
        })?;
        if grp != seq || gsz != 1 || dest_cut != seq - 1 {
            return Err(reject(
                REASON_CONTINUATION,
                "review group/destination_cut_seq invalid",
            ));
        }
        let prefix = self.source_cut(src)?;
        let src_cut_seq = int_of(src, "cut_seq").unwrap_or(0);
        let cont_cut_seq = int_of(&continues, "cut_seq").unwrap_or(0);
        if src_cut_seq < cont_cut_seq {
            return Err(reject(
                REASON_CONTINUATION,
                "review cut_seq precedes opening cut",
            ));
        }
        let open_dig = slice_digest(&prefix[..(cont_cut_seq as usize)])
            .map_err(|_| reject(REASON_CONTINUATION, "opening prefix digest failed"))?;
        let cont_dig = str_of(&continues, "cut_digest").unwrap_or("");
        if open_dig != cont_dig {
            return Err(reject(
                REASON_CONTINUATION,
                "opening prefix digest mismatch",
            ));
        }
        let sm_dig = source_manifest(&prefix)
            .map_err(|_| reject(REASON_CONTINUATION, "source manifest calculation failed"))?;
        let want_src_dig = str_of(e, "source_state_digest")
            .map_err(|_| reject(REASON_CONTINUATION, "source_state_digest mismatch"))?;
        if sm_dig != want_src_dig {
            return Err(reject(REASON_CONTINUATION, "source_state_digest mismatch"));
        }
        let known =
            source_pins(&prefix).map_err(|_| reject(REASON_CONTINUATION, "source pins failed"))?;
        for imp in &self.imports {
            if !known.contains(&imp.pin) {
                return Err(reject(
                    REASON_CONTINUATION,
                    "imported pin not in source prefix",
                ));
            }
        }
        let am_dig = applied_manifest(&self.journal)
            .map_err(|_| reject(REASON_CONTINUATION, "applied manifest calculation failed"))?;
        let want_app_dig = str_of(e, "destination_applied_state_digest").map_err(|_| {
            reject(
                REASON_CONTINUATION,
                "destination_applied_state_digest mismatch",
            )
        })?;
        if am_dig != want_app_dig {
            return Err(reject(
                REASON_CONTINUATION,
                "destination_applied_state_digest mismatch",
            ));
        }
        self.review = Some(e.clone());
        Ok(())
    }

    pub(crate) fn set_source_chain(&mut self, input: SourceInput) -> Result<(), Reject> {
        let first_spec = input
            .raw_source_journal
            .as_ref()
            .or(input.source_journal.as_ref());
        let Some(first_spec) = first_spec else {
            return Ok(());
        };
        let anc_specs = input.raw_ancestors.as_ref().or(input.ancestors.as_ref());
        let (src, up, unobs) = chain_with_journals(first_spec, anc_specs, self.window)?;
        self.source = src;
        self.upstream = up.map(Box::new);
        self.upstream_unobserved = unobs;
        Ok(())
    }
}

fn capability_by_family(v: i64) -> Option<&'static str> {
    match v {
        0 => Some("contract: durable-record/v0"),
        1 => Some("contract: durable-record/v1"),
        _ => None,
    }
}

fn complete_cut(journal: &[Node], cut_seq: i64) -> bool {
    if cut_seq < 1 || cut_seq > (journal.len() as i64) {
        return false;
    }
    let e = &journal[(cut_seq - 1) as usize];
    let (Ok(seq), Ok(grp), Ok(gsz)) = (
        int_of(e, "seq"),
        int_of(e, "group"),
        int_of(e, "group_size"),
    ) else {
        return false;
    };
    seq == grp + gsz - 1
}

fn sort_by_jcs(nodes: &[Node]) -> Result<Vec<Node>, Reject> {
    let mut pairs = Vec::with_capacity(nodes.len());
    for n in nodes {
        pairs.push((jcs_string(n)?, n.clone()));
    }
    pairs.sort_by(|a, b| a.0.cmp(&b.0));
    Ok(pairs.into_iter().map(|(_, n)| n).collect())
}

fn manifest_digest(
    holds: &[Node],
    retired: &[Node],
    inherited: &[Node],
    include_inherited: bool,
) -> Result<String, Reject> {
    let oh = sort_by_jcs(holds)?;
    let or = sort_by_jcs(retired)?;
    let mut fields = vec![
        Member {
            name: "holds".to_owned(),
            value: Node::array(oh),
        },
        Member {
            name: "retired".to_owned(),
            value: Node::array(or),
        },
    ];
    if include_inherited {
        let oi = sort_by_jcs(inherited)?;
        fields.push(Member {
            name: "inherited".to_owned(),
            value: Node::array(oi),
        });
    }
    record_digest(&Node::object(fields))
}

type RestrictionTuple = (BTreeMap<i64, String>, BTreeMap<String, bool>, Option<Node>);

fn restriction_state(journal: &[Node]) -> Result<RestrictionTuple, Reject> {
    let mut holds = BTreeMap::new();
    let mut retired = BTreeMap::new();
    let mut review = None;
    for e in journal {
        let typ = str_of(e, "type")?;
        match typ {
            "tombstoned" => {
                let seq = int_of(e, "seq")?;
                let rd = str_of(e, "request_digest")?.to_owned();
                holds.insert(seq, rd);
            }
            "untombstoned" => {
                let Some(req) = e.get("request") else {
                    return Err(reject(
                        REASON_SHAPE,
                        "untombstoned.request is not an object",
                    ));
                };
                if req.kind != Kind::Object {
                    return Err(reject(
                        REASON_SHAPE,
                        "untombstoned.request is not an object",
                    ));
                }
                let rest = int_of(req, "restores")?;
                holds.remove(&rest);
            }
            "retired" => {
                let rid = str_of(e, "record_id")?.to_owned();
                retired.insert(rid, true);
            }
            "restrictions_reviewed" => {
                review = Some(e.clone());
            }
            _ => {}
        }
    }
    Ok((holds, retired, review))
}

fn source_manifest(journal: &[Node]) -> Result<String, Reject> {
    if journal.is_empty() {
        return Err(reject(REASON_CONTINUATION, "empty source journal"));
    }
    let v = int_of(&journal[0], "v")?;
    let Some(capability) = capability_by_family(v) else {
        return Err(reject(
            REASON_CONTINUATION,
            format!("unsupported capability family {v}"),
        ));
    };
    let stream = str_of(&journal[0], "stream")?.to_owned();
    let (holds, retired, review) = restriction_state(journal)?;
    let mut inherited = Vec::new();
    if let Some(rev) = review {
        let Some(src) = rev.get("source") else {
            return Err(reject(
                REASON_CONTINUATION,
                "review.source is not an object",
            ));
        };
        if src.kind != Kind::Object {
            return Err(reject(
                REASON_CONTINUATION,
                "review.source is not an object",
            ));
        }
        let sc = str_of(src, "capability")?.to_owned();
        let ss = str_of(src, "stream")?.to_owned();
        let ssd = str_of(&rev, "source_state_digest")?.to_owned();
        inherited.push(Node::object(vec![
            Member {
                name: "capability".to_owned(),
                value: Node::string(sc),
            },
            Member {
                name: "stream".to_owned(),
                value: Node::string(ss),
            },
            Member {
                name: "source_state_digest".to_owned(),
                value: Node::string(ssd),
            },
        ]));
    }
    let mut hold_nodes = Vec::with_capacity(holds.len());
    for (seq, d) in holds {
        hold_nodes.push(Node::object(vec![
            Member {
                name: "capability".to_owned(),
                value: Node::string(capability),
            },
            Member {
                name: "stream".to_owned(),
                value: Node::string(stream.clone()),
            },
            Member {
                name: "seq".to_owned(),
                value: Node::int(seq),
            },
            Member {
                name: "request_digest".to_owned(),
                value: Node::string(d),
            },
        ]));
    }
    let mut ret_nodes = Vec::with_capacity(retired.len());
    for r in retired.keys() {
        ret_nodes.push(Node::object(vec![
            Member {
                name: "capability".to_owned(),
                value: Node::string(capability),
            },
            Member {
                name: "stream".to_owned(),
                value: Node::string(stream.clone()),
            },
            Member {
                name: "record_id".to_owned(),
                value: Node::string(r.clone()),
            },
        ]));
    }
    manifest_digest(&hold_nodes, &ret_nodes, &inherited, true)
}

fn applied_manifest(journal: &[Node]) -> Result<String, Reject> {
    if journal.is_empty() {
        return Err(reject(REASON_CONTINUATION, "empty destination journal"));
    }
    let capability = "contract: durable-record/v1";
    let stream = str_of(&journal[0], "stream")?.to_owned();
    let (holds, retired, _) = restriction_state(journal)?;
    let mut hold_nodes = Vec::with_capacity(holds.len());
    for (seq, d) in holds {
        hold_nodes.push(Node::object(vec![
            Member {
                name: "capability".to_owned(),
                value: Node::string(capability),
            },
            Member {
                name: "stream".to_owned(),
                value: Node::string(stream.clone()),
            },
            Member {
                name: "seq".to_owned(),
                value: Node::int(seq),
            },
            Member {
                name: "request_digest".to_owned(),
                value: Node::string(d),
            },
        ]));
    }
    let mut ret_nodes = Vec::with_capacity(retired.len());
    for r in retired.keys() {
        ret_nodes.push(Node::object(vec![
            Member {
                name: "capability".to_owned(),
                value: Node::string(capability),
            },
            Member {
                name: "stream".to_owned(),
                value: Node::string(stream.clone()),
            },
            Member {
                name: "record_id".to_owned(),
                value: Node::string(r.clone()),
            },
        ]));
    }
    manifest_digest(&hold_nodes, &ret_nodes, &[], false)
}

fn source_pins(journal: &[Node]) -> Result<HashSet<Pin>, Reject> {
    let mut out = HashSet::new();
    for e in journal {
        let typ = str_of(e, "type")?;
        if typ == "appended" || typ == "revised" {
            out.insert(pin_of(e, "source")?);
        }
    }
    Ok(out)
}

fn check_journal_source(journal: &[Node]) -> Result<(), Reject> {
    check_journal(journal)
        .map_err(|e| reject(REASON_CONTINUATION, format!("source journal invalid: {e}")))?;
    for e in journal {
        let at_str =
            str_of(e, "at").map_err(|_| reject(REASON_CONTINUATION, "source entry missing at"))?;
        if !parse_v0_instant(at_str) {
            return Err(reject(
                REASON_CONTINUATION,
                "source entry invalid at timestamp",
            ));
        }
    }
    Ok(())
}

fn parse_source_spec(spec: &Node) -> Result<Vec<Node>, Reject> {
    if spec.kind != Kind::Array || spec.elems.is_empty() {
        return Err(reject(
            REASON_CONTINUATION,
            "invalid source journal container",
        ));
    }
    if spec.elems[0].kind == Kind::String {
        let mut parsed = Vec::with_capacity(spec.elems.len());
        for el in &spec.elems {
            if el.kind != Kind::String {
                return Err(reject(
                    REASON_CONTINUATION,
                    "mixed source journal container",
                ));
            }
            let n = wire::ingest(el.str_val.as_bytes(), KIND_JOURNAL_ENTRY)
                .map_err(|_| reject(REASON_CONTINUATION, "source raw entry failed wire ingest"))?;
            parsed.push(n);
        }
        if parsed[0].kind != Kind::Object || parsed[0].get("v").is_none() {
            return Err(reject(REASON_CONTINUATION, "source[0] missing v"));
        }
        return Ok(parsed);
    }
    let mut parsed = Vec::with_capacity(spec.elems.len());
    for el in &spec.elems {
        if el.kind != Kind::Object {
            return Err(reject(REASON_CONTINUATION, "source entry is not an object"));
        }
        let n = wire::ingest(&wire::encode(el), KIND_JOURNAL_ENTRY)
            .map_err(|_| reject(REASON_CONTINUATION, "source entry failed wire domain"))?;
        parsed.push(n);
    }
    if parsed[0].get("v").is_none() {
        return Err(reject(REASON_CONTINUATION, "source[0] missing v"));
    }
    Ok(parsed)
}

fn observed(
    journal: &[Node],
    window: i64,
    source: Vec<Node>,
    upstream: Option<State>,
    upstream_unobs: bool,
) -> Result<Option<State>, Reject> {
    if journal.is_empty() {
        return Err(reject(REASON_CONTINUATION, "empty observed journal"));
    }
    let family = int_of(&journal[0], "v")
        .map_err(|_| reject(REASON_CONTINUATION, "observed[0].v invalid"))?;
    let schema_name = match family {
        0 => "v0/journal-entry.schema.json",
        1 => "v1/journal-entry.schema.json",
        _ => {
            return Err(reject(
                REASON_CONTINUATION,
                format!("unsupported family {family}"),
            ))
        }
    };
    for e in journal {
        let ev = int_of(e, "v")
            .map_err(|_| reject(REASON_CONTINUATION, "observed entry family mismatch"))?;
        if ev != family {
            return Err(reject(
                REASON_CONTINUATION,
                "observed entry family mismatch",
            ));
        }
        shape::validate(schema_name, e)
            .map_err(|_| reject(REASON_CONTINUATION, "observed entry failed shape"))?;
    }
    check_journal_source(journal)?;
    if family == 0 {
        return Ok(None);
    }
    let mut st = State::new(&Node::int(window))
        .map_err(|_| reject(REASON_CONTINUATION, "window invalid"))?;
    st.source = source;
    st.upstream = upstream.map(Box::new);
    st.upstream_unobserved = upstream_unobs;
    for e in journal {
        st.ingest(e.clone()).map_err(|err| {
            reject(
                REASON_CONTINUATION,
                format!("observed v1 ingest failed: {err}"),
            )
        })?;
    }
    Ok(Some(st))
}

fn chain_with_journals(
    first_spec: &Node,
    anc_specs: Option<&Node>,
    window: i64,
) -> Result<(Vec<Node>, Option<State>, bool), Reject> {
    let mut specs = vec![first_spec];
    if let Some(anc) = anc_specs {
        if anc.kind != Kind::Array {
            return Err(reject(REASON_CONTINUATION, "ancestors is not an array"));
        }
        for el in &anc.elems {
            specs.push(el);
        }
    }
    let mut cur_source: Vec<Node> = Vec::new();
    let mut cur_upstream: Option<State> = None;
    let mut cur_unobs = false;
    let mut has_upstream = false;

    for i in (0..specs.len()).rev() {
        let journal = parse_source_spec(specs[i])?;
        let has_continues = journal[0].get("continues").is_some();
        if has_continues && !has_upstream {
            let first_journal = parse_source_spec(specs[0])?;
            return Ok((first_journal, None, true));
        }
        if !has_continues && has_upstream {
            return Err(reject(
                REASON_CONTINUATION,
                "non-continuation stream has ancestor",
            ));
        }
        let st = observed(&journal, window, cur_source, cur_upstream, cur_unobs)?;
        let is_cont = st.as_ref().is_some_and(|s| s.continues.is_some());
        cur_source = journal;
        cur_upstream = if is_cont { st } else { None };
        cur_unobs = false;
        has_upstream = true;
    }
    Ok((cur_source, cur_upstream, cur_unobs))
}

fn pin_from_node(n: Option<&Node>, where_ctx: &str) -> Result<Pin, Reject> {
    let Some(n) = n else {
        return Err(reject(
            REASON_SHAPE,
            format!("{where_ctx} is not an object"),
        ));
    };
    if n.kind != Kind::Object {
        return Err(reject(
            REASON_SHAPE,
            format!("{where_ctx} is not an object"),
        ));
    }
    Ok(Pin {
        stream: str_of(n, "stream")?.to_owned(),
        record_id: str_of(n, "record_id")?.to_owned(),
        revision: int_of(n, "revision")?,
        digest: str_of(n, "digest")?.to_owned(),
    })
}

fn pin_of(e: &Node, where_ctx: &str) -> Result<Pin, Reject> {
    let Some(rec) = e.get("record") else {
        return Err(reject(
            REASON_SHAPE,
            format!("{where_ctx}: record is not an object"),
        ));
    };
    if rec.kind != Kind::Object {
        return Err(reject(
            REASON_SHAPE,
            format!("{where_ctx}: record is not an object"),
        ));
    }
    Ok(Pin {
        stream: str_of(rec, "stream")?.to_owned(),
        record_id: str_of(rec, "record_id")?.to_owned(),
        revision: int_of(rec, "revision")?,
        digest: str_of(e, "digest")?.to_owned(),
    })
}

fn str_node(s: &str) -> Node {
    Node::string(s)
}

fn member(name: &str, value: Node) -> Member {
    Member {
        name: name.to_owned(),
        value,
    }
}

pub(crate) fn pin_node(p: &Pin) -> Node {
    Node::object(vec![
        member("stream", str_node(&p.stream)),
        member("record_id", str_node(&p.record_id)),
        member("revision", Node::int(p.revision)),
        member("digest", str_node(&p.digest)),
    ])
}

fn content_receipt(
    stream: &str,
    key: &str,
    outcome: &str,
    p: &Pin,
    seq: i64,
    failure_model: &str,
    body_status: &str,
) -> Node {
    let mut fields = vec![
        member("v", Node::int(1)),
        member("kind", str_node("receipt")),
        member("stream", str_node(stream)),
        member("idempotency_key", str_node(key)),
        member("outcome", str_node(outcome)),
        member("pin", pin_node(p)),
        member("seq", Node::int(seq)),
        member("failure_model", str_node(failure_model)),
    ];
    if !body_status.is_empty() {
        fields.push(member("body_status", str_node(body_status)));
    }
    Node::object(fields)
}

#[cfg_attr(not(test), allow(dead_code))]
fn decode_hex(s: &str) -> Option<Vec<u8>> {
    if s.len() % 2 != 0 {
        return None;
    }
    (0..s.len())
        .step_by(2)
        .map(|i| u8::from_str_radix(s.get(i..i + 2)?, 16).ok())
        .collect()
}

fn sha256_prefixed(b: &[u8]) -> String {
    use sha2::{Digest as _, Sha256};
    let sum = Sha256::digest(b);
    let mut out = String::from("sha256:");
    for byte in sum {
        out.push_str(&format!("{byte:02x}"));
    }
    out
}

fn verified_outcome() -> Node {
    Node::object(vec![member("outcome", str_node("receipt_verified"))])
}

fn retirement_request(e: &Node) -> Node {
    let mut fields = Vec::new();
    for k in ["stream", "record_id", "reason", "idempotency_key"] {
        if let Some(v) = e.get(k) {
            fields.push(Member {
                name: k.to_owned(),
                value: v.clone(),
            });
        }
    }
    Node::object(fields)
}

fn body_matches(rec_body: Option<&Node>, target_body: Option<&Node>) -> Result<bool, Reject> {
    let Some(rec_body) = rec_body else {
        return Ok(false);
    };
    if rec_body.kind != Kind::Object {
        return Ok(false);
    }
    let Some(target_body) = target_body else {
        return Err(reject(REASON_SHAPE, "body is not an object"));
    };
    if target_body.kind != Kind::Object {
        return Err(reject(REASON_SHAPE, "body is not an object"));
    }
    let d1 = str_of(rec_body, "digest")?;
    let l1 = int_of(rec_body, "length")?;
    let d2 = str_of(target_body, "digest")?;
    let l2 = int_of(target_body, "length")?;
    Ok(d1 == d2 && l1 == l2)
}

fn integral(n: Option<&Node>) -> Option<i64> {
    let n = n?;
    if n.kind != Kind::Number {
        return None;
    }
    if n.is_int {
        return (0..=MAX_SAFE_INTEGER)
            .contains(&n.int_val)
            .then_some(n.int_val);
    }
    let v = wire::safe_int(&n.token).ok()?;
    (0..=MAX_SAFE_INTEGER).contains(&v).then_some(v)
}

fn body_key_of(b: Option<&Node>, where_ctx: &str) -> Result<BodyKey, Reject> {
    let Some(b) = b else {
        return Err(reject(
            REASON_SHAPE,
            format!("{where_ctx}: body is not an object"),
        ));
    };
    if b.kind != Kind::Object {
        return Err(reject(
            REASON_SHAPE,
            format!("{where_ctx}: body is not an object"),
        ));
    }
    let dig = str_of(b, "digest")?.to_owned();
    let Some(length) = integral(b.get("length")) else {
        return Err(reject(
            REASON_SHAPE,
            format!("{where_ctx}: body.length is not a nonnegative safe integer"),
        ));
    };
    Ok(BodyKey {
        digest: dig,
        length,
    })
}

fn body_matches_target(rec_body: Option<&Node>, target: &BodyKey) -> bool {
    let Some(rec_body) = rec_body else {
        return false;
    };
    if rec_body.kind != Kind::Object {
        return false;
    }
    let Some(dig) = rec_body.get("digest") else {
        return false;
    };
    if dig.kind != Kind::String || dig.str_val != target.digest {
        return false;
    }
    integral(rec_body.get("length")) == Some(target.length)
}

fn compute_recovery_row(
    prior_phase: &str,
    prior_bound: i64,
    floor: i64,
    obs: &str,
    damaged: bool,
    _referenced: bool,
) -> &'static str {
    match prior_phase {
        "" => {
            if !damaged {
                if obs == "absent" {
                    return "unpublished";
                }
                return "publishing";
            }
            if floor == 0 && obs == "absent" {
                return "unpublished";
            }
            if floor > 0 && obs == "absent" {
                return "integrity_loss";
            }
            "publishing"
        }
        "published" => {
            if obs == "present_valid" {
                if !damaged || floor == prior_bound {
                    return "available";
                }
                return "publishing";
            }
            "integrity_loss"
        }
        "started" => {
            if !damaged {
                return "removal_pending";
            }
            if obs == "present_valid" {
                if floor == prior_bound {
                    return "available";
                }
                return "publishing";
            }
            if floor == prior_bound {
                return "removal_pending";
            }
            "integrity_loss"
        }
        "removed" => {
            if obs == "absent" {
                if !damaged || floor == prior_bound {
                    return "removed";
                }
                return "integrity_loss";
            }
            "publishing"
        }
        "recovery_integrity_loss" => "integrity_loss",
        _ => {
            if obs == "present_valid" {
                "publishing"
            } else {
                "integrity_loss"
            }
        }
    }
}
