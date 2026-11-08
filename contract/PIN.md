# Contract pin: durable-record/v1

recordprims implements `contract: durable-record/v1`. The contract is pinned by
immutable identity, never by branch.

| Field | Value |
| --- | --- |
| Repository | `3leaps/crucible` (public) |
| Commit | `3b0647d2378ee9cf24505dae223418886dd6fc83` (tag `v0.2.8`) |
| Family tree `schemas/durable-record/v1` | `4a5a8e2400e045ea3d08274bef81c51bbfca72fc` |
| Family tree `schemas/durable-record/v0` | `34b39c49c3feab9910fbba95f0b501c17fc83f40` (v1 reuses the v0 record envelope and helpers) |
| Status | proposed / experimental (crucible ADR-0019) |

## Vendored set

`contract/crucible/` holds these paths verbatim from the pinned commit, in
crucible's own relative layout, so the reference interpreter runs unmodified:

| Path | Git blob or tree |
| --- | --- |
| `schemas/durable-record/v1/` | tree `4a5a8e2400e045ea3d08274bef81c51bbfca72fc` |
| `schemas/durable-record/v0/` | tree `34b39c49c3feab9910fbba95f0b501c17fc83f40` |
| `docs/standards/durable-record-v1-contract.md` | `13cbb1103fda9dcde253e9167af265828eee7c2e` |
| `docs/standards/durable-record-contract.md` | `d0c9ed2a4d9f56117596e160a0186527ed41fec1` |
| `docs/decisions/ADR-0019-durable-record-tombstoning.md` | `284445edac42519cff7b4909808edc7a1e4cb62e` |
| `scripts/test-durable-record-v1-controls.py` | `13383c916d139d2ebd4cbc095f480c85c20fb4fe` |
| `scripts/validate-durable-record-fixtures.py` | `122378ebb7860eff583ec88ff47902cf58bfa689` |
| `scripts/rfc8785-canonicalize.py` | `4e09387ced83db97aa6ed7e239296bf5bba27501` |
| `scripts/rfc3339-instant.py` | `87a69543642c40bc008700961e0935a3496bd0a4` |

Archive identity, produced by
`git archive 3b0647d <the paths above, in table order> | shasum -a 256`:

```text
96a2a8d228c71fd7f9164ad9fd0007d3beeb029f7627519b9dfbf48b9dcc484d
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
