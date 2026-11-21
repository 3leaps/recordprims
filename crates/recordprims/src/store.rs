// SPDX-License-Identifier: MIT OR Apache-2.0

//! Store initialization, opening, metadata verification, and lock management
//! (`store.go` and `store_lock.go` parity).

use std::io::Write;
use std::path::Path;
use std::sync::atomic::{AtomicBool, AtomicI64, Ordering};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use sha2::{Digest as _, Sha256};

use crate::body::DEFAULT_PENDING_BODY_TTL;
use crate::canon;
use crate::error::Error;
use crate::fsroot::{self, FsRootError, Root};
use crate::instant::{format_instant, Clock, SystemClock};
use crate::jfile::{Appender, JFileError};
use crate::journal;
use crate::lock::FileLock;
use crate::shape;
use crate::wire::{self, Kind, Member, Node, MAX_SAFE_INTEGER};

/// Default idempotency window (7 days, matching `DefaultIdempotencyWindow` in `store.go`).
pub const DEFAULT_IDEMPOTENCY_WINDOW: Duration = Duration::from_secs(7 * 24 * 3600);

/// Default designated body-removal lifecycle stream identifier (`"bodies"`).
pub const DEFAULT_BODY_REMOVAL_STREAM: &str = "bodies";

pub(crate) const STORE_FORMAT: &str = "recordprims-fs/v2";
pub(crate) const LEGACY_STORE_FORMAT: &str = "recordprims-fs/v1";

pub(crate) const FILE_STORE: &str = "STORE";
pub(crate) const FILE_PROFILE: &str = "profile.json";
pub(crate) const FILE_LOCK: &str = "lock";
pub(crate) const FILE_ATTEMPT_KEY: &str = "attempt.key";
pub(crate) const FILE_DIAG_KEY: &str = "diag.key";
pub(crate) const DIR_STREAMS: &str = "streams";
pub(crate) const DIR_BODIES: &str = "bodies";

pub(crate) const KEY_LEN: usize = 32;
pub(crate) const STORE_ID_BYTES: usize = 16;

/// Immutable build capabilities section of `profile.json` (`ProfileBuild` in `store.go`).
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ProfileBuild {
    /// Library name (`"recordprims"`).
    pub name: String,
    /// Schema/profile build version (`"0.0.1"`).
    pub version: String,
    /// Target triple slug (`"<arch>-<os>"`).
    pub target: String,
    /// Native dependencies (`[]`).
    pub native_dependencies: Vec<String>,
    /// Compiled strata (`["journal-files", "ndjson-cas"]`).
    pub compiled_strata: Vec<String>,
    /// Compiled capabilities (`["revisioned", "body_ref", "tombstoning"]`).
    pub compiled_capabilities: Vec<String>,
}

/// Supported deletion surfaces section of `profile.json` (`ProfileDeletion` in `store.go`).
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ProfileDeletion {
    /// Live head deletion support (`"supported"`).
    pub live_head: String,
    /// Retained history deletion support (`"unsupported"`).
    pub retained_history: String,
    /// Managed replicas deletion support (`"not_applicable"`).
    pub managed_replicas: String,
    /// External bodies deletion support (`"supported"`).
    pub external_bodies: String,
    /// Decryption deletion support (`"not_applicable"`).
    pub decryption: String,
    /// Unmanaged copies deletion support (`"outside_claim"`).
    pub unmanaged_copies: String,
}

/// Runtime section of a store's `profile.json` (`ProfileRuntime` in `store.go`).
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ProfileRuntime {
    /// Stratum identifier (`"journal-files"`).
    pub stratum: String,
    /// Failure model (`"survives_process_loss"`).
    pub failure_model: String,
    /// Commit durability strategy (`"fsync_file_and_dir"`).
    pub commit_strategy: String,
    /// Commit durability note.
    pub commit_note: String,
    /// History policy (`"append_only"`).
    pub history_policy: String,
    /// Enabled capabilities (`["revisioned", "body_ref", "tombstoning"]`).
    pub enabled_capabilities: Vec<String>,
    /// Idempotency window in milliseconds (`1..=MAX_SAFE_INTEGER`).
    pub idempotency_window_ms: i64,
    /// Clock skew bound in milliseconds (`0`).
    pub clock_skew_bound_ms: i64,
    /// Designated body-removal stream identifier (`"bodies"` by default).
    pub body_removal_stream: String,
    /// Incarnation allocation strategy (`"prewrite_durable_ledger"`).
    pub incarnation_allocation: String,
    /// Quarantined designated streams (`[]` when none).
    pub quarantined_designated_streams: Vec<String>,
    /// Supported deletion surfaces.
    pub deletion: ProfileDeletion,
}

/// Store stratum profile (`profile.json`, `Profile` in `store.go`).
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Profile {
    /// Contract capabilities (`["contract: durable-record/v1"]`).
    pub capabilities: Vec<String>,
    /// Profile origin (`"declared"`).
    pub origin: String,
    /// Conformance declaration (`"none"`).
    pub conformance: String,
    /// Build section.
    pub build: ProfileBuild,
    /// Runtime section.
    pub runtime: ProfileRuntime,
}

