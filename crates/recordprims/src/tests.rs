// SPDX-License-Identifier: MIT OR Apache-2.0

//! Unit tests for `crates/recordprims`, mirroring `store_test.go`, `body_test.go`,
//! `body_unix_test.go`, `internal/fsroot/fsroot_test.go`, `internal/lock/lock_test.go`,
//! and `internal/jfile/jfile_test.go`.

use std::fs;
use std::io::{self, Cursor, Read};
use std::panic::{catch_unwind, AssertUnwindSafe};
use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use sha2::{Digest as _, Sha256};

use crate::body::{
    body_file, digest_hex, BodyStateKind, Provenance, PublicationLease, RemoveBodyOutcome,
    RemoveBodyReason, RemoveBodyRequest, DEFAULT_PENDING_BODY_TTL, DIR_BODY_INCARNATION,
    DIR_BODY_SHA256, DIR_BODY_TMP,
};
use crate::error::{is_indeterminate, Error, ErrorCode};
use crate::fsroot::{self, FsRootError, Root};
use crate::instant::{format_instant, parse_instant, Clock, Instant};
use crate::jfile::{Appender, JFileError};
use crate::lock::FileLock;
#[cfg(unix)]
use crate::store::FILE_DIAG_KEY;
use crate::store::{
    hex_encode, init, open, BodyRef, InitOptions, OpenOptions, Store, DEFAULT_BODY_REMOVAL_STREAM,
    DEFAULT_IDEMPOTENCY_WINDOW, FILE_ATTEMPT_KEY, FILE_STORE, KEY_LEN,
};
use crate::wire::MAX_SAFE_INTEGER;

static TEST_COUNTER: AtomicU64 = AtomicU64::new(0);

fn test_fs() -> fsroot::Options {
    fsroot::Options {
        allow_tmpfs: true,
        allow_overlay: true,
    }
}

struct TempStoreDir {
    path: PathBuf,
}

impl TempStoreDir {
    fn new() -> Self {
        let mut raw = [0u8; 8];
        getrandom::fill(&mut raw).expect("random");
        let n = TEST_COUNTER.fetch_add(1, Ordering::SeqCst);
        let dir = std::env::temp_dir().join(format!(
            "recordprims-rust-test-{}-{}-{}",
            std::process::id(),
            n,
            hex_encode(&raw)
        ));
        fs::create_dir_all(&dir).expect("create temp dir");
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            fs::set_permissions(&dir, fs::Permissions::from_mode(0o700)).expect("chmod 0700");
        }
        Self { path: dir }
    }

    fn path(&self) -> &Path {
        &self.path
    }
}

impl Drop for TempStoreDir {
    fn drop(&mut self) {
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            let _ = fs::set_permissions(&self.path, fs::Permissions::from_mode(0o700));
            let tmp_dir = self.path.join("bodies").join("tmp");
            let _ = fs::set_permissions(&tmp_dir, fs::Permissions::from_mode(0o700));
        }
        let _ = fs::remove_dir_all(&self.path);
    }
}

struct StepClock {
    now: Mutex<Instant>,
}

impl StepClock {
    fn new(initial: Instant) -> Arc<Self> {
        Arc::new(Self {
            now: Mutex::new(initial),
        })
    }

    fn advance(&self, d: Duration) {
        let mut g = self.now.lock().unwrap();
        *g = g.saturating_add(d);
    }
}

impl Clock for StepClock {
    fn now(&self) -> Instant {
        *self.now.lock().unwrap()
    }
}

fn open_body_store() -> (Store, TempStoreDir, Arc<StepClock>) {
    let td = TempStoreDir::new();
    let t0 = parse_instant("2026-10-25T12:00:00.000000Z").unwrap();
    let clk = StepClock::new(t0);
    init(
        td.path(),
        InitOptions {
            clock: Some(clk.clone()),
            fs: test_fs(),
            ..Default::default()
        },
    )
    .expect("init");
    let store = open(
        td.path(),
        OpenOptions {
            clock: Some(clk.clone()),
            fs: test_fs(),
            ..Default::default()
        },
    )
    .expect("open");
    (store, td, clk)
}

fn digest_of(b: &[u8]) -> String {
    let sum = Sha256::digest(b);
    format!("sha256:{}", hex_encode(&sum))
}

fn put(s: &Store, b: &[u8]) -> (BodyRef, PublicationLease) {
    s.put_body(Cursor::new(b), "application/octet-stream")
        .expect("put_body")
}

fn state_of(s: &Store, b: &[u8]) -> BodyStateKind {
    s.with_lock(|| {
        let desig = s.read_designated_state()?;
        let d = digest_of(b);
        let st = s.eval_body_state(&desig, digest_hex(&d), b.len() as i64)?;
        Ok(st.kind)
    })
    .expect("state_of")
}

fn test_prov() -> Provenance {
    Provenance {
        actor_kind: "process".into(),
        actor: "test".into(),
        ..Default::default()
    }
}

fn remove_req(b: &[u8]) -> RemoveBodyRequest {
    RemoveBodyRequest {
        digest: digest_of(b),
        length: b.len() as i64,
        complete_incarnation: None,
        reason: RemoveBodyReason::Retention,
        provenance: test_prov(),
        authorization: None,
    }
}

fn remove(s: &Store, b: &[u8]) -> RemoveBodyOutcome {
    s.remove_body(&remove_req(b)).expect("remove_body").outcome
}

fn expire(clk: &StepClock) {
    clk.advance(DEFAULT_PENDING_BODY_TTL + Duration::from_secs(1));
}

