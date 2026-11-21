// SPDX-License-Identifier: MIT OR Apache-2.0

//! Drives the vendored `durable-record/v1` vectors through the Rust journal
//! state and keeps a ledger of which vectors the implemented stages decide.
//!
//! This mirrors the Go driver in `internal/conformance`. The Rust ledger
//! (`conformance/v1-ledger-rust.json`) lists every vector as `pass`,
//! `pass:partial` or `pending:semantics`. A vector the implemented stages
//! contradict fails the test, a `pass` never regresses, and a vector whose
//! status changes fails the test until the ledger is updated with
//! `RECORDPRIMS_UPDATE_LEDGER=1`, so the ledger is always exact.
//!
//! A vector that reaches a step or input the Rust state does not implement
//! yet is incomplete: it never counts as a pass, because an unported check
//! might have refused first.

use std::collections::BTreeMap;
use std::fs;
use std::path::{Path, PathBuf};

use crate::journal::{self, SourceInput, State};
use crate::wire::{self, Kind, Member, Node, Reject, REASON_INGEST, REASON_NUMERIC_DOMAIN};

const STATUS_PASS: &str = "pass";
const STATUS_PENDING: &str = "pending:semantics";
const STATUS_PASS_PARTIAL: &str = "pass:partial";

fn repo_root() -> PathBuf {
    Path::new(env!("CARGO_MANIFEST_DIR")).join("..").join("..")
}

fn first_reject(raw: &str, kind: &str) -> Option<&'static str> {
    wire::ingest(raw.as_bytes(), kind).err().map(|r| r.reason)
}

/// Runs the wire stages over a vector's raw inputs in the reference
/// interpreter's order. Returns `None` when every raw input passes.
fn stage_result(v: &Node) -> Option<&'static str> {
    if let Some(p) = v.get("raw_profile") {
        if let Some(r) = first_reject(&p.str_val, wire::KIND_STRATUM_PROFILE) {
            return Some(r);
        }
    }
    if let Some(nd) = v.get("raw_journal_ndjson") {
        if let Err(r) = wire::ingest_lines(nd.str_val.as_bytes(), wire::KIND_JOURNAL_ENTRY) {
            return Some(r.reason);
        }
    } else if let Some(j) = v.get("raw_journal") {
        for line in &j.elems {
            if let Some(r) = first_reject(&line.str_val, wire::KIND_JOURNAL_ENTRY) {
                return Some(r);
            }
        }
    }
    if let Some(steps) = v.get("steps") {
        for s in &steps.elems {
            for (field, kind) in [
                ("raw_request", "protection_request"),
                ("raw_entry", wire::KIND_JOURNAL_ENTRY),
                ("raw_body_state", "body_state_request"),
                ("raw_removal_eligibility", "removal_eligibility"),
                ("raw_verify_seal", "seal_context"),
                ("raw_use", "export_request"),
            ] {
                if let Some(raw) = s.get(field) {
                    if let Some(r) = first_reject(&raw.str_val, kind) {
                        return Some(r);
                    }
                }
            }
            if let Some(raw) = s.get("raw_receipt") {
                if let Err(r) = wire::ingest_receipt(raw.str_val.as_bytes()) {
                    return Some(r.reason);
                }
            }
        }
    }
    if let Some(src) = v.get("raw_source_journal") {
        for line in &src.elems {
            if first_reject(&line.str_val, wire::KIND_JOURNAL_ENTRY).is_some() {
                return Some("continuation");
            }
        }
    }
    None
}

/// Re-runs the wire stages over a document the vector gives as JSON, so its
/// protocol integers are typed as the reference types them.
fn typed(n: &Node, kind: &str) -> Result<Node, Reject> {
    wire::ingest(&wire::encode(n), kind)
}

struct Semantic {
    refusal: Option<String>,
    results: Vec<Node>,
    active_holds: Vec<i64>,
    journal_size: usize,
    complete: bool,
}

