#!/usr/bin/env python3
"""Build and publish this project's GitHub assets with the standard library."""

import argparse
import gzip
import hashlib
import io
import json
import os
from pathlib import Path
import re
import subprocess
import tarfile
import tempfile

import licenses


TARGETS = ("linux-amd64", "linux-arm64", "darwin-amd64", "darwin-arm64")
TAG = re.compile(r"v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-[0-9A-Za-z]+(?:[.-][0-9A-Za-z]+)*)?")


def run(*args, data=None, env=None):
    return subprocess.run(args, input=data, env=env, check=True, stdout=subprocess.PIPE).stdout


def commit():
    return run("git", "rev-parse", "HEAD").decode().strip()


def digest(data):
    return hashlib.sha256(data).hexdigest()


def validate_tag(tag):
    if not TAG.fullmatch(tag):
        raise ValueError("tag must be a version such as v0.1.0 or v0.1.0-rc.1")


def archive_name(tag, target):
    return f"brokk-release-bot-{tag}-{target}.tar.gz"


def archive(path, files, timestamp):
    # Fixed tar metadata; gzip bytes can still differ across compressor versions.
    with path.open("wb") as output, gzip.GzipFile(filename="", fileobj=output, mode="wb", mtime=0) as compressed:
        with tarfile.open(fileobj=compressed, mode="w") as tar:
            for name, data in sorted(files.items()):
                entry = tarfile.TarInfo(name)
                entry.size = len(data)
                entry.mode = 0o755 if name == "brb" else 0o644
                entry.mtime = timestamp
                tar.addfile(entry, io.BytesIO(data))


def package(tag, directory):
    validate_tag(tag)
    if run("git", "status", "--porcelain").strip():
        raise ValueError("commit all preparation changes before packaging a release")
    licenses.check()
    directory.mkdir(parents=True, exist_ok=True)
    if any(directory.iterdir()):
        raise ValueError("package output directory must be empty")
    sha = commit()
    timestamp = int(run("git", "show", "-s", "--format=%ct", sha))
    manifest = {"tag": tag, "commit": sha, "assets": []}
    with tempfile.TemporaryDirectory() as temp:
        binary = Path(temp) / "brb"
        for target in TARGETS:
            goos, goarch = target.split("-")
            env = dict(os.environ, CGO_ENABLED="0", GOOS=goos, GOARCH=goarch,
                       GOWORK="off", GOFLAGS="-mod=readonly")
            run("go", "build", "-trimpath", "-buildvcs=false", f"-ldflags=-s -w -X main.version={tag}", "-o", str(binary), "./cmd/brb", env=env)
            metadata = {"tag": tag, "commit": sha, "target": target}
            name = archive_name(tag, target)
            archive(directory / name, {
                "brb": binary.read_bytes(),
                **licenses.legal_files(),
                "README.md": Path("README.md").read_bytes(),
                "BUILD.json": json.dumps(metadata, sort_keys=True).encode() + b"\n",
            }, timestamp)
            data = (directory / name).read_bytes()
            manifest["assets"].append({"name": name, "size": len(data), "sha256": digest(data)})
    (directory / "release.json").write_text(json.dumps(manifest, indent=2) + "\n")
    (directory / "checksums.txt").write_text("".join(f"{a['sha256']}  {a['name']}\n" for a in manifest["assets"]))
    if run("git", "status", "--porcelain").strip() or commit() != sha:
        raise ValueError("checkout changed during packaging; discard these assets and rebuild")
    verify_local(tag, directory, sha)
    print(f"Built and validated {len(TARGETS)} archives at {sha}")