/// Options for [`init`].
#[derive(Clone, Default)]
pub struct InitOptions {
    /// Idempotency window (`None` or `Duration::ZERO` uses [`DEFAULT_IDEMPOTENCY_WINDOW`]).
    pub idempotency_window: Option<Duration>,
    /// Designated body-removal stream name (empty selects [`DEFAULT_BODY_REMOVAL_STREAM`]).
    pub body_removal_stream: String,
    /// Optional clock override (`None` uses [`SystemClock`]).
    pub clock: Option<Arc<dyn Clock>>,
    /// Filesystem allowlist options.
    pub fs: fsroot::Options,
}

/// Options for [`open`].
#[derive(Clone, Default)]
pub struct OpenOptions {
    /// Maximum time to wait for the store lock (`Duration::ZERO` tries once).
    pub lock_timeout: Duration,
    /// Optional clock override (`None` uses [`SystemClock`]).
    pub clock: Option<Arc<dyn Clock>>,
    /// Unconsumed body publication lease TTL (`Duration::ZERO` selects [`DEFAULT_PENDING_BODY_TTL`]).
    pub pending_body_ttl: Duration,
    /// Admit overlay filesystems (CI containers).
    pub allow_overlay: bool,
    /// Filesystem allowlist options.
    pub fs: fsroot::Options,
}

/// Coordinates of one record revision in a stream.
#[derive(Debug, Clone, PartialEq, Eq, Hash)]
pub struct Pin {
    /// Stream identifier.
    pub stream: String,
    /// Record identifier.
    pub record_id: String,
    /// Revision number (`>= 1`).
    pub revision: u64,
    /// JCS SHA-256 digest (`sha256:<64 hex>`).
    pub digest: String,
}

/// Reference to an external body stored in a [`Store`].
#[derive(Debug, Clone, PartialEq, Eq, Hash)]
pub struct BodyRef {
    /// Content digest (`sha256:<64 lowercase hex>`).
    pub digest: String,
    /// Byte length (`0..=MAX_SAFE_INTEGER`).
    pub length: i64,
    /// Lowercase `type/subtype` media type.
    pub media_type: String,
}

#[derive(Debug, Clone)]
pub(crate) struct StoreMeta {
    #[allow(dead_code)]
    pub(crate) format: String,
    pub(crate) store_id: String,
    #[allow(dead_code)]
    pub(crate) key_generation: u64,
    #[allow(dead_code)]
    pub(crate) created_at: String,
    pub(crate) profile_digest: String,
    pub(crate) idempotency_window_ms: i64,
}

#[cfg(test)]
pub(crate) type FaultHook = Arc<dyn Fn(&str) -> Result<(), Error> + Send + Sync>;

/// Open handle to a verified `recordprims-fs/v2` store directory.
pub struct Store {
    pub(crate) root: Root,
    pub(crate) fso: fsroot::Options,
    pub(crate) appender: Appender,
    pub(crate) meta: StoreMeta,
    pub(crate) profile: Profile,
    pub(crate) profile_node: Node,
    #[allow(dead_code)]
    pub(crate) attempt_key: [u8; KEY_LEN],
    #[allow(dead_code)]
    pub(crate) diag_key: [u8; KEY_LEN],
    pub(crate) lock_timeout: Duration,
    pub(crate) pending_body_ttl: Duration,
    #[allow(dead_code)]
    pub(crate) max_body_length: AtomicI64,
    pub(crate) clock: Arc<dyn Clock>,
    pub(crate) mu: Mutex<()>,
    pub(crate) closed: AtomicBool,
    pub(crate) body_dirs_ready: AtomicBool,
    /// Thread currently inside a release sink callback, if any.
    pub(crate) release_thread: Mutex<Option<std::thread::ThreadId>>,
    #[cfg(test)]
    pub(crate) hook: Option<FaultHook>,
}

impl Store {
    /// Store's unique 128-bit identifier (32 lowercase hex characters).
    pub fn store_id(&self) -> &str {
        &self.meta.store_id
    }

    /// Store's configured idempotency window.
    pub fn idempotency_window(&self) -> Duration {
        Duration::from_millis(self.meta.idempotency_window_ms as u64)
    }

    pub(crate) fn window_node(&self) -> Node {
        Node::int(self.meta.idempotency_window_ms)
    }

    /// Store's verified stratum profile.
    pub fn profile(&self) -> Profile {
        self.profile.clone()
    }

    /// Close the store handle. Subsequent operations return [`Error::Closed`].
    pub fn close(&self) -> Result<(), Error> {
        if self.closed.swap(true, Ordering::SeqCst) {
            return Err(Error::Closed);
        }
        Ok(())
    }

    #[cfg(test)]
    pub(crate) fn fault(&self, phase: &str) -> Result<(), Error> {
        if let Some(ref h) = self.hook {
            h(phase)?;
        }
        Ok(())
    }

    #[cfg(not(test))]
    #[inline]
    pub(crate) fn fault(&self, _phase: &str) -> Result<(), Error> {
        Ok(())
    }

