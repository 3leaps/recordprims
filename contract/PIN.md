# Contract pin: durable-record/v1

recordprims implements `contract: durable-record/v1`. The contract is pinned by
immutable identity, never by branch.

| Field | Value |
| --- | --- |
| Repository | `3leaps/crucible` (public) |
| Commit | `16e6abcb1ee498da60e98f0712a29087ba55bbee` |
| Family tree `schemas/durable-record/v1` | `0e03a2b58b9322e57ae0d8277e43523e31c17993` |
| Family tree `schemas/durable-record/v0` | `95d2b93a09f083c2ed0e09dcfce2c10e29e38f78` (v1 reuses the v0 record envelope and helpers) |
| Status | proposed / experimental (crucible ADR-0019) |

## Vendored set

`contract/crucible/` holds these paths verbatim from the pinned commit, in
crucible's own relative layout, so the reference interpreter runs unmodified:

| Path | Git blob or tree |
| --- | --- |
| `schemas/durable-record/v1/` | tree `0e03a2b58b9322e57ae0d8277e43523e31c17993` |
| `schemas/durable-record/v0/` | tree `95d2b93a09f083c2ed0e09dcfce2c10e29e38f78` |
| `docs/standards/durable-record-v1-contract.md` | `d20d5500ce54c6c8aebf56ba57408398b1fd6f1f` |
| `docs/standards/durable-record-contract.md` | `e0e9502cd861564982511872502e7f6009948999` |
| `docs/decisions/ADR-0019-durable-record-tombstoning.md` | `71d1abb72c6d7be9460d33ae8eea2c2850917341` |
| `scripts/test-durable-record-v1-controls.py` | `a141c3485d4aacdf0c73d67d6762453da1878dcf` |
| `scripts/validate-durable-record-fixtures.py` | `615ba229bb7c395e99ccc9717751a5a88cadfba4` |
| `scripts/rfc8785-canonicalize.py` | `4e09387ced83db97aa6ed7e239296bf5bba27501` |
| `scripts/rfc3339-instant.py` | `2a43d4763b55fc3c8e492bb3451a8b573504e765` |

Archive identity, produced by
`git archive 16e6abc <the paths above, in table order> | shasum -a 256`:

```text
439829fd713b32b478ec175fb81859a14638ceca5972b90d37f70eb16c4c65e4
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
