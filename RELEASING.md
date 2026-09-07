# Releasing Brokk Release Bot

This repository publishes a GitHub release containing four archives: Linux and
macOS, each for amd64 and arm64. Every archive contains `brb`, the Apache
2.0 `LICENSE`, `README.md`, and `BUILD.json` identifying its exact commit, version,
and platform. Archives are named `brokk-release-bot-TAG-OS-ARCH.tar.gz`;
`checksums.txt` and `release.json` accompany them. The same binaries are distributed
through npm as `@brokkai/release-bot` and through the PyPI launcher
`brokk-release-bot` for uv. Every method installs the `brb` command.
The Go module remains `github.com/BrokkAi/release-bot`, with its command at `cmd/brb`.
Pushing a semantic version tag makes the module installable with `go install`;
there is no separate Go registry upload. Existing state stays under `release-bot`.

## Checks on every change

`ci.yml` runs on pushes, pull requests, and manual dispatch. It checks formatting,
race tests, `go vet`, the CLI build/help, and release tooling on Linux and macOS.
It also tests curl and Python checksum failures, npm argument/signal forwarding,
builds all installer packages, and smoke-tests actual local npm and uv installs.
Actionlint checks workflow syntax and shell commands. Official actions are pinned
to commits; Dependabot proposes updates weekly.

## Prepare without publishing

Commit and push preparation changes to `master`. Choose an unused version such as
`v0.1.0` (or `v0.1.0-rc.1` for a prerelease), then run:

```sh
gh workflow run release.yml --repo BrokkAi/release-bot --ref master -f tag=v0.1.0
gh run list --repo BrokkAi/release-bot --workflow release.yml --limit 5
gh run watch RUN_ID --repo BrokkAi/release-bot --exit-status
```

Confirm the run's `headSha` is the intended prepared commit. The workflow reruns
all CI, builds all four archives with the Go version in `go.mod`, validates their
contents and checksums without uploading them. Its publishing job uses the
automatic `GITHUB_TOKEN` with `contents: write`.
It creates or updates an **unpublished draft** using that actual token to verify
write access. No personal access token or package-registry secret is required.
Missing rights fail here; no release assets are uploaded to the draft in this mode.

The empty draft is disposable: deleting it does not invalidate the successful
workflow authorization check. The publishing job recreates it with its own
credential and rechecks write access before uploading. A public release or tag
for the same version must match the intended commit. A draft targeting another
commit blocks; do not overwrite it automatically. Resolve the unused draft or
choose a new version and repeat preflight. Never move a public tag.

## Publish

Run the same workflow and version with publication enabled:

```sh
gh workflow run release.yml --repo BrokkAi/release-bot --ref master -f tag=v0.1.0 -F publish=true
```

Keep `master` at the validated commit between preflight and this dispatch. If it
changed, prepare the new commit and version again. The workflow independently
repeats every check; an existing draft for another commit prevents publication.
Only dispatch from `master` is supported, so the code, workflow, and run SHA agree.

After preflight, all six files are uploaded to the private draft, downloaded and
checked byte-for-byte. Only a complete set can become public. GitHub creates the
tag at the exact prepared commit when publishing. The script then verifies the
public release, remote tag, and all downloads again. Stable releases become
latest; prereleases do not. Public artifacts are never overwritten on retry.

No workflow publishes automatically on a code push. The release bot decides when
new commits need a release, prepares a plan, runs the preflight dispatch, and uses
`publish=true` only in its publication phase. Include `ci.yml` and `release.yml`
among the required workflows in that plan. Their Actions runs must pass for the
exact release commit.

The release bot's reproducible destination checks read `RELEASE_COMMIT`,
`RELEASE_TAG`, and `RELEASE_TARGET` from the environment. Run them only after the
successful preflight dispatch above:

```sh
python3 scripts/release_check.py build
python3 scripts/release_check.py authorization
python3 scripts/release_check.py version
```

The authorization check finds the successful `Release` preflight run for the
exact commit and tag, requires every job to have passed, verifies the publisher
step used that run's `github.token`. A remaining draft must target the same
commit, but the draft need not still exist. The publishing job checks its actual
credential again before uploads. An already-public release goes directly to
verification without requiring another private draft or publishing permission.
The checker does not treat this machine's `gh` login as the publisher. The version check rejects conflicting tags/releases and permits an
exact matching draft or a complete immutable published release during recovery.
After publication, `python3 scripts/release_check.py published` rebuilds the
assets, validates the downloads against their own published manifest and
checksums, then compares every unpacked file and its permissions with the rebuild.
Gzip encoding may differ across machines even when the archive contents are
identical. Same-job staging checks still compare exact uploaded bytes.

## npm and PyPI installers

