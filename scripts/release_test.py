import gzip
import json
import shutil
import tarfile
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

import release


class ReleaseAssets(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.directory = Path(self.temp.name)
        self.tag, self.sha = "v0.1.0", "a" * 40
        self.manifest = {"tag": self.tag, "commit": self.sha, "assets": []}
        for target in release.TARGETS:
            name = release.archive_name(self.tag, target)
            path = self.directory / name
            release.archive(path, {
                "brb": b"fixture executable", "LICENSE": b"fixture license", "README.md": b"fixture readme",
                "BUILD.json": json.dumps({"tag": self.tag, "commit": self.sha, "target": target}).encode(),
            }, 100)
            self.manifest["assets"].append({"name": name, "size": path.stat().st_size, "sha256": release.digest(path.read_bytes())})
        self.write_manifest()

    def write_manifest(self):
        (self.directory / "release.json").write_text(json.dumps(self.manifest))
        (self.directory / "checksums.txt").write_text("".join(f"{a['sha256']}  {a['name']}\n" for a in self.manifest["assets"]))

    def test_complete_assets(self):
        release.verify_local(self.tag, self.directory, self.sha)

    def test_missing_platform_cannot_hide_in_manifest(self):
        missing = self.manifest["assets"].pop()
        (self.directory / missing["name"]).unlink()
        self.write_manifest()
        with self.assertRaisesRegex(ValueError, "every supported platform"):
            release.verify_local(self.tag, self.directory, self.sha)

    def test_corruption_is_rejected(self):
        (self.directory / self.manifest["assets"][0]["name"]).write_bytes(b"truncated")
        with self.assertRaisesRegex(ValueError, "corrupt"):
            release.verify_local(self.tag, self.directory, self.sha)

    def test_wrong_commit_is_rejected(self):
        with self.assertRaisesRegex(ValueError, "exact checkout commit"):
            release.verify_local(self.tag, self.directory, "b" * 40)

    def test_bad_checksums_are_rejected(self):
        (self.directory / "checksums.txt").write_text("incorrect")
        with self.assertRaisesRegex(ValueError, "checksum list"):
            release.verify_local(self.tag, self.directory, self.sha)

    def test_invalid_tag(self):
        for tag in ("../v1", "v1", "v01.0.0", "v1.0.0;echo", "v1.0.0\n"):
            with self.assertRaises(ValueError):
                release.validate_tag(tag)

    def test_failed_remote_asset_check_keeps_draft_private(self):
        github = release.GitHub("fixture/repo")
        draft = {"id": 1, "tag_name": self.tag, "draft": True, "prerelease": False}
        with patch.object(release, "run"), patch.object(github, "api") as api, patch.object(github, "verify_assets", side_effect=ValueError("missing upload")):
            with self.assertRaisesRegex(ValueError, "missing upload"):
                github.publish(draft, self.directory, self.sha)
            api.assert_not_called()

    def test_completed_release_is_verified_without_modification(self):
        github = release.GitHub("fixture/repo")
        published = {"id": 1, "tag_name": self.tag, "draft": False, "published_at": "2026-09-07", "html_url": "fixture"}
        with patch.object(github, "api") as api, patch.object(github, "tag_commit", return_value=self.sha), patch.object(github, "verify_published_assets") as verify, patch.object(release, "run") as run:
            github.publish(published, self.directory, self.sha)
            api.assert_not_called()
            run.assert_not_called()
            verify.assert_called_once()

    def test_existing_draft_rechecks_actual_writer(self):
        github = release.GitHub("fixture/repo")
        draft = {"id": 1, "tag_name": self.tag, "name": self.tag, "draft": True, "target_commitish": self.sha}
        with patch.object(github, "tag_commit", return_value=None), patch.object(github, "pages", return_value=[draft]), patch.object(github, "api", side_effect=PermissionError("contents:write denied")):
            with self.assertRaises(PermissionError):
                github.prepare(self.tag, self.sha)

    def test_existing_tag_cannot_be_moved(self):
        github = release.GitHub("fixture/repo")
        with patch.object(github, "tag_commit", return_value="b" * 40), patch.object(github, "api") as api:
            with self.assertRaisesRegex(ValueError, "refusing to move"):
                github.prepare(self.tag, self.sha)
            api.assert_not_called()

    def remote_package(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        remote = Path(temporary.name)
        for path in self.directory.iterdir():
            shutil.copyfile(path, remote / path.name)
        for path in remote.glob("*.tar.gz"):
            path.write_bytes(gzip.compress(gzip.decompress(path.read_bytes()), compresslevel=1, mtime=123))
        self.refresh_remote_manifest(remote)
        return remote

    def refresh_remote_manifest(self, remote):
        manifest = json.loads((remote / "release.json").read_text())
        for asset in manifest["assets"]:
            data = (remote / asset["name"]).read_bytes()
            asset.update(size=len(data), sha256=release.digest(data))
        (remote / "release.json").write_text(json.dumps(manifest, indent=2))
        (remote / "checksums.txt").write_text("".join(f"{a['sha256']}  {a['name']}\n" for a in manifest["assets"]))

    def remote_downloads(self, remote):
        data = {str(i): p.read_bytes() for i, p in enumerate(sorted(remote.iterdir()))}
        assets = [{"id": str(i), "name": p.name, "state": "uploaded", "size": p.stat().st_size}
                  for i, p in enumerate(sorted(remote.iterdir()))]
        return assets, lambda *args: data[args[-1].rsplit("/", 1)[-1]]

    def test_published_recompressed_assets_pass_without_any_writes(self):
        remote = self.remote_package()
        assets, download = self.remote_downloads(remote)
        self.assertNotEqual((remote / "checksums.txt").read_bytes(), (self.directory / "checksums.txt").read_bytes())
        github = release.GitHub("fixture/repo")
        published = {"id": 1, "tag_name": self.tag, "draft": False, "published_at": "2026-09-07", "html_url": "fixture"}
        with patch.object(github, "pages", return_value=assets), patch.object(github, "tag_commit", return_value=self.sha), patch.object(github, "api") as api, patch.object(release, "run", side_effect=download) as run:
            github.publish(published, self.directory, self.sha)
            api.assert_not_called()
            self.assertEqual(run.call_count, 6)
            for call in run.call_args_list:
                self.assertEqual(call.args[:4], ("gh", "api", "-H", "Accept: application/octet-stream"))

    def test_same_run_upload_still_requires_identical_bytes(self):
        remote = self.remote_package()
        assets, download = self.remote_downloads(remote)
        github = release.GitHub("fixture/repo")
        with patch.object(github, "pages", return_value=assets), patch.object(release, "run", side_effect=download):
            with self.assertRaisesRegex(ValueError, "checksum mismatch|incomplete GitHub asset"):
                github.verify_assets({"id": 1}, self.directory)

    def test_rebuilt_contents_must_match_even_with_self_consistent_checksums(self):
        for member in ("brb", "README.md", "LICENSE", "BUILD.json"):
            with self.subTest(member=member):
                remote = self.remote_package()
                path = remote / release.archive_name(self.tag, release.TARGETS[0])
                with tarfile.open(path) as archive:
                    files = {m.name: archive.extractfile(m).read() for m in archive.getmembers()}
                if member == "BUILD.json":
                    metadata = json.loads(files[member])
                    metadata["commit"] = "b" * 40
                    files[member] = json.dumps(metadata).encode()
                else:
                    files[member] += b"changed"
                release.archive(path, files, 100)
                self.refresh_remote_manifest(remote)
                with self.assertRaisesRegex(ValueError, "content mismatch|build metadata"):
                    release.compare_contents(self.tag, self.directory, remote, self.sha)

    def test_published_download_rejects_corruption_missing_files_and_unsafe_names(self):
        for failure in ("corruption", "missing", "unexpected", "truncated", "pending", "checksums", "wrong commit"):
            with self.subTest(failure=failure):
                remote = self.remote_package()
                if failure == "corruption":
                    path = next(remote.glob("*.tar.gz"))
                    path.write_bytes(path.read_bytes() + b"corrupt")
                elif failure == "checksums":
                    (remote / "checksums.txt").write_text("invalid")
                elif failure == "wrong commit":
                    manifest = json.loads((remote / "release.json").read_text())
                    manifest["commit"] = "b" * 40
                    (remote / "release.json").write_text(json.dumps(manifest))
                assets, download = self.remote_downloads(remote)
                if failure == "missing":
                    assets.pop()
                elif failure == "unexpected":
                    assets[0]["name"] = "../outside"
                elif failure == "truncated":
                    assets[0]["size"] += 1
                elif failure == "pending":
                    assets[0]["state"] = "new"
                github = release.GitHub("fixture/repo")
                with patch.object(github, "pages", return_value=assets), patch.object(release, "run", side_effect=download):
                    with self.assertRaises(ValueError):
                        github.verify_published_assets({"id": 1, "tag_name": self.tag}, self.directory, self.sha)

    def test_deleted_empty_draft_is_recreated_by_publishing_identity(self):
        github = release.GitHub("fixture/repo")
        draft = {"id": 2, "tag_name": self.tag, "draft": True, "target_commitish": self.sha}
        with patch.object(github, "tag_commit", return_value=None), patch.object(github, "pages", return_value=[]), patch.object(github, "api", return_value=draft) as api:
            self.assertEqual(github.prepare(self.tag, self.sha), draft)
            self.assertEqual(api.call_args.args[2], "POST")
            self.assertTrue(api.call_args.args[1]["draft"])
        with patch.object(github, "tag_commit", return_value=None), patch.object(github, "pages", return_value=[]), patch.object(github, "api", side_effect=PermissionError("revoked")):
            with self.assertRaises(PermissionError):
                github.prepare(self.tag, self.sha)


if __name__ == "__main__":
    unittest.main()
