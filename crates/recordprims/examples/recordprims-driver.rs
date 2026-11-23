// SPDX-License-Identifier: MIT OR Apache-2.0

//! Language-neutral JSON-lines conformance and differential driver for the
//! Rust `recordprims` crate, matching `internal/difftool` in the Go tree.

use std::collections::HashMap;
use std::fs::{self, OpenOptions as FsOpenOptions};
use std::io::{self, BufRead, Cursor, Write};
use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicI64, Ordering};
use std::sync::Arc;
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use recordprims::fsroot;
use recordprims::wire::{self, Kind, Member, Node};
use recordprims::{
    format_instant, init, is_indeterminate, normalize_producer_instant, open, parse_instant,
    Authorization, Clock, Error, FileLock, FrameHeader, FrameKind, FrameReader, InitOptions,
    Instant, Mode, OpenOptions, Pin, ProtectionRequest, Provenance, Receipt, ReleaseOutcome,
    ReleaseSink, RemoveBodyReason, RemoveBodyRequest, RetireReason, Store, UseClosure, UsePurpose,
    UseRequest, Write as RecordWrite,
};

struct StepClock {
    now_us: AtomicI64,
}

impl StepClock {
    fn new(t: Instant) -> Self {
        Self {
            now_us: AtomicI64::new(t.as_unix_micros()),
        }
    }

    fn set(&self, t: Instant) {
        self.now_us.store(t.as_unix_micros(), Ordering::SeqCst);
    }
}

impl Clock for StepClock {
    fn now(&self) -> Instant {
        Instant::from_unix_micros(self.now_us.load(Ordering::SeqCst)).expect("valid instant")
    }
}

struct HandleEntry {
    store: Store,
    clock: Arc<StepClock>,
    streams: HashMap<String, Mode>,
}

fn opt_str(node: &Node, key: &str) -> Option<String> {
    node.get(key)
        .filter(|n| n.kind == Kind::String)
        .map(|n| n.str_val.clone())
}

fn opt_i64(node: &Node, key: &str) -> Option<i64> {
    node.get(key)
        .filter(|n| n.kind == Kind::Number)
        .and_then(|n| n.token.parse::<i64>().ok())
}

fn opt_u64(node: &Node, key: &str) -> Option<u64> {
    node.get(key)
        .filter(|n| n.kind == Kind::Number)
        .and_then(|n| n.token.parse::<u64>().ok())
}

fn opt_u32(node: &Node, key: &str) -> Option<u32> {
    node.get(key)
        .filter(|n| n.kind == Kind::Number)
        .and_then(|n| n.token.parse::<u32>().ok())
}

fn decode_hex(s: &str) -> Result<Vec<u8>, String> {
    if !s.len().is_multiple_of(2) {
        return Err("odd hex length".to_owned());
    }
    let mut out = Vec::with_capacity(s.len() / 2);
    let bytes = s.as_bytes();
    for i in (0..bytes.len()).step_by(2) {
        let hi = (bytes[i] as char)
            .to_digit(16)
            .ok_or_else(|| "invalid hex digit".to_owned())?;
        let lo = (bytes[i + 1] as char)
            .to_digit(16)
            .ok_or_else(|| "invalid hex digit".to_owned())?;
        out.push(((hi << 4) | lo) as u8);
    }
    Ok(out)
}

fn decode_step_bytes(st: &Node) -> Result<Vec<u8>, String> {
    if let Some(h) = opt_str(st, "data_hex") {
        if !h.is_empty() {
            return decode_hex(&h);
        }
    }
    Ok(opt_str(st, "data_utf8").unwrap_or_default().into_bytes())
}

fn resolve_dir(base: &Path, subdir: Option<&str>) -> PathBuf {
    match subdir.filter(|s| !s.is_empty()) {
        Some(sub) => {
            let mut p = base.to_path_buf();
            for part in sub.split('/') {
                if !part.is_empty() {
                    p.push(part);
                }
            }
            p
        }
        None => base.to_path_buf(),
    }
}

fn default_clock_time(at: Option<&str>) -> Result<Instant, Error> {
    match at.filter(|s| !s.is_empty()) {
        Some(s) => parse_instant(s),
        None => parse_instant("2026-10-25T12:00:00.000000Z"),
    }
}

