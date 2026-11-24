# Release Signing and Verification

This page states the release trust model, its trust files, and how to verify
a release. The signing pins and fingerprint anchors were written once, before
the first release, and landed through review.

## Release identity

A release is one signed annotated tag, `vX.Y.Z`, at a commit of `main`. The
tag releases the root Go module (`github.com/3leaps/recordprims`). The Rust
crate is a preview in the repository and is not published; when it is, the
same tag will cover it. A release that publishes artifacts will also carry
`SHA256SUMS` and `SHA512SUMS` manifests, each with a minisign signature and an
OpenPGP signature by the committed signing subkey; these are separate
signatures, and all will be required.

Establish the expected GPG primary key, the exact signing subkey, and the
minisign public-blob fingerprint through an independently trusted maintainer
channel before accepting downloaded public keys. A key distributed beside a
signature is not independent authentication.

## Trust files

| File | Contents |
| --- | --- |
| `docs/security/release-signing-keys.asc` | Public OpenPGP export: the approved primary key and the exact signing subkey, nothing else |
| `docs/security/recordprims-minisign.pub` | The minisign public key that signs the checksum manifests |
| `config/release/signing-subkey.txt` | The fingerprint of the one signing subkey tags may use |
| `keys/expected-fingerprints.txt` | Two lines: the OpenPGP primary fingerprint, then the minisign public-blob SHA-256 |
| `keys/expected-fingerprints.ndjson` | The checker's fingerprint records for the same two keys |
| `config/release/tagger-identity.txt` | The exact git tagger identity for release tags |
| `config/release/release-workflow.sha256` | Digests of every reviewed workflow and local action |

## Workflows

Every workflow and local action is pinned by digest in
`config/release/release-workflow.sha256`, and every workflow must declare
top-level `permissions: contents: read` with no job granting more. `make
check` fails on any change until the pin is updated, so a workflow change is
always a reviewed change to a trust file. CI holds no signing, release or
registry authority.

## Pins and anchors

The pins are written once by `make release-export-pin`, from the maintainer's
approved keys, and validated before anything is written; existing pins are
never overwritten. `make release-insert-anchors` derives the two fingerprint
anchors from the committed exports with `decernor` 0.1.8 or newer, twice, and
compares the results. `make release-validate-pin` reads the committed pins and
anchors as git blobs and checks them in an isolated keyring. The minisign
fingerprint covers the decoded public blob, not its comment or the whole file.

## Tags

A release is one tag, `vX.Y.Z`, which releases the root Go module (and the
Rust crate, once it is published). It must be annotated, signed by the
committed signing subkey, tagged by the committed tagger identity, carry exactly the generated message
(`recordprims X.Y.Z`), and
point at a commit whose root `go.mod` declares `github.com/3leaps/recordprims`.
Verification refuses a release where the tag is missing or points elsewhere.

Tag verification reads committed public data from the tagged commit in an
isolated keyring and requires the exact signing subkey of the approved primary,
which comes from the maintainer environment, never from the tree. The forge's
own "verified" flag is an additional check.

## Verify a release

Check which key actually signed the tag, not only that a signature verifies.
Import the public keys committed in the tagged commit into an isolated keyring
and read the signature status:

```bash
git fetch --tags origin
(
  set -eu
  tag=refs/tags/vX.Y.Z
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' EXIT
  git show "$tag:docs/security/release-signing-keys.asc" | gpg --homedir "$tmp" --import
  if ! status="$(GNUPGHOME="$tmp" git verify-tag --raw "$tag" 2>&1)"; then
    echo "tag signature did not verify" >&2
    exit 1
  fi
  printf '%s\n' "$status" | grep '^\[GNUPG:\] VALIDSIG '
)
```

The `VALIDSIG` line names the signing subkey fingerprint first and the primary
key fingerprint last. Compare both with values obtained through an
independent channel; the committed `config/release/signing-subkey.txt` and
`keys/expected-fingerprints.txt` must match them too, but they travel with the
tag and are not independent. A plain `git verify-tag` in your own keyring
reports `No public key` until the key is imported. Maintainers run the full
check with `make release-validate-pin` and `make release-verify-remote-tag`
(see [RELEASE_CHECKLIST.md](../../RELEASE_CHECKLIST.md), section 3).
