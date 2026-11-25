# Contract pin: durable-record/v1 and record-collection/v0

recordprims implements `contract: durable-record/v1`. It also vendors
`contract: record-collection/v0` as a separately pinned family. Both are
pinned by immutable identity, never by branch.

| Field | Value |
| --- | --- |
| Repository | `3leaps/crucible` (public) |
| Commit | `38e40aa02bbf4954662142adfb02d0420f0c4caf` (tag `v0.2.14`) |
| Family tree `schemas/durable-record/v1` | `64dd038bcc1aa596fdf0c9faed32c6ed6f0f8504` |
| Family tree `schemas/durable-record/v0` | `5c4c03d3502fa500436f551d2d2d2018989be19f` (v1 reuses the v0 record envelope and helpers) |
| Family tree `schemas/record-collection/v0` | `47bd13abbc630616ba105e7b927e5b1cb0002c3c` |
| Status | proposed / experimental (crucible ADR-0019); `record-collection/v0` proposed / experimental |

Every durable-record path below has the same Git object id at `v0.2.14` as at
the previous pin, `v0.2.10` (`24c8920`).

## Vendored set: durable-record/v1

`contract/crucible/` holds these paths verbatim from the pinned commit, in
crucible's own relative layout, so the reference interpreter runs unmodified:

| Path | Git blob or tree |
| --- | --- |
| `schemas/durable-record/v1/` | tree `64dd038bcc1aa596fdf0c9faed32c6ed6f0f8504` |
| `schemas/durable-record/v0/` | tree `5c4c03d3502fa500436f551d2d2d2018989be19f` |
| `docs/standards/durable-record-v1-contract.md` | `f17fc378ea1d907297cac233c63ac072a75f0ba9` |
| `docs/standards/durable-record-contract.md` | `aac6b12939d6abba93c1c0bbbf159102473160b2` |
| `docs/decisions/ADR-0019-durable-record-tombstoning.md` | `284445edac42519cff7b4909808edc7a1e4cb62e` |
| `scripts/test-durable-record-v1-controls.py` | `e4fb1cc07970b17a515ebfd23256dcb3d23a6880` |
| `scripts/validate-durable-record-fixtures.py` | `d7652f1b546ee11d5f7cc92bfc91d44606dfa3f5` |
| `scripts/rfc8785-canonicalize.py` | `3fce69cb93ee4cd46cf595f6288cc0d53553e8a1` |
| `scripts/rfc3339-instant.py` | `1fbb9f09a9ed505c958888f91bc67cafad827a94` |

Archive identity, produced by
`git archive 38e40aa <the paths above, in table order> | shasum -a 256`:

```text
b3059cd1537b81f6aadfe2316e48d32bc3c39258759386d98d3153b2ac717d2e
```

## Vendored set: record-collection/v0

The collection reference script also reads the durable-record v1 record schema
and `scripts/rfc8785-canonicalize.py` from the set above.

| Path | Git blob or tree |
| --- | --- |
| `schemas/record-collection/v0/` | tree `47bd13abbc630616ba105e7b927e5b1cb0002c3c` |
| `scripts/test-record-collection-controls.py` | `d3b56849c459cb6f8062de56d958aa55941068b5` |

Archive identity, produced by
`git archive 38e40aa <the paths above, in table order> | shasum -a 256`:

```text
9411528632beed8c718bf5b5eb8d6294e0f767c2d5c94a67a82ab43a527ddc43
```

`INVENTORY.sha256` lists the SHA-256 of every vendored file.

## Checks

`make contract-check` recomputes the Git object id of every vendored file and
tree from the bytes on disk, compares them with every vendored-set table, and
checks the inventory. Any drift fails. `make contract-oracle` runs the vendored v1
interpreter (`scripts/test-durable-record-v1-controls.py`) and the vendored
collection controls (`scripts/test-record-collection-controls.py`) against
their own vectors.

## Changing the pin

Move the pin deliberately, in its own change: re-vendor with `git archive`
from the new commit, regenerate the inventory, update this file, and review
the contract diff before any implementation change that depends on it.