fn fail_at(s: &mut Store, phase: &'static str) {
    let fired = Arc::new(AtomicBool::new(false));
    s.hook = Some(Arc::new(move |p| {
        if p == phase && !fired.swap(true, Ordering::SeqCst) {
            return Err(Error::Io {
                op: "write",
                class: crate::error::IoClass::Other,
                indeterminate: false,
            });
        }
        Ok(())
    }));
}

fn set_incarnation(s: &Store, b: &[u8], n: u64) {
    s.with_lock(|| {
        s.ensure_body_dirs()?;
        let d = digest_of(b);
        s.write_incarnation(digest_hex(&d), n)
    })
    .expect("set_incarnation");
}

#[test]
fn test_init_open_round_trip() {
    let (s, _td, _clk) = open_body_store();
    assert_eq!(s.store_id().len(), 32);
    assert_eq!(s.idempotency_window(), DEFAULT_IDEMPOTENCY_WINDOW);
    assert_eq!(s.attempt_key.len(), KEY_LEN);
    assert_eq!(s.diag_key.len(), KEY_LEN);
    let prof = s.profile();
    assert_eq!(prof.runtime.stratum, "journal-files");
    assert_eq!(
        prof.runtime.body_removal_stream,
        DEFAULT_BODY_REMOVAL_STREAM
    );
    assert_eq!(prof.runtime.commit_strategy, "fsync_file_and_dir");
}

#[test]
fn test_open_refuses_v1_store_format() {
    let (s, td, _) = open_body_store();
    drop(s);
    let p = td.path().join(FILE_STORE);
    fs::write(
        &p,
        r#"{"format":"recordprims-fs/v1","store_id":"0123456789abcdef0123456789abcdef","key_generation":1,"created_at":"2026-11-10T00:00:00.000000Z","idempotency_window_ms":60000}"#,
    )
    .unwrap();
    let err = open(
        td.path(),
        OpenOptions {
            fs: test_fs(),
            ..Default::default()
        },
    )
    .err()
    .expect("want error");
    assert_eq!(err.code(), ErrorCode::StoreInvalid);
    assert!(
        err.to_string().contains("pre-journal store format"),
        "got {err}"
    );
}

#[test]
fn test_init_records_custom_window_and_refuses_non_empty() {
    let td = TempStoreDir::new();
    init(
        td.path(),
        InitOptions {
            idempotency_window: Some(Duration::from_secs(60)),
            fs: test_fs(),
            ..Default::default()
        },
    )
    .unwrap();
    let s = open(
        td.path(),
        OpenOptions {
            fs: test_fs(),
            ..Default::default()
        },
    )
    .unwrap();
    assert_eq!(s.idempotency_window(), Duration::from_secs(60));

    let err = init(
        td.path(),
        InitOptions {
            fs: test_fs(),
            ..Default::default()
        },
    )
    .unwrap_err();
    assert_eq!(err.code(), ErrorCode::StoreInvalid);
}

#[test]
fn test_store_ids_are_unique_and_key_tampering_refused() {
    let (a, td_a, _) = open_body_store();
    let (b, _td_b, _) = open_body_store();
    assert_ne!(a.store_id(), b.store_id());
    assert_ne!(a.attempt_key, b.attempt_key);
    drop(a);

    fs::write(td_a.path().join(FILE_ATTEMPT_KEY), b"short").unwrap();
    let err = open(
        td_a.path(),
        OpenOptions {
            fs: test_fs(),
            ..Default::default()
        },
    )
    .err()
    .unwrap();
    assert_eq!(err.code(), ErrorCode::StoreInvalid);
}

#[cfg(unix)]
#[test]
fn test_open_refuses_permissive_key_and_hardlinked_file() {
    use std::os::unix::fs::PermissionsExt;
    let (s, td, _) = open_body_store();
    drop(s);
    let diag_path = td.path().join(FILE_DIAG_KEY);
    fs::set_permissions(&diag_path, fs::Permissions::from_mode(0o644)).unwrap();
    let err = open(
        td.path(),
        OpenOptions {
            fs: test_fs(),
            ..Default::default()
        },
    )
    .err()
    .unwrap();
    assert_eq!(err.code(), ErrorCode::StoreInvalid);
    fs::set_permissions(&diag_path, fs::Permissions::from_mode(0o600)).unwrap();

    // Hardlink check: link attempt.key to another name inside the store root.
    let link_path = td.path().join("extra.link");
    fs::hard_link(td.path().join(FILE_ATTEMPT_KEY), &link_path).unwrap();
    let err = open(
        td.path(),
        OpenOptions {
            fs: test_fs(),
            ..Default::default()
        },
    )
    .err()
    .unwrap();
    assert_eq!(err.code(), ErrorCode::StoreInvalid);
}

#[test]
fn test_errors_carry_no_path() {
    let td = TempStoreDir::new();
    let err = open(
        td.path(),
        OpenOptions {
            fs: test_fs(),
            ..Default::default()
        },
    )
    .err()
    .unwrap();
    let msg = err.to_string();
    let dir_str = td.path().to_string_lossy();
    let base_str = td.path().file_name().unwrap().to_string_lossy();
    assert!(
        !msg.contains(dir_str.as_ref()) && !msg.contains(base_str.as_ref()),
        "error discloses path: {msg}"
    );
}

