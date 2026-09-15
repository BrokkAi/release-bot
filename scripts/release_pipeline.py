#!/usr/bin/env python3
"""Validate all destinations before uploads; finalize GitHub only after npm."""
import argparse
import json
import os
from pathlib import Path

import npm_authorization
import package_registry
import release


def run(command, tag, native, packages):
    sha = release.commit()
    release.verify_local(tag, native, sha)
    manifest = json.loads((packages / "npm/manifest.json").read_text())
    if manifest["commit"] != sha or manifest["tag"] != tag:
        raise ValueError("installer packages belong to another commit or tag")
    package_registry.run("check", packages, "npm")
    github = release.GitHub(os.environ["GH_REPO"])
    record = github.prepare(tag, sha)
    if not record["draft"]:
        github.verify_published_assets(record, native, sha)
    if command == "preflight":
        return
    # Refresh authorization in the actual publishing step before any upload.
    npm_authorization.check()
    package_registry.run("publish", packages, "npm")
    def complete():
        try:
            package_registry.run("verify", packages, "npm")
            return True
        except ValueError as error:
            if "publication is incomplete:" in str(error):
                return False
            raise
    package_registry.wait_visible(complete)
    github.publish(record, native, sha)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=("preflight", "publish"))
    parser.add_argument("tag")
    parser.add_argument("native", type=Path)
    parser.add_argument("packages", type=Path)
    args = parser.parse_args()
    run(args.command, args.tag, args.native, args.packages)
