#!/usr/bin/env python3
"""Reproducible publishability and publication checks for release-bot."""

import argparse
import os
from pathlib import Path
import re
import tempfile

import release


PREFLIGHT_STEP = "Check every artifact and the actual publishing credential"
PUBLISHER_JOB = "Validate publisher and optionally publish"
RELEASE_WORKFLOW = ".github/workflows/release.yml"


def required(name):
    value = os.environ.get(name, "").strip()
    if not value:
        raise ValueError(f"{name} is required")
    return value


def context():
    sha = required("RELEASE_COMMIT")
    tag = required("RELEASE_TAG")
    target = required("RELEASE_TARGET")
    if not re.fullmatch(r"[0-9a-f]{40}", sha) or not re.fullmatch(r"[0-9a-f]{40}", target):
        raise ValueError("RELEASE_COMMIT and RELEASE_TARGET must be full lowercase Git hashes")
    release.validate_tag(tag)
    if release.commit() != sha:
        raise ValueError("checkout HEAD is not RELEASE_COMMIT")
    release.run("git", "merge-base", "--is-ancestor", target, sha)
    return sha, tag


def repository():
    configured = os.environ.get("GH_REPO", "").strip()
    if configured:
        return configured
    remote = release.run("git", "remote", "get-url", "origin").decode().strip()
    patterns = (
        r"https://github\.com/([^/]+/[^/]+?)(?:\.git)?$",
        r"git@github\.com:([^/]+/[^/]+?)(?:\.git)?$",
        r"ssh://git@github\.com/([^/]+/[^/]+?)(?:\.git)?$",
    )
    for pattern in patterns:
        match = re.fullmatch(pattern, remote)
        if match:
            return match.group(1)
    raise ValueError("origin is not a supported github.com repository URL")


def matching_releases(github, tag):
    matches = [item for item in github.pages(f"{github.base}/releases?per_page=100") if item["tag_name"] == tag]
    if len(matches) > 1:
        raise ValueError("multiple GitHub releases use RELEASE_TAG")
    return matches


def build():
    sha, tag = context()
    go_files = release.run("git", "ls-files", "*.go").decode().splitlines()
    if go_files and release.run("gofmt", "-l", *go_files).strip():
        raise ValueError("tracked Go files are not formatted")
    release.run("go", "test", "-race", "./...")
    release.run("go", "vet", "./...")
    release.run("go", "build", "-o", os.devnull, "./cmd/brb")
    release.run("python3", "-m", "unittest", "discover", "-s", "scripts", "-p", "*_test.py", "-v")
    with tempfile.TemporaryDirectory() as temporary:
        directory = Path(temporary)
        release.package(tag, directory)
        release.verify_local(tag, directory, sha)
    print(f"Build, tests, packaging, metadata and checksums passed for {tag} at {sha}")


def selected_preflight_run(github, sha, tag):
    response = github.api(
        f"{github.base}/actions/workflows/release.yml/runs"
        f"?head_sha={sha}&event=workflow_dispatch&per_page=100"
    )
    title = f"Release {tag} (publish=false)"
    matches = [
        run for run in response.get("workflow_runs", [])
        if run.get("head_sha") == sha
        and run.get("event") == "workflow_dispatch"
        and run.get("path") == RELEASE_WORKFLOW
        and run.get("display_title") == title
    ]
    if not matches:
        raise ValueError("no exact-commit, exact-tag non-publishing Release workflow run exists")
    run = max(matches, key=lambda item: item["id"])
    if run.get("status") != "completed" or run.get("conclusion") != "success":
        raise ValueError(f"preflight workflow run {run['id']} did not complete successfully")
    return run


def authorization():
    sha, tag = context()
    github = release.GitHub(repository())
    matches = matching_releases(github, tag)
    tag_sha = github.tag_commit(tag)
    if tag_sha is not None and tag_sha != sha:
        raise ValueError("RELEASE_TAG already belongs to another commit")
    if matches and not matches[0].get("draft"):
        # Recovery is verification only; no new publishing permission is needed.
        verify_published(github, matches[0], tag, sha)
        print(f"{tag} is already published and verified; no publication write is needed")
        return
    if matches and matches[0].get("target_commitish") != sha:
        raise ValueError("existing draft targets another commit")
    if not matches and tag_sha is not None:
        raise ValueError("RELEASE_TAG exists without a recoverable GitHub release")
    run = selected_preflight_run(github, sha, tag)
    jobs = github.api(f"{github.base}/actions/runs/{run['id']}/jobs?per_page=100").get("jobs", [])
    if not jobs or any(job.get("status") != "completed" or job.get("conclusion") != "success" for job in jobs):
        raise ValueError("the exact preflight run has missing, incomplete, or failed jobs")
    publisher = [job for job in jobs if job.get("name") == PUBLISHER_JOB]
    if len(publisher) != 1:
        raise ValueError("the exact preflight run does not contain one publisher validation job")
    steps = [step for step in publisher[0].get("steps", []) if step.get("name") == PREFLIGHT_STEP]
    if len(steps) != 1 or steps[0].get("status") != "completed" or steps[0].get("conclusion") != "success":
        raise ValueError("the actual github.token publisher validation step did not pass")
    # The completed publishing-token check is the evidence, not the continued
    # existence of its empty draft. The publishing job recreates a deleted draft
    # and checks its own actual token again before uploading any assets.
    print(f"Actions run {run['id']} passed publisher authorization for {tag} at {sha}; the draft is optional")


def verify_published(github, release_record, tag, sha):
    if release_record.get("draft") or not release_record.get("published_at"):
        raise ValueError("GitHub release is not public and complete")
    if github.tag_commit(tag) != sha:
        raise ValueError("published tag does not resolve to RELEASE_COMMIT")
    with tempfile.TemporaryDirectory() as temporary:
        directory = Path(temporary)
        release.package(tag, directory)
        github.verify_published_assets(release_record, directory, sha)


def version():
    sha, tag = context()
    github = release.GitHub(repository())
    tag_sha = github.tag_commit(tag)
    if tag_sha is not None and tag_sha != sha:
        raise ValueError("RELEASE_TAG already belongs to another commit")
    matches = matching_releases(github, tag)
    if not matches:
        if tag_sha is not None:
            raise ValueError("RELEASE_TAG exists without a recoverable GitHub release")
        print(f"{tag} is available on {github.repo}")
        return
    record = matches[0]
    if record.get("draft"):
        if record.get("target_commitish") != sha:
            raise ValueError("existing draft targets another commit")
        print(f"{tag} is reserved by matching private draft {record['id']} for safe recovery")
        return
    verify_published(github, record, tag, sha)
    print(f"{tag} is already published with verified checksums and matching archive contents")


def published():
    sha, tag = context()
    github = release.GitHub(repository())
    matches = matching_releases(github, tag)
    if len(matches) != 1:
        raise ValueError("exactly one GitHub release must exist for RELEASE_TAG")
    verify_published(github, matches[0], tag, sha)
    print(f"Verified the public {tag} tag, all six asset checksums and matching archive contents at {sha}")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("check", choices=("build", "authorization", "version", "published"))
    args = parser.parse_args()
    globals()[args.check]()


if __name__ == "__main__":
    main()
