import json
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
                "release-bot": b"fixture executable", "LICENSE": b"fixture license", "README.md": b"fixture readme",
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
        with patch.object(github, "api") as api, patch.object(github, "tag_commit", return_value=self.sha), patch.object(github, "verify_assets") as verify, patch.object(release, "run") as run:
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


if __name__ == "__main__":
    unittest.main()
