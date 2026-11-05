# Contract pin: durable-record/v1

recordprims implements `contract: durable-record/v1`. The contract is pinned by
immutable identity, never by branch.

| Field | Value |
| --- | --- |
| Repository | `3leaps/crucible` (public) |
| Commit | `0e3434743f4139ba8ec821552f86d8f9c996f207` (tag `v0.2.7`) |
| Family tree `schemas/durable-record/v1` | `075d1004d765a04813be1edc7ce922d97eddb2a9` |
| Family tree `schemas/durable-record/v0` | `d7ad4bba0b32a50530f3b8faa07777f89f3204ce` (v1 reuses the v0 record envelope and helpers) |
| Status | proposed / experimental (crucible ADR-0019) |

## Vendored set

`contract/crucible/` holds these paths verbatim from the pinned commit, in
crucible's own relative layout, so the reference interpreter runs unmodified:

| Path | Git blob or tree |
| --- | --- |
| `schemas/durable-record/v1/` | tree `075d1004d765a04813be1edc7ce922d97eddb2a9` |
| `schemas/durable-record/v0/` | tree `d7ad4bba0b32a50530f3b8faa07777f89f3204ce` |
| `docs/standards/durable-record-v1-contract.md` | `1f73cce0b1a8f1e66e325422af4ff7d5eb55ce9a` |
| `docs/standards/durable-record-contract.md` | `c5d59780efcc849e73831bbf34a160803c9578d0` |
| `docs/decisions/ADR-0019-durable-record-tombstoning.md` | `284445edac42519cff7b4909808edc7a1e4cb62e` |
| `scripts/test-durable-record-v1-controls.py` | `cd0c8ded4c79d22ecb12e33f90c4d02aff55f28f` |
| `scripts/validate-durable-record-fixtures.py` | `ef8c4a12619fdcdab40be5e70b2371951d230ed2` |
| `scripts/rfc8785-canonicalize.py` | `4e09387ced83db97aa6ed7e239296bf5bba27501` |
| `scripts/rfc3339-instant.py` | `2a43d4763b55fc3c8e492bb3451a8b573504e765` |

Archive identity, produced by
`git archive 0e34347 <the paths above, in table order> | shasum -a 256`:

```text
b21634eb5e2a135da058d901fc15a9cac59bcaa40f6ee95f159a0286359990ba
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