    pub(crate) fn with_lock<T, F>(&self, f: F) -> Result<T, Error>
    where
        F: FnOnce() -> Result<T, Error>,
    {
        if *self
            .release_thread
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            == Some(std::thread::current().id())
        {
            return Err(Error::SinkReentered);
        }
        let _guard = self.mu.lock().unwrap_or_else(|e| e.into_inner());
        if self.closed.load(Ordering::SeqCst) {
            return Err(Error::Closed);
        }
        acquire_store_lock(&self.root, self.fso, self.lock_timeout, f)
    }

    pub(crate) fn exists(&self, rel: &str) -> Result<bool, Error> {
        match self.root.stat_verified(rel, false, self.fso) {
            Ok(_) => Ok(true),
            Err(e) if e.is_not_found() => Ok(false),
            Err(FsRootError::NotRegular) => self
                .root
                .stat_verified(rel, true, self.fso)
                .map(|_| true)
                .map_err(|de| de.into_store_error("stat", false)),
            Err(e) => Err(e.into_store_error("stat", false)),
        }
    }
}

/// Initialize a new `recordprims-fs/v2` store in `dir`.
///
/// `dir` must already exist, be owned by the current user with owner-only
/// permissions, reside on an admitted local filesystem, and be empty.
pub fn init(dir: impl AsRef<Path>, opts: InitOptions) -> Result<(), Error> {
    let window = match opts.idempotency_window {
        Some(d) if !d.is_zero() => d,
        _ => DEFAULT_IDEMPOTENCY_WINDOW,
    };
    let window_ms = i64::try_from(window.as_millis())
        .ok()
        .filter(|&ms| (1..=MAX_SAFE_INTEGER).contains(&ms))
        .ok_or_else(|| Error::schema("idempotency window out of domain"))?;

    let body_stream = if opts.body_removal_stream.is_empty() {
        DEFAULT_BODY_REMOVAL_STREAM
    } else {
        opts.body_removal_stream.as_str()
    };
    if !valid_stream_id(body_stream) {
        return Err(Error::schema("invalid body_removal_stream"));
    }

    let clock: Arc<dyn Clock> = opts.clock.unwrap_or_else(|| Arc::new(SystemClock));
    let root = Root::open(dir.as_ref(), opts.fs).map_err(FsRootError::into_open_root_error)?;

    let names = root
        .read_dir_names(".", opts.fs)
        .map_err(|e| e.into_store_error("init", false))?;
    if !names.is_empty() {
        return Err(Error::store_invalid("directory is not empty"));
    }

    let mut id_bytes = [0u8; STORE_ID_BYTES];
    fill_random(&mut id_bytes)?;
    let created_at = format_instant(clock.now());

    for name in [FILE_ATTEMPT_KEY, FILE_DIAG_KEY] {
        let mut k = [0u8; KEY_LEN];
        fill_random(&mut k)?;
        write_file_durable(&root, name, &k, opts.fs)?;
    }

    for d in [DIR_STREAMS, DIR_BODIES] {
        root.mkdir(d, opts.fs)
            .map_err(|e| e.into_store_error("mkdir", true))?;
    }

    let prof = default_profile(window_ms, body_stream);
    let prof_node = encode_profile_node(&prof);
    let prof_bytes = wire::encode(&prof_node);
    let prof_digest =
        canon::digest(&prof_bytes).map_err(|_| Error::store_invalid("digest profile"))?;
    write_file_durable(&root, FILE_PROFILE, &prof_bytes, opts.fs)?;

    let appender = Appender::new(opts.fs);
    create_stream_journal(
        &root,
        opts.fs,
        &appender,
        body_stream,
        "single_revision",
        &prof.runtime.failure_model,
        &created_at,
        None,
        true,
    )?;

    let meta_node = Node::object(vec![
        Member {
            name: "format".into(),
            value: Node::string(STORE_FORMAT),
        },
        Member {
            name: "store_id".into(),
            value: Node::string(hex_encode(&id_bytes)),
        },
        Member {
            name: "key_generation".into(),
            value: Node::int(1),
        },
        Member {
            name: "created_at".into(),
            value: Node::string(created_at),
        },
        Member {
            name: "profile_digest".into(),
            value: Node::string(prof_digest),
        },
        Member {
            name: "idempotency_window_ms".into(),
            value: Node::int(window_ms),
        },
    ]);
    let meta_bytes = wire::encode(&meta_node);
    write_file_durable(&root, FILE_STORE, &meta_bytes, opts.fs)?;
    Ok(())
}