The installer packaging follows the sibling Anvil and Mjolnir projects: npm uses
four native platform packages plus a launcher, and Python uses a small launcher
that downloads a pinned GitHub archive. Python packages embed the archive and
binary hashes, so no Go toolchain or curl is required by either package manager.
The Python launcher replaces itself with `brb`; the npm launcher forwards
arguments, terminal streams, exit status, and termination signals.

`scripts/package_installers.py` validates all native archives against the release
manifest and the checkout's exact commit before packaging. Versions come from the
tag. Use `vX.Y.Z`, `vX.Y.Z-alpha.N`, `vX.Y.Z-beta.N`, or `vX.Y.Z-rc.N`:
Python maps these prereleases to `X.Y.ZaN`, `X.Y.ZbN`, and `X.Y.ZrcN`, while npm
keeps the SemVer spelling and publishes prereleases under `next`.

Before the first registry publication, configure the GitHub environment
`packages-publish` and the publishing accounts:

- PyPI: add a pending trusted publisher for `brokk-release-bot`, owner `BrokkAi`,
  repository `release-bot`, workflow `publish-packages.yml`, environment
  `packages-publish`. PyPI supports [creating the project on first OIDC publication](https://docs.pypi.org/trusted-publishers/creating-a-project-through-oidc/).
- npm: establish publishing rights for `@brokkai/release-bot` and
  `@brokkai/release-bot-{linux,darwin}-{x64,arm64}` (five packages total).
  Configure each package's [trusted publisher](https://docs.npmjs.com/trusted-publishers/)
  with the same repository, workflow, and environment. If an initial publication
  needs a token, the workflow accepts an authorized granular token in the
  environment's `NPM_TOKEN` secret. Remove it after OIDC is configured.

Publish the native GitHub release first, then dispatch the package workflow
**from that exact tag**, which must include these installer changes:

```sh
gh workflow run publish-packages.yml --repo BrokkAi/release-bot --ref v0.1.0 -f tag=v0.1.0
```

The default run downloads and verifies the published native assets, builds the
five npm tarballs and Python wheel/sdist, tests local installation, checks
registry version availability, and tests the Python launcher's cold download.
It saves the packages as a workflow artifact. These checks make no registry
writes and do **not** establish publishing authorization. Verify the actual
trusted-publisher configuration and publishing rights before requesting writes.

```sh
gh workflow run publish-packages.yml --repo BrokkAi/release-bot --ref v0.1.0 -f tag=v0.1.0 -F publish=true
```

Publication checks every existing package for conflicts before uploading, publishes
and verifies all four npm platform packages before the root package, then publishes
the Python distributions with uv's [trusted publishing](https://docs.astral.sh/uv/guides/package/).
The workflow finishes with public npm and uv install smoke tests. Retries accept
existing files only when their hashes match the staged bytes; partial Python
uploads resume through `uv publish --check-url`. Preserve validated artifacts if
toolchain changes make a later rebuild differ. Never overwrite conflicting versions.

When driving this repository with the bot, enumerate GitHub, all five npm packages,
and PyPI in the publication plan. Include `publish-packages.yml` in its required
workflows and verify registry publication separately; the existing
`release_check.py published` command verifies only the native GitHub destination.
GitHub assets must be public before the Python launcher can download them, so
registry failure can leave a partial release. Reconcile it by rerunning the package
workflow for the same tag; do not mark the overall release successful until both
registries pass verification. Installer packaging can be validated locally before
publishing GitHub assets:

```sh
python3 scripts/package_installers.py v0.1.0 dist/native dist/packages
python3 scripts/smoke_installers.py --tag v0.1.0 --assets dist/native --packages dist/packages
python3 scripts/package_registry.py check dist/packages
python3 scripts/package_registry.py verify dist/packages
```

The first two commands build/test; `check` reads registry availability and
integrity; `verify` requires all files to be public and identical. For a clean
checkout, `python3 scripts/smoke_installers.py` builds native assets and packages
in temporary directories and smoke-tests both installers without publishing.
Package tooling additionally requires Node.js/npm and uv; CI tests Node.js 24.

## Recovery and local tooling

A failed upload or validation leaves the draft unpublished. Rerun the same
workflow for the same commit/version; private draft uploads can be replaced.
A retry finding an already-published release only verifies it and does not edit
its assets. Network or permission failures are reported, never treated as success.

Local packaging and checks need Go, Git, and Python 3; publishing also needs `gh`:

```sh
make check build
python3 -m unittest discover -s scripts -p '*_test.py' -v
python3 scripts/release.py package v0.1.0 dist
python3 scripts/release.py verify v0.1.0 dist
```

Use an empty output directory for packaging. Local packaging and `verify` make
no GitHub writes. `preflight` and `publish` additionally require `GH_REPO` and an
authenticated `gh` environment. The bot must use evidence from the actual Actions
publishing job; a successful invocation with a developer's token is insufficient.
