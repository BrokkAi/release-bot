# Releasing release-bot

This repository publishes a GitHub release containing four archives: Linux and
macOS, each for amd64 and arm64. Every archive contains `release-bot`, the Apache
2.0 `LICENSE`, `README.md`, and `BUILD.json` identifying its exact commit, version,
and platform. `checksums.txt` and `release.json` accompany the archives. There are
no package registry, container, signing, or notarization destinations currently.

## Checks on every change

`ci.yml` runs on pushes, pull requests, and manual dispatch. It checks formatting,
race tests, `go vet`, the CLI build/help, and release tooling on Linux and macOS.
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
