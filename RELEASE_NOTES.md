# Release Notes

The latest three releases, newest first. The complete history is in
[CHANGELOG.md](CHANGELOG.md); the notes for each release are in
[docs/releases/](docs/releases/). When a fourth release is added here, the
oldest is removed; its notes remain in `docs/releases/`.

---

## v0.0.2 - 2026-11-11

The pure-Go `durable-record/v1` store (`recordprims-fs/v2`), passing all 400
vectors of the pinned `3leaps/crucible` `v0.2.10` contract. Pre-release; the
Rust twin and language bindings land before 0.1.0.

- Complete `durable-record/v1` journal and stream semantics in pure Go:
  streams, continuations, detached seals, content writes, keyed idempotency,
  protection holds, retirement, designated `body_removal_stream` recovery and
  removal eligibility, and `Release` with callback-scoped reader revocation.
- `recordprims-fs/v2` local filesystem store with watermarked stream indexes
  (`heads.idx`, `keys.idx`, `records.idx`) and bounded partial-write abort, plus
  the internal `ndjson-cas` conditional-PUT bucket stratum (`internal/stratum/bucketcas`).
- Pinned to `3leaps/crucible` `v0.2.10` (`400 / 400` v1 conformance vectors).
- One signed tag, `v0.0.2`. No registry package is published beyond the Go
  module tag.

Details: [docs/releases/v0.0.2.md](docs/releases/v0.0.2.md).

---

## v0.0.1 - 2026-10-27

The first tag: the foundation of a durable record store for the
`durable-record/v1` contract, in pure Go. Pre-release; the API will change
before 0.1.0.

- Go module `github.com/3leaps/recordprims`, cgo-free, Go 1.26 or later.
- A verified, confined store root with owner and permission checks on every
  platform, and a filesystem allowlist.
- A cross-process store lock.
- Content-addressed bodies with publication leases, incarnations and
  retention tombstones, idempotent by state.
- One signed tag, `v0.0.1`. No registry package is published.

Details: [docs/releases/v0.0.1.md](docs/releases/v0.0.1.md).