#[test]
fn test_with_lock_is_exclusive_between_handles() {
    let (a, td, _) = open_body_store();
    let b = open(
        td.path(),
        OpenOptions {
            fs: test_fs(),
            ..Default::default()
        },
    )
    .unwrap();
    let (tx_held, rx_held) = std::sync::mpsc::channel();
    let (tx_done, rx_done) = std::sync::mpsc::channel();
    let handle = std::thread::spawn(move || {
        a.with_lock(|| {
            tx_held.send(()).unwrap();
            rx_done.recv().unwrap();
            Ok(())
        })
        .unwrap();
    });
    rx_held.recv().unwrap();
    let err = b.with_lock(|| Ok(())).unwrap_err();
    assert_eq!(err.code(), ErrorCode::Locked);
    tx_done.send(()).unwrap();
    handle.join().unwrap();
}

#[test]
fn test_parse_instant_canonical_spelling() {
    let cases = [
        ("2026-10-20T00:00:01.000000Z", true),
        ("2026-10-25T13:00:00.123456Z", true),
        ("2024-02-29T12:00:00.000000Z", true),
        ("0001-01-01T00:00:00.000000Z", true),
        ("9999-12-31T23:59:59.999999Z", true),
        ("2026-10-20T00:00:03Z", false),
        ("2026-10-20T00:00:01.25Z", false),
        ("2026-10-20T00:00:03.0000000Z", false),
        ("2026-10-25T13:00:00.1234567Z", false),
        ("2026-10-20T00:00:01.000000+00:00", false),
        ("2026-10-20t00:00:01.000000Z", false),
        ("2026-10-20T00:00:01.000000z", false),
        ("2026-10-20 00:00:01.000000Z", false),
        ("2026-10-20T00:00:01,000000Z", false),
        (" 2026-10-20T00:00:01.000000Z", false),
        ("2026-10-20T00:00:01.000000Z\n", false),
        ("2026-02-30T00:00:01.000000Z", false),
        ("2026-02-29T00:00:01.000000Z", false),
        ("2026-10-20T24:00:00.000000Z", false),
        ("2026-10-20T23:60:00.000000Z", false),
        ("2026-12-31T23:59:60.000000Z", false),
        ("2026-13-01T00:00:00.000000Z", false),
        ("2026-00-01T00:00:00.000000Z", false),
        ("2026-10-00T00:00:00.000000Z", false),
        ("0000-01-01T00:00:00.000000Z", false),
        ("\u{ff12}026-10-20T00:00:01.000000Z", false),
        ("", false),
    ];
    for (inp, ok) in cases {
        match parse_instant(inp) {
            Ok(inst) => {
                assert!(ok, "expected {inp:?} to be rejected");
                assert_eq!(format_instant(inst), inp);
            }
            Err(e) => {
                assert!(!ok, "expected {inp:?} to be accepted, got {e}");
                assert_eq!(e.code(), ErrorCode::Schema);
            }
        }
    }
}

#[test]
fn test_put_body_publishes_available_with_lease_and_empty_body() {
    let (s, td, _) = open_body_store();
    let b = b"hello body";
    let (r, l) = put(&s, b);
    assert_eq!(r.digest, digest_of(b));
    assert_eq!(r.length, b.len() as i64);
    assert_eq!(r.media_type, "application/octet-stream");
    assert_eq!(l.digest, r.digest);
    assert_eq!(l.id.len(), 32);
    assert_eq!(state_of(&s, b), BodyStateKind::Available);

    let h = digest_hex(&r.digest);
    let stored = fs::read(
        td.path()
            .join("bodies")
            .join("sha256")
            .join(&h[..2])
            .join(h),
    )
    .unwrap();
    assert_eq!(stored, b);
    let tmp_entries: Vec<_> = fs::read_dir(td.path().join("bodies").join("tmp"))
        .unwrap()
        .collect();
    assert!(tmp_entries.is_empty());

    let (empty_ref, _) = put(&s, b"");
    assert_eq!(empty_ref.length, 0);
    assert_eq!(empty_ref.digest, digest_of(b""));
}

#[test]
fn test_put_body_refuses_bad_media_type() {
    let (s, _td, _clk) = open_body_store();
    for mt in ["", "text", "Text/plain", "text/plain; charset=utf-8"] {
        let err = s.put_body(Cursor::new(b"x"), mt).unwrap_err();
        assert_eq!(err.code(), ErrorCode::Schema);
    }
}

#[test]
fn test_put_body_twice_keeps_incarnation_and_issues_second_lease() {
    let (s, _td, _clk) = open_body_store();
    let b = b"same";
    let (_, l1) = put(&s, b);
    let (_, l2) = put(&s, b);
    assert_ne!(l1.id, l2.id);
    let d = digest_of(b);
    let (n, _) = s.read_incarnation(digest_hex(&d)).unwrap();
    assert_eq!(n, 1);
}

#[test]
fn test_remove_body_lifecycle_and_leases() {
    let (s, td, clk) = open_body_store();
    assert_eq!(remove(&s, b"never"), RemoveBodyOutcome::Absent);

    let mut req = remove_req(b"never");
    req.complete_incarnation = Some(1);
    assert_eq!(
        s.remove_body(&req).unwrap().outcome,
        RemoveBodyOutcome::Superseded
    );

    let b = b"leased";
    put(&s, b);
    assert_eq!(remove(&s, b), RemoveBodyOutcome::Pending);
    expire(&clk);
    assert_eq!(remove(&s, b), RemoveBodyOutcome::Removed);
    assert_eq!(state_of(&s, b), BodyStateKind::Removed);
    assert_eq!(remove(&s, b), RemoveBodyOutcome::Absent);

    // Republish -> incarnation 2
    put(&s, b);
    expire(&clk);
    let mut old_req = remove_req(b);
    old_req.complete_incarnation = Some(1);
    assert_eq!(
        s.remove_body(&old_req).unwrap().outcome,
        RemoveBodyOutcome::Superseded
    );
    assert_eq!(remove(&s, b), RemoveBodyOutcome::Removed);

    // ProtectionUnknown when an unreadable/empty stream dir exists
    let b2 = b"prot";
    put(&s, b2);
    expire(&clk);
    fs::create_dir_all(td.path().join("streams").join("0123")).unwrap();
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        fs::set_permissions(
            td.path().join("streams").join("0123"),
            fs::Permissions::from_mode(0o700),
        )
        .unwrap();
    }
    assert_eq!(remove(&s, b2), RemoveBodyOutcome::ProtectionUnknown);
}