/// Open and verify an existing `recordprims-fs/v2` store in `dir`.
pub fn open(dir: impl AsRef<Path>, opts: OpenOptions) -> Result<Store, Error> {
    let mut fso = opts.fs;
    fso.allow_overlay = fso.allow_overlay || opts.allow_overlay;
    let clock: Arc<dyn Clock> = opts.clock.unwrap_or_else(|| Arc::new(SystemClock));
    let pending_body_ttl = if opts.pending_body_ttl.is_zero() {
        DEFAULT_PENDING_BODY_TTL
    } else {
        opts.pending_body_ttl
    };

    let root = Root::open(dir.as_ref(), fso).map_err(FsRootError::into_open_root_error)?;
    let meta = read_and_verify_meta(&root, fso)?;
    let (prof, prof_node) = read_and_verify_profile(&root, fso, &meta)?;
    let ak = read_key(&root, FILE_ATTEMPT_KEY, fso)?;
    let dk = read_key(&root, FILE_DIAG_KEY, fso)?;
    let mut attempt_key = [0u8; KEY_LEN];
    let mut diag_key = [0u8; KEY_LEN];
    attempt_key.copy_from_slice(&ak);
    diag_key.copy_from_slice(&dk);

    Ok(Store {
        root,
        fso,
        appender: Appender::new(fso),
        meta,
        profile: prof,
        profile_node: prof_node,
        attempt_key,
        diag_key,
        lock_timeout: opts.lock_timeout,
        pending_body_ttl,
        max_body_length: AtomicI64::new(MAX_SAFE_INTEGER),
        clock,
        mu: Mutex::new(()),
        closed: AtomicBool::new(false),
        body_dirs_ready: AtomicBool::new(false),
        release_thread: Mutex::new(None),
        #[cfg(test)]
        hook: None,
    })
}

fn target_slug() -> String {
    let arch = match std::env::consts::ARCH {
        "aarch64" => "arm64",
        "x86_64" => "amd64",
        other => other,
    };
    let os = match std::env::consts::OS {
        "macos" => "darwin",
        other => other,
    };
    format!("{arch}-{os}")
}

fn default_profile(window_ms: i64, body_stream: &str) -> Profile {
    Profile {
        capabilities: vec!["contract: durable-record/v1".into()],
        origin: "declared".into(),
        conformance: "none".into(),
        build: ProfileBuild {
            name: "recordprims".into(),
            version: "0.0.1".into(),
            target: target_slug(),
            native_dependencies: Vec::new(),
            compiled_strata: vec!["journal-files".into(), "ndjson-cas".into()],
            compiled_capabilities: vec![
                "revisioned".into(),
                "body_ref".into(),
                "tombstoning".into(),
            ],
        },
        runtime: ProfileRuntime {
            stratum: "journal-files".into(),
            failure_model: "survives_process_loss".into(),
            commit_strategy: "fsync_file_and_dir".into(),
            commit_note: "fsync file and directory before receipt".into(),
            history_policy: "append_only".into(),
            enabled_capabilities: vec![
                "revisioned".into(),
                "body_ref".into(),
                "tombstoning".into(),
            ],
            idempotency_window_ms: window_ms,
            clock_skew_bound_ms: 0,
            body_removal_stream: body_stream.into(),
            incarnation_allocation: "prewrite_durable_ledger".into(),
            quarantined_designated_streams: Vec::new(),
            deletion: ProfileDeletion {
                live_head: "supported".into(),
                retained_history: "unsupported".into(),
                managed_replicas: "not_applicable".into(),
                external_bodies: "supported".into(),
                decryption: "not_applicable".into(),
                unmanaged_copies: "outside_claim".into(),
            },
        },
    }
}