impl Semantic {
    fn refused(reason: &str, complete: bool) -> Self {
        Self {
            refusal: Some(reason.to_owned()),
            results: Vec::new(),
            active_holds: Vec::new(),
            journal_size: 0,
            complete,
        }
    }

    fn incomplete() -> Self {
        Self {
            refusal: None,
            results: Vec::new(),
            active_holds: Vec::new(),
            journal_size: 0,
            complete: false,
        }
    }
}

fn clone_list(n: Option<&Node>) -> Option<Node> {
    n.cloned()
}

fn journaled() -> Node {
    Node::object(vec![Member {
        name: "outcome".into(),
        value: Node::string("journaled"),
    }])
}

/// Vector inputs whose checks the Rust state does not implement yet.
fn has_unported_input(v: &Node) -> bool {
    [
        "sharing_streams",
        "quarantined_streams",
        "candidate_streams",
        "expected_store",
        "recovery_contexts",
        "source_seal",
    ]
    .iter()
    .any(|k| v.get(k).is_some())
}

/// Replays a vector whose raw inputs passed the wire stages, in the reference
/// interpreter's order, and returns the first refusal.
fn semantic_result(v: &Node) -> Semantic {
    let Some(window) = v.get("window_ms") else {
        return Semantic::incomplete();
    };
    let mut st = match State::new(window) {
        Ok(s) => s,
        Err(r) => return Semantic::refused(r.reason, true),
    };
    let prof = if let Some(p) = v.get("raw_profile") {
        Some(wire::ingest(
            p.str_val.as_bytes(),
            wire::KIND_STRATUM_PROFILE,
        ))
    } else {
        v.get("profile")
            .map(|p| typed(p, wire::KIND_STRATUM_PROFILE))
    };
    if let Some(prof) = prof {
        let prof = match prof {
            Ok(p) => p,
            Err(r) => return Semantic::refused(r.reason, true),
        };
        if let Err(r) = journal::check_profile(&prof) {
            return Semantic::refused(r.reason, true);
        }
        st.set_profile(&prof);
    }
    let mut entries = Vec::new();
    if let Some(nd) = v.get("raw_journal_ndjson") {
        match wire::ingest_lines(nd.str_val.as_bytes(), wire::KIND_JOURNAL_ENTRY) {
            Ok(es) => entries = es,
            Err(r) => return Semantic::refused(r.reason, true),
        }
    } else if let Some(j) = v.get("raw_journal") {
        for line in &j.elems {
            match wire::ingest(line.str_val.as_bytes(), wire::KIND_JOURNAL_ENTRY) {
                Ok(n) => entries.push(n),
                Err(r) => return Semantic::refused(r.reason, true),
            }
        }
    } else if let Some(j) = v.get("journal") {
        for e in &j.elems {
            match typed(e, wire::KIND_JOURNAL_ENTRY) {
                Ok(n) => entries.push(n),
                Err(r) => return Semantic::refused(r.reason, true),
            }
        }
    }
    // Sharing-stream observation, recovery validation and source seals are
    // not ported: a vector carrying them can never be complete.
    // Recovery validation also runs on a first entry with a `recovery`
    // header and on a profile that lists quarantined designated streams.
    let recovery_header = entries.first().is_some_and(|e| e.get("recovery").is_some());
    let quarantined = v
        .get("raw_profile")
        .map(|p| p.str_val.contains("quarantined_designated_streams"))
        .unwrap_or(false)
        || v.get("profile")
            .and_then(|p| p.get("runtime"))
            .and_then(|r| r.get("quarantined_designated_streams"))
            .is_some();
    let unported = has_unported_input(v) || recovery_header || quarantined;
    let complete = |st: &State| !unported && !st.partial();

    let mut src_in = SourceInput {
        source_journal: clone_list(v.get("source_journal")),
        raw_source_journal: clone_list(v.get("raw_source_journal")),
        ancestors: clone_list(v.get("ancestors")),
        raw_ancestors: clone_list(v.get("raw_ancestors")),
    };
    if let Err(r) = st.set_source_chain(src_in.clone()) {
        return Semantic::refused(r.reason, !unported);
    }
    if let Err(r) = journal::check_journal(&entries) {
        return Semantic::refused(r.reason, !unported);
    }
    for e in entries {
        if let Err(r) = st.ingest(e) {
            return Semantic::refused(r.reason, complete(&st));
        }
    }
    let mut results = Vec::new();
    if let Some(steps) = v.get("steps") {
        for s in &steps.elems {
            if s.get("raw_entry").is_some() || s.get("entry").is_some() {
                let e = match s.get("raw_entry") {
                    Some(raw) => wire::ingest(raw.str_val.as_bytes(), wire::KIND_JOURNAL_ENTRY),
                    None => typed(
                        s.get("entry").expect("entry step"),
                        wire::KIND_JOURNAL_ENTRY,
                    ),
                };
                let e = match e {
                    Ok(e) => e,
                    Err(r) => return Semantic::refused(r.reason, complete(&st)),
                };
                if let Err(r) = st.ingest(e) {
                    return Semantic::refused(r.reason, complete(&st));
                }
                results.push(journaled());
            } else if let Some(se) = s.get("source_entry") {
                push_elem(&mut src_in.source_journal, se.clone());
                if let Err(r) = st.set_source_chain(src_in.clone()) {
                    return Semantic::refused(r.reason, complete(&st));
                }
                results.push(journaled());
            } else if let Some(se) = s.get("raw_source_entry") {
                push_elem(&mut src_in.raw_source_journal, se.clone());
                if let Err(r) = st.set_source_chain(src_in.clone()) {
                    return Semantic::refused(r.reason, complete(&st));
                }
                results.push(journaled());
            } else if s.get("ancestor_entry").is_some() || s.get("raw_ancestor_entry").is_some() {
                let raw = s.get("raw_ancestor_entry").is_some();
                let entry = s
                    .get(if raw {
                        "raw_ancestor_entry"
                    } else {
                        "ancestor_entry"
                    })
                    .expect("ancestor step")
                    .clone();
                let idx = match s.get("ancestor").map(|a| wire::safe_int(&a.token)) {
                    Some(Ok(i)) => i,
                    Some(Err(r)) => return Semantic::refused(r.reason, complete(&st)),
                    None => return Semantic::incomplete(),
                };
                let list = if raw {
                    &mut src_in.raw_ancestors
                } else {
                    &mut src_in.ancestors
                };
                let Some(anc) = list
                    .as_mut()
                    .and_then(|l| l.elems.get_mut(usize::try_from(idx).ok()?))
                else {
                    return Semantic::incomplete();
                };
                anc.elems.push(entry);
                if let Err(r) = st.set_source_chain(src_in.clone()) {
                    return Semantic::refused(r.reason, complete(&st));
                }
                results.push(journaled());
            } else if s.get("raw_request").is_some() || s.get("request").is_some() {
                let req = match s.get("raw_request") {
                    Some(raw) => wire::ingest(raw.str_val.as_bytes(), "protection_request"),
                    None => typed(
                        s.get("request").expect("request step"),
                        "protection_request",
                    ),
                };
                let at = s.get("at").map(|a| a.str_val.clone()).unwrap_or_default();
                match req.and_then(|r| st.request(&r, &at)) {
                    Ok(res) => results.push(res),
                    Err(r) => return Semantic::refused(r.reason, complete(&st)),
                }
            } else if let Some(r) = s.get("retire") {
                let at = s.get("at").map(|a| a.str_val.clone()).unwrap_or_default();
                match st.retire(r, &at) {
                    Ok(res) => results.push(res),
                    Err(r) => return Semantic::refused(r.reason, complete(&st)),
                }
            } else if s.get("raw_receipt").is_some() || s.get("receipt").is_some() {
                let rcpt = match s.get("raw_receipt") {
                    Some(raw) => wire::ingest_receipt(raw.str_val.as_bytes()),
                    None => {
                        let r = s.get("receipt").expect("receipt step");
                        typed(r, wire::receipt_kind(r))
                    }
                };
                match rcpt.and_then(|r| st.verify_receipt(&r)) {
                    Ok(res) => results.push(res),
                    Err(r) => return Semantic::refused(r.reason, complete(&st)),
                }
            } else if let Some(rec) = s.get("reconcile") {
                let f = |k: &str| rec.get(k).map(|x| x.str_val.clone()).unwrap_or_default();
                match st.reconcile(&f("key"), &f("at"), &f("attempted_at")) {
                    Ok(res) => results.push(res),
                    Err(r) => return Semantic::refused(r.reason, complete(&st)),
                }
            } else {
                // body_state, removal_eligibility, verify_seal and use are
                // not ported.
                return Semantic::incomplete();
            }
        }
    }
    if let Err(r) = journal::check_journal(st.journal()) {
        return Semantic::refused(r.reason, complete(&st));
    }
    Semantic {
        refusal: None,
        results,
        active_holds: st.active_holds(),
        journal_size: st.len(),
        complete: complete(&st),
    }
}