#[test]
fn test_corrupt_and_missing_body_detection() {
    let (s, td, clk) = open_body_store();
    let b = b"pristine";
    let (r, _) = put(&s, b);
    expire(&clk);
    let h = digest_hex(&r.digest);
    let p = td
        .path()
        .join("bodies")
        .join("sha256")
        .join(&h[..2])
        .join(h);
    fs::write(&p, b"tampered").unwrap();
    assert_eq!(state_of(&s, b), BodyStateKind::Corrupt);
    assert_eq!(remove(&s, b), RemoveBodyOutcome::Conflict);
    let err = s.put_body(Cursor::new(b), "text/plain").unwrap_err();
    assert_eq!(err.code(), ErrorCode::StoreInvalid);

    let (s2, td2, clk2) = open_body_store();
    let b2 = b"vanishes";
    let (r2, _) = put(&s2, b2);
    expire(&clk2);
    let h2 = digest_hex(&r2.digest);
    fs::remove_file(
        td2.path()
            .join("bodies")
            .join("sha256")
            .join(&h2[..2])
            .join(h2),
    )
    .unwrap();
    assert_eq!(state_of(&s2, b2), BodyStateKind::IntegrityLoss);
    assert_eq!(remove(&s2, b2), RemoveBodyOutcome::Missing);
}

#[test]
fn test_crash_recovery_and_fault_hooks() {
    let (mut s, td, clk) = open_body_store();
    let b = b"half removed";
    put(&s, b);
    expire(&clk);
    fail_at(&mut s, "remove.tombstone_fsynced");
    let err = s.remove_body(&remove_req(b)).unwrap_err();
    assert_eq!(err.code(), ErrorCode::BodyIncomplete);
    assert_eq!(err.body_incomplete_state(), Some("removal_pending"));
    assert!(is_indeterminate(&err));
    assert_eq!(remove(&s, b), RemoveBodyOutcome::Removed);

    // Put after removal_started advances incarnation
    let b2 = b"resurrected";
    put(&s, b2);
    expire(&clk);
    fail_at(&mut s, "remove.tombstone_fsynced");
    let _ = s.remove_body(&remove_req(b2));
    s.hook = None;
    put(&s, b2);
    let d2 = digest_of(b2);
    let h2 = digest_hex(&d2);
    assert_eq!(s.read_incarnation(h2).unwrap().0, 2);
    assert_eq!(state_of(&s, b2), BodyStateKind::Available);

    // Deleted counter uses journal bound + 1
    expire(&clk);
    assert_eq!(remove(&s, b2), RemoveBodyOutcome::Removed);
    fs::remove_file(td.path().join("bodies").join("incarnation").join(h2)).unwrap();
    put(&s, b2);
    assert_eq!(s.read_incarnation(h2).unwrap().0, 3);

    // Crash between rename and body_published leaves publishing; retry completes
    let b3 = b"half published";
    fail_at(&mut s, "body.dir_fsynced");
    let err = s.put_body(Cursor::new(b3), "text/plain").unwrap_err();
    assert_eq!(err.body_incomplete_state(), Some("publishing"));
    assert_eq!(remove(&s, b3), RemoveBodyOutcome::Pending);
    let phases = Arc::new(Mutex::new(Vec::new()));
    let phases_clone = phases.clone();
    s.hook = Some(Arc::new(move |p| {
        phases_clone.lock().unwrap().push(p.to_owned());
        Ok(())
    }));
    put(&s, b3);
    assert_eq!(state_of(&s, b3), BodyStateKind::Available);
    let d3 = digest_of(b3);
    assert_eq!(s.read_incarnation(digest_hex(&d3)).unwrap().0, 1);
    let recorded = phases.lock().unwrap().clone();
    let pub_idx = recorded
        .iter()
        .position(|x| x == "body.published_written")
        .unwrap();
    let lease_idx = recorded
        .iter()
        .position(|x| x == "body.lease_written")
        .unwrap();
    assert!(pub_idx < lease_idx);
}

#[test]
fn test_incarnation_exhaustion_and_ceiling() {
    let (s, _td, _clk) = open_body_store();
    let b = b"exhausted";
    set_incarnation(&s, b, MAX_SAFE_INTEGER as u64);
    assert_eq!(state_of(&s, b), BodyStateKind::Unpublished);
    let err = s.put_body(Cursor::new(b), "text/plain").unwrap_err();
    assert_eq!(err.code(), ErrorCode::Schema);
    assert!(!is_indeterminate(&err));

    let b_last = b"last";
    set_incarnation(&s, b_last, (MAX_SAFE_INTEGER as u64) - 1);
    put(&s, b_last);
    let d_last = digest_of(b_last);
    assert_eq!(
        s.read_incarnation(digest_hex(&d_last)).unwrap().0,
        MAX_SAFE_INTEGER as u64
    );
    // Republishing live body at ceiling succeeds without increment
    put(&s, b_last);
    assert_eq!(state_of(&s, b_last), BodyStateKind::Available);
}