def verify_local(tag, directory, sha):
    validate_tag(tag)
    manifest = json.loads((directory / "release.json").read_text())
    if manifest["tag"] != tag or manifest["commit"] != sha:
        raise ValueError("assets do not belong to the requested tag and exact checkout commit")
    expected = {archive_name(tag, target) for target in TARGETS}
    names = [a["name"] for a in manifest["assets"]]
    if len(names) != len(expected) or set(names) != expected:
        raise ValueError("manifest must include every supported platform exactly once")
    if {p.name for p in directory.iterdir()} != expected | {"release.json", "checksums.txt"}:
        raise ValueError("release directory contains missing or unexpected assets")
    for asset in manifest["assets"]:
        path = directory / asset["name"]
        data = path.read_bytes()
        if not data or len(data) != asset["size"] or digest(data) != asset["sha256"]:
            raise ValueError(f"corrupt release asset: {path.name}")
        with tarfile.open(path, "r:gz") as tar:
            entries = tar.getmembers()
            expected_entries = {"brb", "README.md", "BUILD.json", *licenses.LEGAL_FILES}
            if len(entries) != len(expected_entries) or {m.name for m in entries} != expected_entries:
                raise ValueError("archive has missing or unexpected contents")
            if any(not m.isfile() or m.size <= 0 for m in entries):
                raise ValueError("archive contains an invalid entry")
            if tar.getmember("brb").mode & 0o111 == 0:
                raise ValueError("release binary is not executable")
            for filename, expected_text in licenses.legal_files().items():
                if tar.extractfile(filename).read() != expected_text:
                    raise ValueError(f"archive legal file does not match the checkout: {filename}")
            metadata = json.load(tar.extractfile("BUILD.json"))
            target = next(t for t in TARGETS if archive_name(tag, t) == path.name)
            if metadata != {"tag": tag, "commit": sha, "target": target}:
                raise ValueError("archive build metadata does not match the release")
    checksums = "".join(f"{a['sha256']}  {a['name']}\n" for a in manifest["assets"])
    if (directory / "checksums.txt").read_text() != checksums:
        raise ValueError("checksum list does not match all release assets")
    return manifest


def compare_contents(tag, expected, downloaded, sha):
    """Verify each package's own checksums, then compare its actual payload."""
    verify_local(tag, expected, sha)
    verify_local(tag, downloaded, sha)
    for target in TARGETS:
        name = archive_name(tag, target)
        with tarfile.open(expected / name, "r:gz") as local, tarfile.open(downloaded / name, "r:gz") as remote:
            for member in local.getmembers():
                other = remote.getmember(member.name)
                if member.mode != other.mode or local.extractfile(member).read() != remote.extractfile(other).read():
                    raise ValueError(f"published archive content mismatch: {name}/{member.name}")