fn encode_profile_node(prof: &Profile) -> Node {
    let mut build_fields = vec![
        Member {
            name: "name".into(),
            value: Node::string(prof.build.name.clone()),
        },
        Member {
            name: "version".into(),
            value: Node::string(prof.build.version.clone()),
        },
    ];
    if !prof.build.target.is_empty() {
        build_fields.push(Member {
            name: "target".into(),
            value: Node::string(prof.build.target.clone()),
        });
    }
    build_fields.push(Member {
        name: "native_dependencies".into(),
        value: Node::array(
            prof.build
                .native_dependencies
                .iter()
                .map(|s| Node::string(s.clone()))
                .collect(),
        ),
    });
    build_fields.push(Member {
        name: "compiled_strata".into(),
        value: Node::array(
            prof.build
                .compiled_strata
                .iter()
                .map(|s| Node::string(s.clone()))
                .collect(),
        ),
    });
    build_fields.push(Member {
        name: "compiled_capabilities".into(),
        value: Node::array(
            prof.build
                .compiled_capabilities
                .iter()
                .map(|s| Node::string(s.clone()))
                .collect(),
        ),
    });

    let mut rt_fields = vec![
        Member {
            name: "stratum".into(),
            value: Node::string(prof.runtime.stratum.clone()),
        },
        Member {
            name: "failure_model".into(),
            value: Node::string(prof.runtime.failure_model.clone()),
        },
    ];
    if !prof.runtime.commit_strategy.is_empty() {
        rt_fields.push(Member {
            name: "commit_strategy".into(),
            value: Node::string(prof.runtime.commit_strategy.clone()),
        });
    }
    if !prof.runtime.commit_note.is_empty() {
        rt_fields.push(Member {
            name: "commit_note".into(),
            value: Node::string(prof.runtime.commit_note.clone()),
        });
    }
    rt_fields.push(Member {
        name: "history_policy".into(),
        value: Node::string(prof.runtime.history_policy.clone()),
    });
    rt_fields.push(Member {
        name: "enabled_capabilities".into(),
        value: Node::array(
            prof.runtime
                .enabled_capabilities
                .iter()
                .map(|s| Node::string(s.clone()))
                .collect(),
        ),
    });
    rt_fields.push(Member {
        name: "idempotency_window_ms".into(),
        value: Node::int(prof.runtime.idempotency_window_ms),
    });
    rt_fields.push(Member {
        name: "clock_skew_bound_ms".into(),
        value: Node::int(prof.runtime.clock_skew_bound_ms),
    });
    if !prof.runtime.body_removal_stream.is_empty() {
        rt_fields.push(Member {
            name: "body_removal_stream".into(),
            value: Node::string(prof.runtime.body_removal_stream.clone()),
        });
    }
    if !prof.runtime.incarnation_allocation.is_empty() {
        rt_fields.push(Member {
            name: "incarnation_allocation".into(),
            value: Node::string(prof.runtime.incarnation_allocation.clone()),
        });
    }
    if !prof.runtime.quarantined_designated_streams.is_empty() {
        rt_fields.push(Member {
            name: "quarantined_designated_streams".into(),
            value: Node::array(
                prof.runtime
                    .quarantined_designated_streams
                    .iter()
                    .map(|s| Node::string(s.clone()))
                    .collect(),
            ),
        });
    }
    rt_fields.push(Member {
        name: "deletion".into(),
        value: Node::object(vec![
            Member {
                name: "live_head".into(),
                value: Node::string(prof.runtime.deletion.live_head.clone()),
            },
            Member {
                name: "retained_history".into(),
                value: Node::string(prof.runtime.deletion.retained_history.clone()),
            },
            Member {
                name: "managed_replicas".into(),
                value: Node::string(prof.runtime.deletion.managed_replicas.clone()),
            },
            Member {
                name: "external_bodies".into(),
                value: Node::string(prof.runtime.deletion.external_bodies.clone()),
            },
            Member {
                name: "decryption".into(),
                value: Node::string(prof.runtime.deletion.decryption.clone()),
            },
            Member {
                name: "unmanaged_copies".into(),
                value: Node::string(prof.runtime.deletion.unmanaged_copies.clone()),
            },
        ]),
    });

    Node::object(vec![
        Member {
            name: "capabilities".into(),
            value: Node::array(
                prof.capabilities
                    .iter()
                    .map(|s| Node::string(s.clone()))
                    .collect(),
            ),
        },
        Member {
            name: "origin".into(),
            value: Node::string(prof.origin.clone()),
        },
        Member {
            name: "conformance".into(),
            value: Node::string(prof.conformance.clone()),
        },
        Member {
            name: "build".into(),
            value: Node::object(build_fields),
        },
        Member {
            name: "runtime".into(),
            value: Node::object(rt_fields),
        },
    ])
}

fn acquire_store_lock<T, F>(
    root: &Root,
    fso: fsroot::Options,
    timeout: Duration,
    f: F,
) -> Result<T, Error>
where
    F: FnOnce() -> Result<T, Error>,
{
    let lf = root
        .open_lock_file(FILE_LOCK, fso)
        .map_err(|e| e.into_store_error("lock", false))?;
    let guard = FileLock::acquire(&lf, timeout).map_err(|e| match e {
        None => Error::Locked,
        Some(ioe) => Error::io("lock", &ioe, false),
    })?;
    let res = f();
    let _ = guard.release();
    res
}

pub(crate) fn stream_hash(id: &str) -> String {
    let sum = Sha256::digest(id.as_bytes());
    hex_encode(&sum[..16])
}

pub(crate) fn stream_dir(id: &str) -> String {
    format!("{DIR_STREAMS}/{}", stream_hash(id))
}

pub(crate) fn stream_journal_file(id: &str) -> String {
    format!("{}/journal.ndjson", stream_dir(id))
}

#[allow(clippy::too_many_arguments)]
pub(crate) fn create_stream_journal(
    root: &Root,
    fso: fsroot::Options,
    appender: &Appender,
    stream_id: &str,
    mode: &str,
    failure_model: &str,
    at: &str,
    continues: Option<Node>,
    designated: bool,
) -> Result<(), Error> {
    let sdir = stream_dir(stream_id);
    root.mkdir(&sdir, fso)
        .map_err(|e| e.into_store_error("mkdir", true))?;
    root.sync_dir(DIR_STREAMS, fso)
        .map_err(|e| e.into_store_error("fsync_dir", true))?;
    let jpath = stream_journal_file(stream_id);
    let f = root
        .create_exclusive(&jpath, 0o600, fso)
        .map_err(|e| e.into_store_error("create", true))?;
    if let Err(e) = f.sync_all() {
        return Err(Error::io("fsync", &e, true));
    }
    drop(f);
    root.sync_dir(&sdir, fso)
        .map_err(|e| e.into_store_error("fsync_dir", true))?;

    let mut fields = vec![
        Member {
            name: "v".into(),
            value: Node::int(1),
        },
        Member {
            name: "seq".into(),
            value: Node::int(1),
        },
        Member {
            name: "group".into(),
            value: Node::int(1),
        },
        Member {
            name: "group_size".into(),
            value: Node::int(1),
        },
        Member {
            name: "stream".into(),
            value: Node::string(stream_id),
        },
        Member {
            name: "type".into(),
            value: Node::string("stream_opened"),
        },
        Member {
            name: "at".into(),
            value: Node::string(at),
        },
        Member {
            name: "mode".into(),
            value: Node::string(mode),
        },
        Member {
            name: "failure_model".into(),
            value: Node::string(failure_model),
        },
    ];
    if let Some(cont) = continues {
        fields.push(Member {
            name: "continues".into(),
            value: cont,
        });
    }
    let opened = Node::object(fields);
    let mut line = wire::encode(&opened);
    line.push(b'\n');
    appender
        .append(root, &jpath, 0, &line, designated)
        .map_err(map_jfile_err)?;
    root.sync_dir(&sdir, fso)
        .map_err(|e| e.into_store_error("fsync_dir", true))
}