fn push_elem(list: &mut Option<Node>, e: Node) {
    match list {
        Some(n) if n.kind == Kind::Array => n.elems.push(e),
        _ => *list = Some(Node::array(vec![e])),
    }
}

/// Reports whether the expected result matches the step results, active
/// holds and final journal size.
fn accepted_as(exp: &Node, sem: &Semantic) -> bool {
    let (Some(res), Some(holds), Some(js)) = (
        exp.get("results"),
        exp.get("active_holds"),
        exp.get("journal_size"),
    ) else {
        return false;
    };
    if exp.fields.len() != 3 || holds.elems.len() != sem.active_holds.len() {
        return false;
    }
    for (h, got) in holds.elems.iter().zip(&sem.active_holds) {
        match wire::safe_int(&h.token) {
            Ok(n) if n == *got => {}
            _ => return false,
        }
    }
    if res.elems.len() != sem.results.len() {
        return false;
    }
    for (want, got) in res.elems.iter().zip(&sem.results) {
        let (Ok(w), Ok(g)) = (
            wire::parse_json(&wire::encode(want)),
            wire::parse_json(&wire::encode(got)),
        ) else {
            return false;
        };
        if !journal::equal_nodes(Some(&w), Some(&g)) {
            return false;
        }
    }
    matches!(wire::safe_int(&js.token), Ok(n) if usize::try_from(n).ok() == Some(sem.journal_size))
}

