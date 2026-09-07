#!/usr/bin/env python3
"""Check, publish, and verify the built npm and PyPI packages; retries require identical bytes."""

import argparse
import json
from pathlib import Path
import subprocess
import time
import urllib.error
import urllib.parse
import urllib.request

import package_installers
import release


def fetch_json(url):
    try:
        with urllib.request.urlopen(url, timeout=60) as response:
            return json.load(response)
    except urllib.error.HTTPError as error:
        error.close()
        if error.code == 404:
            return None
        raise


def npm_exists(package):
    name = urllib.parse.quote(package["name"], safe="")
    record = fetch_json(f"https://registry.npmjs.org/{name}/{package['version']}")
    if record is None:
        return False
    if record.get("name") != package["name"] or record.get("version") != package["version"] or record.get("dist", {}).get("integrity") != package["integrity"]:
        raise ValueError(f"published npm package differs from staged bytes: {package['name']}")
    return True


def python_files(directory):
    # uv may create a hidden .gitignore in its output directory; only the
    # distribution files are registry inputs.
    files = sorted(path for path in directory.iterdir() if not path.name.startswith("."))
    if len(files) != 2 or sum(p.name.endswith(".whl") for p in files) != 1 or sum(p.name.endswith(".tar.gz") for p in files) != 1:
        raise ValueError("expected exactly one Python wheel and one sdist")
    return {p.name: release.digest(p.read_bytes()) for p in files}


def python_exists(version, expected):
    record = fetch_json(f"https://pypi.org/pypi/brokk-release-bot/{version}/json")
    if record is None:
        return False
    found = {item["filename"]: item["digests"]["sha256"] for item in record["urls"]}
    for filename, checksum in found.items():
        if expected.get(filename) != checksum:
            raise ValueError(f"published PyPI file differs from staged bytes: {filename}")
    return found == expected


def wait_visible(check):
    for attempt in range(30):
        if check():
            return
        if attempt < 29:
            time.sleep(2)
    raise ValueError("published package did not become visible within 60 seconds; retry verification")


def run(command, directory, registry="all"):
    if registry not in ("all", "npm", "pypi"):
        raise ValueError("registry must be all, npm or pypi")
    manifest = json.loads((directory / "npm/manifest.json").read_text())
    npm_version, python_version = package_installers.versions(manifest["tag"])
    expected_names = {package_installers.NPM_ROOT} | {
        f"{package_installers.NPM_ROOT}-{system}-{arch}" for system in ("linux", "darwin") for arch in ("x64", "arm64")
    }
    packages = manifest["packages"]
    if len(packages) != 5 or {p["name"] for p in packages} != expected_names:
        raise ValueError("manifest must contain all five npm packages exactly once")
    packages.sort(key=lambda p: p["name"] == package_installers.NPM_ROOT)
    for package in packages:
        if package["version"] != npm_version or Path(package["filename"]).name != package["filename"]:
            raise ValueError("invalid npm package version or filename")
        if release.digest((directory / "npm" / package["filename"]).read_bytes()) != package["sha256"]:
            raise ValueError(f"corrupt staged npm package: {package['filename']}")
    if registry == "pypi":
        packages = []
    expected_python = python_files(directory / "python") if registry != "npm" else {}
    # Discover conflicts in every destination before making the first write.
    existing = {p["name"]: npm_exists(p) for p in packages}
    existing_python = python_exists(python_version, expected_python) if expected_python else True
    if command == "check":
        print("Package versions are available or identical. This checks availability, not publishing authorization.")
        return
    if command == "verify":
        if not all(existing.values()) or not existing_python:
            raise ValueError("publication is incomplete: a selected package or distribution is missing")
        print(f"All selected packages ({registry}) match the staged bytes")
        return
    for package in packages:
        if not existing[package["name"]]:
            subprocess.run(["npm", "publish", str((directory / "npm" / package["filename"]).resolve()),
                            "--access", "public", "--registry", "https://registry.npmjs.org",
                            "--tag", "next" if "-" in npm_version else "latest"], check=True)
        wait_visible(lambda: npm_exists(package))
    if not existing_python:
        subprocess.run(["uv", "publish", "--trusted-publishing", "always", "--check-url", "https://pypi.org/simple/",
                        *[str((directory / "python" / name).resolve()) for name in expected_python]], check=True)
    if expected_python:
        wait_visible(lambda: python_exists(python_version, expected_python))
    print(f"Published and verified selected packages ({registry})")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=("check", "publish", "verify"))
    parser.add_argument("directory", type=Path)
    parser.add_argument("--registry", choices=("all", "npm", "pypi"), default="all")
    args = parser.parse_args()
    run(args.command, args.directory, args.registry)


if __name__ == "__main__":
    main()
