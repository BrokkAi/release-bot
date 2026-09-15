#!/usr/bin/env python3
"""Independent non-publishing release checks and complete npm verification."""
import argparse
from pathlib import Path
import tempfile

import package_installers
import package_registry
import release
import release_check

WORKFLOW = ".github/workflows/publish-packages.yml"
AUTH_STEP = "Validate actual npm publisher without uploading"
BUILD_STEP = "Test launchers and build packages from verified assets"
GITHUB_STEP = "Validate GitHub publisher and all destination versions"


def evidence(step):
    sha, tag = release_check.context()
    github = release.GitHub(release_check.repository())
    runs = github.api(f"{github.base}/actions/workflows/publish-packages.yml/runs?head_sha={sha}&event=workflow_dispatch&per_page=100")["workflow_runs"]
    matches = [r for r in runs if r["head_sha"] == sha and r["event"] == "workflow_dispatch"
               and r["path"] == WORKFLOW and r["display_title"] == f"Release {tag} (publish=false)"]
    if not matches:
        raise ValueError("missing exact-commit/tag package preflight run")
    run = max(matches, key=lambda r: r["id"])
    if run["status"] != "completed" or run["conclusion"] != "success":
        raise ValueError(f"package preflight run {run['id']} is not successful")
    jobs = github.api(f"{github.base}/actions/runs/{run['id']}/jobs?per_page=100")["jobs"]
    if not jobs or any(j["status"] != "completed" or j["conclusion"] != "success" for j in jobs):
        raise ValueError("missing, incomplete, or failed preflight jobs")
    publisher = [j for j in jobs if j["name"] == "packages"]
    if len(publisher) != 1:
        raise ValueError("missing actual packages publishing job")
    steps = [s for s in publisher[0]["steps"] if s["name"] == step]
    if len(steps) != 1 or steps[0]["status"] != "completed" or steps[0]["conclusion"] != "success":
        raise ValueError(f"missing successful publisher step: {step}")
    artifacts = github.api(f"{github.base}/actions/runs/{run['id']}/artifacts?per_page=100")["artifacts"]
    expected = [a for a in artifacts if a["name"] == f"packages-{tag}" and not a["expired"] and a["size_in_bytes"] > 0]
    if len(expected) != 1 or expected[0].get("workflow_run", {}).get("head_sha") != sha:
        raise ValueError("missing unexpired exact-commit package build artifact")
    print(f"Run {run['id']} at {sha}: {step}; packages-publish environment; validated package artifact {expected[0]['id']}")


def registry(command):
    sha, tag = release_check.context()
    with tempfile.TemporaryDirectory() as temp:
        root = Path(temp)
        release.package(tag, root / "native")
        package_installers.package(tag, root / "native", root / "packages", sha)
        package_registry.run(command, root / "packages", "npm")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("check", choices=("build", "authorization", "github-authorization", "version", "published"))
    args = parser.parse_args()
    if args.check in ("build", "authorization", "github-authorization"):
        evidence({"build": BUILD_STEP, "authorization": AUTH_STEP, "github-authorization": GITHUB_STEP}[args.check])
    else:
        registry("check" if args.check == "version" else "verify")
