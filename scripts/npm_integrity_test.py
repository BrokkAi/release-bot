import base64
import gzip
import hashlib
import io
from pathlib import Path
import tarfile
import tempfile
import unittest
from unittest.mock import patch

import package_registry as registry


def archive(payload, mode=0o755, mtime=0):
    buf = io.BytesIO()
    with tarfile.open(fileobj=buf, mode="w") as tar:
        entry = tarfile.TarInfo("package/bin/brb")
        entry.mode = mode
        entry.size = len(payload)
        tar.addfile(entry, io.BytesIO(payload))
    return gzip.compress(buf.getvalue(), mtime=mtime)


class NpmIntegrityTests(unittest.TestCase):
    def test_download_integrity_payload_permissions_and_gzip(self):
        with tempfile.TemporaryDirectory() as temp:
            path = Path(temp) / "package.tgz"
            path.write_bytes(archive(b"binary"))
            package = {"name": "@brokkai/release-bot", "version": "0.6.0", "_path": path}
            for data, success in [(archive(b"binary", mtime=99), True), (archive(b"changed"), False), (archive(b"binary", mode=0o644), False)]:
                record = dict(name=package["name"], version=package["version"], dist={"tarball": "https://registry.npmjs.org/fixture.tgz", "integrity": "sha512-" + base64.b64encode(hashlib.sha512(data).digest()).decode()})
                with patch.object(registry, "fetch_json", return_value=record), patch.object(registry.urllib.request, "urlopen", return_value=io.BytesIO(data)):
                    if success:
                        self.assertTrue(registry.npm_exists(package))
                    else:
                        with self.assertRaisesRegex(ValueError, "differs"):
                            registry.npm_exists(package)
