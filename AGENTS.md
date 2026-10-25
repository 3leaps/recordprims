# recordprims: AI agent guide

## Read first

1. This file.
2. `AGENTS.local.md` if present (untracked; machine-local only).
3. `README.md` for the product.
4. `contract/PIN.md` for the pinned durable-record/v1 contract.

recordprims is a **prims** library with two implementations under one
contract: pure Go (the first) and Rust (the twin). Both must pass the same
conformance suite with byte-identical normalised output before 0.1.0.

## Operating model

| Aspect | Setting |
| ------ | ------- |
| Mode | Supervised (a human reviews before merge) |
| Classification | code-substantive |
| Default role | devlead |

## Rules

- Never edit `contract/crucible/`. It is vendored verbatim; move the pin
  deliberately, as described in `contract/PIN.md`.
- The Go tree stays cgo-free. Dependencies on sibling prims are allow-listed
  in `scripts/deps-check.sh`. No sibling type appears in the public API.
- While `profile_status: bootstrapping` (`.fulmen/repo.yaml`), Go is the operating
  tree and the only one graded against `conformance/`; vector expectations are
  authored as data, never snapshotted from Go. Once `locked`, a contract or
  behaviour change lands with both implementations and the shared tests changed
  in the same PR.
- No sibling prim type in the exported API (`make api-scan`). Wrap siblings;
  never re-export them.
- Run `make check` (and `make pr-final` before opening a PR) before proposing a change.
- Private planning, client material and local notes stay out of this tree.
  `.gitignore` is a convenience filter, not a security boundary.

## Attribution

Commits and PR bodies end with:

```text
Role: <role>
Co-authored-by: <model> <noreply@3leaps.net>
Committer-of-Record: @3leapsdave
```