fn classify_err(op: &str, err: &Error) -> Node {
    let ind = is_indeterminate(err);
    let mut cleanup_detail: Option<String> = None;
    let mut cur = err;
    while let Error::WithCleanupIo {
        primary,
        op: cop,
        class: cclass,
    } = cur
    {
        if cleanup_detail.is_none() {
            cleanup_detail = Some(format!("recordprims: io_error{{op={cop}, class={cclass}}}"));
        }
        cur = primary;
    }
    let (code, reason, state) = match cur {
        Error::StoreInvalid(_) => ("store_invalid", None, None),
        Error::Locked => ("locked", None, None),
        Error::Schema(_) => ("schema", None, None),
        Error::Unauthorized => ("unauthorized", None, None),
        Error::JournalInvalid { reason } => ("journal_invalid", Some(reason.as_str()), None),
        Error::BodyIncomplete { state } => ("body_incomplete", None, Some(state.as_str())),
        Error::ClockRegression => ("clock_regression", None, None),
        Error::StreamSealed => ("stream_sealed", None, None),
        Error::ContinuationUnsupported => ("continuation_unsupported", None, None),
        Error::StaleReview => ("stale_review", None, None),
        Error::StaleCut => ("stale_cut", None, None),
        Error::Closed => ("closed", None, None),
        Error::DamagedTail { .. } => ("damaged_tail", None, None),
        Error::ReaderRevoked => ("reader_revoked", None, None),
        Error::SinkReentered => ("sink_reentered", None, None),
        Error::Io { .. } | Error::WithCleanupIo { .. } => ("io_error", None, None),
        _ => ("other", None, None),
    };
    let mut fields = vec![
        Member {
            name: "op".to_owned(),
            value: Node::string(op),
        },
        Member {
            name: "ok".to_owned(),
            value: Node::boolean(false),
        },
        Member {
            name: "error_code".to_owned(),
            value: Node::string(code),
        },
    ];
    if let Some(r) = reason {
        if !r.is_empty() {
            fields.push(Member {
                name: "reason".to_owned(),
                value: Node::string(r),
            });
        }
    }
    if let Some(s) = state {
        if !s.is_empty() {
            fields.push(Member {
                name: "state".to_owned(),
                value: Node::string(s),
            });
        }
    }
    if let Some(d) = cleanup_detail {
        fields.push(Member {
            name: "detail".to_owned(),
            value: Node::string(d),
        });
    }
    fields.push(Member {
        name: "indeterminate".to_owned(),
        value: Node::boolean(ind),
    });
    Node::object(fields)
}

fn ok_node(op: &str, extra: Vec<Member>) -> Node {
    let mut fields = vec![
        Member {
            name: "op".to_owned(),
            value: Node::string(op),
        },
        Member {
            name: "ok".to_owned(),
            value: Node::boolean(true),
        },
    ];
    fields.extend(extra);
    Node::object(fields)
}

fn create_temp_store_dir() -> Result<PathBuf, String> {
    let base = std::env::temp_dir();
    let pid = std::process::id();
    for attempt in 0u64..10_000 {
        let nanos = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .unwrap_or_default()
            .as_nanos();
        let cand = base.join(format!("recordprims-rust-driver-{pid}-{nanos}-{attempt}"));
        match fs::create_dir(&cand) {
            Ok(()) => {
                #[cfg(unix)]
                {
                    use std::os::unix::fs::PermissionsExt;
                    fs::set_permissions(&cand, fs::Permissions::from_mode(0o700))
                        .map_err(|e| e.to_string())?;
                }
                return Ok(cand);
            }
            Err(e) if e.kind() == io::ErrorKind::AlreadyExists => continue,
            Err(e) => return Err(e.to_string()),
        }
    }
    Err("could not allocate temp store dir".to_owned())
}

fn set_mode_if_unix(path: &Path, mode: u32) -> Result<(), String> {
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        fs::set_permissions(path, fs::Permissions::from_mode(mode)).map_err(|e| e.to_string())?;
    }
    #[cfg(not(unix))]
    {
        let _ = (path, mode);
    }
    Ok(())
}

