# Releasing Brokk Release Bot

## Destinations and ordering

The release version comes from a new `vX.Y.Z` tag (or `-alpha.N`, `-beta.N`,
`-rc.N`). Never move a completed release's tag. Unreleased commits after v0.5.1
use a new version; the worktree feature starts the v0.6.0 release line.

1. Build native Linux/macOS archives for amd64/arm64 and installer packages.
2. Validate every destination's version and actual publishing identity.
3. Publish four npm platform packages, then their dependent launcher:
   - `@brokkai/release-bot-linux-x64`
   - `@brokkai/release-bot-linux-arm64`
   - `@brokkai/release-bot-darwin-x64`
   - `@brokkai/release-bot-darwin-arm64`
   - `@brokkai/release-bot` (exact-version optional platform dependencies)
4. Verify all npm packages, then upload GitHub assets to a private draft,
   check their exact uploaded bytes, and finalize the GitHub release.

GitHub assets are `brokk-release-bot-TAG-OS-ARCH.tar.gz` for all four targets,
`checksums.txt`, and `release.json`. Each archive contains `brb`, `README.md`,
`BUILD.json` (exact commit/tag/platform), `LICENSE`, `NOTICE`, and
`licenses/THIRD_PARTY_NOTICES.txt`.

The version tag also exposes the Go module `github.com/BrokkAi/release-bot`
with command `./cmd/brb`. It has no separate registry upload: verify the exact
remote tag and committed module metadata as part of the GitHub destination.
There are no containers, crates, Maven packages, documentation deployments,
update feeds, signing services, or notarization targets.

PyPI `brokk-release-bot` remains deferred, as before: its trusted publisher is
not configured. The Python wheel/sdist are built and locally tested but are not
release destinations. Selecting `all` or `pypi` fails closed until that destination
has a real non-publishing authorization check. Do not infer PyPI rights from npm.

## Prepare without publication

Read the repository instructions and fetch `origin/master`. Preserve all local
work on the job's unique topic branch. Push preparation changes and merge their
PR through the repository rules; never push preparation directly to master.
Branch pushes and pull requests run `ci.yml` and do not publish.

Fetch after merging and check out the actual merged SHA detached. Choose an
unused version, and dispatch from master only if master still equals that SHA:

```sh
gh workflow run publish-packages.yml --repo github.com/BrokkAi/release-bot --ref master -f tag=v0.6.0 -F publish=false -f registry=npm
gh run list --repo github.com/BrokkAi/release-bot --commit FULL_SHA --limit 100
```

`publish-packages.yml` calls the existing `release.yml` validation and `ci.yml`.
The `packages` job runs in `packages-publish` with `contents: write` and
`id-token: write`. It builds all deliverables from the exact checkout without
requiring a public release or tag. Its actual GitHub token creates/updates an
empty private draft to prove write access. No final assets or registry packages
are uploaded in preflight. The ordinary Actions package artifact is retained as
build evidence. The empty draft is disposable and is recreated during publication.

For every npm package, `scripts/npm_authorization.py` exchanges this same job's
GitHub OIDC identity with npm for a package-scoped token, checks its type and
expiry, then discards it without logging or saving it. npm must accept the
configured repository/workflow/environment trust for each package; public
metadata or a local developer login is not authorization evidence. See the
[npm registry OIDC API](https://api-docs.npmjs.com/#tag/OIDC).
The exchange establishes package-scoped identity evidence. It does not prove
permission for direct publication rather than staging; npm enforces that grant
on the first final registry upload. Keep the GitHub draft private if upload fails.
The existing trusted publishers must name BrokkAi/release-bot,
`publish-packages.yml`, and `packages-publish`. A legacy `NPM_TOKEN` secret
blocks this check, because checking OIDC would not validate that other identity.
Remove the legacy secret only after confirming the intended OIDC configuration.
Required environment approvals remain required. Failed exchanges identify the
exact package needing its owner's trusted-publisher repair; do not test by upload.

Require completed successful exact-SHA runs and all validation jobs. Poll
`gh run view RUN_ID --repo github.com/BrokkAi/release-bot --json status,conclusion`
at 15–30 second intervals. Inspect failed job logs before repairing a failure.
An unrelated or earlier successful SHA is insufficient. Required workflow files:
`ci.yml`, `release.yml`, `publish-packages.yml`.

Daemon commands use `RELEASE_COMMIT`, `RELEASE_TAG`, and `RELEASE_TARGET`:

```sh
# Build evidence for every destination, including all native builds and CI:
python3 scripts/package_check.py build
# Same actual packages job's GitHub authorization:
python3 scripts/package_check.py github-authorization
# Same actual packages job's npm authorization, all five packages:
python3 scripts/package_check.py authorization
# Mutable version gates:
python3 scripts/release_check.py version
python3 scripts/package_check.py version
# Independent post-publication verification:
python3 scripts/release_check.py published
python3 scripts/package_check.py published
```

Evidence checks reject absent/failed/incomplete/wrong-SHA runs, missing publisher
steps, and missing/expired exact-SHA build artifacts. Registry checks rebuild in
a temporary directory and check all five destinations; any conflict fails.

## Publication (separate phase)

After all preflight gates succeed, dispatch the exact prepared commit's branch
or existing tag through `publish-packages.yml` with `publish=true`. If master
advanced, do not dispatch its new commit for this release. A semantic version
tag push also invokes publication and is prohibited during preflight.

```sh
gh workflow run publish-packages.yml --repo github.com/BrokkAi/release-bot --ref master -f tag=v0.6.0 -F publish=true -f registry=npm
```

`release.yml` is now validation-only; `publish=true` there fails with directions
to the complete pipeline. GitHub's final release operation creates the exact tag;
this workflow already handles npm and does not rely on a GITHUB_TOKEN-created
tag starting another workflow.

Before its first upload, the pipeline checks native/package metadata, all version
conflicts, GitHub write access, and the actual npm identity. The publishing step
refreshes authorization. Platform packages precede the launcher. A failed npm
upload leaves GitHub unpublished. Retries reuse identical existing versions and
resume missing packages; they never overwrite conflicting immutable versions.
All npm packages must be visible and verified before GitHub is finalized. Registry
propagation has a bounded wait; a timeout requires verification/retry, not an
unconditional re-upload. Existing public GitHub assets are read-only verified.

Uploaded native bytes are compared to the exact same-job staged bytes. Later
independent verification validates downloaded archives against their published
manifest/checksums, then compares unpacked files, permissions and metadata to
the expected build. npm verification similarly validates the downloaded tarball's
published SHA-512 integrity and compares every file and permission to the expected
package. Cross-machine gzip encoding differences are allowed; payload changes fail.

## Local validation and recovery

```sh
make check build
python3 -m unittest discover -s scripts -p '*_test.py' -v
python3 scripts/licenses.py
python3 scripts/smoke_installers.py
```

Use the Go version in `go.mod`, Node.js 24, Python 3, and uv 0.12.3. CI runs Go
race tests/vet, CLI smoke tests, release tooling and real local npm/uv installation
on Linux and macOS, plus actionlint. Follow `licenses/README.md` for dependency
changes. Native/package builds repeat legal-file validation.

A partial release is not complete until both independent published checks pass.
Keep its immutable version and exact commit, inspect matching drafts and existing
packages, and resume the same pipeline. Old `untagged-*` draft aliases from earlier
releases are historical recovery records; they do not authorize reusing completed
tags or overwriting their assets.
