# Contract pin: durable-record/v1

recordprims implements `contract: durable-record/v1`. The contract is pinned by
immutable identity, never by branch.

| Field | Value |
| --- | --- |
| Repository | `3leaps/crucible` (public) |
| Commit | `f1064b4f16a3e6018480c6dc3f030e070a3b506e` |
| Family tree `schemas/durable-record/v1` | `64dd038bcc1aa596fdf0c9faed32c6ed6f0f8504` |
| Family tree `schemas/durable-record/v0` | `5c4c03d3502fa500436f551d2d2d2018989be19f` (v1 reuses the v0 record envelope and helpers) |
| Status | proposed / experimental (crucible ADR-0019) |

## Vendored set

`contract/crucible/` holds these paths verbatim from the pinned commit, in
crucible's own relative layout, so the reference interpreter runs unmodified:

| Path | Git blob or tree |
| --- | --- |
| `schemas/durable-record/v1/` | tree `64dd038bcc1aa596fdf0c9faed32c6ed6f0f8504` |
| `schemas/durable-record/v0/` | tree `5c4c03d3502fa500436f551d2d2d2018989be19f` |
| `docs/standards/durable-record-v1-contract.md` | `ea7fa6aa431a45dfed2119cdc37995b8e029865a` |
| `docs/standards/durable-record-contract.md` | `d62574c5fd51d9158d81609ac0e6406142e5cd2b` |
| `docs/decisions/ADR-0019-durable-record-tombstoning.md` | `284445edac42519cff7b4909808edc7a1e4cb62e` |
| `scripts/test-durable-record-v1-controls.py` | `e4fb1cc07970b17a515ebfd23256dcb3d23a6880` |
| `scripts/validate-durable-record-fixtures.py` | `d7652f1b546ee11d5f7cc92bfc91d44606dfa3f5` |
| `scripts/rfc8785-canonicalize.py` | `3fce69cb93ee4cd46cf595f6288cc0d53553e8a1` |
| `scripts/rfc3339-instant.py` | `1fbb9f09a9ed505c958888f91bc67cafad827a94` |

Archive identity, produced by
`git archive f1064b4 <the paths above, in table order> | shasum -a 256`:

```text
02407a0f53a4b88a377ff28b414c0c6aa6f733c85ba648bdef72ba1325ee33fb
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