pub(crate) fn map_jfile_err(err: JFileError) -> Error {
    match err {
        JFileError::CutMismatch => Error::StaleCut,
        JFileError::Aborted => Error::Io {
            op: "write",
            class: crate::error::IoClass::NoSpace,
            indeterminate: false,
        },
        JFileError::Indeterminate | JFileError::Poisoned => Error::Io {
            op: "append",
            class: crate::error::IoClass::Other,
            indeterminate: true,
        },
        JFileError::PreWriteIo(op, kind, raw) => {
            let ioe = match raw {
                Some(code) => std::io::Error::from_raw_os_error(code),
                None => std::io::Error::from(kind),
            };
            Error::io(op, &ioe, true)
        }
        JFileError::Structural(msg) => Error::store_invalid(msg),
    }
}

fn read_and_verify_meta(root: &Root, fso: fsroot::Options) -> Result<StoreMeta, Error> {
    let raw = read_file_verified(root, FILE_STORE, fso, 1 << 16)
        .map_err(|_| Error::store_invalid("store metadata unreadable"))?;
    let node =
        wire::parse_json(&raw).map_err(|_| Error::store_invalid("store metadata invalid"))?;
    if node.kind != Kind::Object {
        return Err(Error::store_invalid("store metadata invalid"));
    }
    let format = match node.get("format") {
        Some(n) if n.kind == Kind::String => n.str_val.clone(),
        Some(n) if n.kind == Kind::Null => String::new(),
        None => String::new(),
        Some(_) => return Err(Error::store_invalid("store metadata invalid")),
    };
    if format == LEGACY_STORE_FORMAT {
        return Err(Error::store_invalid("pre-journal store format"));
    }
    let store_id = match node.get("store_id") {
        Some(n) if n.kind == Kind::String => n.str_val.clone(),
        Some(n) if n.kind == Kind::Null => String::new(),
        None => String::new(),
        Some(_) => return Err(Error::store_invalid("store metadata invalid")),
    };
    let key_generation = match node.get("key_generation") {
        Some(n) if n.kind == Kind::Number => n
            .token
            .parse::<u64>()
            .map_err(|_| Error::store_invalid("store metadata invalid"))?,
        Some(n) if n.kind == Kind::Null => 0,
        None => 0,
        Some(_) => return Err(Error::store_invalid("store metadata invalid")),
    };
    let window_ms = match node.get("idempotency_window_ms") {
        Some(n) if n.kind == Kind::Number => n
            .token
            .parse::<i64>()
            .map_err(|_| Error::store_invalid("store metadata invalid"))?,
        Some(n) if n.kind == Kind::Null => 0,
        None => 0,
        Some(_) => return Err(Error::store_invalid("store metadata invalid")),
    };
    let created_at = match node.get("created_at") {
        Some(n) if n.kind == Kind::String => n.str_val.clone(),
        Some(n) if n.kind == Kind::Null => String::new(),
        None => String::new(),
        Some(_) => return Err(Error::store_invalid("store metadata invalid")),
    };
    let profile_digest = match node.get("profile_digest") {
        Some(n) if n.kind == Kind::String => n.str_val.clone(),
        Some(n) if n.kind == Kind::Null => String::new(),
        None => String::new(),
        Some(_) => return Err(Error::store_invalid("store metadata invalid")),
    };

    if format != STORE_FORMAT
        || store_id.len() != 32
        || key_generation == 0
        || window_ms <= 0
        || window_ms > MAX_SAFE_INTEGER
    {
        return Err(Error::store_invalid("store metadata invalid"));
    }
    Ok(StoreMeta {
        format,
        store_id,
        key_generation,
        created_at,
        profile_digest,
        idempotency_window_ms: window_ms,
    })
}