fn or_accept(s: &str) -> &str {
    if s.is_empty() {
        "acceptance"
    } else {
        s
    }
}

fn read_ledger(path: &Path) -> BTreeMap<String, String> {
    let Ok(raw) = fs::read(path) else {
        return BTreeMap::new();
    };
    let n = wire::parse_json(&raw).expect("ledger is strict JSON");
    n.fields
        .iter()
        .map(|m| (m.name.clone(), m.value.str_val.clone()))
        .collect()
}

fn write_ledger(path: &Path, got: &BTreeMap<String, String>) {
    let mut out = String::from("{\n");
    let n = got.len();
    for (i, (k, v)) in got.iter().enumerate() {
        out.push_str(&format!("  \"{k}\": \"{v}\""));
        out.push_str(if i + 1 < n { ",\n" } else { "\n" });
    }
    out.push_str("}\n");
    fs::write(path, out).expect("write ledger");
}

#[test]
fn v1_vectors() {
    let root = repo_root();
    let dir = root.join("contract/crucible/schemas/durable-record/v1/vectors");
    let mut files: Vec<PathBuf> = fs::read_dir(&dir)
        .unwrap_or_else(|e| panic!("no vectors at {}: {e}", dir.display()))
        .filter_map(|e| e.ok().map(|e| e.path()))
        .filter(|p| p.extension().is_some_and(|x| x == "json"))
        .collect();
    files.sort();
    assert!(!files.is_empty(), "no vectors found");
    let ledger_path = root.join("conformance/v1-ledger-rust.json");
    let ledger = read_ledger(&ledger_path);

    let mut got = BTreeMap::new();
    let mut errors = Vec::new();
    for f in &files {
        let name = f.file_stem().unwrap().to_string_lossy().into_owned();
        let raw = fs::read(f).unwrap();
        let v = wire::parse_json(&raw)
            .unwrap_or_else(|e| panic!("{name}: vector file is not strict JSON: {e}"));
        let want = v
            .get("expected")
            .and_then(|e| e.get("refusal"))
            .map(|r| r.str_val.clone())
            .unwrap_or_default();
        let stage = stage_result(&v);
        let Some(stage) = stage else {
            let sem = semantic_result(&v);
            let refusal = sem.refusal.clone().unwrap_or_default();
            let status = if !sem.complete && !refusal.is_empty() && refusal == want {
                Some(STATUS_PASS_PARTIAL)
            } else if !sem.complete {
                Some(STATUS_PENDING)
            } else if !refusal.is_empty() && refusal == want {
                Some(STATUS_PASS)
            } else if !refusal.is_empty() {
                errors.push(format!(
                    "{name}: semantic stage refuses {refusal:?}, vector expects {:?}",
                    or_accept(&want)
                ));
                None
            } else if !want.is_empty() {
                errors.push(format!(
                    "{name}: semantic stage accepts a complete replay, vector expects {want:?}"
                ));
                None
            } else if !accepted_as(v.get("expected").unwrap_or(&Node::null()), &sem) {
                errors.push(format!(
                    "{name}: complete replay of {} entries does not match the expected result",
                    sem.journal_size
                ));
                None
            } else {
                Some(STATUS_PASS)
            };
            if let Some(s) = status {
                got.insert(name, s.to_owned());
            }
            continue;
        };
        let early = want == REASON_INGEST
            || want == REASON_NUMERIC_DOMAIN
            || (want == "continuation"
                && v.get("raw_source_journal").is_some()
                && stage == "continuation");
        if stage != want {
            errors.push(format!(
                "{name}: implemented stages refuse {stage:?}, vector expects {:?}",
                or_accept(&want)
            ));
        } else if early {
            got.insert(name, STATUS_PASS.to_owned());
        } else {
            got.insert(name, STATUS_PENDING.to_owned());
        }
    }

    if std::env::var("RECORDPRIMS_UPDATE_LEDGER").as_deref() == Ok("1") {
        for (n, s) in &ledger {
            if s == STATUS_PASS && got.get(n).map(String::as_str) != Some(STATUS_PASS) {
                panic!(
                    "{n}: ledger says pass, run gives {:?}; update mode never lowers a pass",
                    got.get(n)
                );
            }
        }
        assert!(errors.is_empty(), "{}", errors.join("\n"));
        write_ledger(&ledger_path, &got);
        return;
    }
    for (n, s) in &got {
        if ledger.get(n) != Some(s) {
            errors.push(format!(
                "{n}: ledger says {:?}, run gives {s:?} (set RECORDPRIMS_UPDATE_LEDGER=1 if this is progress)",
                ledger.get(n)
            ));
        }
    }
    for n in ledger.keys() {
        if !got.contains_key(n) {
            errors.push(format!("{n}: in the ledger but not among the vectors"));
        }
    }
    let pass = got.values().filter(|s| *s == STATUS_PASS).count();
    let partial = got.values().filter(|s| *s == STATUS_PASS_PARTIAL).count();
    eprintln!(
        "{} vectors: {pass} pass, {partial} pass:partial, {} pending",
        got.len(),
        got.len() - pass - partial
    );
    assert!(errors.is_empty(), "{}", errors.join("\n"));
}
