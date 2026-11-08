# Contract pin: durable-record/v1

recordprims implements `contract: durable-record/v1`. The contract is pinned by
immutable identity, never by branch.

| Field | Value |
| --- | --- |
| Repository | `3leaps/crucible` (public) |
| Commit | `0e1e5f9b1f2e262a72ff166086d9543199b59300` (tag `v0.2.9`) |
| Family tree `schemas/durable-record/v1` | `ebc490a780d627d3d765879cb473f597b088d67e` |
| Family tree `schemas/durable-record/v0` | `34b39c49c3feab9910fbba95f0b501c17fc83f40` (v1 reuses the v0 record envelope and helpers) |
| Status | proposed / experimental (crucible ADR-0019) |

## Vendored set

`contract/crucible/` holds these paths verbatim from the pinned commit, in
crucible's own relative layout, so the reference interpreter runs unmodified:

| Path | Git blob or tree |
| --- | --- |
| `schemas/durable-record/v1/` | tree `ebc490a780d627d3d765879cb473f597b088d67e` |
| `schemas/durable-record/v0/` | tree `34b39c49c3feab9910fbba95f0b501c17fc83f40` |
| `docs/standards/durable-record-v1-contract.md` | `4b6ce8c6076da7685c80bef9a72bcda46ad6e73c` |
| `docs/standards/durable-record-contract.md` | `d0c9ed2a4d9f56117596e160a0186527ed41fec1` |
| `docs/decisions/ADR-0019-durable-record-tombstoning.md` | `284445edac42519cff7b4909808edc7a1e4cb62e` |
| `scripts/test-durable-record-v1-controls.py` | `6d7573801321e414bbc2996e9d3fa3a91c8d478e` |
| `scripts/validate-durable-record-fixtures.py` | `65e20f236f1f319fecc25d3f5a199f6db92bec7e` |
| `scripts/rfc8785-canonicalize.py` | `3fce69cb93ee4cd46cf595f6288cc0d53553e8a1` |
| `scripts/rfc3339-instant.py` | `1fbb9f09a9ed505c958888f91bc67cafad827a94` |

Archive identity, produced by
`git archive 0e1e5f9 <the paths above, in table order> | shasum -a 256`:

```text
266c3aa29025f1b6c74ad3aec5f58ceb8bbebf95884189b9f92fb0dd100da473
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
