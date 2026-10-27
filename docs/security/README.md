# Release Signing and Verification

recordprims has not been released. This page states the release trust model
and the trust files that exist today; the signing pins and fingerprint anchors
are added before the first release.

## Release identity

A release is one signed annotated tag, `vX.Y.Z`, at a commit of `main`. It
releases the Rust crate and the root Go module (`github.com/3leaps/recordprims`)
together. Official releases will use GPG-signed tags and `SHA256SUMS` and
`SHA512SUMS` manifests, each carrying a minisign signature and an OpenPGP
signature by the committed signing subkey. These are separate signatures, and
all are required.

Establish the expected GPG primary key, the exact signing subkey, and the
minisign public-blob fingerprint through an independently trusted maintainer
channel before accepting downloaded public keys. A key distributed beside a
signature is not independent authentication.

## Trust files

| File | Contents |
| --- | --- |
| `config/release/tagger-identity.txt` | The exact git tagger identity for release tags |
| `config/release/release-workflow.sha256` | Digests of every reviewed workflow and local action |
| `config/ci-tools.sha256` | Digests of the tools CI downloads |
| `config/ci-python-requirements.txt` | Python packages the CI gates import, pinned by version and source-archive hash (copied in as pure Python; nothing built) |

## Workflows

Every workflow and local action is pinned by digest in
`config/release/release-workflow.sha256`, and every workflow must declare
top-level `permissions: contents: read` with no job granting more. `make
check` fails on any change until the pin is updated, so a workflow change is
always a reviewed change to a trust file. CI holds no signing, release or
registry authority.