#[test]
fn test_put_body_refuses_length_above_ceiling() {
    let (s, _td, _clk) = open_body_store();
    s.max_body_length.store(4, Ordering::SeqCst);
    let res_ok = s.put_body(Cursor::new(b"1234"), "text/plain");
    let res_err = s.put_body(Cursor::new(b"12345"), "text/plain");
    s.max_body_length.store(MAX_SAFE_INTEGER, Ordering::SeqCst);

    res_ok.expect("at ceiling should succeed");
    let err = res_err.unwrap_err();
    assert_eq!(err.code(), ErrorCode::Schema);
    assert!(!is_indeterminate(&err));
    assert_eq!(state_of(&s, b"12345"), BodyStateKind::Unpublished);
}

struct PanicReader {
    remaining: usize,
}

impl Read for PanicReader {
    fn read(&mut self, buf: &mut [u8]) -> io::Result<usize> {
        if self.remaining == 0 {
            panic!("reader panic");
        }
        let k = buf.len().min(self.remaining);
        buf[..k].fill(b'x');
        self.remaining -= k;
        Ok(k)
    }
}

#[test]
fn test_put_body_panic_removes_staged_file() {
    let b = b"panic body";
    for phase in [
        "reader",
        "body.tmp_written",
        "body.tmp_fsynced",
        "body.incarnation_written",
        "body.renamed",
    ] {
        let (mut s, _td, _clk) = open_body_store();
        if phase != "reader" {
            let target = phase.to_owned();
            s.hook = Some(Arc::new(move |p| {
                if p == target {
                    panic!("hook panic");
                }
                Ok(())
            }));
        }
        let panicked = catch_unwind(AssertUnwindSafe(|| {
            if phase == "reader" {
                let _ = s.put_body(
                    PanicReader {
                        remaining: 100 << 10,
                    },
                    "application/octet-stream",
                );
            } else {
                let _ = s.put_body(Cursor::new(b), "application/octet-stream");
            }
        }));
        assert!(panicked.is_err(), "expected panic at {phase}");
        s.hook = None;
        let names = s.root.read_dir_names(DIR_BODY_TMP, s.fso).unwrap();
        assert!(
            names.is_empty(),
            "staged files left after panic at {phase}: {names:?}"
        );
        put(&s, b);
        assert_eq!(state_of(&s, b), BodyStateKind::Available);
    }
}

#[cfg(unix)]
#[test]
fn test_staging_cleanup_failure_reporting() {
    use std::os::unix::fs::PermissionsExt;
    if rustix::process::geteuid().is_root() {
        return;
    }

    // 1. Determinate refusal after staging + failed cleanup -> Schema with unlink IoError
    let (mut s, td, _) = open_body_store();
    let b = b"orphan";
    set_incarnation(&s, b, MAX_SAFE_INTEGER as u64);
    let tmp_dir = td.path().join("bodies").join("tmp");
    let tmp_dir_clone = tmp_dir.clone();
    s.hook = Some(Arc::new(move |p| {
        if p == "body.tmp_fsynced" {
            fs::set_permissions(&tmp_dir_clone, fs::Permissions::from_mode(0o500)).unwrap();
        }
        Ok(())
    }));
    let err = s.put_body(Cursor::new(b), "text/plain").unwrap_err();
    s.hook = None;
    fs::set_permissions(&tmp_dir, fs::Permissions::from_mode(0o700)).unwrap();
    assert_eq!(err.code(), ErrorCode::Schema);
    assert!(!is_indeterminate(&err));
    assert_eq!(err.io_error().map(|(op, _)| op), Some("unlink"));
    assert_eq!(s.root.read_dir_names(DIR_BODY_TMP, s.fso).unwrap().len(), 1);

    // 2. Indeterminate failure after durable step + failed cleanup -> indeterminate BodyIncomplete with unlink IoError
    let (mut s2, td2, _) = open_body_store();
    let b2 = b"mid publication";
    let tmp_dir2 = td2.path().join("bodies").join("tmp");
    let tmp_dir2_clone = tmp_dir2.clone();
    s2.hook = Some(Arc::new(move |p| {
        if p == "body.incarnation_fsynced" {
            fs::set_permissions(&tmp_dir2_clone, fs::Permissions::from_mode(0o500)).unwrap();
            return Err(Error::Io {
                op: "fsync",
                class: crate::error::IoClass::Other,
                indeterminate: true,
            });
        }
        Ok(())
    }));
    let err2 = s2.put_body(Cursor::new(b2), "text/plain").unwrap_err();
    s2.hook = None;
    fs::set_permissions(&tmp_dir2, fs::Permissions::from_mode(0o700)).unwrap();
    assert_eq!(err2.code(), ErrorCode::BodyIncomplete);
    assert!(is_indeterminate(&err2));
    assert_eq!(err2.io_error().map(|(op, _)| op), Some("unlink"));
    put(&s2, b2);
    assert_eq!(state_of(&s2, b2), BodyStateKind::Available);
}