class GitHub:
    def __init__(self, repo):
        if not re.fullmatch(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", repo):
            raise ValueError("GH_REPO must be owner/repository")
        self.repo = repo
        self.base = f"repos/{repo}"

    def api(self, path, payload=None, method="GET"):
        args = ["gh", "api", "--method", method, path]
        data = None
        if payload is not None:
            args += ["--input", "-"]
            data = json.dumps(payload).encode()
        return json.loads(run(*args, data=data))

    def pages(self, path):
        pages = json.loads(run("gh", "api", "--paginate", "--slurp", path))
        return [entry for page in pages for entry in page]

    def tag_commit(self, tag):
        refs = self.api(f"{self.base}/git/matching-refs/tags/{tag}")
        found = [r for r in refs if r["ref"] == f"refs/tags/{tag}"]
        if not found:
            return None
        obj = found[0]["object"]
        for _ in range(8):
            if obj["type"] == "commit":
                return obj["sha"]
            if obj["type"] != "tag":
                break
            obj = self.api(f"{self.base}/git/tags/{obj['sha']}")["object"]
        raise ValueError("release tag does not resolve to a commit")

    def prepare(self, tag, sha):
        existing_tag = self.tag_commit(tag)
        if existing_tag is not None and existing_tag != sha:
            raise ValueError("existing tag belongs to another commit; refusing to move it")
        matches = [r for r in self.pages(f"{self.base}/releases?per_page=100") if r["tag_name"] == tag]
        if len(matches) > 1:
            raise ValueError("multiple release records use this tag")
        if matches:
            release = matches[0]
            if release["draft"] and release["target_commitish"] != sha:
                raise ValueError("existing draft belongs to another commit; use a new version or resolve that draft")
            if release["draft"]:
                # Recheck write access even when another run created the draft.
                release = self.api(f"{self.base}/releases/{release['id']}", {"name": release["name"]}, "PATCH")
            return release
        # An actual private write under the publishing job's token establishes
        # release permission. A local gh login or secret name is not evidence.
        release = self.api(f"{self.base}/releases", {
            "tag_name": tag, "target_commitish": sha, "name": tag,
            "draft": True, "prerelease": "-" in tag, "generate_release_notes": True,
        }, "POST")
        if not release["draft"] or release["target_commitish"] != sha:
            raise ValueError("GitHub did not create the requested private draft")
        return release

    def verify_assets(self, release, directory):
        """Check an upload against the exact files staged by this publisher."""
        assets = self.pages(f"{self.base}/releases/{release['id']}/assets?per_page=100")
        expected = {p.name: p for p in directory.iterdir()}
        if len(assets) != len(expected) or {a["name"] for a in assets} != set(expected):
            raise ValueError("GitHub release is missing assets or contains unexpected ones")
        for asset in assets:
            local = expected[asset["name"]].read_bytes()
            if asset["state"] != "uploaded" or asset["size"] != len(local):
                raise ValueError(f"incomplete GitHub asset: {asset['name']}")
            remote = run("gh", "api", "-H", "Accept: application/octet-stream", f"{self.base}/releases/assets/{asset['id']}")
            if digest(remote) != digest(local):
                raise ValueError(f"GitHub asset checksum mismatch: {asset['name']}")

    def verify_published_assets(self, release, directory, sha):
        """Compare a rebuilt package without assuming identical gzip encoding."""
        tag = release["tag_name"]
        validate_tag(tag)
        expected = {archive_name(tag, target) for target in TARGETS} | {"release.json", "checksums.txt"}
        assets = self.pages(f"{self.base}/releases/{release['id']}/assets?per_page=100")
        if len(assets) != len(expected) or {a["name"] for a in assets} != expected:
            raise ValueError("GitHub release is missing assets or contains unexpected ones")
        with tempfile.TemporaryDirectory() as temporary:
            downloaded = Path(temporary)
            for asset in assets:
                if asset["state"] != "uploaded" or asset["size"] <= 0:
                    raise ValueError(f"incomplete GitHub asset: {asset['name']}")
                data = run("gh", "api", "-H", "Accept: application/octet-stream", f"{self.base}/releases/assets/{asset['id']}")
                if len(data) != asset["size"]:
                    raise ValueError(f"incomplete GitHub download: {asset['name']}")
                (downloaded / asset["name"]).write_bytes(data)
            compare_contents(tag, directory, downloaded, sha)

    def publish(self, release, directory, sha):
        tag = release["tag_name"]
        staged_here = release["draft"]
        if release["draft"]:
            for path in sorted(directory.iterdir()):
                run("gh", "release", "upload", tag, str(path), "--repo", self.repo, "--clobber")
            # Every byte is staged and checked while the release is private.
            self.verify_assets(release, directory)
            if self.tag_commit(tag) not in (None, sha):
                raise ValueError("tag changed while staging assets")
            release = self.api(f"{self.base}/releases/{release['id']}", {"draft": False, "make_latest": "false" if release["prerelease"] else "true"}, "PATCH")
        if release["draft"] or not release.get("published_at") or self.tag_commit(tag) != sha:
            raise ValueError("release is not published at the validated commit")
        if staged_here:
            self.verify_assets(release, directory)
        else:
            self.verify_published_assets(release, directory, sha)
        print(f"Verified published release: {release['html_url']}")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=("package", "verify", "preflight", "publish"))
    parser.add_argument("tag")
    parser.add_argument("directory", type=Path)
    args = parser.parse_args()
    validate_tag(args.tag)
    if args.command == "package":
        package(args.tag, args.directory)
        return
    sha = commit()
    verify_local(args.tag, args.directory, sha)
    if args.command == "verify":
        return
    github = GitHub(os.environ.get("GH_REPO", ""))
    release = github.prepare(args.tag, sha)
    if not release["draft"]:
        # A retry may find a completed immutable release. Verify without edits.
        github.publish(release, args.directory, sha)
    elif args.command == "publish":
        github.publish(release, args.directory, sha)
    else:
        print(f"Publishability passed for {args.tag} at {sha}; draft {release['id']} is unpublished")


if __name__ == "__main__":
    main()