fn mkdir_all_0700(base: &Path, target: &Path) -> Result<(), String> {
    let rel = target.strip_prefix(base).unwrap_or(target);
    let mut cur = base.to_path_buf();
    for comp in rel.components() {
        cur.push(comp);
        if !cur.exists() {
            fs::create_dir(&cur).map_err(|e| e.to_string())?;
            set_mode_if_unix(&cur, 0o700)?;
        }
    }
    Ok(())
}

fn run_scenario(sc: &Node) -> Result<Node, String> {
    let id = opt_str(sc, "id").unwrap_or_default();
    let given_dir = opt_str(sc, "store_dir").filter(|s| !s.is_empty());
    let (store_dir, cleanup) = match given_dir {
        Some(d) => (PathBuf::from(d), false),
        None => (create_temp_store_dir()?, true),
    };

    let steps = sc
        .get("steps")
        .filter(|n| n.kind == Kind::Array)
        .map(|n| n.elems.as_slice())
        .unwrap_or(&[]);

    let mut handles: HashMap<String, HandleEntry> = HashMap::new();
    let mut results: Vec<Node> = Vec::with_capacity(steps.len());

    let fso = fsroot::Options {
        allow_tmpfs: true,
        allow_overlay: true,
    };

    let run_res: Result<(), String> = (|| {
        for st in steps {
            let op = opt_str(st, "op").unwrap_or_default();
            let hname = opt_str(st, "handle")
                .filter(|s| !s.is_empty())
                .unwrap_or_else(|| "default".to_owned());

            match op.as_str() {
                "init" => {
                    let t0 = match default_clock_time(opt_str(st, "at").as_deref()) {
                        Ok(t) => t,
                        Err(e) => {
                            results.push(classify_err(&op, &e));
                            continue;
                        }
                    };
                    if let Some(ms) = opt_i64(st, "window_ms") {
                        if ms < 0 {
                            results.push(classify_err(
                                &op,
                                &Error::Schema("negative window_ms".to_owned()),
                            ));
                            continue;
                        }
                    }
                    let clk = Arc::new(StepClock::new(t0));
                    let opts = InitOptions {
                        idempotency_window: opt_i64(st, "window_ms")
                            .map(|ms| Duration::from_millis(ms as u64)),
                        body_removal_stream: opt_str(st, "body_removal_stream").unwrap_or_default(),
                        clock: Some(clk),
                        fs: fso,
                    };
                    let target_dir = resolve_dir(&store_dir, opt_str(st, "subdir").as_deref());
                    match init(&target_dir, opts) {
                        Ok(()) => results.push(ok_node(&op, Vec::new())),
                        Err(e) => results.push(classify_err(&op, &e)),
                    }
                }

                "open" => {
                    let t0 = match default_clock_time(opt_str(st, "at").as_deref()) {
                        Ok(t) => t,
                        Err(e) => {
                            results.push(classify_err(&op, &e));
                            continue;
                        }
                    };
                    let clk = Arc::new(StepClock::new(t0));
                    let opts = OpenOptions {
                        clock: Some(clk.clone()),
                        pending_body_ttl: opt_i64(st, "pending_body_ttl_ms")
                            .map(|ms| Duration::from_millis(ms.max(0) as u64))
                            .unwrap_or(Duration::ZERO),
                        lock_timeout: opt_i64(st, "lock_timeout_ms")
                            .map(|ms| Duration::from_millis(ms.max(0) as u64))
                            .unwrap_or(Duration::ZERO),
                        allow_overlay: true,
                        fs: fso,
                    };
                    let target_dir = resolve_dir(&store_dir, opt_str(st, "subdir").as_deref());
                    match open(&target_dir, opts) {
                        Ok(s) => {
                            if let Some(old) = handles.remove(&hname) {
                                let _ = old.store.close();
                            }
                            let prof = s.profile();
                            let out = ok_node(
                                &op,
                                vec![
                                    Member {
                                        name: "store_id_len".to_owned(),
                                        value: Node::int(s.store_id().len() as i64),
                                    },
                                    Member {
                                        name: "window_ms".to_owned(),
                                        value: Node::int(s.idempotency_window().as_millis() as i64),
                                    },
                                    Member {
                                        name: "stratum".to_owned(),
                                        value: Node::string(prof.runtime.stratum),
                                    },
                                    Member {
                                        name: "failure_model".to_owned(),
                                        value: Node::string(prof.runtime.failure_model),
                                    },
                                    Member {
                                        name: "commit_strategy".to_owned(),
                                        value: Node::string(prof.runtime.commit_strategy),
                                    },
                                    Member {
                                        name: "body_removal_stream".to_owned(),
                                        value: Node::string(prof.runtime.body_removal_stream),
                                    },
                                ],
                            );
                            handles.insert(
                                hname,
                                HandleEntry {
                                    store: s,
                                    clock: clk,
                                    streams: HashMap::new(),
                                },
                            );
                            results.push(out);
                        }
                        Err(e) => results.push(classify_err(&op, &e)),
                    }
                }

                "close" => {
                    let he = handles
                        .get(&hname)
                        .ok_or_else(|| format!("unknown handle {hname:?}"))?;
                    match he.store.close() {
                        Ok(()) => results.push(ok_node(&op, Vec::new())),
                        Err(e) => results.push(classify_err(&op, &e)),
                    }
                }

                "set_clock" => {
                    let he = handles
                        .get(&hname)
                        .ok_or_else(|| format!("unknown handle {hname:?}"))?;
                    let at = opt_str(st, "at").unwrap_or_default();
                    match parse_instant(&at) {
                        Ok(t) => {
                            he.clock.set(t);
                            results.push(ok_node(&op, Vec::new()));
                        }
                        Err(e) => results.push(classify_err(&op, &e)),
                    }
                }

                "put_body" => {
                    let he = handles
                        .get(&hname)
                        .ok_or_else(|| format!("unknown handle {hname:?}"))?;
                    let b = decode_step_bytes(st)?;
                    let media_type = opt_str(st, "media_type").unwrap_or_default();
                    match he.store.put_body(Cursor::new(b), &media_type) {
                        Ok((bref, lease)) => {
                            let mut extra = vec![
                                Member {
                                    name: "digest".to_owned(),
                                    value: Node::string(bref.digest),
                                },
                                Member {
                                    name: "length".to_owned(),
                                    value: Node::int(bref.length),
                                },
                            ];
                            if !bref.media_type.is_empty() {
                                extra.push(Member {
                                    name: "media_type".to_owned(),
                                    value: Node::string(bref.media_type),
                                });
                            }
                            extra.push(Member {
                                name: "lease_id_len".to_owned(),
                                value: Node::int(lease.id.len() as i64),
                            });
                            extra.push(Member {
                                name: "lease_digest".to_owned(),
                                value: Node::string(lease.digest),
                            });
                            extra.push(Member {
                                name: "lease_expires_at".to_owned(),
                                value: Node::string(format_instant(lease.expires_at)),
                            });
                            results.push(ok_node(&op, extra));
                        }
                        Err(e) => results.push(classify_err(&op, &e)),
                    }
                }

                "remove_body" => {
                    let he = handles
                        .get(&hname)
                        .ok_or_else(|| format!("unknown handle {hname:?}"))?;
                    let prov = st
                        .get("provenance")
                        .filter(|n| n.kind == Kind::Object)
                        .map(|p| Provenance {
                            actor_kind: opt_str(p, "actor_kind").unwrap_or_default(),
                            actor: opt_str(p, "actor").unwrap_or_default(),
                            supervisor: opt_str(p, "supervisor").unwrap_or_default(),
                            model: opt_str(p, "model").unwrap_or_default(),
                            tool: opt_str(p, "tool").unwrap_or_default(),
                        })
                        .unwrap_or_default();
                    let auth = st
                        .get("authorization")
                        .filter(|n| n.kind == Kind::Object)
                        .map(|a| Authorization {
                            r#ref: opt_str(a, "ref").unwrap_or_default(),
                            digest: opt_str(a, "digest").unwrap_or_default(),
                        });
                    let reason_str = opt_str(st, "reason").unwrap_or_default();
                    let Some(reason) = RemoveBodyReason::from_wire(&reason_str) else {
                        results.push(classify_err(&op, &Error::schema("removal reason")));
                        continue;
                    };
                    let req = RemoveBodyRequest {
                        digest: opt_str(st, "digest").unwrap_or_default(),
                        length: opt_i64(st, "length").unwrap_or(0),
                        complete_incarnation: opt_u64(st, "complete_incarnation"),
                        reason,
                        provenance: prov,
                        authorization: auth,
                    };
                    match he.store.remove_body(&req) {
                        Ok(res) => {
                            results.push(ok_node(
                                &op,
                                vec![Member {
                                    name: "outcome".to_owned(),
                                    value: Node::string(res.outcome.as_str()),
                                }],
                            ));
                        }
                        Err(e) => results.push(classify_err(&op, &e)),
                    }
                }

                "normalize_producer_instant" => {
                    let inp = opt_str(st, "input").unwrap_or_default();
                    match normalize_producer_instant(&inp) {
                        Ok(out) => results.push(ok_node(
                            &op,
                            vec![Member {
                                name: "formatted".to_owned(),
                                value: Node::string(out),
                            }],
                        )),
                        Err(e) => results.push(classify_err(&op, &e)),
                    }
                }
                "parse_instant" => {
                    let inp = opt_str(st, "input").unwrap_or_default();
                    match parse_instant(&inp) {
                        Ok(ts) => {
                            results.push(ok_node(
                                &op,
                                vec![Member {
                                    name: "formatted".to_owned(),
                                    value: Node::string(format_instant(ts)),
                                }],
                            ));
                        }
                        Err(e) => results.push(classify_err(&op, &e)),
                    }
                }

                "fs_write" => {
                    let b = decode_step_bytes(st)?;
                    let mode = opt_u32(st, "mode").unwrap_or(0o600);
                    let rel = opt_str(st, "rel").unwrap_or_default();
                    let p = resolve_dir(&store_dir, Some(&rel));
                    if let Some(parent) = p.parent() {
                        mkdir_all_0700(&store_dir, parent)?;
                    }
                    fs::write(&p, &b).map_err(|e| e.to_string())?;
                    set_mode_if_unix(&p, mode)?;
                    results.push(ok_node(&op, Vec::new()));
                }

                "fs_remove" => {
                    let rel = opt_str(st, "rel").unwrap_or_default();
                    let p = resolve_dir(&store_dir, Some(&rel));
                    if p.is_dir() {
                        let _ = fs::remove_dir_all(&p);
                    } else {
                        let _ = fs::remove_file(&p);
                    }
                    results.push(ok_node(&op, Vec::new()));
                }

                "fs_mkdir" => {
                    let rel = opt_str(st, "rel").unwrap_or_default();
                    let p = resolve_dir(&store_dir, Some(&rel));
                    mkdir_all_0700(&store_dir, &p)?;
                    set_mode_if_unix(&p, 0o700)?;
                    results.push(ok_node(&op, Vec::new()));
                }

                "hold_lock_and_put" => {
                    let he = handles
                        .get(&hname)
                        .ok_or_else(|| format!("unknown handle {hname:?}"))?;
                    let lock_path = store_dir.join("lock");
                    let lf = FsOpenOptions::new()
                        .read(true)
                        .write(true)
                        .create(true)
                        .truncate(false)
                        .open(&lock_path)
                        .map_err(|e| e.to_string())?;
                    let _ = set_mode_if_unix(&lock_path, 0o600);
                    let guard = FileLock::acquire(&lf, Duration::ZERO)
                        .map_err(|_| "failed to acquire external lock".to_owned())?;
                    let b = decode_step_bytes(st)?;
                    let media_type = opt_str(st, "media_type").unwrap_or_default();
                    let put_res = he.store.put_body(Cursor::new(b), &media_type);
                    let _ = guard.release();
                    drop(lf);
                    match put_res {
                        Ok(_) => results.push(ok_node(&op, Vec::new())),
                        Err(e) => results.push(classify_err(&op, &e)),
                    }
                }

                "probe_cleanup_err" => {
                    let io_err = io::Error::new(io::ErrorKind::PermissionDenied, "denied");
                    let err = Error::schema("simulated failure").with_cleanup_io("unlink", &io_err);
                    results.push(classify_err(&op, &err));
                }

                "open_stream" | "append" | "retire" | "protect" | "head" | "revisions"
                | "reconcile" | "release" => {
                    let he = handles
                        .get_mut(&hname)
                        .ok_or_else(|| format!("unknown handle {hname:?}"))?;
                    match run_stream_op(he, &op, st) {
                        Ok(data) => results.push(ok_node(
                            &op,
                            vec![Member {
                                name: "data".to_owned(),
                                value: data,
                            }],
                        )),
                        Err(e) => results.push(classify_err(&op, &e)),
                    }
                }

                other => return Err(format!("unsupported op {other:?}")),
            }
        }
        Ok(())
    })();

    for (_, h) in handles {
        let _ = h.store.close();
    }
    if cleanup {
        let _ = fs::remove_dir_all(&store_dir);
    }

    run_res?;

    Ok(Node::object(vec![
        Member {
            name: "id".to_owned(),
            value: Node::string(id),
        },
        Member {
            name: "results".to_owned(),
            value: Node::array(results),
        },
    ]))
}

