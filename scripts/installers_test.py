import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

import package_installers
import release

ROOT = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(ROOT / "python"))
from brokk_release_bot import launcher


class Installers(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.assets = self.root / "assets"
        self.assets.mkdir()
        self.tag, self.sha = "v0.1.0", "a" * 40
        self.binary = b'#!/bin/sh\nprintf "%s\\n" "$@"\nexit 23\n'
        self.manifest = {"tag": self.tag, "commit": self.sha, "assets": []}
        self.metadata = {"tag": self.tag, "commit": self.sha, "targets": {}}
        for target in release.TARGETS:
            name = release.archive_name(self.tag, target)
            release.archive(self.assets / name, {
                "brb": self.binary, "LICENSE": b"fixture license", "README.md": b"fixture readme",
                "BUILD.json": json.dumps({"tag": self.tag, "commit": self.sha, "target": target}).encode(),
            }, 100)
            data = (self.assets / name).read_bytes()
            asset = {"name": name, "size": len(data), "sha256": release.digest(data)}
            self.manifest["assets"].append(asset)
            self.metadata["targets"][target] = dict(asset, binary_sha256=release.digest(self.binary))
        (self.assets / "release.json").write_text(json.dumps(self.manifest))
        (self.assets / "checksums.txt").write_text("".join(f"{a['sha256']}  {a['name']}\n" for a in self.manifest["assets"]))
        self.bin = self.root / "tools"
        self.bin.mkdir()
        self.executable("uname", '#!/bin/sh\ncase "$1" in -s) echo "${TEST_OS:-Linux}";; -m) echo "${TEST_ARCH:-x86_64}";; esac\n')
        self.executable("curl", f'''#!{sys.executable}
import os, pathlib, shutil, sys
args = sys.argv[1:]
url = args[-1]
if os.environ.get("FAIL_DOWNLOAD"):
    sys.exit(22)
if url.endswith("/latest"):
    print("https://github.com/BrokkAi/release-bot/releases/tag/v0.1.0", end="")
else:
    assert "/download/v0.1.0/" in url, url
    shutil.copyfile(pathlib.Path(os.environ["TEST_ASSETS"]) / url.rsplit("/", 1)[1], args[args.index("--output") + 1])
''')
        self.env = dict(os.environ, PATH=f"{self.bin}:{os.environ['PATH']}", TEST_ASSETS=str(self.assets),
                        INSTALL_DIR=str(self.root / "install space"), TMPDIR=str(self.root),
                        BROKK_RELEASE_BOT_CACHE_DIR=str(self.root / "cache"))

    def executable(self, name, contents):
        path = self.bin / name
        path.write_text(contents)
        path.chmod(0o755)

    def install(self, *args, **env):
        return subprocess.run(["sh", str(ROOT / "install.sh"), *args], env=dict(self.env, **env), capture_output=True, text=True)

    def test_curl_latest_and_all_platforms(self):
        for system, machine in (("Linux", "x86_64"), ("Linux", "aarch64"), ("Darwin", "x86_64"), ("Darwin", "arm64")):
            with self.subTest(system=system, machine=machine):
                result = self.install(TEST_OS=system, TEST_ARCH=machine)
                self.assertEqual(result.returncode, 0, result.stderr)
                binary = Path(self.env["INSTALL_DIR"]) / "brb"
                self.assertEqual(binary.read_bytes(), self.binary)
                self.assertTrue(os.access(binary, os.X_OK))

    def test_curl_pinned_version_and_shasum_fallback(self):
        # Use a controlled PATH that deliberately excludes sha256sum.
        for command in ("sh", "tar", "awk", "mktemp", "mkdir", "chmod", "mv", "rm", "gzip", "shasum"):
            resolved = shutil.which(command)
            if not resolved:
                self.skipTest(f"{command} is unavailable")
            (self.bin / command).symlink_to(resolved)
        result = self.install(self.tag, PATH=str(self.bin))
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_curl_failure_preserves_existing_install(self):
        binary = Path(self.env["INSTALL_DIR"]) / "brb"
        binary.parent.mkdir()
        binary.write_bytes(b"old executable")
        (self.assets / self.manifest["assets"][0]["name"]).write_bytes(b"corrupted")
        result = self.install(self.tag)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("checksum mismatch", result.stderr)
        self.assertEqual(binary.read_bytes(), b"old executable")
        self.assertEqual(list(binary.parent.iterdir()), [binary])

    def test_curl_rejects_missing_or_duplicate_checksum(self):
        checksums = self.assets / "checksums.txt"
        original = checksums.read_text()
        for contents in ("", original + original):
            checksums.write_text(contents)
            self.assertNotEqual(self.install().returncode, 0)
            self.assertFalse((Path(self.env["INSTALL_DIR"]) / "brb").exists())

    def test_curl_rejects_unsupported_platform_bad_version_and_download_failure(self):
        for args, env in ((("../bad",), {}), (("v1.0.0\nv1.0.0",), {}), ((), {"TEST_OS": "Windows"}),
                          ((), {"TEST_ARCH": "i686"}), ((), {"FAIL_DOWNLOAD": "1"})):
            with self.subTest(args=args, env=env):
                self.assertNotEqual(self.install(*args, **env).returncode, 0)
                self.assertFalse((Path(self.env["INSTALL_DIR"]) / "brb").exists())

    def test_python_cold_cache_reuse_and_corruption_repair(self):
        module = self.root / "launcher.py"
        module.with_name("release.json").write_text(json.dumps(self.metadata))
        def download(url, destination):
            shutil.copyfile(self.assets / url.rsplit("/", 1)[1], destination)
        with patch.dict(os.environ, self.env), patch.object(launcher, "__file__", str(module)), \
                patch.object(launcher, "target", return_value="linux-amd64"), patch.object(launcher, "download", side_effect=download) as fetch:
            binary = launcher.resolve_binary()
            self.assertEqual(binary.read_bytes(), self.binary)
            self.assertEqual(launcher.resolve_binary(), binary)
            fetch.assert_called_once()
            binary.write_bytes(b"corrupted")
            self.assertEqual(launcher.resolve_binary().read_bytes(), self.binary)
            self.assertEqual(fetch.call_count, 2)
            result = subprocess.run([str(binary), "argument with spaces", "--flag"], capture_output=True, text=True)
            self.assertEqual(result.returncode, 23)
            self.assertEqual(result.stdout, "argument with spaces\n--flag\n")

    def test_python_rejects_checksum_mismatch(self):
        module = self.root / "launcher.py"
        module.with_name("release.json").write_text(json.dumps(self.metadata))
        with patch.dict(os.environ, self.env), patch.object(launcher, "__file__", str(module)), \
                patch.object(launcher, "target", return_value="linux-amd64"), \
                patch.object(launcher, "download", side_effect=lambda url, path: path.write_bytes(b"corrupted")):
            with self.assertRaisesRegex(ValueError, "checksum mismatch"):
                launcher.resolve_binary()
            self.assertFalse(list((self.root / "cache").rglob("brb")))

    def test_python_exec_forwards_arguments(self):
        with patch.object(launcher, "resolve_binary", return_value=Path("/tmp/brb")), \
                patch.object(sys, "argv", ["brb", "--once", "repo with spaces"]), patch.object(os, "execv") as execute:
            launcher.main()
            execute.assert_called_once_with("/tmp/brb", ["/tmp/brb", "--once", "repo with spaces"])

    def test_python_unsupported_platform(self):
        with patch.object(launcher.platform, "system", return_value="Windows"):
            with self.assertRaisesRegex(ValueError, "supports Linux and macOS"):
                launcher.target()

    def test_package_versions(self):
        for tag, expected in (("v1.2.3", ("1.2.3", "1.2.3")), ("v1.2.3-rc.1", ("1.2.3-rc.1", "1.2.3rc1")),
                              ("v1.2.3-alpha.2", ("1.2.3-alpha.2", "1.2.3a2"))):
            self.assertEqual(package_installers.versions(tag), expected)
        for tag in ("v1.2.3-preview.1", "v1.2.3-rc.01", "../v1.2.3"):
            with self.assertRaises(ValueError):
                package_installers.versions(tag)

    def test_package_refuses_corrupt_or_wrong_commit_assets(self):
        with self.assertRaisesRegex(ValueError, "exact checkout commit"):
            package_installers.package(self.tag, self.assets, self.root / "out", "b" * 40)
        (self.assets / self.manifest["assets"][0]["name"]).write_bytes(b"corrupted")
        with self.assertRaisesRegex(ValueError, "corrupt release asset"):
            package_installers.package(self.tag, self.assets, self.root / "out", self.sha)
