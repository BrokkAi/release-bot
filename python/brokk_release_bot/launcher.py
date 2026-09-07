"""Download the package's exact native release, verify it, cache it, and exec brb."""

import hashlib
import json
import os
from pathlib import Path
import platform
import shutil
import sys
import tarfile
import tempfile
import urllib.request


def target():
    system = {"Linux": "linux", "Darwin": "darwin"}.get(platform.system())
    arch = {"x86_64": "amd64", "amd64": "amd64", "aarch64": "arm64", "arm64": "arm64"}.get(platform.machine().lower())
    if not system or not arch:
        raise ValueError("Brokk Release Bot supports Linux and macOS on amd64 and arm64")
    return f"{system}-{arch}"


def digest(path):
    checksum = hashlib.sha256()
    with path.open("rb") as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b""):
            checksum.update(chunk)
    return checksum.hexdigest()


def download(url, destination):
    request = urllib.request.Request(url, headers={"User-Agent": "brokk-release-bot"})
    with urllib.request.urlopen(request, timeout=120) as response:
        if not response.url.startswith("https://"):
            raise ValueError("release download redirected away from HTTPS")
        with destination.open("wb") as output:
            shutil.copyfileobj(response, output)


def resolve_binary():
    metadata = json.loads(Path(__file__).with_name("release.json").read_text())
    selected = target()
    asset = metadata["targets"][selected]
    cache = Path(os.environ.get("BROKK_RELEASE_BOT_CACHE_DIR") or
                 Path(os.environ.get("XDG_CACHE_HOME") or Path.home() / ".cache") / "brokk-release-bot")
    directory = cache / metadata["tag"] / selected
    directory.mkdir(parents=True, exist_ok=True)
    binary = directory / "brb"
    # A checksum also catches interrupted writes, corruption, and stale caches.
    if binary.is_file() and digest(binary) == asset["binary_sha256"]:
        binary.chmod(0o755)
        return binary
    print(f"brb: downloading Brokk Release Bot {metadata['tag']} for {selected}", file=sys.stderr)
    # Keep the temporary executable on the destination filesystem for atomic replacement.
    with tempfile.TemporaryDirectory(prefix=".install-", dir=directory) as temporary:
        temporary = Path(temporary)
        archive = temporary / "archive.tar.gz"
        download(f"https://github.com/BrokkAi/release-bot/releases/download/{metadata['tag']}/{asset['name']}", archive)
        if digest(archive) != asset["sha256"]:
            raise ValueError("release archive checksum mismatch")
        staged = temporary / "brb"
        with tarfile.open(archive, "r:gz") as bundle:
            members = [member for member in bundle.getmembers() if member.name == "brb"]
            if len(members) != 1 or not members[0].isfile() or members[0].size <= 0:
                raise ValueError("release archive must contain one regular, nonempty brb executable")
            with bundle.extractfile(members[0]) as source, staged.open("wb") as output:
                shutil.copyfileobj(source, output)
        if digest(staged) != asset["binary_sha256"]:
            raise ValueError("release binary checksum mismatch")
        staged.chmod(0o755)
        os.replace(staged, binary)
    return binary


def main():
    try:
        binary = resolve_binary()
        os.execv(str(binary), [str(binary), *sys.argv[1:]])
    except (OSError, ValueError, KeyError, tarfile.TarError) as error:
        print(f"brb: {error}", file=sys.stderr)
        return 1