fn main() {
    let stdin = io::stdin();
    let mut stdout = io::stdout();

    for line_res in stdin.lock().lines() {
        let line = match line_res {
            Ok(l) => l,
            Err(e) => {
                eprintln!("recordprims-driver: read stdin: {e}");
                std::process::exit(1);
            }
        };
        let trimmed = line.trim();
        if trimmed.is_empty() {
            continue;
        }
        let sc = match wire::parse_json(trimmed.as_bytes()) {
            Ok(n) => n,
            Err(e) => {
                eprintln!("recordprims-driver: parse scenario: {e}");
                std::process::exit(1);
            }
        };
        let out = match run_scenario(&sc) {
            Ok(n) => n,
            Err(e) => {
                eprintln!("recordprims-driver: run scenario: {e}");
                std::process::exit(1);
            }
        };
        let mut encoded = wire::encode(&out);
        encoded.push(b'\n');
        if let Err(e) = stdout.write_all(&encoded).and_then(|()| stdout.flush()) {
            eprintln!("recordprims-driver: write stdout: {e}");
            std::process::exit(1);
        }
    }
}

/// An object with members sorted by name, as RFC 8785 JCS orders them, so
/// the driver's output matches the Go driver's canonical data.
/// A pin object from a scenario step.
fn pin_of(n: &Node) -> Pin {
    Pin {
        stream: opt_str(n, "stream").unwrap_or_default(),
        record_id: opt_str(n, "record_id").unwrap_or_default(),
        revision: n
            .get("revision")
            .and_then(|v| v.token.parse().ok())
            .unwrap_or(0),
        digest: opt_str(n, "digest").unwrap_or_default(),
    }
}