#[test]
fn test_fsroot_lock_and_jfile_primitives() {
    let td = TempStoreDir::new();
    let root = Root::open(td.path(), test_fs()).unwrap();

    // Path traversal refused
    assert!(matches!(
        root.open_verified("../escape", test_fs()),
        Err(FsRootError::Path)
    ));
    assert!(matches!(
        root.open_verified("/abs", test_fs()),
        Err(FsRootError::Path)
    ));

    // Lock acquisition and contention
    let f1 = root.open_lock_file("lock", test_fs()).unwrap();
    let f2 = root.open_lock_file("lock", test_fs()).unwrap();
    let g1 = FileLock::acquire(&f1, Duration::ZERO).unwrap();
    assert!(FileLock::acquire(&f2, Duration::ZERO).is_err());
    g1.release().unwrap();
    let g2 = FileLock::acquire(&f2, Duration::ZERO).unwrap();
    g2.release().unwrap();

    // JFile cut-verified append
    let app = Appender::new(test_fs());
    let jf = root
        .create_exclusive("journal.ndjson", 0o600, test_fs())
        .unwrap();
    jf.sync_all().unwrap();
    drop(jf);
    app.append(&root, "journal.ndjson", 0, b"{\"seq\":1}\n", true)
        .unwrap();
    assert!(matches!(
        app.append(&root, "journal.ndjson", 0, b"{\"seq\":2}\n", true),
        Err(JFileError::CutMismatch)
    ));
    app.append(&root, "journal.ndjson", 10, b"{\"seq\":2}\n", true)
        .unwrap();
    let _ = DIR_BODY_SHA256;
    let _ = DIR_BODY_INCARNATION;
    let _ = body_file("0000");
}

#[cfg(unix)]
#[test]
fn test_unix_fifo_refused_without_blocking() {
    let td = TempStoreDir::new();
    let root = Root::open(td.path(), test_fs()).unwrap();
    let fifo_path = td.path().join("pipe.fifo");
    let status = std::process::Command::new("mkfifo")
        .arg(&fifo_path)
        .status()
        .expect("run mkfifo");
    assert!(status.success(), "mkfifo failed");

    assert!(matches!(
        root.open_verified("pipe.fifo", test_fs()),
        Err(FsRootError::NotRegular)
    ));
    assert!(matches!(
        root.open_append("pipe.fifo", test_fs()),
        Err(FsRootError::NotRegular)
    ));
    assert!(matches!(
        root.open_write("pipe.fifo", test_fs()),
        Err(FsRootError::NotRegular)
    ));
    assert!(matches!(
        root.open_lock_file("pipe.fifo", test_fs()),
        Err(FsRootError::NotRegular)
    ));
}

// ---- Streams (stream.go parity for plain streams) ----

mod streams {
    use super::*;
    use crate::store::stream_dir;
    use crate::stream::{
        Continues, FrameHeader, FrameReader, Mode, ProtectionRequest, ReleaseOutcome, ReleaseSink,
        RetireOutcome, RetireReason, UseClosure, UsePurpose, UseRequest, Write, WriteOutcome,
    };

    fn record(stream: &str, rid: &str, rev: u64) -> Vec<u8> {
        format!(
            r#"{{"v":1,"stream":"{stream}","record_id":"{rid}","revision":{rev},"payload_contract":"contract: example/v0","payload":{{"n":{rev}}},"provenance":{{"actor_kind":"process","actor":"test"}},"classification":{{"retention":"standard","sensitivity":"0-public"}}}}"#
        )
        .into_bytes()
    }

    fn write(stream: &str, rid: &str, key: &str) -> Write {
        Write {
            record: record(stream, rid, 1),
            idempotency_key: key.to_owned(),
            ..Write::default()
        }
    }

    fn serve() -> UseRequest {
        UseRequest {
            purpose: UsePurpose::Serve,
            selection: Vec::new(),
            closure: UseClosure::MetadataOnly,
        }
    }

    fn store_with_record() -> (Store, TempStoreDir, Arc<StepClock>) {
        let (s, td, clk) = open_body_store();
        let st = s.open_stream("obs.t", Mode::Revisioned, None).unwrap();
        clk.advance(Duration::from_secs(1));
        let r = st.append(&write("obs.t", "a", "k1")).unwrap();
        assert_eq!(r.outcome, WriteOutcome::Appended);
        (s, td, clk)
    }

    fn protection(
        stream: &str,
        key: &str,
        action: &str,
        rid: &str,
        digest_hex: &str,
        extra: &str,
    ) -> ProtectionRequest {
        ProtectionRequest {
            raw: format!(
                r#"{{"v":1,"stream":"{stream}","idempotency_key":"{key}","action":"{action}"{extra},"target":{{"stream":"{stream}","record_id":"{rid}","revision":1,"digest":"sha256:{digest_hex}"}},"scope":"record","reason":"{reason}","evidence":{{"ref":"case-1","digest":"sha256:{e}"}},"observed_at":"2026-01-01T00:00:00.000000Z","provenance":{{"actor_kind":"process","actor":"test"}}}}"#,
                e = "e".repeat(64),
                reason = if action == "restore" { "hold_released" } else { "security_hold" },
            )
            .into_bytes(),
        }
    }

