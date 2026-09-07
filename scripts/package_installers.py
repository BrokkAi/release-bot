#!/usr/bin/env python3
"""Build npm packages and a uv-installable Python launcher from verified release assets."""

import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import tarfile
import tempfile

import release

ROOT = Path(__file__).resolve().parent.parent
NPM_ROOT = "@brokkai/release-bot"


def versions(tag):
    release.validate_tag(tag)
    match = re.fullmatch(r"v(\d+\.\d+\.\d+)(?:-(alpha|beta|rc)\.(0|[1-9][0-9]*))?", tag)
    if not match:
        raise ValueError("package tags must be vX.Y.Z or vX.Y.Z-{alpha,beta,rc}.N for PyPI compatibility")
    base, phase, number = match.groups()
    return tag[1:], base + ({"alpha": "a", "beta": "b", "rc": "rc"}[phase] + number if phase else "")


def write_json(path, value):
    path.write_text(json.dumps(value, indent=2) + "\n")


def package(tag, assets, output, sha):
    npm_version, python_version = versions(tag)
    manifest = release.verify_local(tag, assets, sha)
    if output.exists() and any(output.iterdir()):
        raise ValueError("installer output directory must be empty")
    output.mkdir(parents=True, exist_ok=True)
    npm_output, python_output = output / "npm", output / "python"
    npm_output.mkdir()
    python_output.mkdir()
    metadata = {"tag": tag, "commit": sha, "targets": {}}
    packages = []
    base = {
        "version": npm_version, "license": "Apache-2.0",
        "repository": {"type": "git", "url": "git+https://github.com/BrokkAi/release-bot.git"},
        "publishConfig": {"access": "public"},
    }
    with tempfile.TemporaryDirectory() as temporary:
        staging = Path(temporary)

        def npm_pack(name, fields, files):
            directory = staging / name.split("/")[-1]
            directory.mkdir()
            write_json(directory / "package.json", dict(base, name=name, **fields))
            for filename, data in files.items():
                path = directory / filename
                path.parent.mkdir(parents=True, exist_ok=True)
                path.write_bytes(data)
                path.chmod(0o755 if filename.startswith("bin/") else 0o644)
            result = subprocess.check_output([
                "npm", "pack", "--ignore-scripts", "--json", "--pack-destination", str(npm_output.resolve()),
            ], cwd=directory)
            info = json.loads(result)[0]
            tarball = npm_output / info["filename"]
            packages.append({"name": name, "version": npm_version, "filename": tarball.name,
                             "sha256": release.digest(tarball.read_bytes()), "integrity": info["integrity"]})

        dependencies = {}
        for target in release.TARGETS:
            name = release.archive_name(tag, target)
            asset = next(item for item in manifest["assets"] if item["name"] == name)
            with tarfile.open(assets / name, "r:gz") as bundle:
                files = {member: bundle.extractfile(member).read() for member in ("brb", "LICENSE", "README.md", "BUILD.json")}
            metadata["targets"][target] = dict(asset, binary_sha256=hashlib.sha256(files["brb"]).hexdigest())
            system, go_arch = target.split("-")
            arch = {"amd64": "x64", "arm64": "arm64"}[go_arch]
            package_name = f"{NPM_ROOT}-{system}-{arch}"
            dependencies[package_name] = npm_version
            npm_pack(package_name, {"os": [system], "cpu": [arch], "description": f"Brokk Release Bot native binary for {system}/{arch}"},
                     {"bin/brb": files["brb"], "LICENSE": files["LICENSE"], "README.md": files["README.md"], "BUILD.json": files["BUILD.json"]})
        npm_pack(NPM_ROOT, {
            "description": "Brokk Release Bot: autonomous releases through Agent Client Protocol",
            "bin": {"brb": "bin/brb.cjs"}, "engines": {"node": ">=18"},
            "os": ["linux", "darwin"], "cpu": ["x64", "arm64"], "optionalDependencies": dependencies,
        }, {"bin/brb.cjs": (ROOT / "npm/brb.cjs").read_bytes(), "LICENSE": files["LICENSE"], "README.md": files["README.md"]})
        write_json(npm_output / "manifest.json", {"tag": tag, "commit": sha, "packages": packages})

        python = staging / "python"
        shutil.copytree(ROOT / "python", python, ignore=shutil.ignore_patterns("__pycache__"))
        project = python / "pyproject.toml"
        project.write_text(project.read_text().replace('version = "0.0.0"', f'version = "{python_version}"'))
        for filename in ("LICENSE", "README.md"):
            (python / filename).write_bytes(files[filename])
        write_json(python / "brokk_release_bot/release.json", metadata)
        subprocess.run(["uv", "build", "--out-dir", str(python_output.resolve()), str(python)], check=True,
                       env=dict(os.environ, SOURCE_DATE_EPOCH="315532800"))
    print(f"Built {len(packages)} npm packages and Python distributions for {tag} at {sha}")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("tag")
    parser.add_argument("assets", type=Path)
    parser.add_argument("output", type=Path)
    args = parser.parse_args()
    package(args.tag, args.assets, args.output, release.commit())


if __name__ == "__main__":
    main()