/// Parses a JSON document returned by the library into sorted driver data.
fn raw_data(raw: &[u8]) -> Result<Node, Error> {
    let n = wire::parse_json(raw).map_err(|e| Error::Schema(e.to_string()))?;
    Ok(sorted(n))
}

fn obj(mut fields: Vec<(&str, Node)>) -> Node {
    fields.sort_by(|a, b| a.0.cmp(b.0));
    Node::object(
        fields
            .into_iter()
            .map(|(k, v)| Member {
                name: k.to_owned(),
                value: v,
            })
            .collect(),
    )
}

fn pin_data(p: &Pin) -> Node {
    obj(vec![
        ("stream", Node::string(&p.stream)),
        ("record_id", Node::string(&p.record_id)),
        ("revision", Node::uint(p.revision)),
        ("digest", Node::string(&p.digest)),
    ])
}

fn receipt_data(r: &Receipt) -> Node {
    let mut f = vec![
        ("v", Node::int(1)),
        ("kind", Node::string("receipt")),
        ("stream", Node::string(&r.stream)),
        ("idempotency_key", Node::string(&r.idempotency_key)),
        ("outcome", Node::string(r.outcome.as_str())),
    ];
    if let Some(seq) = r.seq.filter(|s| *s != 0) {
        f.push(("seq", Node::uint(seq)));
    }
    if let Some(p) = &r.pin {
        f.push(("pin", pin_data(p)));
    }
    if let Some(b) = r.body_status.as_deref().filter(|b| !b.is_empty()) {
        f.push(("body_status", Node::string(b)));
    }
    if let Some(fm) = r.failure_model.as_deref().filter(|b| !b.is_empty()) {
        f.push(("failure_model", Node::string(fm)));
    }
    if let Some(p) = &r.current {
        f.push(("current", pin_data(p)));
    }
    obj(f)
}