fn read_and_verify_profile(
    root: &Root,
    fso: fsroot::Options,
    meta: &StoreMeta,
) -> Result<(Profile, Node), Error> {
    let raw = read_file_verified(root, FILE_PROFILE, fso, 1 << 16)
        .map_err(|_| Error::store_invalid("store profile unreadable"))?;
    if !meta.profile_digest.is_empty() {
        let got_digest = canon::digest(&raw)
            .map_err(|_| Error::store_invalid("store profile digest mismatch"))?;
        if got_digest != meta.profile_digest {
            return Err(Error::store_invalid("store profile digest mismatch"));
        }
    }
    let node = wire::ingest(&raw, wire::KIND_STRATUM_PROFILE)
        .map_err(|_| Error::store_invalid("store profile invalid"))?;
    shape::validate("v1/stratum-profile.schema.json", &node)
        .map_err(|_| Error::store_invalid("store profile schema invalid"))?;
    journal::check_profile(&node)
        .map_err(|_| Error::store_invalid("store profile check failed"))?;

    let capabilities = parse_string_array(node.get("capabilities"))
        .ok_or_else(|| Error::store_invalid("store profile invalid"))?;
    let origin = parse_req_str(node.get("origin"))
        .ok_or_else(|| Error::store_invalid("store profile invalid"))?;
    let conformance = parse_req_str(node.get("conformance"))
        .ok_or_else(|| Error::store_invalid("store profile invalid"))?;

    let build_node = node
        .get("build")
        .filter(|n| n.kind == Kind::Object)
        .ok_or_else(|| Error::store_invalid("store profile invalid"))?;
    let build = ProfileBuild {
        name: parse_req_str(build_node.get("name"))
            .ok_or_else(|| Error::store_invalid("store profile invalid"))?,
        version: parse_req_str(build_node.get("version"))
            .ok_or_else(|| Error::store_invalid("store profile invalid"))?,
        target: build_node
            .get("target")
            .filter(|n| n.kind == Kind::String)
            .map(|n| n.str_val.clone())
            .unwrap_or_default(),
        native_dependencies: parse_string_array(build_node.get("native_dependencies"))
            .ok_or_else(|| Error::store_invalid("store profile invalid"))?,
        compiled_strata: parse_string_array(build_node.get("compiled_strata"))
            .ok_or_else(|| Error::store_invalid("store profile invalid"))?,
        compiled_capabilities: parse_string_array(build_node.get("compiled_capabilities"))
            .ok_or_else(|| Error::store_invalid("store profile invalid"))?,
    };

    let rt_node = node
        .get("runtime")
        .filter(|n| n.kind == Kind::Object)
        .ok_or_else(|| Error::store_invalid("store profile invalid"))?;
    let del_node = rt_node
        .get("deletion")
        .filter(|n| n.kind == Kind::Object)
        .ok_or_else(|| Error::store_invalid("store profile invalid"))?;
    let deletion = ProfileDeletion {
        live_head: parse_req_str(del_node.get("live_head"))
            .ok_or_else(|| Error::store_invalid("store profile invalid"))?,
        retained_history: parse_req_str(del_node.get("retained_history"))
            .ok_or_else(|| Error::store_invalid("store profile invalid"))?,
        managed_replicas: parse_req_str(del_node.get("managed_replicas"))
            .ok_or_else(|| Error::store_invalid("store profile invalid"))?,
        external_bodies: parse_req_str(del_node.get("external_bodies"))
            .ok_or_else(|| Error::store_invalid("store profile invalid"))?,
        decryption: parse_req_str(del_node.get("decryption"))
            .ok_or_else(|| Error::store_invalid("store profile invalid"))?,
        unmanaged_copies: parse_req_str(del_node.get("unmanaged_copies"))
            .ok_or_else(|| Error::store_invalid("store profile invalid"))?,
    };

    let idempotency_window_ms = rt_node
        .get("idempotency_window_ms")
        .filter(|n| n.is_int)
        .and_then(|n| n.token.parse::<i64>().ok())
        .ok_or_else(|| Error::store_invalid("store profile invalid"))?;
    let clock_skew_bound_ms = rt_node
        .get("clock_skew_bound_ms")
        .filter(|n| n.is_int)
        .and_then(|n| n.token.parse::<i64>().ok())
        .ok_or_else(|| Error::store_invalid("store profile invalid"))?;
    let body_removal_stream = rt_node
        .get("body_removal_stream")
        .filter(|n| n.kind == Kind::String)
        .map(|n| n.str_val.clone())
        .unwrap_or_default();

    let runtime = ProfileRuntime {
        stratum: parse_req_str(rt_node.get("stratum"))
            .ok_or_else(|| Error::store_invalid("store profile invalid"))?,
        failure_model: parse_req_str(rt_node.get("failure_model"))
            .ok_or_else(|| Error::store_invalid("store profile invalid"))?,
        commit_strategy: rt_node
            .get("commit_strategy")
            .filter(|n| n.kind == Kind::String)
            .map(|n| n.str_val.clone())
            .unwrap_or_default(),
        commit_note: rt_node
            .get("commit_note")
            .filter(|n| n.kind == Kind::String)
            .map(|n| n.str_val.clone())
            .unwrap_or_default(),
        history_policy: parse_req_str(rt_node.get("history_policy"))
            .ok_or_else(|| Error::store_invalid("store profile invalid"))?,
        enabled_capabilities: parse_string_array(rt_node.get("enabled_capabilities"))
            .ok_or_else(|| Error::store_invalid("store profile invalid"))?,
        idempotency_window_ms,
        clock_skew_bound_ms,
        body_removal_stream: body_removal_stream.clone(),
        incarnation_allocation: rt_node
            .get("incarnation_allocation")
            .filter(|n| n.kind == Kind::String)
            .map(|n| n.str_val.clone())
            .unwrap_or_default(),
        quarantined_designated_streams: match rt_node.get("quarantined_designated_streams") {
            Some(qn) => parse_string_array(Some(qn))
                .ok_or_else(|| Error::store_invalid("store profile invalid"))?,
            None => Vec::new(),
        },
        deletion,
    };

    if runtime.idempotency_window_ms != meta.idempotency_window_ms
        || runtime.body_removal_stream.is_empty()
    {
        return Err(Error::store_invalid("store profile mismatch"));
    }

    Ok((
        Profile {
            capabilities,
            origin,
            conformance,
            build,
            runtime,
        },
        node,
    ))
}

