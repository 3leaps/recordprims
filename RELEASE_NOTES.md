# Release Notes

The latest three releases, newest first. The complete history is in
[CHANGELOG.md](CHANGELOG.md); the notes for each release are in
[docs/releases/](docs/releases/). When a fourth release is added here, the
oldest is removed; its notes remain in `docs/releases/`.

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
