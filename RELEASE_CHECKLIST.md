# Release Checklist

How a maintainer cuts a recordprims release. A release is one signed annotated
tag, `vX.Y.Z`, at a commit of `main`; it releases the Rust crate and the root Go
module together. The signing material and the trust rules are in
[docs/security/README.md](docs/security/README.md).

Signing, tagging, publishing and registry uploads are maintainer steps on the
maintainer's machine. CI holds no signing, release or registry authority.

## Prerequisites

- `gpg`, `minisign`, `gh`, `jq` and `python3` (3.11 or newer). `make doctor`
  reports each tool's version against the recommended minimum in
  `.goneat/tools.yaml` and fails on any tool that is missing, older, or whose
  version it cannot read.
- decernor 0.1.8 or newer as a regular file at an absolute path.
- limensafe and the maintainer's catalog, which the pre-tag gate requires for
  the public-surface scan (it never runs in CI).
- The ceremony environment (outside this repository):

  ```bash
  export RECORDPRIMS_RELEASE_TAG="vX.Y.Z"                  # explicit; never derived
  export RECORDPRIMS_TAG_MESSAGE_DIR="/path/outside/the/repository/$RECORDPRIMS_RELEASE_TAG"
  export RECORDPRIMS_DECERNOR_BIN=/absolute/path/to/decernor
  export RECORDPRIMS_TAGGER_NAME='3 Leaps Infosec Team'    # config/release/tagger-identity.txt
  export RECORDPRIMS_TAGGER_EMAIL=infosec@3leaps.net
  export RECORDPRIMS_PGP_KEY_ID='<signing subkey>!'        # config/release/signing-subkey.txt
  export RECORDPRIMS_GPG_HOMEDIR=/path/outside/the/repository
  export RECORDPRIMS_GPG_SIGNING_FINGERPRINT='<primary>'   # approved primary, from a trusted channel
  export RECORDPRIMS_RELEASE_ANCHOR_FILE=/path/outside/the/repository/anchor
  export LIMENSAFE_CATALOG_PATH=/path/outside/the/repository/catalog
  ```

  A bare `RELEASE_TAG` is refused by `make`.

## 0. First adoption only: public pins

The pins are written once and land on `main` through review before any release
uses them.

- [ ] With `RECORDPRIMS_GPG_HOMEDIR`, `RECORDPRIMS_PGP_KEY_ID`,
  `RECORDPRIMS_MINISIGN_PUB_EXPORT` (the approved minisign public key) and
  `RECORDPRIMS_DECERNOR_BIN` loaded: `make release-export-pin`. It writes
  `docs/security/release-signing-keys.asc`,
  `docs/security/recordprims-minisign.pub` and
  `config/release/signing-subkey.txt`, refusing to overwrite and writing
  nothing unless all three validate. Commit them, then
  `scripts/release-validate-pin.sh --pins-only`.
- [ ] `make release-insert-anchors`: decernor derives
  `keys/expected-fingerprints.{txt,ndjson}` from the committed exports, twice,
  and compares. Commit them.
- [ ] `make release-validate-pin`. The anchors must match the organization's
  published release fingerprints; a mismatch is a stop.

## Changing a workflow

Every file under `.github/workflows/` and `.github/actions/` is pinned by
digest in `config/release/release-workflow.sha256`; every workflow must declare
top-level `permissions: contents: read` with no job granting more. Editing one
fails `make check` until the pin is updated, and tag verification checks the
pin in the tagged commit. To change one:

- [ ] Edit the workflow or action and update its line in the pin file in the
  same change (`shasum -a 256 <file>`).
- [ ] Have the change reviewed as a trust-file change.

## 1. Release pack

- [ ] All in-scope PRs merged; on `main`, clean, in sync with `origin/main`.
- [ ] Set the version through the Makefile, never by hand: `make version-patch`,
  `version-minor`, `version-major` or `version-set V=X.Y.Z`. `VERSION` is the
  single source of truth; the target writes it to `Cargo.toml` and
  `Cargo.lock`, and `make version-check` confirms every site agrees.
- [ ] `CHANGELOG.md`: move `[Unreleased]` to `## [X.Y.Z] - YYYY-MM-DD`, and
  **advance the compare links by hand**: `[Unreleased]: .../compare/vX.Y.Z...HEAD`
  and `[X.Y.Z]: .../compare/vPREV...vX.Y.Z` (the first release links to its
  tag). Every `## [x.y.z]` heading needs its footer definition.
- [ ] `docs/releases/vX.Y.Z.md`: the permanent notes for this release,
  starting with `# vX.Y.Z`.
- [ ] `RELEASE_NOTES.md`: add `## vX.Y.Z - YYYY-MM-DD` at the top, summarizing
  the release and linking its `docs/releases/` notes. Keep the latest three
  releases; remove the oldest when adding a fourth (its notes stay in
  `docs/releases/`).
- [ ] Read `README.md` through and correct anything the release changes
  (status, bindings, supported platforms and toolchains).
- [ ] Merge the release pack with an exact squash message whose body ends with
  the standard attribution footer, then check it: `git log -1 --format=%B`.
- [ ] `make release-preflight` on the merged `main`: the checker, a clean tree
  on `main` equal to `origin/main`, `make check`, the tool and ceremony
  controls, the required release-scope surface scan, the changelog entry, the
  release notes and `docs/releases/vX.Y.Z.md`.
- [ ] Main CI is green on this exact commit. Preflight alone is not enough.

## 2. Tag

- [ ] `make release-prepare-tag-message`. It writes `vX.Y.Z.txt` in
  `RECORDPRIMS_TAG_MESSAGE_DIR`, already complete: one line,
  `recordprims X.Y.Z`. Nothing needs editing; the tag must carry exactly
  that message.
- [ ] `make release-validate-pin`.
- [ ] `make release-tag`: the tag, signed by the committed subkey, local only,
  at `HEAD`, which must equal `origin/main`.
- [ ] `make release-verify-tag`: message bytes, tagger, target, and the
  signature against the pin committed in the tagged commit.
- [ ] `make release-push-tag`: one push, then the remote object is
  re-verified. It prints the tag object and commit, and the exact
  `release-record-anchor` command for them.
- [ ] `make release-verify-remote-tag`: the tag verifies against the committed
  pin and the approved primary, and GitHub reports it verified.
- [ ] Record the ceremony identity once, outside the repository, and keep
  `RECORDPRIMS_RELEASE_ANCHOR_FILE` set for every later step: run the
  `RECORDPRIMS_EXPECTED_TAG_OBJECT=... RECORDPRIMS_EXPECTED_COMMIT=... make release-record-anchor`
  line that `make release-push-tag` printed, after checking both values.

## 3. After the tag

- [ ] `go list -m github.com/3leaps/recordprims@vX.Y.Z` resolves.
- [ ] Someone other than the publisher verifies the tag and the anchors.