    #[test]
    fn retire_unkeyed_and_keyed_outcomes() {
        let (s, _td, clk) = store_with_record();
        let st = s.open_stream("obs.t", Mode::Revisioned, None).unwrap();
        clk.advance(Duration::from_secs(1));
        st.append(&write("obs.t", "b", "k2")).unwrap();

        clk.advance(Duration::from_secs(1));
        let r = st.retire("a", RetireReason::Withdrawn, None).unwrap();
        assert_eq!(
            (r.outcome, r.seq, r.receipt),
            (RetireOutcome::Retired, Some(4), None)
        );
        let at = r.at.unwrap();
        clk.advance(Duration::from_secs(1));
        let again = st.retire("a", RetireReason::Operator, None).unwrap();
        assert_eq!(again.outcome, RetireOutcome::AlreadyRetired);
        assert_eq!((again.seq, again.at), (Some(4), Some(at)));
        assert_eq!(
            st.retire("zz", RetireReason::Withdrawn, None)
                .unwrap()
                .outcome,
            RetireOutcome::NoRecord
        );
        assert!(st.head("a").unwrap().unwrap().retired);
        assert_eq!(
            st.append(&Write {
                record: record("obs.t", "a", 1),
                idempotency_key: "k3".into(),
                ..Write::default()
            })
            .unwrap()
            .outcome,
            WriteOutcome::RetiredRecord
        );

        let keyed = st
            .retire("b", RetireReason::Retention, Some("k-r"))
            .unwrap();
        assert_eq!(
            (keyed.outcome, keyed.seq),
            (RetireOutcome::Retired, Some(5))
        );
        clk.advance(Duration::from_secs(1));
        let replay = st
            .retire("b", RetireReason::Retention, Some("k-r"))
            .unwrap();
        assert_eq!(replay, keyed, "a keyed replay returns the stored receipt");
        assert_eq!(
            st.retire("b", RetireReason::Retention, Some("k2"))
                .unwrap()
                .outcome,
            RetireOutcome::IdempotencyConflict
        );
        assert_eq!(
            st.retire("zz", RetireReason::Retention, Some("k-z"))
                .unwrap()
                .outcome,
            RetireOutcome::TargetNotFound
        );
        assert!(matches!(
            st.retire("bad id", RetireReason::Withdrawn, None),
            Err(Error::Schema(_))
        ));
        assert!(matches!(
            st.retire("b", RetireReason::Withdrawn, Some("")),
            Err(Error::Schema(_))
        ));
        assert_eq!(st.head("b").unwrap().unwrap().pin.revision, 1);
    }

    #[test]
    fn protect_hold_refuses_export_until_restored() {
        let (s, _td, clk) = store_with_record();
        let st = s.open_stream("obs.t", Mode::Revisioned, None).unwrap();
        let pin = st.head("a").unwrap().unwrap().pin;
        let dhex = pin.digest.strip_prefix("sha256:").unwrap().to_owned();
        let export = UseRequest {
            purpose: UsePurpose::Export,
            selection: vec![pin],
            closure: UseClosure::MetadataOnly,
        };

        clk.advance(Duration::from_secs(1));
        let hold = protection("obs.t", "k-h", "tombstone", "a", &dhex, "");
        let rc = st.protect(&hold).unwrap();
        assert_eq!((rc.outcome.as_str(), rc.seq), ("tombstoned", Some(3)));
        clk.advance(Duration::from_secs(1));
        assert_eq!(
            st.protect(&hold).unwrap(),
            rc,
            "replay returns the stored receipt"
        );
        let res = st.release(&export, None).unwrap();
        assert_eq!(res.outcome, ReleaseOutcome::Refused);
        assert_eq!(res.refusal_reason.as_deref(), Some("protected_use"));

        let ghost = protection("obs.t", "k-g", "tombstone", "zz", &dhex, "");
        let rej = st.protect(&ghost).unwrap();
        assert_eq!(rej.outcome, "rejected");
        assert_eq!(rej.reason.as_deref(), Some("target_not_found"));
        assert_eq!(rej.seq, None);
        assert!(matches!(
            st.protect(&protection("obs.other", "k-x", "tombstone", "a", &dhex, "")),
            Err(Error::Schema(_))
        ));
        assert!(matches!(
            st.protect(&ProtectionRequest {
                raw: b"{}".to_vec()
            }),
            Err(Error::Schema(_))
        ));

        let restore = protection("obs.t", "k-u", "restore", "a", &dhex, r#","restores":3"#);
        let un = st.protect(&restore).unwrap();
        assert_eq!((un.outcome.as_str(), un.seq), ("untombstoned", Some(4)));
        assert_eq!(
            st.release(&export, None).unwrap().outcome,
            ReleaseOutcome::Released
        );
    }

    struct Reenter<'a> {
        store: &'a Store,
        inner: Option<Error>,
    }