fn parse_req_str(n: Option<&Node>) -> Option<String> {
    n.filter(|x| x.kind == Kind::String && !x.str_val.is_empty())
        .map(|x| x.str_val.clone())
}

fn parse_string_array(n: Option<&Node>) -> Option<Vec<String>> {
    let arr = n.filter(|x| x.kind == Kind::Array)?;
    let mut out = Vec::with_capacity(arr.elems.len());
    for e in &arr.elems {
        if e.kind != Kind::String || e.str_val.is_empty() {
            return None;
        }
        out.push(e.str_val.clone());
    }
    Some(out)
}

fn read_key(root: &Root, name: &str, fso: fsroot::Options) -> Result<Vec<u8>, Error> {
    let b = read_file_verified(root, name, fso, (KEY_LEN + 1) as i64)
        .map_err(|_| Error::store_invalid(format!("key {name} failed validation")))?;
    if b.len() != KEY_LEN {
        return Err(Error::store_invalid(format!(
            "key {name} has the wrong length"
        )));
    }
    Ok(b)
}

pub(crate) fn read_file_verified(
    root: &Root,
    name: &str,
    fso: fsroot::Options,
    max_bytes: i64,
) -> Result<Vec<u8>, Error> {
    let (mut f, size) = root
        .open_verified(name, fso)
        .map_err(|e| e.into_store_error("open", false))?;
    if (size as i64) > max_bytes {
        return Err(Error::store_invalid(format!("{name} exceeds size limit")));
    }
    let mut buf = Vec::with_capacity(size as usize);
    use std::io::Read;
    (&mut f)
        .take((max_bytes as u64) + 1)
        .read_to_end(&mut buf)
        .map_err(|e| Error::io("read", &e, false))?;
    if (buf.len() as i64) > max_bytes {
        return Err(Error::store_invalid(format!("{name} exceeds size limit")));
    }
    Ok(buf)
}

pub(crate) fn write_file_durable(
    root: &Root,
    name: &str,
    data: &[u8],
    fso: fsroot::Options,
) -> Result<(), Error> {
    let mut f = root
        .create_exclusive(name, 0o600, fso)
        .map_err(|e| e.into_store_error("create", false))?;
    if let Err(e) = f.write_all(data) {
        return Err(Error::io("write", &e, true));
    }
    if let Err(e) = f.sync_all() {
        return Err(Error::io("fsync", &e, true));
    }
    drop(f);
    let parent = match name.rsplit_once('/') {
        Some((dir, _)) if !dir.is_empty() => dir,
        _ => ".",
    };
    root.sync_dir(parent, fso)
        .map_err(|e| e.into_store_error("fsync_dir", true))
}

pub(crate) fn fill_random(buf: &mut [u8]) -> Result<(), Error> {
    getrandom::fill(buf).map_err(|_| Error::Io {
        op: "read",
        class: crate::error::IoClass::Other,
        indeterminate: false,
    })
}

pub(crate) fn hex_encode(bytes: &[u8]) -> String {
    const HEX: &[u8; 16] = b"0123456789abcdef";
    let mut s = String::with_capacity(bytes.len() * 2);
    for &b in bytes {
        s.push(HEX[usize::from(b >> 4)] as char);
        s.push(HEX[usize::from(b & 0x0f)] as char);
    }
    s
}

pub(crate) fn valid_digest(s: &str) -> bool {
    let Some(hex) = s.strip_prefix("sha256:") else {
        return false;
    };
    hex.len() == 64
        && hex
            .bytes()
            .all(|b| b.is_ascii_digit() || (b'a'..=b'f').contains(&b))
}

pub(crate) fn valid_stream_id(s: &str) -> bool {
    let b = s.as_bytes();
    if b.is_empty() || b.len() > 128 {
        return false;
    }
    let first = b[0];
    if !(first.is_ascii_lowercase() || first.is_ascii_digit()) {
        return false;
    }
    b.iter()
        .all(|&c| c.is_ascii_lowercase() || c.is_ascii_digit() || matches!(c, b'.' | b'_' | b'-'))
}
