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
- A contract or behaviour change lands with both implementations and the
  shared tests changed in the same PR, once the Rust tree is operating.
- Run `make check` before proposing a change.
- Private planning, client material and local notes stay out of this tree.
  `.gitignore` is a convenience filter, not a security boundary.

## Attribution

Commits and PR bodies end with:

```text
Role: <role>
Co-authored-by: <model> <noreply@3leaps.net>
Committer-of-Record: @3leapsdave
```