    impl ReleaseSink for Reenter<'_> {
        fn frame(&mut self, _: &FrameHeader, r: &mut FrameReader<'_>) -> Result<(), Error> {
            let mut b = Vec::new();
            r.read_to_end(&mut b).unwrap();
            self.inner = self
                .store
                .open_stream("obs.t", Mode::Revisioned, None)
                .err();
            Ok(())
        }
    }

    #[test]
    fn release_sink_reentry_is_refused() {
        let (s, _td, _clk) = store_with_record();
        let st = s.open_stream("obs.t", Mode::Revisioned, None).unwrap();
        let mut sink = Reenter {
            store: &s,
            inner: None,
        };
        let res = st.release(&serve(), Some(&mut sink)).unwrap();
        assert_eq!(sink.inner, Some(Error::SinkReentered));
        assert_eq!(res.outcome, ReleaseOutcome::Released);
        // The guard is cleared after release: the store is usable again.
        s.open_stream("obs.t", Mode::Revisioned, None).unwrap();
    }

    struct Failing;
    impl ReleaseSink for Failing {
        fn frame(&mut self, _: &FrameHeader, _: &mut FrameReader<'_>) -> Result<(), Error> {
            Err(Error::Schema("caller detail".into()))
        }
    }

    struct Skipping;
    impl ReleaseSink for Skipping {
        fn frame(&mut self, _: &FrameHeader, _: &mut FrameReader<'_>) -> Result<(), Error> {
            Ok(())
        }
    }

    /// Consumes the first frame fully, then fails on the next.
    struct FailSecond {
        seen: usize,
    }
    impl ReleaseSink for FailSecond {
        fn frame(&mut self, _: &FrameHeader, r: &mut FrameReader<'_>) -> Result<(), Error> {
            self.seen += 1;
            if self.seen == 2 {
                return Err(Error::Schema("caller detail".into()));
            }
            let mut b = Vec::new();
            r.read_to_end(&mut b).unwrap();
            Ok(())
        }
    }

    #[test]
    fn release_sink_error_keeps_progress() {
        let (s, _td, clk) = store_with_record();
        let st = s.open_stream("obs.t", Mode::Revisioned, None).unwrap();
        clk.advance(Duration::from_secs(1));
        st.append(&write("obs.t", "b", "k2")).unwrap();
        let mut sink = FailSecond { seen: 0 };
        let res = st.release(&serve(), Some(&mut sink)).unwrap();
        assert_eq!(res.sink_error, Some(Error::SinkError));
        assert_eq!(res.outcome, ReleaseOutcome::Uncertain);
        assert_eq!(res.frames_completed, 1);
        assert_eq!(res.frame_in_flight, Some(1));
        let first = res.released_through_pin.expect("first frame delivered");
        assert_eq!(first.record_id, "a");
        assert_eq!(res.selection.len(), 2);
    }

    #[test]
    fn release_sink_error_and_short_read() {
        let (s, _td, _clk) = store_with_record();
        let st = s.open_stream("obs.t", Mode::Revisioned, None).unwrap();
        let res = st.release(&serve(), Some(&mut Failing)).unwrap();
        assert_eq!(res.sink_error, Some(Error::SinkError));
        assert_eq!(res.outcome, ReleaseOutcome::Uncertain);
        assert_eq!(res.frame_in_flight, Some(0));
        assert_eq!(res.frames_completed, 0);
        assert_eq!(res.released_through_pin, None);
        let res = st.release(&serve(), Some(&mut Skipping)).unwrap();
        assert_eq!(res.sink_error, None, "a short read is not a sink error");
        assert_eq!(res.outcome, ReleaseOutcome::Uncertain);
        assert_eq!(res.frame_in_flight, Some(0));
        assert_eq!(res.frames_completed, 0);
    }

    #[test]
    fn torn_tail_is_reported_not_repaired() {
        let (s, td, _clk) = store_with_record();
        let jpath = td.path().join(stream_dir("obs.t")).join("journal.ndjson");
        let mut raw = fs::read(&jpath).unwrap();
        let good = raw.len();
        raw.extend_from_slice(b"{\"v\":1,\"seq\":3");
        fs::write(&jpath, &raw).unwrap();
        let st = s.open_stream("obs.t", Mode::Revisioned, None);
        match st {
            Err(Error::DamagedTail {
                last_cut_seq,
                last_cut_offset,
                damaged_bytes_count,
                ..
            }) => {
                assert_eq!(last_cut_seq, 2);
                assert_eq!(last_cut_offset, good as i64);
                assert_eq!(damaged_bytes_count, (raw.len() - good) as i64);
            }
            other => panic!("want damaged tail, got {other:?}"),
        }
        assert_eq!(fs::read(&jpath).unwrap(), raw, "replay must not repair");
    }

    #[test]
    fn sealed_and_continuation_streams_are_refused() {
        let (s, td, _clk) = store_with_record();
        assert_eq!(
            s.open_stream(
                "obs.next",
                Mode::Revisioned,
                Some(Continues {
                    capability: "contract: durable-record/v1".into(),
                    stream: "obs.t".into(),
                    cut_seq: 2,
                    cut_digest: format!("sha256:{}", "0".repeat(64)),
                }),
            )
            .unwrap_err(),
            Error::ContinuationUnsupported
        );
        let st = s.open_stream("obs.t", Mode::Revisioned, None).unwrap();
        for marker in ["SEALED", "stream-seal.json", "reclaimed.json"] {
            let p = td.path().join(stream_dir("obs.t")).join(marker);
            // Written through the store root, so the file carries the
            // owner-only permissions (Unix) or DACL (Windows) it verifies.
            crate::store::write_file_durable(
                &s.root,
                &format!("{}/{marker}", stream_dir("obs.t")),
                b"{}",
                s.fso,
            )
            .unwrap();
            assert_eq!(st.head("a").unwrap_err(), Error::StreamSealed, "{marker}");
            assert_eq!(
                st.retire("a", RetireReason::Withdrawn, None).unwrap_err(),
                Error::StreamSealed,
                "{marker}"
            );
            let hold = protection("obs.t", "k-h", "tombstone", "a", &"0".repeat(64), "");
            assert_eq!(
                st.protect(&hold).unwrap_err(),
                Error::StreamSealed,
                "{marker}"
            );
            let res = st.release(&serve(), None).unwrap();
            assert_eq!(res.outcome, ReleaseOutcome::Refused, "{marker}");
            fs::remove_file(&p).unwrap();
        }
        assert!(st.head("a").unwrap().is_some());
    }

    #[test]
    fn tampered_index_is_rebuilt_from_the_journal() {
        let (s, td, _clk) = store_with_record();
        let idx = td.path().join(stream_dir("obs.t")).join("heads.idx");
        let want = fs::read(&idx).unwrap();
        fs::write(&idx, b"{\"entries\":[]}\n").unwrap();
        let st = s.open_stream("obs.t", Mode::Revisioned, None).unwrap();
        assert!(st.head("a").unwrap().is_some());
        assert_eq!(fs::read(&idx).unwrap(), want);
    }

    #[test]
    fn designated_stream_and_mode_mismatch_are_refused() {
        let (s, _td, _clk) = store_with_record();
        assert!(matches!(
            s.open_stream(DEFAULT_BODY_REMOVAL_STREAM, Mode::Revisioned, None),
            Err(Error::Schema(_))
        ));
        assert!(matches!(
            s.open_stream("obs.t", Mode::SingleRevision, None),
            Err(Error::Schema(_))
        ));
    }
}
