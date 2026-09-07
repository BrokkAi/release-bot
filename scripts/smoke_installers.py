#!/usr/bin/env python3
"""Build all installers and exercise real local npm and uv installs without publishing."""

import argparse
import json
import os
from pathlib import Path
import platform
import shutil
import subprocess
import tarfile
import tempfile

import package_installers
import release


def smoke(packages, assets, tag):
    system = {"Linux": "linux", "Darwin": "darwin"}[platform.system()]
    arch = {"x86_64": "amd64", "arm64": "arm64", "aarch64": "arm64"}[platform.machine()]
    target = f"{system}-{arch}"
    with tempfile.TemporaryDirectory() as temporary:
        temporary = Path(temporary)
        env = dict(os.environ, npm_config_cache=str(temporary / "npm-cache"),
                   UV_CACHE_DIR=str(temporary / "uv-cache"), UV_TOOL_DIR=str(temporary / "uv-tools"),
                   UV_TOOL_BIN_DIR=str(temporary / "uv-bin"), BROKK_RELEASE_BOT_CACHE_DIR=str(temporary / "native-cache"))
        npm_arch = "x64" if arch == "amd64" else "arm64"
        manifest = json.loads((packages / "npm/manifest.json").read_text())
        selected = [p for p in manifest["packages"] if p["name"] in
                    (package_installers.NPM_ROOT, f"{package_installers.NPM_ROOT}-{system}-{npm_arch}")]
        subprocess.run(["npm", "install", "--offline", "--ignore-scripts", "--no-audit", "--no-fund", "--prefix", str(temporary / "npm"),
                        *[str((packages / "npm" / p["filename"]).resolve()) for p in selected]], check=True, env=env)
        subprocess.run([str(temporary / "npm/node_modules/.bin/brb"), "--help"], check=True, env=env)
        wheels = list((packages / "python").glob("*.whl"))
        if len(wheels) != 1:
            raise ValueError("expected exactly one Python wheel")
        subprocess.run(["uv", "tool", "install", "--no-index", str(wheels[0].resolve())], check=True, env=env)
        # Use the exact release payload for an offline installed-wheel smoke test.
        # Unit tests separately exercise first-run download, verification and repair.
        cached = Path(env["BROKK_RELEASE_BOT_CACHE_DIR"]) / tag / target / "brb"
        cached.parent.mkdir(parents=True)
        with tarfile.open(assets / release.archive_name(tag, target), "r:gz") as archive:
            with archive.extractfile("brb") as source, cached.open("wb") as output:
                shutil.copyfileobj(source, output)
        cached.chmod(0o755)
        subprocess.run([str(temporary / "uv-bin/brb"), "--help"], check=True, env=env)
        print("Local npm and uv installs both launched brb successfully")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--tag", default="v0.0.0")
    parser.add_argument("--assets", type=Path)
    parser.add_argument("--packages", type=Path)
    args = parser.parse_args()
    if bool(args.assets) != bool(args.packages):
        parser.error("--assets and --packages must be supplied together")
    if args.packages:
        smoke(args.packages, args.assets, args.tag)
        return
    with tempfile.TemporaryDirectory() as temporary:
        assets, packages = Path(temporary) / "assets", Path(temporary) / "packages"
        release.package(args.tag, assets)
        package_installers.package(args.tag, assets, packages, release.commit())
        smoke(packages, assets, args.tag)


if __name__ == "__main__":
    main()
