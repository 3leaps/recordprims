# Contract pin: durable-record/v1

recordprims implements `contract: durable-record/v1`. The contract is pinned by
immutable identity, never by branch.

| Field | Value |
| --- | --- |
| Repository | `3leaps/crucible` (public) |
| Commit | `0a93e24c6810a20c578fbd94ab5186e72353eba8` (tag `v0.2.3`) |
| Family tree `schemas/durable-record/v1` | `ff70ccffb4f0b8760fa7cf7eb0ad18744d386874` |
| Family tree `schemas/durable-record/v0` | `95d2b93a09f083c2ed0e09dcfce2c10e29e38f78` (v1 reuses the v0 record envelope and helpers) |
| Status | proposed / experimental (crucible ADR-0019) |

## Vendored set

`contract/crucible/` holds these paths verbatim from the pinned commit, in
crucible's own relative layout, so the reference interpreter runs unmodified:

| Path | Git blob or tree |
| --- | --- |
| `schemas/durable-record/v1/` | tree `ff70ccffb4f0b8760fa7cf7eb0ad18744d386874` |
| `schemas/durable-record/v0/` | tree `95d2b93a09f083c2ed0e09dcfce2c10e29e38f78` |
| `docs/standards/durable-record-v1-contract.md` | `0af298cb30b21af8808c4fa766a113bc14a99142` |
| `docs/standards/durable-record-contract.md` | `e0e9502cd861564982511872502e7f6009948999` |
| `docs/decisions/ADR-0019-durable-record-tombstoning.md` | `284445edac42519cff7b4909808edc7a1e4cb62e` |
| `scripts/test-durable-record-v1-controls.py` | `51353c1141ded3de65df6999a7797dde2f36f624` |
| `scripts/validate-durable-record-fixtures.py` | `615ba229bb7c395e99ccc9717751a5a88cadfba4` |
| `scripts/rfc8785-canonicalize.py` | `4e09387ced83db97aa6ed7e239296bf5bba27501` |
| `scripts/rfc3339-instant.py` | `2a43d4763b55fc3c8e492bb3451a8b573504e765` |

Archive identity, produced by
`git archive 0a93e24 <the paths above, in table order> | shasum -a 256`:

```text
771cf80c9cb2e0298a21a4bce4fe7a1418227378ed61c7638a38202f3fab96aa
```

`INVENTORY.sha256` lists the SHA-256 of every vendored file.

## Checks

`make contract-check` recomputes the Git object id of every vendored file and
tree from the bytes on disk, compares them with this table, and checks the
inventory. Any drift fails. `make contract-oracle` runs the vendored v1
interpreter (`scripts/test-durable-record-v1-controls.py`) against its own
vectors.

## Changing the pin

Move the pin deliberately, in its own change: re-vendor with `git archive`
from the new commit, regenerate the inventory, update this file, and review
the contract diff before any implementation change that depends on it.