/// Re-sorts every object in `n` by member name.
fn sorted(n: Node) -> Node {
    match n.kind {
        Kind::Object => {
            let mut fields: Vec<Member> = n
                .fields
                .into_iter()
                .map(|m| Member {
                    name: m.name,
                    value: sorted(m.value),
                })
                .collect();
            fields.sort_by(|a, b| a.name.cmp(&b.name));
            Node::object(fields)
        }
        Kind::Array => Node::array(n.elems.into_iter().map(sorted).collect()),
        _ => n,
    }
}

struct CollectSink {
    frames: Vec<Node>,
}

impl ReleaseSink for CollectSink {
    fn frame(&mut self, h: &FrameHeader, r: &mut FrameReader<'_>) -> Result<(), Error> {
        let mut b = Vec::new();
        io::Read::read_to_end(r, &mut b).map_err(|e| Error::io("read", &e, false))?;
        let hex: String = b.iter().map(|x| format!("{x:02x}")).collect();
        self.frames.push(obj(vec![
            ("index", Node::uint(h.index as u64)),
            (
                "kind",
                Node::string(match h.kind {
                    FrameKind::Record => "record",
                    FrameKind::Body => "body",
                }),
            ),
            ("length", Node::uint(h.length)),
            ("digest", Node::string(&h.digest)),
            ("pin", pin_data(&h.pin)),
            ("bytes_hex", Node::string(hex)),
        ]));
        Ok(())
    }
}

