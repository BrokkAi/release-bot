import os
import unittest
from unittest.mock import patch

import release_check


class FakeGitHub:
    base = "repos/BrokkAi/release-bot"

    def __init__(self, runs=None, jobs=None):
        self.runs = runs or []
        self.jobs = jobs or []

    def api(self, path):
        if "/jobs?" in path:
            return {"jobs": self.jobs}
        return {"workflow_runs": self.runs}


class ReleaseChecks(unittest.TestCase):
    def test_selects_only_exact_nonpublishing_run(self):
        expected = {
            "id": 9, "head_sha": "a" * 40, "event": "workflow_dispatch",
            "path": release_check.RELEASE_WORKFLOW,
            "display_title": "Release v0.1.0 (publish=false)",
            "status": "completed", "conclusion": "success",
        }
        wrong_tag = dict(expected, id=10, display_title="Release v0.2.0 (publish=false)")
        selected = release_check.selected_preflight_run(FakeGitHub([expected, wrong_tag]), "a" * 40, "v0.1.0")
        self.assertEqual(selected["id"], 9)

    def test_incomplete_latest_preflight_is_not_hidden(self):
        complete = {
            "id": 9, "head_sha": "a" * 40, "event": "workflow_dispatch",
            "path": release_check.RELEASE_WORKFLOW,
            "display_title": "Release v0.1.0 (publish=false)",
            "status": "completed", "conclusion": "success",
        }
        incomplete = dict(complete, id=11, status="in_progress", conclusion=None)
        with self.assertRaisesRegex(ValueError, "did not complete"):
            release_check.selected_preflight_run(FakeGitHub([complete, incomplete]), "a" * 40, "v0.1.0")

    @patch.dict(os.environ, {"RELEASE_COMMIT": "a" * 40, "RELEASE_TAG": "v0.1.0", "RELEASE_TARGET": "b" * 40})
    def test_context_rejects_wrong_checkout(self):
        with patch.object(release_check.release, "commit", return_value="c" * 40):
            with self.assertRaisesRegex(ValueError, "HEAD"):
                release_check.context()


if __name__ == "__main__":
    unittest.main()