fn run_stream_op(he: &mut HandleEntry, op: &str, st: &Node) -> Result<Node, Error> {
    let stream = opt_str(st, "stream").unwrap_or_default();
    if op == "open_stream" {
        let mode = match opt_str(st, "stream_mode").as_deref() {
            Some("single_revision") => Mode::SingleRevision,
            Some("revisioned") => Mode::Revisioned,
            _ => return Err(Error::Schema("invalid stream mode".to_owned())),
        };
        let s = he.store.open_stream(&stream, mode, None)?;
        he.streams.insert(stream, mode);
        return Ok(obj(vec![
            ("id", Node::string(s.id())),
            ("mode", Node::string(s.mode().as_str())),
        ]));
    }
    let Some(mode) = he.streams.get(&stream).copied() else {
        return Err(Error::Schema(format!(
            "stream {stream:?} not opened in this scenario"
        )));
    };
    let s = he.store.open_stream(&stream, mode, None)?;
    match op {
        "append" => {
            let record = st.get("record").map(wire::encode).unwrap_or_default();
            let w = RecordWrite {
                record,
                idempotency_key: opt_str(st, "key").unwrap_or_default(),
                ..RecordWrite::default()
            };
            Ok(receipt_data(&s.append(&w)?))
        }
        "retire" => {
            let reason = match opt_str(st, "reason").as_deref() {
                Some("superseded") => RetireReason::Superseded,
                Some("withdrawn") => RetireReason::Withdrawn,
                Some("retention") => RetireReason::Retention,
                Some("operator") => RetireReason::Operator,
                _ => return Err(Error::Schema("invalid retire reason".to_owned())),
            };
            // `"key": ""` reaches the API as `Some("")`, as the Go driver
            // passes `WithRetireKey("")` under `retire_with_empty_key`.
            let key = opt_str(st, "key");
            let rid = opt_str(st, "record_id").unwrap_or_default();
            let r = s.retire(&rid, reason, key.as_deref())?;
            let mut f = vec![("outcome", Node::string(r.outcome.as_str()))];
            if let Some(seq) = r.seq {
                f.push(("seq", Node::uint(seq)));
            }
            if let Some(at) = &r.at {
                f.push(("at", Node::string(at)));
            }
            if let Some(raw) = &r.receipt {
                f.push(("receipt", raw_data(raw)?));
            }
            Ok(obj(f))
        }
        "protect" => {
            let raw = st.get("request").map(wire::encode).unwrap_or_default();
            let r = s.protect(&ProtectionRequest { raw })?;
            let mut f = vec![
                ("outcome", Node::string(&r.outcome)),
                ("receipt", raw_data(&r.raw)?),
            ];
            if let Some(seq) = r.seq {
                f.push(("seq", Node::uint(seq)));
            }
            if let Some(at) = &r.at {
                f.push(("at", Node::string(at)));
            }
            if let Some(reason) = &r.reason {
                f.push(("reason", Node::string(reason)));
            }
            Ok(obj(f))
        }
        "head" => {
            let rid = opt_str(st, "record_id").unwrap_or_default();
            Ok(match s.head(&rid)? {
                None => obj(vec![("found", Node::boolean(false))]),
                Some(h) => obj(vec![
                    ("found", Node::boolean(true)),
                    ("pin", pin_data(&h.pin)),
                    ("retired", Node::boolean(h.retired)),
                ]),
            })
        }
        "revisions" => {
            let rid = opt_str(st, "record_id").unwrap_or_default();
            let revs = s
                .revisions(&rid)?
                .into_iter()
                .map(|rv| {
                    let mut f = vec![
                        ("pin", pin_data(&rv.pin)),
                        ("payload_contract", Node::string(&rv.payload_contract)),
                        ("has_body", Node::boolean(rv.has_body)),
                        ("retired", Node::boolean(rv.retired)),
                        ("at", Node::string(&rv.at)),
                    ];
                    if let Some(sup) = &rv.supersedes {
                        f.push(("supersedes", pin_data(sup)));
                    }
                    obj(f)
                })
                .collect();
            Ok(obj(vec![("revisions", Node::array(revs))]))
        }
        "reconcile" => {
            let att = parse_instant(&opt_str(st, "attempted_at").unwrap_or_default())?;
            let r = s.reconcile(&opt_str(st, "key").unwrap_or_default(), att)?;
            let mut f = vec![
                ("v", Node::int(1)),
                ("kind", Node::string("reconciliation")),
                ("result", Node::string(&r.result)),
                ("stream", Node::string(&r.stream)),
                ("idempotency_key", Node::string(&r.idempotency_key)),
                ("attempted_at", Node::string(&r.attempted_at)),
            ];
            if let Some(reason) = &r.reason {
                f.push(("reason", Node::string(reason)));
            }
            if let Some(raw) = &r.receipt {
                let n = wire::parse_json(raw).map_err(|e| Error::Schema(e.to_string()))?;
                f.push(("receipt", sorted(n)));
            }
            Ok(obj(f))
        }
        "release" => {
            let purpose = match opt_str(st, "purpose").as_deref() {
                Some("serve") => UsePurpose::Serve,
                Some("export") => UsePurpose::Export,
                Some("reuse") => UsePurpose::Reuse,
                _ => return Err(Error::Schema("invalid release purpose".to_owned())),
            };
            let closure = match opt_str(st, "closure").as_deref() {
                None | Some("") | Some("full_content") => UseClosure::FullContent,
                Some("metadata_only") => UseClosure::MetadataOnly,
                _ => return Err(Error::Schema("invalid release closure".to_owned())),
            };
            let mut sink = CollectSink { frames: Vec::new() };
            let res = s.release(
                &UseRequest {
                    purpose,
                    selection: st
                        .get("selection")
                        .map(|a| a.elems.iter().map(pin_of).collect())
                        .unwrap_or_default(),
                    closure,
                },
                Some(&mut sink),
            )?;
            if let Some(e) = res.sink_error {
                return Err(e);
            }
            let mut f = vec![
                (
                    "outcome",
                    Node::string(match res.outcome {
                        ReleaseOutcome::Released => "released",
                        ReleaseOutcome::Partial => "released_partial",
                        ReleaseOutcome::Uncertain => "released_uncertain",
                        ReleaseOutcome::Refused => "refused",
                    }),
                ),
                ("cut", Node::uint(res.cut)),
                (
                    "selection",
                    Node::array(res.selection.iter().map(pin_data).collect()),
                ),
                (
                    "omitted",
                    Node::array(res.omitted.iter().map(pin_data).collect()),
                ),
                ("frames_completed", Node::uint(res.frames_completed as u64)),
                ("frames", Node::array(sink.frames)),
            ];
            if let Some(r) = &res.refusal_reason {
                f.push(("refusal_reason", Node::string(r)));
            }
            Ok(obj(f))
        }
        other => Err(Error::Schema(format!("unsupported stream op {other:?}"))),
    }
}
